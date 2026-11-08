package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// ChatPath is the v1 Raccoon chat completion endpoint. The upstream always
	// speaks OpenAI-style SSE.
	ChatPath = "/api/web/llm/v2/chat/completions"

	// disabledChatMessage is the exact v1 explanation shown when the provider
	// is disabled. The API layer surfaces it as a 400.
	disabledChatMessage = "Raccoon 出口未启用：请到设置页打开「启用 Raccoon 出口」。"

	// noAccountMessage is v1's last-resort explanation when the pool cannot
	// name a specific account reason.
	noAccountMessage = "没有可用的 Raccoon 账号：请在看板「账号」页导入并启用至少一个账号。"
)

// KindUnavailable means no local account can serve the request. It is distinct
// from an upstream 5xx so callers can map it to 503 with a readable reason.
const KindUnavailable ErrorKind = 6

// chatAllowedFields is the exact v1 CHAT_ALLOWED_FIELDS allowlist. Everything
// else (client metadata, legacy functions, stream_options, n, logit_bias, ...)
// is dropped before the body reaches upstream.
var chatAllowedFields = map[string]struct{}{
	"model":                 {},
	"messages":              {},
	"tools":                 {},
	"tool_choice":           {},
	"parallel_tool_calls":   {},
	"temperature":           {},
	"top_p":                 {},
	"stream":                {},
	"stop":                  {},
	"max_tokens":            {},
	"max_completion_tokens": {},
	"presence_penalty":      {},
	"frequency_penalty":     {},
	"user":                  {},
	"seed":                  {},
	"response_format":       {},
	"reasoning_effort":      {},
	"thinking":              {},
	"enable_thinking":       {},
}

