package workbuddy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	acpProtocolVersion = 1
	acpRequestTimeout  = 30 * time.Second
	acpMaxBodyBytes    = 1 << 20
)

// acpTurnResult is the minimal v1 wb_webagent.run_turn result.
type acpTurnResult struct {
	OK        bool
	Status    string
	Events    int
	Chunks    int
	ElapsedMS int64
	Error     string
}

// acpTurnChannel is the streamable-HTTP transport: one SSE GET carries events
// and JSON-RPC POSTs drive the turn. It intentionally supports only the
// initialize / session-load / session-prompt subset the daily activity needs.
type acpTurnChannel struct {
	client  ContextHTTPDoer
	link    string
	token   string
	ua      string
	proxy   string
	connID  string
	err     error
	body    io.ReadCloser
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	updates int
	chunks  int
}

func newACPTurnChannel(client ContextHTTPDoer, link, token, ua, proxy string) *acpTurnChannel {
	return &acpTurnChannel{
		client: client, link: link, token: token, ua: ua, proxy: proxy,
		done: make(chan struct{}),
	}
}

func (c *acpTurnChannel) open(ctx context.Context) error {
	parsed, err := url.Parse(strings.TrimSpace(c.link))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("ACP sandbox address is invalid")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	request, err := http.NewRequestWithContext(streamCtx, http.MethodGet, c.link, nil)
	if err != nil {
		cancel()
		return errors.New("ACP SSE request is invalid")
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("User-Agent", c.ua)
	response, err := c.client.Do(streamCtx, request, c.proxy)
	if err != nil {
		cancel()
		return errors.New("ACP SSE connection failed")
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		cancel()
		return fmt.Errorf("ACP SSE returned HTTP %d", response.StatusCode)
	}
	connectionID := strings.TrimSpace(response.Header.Get("Acp-Connection-Id"))
	if connectionID == "" {
		_ = response.Body.Close()
		cancel()
		return errors.New("ACP SSE returned no Acp-Connection-Id")
	}
	c.cancel = cancel
	c.body = response.Body
	c.connID = connectionID
	go c.readSSE(response.Body)
	return nil
}

func (c *acpTurnChannel) readSSE(body io.ReadCloser) {
	defer close(c.done)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var message map[string]any
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			continue
		}
		if creditText(message["method"]) != "session/update" {
			continue
		}
		c.mu.Lock()
		c.updates++
		if update := creditObject(creditObject(message["params"])["update"]); update != nil &&
			creditText(update["sessionUpdate"]) == "agent_message_chunk" {
			c.chunks++
		}
		c.mu.Unlock()
	}
	if err := scanner.Err(); err != nil {
		c.mu.Lock()
		if c.err == nil {
			c.err = err
		}
		c.mu.Unlock()
	}
}

func (c *acpTurnChannel) request(ctx context.Context, method string, params any, id int) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, acpRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.link, bytes.NewReader(body))
	if err != nil {
		return errors.New("ACP request is invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Acp-Connection-Id", c.connID)
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("User-Agent", c.ua)
	response, err := c.client.Do(requestCtx, request, c.proxy)
	if err != nil {
		return errors.New("ACP request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, acpMaxBodyBytes))
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("%s returned HTTP %d", method, response.StatusCode)
	}
	return nil
}

func (c *acpTurnChannel) close() {
	if c == nil {
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	if c.body != nil {
		_ = c.body.Close()
	}
	select {
	case <-c.done:
	case <-time.After(time.Second):
	}
}

func (c *acpTurnChannel) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updates, c.chunks
}

// runACPTurn drives one web conversation turn exactly like v1's minimal
// wb_webagent: open the sandbox SSE stream, initialize, load the session, pin
// the model when supplied, send the prompt, then poll the console status until
// completed/failed or the wait budget expires.
func runACPTurn(
	ctx context.Context,
	client ContextHTTPDoer,
	link, token, sessionID, cwd, prompt, ua, model, proxy string,
	wait, pollInterval time.Duration,
	pollStatus func() string,
) acpTurnResult {
	started := time.Now()
	result := acpTurnResult{}
	if client == nil {
		result.Error = "ACP transport is unavailable"
		return result
	}
	channel := newACPTurnChannel(client, link, token, ua, proxy)
	defer channel.close()
	if err := channel.open(ctx); err != nil {
		result.Error = err.Error()
		return result
	}
	capabilities := map[string]any{
		"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
		"terminal": false,
	}
	if err := channel.request(ctx, "initialize", map[string]any{
		"protocolVersion": acpProtocolVersion, "clientCapabilities": capabilities,
	}, 1); err != nil {
		result.Error = err.Error()
		return result
	}
	if err := channel.request(ctx, "session/load", map[string]any{
		"sessionId": sessionID, "cwd": firstNonEmpty(cwd, "/workspace"), "mcpServers": []any{},
	}, 2); err != nil {
		result.Error = err.Error()
		return result
	}
	if strings.TrimSpace(model) != "" {
		// Model pinning is best-effort in v1: the turn still runs on the
		// conversation's default model when this extension is rejected.
		_ = channel.request(ctx, "session/set_model", map[string]any{
			"sessionId": sessionID, "modelId": model,
		}, 3)
	}
	if err := channel.request(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": prompt}},
	}, 4); err != nil {
		result.Error = err.Error()
		return result
	}

	finished := false
	status := ""
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			result.Error = err.Error()
			break
		}
		if pollStatus != nil {
			status = strings.TrimSpace(pollStatus())
			switch strings.ToLower(status) {
			case "completed":
				finished = true
			case "failed", "error":
				result.Error = "会话状态=" + status
			}
			if finished || result.Error != "" {
				break
			}
		}
		if pollInterval > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(pollInterval):
			}
		}
	}
	if !finished && result.Error == "" {
		result.Error = fmt.Sprintf("会话在 %ds 内没有跑完（状态=%s）",
			int(wait/time.Second), firstNonEmpty(status, "未知"))
	}
	if pollStatus != nil && status == "" {
		status = strings.TrimSpace(pollStatus())
	}
	result.OK = finished && result.Error == ""
	result.Status = status
	result.Events, result.Chunks = channel.counts()
	result.ElapsedMS = time.Since(started).Milliseconds()
	return result
}
