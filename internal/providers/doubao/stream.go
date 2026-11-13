package doubao

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const streamIdleTimeout = 10 * time.Minute

type blockState struct {
	text   string
	parent string
}

type chatStream struct {
	parent context.Context
	ctx    context.Context
	cancel context.CancelFunc
	body   io.ReadCloser

	provider     *Provider
	account      *Account
	model        *Model
	conversation *conversation
	sessionKey   string

	frames chan []byte
	errs   chan error
	done   chan struct{}

	closeOnce sync.Once

	mu       sync.Mutex
	usage    providers.UsageSnapshot
	sawError error
	finished bool
}

func newChatStream(parent context.Context, body io.ReadCloser, provider *Provider, account *Account, model *Model, conv *conversation, sessionKey string) *chatStream {
	ctx, cancel := context.WithCancel(parent)
	s := &chatStream{
		parent: parent, ctx: ctx, cancel: cancel, body: body,
		provider: provider, account: account, model: model, conversation: conv,
		sessionKey: sessionKey,
		frames:     make(chan []byte, 32),
		errs:       make(chan error, 1),
		done:       make(chan struct{}),
		usage: providers.UsageSnapshot{
			Realm: ProviderRealm, Account: account.UID, Model: model.ID,
			Kind: "chat", CostPolicy: "doubao:unpriced",
		},
	}
	go s.run()
	return s
}

func (s *chatStream) run() {
	defer close(s.done)
	defer close(s.frames)
	defer s.provider.pool.Release(s.account)
	defer s.body.Close()
	if s.conversation != nil {
		defer s.provider.remember(s.sessionKey, s.model.ID, s.conversation)
	}

	reader := bufio.NewReaderSize(s.body, 64*1024)
	blocks := map[string]blockState{}
	thinking := map[string]bool{}
	eventName := ""
	var data bytes.Buffer
	created := time.Now().Unix()
	id := "chatcmpl-db-" + randomID()[:16]

	emit := func(delta map[string]any, finish any) bool {
		payload := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created,
			"model": s.model.ID,
			"choices": []any{map[string]any{
				"index": 0, "delta": delta, "finish_reason": finish,
			}},
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		select {
		case s.frames <- []byte("data: " + string(raw) + "\n\n"):
			return true
		case <-s.ctx.Done():
			return false
		}
	}

	applyBlock := func(cb map[string]any, mode string) {
		content, _ := cb["content"].(map[string]any)
		textBlock, _ := content["text_block"].(map[string]any)
		text, _ := textBlock["text"].(string)
		if text == "" {
			return
		}
		blockID, _ := cb["block_id"].(string)
		parentID, _ := cb["parent_id"].(string)
		prev := blocks[blockID]
		next := text
		if mode != "set" {
			next = prev.text + text
		}
		blocks[blockID] = blockState{text: next, parent: firstNonEmpty(prev.parent, parentID)}
		if intValue(cb["block_type"]) == 10040 {
			thinking[blockID] = true
			return
		}
		if thinking[prev.parent] || thinking[parentID] || thinking[blockID] {
			return
		}
		delta := ""
		if strings.HasPrefix(next, prev.text) {
			delta = next[len(prev.text):]
		} else if mode != "set" {
			delta = text
		}
		if delta != "" {
			emit(map[string]any{"content": delta}, nil)
		}
	}

	handleEvent := func(event string, raw []byte) error {
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil
		}
		switch event {
		case "SSE_ACK":
			ack, _ := payload["ack_client_meta"].(map[string]any)
			if s.conversation != nil {
				if value := stringValue(ack["conversation_id"]); value != "" {
					s.conversation.ConversationID = value
				}
				if value := stringValue(ack["section_id"]); value != "" {
					s.conversation.SectionID = value
				}
				if value := stringValue(ack["local_conversation_id"]); value != "" {
					s.conversation.LocalConversationID = value
				}
			}
		case "FULL_MSG_NOTIFY":
			// model_type is informational; no client frame.
		case "STREAM_MSG_NOTIFY":
			if content, ok := payload["content"].(map[string]any); ok {
				if list, ok := content["content_block"].([]any); ok {
					for _, item := range list {
						if cb, ok := item.(map[string]any); ok {
							applyBlock(cb, "set")
						}
					}
				}
			}
		case "STREAM_CHUNK":
			if ops, ok := payload["patch_op"].([]any); ok {
				for _, item := range ops {
					op, _ := item.(map[string]any)
					if intValue(op["patch_object"]) != 1 {
						continue
					}
					value, _ := op["patch_value"].(map[string]any)
					list, _ := value["content_block"].([]any)
					mode := "append"
					if intValue(op["patch_type"]) == 2 {
						mode = "set"
					}
					for _, rawBlock := range list {
						if cb, ok := rawBlock.(map[string]any); ok {
							applyBlock(cb, mode)
						}
					}
				}
			}
		case "SSE_REPLY_END":
			s.finished = true
		case "STREAM_ERROR":
			message := stringValue(payload["error_msg"])
			if message == "" {
				message = stringValue(payload["message"])
			}
			if message == "" {
				message = "doubao upstream error"
			}
			kind := KindUpstream
			if strings.Contains(strings.ToLower(message), "rate limited") || strings.Contains(strings.ToLower(message), "verify") {
				kind = KindRateLimited
			}
			return &APIError{Kind: kind, Message: message, Code: payload["error_code"]}
		}
		return nil
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			if errors.Is(err, io.EOF) {
				break
			}
			s.sendError(&APIError{Kind: KindTransient, Message: "upstream stream read failed"})
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if data.Len() > 0 {
				event := eventName
				if event == "" {
					event = "message"
				}
				if handleErr := handleEvent(event, data.Bytes()); handleErr != nil {
					s.sendError(handleErr)
					return
				}
				data.Reset()
				eventName = ""
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				s.sendError(&APIError{Kind: KindTransient, Message: "upstream stream read failed"})
				return
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			s.sendError(&APIError{Kind: KindTransient, Message: "upstream stream read failed"})
			return
		}
	}
	if data.Len() > 0 {
		event := eventName
		if event == "" {
			event = "message"
		}
		if handleErr := handleEvent(event, data.Bytes()); handleErr != nil {
			s.sendError(handleErr)
			return
		}
	}
	if !s.finished {
		s.finished = true
	}
	select {
	case s.frames <- []byte("data: [DONE]\n\n"):
	case <-s.ctx.Done():
	}
}

func (s *chatStream) sendError(err error) {
	s.mu.Lock()
	s.sawError = err
	s.mu.Unlock()
	select {
	case s.errs <- err:
	case <-s.ctx.Done():
	}
}

func (s *chatStream) Next() ([]byte, error) {
	if s == nil {
		return nil, io.EOF
	}
	select {
	case frame, ok := <-s.frames:
		if !ok {
			s.mu.Lock()
			err := s.sawError
			s.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		return frame, nil
	case err := <-s.errs:
		return nil, err
	case <-s.ctx.Done():
		return nil, io.EOF
	}
}

func (s *chatStream) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		s.cancel()
		err = s.body.Close()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
	})
	return err
}

func (s *chatStream) UsageSnapshot() providers.UsageSnapshot {
	if s == nil {
		return providers.UsageSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

var _ providers.ChatStream = (*chatStream)(nil)
var _ providers.UsageSnapshotReporter = (*chatStream)(nil)

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		n, _ := typed.Int64()
		return int(n)
	}
	return 0
}