// OpenChat picks an account, ensures its token, POSTs the v1 allowlisted chat
// body, preflights the first SSE event, and returns the pass-through stream.
//
// The returned stream owns one acquired account slot. Callers must Close it or
// read it through io.EOF.
func (p *Pool) OpenChat(ctx context.Context, req providers.ChatRequest) (providers.ChatStream, error) {
	if p == nil {
		return nil, errors.New("raccoon: pool is nil")
	}
	if ctx == nil {
		return nil, errors.New("raccoon: nil context")
	}

	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()
	if !cfg.Enabled {
		return nil, &APIError{
			Kind:    KindBadRequest,
			Status:  http.StatusBadRequest,
			Message: disabledChatMessage,
		}
	}

	body, model, err := buildChatBody(req)
	if err != nil {
		return nil, err
	}
	client := p.HTTPClient()
	tokens := p.TokenManager()

	// v1 warms only unknown balances when a reserve guard is configured. A
	// fresh/headless install may reach chat before the dashboard ever refreshed
	// credits; without this the reserve guard cannot block spending.
	p.prewarmReserves(ctx)

	accounts := p.Accounts()
	attempts := len(accounts) + 1
	if attempts < 2 {
		attempts = 2
	}
	exclude := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < attempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		account, pickErr := p.Pick(model, req.SessionKey, exclude)
		if pickErr != nil {
			// v1 waits briefly while the only candidates are at their
			// concurrency limit, so a transient burst does not immediately
			// become a 503.
			if p.anyBusy() && len(exclude) < len(accounts) {
				if sleepErr := sleepContext(ctx, 50*time.Millisecond); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			break
		}

		uid, _ := account.currentUID()
		if !p.Acquire(account) {
			exclude[uid] = true
			continue
		}

		stream, attemptErr := p.openChatAccount(ctx, account, body, model, client, tokens, cfg)
		if attemptErr == nil {
			_ = p.NoteSuccess(account)
			// A refresh may have rotated the JWT subject while the request
			// was in flight; bind the session to the live UID.
			boundUID, _ := account.currentUID()
			p.BindSession(req.SessionKey, boundUID)
			return stream, nil
		}
		p.Release(account)
		// Use the current UID for exclusion too: a failed refresh may have
		// rotated the subject before the request was rejected.
		if currentUID, _ := account.currentUID(); currentUID != "" {
			uid = currentUID
		}

		if errors.Is(attemptErr, context.Canceled) || errors.Is(attemptErr, context.DeadlineExceeded) {
			return nil, attemptErr
		}

		var apiErr *APIError
		if !errors.As(attemptErr, &apiErr) {
			// Local failures (filesystem, programming errors) are not upstream
			// failures and must not trigger failover, matching v1's generic
			// exception branch.
			return nil, attemptErr
		}

		switch apiErr.Kind {
		case KindBadRequest:
			// A rejected request is never retried on another account.
			return nil, attemptErr
		case KindAuth:
			_ = p.NoteError(account, attemptErr.Error(), false, true)
			exclude[uid] = true
			lastErr = attemptErr
		case KindRateLimited:
			_ = p.NoteError(account, attemptErr.Error(), true, false)
			exclude[uid] = true
			lastErr = attemptErr
		case KindUpstream:
			soft := apiErr.Status == 0 ||
				apiErr.Status == http.StatusTooManyRequests ||
				apiErr.Status >= http.StatusInternalServerError
			_ = p.NoteError(account, attemptErr.Error(), soft, false)
			exclude[uid] = true
			lastErr = attemptErr
		case KindTransient:
			// A transport/network failure has no HTTP status and is soft like
			// v1's RaccoonUnavailable. An unmapped HTTP status keeps v1's
			// ordinary consecutive-error accounting.
			_ = p.NoteError(account, attemptErr.Error(), apiErr.Status == 0, false)
			exclude[uid] = true
			lastErr = attemptErr
		default:
			_ = p.NoteError(account, attemptErr.Error(), false, false)
			exclude[uid] = true
			lastErr = attemptErr
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, p.noAccountError(model)
}

// prewarmReserveTimeout is v1's per-account credit fetch budget before Pick.
const prewarmReserveTimeout = 6 * time.Second

// prewarmReserves fetches credits only for enabled accounts whose reserve guard
// is active but whose balance is still unknown. Failures are swallowed: Pick
// will use whatever guard state exists and v1 treats this as best effort.
func (p *Pool) prewarmReserves(ctx context.Context) {
	if p == nil {
		return
	}
	needs := false
	candidates := make([]*Account, 0)
	for _, account := range p.Accounts() {
		account.mu.Lock()
		unknown := account.ReserveCredits > 0 && account.Credits == nil &&
			!account.State.Disabled && !account.State.ManualDisabled
		account.mu.Unlock()
		if unknown {
			needs = true
			candidates = append(candidates, account)
		}
	}
	if !needs {
		return
	}
	for _, account := range candidates {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return
		}
		fetchCtx, cancel := context.WithTimeout(ctx, prewarmReserveTimeout)
		_, _ = p.FetchCredits(fetchCtx, account, false)
		cancel()
	}
}

// openChatAccount performs one selected account's request. The caller owns the
// acquired slot; on error no bookkeeping is released here.
//
// A concurrent re-import (or any credential rewrite) can land while the token
// manager is refreshing; Task 9 surfaces that race as ErrCredentialsChanged.
// The slot is still ours, so retry this account once with the freshly published
// generation/credentials instead of leaking a client-visible local error.
func (p *Pool) openChatAccount(
	ctx context.Context,
	account *Account,
	body map[string]json.RawMessage,
	model string,
	client *HTTPClient,
	tokens *TokenManager,
	cfg Config,
) (providers.ChatStream, error) {
	if client == nil || client.client == nil {
		return nil, errors.New("raccoon: chat http client is not configured")
	}
	upstream := cloneRawBody(body)
	if err := p.clampOutputTokens(account, model, upstream); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(upstream)
	if err != nil {
		return nil, &APIError{
			Kind:    KindBadRequest,
			Status:  http.StatusBadRequest,
			Message: "raccoon: chat request body is not valid JSON",
		}
	}

	stream, attemptErr := p.runChatAccountAttempt(ctx, account, encoded, model, client, tokens, cfg)
	if errors.Is(attemptErr, ErrCredentialsChanged) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// Retry once from the top so Ensure and the generation snapshot use the
		// current credentials. This is the same acquired slot, never a second
		// Acquire.
		stream, attemptErr = p.runChatAccountAttempt(ctx, account, encoded, model, client, tokens, cfg)
	}
	if errors.Is(attemptErr, ErrCredentialsChanged) {
		// Do not expose a Task 9 internal credential race to the client. A
		// second consecutive rewrite becomes a transient failure so the pool
		// can fail over or cooldown normally.
		return nil, normalizeChatTokenError(attemptErr)
	}
	return stream, attemptErr
}

