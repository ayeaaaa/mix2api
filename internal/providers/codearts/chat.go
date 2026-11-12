package codearts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// chatCompletionsPath is v1's CodeArts SSE endpoint.
	chatCompletionsPath = "/api/v2/chat/completions"

	headerMaasType = "maas_type"
	maasBenefit    = "benefit"

	// disabledChatMessage is the exact v1 explanation shown when the provider
	// is disabled. The API surfaces it as a 400.
	disabledChatMessage = "CodeArts 出口未启用：请到设置页打开「启用 CodeArts 出口」。"

	// noAccountMessage is v1's last-resort explanation when the pool cannot
	// name a specific account reason.
	noAccountMessage = "没有可用的 CodeArts 账号：请在看板「账号」页添加并启用至少一个账号。"

	// allCreditsExhaustedMessage is v1's readable 400 after every candidate
	// reported an empty credit balance.
	allCreditsExhaustedMessage = "所有可用的 CodeArts 账号积分已耗尽，请领取积分或升级套餐。"

	defaultChatModel        = "GLM-5.2"
	defaultMaxOutputTokens  = int64(65536)
	chatErrorBodyMax        = 256 * 1024
	chatConcurrencyDelay    = 2 * time.Second
	chatConcurrencyAttempts = 3
)

// chatAllowedFields is the exact v1 CodeArts chat allowlist. Everything else,
// including conversation_id, stream_options, n, and client metadata, is
// stripped before the body reaches upstream.
var chatAllowedFields = map[string]struct{}{
	"model":                 {},
	"messages":              {},
	"temperature":           {},
	"top_p":                 {},
	"max_tokens":            {},
	"max_completion_tokens": {},
	"reasoning_effort":      {},
	"tools":                 {},
	"tool_choice":           {},
	"stop":                  {},
	"frequency_penalty":     {},
	"presence_penalty":      {},
	"seed":                  {},
	"response_format":       {},
	"top_k":                 {},
	"parallel_tool_calls":   {},
}

var chatToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// chatBuild is the account-independent normalized request. Per-account
// canonicalization and output-token clamping happen on a shallow copy.
type chatBuild struct {
	Body   map[string]any
	Model  string
	ChatID string
}

// OpenChat picks an account, ensures its token and catalogue, posts the v1
// allowlisted chat body, preflights the first SSE data event, and returns the
// normalized stream. The returned stream owns one acquired account slot;
// callers must Close it or read through io.EOF.
func (p *Provider) OpenChat(ctx context.Context, req providers.ChatRequest) (providers.ChatStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if p == nil || p.pool == nil {
		return nil, &APIError{Kind: KindUnavailable, Status: http.StatusBadGateway,
			Message: noAccountMessage}
	}

	// Chat is reached through the generic API, not a CodeArts admin handler.
	// Reload live settings here so an external settings save disables (or
	// re-enables) the realm without a process restart.
	cfg, cfgErr := p.applySettings()
	if cfgErr != nil {
		return nil, cfgErr
	}
	if !cfg.Enabled {
		return nil, &APIError{Kind: KindBadRequest, Status: http.StatusBadRequest,
			Message: disabledChatMessage}
	}
	streamTimeout := time.Duration(cfg.StreamTimeoutSeconds * float64(time.Second))
	if p.streamIdleTimeout > 0 {
		streamTimeout = p.streamIdleTimeout
	}

	build, buildErr := buildChatBody(req)
	if buildErr != nil {
		return nil, buildErr
	}

	accounts := p.pool.Accounts()
	attempts := len(accounts) + 1
	if attempts < 2 {
		attempts = 2
	}
	exclude := blockedCodeArtsAccounts(accounts, req.AllowedAccounts)
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		account, pickErr := p.pool.Pick(build.Model, req.SessionKey, exclude)
		if pickErr != nil {
			break
		}
		uid, _ := account.currentUID()
		if !p.pool.Acquire(account) {
			exclude[uid] = true
			continue
		}

		stream, attemptErr := p.openChatAccount(ctx, account, build, streamTimeout)
		if attemptErr == nil {
			_ = p.pool.NoteSuccess(account)
			boundUID, _ := account.currentUID()
			p.pool.BindSession(req.SessionKey, boundUID)
			return stream, nil
		}

		if currentUID, _ := account.currentUID(); currentUID != "" {
			uid = currentUID
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			p.pool.Release(account)
			return nil, ctxErr
		}

		var apiErr *APIError
		if !errors.As(attemptErr, &apiErr) {
			// Local failures are not upstream failures and must not fail over.
			p.pool.Release(account)
			return nil, attemptErr
		}

		lastErr = attemptErr
		switch apiErr.Kind {
		case KindBadRequest:
			// A rejected request is never retried on another account.
			p.pool.Release(account)
			return nil, attemptErr
		case KindCreditExhausted:
			// v1 releases before marking zero credits. The account is already
			// excluded from this request; a second request could acquire the
			// slot in the tiny release/mark window, which v1 accepts. All other
			// failure kinds update state before Release so cooling is visible
			// before another request can acquire.
			p.pool.Release(account)
			if markErr := account.MarkCreditExhausted(); markErr != nil {
				// The account is still excluded for this request; the state
				// write failure is not worth failing the failover over.
				_ = markErr
			}
			exclude[uid] = true
		case KindAuth:
			// v1 notes terminal errors before releasing, so a concurrent
			// request cannot acquire the account before it is disabled.
			_ = p.pool.NoteError(account, attemptErr.Error(), false, true)
			p.pool.Release(account)
			exclude[uid] = true
		case KindRateLimited, KindConcurrency:
			_ = p.pool.NoteError(account, attemptErr.Error(), true, false)
			p.pool.Release(account)
			exclude[uid] = true
		case KindUnavailable:
			_ = p.pool.NoteError(account, attemptErr.Error(), true, false)
			p.pool.Release(account)
			exclude[uid] = true
		default:
			// v1 treats an ordinary 5xx CodeArtsError as a normal error
			// (three strikes), not a soft 60s cooldown. Only 429/concurrency
			// and unavailable/network failures are soft.
			_ = p.pool.NoteError(account, attemptErr.Error(), false, false)
			p.pool.Release(account)
			exclude[uid] = true
		}
	}

	if lastErr != nil {
		var apiErr *APIError
		if errors.As(lastErr, &apiErr) && apiErr.Kind == KindCreditExhausted {
			return nil, &APIError{
				Kind:    KindCreditExhausted,
				Status:  http.StatusBadRequest,
				Message: allCreditsExhaustedMessage,
			}
		}
		return nil, lastErr
	}
	if p.allKnownCreditsExhausted() {
		return nil, &APIError{
			Kind:    KindCreditExhausted,
			Status:  http.StatusBadRequest,
			Message: allCreditsExhaustedMessage,
		}
	}
	return nil, p.noAccountError()
}