// runChatAccountAttempt ensures the token then sends at most two requests
// (initial + one forced-refresh retry for 401/403).
func (p *Pool) runChatAccountAttempt(
	ctx context.Context,
	account *Account,
	encoded []byte,
	model string,
	client *HTTPClient,
	tokens *TokenManager,
	cfg Config,
) (providers.ChatStream, error) {
	if ensureErr := p.ensureChatToken(ctx, tokens, account); ensureErr != nil {
		if errors.Is(ensureErr, ErrCredentialsChanged) {
			return nil, ensureErr
		}
		return nil, normalizeChatTokenError(ensureErr)
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		generation := uint64(0)
		if tokens != nil {
			generation = tokens.Generation(account)
		}
		snapshot := account.refreshCredentials()
		if snapshot.Deleted {
			return nil, &APIError{
				Kind:    KindAuth,
				Status:  http.StatusUnauthorized,
				Message: "raccoon: account was deleted",
			}
		}
		// A refresh may have rotated credentials between Generation and the
		// header snapshot. Pin the generation that produced the actual token so
		// RefreshIfGeneration does not repeat an already-completed refresh.
		if snapshot.Generation != generation {
			generation = snapshot.Generation
		}

		stream, sendErr := p.sendChatRequest(
			ctx, account, client, chatHeaders(snapshot.Auth), encoded, model,
			authSecretValues(snapshot.Auth), cfg,
		)
		if sendErr != nil {
			lastErr = sendErr
			if !IsAuthError(sendErr) || attempt != 0 || tokens == nil {
				return nil, sendErr
			}
			if refreshErr := p.refreshChatToken(ctx, tokens, account, generation); refreshErr != nil {
				if errors.Is(refreshErr, ErrCredentialsChanged) {
					return nil, refreshErr
				}
				return nil, normalizeChatTokenError(refreshErr)
			}
			continue
		}

		if preflightErr := stream.preflight(); preflightErr != nil {
			stream.abort()
			if IsAuthError(preflightErr) && attempt == 0 && tokens != nil {
				if refreshErr := p.refreshChatToken(ctx, tokens, account, generation); refreshErr != nil {
					if errors.Is(refreshErr, ErrCredentialsChanged) {
						return nil, refreshErr
					}
					return nil, normalizeChatTokenError(refreshErr)
				}
				continue
			}
			return nil, preflightErr
		}
		return stream, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &APIError{
		Kind:    KindAuth,
		Status:  http.StatusUnauthorized,
		Message: "raccoon: rejected after token refresh",
	}
}

func (p *Pool) ensureChatToken(ctx context.Context, tokens *TokenManager, account *Account) error {
	if p.chatEnsureHook != nil {
		return p.chatEnsureHook(ctx, account)
	}
	if tokens == nil {
		return nil
	}
	return tokens.Ensure(ctx, account)
}

func (p *Pool) refreshChatToken(
	ctx context.Context,
	tokens *TokenManager,
	account *Account,
	generation uint64,
) error {
	if p.chatRefreshHook != nil {
		return p.chatRefreshHook(ctx, account, generation)
	}
	if tokens == nil {
		return nil
	}
	return tokens.RefreshIfGeneration(ctx, account, generation)
}

// sendChatRequest sends one streaming POST through the account's proxy. The
// returned stream owns the response body. The header wait is bounded by the
// configured timeout, but body reads use a per-read idle timeout that resets on
// every upstream chunk; there is no absolute whole-stream deadline.
func (p *Pool) sendChatRequest(
	ctx context.Context,
	account *Account,
	client *HTTPClient,
	headers http.Header,
	body []byte,
	model string,
	secrets []string,
	cfg Config,
) (*chatStream, error) {
	timeout := chatStreamTimeout(cfg)
	streamCtx, cancel := context.WithCancel(ctx)
	startedAt := p.timeNow()

	request, err := http.NewRequestWithContext(
		streamCtx, http.MethodPost, client.resolve(ChatPath), bytes.NewReader(body),
	)
	if err != nil {
		cancel()
		return nil, &APIError{Kind: KindTransient, Message: "raccoon: build chat request failed"}
	}
	request.Header = headers.Clone()

	type headerResult struct {
		response *http.Response
		err      error
	}
	headersCh := make(chan headerResult, 1)
	go func() {
		response, requestErr := client.client.Do(streamCtx, request, p.ProxyURL(account))
		headersCh <- headerResult{response: response, err: requestErr}
	}()
	timer := time.NewTimer(timeout)
	var response *http.Response
	select {
	case result := <-headersCh:
		timer.Stop()
		response = result.response
		err = result.err
	case <-ctx.Done():
		timer.Stop()
		cancel()
		result := <-headersCh
		if result.response != nil {
			_ = result.response.Body.Close()
		}
		return nil, ctx.Err()
	case <-timer.C:
		cancel()
		result := <-headersCh
		if result.response != nil {
			_ = result.response.Body.Close()
		}
		return nil, &APIError{
			Kind:    KindTransient,
			Message: "raccoon: chat stream timed out",
		}
	}

	if err != nil {
		cancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &APIError{
			Kind:    KindTransient,
			Message: redactText("raccoon: cannot reach upstream: "+err.Error(), secrets...),
		}
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// A body-read failure must not erase the status classification: a 401
		// with a truncated body is still an auth failure, a 400 is still a bad
		// request, and so on. newAPIError handles an empty/partial body by
		// producing a status-labelled, redacted message.
		raw, _ := io.ReadAll(io.LimitReader(response.Body, maxHTTPBodyBytes))
		_ = response.Body.Close()
		cancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, newAPIError(response.StatusCode, raw, secrets...)
	}

	return newChatStream(ctx, streamCtx, cancel, response.Body, p, account, model, secrets, timeout, startedAt), nil
}

// chatStreamTimeout reads cfg.StreamTimeoutSeconds with v1's default. The
// config loader clamps stored values; direct test configs may still be zero.
func chatStreamTimeout(cfg Config) time.Duration {
	seconds := cfg.StreamTimeoutSeconds
	if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		seconds = defaultStreamTimeoutSeconds
	}
	if seconds > maxStreamTimeoutSeconds {
		seconds = maxStreamTimeoutSeconds
	}
	return time.Duration(seconds * float64(time.Second))
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// anyBusy reports v1's pool-level concurrency check: a healthy account is at
// its slot limit even though Pick could not select it.
func (p *Pool) anyBusy() bool {
	now := time.Now()
	for _, account := range p.Accounts() {
		account.mu.Lock()
		busy := account.healthyLocked(now) &&
			account.activeConcurrent >= normalizedMaxConcurrent(account.maxConcurrent)
		account.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}

// noAccountError builds v1's readable no-account explanation. At most two
// account reasons are included, matching v1's reasons[:2].
func (p *Pool) noAccountError(model string) error {
	now := time.Now()
	reasons := make([]string, 0, 2)

	for _, account := range p.Accounts() {
		account.mu.Lock()
		nickname := strings.TrimSpace(account.Auth.OfficeOrgName)
		if nickname == "" {
			nickname = strings.TrimSpace(account.Auth.UID)
		}
		if nickname == "" {
			nickname = strings.TrimSpace(account.State.UID)
		}

		disabled := account.State.Disabled || account.State.ManualDisabled
		cooling := !disabled && now.Before(account.State.CoolUntil)
		guard := ""
		if !disabled && !cooling {
			guard = account.guardBlockedLocked(model)
		}
		secrets := authSecretValues(account.Auth)

		reason := ""
		switch {
		case disabled:
			reason = strings.TrimSpace(account.State.Reason)
			if reason == "" {
				reason = "手动停用"
			}
		case cooling:
			reason = strings.TrimSpace(account.State.LastError)
			if reason == "" {
				reason = "稍后重试"
			}
		case guard == "reserve":
			reason = fmt.Sprintf(
				"积分低于预留阈值（%s <= %d）",
				creditsRemainText(account.Credits), account.ReserveCredits,
			)
		case guard == "daily_token":
			reason = fmt.Sprintf(
				"今日 token 已达上限（%s/%d）",
				int64PointerText(account.DailyTokensToday), account.DailyTokenLimit,
			)
		case guard == "model_daily_token":
			reason = fmt.Sprintf("模型 %s 今日 token 已达上限", model)
		case guard == "daily_credit":
			reason = fmt.Sprintf(
				"今日积分已达上限（%s/%d）",
				float64PointerText(account.DailyCreditsToday), account.DailyCreditLimit,
			)
		case account.activeConcurrent >= normalizedMaxConcurrent(account.maxConcurrent):
			reason = "并发已满"
		}
		account.mu.Unlock()

		if reason == "" {
			continue
		}
		if disabled || cooling {
			reason = truncateUTF8(redactText(reason, secrets...), maxErrorMessageBytes)
		}
		switch {
		case disabled:
			reasons = append(reasons, fmt.Sprintf("%s 已停用：%s", nickname, reason))
		case cooling:
			reasons = append(reasons, fmt.Sprintf("%s 冷却中：%s", nickname, reason))
		default:
			reasons = append(reasons, fmt.Sprintf("%s %s", nickname, reason))
		}
		if len(reasons) >= 2 {
			break
		}
	}

	message := noAccountMessage
	if len(reasons) > 0 {
		message = "没有可用的 Raccoon 账号：" + strings.Join(reasons, "；")
	}
	return &APIError{
		Kind:    KindUnavailable,
		Status:  http.StatusServiceUnavailable,
		Message: message,
	}
}

func creditsRemainText(credits *CreditsSnapshot) string {
	if credits == nil || credits.Remain == nil {
		return "0"
	}
	return catalogText(credits.Remain)
}

func int64PointerText(value *int64) string {
	if value == nil {
		return "0"
	}
	return strconv.FormatInt(*value, 10)
}

func float64PointerText(value *float64) string {
	if value == nil {
		return "0"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

// buildChatBody reduces an OpenAI chat payload to the v1 allowlist. When Raw
// is missing it falls back to the contract's Model/Messages fields.
func buildChatBody(req providers.ChatRequest) (map[string]json.RawMessage, string, error) {
	raw := req.Raw
	if raw == nil {
		raw = make(map[string]json.RawMessage, 2)
		trimmedModel := strings.TrimSpace(req.Model)
		if trimmedModel != "" {
			raw["model"] = mustRawJSON(trimmedModel)
		}
		if len(req.Messages) > 0 {
			raw["messages"] = mustRawJSON(req.Messages)
		}
	}

	body := make(map[string]json.RawMessage, len(chatAllowedFields))
	for key, value := range raw {
		if _, allowed := chatAllowedFields[key]; !allowed {
			continue
		}
		if !json.Valid(value) {
			return nil, "", &APIError{
				Kind:    KindBadRequest,
				Status:  http.StatusBadRequest,
				Message: "raccoon: request body is not valid JSON",
			}
		}
		body[key] = append(json.RawMessage(nil), value...)
	}

	model, hasModel := body["model"]
	modelName := ""
	if hasModel {
		modelName = decodeChatModel(model)
	}
	if modelName == "" {
		return nil, "", &APIError{
			Kind:    KindBadRequest,
			Status:  http.StatusBadRequest,
			Message: "model is required",
		}
	}
	body["model"] = mustRawJSON(modelName)

	rawMessages, ok := body["messages"]
	if !ok {
		return nil, "", &APIError{
			Kind:    KindBadRequest,
			Status:  http.StatusBadRequest,
			Message: "messages is empty",
		}
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil || len(messages) == 0 {
		return nil, "", &APIError{
			Kind:    KindBadRequest,
			Status:  http.StatusBadRequest,
			Message: "messages is empty",
		}
	}
	body["messages"] = mustRawJSON(messages)
	body["stream"] = json.RawMessage("true")
	return body, modelName, nil
}

func mustRawJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Callers only pass JSON-marshalable values.
		return json.RawMessage("null")
	}
	return encoded
}

func decodeChatModel(raw json.RawMessage) string {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		// v1 uses `body.get("model") or ""`: JSON 0 is falsy.
		if number, err := typed.Float64(); err == nil && number == 0 {
			return ""
		}
		return strings.TrimSpace(typed.String())
	case bool:
		if !typed {
			return ""
		}
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

func cloneRawBody(body map[string]json.RawMessage) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(body))
	for key, value := range body {
		cloned[key] = append(json.RawMessage(nil), value...)
	}
	return cloned
}

// clampOutputTokens validates and clamps both OpenAI output-token spellings.
// A nil value is left untouched, matching v1's `body.get(key) is None`.
func (p *Pool) clampOutputTokens(
	account *Account,
	model string,
	body map[string]json.RawMessage,
) error {
	limit := p.modelOutputLimit(account, model)
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		raw, ok := body[key]
		if !ok {
			continue
		}
		requested, present, err := decodeOutputTokens(raw)
		if err != nil {
			return &APIError{
				Kind:    KindBadRequest,
				Status:  http.StatusBadRequest,
				Message: key + " must be a positive integer",
			}
		}
		if !present {
			continue
		}
		if limit > 0 && requested > limit {
			requested = limit
		}
		body[key] = json.RawMessage(strconv.FormatInt(requested, 10))
	}
	return nil
}

var errOutputTokenValue = errors.New("output token value is invalid")

func decodeOutputTokens(raw json.RawMessage) (int64, bool, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return 0, false, err
	}
	if value == nil {
		return 0, false, nil
	}
	if _, isBool := value.(bool); isBool {
		return 0, false, errOutputTokenValue
	}
	requested, ok := outputTokenInt(value)
	if !ok || requested <= 0 {
		return 0, false, errOutputTokenValue
	}
	return requested, true, nil
}