// openChatAccount performs one selected account's request. The caller owns the
// acquired slot; on error this method never releases it. Concurrency
// rejections are retried on the same account three times total with v1's
// 2s/4s backoff.
func (p *Provider) openChatAccount(
	ctx context.Context, account *Account, build *chatBuild, streamTimeout time.Duration,
) (providers.ChatStream, error) {
	if p.transport == nil {
		return nil, errors.New("codearts: chat transport is nil")
	}

	if p.tokenManager != nil {
		if err := p.tokenManager.Ensure(ctx, account); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			// A refresh may have rotated credentials even on the failure path;
			// classify the error against the current document.
			return nil, chatTokenError(err, currentAccountAuth(account))
		}
	}

	// Model discovery is best effort and bounded by v1's 8-second budget. A
	// stale/failed catalogue never blocks a chat request.
	modelsCtx, cancelModels := context.WithTimeout(ctx, modelsCatalogFetchTimeout)
	_, _ = p.ensureModels(modelsCtx, account, false)
	cancelModels()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	model := CanonicalModel(account, build.Model)
	body := cloneChatBody(build.Body)
	body["model"] = model
	if clampErr := clampChatOutputTokens(account, model, body); clampErr != nil {
		return nil, clampErr
	}

	sessionID := chatSHA256Hex32("codearts-session:" + build.ChatID)
	delay := p.chatRetryDelay
	if delay <= 0 {
		delay = chatConcurrencyDelay
	}

	var lastErr error
	for attempt := 0; attempt < chatConcurrencyAttempts; attempt++ {
		stream, attemptErr := p.sendModelChatAttempt(ctx, account, model, build.ChatID, sessionID, body, streamTimeout)
		if attemptErr == nil {
			return stream, nil
		}
		lastErr = attemptErr
		var apiErr *APIError
		if !errors.As(attemptErr, &apiErr) || apiErr.Kind != KindConcurrency || attempt == chatConcurrencyAttempts-1 {
			return nil, attemptErr
		}
		// v1 backs off 2s then 4s across its three concurrency attempts.
		if err := sleepChatContext(ctx, delay*time.Duration(attempt+1)); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// currentAccountAuth snapshots the live credential document. A token refresh
// can rotate it between the initial Ensure and a later request or stream
// error, so redaction must read the current value rather than an old copy.
func currentAccountAuth(account *Account) Auth {
	if account == nil {
		return Auth{}
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.Auth
}

// sendModelChatAttempt sends one model spelling and retries the alternate
// casing once when InferHub reports a model-ID/permission mismatch. The
// accepted spelling is cached per account so later requests skip the failed
// attempt.
func (p *Provider) sendModelChatAttempt(
	ctx context.Context,
	account *Account,
	model, chatID, sessionID string,
	body map[string]any,
	streamTimeout time.Duration,
) (providers.ChatStream, error) {
	canonical := CanonicalModel(account, model)
	key := strings.ToLower(canonical)
	first, alternate := canonical, strings.ToLower(canonical)
	if original := strings.TrimSpace(model); canonical == key && strings.EqualFold(canonical, original) {
		alternate = original
	}
	if cached, ok := account.chatModelIDs.Load(key); ok {
		if accepted, _ := cached.(string); accepted != "" && accepted != canonical {
			first, alternate = accepted, canonical
		}
	}

	spellings := []string{first}
	if alternate != "" && alternate != first {
		spellings = append(spellings, alternate)
	}
	var firstErr error
	for index, spelling := range spellings {
		attemptBody := cloneChatBody(body)
		attemptBody["model"] = spelling
		raw, err := json.Marshal(attemptBody)
		if err != nil {
			return nil, &APIError{Kind: KindBadRequest, Status: http.StatusBadRequest,
				Message: "codearts: chat request body is not valid JSON"}
		}
		stream, err := p.sendChatAttempt(ctx, account, spelling, chatID, sessionID, raw, streamTimeout)
		if err == nil {
			if index > 0 {
				account.chatModelIDs.Store(key, spelling)
			}
			return stream, nil
		}
		if index == 0 {
			firstErr = err
			if !isCodeArtsModelIDError(err) {
				return nil, err
			}
			continue
		}
		// Preserve the original permission error when the alternate is also
		// rejected: an unregistered alternate spelling did not resolve the
		// original denial.
		if isCodeArtsModelAccessError(firstErr) && isCodeArtsModelIDError(err) {
			return nil, firstErr
		}
		return nil, err
	}
	return nil, firstErr
}

// isCodeArtsModelAccessError detects the explicit "account may not call this
// model" response. InferHub sometimes reports a casing mismatch with the same
// code, so callers retry a different spelling before treating it as terminal.
func isCodeArtsModelAccessError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	message := strings.ToLower(apiErr.Message)
	return strings.Contains(message, "tm.000010051") ||
		strings.Contains(apiErr.Message, "无权调用该模型")
}

// isCodeArtsModelIDError also accepts the unregistered-model code used when
// the alternate spelling is wrong.
func isCodeArtsModelIDError(err error) bool {
	if isCodeArtsModelAccessError(err) {
		return true
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.Message), "inferhub.002002009")
}