// outputTokenInt mirrors Python int(): integral numbers and numeric strings
// are accepted, integral floats are truncated toward zero, and booleans,
// fractions-as-strings, arrays, and objects are rejected.
func outputTokenInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return number, true
		}
		number, err := typed.Float64()
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, false
		}
		return float64ToInt64(number), true
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
	default:
		return 0, false
	}
}

func float64ToInt64(number float64) int64 {
	if number >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	if number <= float64(math.MinInt64) {
		return math.MinInt64
	}
	return int64(number)
}

// modelOutputLimit mirrors v1's read-only _model_output_limit: the selected
// account's catalog, then the shared last-good catalog, then the seed table.
func (p *Pool) modelOutputLimit(account *Account, model string) int64 {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return 0
	}
	if account != nil {
		account.mu.Lock()
		// Deep-copy the published slice under Account.mu so the scan below
		// cannot race a concurrent adopt/re-import of the field.
		models := cloneModels(account.Models)
		account.mu.Unlock()
		if limit := catalogOutputLimit(models, key); limit > 0 {
			return limit
		}
	}
	// v1's _model_output_limit reads the shared cache list directly, without
	// checking whether it came from a live Raccoon fetch.
	p.catalogMu.Lock()
	shared := cloneModels(p.catalog.models)
	p.catalogMu.Unlock()
	if limit := catalogOutputLimit(shared, key); limit > 0 {
		return limit
	}
	if seed, ok := seedModelsByID[key]; ok {
		return seed.maxTokens
	}
	return 0
}