// sendChatAttempt sends one POST, classifies non-2xx responses, and preflights
// the first SSE data event before returning a stream.
func (p *Provider) sendChatAttempt(
	ctx context.Context,
	account *Account,
	model, chatID, sessionID string,
	raw []byte,
	streamTimeout time.Duration,
) (providers.ChatStream, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	request, err := p.newChatRequest(streamCtx, account, model, chatID, sessionID, raw)
	if err != nil {
		cancel()
		return nil, newChatError(KindUpstream, http.StatusBadGateway,
			"codearts: build chat request failed: "+err.Error(), currentAccountAuth(account))
	}

	response, err := p.transport.Do(streamCtx, request, account.ProxyURL())
	if err != nil {
		cancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, newChatError(KindUnavailable, http.StatusBadGateway,
			"连不上 CodeArts 聊天接口："+err.Error(), currentAccountAuth(account))
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, chatErrorBodyMax))
		_ = response.Body.Close()
		cancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, classifyChatHTTP(response.StatusCode, body, currentAccountAuth(account))
	}

	// v1 does not gzip request bodies. Go's transport may negotiate gzip for
	// the response's Accept-Encoding, but the request body stays plain JSON.
	stream := newCodeArtsStream(ctx, cancel, response.Body, p.pool, account, model, streamTimeout)
	if preflightErr := stream.preflight(); preflightErr != nil {
		// Preflight failures have not returned a stream, so the caller still
		// owns the slot and releases it.
		stream.abort()
		return nil, preflightErr
	}
	return stream, nil
}

// chatTokenError classifies a token Ensure failure. Terminal credential
// rejections disable the account; transport/endpoint failures are soft.
func chatTokenError(err error, auth Auth) *APIError {
	if errors.Is(err, ErrRefreshTokenMissing) || errors.Is(err, ErrTokenRejected) {
		return newChatError(KindAuth, http.StatusBadGateway,
			"CodeArts 账号令牌已失效："+err.Error(), auth)
	}
	return newChatError(KindUnavailable, http.StatusBadGateway,
		"CodeArts 令牌续期失败："+err.Error(), auth)
}

func sleepChatContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newChatRequest builds and signs one chat request. Chat-Id, Session-Id, and
// the x-ot-* headers are appended after signing so they are not part of
// SignedHeaders, matching v1.
func (p *Provider) newChatRequest(
	ctx context.Context,
	account *Account,
	model, chatID, sessionID string,
	raw []byte,
) (*http.Request, error) {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, snapHost()+chatCompletionsPath, bytes.NewReader(raw),
	)
	if err != nil {
		return nil, err
	}

	account.mu.Lock()
	auth := account.Auth
	uid := account.uidLocked()
	account.mu.Unlock()

	traceID, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("x-auth-token", auth.CloudDragonToken)
	request.Header.Set("x-snap-traceid", traceID)
	request.Header.Set("X-Language", "zh-cn")
	request.Header.Set("app-id", "CodeAgent3.0")
	request.Header.Set("is_confidential", "false")
	if account.IsBenefit(model) {
		request.Header.Set(headerMaasType, maasBenefit)
	}

	if err := SignRequest(request, raw, SignCredentials{
		AccessKeyID:     auth.AccessKeyID,
		SecretAccessKey: auth.SecretAccessKey,
		SecurityToken:   auth.CloudDragonToken,
	}); err != nil {
		return nil, err
	}

	request.Header.Set("Chat-Id", chatID)
	request.Header.Set("Session-Id", sessionID)
	if otSessionEnabled() {
		otSession := chatSHA256Hex32("codearts-ot-session:" + uid)
		otTrace, traceErr := randomHex(16)
		if traceErr != nil {
			return nil, traceErr
		}
		request.Header.Set("x-ot-session-id", otSession)
		request.Header.Set("user-session-id", otSession)
		request.Header.Set("x-ot-trace-id", otTrace)
	}
	return request, nil
}

// otSessionEnabled mirrors v1's WB_CODEARTS_OT_SESSION switch. The default is
// on; only 0/false/no/off disables the gateway session headers.
func otSessionEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(envString("WB_CODEARTS_OT_SESSION", ""))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func classifyChatHTTP(status int, raw []byte, auth Auth) *APIError {
	message := chatErrorText(raw)
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("CodeArts 返回 HTTP %d", status)
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return newChatError(KindAuth, status, message, auth)
	case http.StatusBadRequest:
		switch {
		case isConcurrencyMessage(message):
			return newChatError(KindConcurrency, status, message, auth)
		case isCreditExhaustedMessage(message):
			return newChatError(KindCreditExhausted, status, message, auth)
		default:
			return newChatError(KindBadRequest, status, message, auth)
		}
	case http.StatusTooManyRequests:
		return newChatError(KindRateLimited, status, message, auth)
	default:
		return newChatError(KindUpstream, status, message, auth)
	}
}

// chatErrorText extracts v1's `_error_text` fields from an upstream error.
func chatErrorText(raw []byte) string {
	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err == nil && decoded != nil {
		for _, key := range []string{"error_msg", "message", "error_description", "error"} {
			value := decoded[key]
			if nested, ok := value.(map[string]any); ok {
				for _, nestedKey := range []string{"message", "error_msg", "detail"} {
					if text := strings.TrimSpace(modelText(nested[nestedKey])); text != "" {
						return text
					}
				}
				continue
			}
			if text := strings.TrimSpace(modelText(value)); text != "" {
				return text
			}
		}
		code := decoded["error_code"]
		if chatZeroCode(code) {
			code = decoded["code"]
		}
		if !chatZeroCode(code) {
			return "error_code=" + modelText(code)
		}
	}
	return strings.TrimSpace(string(raw))
}