func catalogOutputLimit(models []providers.Model, key string) int64 {
	for _, item := range models {
		id, _ := item["id"].(string)
		if strings.ToLower(strings.TrimSpace(id)) != key {
			continue
		}
		limit := catalogInt(item["max_tokens"])
		if limit <= 0 {
			limit = catalogInt(item["max_completion_tokens"])
		}
		if limit > 0 {
			return limit
		}
	}
	return 0
}

// chatHeaders mirrors v1's desktop-client SSE headers exactly. X-Org-Code is
// only sent for a real organization identity.
func chatHeaders(auth Auth) http.Header {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+cleanToken(auth.AccessToken))
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("X-Client-Platform", ClientPlatform)
	headers.Set("X-Client-Version", ClientVersion)
	headers.Set("X-Raccoon-Language", ClientLanguage)
	headers.Set("Cache-Control", "no-store")
	identity := strings.TrimSpace(auth.OfficeIdentity)
	if identity != "" &&
		!strings.EqualFold(identity, "personal") &&
		!strings.EqualFold(identity, "none") {
		headers.Set("X-Org-Code", identity)
	}
	return headers
}

func normalizeChatTokenError(err error) error {
	switch {
	case errors.Is(err, ErrAccountDeleted):
		return &APIError{
			Kind:    KindAuth,
			Status:  http.StatusUnauthorized,
			Message: "raccoon: account was deleted",
		}
	case errors.Is(err, ErrCredentialsChanged):
		return &APIError{
			Kind:    KindTransient,
			Message: "raccoon: account credentials changed during token refresh",
		}
	case err == nil:
		return nil
	default:
		return err
	}
}