func isConcurrencyMessage(message string) bool {
	text := strings.ToLower(message)
	for _, marker := range []string{
		"并发会话", "tm.00001041", "concurrent session", "maximum number of active",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func isCreditExhaustedMessage(message string) bool {
	text := strings.ToLower(message)
	for _, marker := range []string{
		"积分已耗尽", "积分不足", "余额不足",
		"insufficient credit", "credit exhausted", "credits exhausted",
		"no credit", "not enough credit",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// buildChatBody normalizes the payload into the v1 CodeArts wire body.
func buildChatBody(req providers.ChatRequest) (*chatBuild, *APIError) {
	if req.Raw == nil {
		req.Raw = make(map[string]json.RawMessage, 2)
		if strings.TrimSpace(req.Model) != "" {
			req.Raw["model"] = mustChatJSON(req.Model)
		}
		if len(req.Messages) > 0 {
			req.Raw["messages"] = mustChatJSON(req.Messages)
		}
	}

	payload := make(map[string]any, len(req.Raw))
	for key, value := range req.Raw {
		decoded, err := decodeChatValue(value)
		if err != nil {
			return nil, &APIError{Kind: KindBadRequest, Status: http.StatusBadRequest,
				Message: "codearts: request body is not valid JSON"}
		}
		payload[key] = decoded
	}

	body := make(map[string]any, len(chatAllowedFields))
	for key := range chatAllowedFields {
		if value, ok := payload[key]; ok {
			body[key] = value
		}
	}
	// Accept legacy function declarations as an input alias, not an upstream
	// field. Dropping them here would recreate the PR #57 failure mode: an
	// agent supplied function schemas, but the model receives none.
	if _, hasTools := payload["tools"]; !hasTools {
		if functions, exists := payload["functions"]; exists {
			body["functions"] = functions
			// Converting declarations must also preserve the legacy client's
			// selection. In particular function_call=none must not become auto.
			if _, modernChoice := payload["tool_choice"]; !modernChoice {
				switch choice := payload["function_call"].(type) {
				case string:
					mode := strings.ToLower(strings.TrimSpace(choice))
					if mode == "none" || mode == "auto" {
						body["tool_choice"] = mode
					}
				case map[string]any:
					if name, ok := choice["name"].(string); ok && strings.TrimSpace(name) != "" {
						body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": strings.TrimSpace(name)}}
					}
				}
			}
		}
	}
	model := defaultChatModel
	if provided, ok := payload["model"]; ok {
		if text := chatTruthyText(provided); text != "" {
			model = text
		}
	}
	body["model"] = model

	effort := requestedChatEffort(payload)
	if effort != "" {
		allowed := reasoningEfforts[strings.ToLower(model)]
		if len(allowed) > 0 && !chatContainsString(allowed, effort) {
			if effort == "none" && !chatContainsString(allowed, "none") {
				// Always-thinking models cannot disable reasoning; use their
				// first accepted level instead of failing the whole request.
				effort = allowed[0]
			} else {
				return nil, &APIError{Kind: KindBadRequest, Status: http.StatusBadRequest,
					Message: fmt.Sprintf("reasoning_effort must be one of %s for %s",
						strings.Join(allowed, "/"), model)}
			}
		}
		body["reasoning_effort"] = effort
	} else {
		delete(body, "reasoning_effort")
	}

	rawMessages, ok := payload["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, &APIError{Kind: KindBadRequest, Status: http.StatusBadRequest,
			Message: "messages is empty"}
	}
	messages := normalizeChatMessages(rawMessages)
	messages = mergeAdjacentToolCalls(messages)
	messages = repackToolResultBlocks(messages)
	messages = cleanupOrphanToolCalls(messages)
	if len(messages) == 0 {
		return nil, &APIError{Kind: KindBadRequest, Status: http.StatusBadRequest,
			Message: "messages is empty"}
	}
	body["messages"] = messages

	normalizeChatTools(body)
	normalizeChatToolChoice(body)

	if stop, ok := body["stop"].(string); ok {
		body["stop"] = []any{stop}
	}
	if completion, ok := body["max_completion_tokens"]; ok && completion != nil {
		if maxTokens, hasMaxTokens := body["max_tokens"]; !hasMaxTokens || maxTokens == nil {
			body["max_tokens"] = completion
			delete(body, "max_completion_tokens")
		}
	}

	chatID := ""
	if candidate := chatTruthyText(payload["conversation_id"]); validChatID(candidate) {
		chatID = candidate
	}
	if chatID == "" {
		chatID = deriveChatID(req.SessionKey, messages)
	}
	body["chat_id"] = chatID
	body["prompt_cache_key"] = chatID
	body["stream"] = true
	body["tool_stream"] = true

	return &chatBuild{Body: body, Model: model, ChatID: chatID}, nil
}

func mustChatJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("null")
	}
	return encoded
}

func decodeChatValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err
}

// chatTruthyText mirrors Python's `str(value or "")` for scalar payloads.
func chatTruthyText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		if number, err := typed.Float64(); err == nil && number == 0 {
			return ""
		}
		return typed.String()
	case float64:
		if typed == 0 {
			return ""
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		if typed == 0 {
			return ""
		}
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case bool:
		if !typed {
			return ""
		}
		return "true"
	case int:
		if typed == 0 {
			return ""
		}
		return strconv.Itoa(typed)
	case int64:
		if typed == 0 {
			return ""
		}
		return strconv.FormatInt(typed, 10)
	case uint64:
		if typed == 0 {
			return ""
		}
		return strconv.FormatUint(typed, 10)
	default:
		return ""
	}
}

// requestedChatEffort resolves every common reasoning spelling: snake case,
// camel case, thinking.type, and enable_thinking.
func requestedChatEffort(payload map[string]any) string {
	if effort, ok := payload["reasoning_effort"]; ok && effort != nil {
		return strings.ToLower(strings.TrimSpace(modelText(effort)))
	}
	if effort, ok := payload["reasoningEffort"]; ok && effort != nil {
		return strings.ToLower(strings.TrimSpace(modelText(effort)))
	}
	if thinking, ok := payload["thinking"].(map[string]any); ok {
		switch strings.ToLower(strings.TrimSpace(modelText(thinking["type"]))) {
		case "disabled":
			return "none"
		case "enabled":
			return "high"
		}
	}
	if value, ok := payload["enable_thinking"]; ok {
		if modelTruthy(value) {
			return "high"
		}
		return "none"
	}
	return ""
}

func chatContainsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validChatID(value string) bool {
	text := strings.TrimSpace(value)
	if len(text) != 32 {
		return false
	}
	_, err := hex.DecodeString(text)
	return err == nil
}

func deriveChatID(sessionKey string, messages []any) string {
	if sessionKey != "" {
		return chatSHA256Hex32(sessionKey)
	}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message == nil || message["role"] != "user" {
			continue
		}
		text, _ := message["content"].(string)
		if text != "" {
			return chatSHA256Hex32("codearts:" + text)
		}
	}
	random, err := randomHex(16)
	if err == nil {
		return random
	}
	return chatSHA256Hex32(fmt.Sprintf("codearts-chat:%d", time.Now().UnixNano()))
}

func chatSHA256Hex32(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:32]
}

func normalizeChatMessages(raw []any) []any {
	out := make([]any, 0, len(raw))
	for _, value := range raw {
		message, ok := value.(map[string]any)
		if !ok || message == nil {
			continue
		}
		if message["role"] == "developer" {
			message["role"] = "system"
		}
		if content, ok := message["content"].([]any); ok {
			message["content"] = flattenChatContent(content)
		}
		out = append(out, message)
	}
	return out
}

// flattenChatContent normalizes OpenAI, Anthropic, and Responses content parts
// into CodeArts' text string or image_url array shape.
func flattenChatContent(content []any) any {
	parts := make([]any, 0, len(content))
	for _, value := range content {
		if text, ok := value.(string); ok {
			parts = append(parts, map[string]any{"type": "text", "text": text})
			continue
		}
		piece, ok := value.(map[string]any)
		if !ok || piece == nil {
			continue
		}
		ptype := strings.TrimSpace(modelText(piece["type"]))
		switch ptype {
		case "text", "input_text", "output_text", "summary_text":
			if text, exists := piece["text"]; exists && text != nil {
				parts = append(parts, map[string]any{"type": "text", "text": modelText(text)})
			}
			continue
		}
		if ptype != "image_url" && ptype != "input_image" && ptype != "image" {
			if _, hasImage := piece["image_url"]; !hasImage {
				continue
			}
		}

		raw := piece["image_url"]
		if raw == nil {
			raw = piece["url"]
		}
		url := ""
		detail := ""
		if nested, ok := raw.(map[string]any); ok {
			url = strings.TrimSpace(modelText(nested["url"]))
			detail = strings.TrimSpace(modelText(nested["detail"]))
		} else if raw != nil {
			url = strings.TrimSpace(modelText(raw))
		}
		if url == "" {
			if source, ok := piece["source"].(map[string]any); ok {
				switch strings.TrimSpace(modelText(source["type"])) {
				case "base64":
					media := strings.TrimSpace(modelText(source["media_type"]))
					if media == "" {
						media = "image/png"
					}
					data := modelText(source["data"])
					if data != "" {
						url = "data:" + media + ";base64," + data
					}
				case "url":
					url = strings.TrimSpace(modelText(source["url"]))
				}
			}
		}
		if url == "" {
			if data := modelText(piece["data"]); data != "" {
				mime := strings.TrimSpace(modelText(piece["mimeType"]))
				if mime == "" {
					mime = strings.TrimSpace(modelText(piece["mime_type"]))
				}
				if mime == "" {
					mime = "image/png"
				}
				url = "data:" + mime + ";base64," + data
			}
		}
		if url == "" {
			continue
		}
		image := map[string]any{"url": url}
		if detail != "" {
			image["detail"] = detail
		}
		parts = append(parts, map[string]any{"type": "image_url", "image_url": image})
	}

	for _, part := range parts {
		if item, _ := part.(map[string]any); item != nil && item["type"] == "image_url" {
			return parts
		}
	}
	var text strings.Builder
	for _, part := range parts {
		item, _ := part.(map[string]any)
		text.WriteString(modelText(item["text"]))
	}
	return text.String()
}

func chatEmptyContent(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case []map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func chatList(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []map[string]any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = item
		}
		return out
	default:
		return nil
	}
}

func mergeChatReasoning(dst, src map[string]any) {
	text, ok := src["reasoning_content"].(string)
	if !ok || text == "" {
		return
	}
	if previous, ok := dst["reasoning_content"].(string); ok && previous != "" {
		dst["reasoning_content"] = previous + "\n" + text
		return
	}
	dst["reasoning_content"] = text
}

// mergeAdjacentToolCalls port of v1's repair: adjacent assistant tool-call
// messages are one batch, and a split plain assistant folds into a preceding
// content-less tool-call assistant.
func mergeAdjacentToolCalls(messages []any) []any {
	if len(messages) < 2 {
		return messages
	}
	out := make([]any, 0, len(messages))
	changed := false
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok || message == nil || message["role"] != "assistant" || len(out) == 0 {
			out = append(out, value)
			continue
		}
		previous, ok := out[len(out)-1].(map[string]any)
		if !ok || previous == nil || previous["role"] != "assistant" {
			out = append(out, value)
			continue
		}
		calls := chatList(message["tool_calls"])
		previousCalls := chatList(previous["tool_calls"])
		if len(calls) > 0 && chatEmptyContent(message["content"]) && len(previousCalls) > 0 {
			previous["tool_calls"] = append(previousCalls, calls...)
			mergeChatReasoning(previous, message)
			changed = true
			continue
		}
		if _, hasCalls := message["tool_calls"]; !hasCalls {
			if text, ok := message["content"].(string); ok && text != "" &&
				len(previousCalls) > 0 && chatEmptyContent(previous["content"]) {
				previous["content"] = text
				mergeChatReasoning(previous, message)
				changed = true
				continue
			}
		}
		out = append(out, value)
	}
	if !changed {
		return messages
	}
	return out
}

// repackToolResultBlocks moves non-result messages out from between a tool
// call batch and its results.
func repackToolResultBlocks(messages []any) []any {
	if len(messages) < 3 {
		return messages
	}
	out := make([]any, 0, len(messages))
	changed := false
	for index := 0; index < len(messages); {
		message, _ := messages[index].(map[string]any)
		calls := chatList(message["tool_calls"])
		if message == nil || message["role"] != "assistant" || len(calls) == 0 {
			out = append(out, messages[index])
			index++
			continue
		}
		wanted := make(map[string]bool, len(calls))
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			if id, ok := call["id"].(string); ok && id != "" {
				wanted[id] = true
			}
		}
		out = append(out, messages[index])
		index++

		results := make([]any, 0)
		between := make([]any, 0)
		for index < len(messages) {
			next, ok := messages[index].(map[string]any)
			if !ok || next == nil {
				break
			}
			if next["role"] == "tool" {
				id, _ := next["tool_call_id"].(string)
				if !wanted[id] {
					break
				}
				results = append(results, messages[index])
				index++
				continue
			}
			if next["role"] == "assistant" {
				break
			}
			between = append(between, messages[index])
			index++
		}
		if len(results) > 0 {
			out = append(out, results...)
			out = append(out, between...)
			if len(between) > 0 {
				changed = true
			}
		} else {
			out = append(out, between...)
		}
	}
	if !changed {
		return messages
	}
	return out
}

// cleanupOrphanToolCalls drops unanswered calls and unmatched tool results.
func cleanupOrphanToolCalls(messages []any) []any {
	callIDs := make(map[string]bool)
	resultIDs := make(map[string]bool)
	for _, value := range messages {
		message, _ := value.(map[string]any)
		if message == nil {
			continue
		}
		if message["role"] == "tool" {
			if id, ok := message["tool_call_id"].(string); ok && id != "" {
				resultIDs[id] = true
			}
			continue
		}
		if message["role"] == "assistant" {
			for _, raw := range chatList(message["tool_calls"]) {
				call, _ := raw.(map[string]any)
				if id, ok := call["id"].(string); ok && id != "" {
					callIDs[id] = true
				}
			}
		}
	}
	if len(callIDs) == 0 && len(resultIDs) == 0 {
		return messages
	}
	keep := make(map[string]bool)
	for id := range callIDs {
		if resultIDs[id] {
			keep[id] = true
		}
	}

	changed := false
	for _, value := range messages {
		message, _ := value.(map[string]any)
		if message == nil || message["role"] != "assistant" {
			continue
		}
		calls := chatList(message["tool_calls"])
		if len(calls) == 0 {
			continue
		}
		kept := make([]any, 0, len(calls))
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			id, _ := call["id"].(string)
			if id != "" && keep[id] {
				kept = append(kept, raw)
			}
		}
		if len(kept) == len(calls) {
			continue
		}
		changed = true
		if len(kept) > 0 {
			message["tool_calls"] = kept
		} else {
			delete(message, "tool_calls")
		}
	}

	out := make([]any, 0, len(messages))
	for _, value := range messages {
		message, _ := value.(map[string]any)
		if message != nil && message["role"] == "tool" {
			id, _ := message["tool_call_id"].(string)
			if !keep[id] {
				changed = true
				continue
			}
		}
		out = append(out, value)
	}
	if !changed {
		return messages
	}
	return out
}

func normalizeChatTools(body map[string]any) {
	if functions, exists := body["functions"]; exists {
		if _, hasTools := body["tools"]; !hasTools {
			tools := make([]any, 0)
			for _, raw := range chatList(functions) {
				if function, ok := raw.(map[string]any); ok {
					tools = append(tools, map[string]any{"type": "function", "function": function})
				}
			}
			body["tools"] = tools
		}
		delete(body, "functions")
	}
	rawTools := chatList(body["tools"])
	if len(rawTools) == 0 {
		delete(body, "tools")
		return
	}
	normalized := make([]any, 0, len(rawTools))
	for _, value := range rawTools {
		item, ok := value.(map[string]any)
		if !ok || item == nil {
			continue
		}
		var function map[string]any
		if _, hasFunction := item["function"]; !hasFunction {
			if _, hasName := item["name"]; !hasName {
				continue
			}
			function = map[string]any{
				"name":        modelText(item["name"]),
				"description": modelText(item["description"]),
				"parameters":  item["parameters"],
			}
			if strict, exists := item["strict"]; exists {
				function["strict"] = strict
			}
			item = map[string]any{"type": "function", "function": function}
		} else {
			copied := make(map[string]any, len(item)+1)
			for key, nested := range item {
				copied[key] = nested
			}
			item = copied
			item["type"] = "function"
			function, _ = item["function"].(map[string]any)
			if function == nil {
				continue
			}
			functionCopy := make(map[string]any, len(function)+1)
			for key, nested := range function {
				functionCopy[key] = nested
			}
			item["function"] = functionCopy
			function = functionCopy
		}
		normalizeChatToolParameters(function)
		normalized = append(normalized, item)
	}

	valid := make([]any, 0, len(normalized))
	seen := make(map[string]bool, len(normalized))
	for _, value := range normalized {
		item, _ := value.(map[string]any)
		function, _ := item["function"].(map[string]any)
		if function == nil {
			continue
		}
		name := strings.TrimSpace(modelText(function["name"]))
		if !chatToolNamePattern.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		valid = append(valid, value)
	}
	if len(valid) == 0 {
		delete(body, "tools")
		return
	}
	body["tools"] = valid
}

func normalizeChatToolParameters(function map[string]any) {
	parameters, ok := function["parameters"].(map[string]any)
	if !ok || parameters == nil {
		function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		return
	}
	copied := make(map[string]any, len(parameters)+2)
	for key, value := range parameters {
		copied[key] = value
	}
	copied["type"] = "object"
	if properties, ok := copied["properties"].(map[string]any); !ok || properties == nil {
		copied["properties"] = map[string]any{}
	}
	function["parameters"] = deepNormalizeChatSchema(copied)
}

// chatSchemaTypeAliases and chatSchemaValidTypes mirror v1's JSON-schema
// repair for the upstream Go validator.
var chatSchemaTypeAliases = map[string]string{
	"str": "string", "text": "string", "int": "integer", "bool": "boolean",
	"dict": "object", "list": "array", "float": "number",
}

var chatSchemaValidTypes = map[string]bool{
	"string": true, "number": true, "integer": true, "boolean": true,
	"array": true, "object": true, "null": true,
}

// deepNormalizeChatSchema recursively repairs the nested tool parameter schema
// shapes v1 handles: type aliases, null metadata, object properties, required
// lists, array items, definitions, and anyOf/oneOf/allOf branches.
func deepNormalizeChatSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}

	for _, key := range []string{"description", "title", "format", "pattern", "items", "default"} {
		if value, exists := schema[key]; exists && value == nil {
			delete(schema, key)
		}
	}

	if rawType, exists := schema["type"]; exists {
		switch typed := rawType.(type) {
		case string:
			kind := strings.ToLower(strings.TrimSpace(typed))
			if alias, ok := chatSchemaTypeAliases[kind]; ok {
				schema["type"] = alias
			} else if chatSchemaValidTypes[kind] {
				schema["type"] = kind
			} else {
				if _, hasProperties := schema["properties"]; hasProperties {
					schema["type"] = "object"
				} else if _, hasItems := schema["items"]; hasItems {
					schema["type"] = "array"
				} else {
					delete(schema, "type")
				}
			}
		case []any:
			normalized := make([]any, 0, len(typed))
			for _, value := range typed {
				kind := strings.ToLower(strings.TrimSpace(modelText(value)))
				if kind == "" {
					continue
				}
				if alias, ok := chatSchemaTypeAliases[kind]; ok {
					kind = alias
				}
				normalized = append(normalized, kind)
			}
			schema["type"] = normalized
		}
	}

	if schema["type"] == "object" || (schema["properties"] != nil && schema["type"] == nil) {
		if schema["type"] == nil {
			schema["type"] = "object"
		}
		if properties, ok := schema["properties"].(map[string]any); !ok || properties == nil {
			schema["properties"] = map[string]any{}
		}
	}

	if rawRequired, exists := schema["required"]; exists {
		switch required := rawRequired.(type) {
		case []any:
			cleaned := make([]any, 0, len(required))
			for _, value := range required {
				if value == nil {
					continue
				}
				text := strings.TrimSpace(modelText(value))
				if text != "" {
					cleaned = append(cleaned, text)
				}
			}
			if len(cleaned) > 0 {
				schema["required"] = cleaned
			} else {
				delete(schema, "required")
			}
		case string:
			if text := strings.TrimSpace(required); text != "" {
				schema["required"] = []any{text}
			} else {
				delete(schema, "required")
			}
		default:
			delete(schema, "required")
		}
	}

	if rawItems, exists := schema["items"]; exists {
		switch items := rawItems.(type) {
		case []any:
			var first map[string]any
			for _, value := range items {
				if candidate, ok := value.(map[string]any); ok && candidate != nil {
					first = candidate
					break
				}
			}
			if first != nil {
				copied := make(map[string]any, len(first))
				for key, value := range first {
					copied[key] = value
				}
				schema["items"] = deepNormalizeChatSchema(copied)
			} else {
				schema["items"] = map[string]any{"type": "string"}
			}
		case map[string]any:
			if items == nil {
				schema["items"] = map[string]any{"type": "string"}
			} else {
				deepNormalizeChatSchema(items)
			}
		default:
			delete(schema, "items")
		}
	}

	if properties, ok := schema["properties"].(map[string]any); ok && properties != nil {
		for _, value := range properties {
			if nested, ok := value.(map[string]any); ok && nested != nil {
				deepNormalizeChatSchema(nested)
			}
		}
	}

	for _, key := range []string{"$defs", "definitions"} {
		definitions, ok := schema[key].(map[string]any)
		if !ok || definitions == nil {
			continue
		}
		for _, value := range definitions {
			if nested, ok := value.(map[string]any); ok && nested != nil {
				deepNormalizeChatSchema(nested)
			}
		}
	}

	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		branches, ok := schema[key].([]any)
		if !ok {
			continue
		}
		for _, value := range branches {
			if nested, ok := value.(map[string]any); ok && nested != nil {
				deepNormalizeChatSchema(nested)
			}
		}
	}

	return schema
}

// normalizeChatToolChoice narrows a named function choice to that one tool and
// uses the string "required" the upstream accepts.
func normalizeChatToolChoice(body map[string]any) {
	choice, _ := body["tool_choice"].(map[string]any)
	if choice != nil {
		if kind, _ := choice["type"].(string); strings.EqualFold(strings.TrimSpace(kind), "none") {
			if len(chatList(body["tools"])) > 0 {
				body["tool_choice"] = "none"
			} else {
				delete(body, "tool_choice")
			}
			return
		}
		function, _ := choice["function"].(map[string]any)
		name := strings.TrimSpace(modelText(function["name"]))
		tools := chatList(body["tools"])
		if name != "" && len(tools) > 0 {
			filtered := make([]any, 0, 1)
			for _, value := range tools {
				tool, _ := value.(map[string]any)
				toolFunction, _ := tool["function"].(map[string]any)
				if strings.TrimSpace(modelText(toolFunction["name"])) == name {
					filtered = append(filtered, value)
				}
			}
			if len(filtered) > 0 {
				body["tools"] = filtered
				body["tool_choice"] = "required"
			} else {
				delete(body, "tool_choice")
			}
			return
		}
		delete(body, "tool_choice")
	}
	if value, ok := body["tool_choice"].(string); ok && strings.EqualFold(strings.TrimSpace(value), "none") {
		body["tool_choice"] = "none"
	}
	if len(chatList(body["tools"])) == 0 {
		delete(body, "tool_choice")
	}
}

func cloneChatBody(body map[string]any) map[string]any {
	copied := make(map[string]any, len(body))
	for key, value := range body {
		copied[key] = value
	}
	return copied
}

var errChatOutputToken = errors.New("output token value is invalid")

// clampChatOutputTokens validates and clamps both output-token spellings using
// the account catalogue, the local seed table, and the 65536 default.
func clampChatOutputTokens(account *Account, model string, body map[string]any) *APIError {
	limit := chatOutputTokenLimit(account, model)
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		value, exists := body[key]
		if !exists {
			continue
		}
		requested, present, err := chatOutputTokenValue(value)
		if err != nil {
			return &APIError{Kind: KindBadRequest, Status: http.StatusBadRequest,
				Message: key + " must be a positive integer"}
		}
		if !present {
			continue
		}
		if requested > limit {
			requested = limit
		}
		body[key] = requested
	}
	return nil
}

func chatOutputTokenLimit(account *Account, model string) int64 {
	key := strings.ToLower(strings.TrimSpace(model))
	if account != nil && key != "" {
		account.mu.Lock()
		models := cloneModels(account.Models)
		account.mu.Unlock()
		for _, item := range models {
			id := strings.ToLower(strings.TrimSpace(modelText(item["id"])))
			if id != key {
				continue
			}
			for _, field := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens"} {
				if value := modelInt(item[field]); value > 0 {
					return value
				}
			}
		}
	}
	if limits, ok := workbuddyFallbackLimits[key]; ok && limits[1] > 0 {
		return int64(limits[1])
	}
	return defaultMaxOutputTokens
}

func chatOutputTokenValue(value any) (int64, bool, error) {
	if value == nil {
		return 0, false, nil
	}
	if _, ok := value.(bool); ok {
		return 0, false, errChatOutputToken
	}
	requested, ok := chatOutputTokenInt(value)
	if !ok || requested <= 0 {
		return 0, false, errChatOutputToken
	}
	return requested, true, nil
}

// chatOutputTokenInt mirrors Python int(): integral numbers and numeric strings
// are accepted, integral floats truncate toward zero, booleans and fractional
// strings are rejected.
func chatOutputTokenInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return number, true
		}
		number, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return chatFloatToInt64(number), true
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return 0, false
		}
		number, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return 0, false
		}
		return number, true
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint64:
		if typed > uint64(1<<63-1) {
			return 0, false
		}
		return int64(typed), true
	case float64:
		return chatFloatToInt64(typed), true
	case float32:
		return chatFloatToInt64(float64(typed)), true
	default:
		return 0, false
	}
}

func chatFloatToInt64(number float64) int64 {
	if number >= float64(1<<63-1) {
		return 1<<63 - 1
	}
	if number <= -float64(1<<63) {
		return -1 << 63
	}
	return int64(number)
}

// allKnownCreditsExhausted reports whether every enabled account already has a
// zero-credit snapshot. In that state v1's scheduler has no candidate at all;
// the chat contract surfaces the readable 400 instead of v1's generic 502.
func (p *Provider) allKnownCreditsExhausted() bool {
	if p == nil || p.pool == nil {
		return false
	}
	enabled := 0
	for _, account := range p.pool.Accounts() {
		account.mu.Lock()
		disabled := account.removed || account.State.Disabled || account.State.ManualDisabled
		exhausted := account.creditExhaustedLocked()
		account.mu.Unlock()
		if disabled {
			continue
		}
		enabled++
		if !exhausted {
			return false
		}
	}
	return enabled > 0
}

// noAccountError builds v1's readable no-account explanation with at most two
// account reasons. The generic API maps it to v1's 502, not 503.
func (p *Provider) noAccountError() error {
	now := p.pool.nowTime()
	reasons := make([]string, 0, 2)
	for _, account := range p.pool.Accounts() {
		account.mu.Lock()
		uid := account.uidLocked()
		nickname := strings.TrimSpace(account.Auth.UserName)
		if nickname == "" {
			nickname = uid
		}
		disabled := account.State.Disabled || account.State.ManualDisabled
		credit := account.creditExhaustedLocked()
		cooling := !disabled && !credit && account.State.CoolUntil > unixSeconds(now)
		reason := account.State.Reason
		if reason == "" {
			reason = account.State.LastError
		}
		lastError := account.State.LastError
		secrets := codeartsSecretValues(account.Auth)
		account.mu.Unlock()

		switch {
		case disabled:
			if strings.TrimSpace(reason) == "" {
				reason = "手动停用"
			}
			reason = truncateRunes(redactText(reason, secrets), maxStateNoteRunes)
			reasons = append(reasons, nickname+" 已停用："+reason)
		case credit:
			reasons = append(reasons, nickname+" 积分已耗尽")
		case cooling:
			if strings.TrimSpace(lastError) == "" {
				lastError = "稍后重试"
			}
			lastError = truncateRunes(redactText(lastError, secrets), maxStateNoteRunes)
			reasons = append(reasons, nickname+" 冷却中："+lastError)
		}
		if len(reasons) >= 2 {
			break
		}
	}
	message := noAccountMessage
	if len(reasons) > 0 {
		message = "没有可用的 CodeArts 账号：" + strings.Join(reasons, "；")
	}
	return &APIError{Kind: KindUnavailable, Status: http.StatusBadGateway, Message: message}
}

// blockedCodeArtsAccounts excludes accounts outside a per-key allow-list.
func blockedCodeArtsAccounts(accounts []*Account, allowed []string) map[string]bool {
	if len(allowed) == 0 {
		return map[string]bool{}
	}
	permitted := make(map[string]bool, len(allowed))
	for _, uid := range allowed {
		permitted[uid] = true
	}
	blocked := make(map[string]bool)
	for _, account := range accounts {
		if account == nil {
			continue
		}
		uid, _ := account.currentUID()
		if uid != "" && !permitted[uid] {
			blocked[uid] = true
		}
	}
	return blocked
}
