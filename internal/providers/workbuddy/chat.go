package workbuddy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// chatPath is v1's CHAT_PATH: the upstream only ever speaks streaming
	// OpenAI-compatible SSE here. The generic API aggregates it for callers
	// that asked for a non-streaming response.
	chatPath = "/v2/chat/completions"

	defaultChatSystemPrompt = "You are a helpful assistant."

	// maxChatErrorBody bounds upstream error reads so a broken/WAF HTML page
	// cannot grow memory without limit.
	maxChatErrorBody = 64 << 10

	// chatDefaultHeaderTimeout and chatDefaultIdleTimeout mirror v1's effective
	// upstream defaults. Config supplies the live values.
	chatDefaultHeaderTimeout = 120 * time.Second
	chatDefaultIdleTimeout   = 300 * time.Second

	// accountRateCooldown is v1's first credential-level soft 429 tier. Model
	// windows start shorter (60s) and extend to the upstream reset clock.
	accountRateCooldown = 600 * time.Second
	modelRateCooldown   = 60 * time.Second

	// vscodeDeviceTokenError mirrors the official extension's failure state:
	// a non-enterprise VSCode account always sends token or this placeholder.
	vscodeDeviceTokenError = "sdk_not_initialized"

	// cliCacheRetention mirrors the official CLI's resolveCacheRetention.
	cliCacheRetention = "long"
)

var (
	// cliBodyDropFields are the Responses API/client metadata fields the
	// official CLI never sends.
	cliBodyDropFields = []string{"metadata", "conversation_id", "previous_response_id"}

	chatResetAtPatternEnglish = regexp.MustCompile(`(?i)(?:reset at|将在)\s*(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`)
	chatResetAtPatternBare    = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})\s*UTC[+-]\d{1,2}`)
	chatResetOffsetPattern    = regexp.MustCompile(`(?i)UTC([+-]\d{1,2})(?::?(\d{2}))?`)

	chatToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

	accountScopePhrases = []string{
		"account-level", "account level", "account-wide", "account scope",
		"account quota", "account rate limit", "account limit",
		"per account", "per-account", "this account", "your account",
		"the account", "an account",
		"credential-level", "credential level", "credential scope",
		"credential limit", "per credential", "this credential",
		"the credential", "your credential",
		"账号级", "账户级", "该账号", "此账号", "该账户", "此账户",
		"账号限流", "账户限流", "凭证级", "该凭证",
	}

	restrictionPhrases = []string{
		"account has been restricted", "account is restricted",
		"账号存在异常行为", "已被限制使用", "账号被限制",
	}
)

// SetPeer wires the sibling WorkBuddy provider used for the opt-in
// international→China fallback. app composition calls this after constructing
// both realm instances (Task 11). The fallback remains disabled unless the
// intl realm's live auto_switch_product setting is true.
func (p *Provider) SetPeer(peer *Provider) {
	if p == nil {
		return
	}
	if peer == p {
		peer = nil
	}
	p.mu.Lock()
	p.peer = peer
	p.mu.Unlock()
}

// Peer returns the provider wired by SetPeer, if any.
func (p *Provider) Peer() *Provider {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.peer
}

// OpenChat starts one WorkBuddy chat completion. The upstream is always asked
// for SSE; non-streaming clients are aggregated by the generic API.
func (p *Provider) OpenChat(ctx context.Context, req providers.ChatRequest) (providers.ChatStream, error) {
	if p == nil {
		return nil, newAPIError(KindUnavailable, http.StatusServiceUnavailable, "WorkBuddy provider is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cfg := p.refreshChatConfig()
	if !cfg.Enabled {
		return nil, newAPIError(KindInvalid, http.StatusBadRequest, "WorkBuddy provider is disabled")
	}
	return p.openChat(ctx, req, cfg, true)
}

// refreshChatConfig reloads this realm's shared settings on every request so
// panel edits (timeouts, auto-switch, concurrency) apply without a restart.
func (p *Provider) refreshChatConfig() Config {
	if p == nil {
		return Config{}
	}
	cfg := LoadConfig(p.store, p.realm)
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	if pool := p.Pool(); pool != nil {
		pool.SetConfig(cfg)
	}
	return cfg
}

func (p *Provider) openChat(
	ctx context.Context,
	req providers.ChatRequest,
	cfg Config,
	allowFallback bool,
) (providers.ChatStream, error) {
	pool := p.Pool()
	if pool == nil {
		return nil, newAPIError(KindUnavailable, http.StatusServiceUnavailable, "WorkBuddy account pool is unavailable")
	}

	baseBody, model, err := p.buildChatBody(req)
	if err != nil {
		return nil, err
	}
	turnRequestID := newTurnRequestID()
	vscodeConversationID := newMessageID()

	accounts := pool.Accounts()
	attemptLimit := len(accounts) + 1
	if attemptLimit < 2 {
		attemptLimit = 2
	}
	exclude := make(map[string]bool)
	refreshed := make(map[string]bool)
	var lastErr error
	sawTransient := false
	retriedTransient := false

	for attempt := 0; attempt < attemptLimit; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		account, pickErr := pool.Pick(model, req.SessionKey, exclude)
		if pickErr != nil {
			// v1 clears the tried set once after a transient/network failure so
			// a one-account pool gets a real second upstream attempt instead of
			// failing on the first hiccup. 429s are windows, not transport
			// blips, and intentionally do not reset the rotation.
			if sawTransient && !retriedTransient {
				retriedTransient = true
				exclude = make(map[string]bool)
				attemptLimit += len(accounts) + 1
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(20 * time.Millisecond):
				}
				continue
			}
			if wait := p.modelCooldownWait(model, exclude); wait > 0 {
				lastErr = &APIError{
					Kind:        KindRateLimited,
					Status:      http.StatusTooManyRequests,
					Message:     "every WorkBuddy account is waiting for the upstream model window to reset",
					RetryAfter:  wait,
					ResetAt:     time.Now().Add(wait),
					ModelScoped: true,
				}
			}
			break
		}

		uid := strings.TrimSpace(account.UID())
		if uid == "" {
			exclude[""] = true
			continue
		}
		if !pool.Acquire(account) {
			exclude[uid] = true
			continue
		}

		if tokenErr := p.EnsureToken(ctx, account); tokenErr != nil {
			pool.Release(account)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			currentUID := strings.TrimSpace(account.UID())
			if currentUID == "" {
				currentUID = uid
			}
			exclude[currentUID] = true
			lastErr = tokenErr
			continue
		}

		stream, attemptErr := p.sendChatWithAuthRetry(
			ctx, account, baseBody, model, req, cfg, turnRequestID, vscodeConversationID, refreshed,
		)
		if attemptErr == nil {
			_ = pool.NoteSuccess(account, model)
			pool.BindSession(req.SessionKey, account.UID())
			return stream, nil
		}

		pool.Release(account)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		currentUID := strings.TrimSpace(account.UID())
		if currentUID == "" {
			currentUID = uid
		}
		exclude[currentUID] = true

		var apiErr *APIError
		if errors.As(attemptErr, &apiErr) {
			if apiErr.Kind == KindUpstream || apiErr.Kind == KindNetwork {
				sawTransient = true
			}
			if apiErr.Kind != KindInvalid && req.SessionKey != "" {
				pool.DemoteSession(req.SessionKey, currentUID)
			}
			switch apiErr.Kind {
			case KindInvalid, KindContentRejected:
				// The same request is invalid/rejected for every credential.
				return nil, apiErr
			case KindAuth:
				if apiErr.Terminal {
					_ = pool.NoteError(account, apiErr.SafeMessage(), false, true)
				} else {
					_ = pool.NoteError(account, apiErr.SafeMessage(), false, false)
				}
			case KindRateLimited:
				if apiErr.ModelScoped {
					_ = pool.NoteModelError(account, model, apiErr.SafeMessage(), apiErr.RetryAfter)
				} else {
					wait := apiErr.RetryAfter
					if wait <= 0 {
						wait = accountRateCooldown
					}
					_ = pool.NoteCooldown(account, apiErr.SafeMessage(), wait)
				}
			case KindRestricted:
				_ = pool.NoteRestricted(account, apiErr.SafeMessage())
			default:
				_ = pool.NoteError(account, apiErr.SafeMessage(), false, false)
			}
		}
		lastErr = attemptErr
	}

	primaryErr := p.finalChatError(lastErr, model, exclude)
	if allowFallback && p.shouldRealmFallback(req, model, primaryErr, cfg) {
		peer := p.Peer()
		peerCfg := peer.refreshChatConfig()
		if !peerCfg.Enabled {
			return nil, primaryErr
		}
		return peer.openChat(ctx, req, peerCfg, false)
	}
	return nil, primaryErr
}

func (p *Provider) finalChatError(lastErr error, model string, exclude map[string]bool) error {
	if lastErr != nil {
		var apiErr *APIError
		if errors.As(lastErr, &apiErr) {
			return apiErr
		}
		if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
			return lastErr
		}
		return newAPIError(KindAuth, http.StatusBadGateway,
			"WorkBuddy account credentials could not be prepared; sign in again")
	}
	if wait := p.modelCooldownWait(model, exclude); wait > 0 {
		return &APIError{
			Kind:        KindRateLimited,
			Status:      http.StatusTooManyRequests,
			Message:     "every WorkBuddy account is waiting for the upstream model window to reset",
			RetryAfter:  wait,
			ResetAt:     time.Now().Add(wait),
			ModelScoped: true,
		}
	}
	message := "no usable WorkBuddy account is available"
	if strings.TrimSpace(p.realm) != "" {
		message = fmt.Sprintf("no usable WorkBuddy account is available for realm %q; add or enable one at the dashboard", p.realm)
	}
	return newAPIError(KindUnavailable, http.StatusServiceUnavailable, message)
}

func (p *Provider) modelCooldownWait(model string, exclude map[string]bool) time.Duration {
	pool := p.Pool()
	if pool == nil {
		return 0
	}
	now := pool.nowTime()
	var earliest time.Time
	for _, account := range pool.Accounts() {
		uid := strings.TrimSpace(account.UID())
		if uid == "" || exclude[uid] {
			continue
		}
		account.mu.RLock()
		if account.removed || !account.Auth.Enabled || account.State.Disabled || account.tokenExpired {
			account.mu.RUnlock()
			continue
		}
		until, ok := account.modelCoolUntil[strings.ToLower(strings.TrimSpace(model))]
		account.mu.RUnlock()
		if !ok || !until.After(now) {
			continue
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	if earliest.IsZero() {
		return 0
	}
	wait := earliest.Sub(now)
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

func (p *Provider) shouldRealmFallback(req providers.ChatRequest, model string, primaryErr error, cfg Config) bool {
	if p == nil || p.realm != realmIntl || !cfg.AutoSwitchProduct {
		return false
	}
	if req.AllowRealmFallback != nil && !*req.AllowRealmFallback {
		return false
	}
	if req.Headers != nil && strings.TrimSpace(req.Headers.Get("X-Realm")) != "" {
		return false
	}
	peer := p.Peer()
	if peer == nil || peer.realm != realmCN {
		return false
	}
	var apiErr *APIError
	if !errors.As(primaryErr, &apiErr) {
		return false
	}
	if apiErr.Kind != KindRateLimited && apiErr.Kind != KindUnavailable {
		return false
	}
	if !p.offersModel(model) || !peer.offersModel(model) {
		return false
	}
	peerPool := peer.Pool()
	if peerPool == nil || peerPool.CountReady(model) == 0 {
		return false
	}
	return true
}

// offersModel performs a non-network catalogue check: the live/last-good
// snapshot first, then the bundled realm table. Fallback must not trigger a
// catalogue discovery round-trip from inside a failed chat request.
func (p *Provider) offersModel(model string) bool {
	model = strings.TrimSpace(model)
	if p == nil || model == "" {
		return false
	}
	if _, ok := staticMetaForRealm(p.realm, model); ok {
		return true
	}
	entries := p.snapshotCatalog().entries
	if len(entries) == 0 {
		entries = mergeCatalog(nil, p.realm, false)
	}
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimSpace(entry.ID), model) {
			return true
		}
	}
	return false
}

func (p *Provider) sendChatWithAuthRetry(
	ctx context.Context,
	account *Account,
	baseBody map[string]any,
	model string,
	req providers.ChatRequest,
	cfg Config,
	turnRequestID, vscodeConversationID string,
	refreshed map[string]bool,
) (*chatStream, error) {
	attemptedRefresh := false
	for {
		stream, err := p.sendChatOnce(ctx, account, baseBody, model, req, cfg, turnRequestID, vscodeConversationID)
		if err == nil {
			return stream, nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Kind != KindAuth || apiErr.Status != http.StatusUnauthorized {
			return nil, err
		}

		uid := strings.TrimSpace(account.UID())
		if uid == "" {
			uid = account.UID()
		}
		if attemptedRefresh || refreshed[uid] {
			apiErr.Terminal = true
			return nil, apiErr
		}
		attemptedRefresh = true
		refreshed[uid] = true

		snapshot := tokenAccountSnapshotOf(account)
		if tokenAuthIsAPIKey(snapshot.auth) || strings.TrimSpace(snapshot.auth.RefreshToken) == "" {
			apiErr.Terminal = true
			return nil, apiErr
		}
		if refreshErr := p.ForceRefresh(ctx, account); refreshErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, newAPIError(KindAuth, http.StatusBadGateway,
				"WorkBuddy account credential refresh failed; sign in again")
		}
		// Retry the same account and the same acquired slot once.
	}
}

func (p *Provider) sendChatOnce(
	ctx context.Context,
	account *Account,
	baseBody map[string]any,
	model string,
	req providers.ChatRequest,
	cfg Config,
	turnRequestID, vscodeConversationID string,
) (*chatStream, error) {
	account.mu.RLock()
	auth := cloneAuth(account.Auth)
	proxyURL := strings.TrimSpace(account.Auth.ProxyURL)
	account.mu.RUnlock()

	product := normalizeProduct(auth.Product)
	body := baseBody
	if promptCacheKeyEnabled() {
		body = injectChatPromptCacheKey(baseBody, auth.UID, req.SessionKey)
	}
	body, err := chatBodyForProduct(body, product)
	if err != nil {
		return nil, err
	}
	data, err := marshalCompactJSON(body)
	if err != nil {
		return nil, newAPIError(KindInvalid, http.StatusBadRequest, "chat request could not be encoded")
	}
	compressed := product == productCLI || product == productVSCode
	if compressed {
		data, err = gzipBody(data)
		if err != nil {
			return nil, newAPIError(KindInvalid, http.StatusBadRequest, "chat request could not be compressed")
		}
	}

	conversationID := ""
	if product == productVSCode {
		conversationID = vscodeConversationID
	} else if strings.TrimSpace(req.SessionKey) != "" {
		conversationID = conversationIDFor(auth.UID, req.SessionKey)
	}
	deviceToken, deviceTokenError := p.chatDeviceToken(auth, product)
	headers := p.chatHeaders(auth, product, model, conversationID, turnRequestID, deviceToken, deviceTokenError, cfg)
	if compressed {
		headers.Set("Content-Encoding", "gzip")
	}

	doer := p.chatDoer
	if doer == nil {
		doer = p.transport
	}
	if doer == nil {
		return nil, newAPIError(KindUnavailable, http.StatusServiceUnavailable, "WorkBuddy chat transport is unavailable")
	}

	requestCtx, cancel := context.WithCancel(ctx)
	var headerTimedOut atomic.Bool
	headerTimeout := chatHeaderTimeout(cfg)
	timer := time.AfterFunc(headerTimeout, func() {
		headerTimedOut.Store(true)
		cancel()
	})

	request, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		strings.TrimRight(p.chatURLFor(product), "/")+chatPath,
		bytes.NewReader(data),
	)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, newAPIError(KindInvalid, http.StatusBadRequest, "chat request could not be created")
	}
	request.Header = headers

	response, err := doer.Do(requestCtx, request, proxyURL)
	timer.Stop()
	if err != nil {
		cancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if headerTimedOut.Load() {
			return nil, newAPIError(KindNetwork, http.StatusServiceUnavailable,
				"WorkBuddy upstream response header timeout")
		}
		return nil, newAPIError(KindNetwork, http.StatusServiceUnavailable,
			"WorkBuddy upstream network request failed")
	}
	if response == nil {
		cancel()
		return nil, newAPIError(KindUpstream, http.StatusBadGateway, "WorkBuddy upstream returned no response")
	}

	if headerTimedOut.Load() {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		cancel()
		return nil, newAPIError(KindNetwork, http.StatusServiceUnavailable,
			"WorkBuddy upstream response header timeout")
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if response.Body == nil {
			cancel()
			return nil, newAPIError(KindUpstream, http.StatusBadGateway, "upstream returned no response body")
		}
		stream, streamErr := newChatStream(
			requestCtx,
			cancel,
			response.Body,
			response.Header.Get("Content-Encoding"),
			p.Pool(),
			account,
			p.realm,
			model,
			chatIdleTimeout(cfg),
		)
		if streamErr != nil {
			cancel()
			return nil, streamErr
		}
		stream.usageMu.Lock()
		stream.usage.Free = p.IsFreeModel(model)
		stream.usageMu.Unlock()
		return stream, nil
	}

	bodyText := ""
	if response.Body != nil {
		bodyText = readChatErrorBody(response.Body, response.Header.Get("Content-Encoding"))
		_ = response.Body.Close()
	}
	cancel()
	safeMessage := p.safeChatError(account, bodyText)
	status := response.StatusCode

	switch status {
	case http.StatusBadRequest:
		return nil, newAPIError(KindInvalid, status, safeMessage)
	case http.StatusUnauthorized:
		return nil, newAPIError(KindAuth, status, safeMessage)
	case http.StatusForbidden:
		if chatRestrictionIsAccountLevel(bodyText) {
			return nil, newAPIError(KindRestricted, status, safeMessage)
		}
		return nil, newAPIError(KindContentRejected, status, safeMessage)
	case http.StatusTooManyRequests:
		return nil, p.chatRateLimitError(bodyText, response.Header.Get("Retry-After"), safeMessage)
	default:
		if status >= 500 {
			return nil, newAPIError(KindUpstream, status, safeMessage)
		}
		return nil, newAPIError(KindUpstream, http.StatusBadGateway, safeMessage)
	}
}

func (p *Provider) chatRateLimitError(bodyText, retryAfterHeader, safeMessage string) *APIError {
	now := time.Now()
	resetAt := parseChatResetAt(bodyText, now)
	accountScoped := resetAt.IsZero() && chatAccountScopePhrase(bodyText) != ""
	wait := modelRateCooldown
	if !resetAt.IsZero() {
		wait = resetAt.Sub(now)
	} else if headerWait := parseChatRetryAfter(retryAfterHeader, now); headerWait > 0 {
		wait = headerWait
	} else if accountScoped {
		wait = accountRateCooldown
	}
	if wait < time.Second {
		wait = time.Second
	}
	apiErr := newAPIError(KindRateLimited, http.StatusTooManyRequests, safeMessage)
	apiErr.ModelScoped = !accountScoped
	apiErr.RetryAfter = wait
	apiErr.ResetAt = resetAt
	if apiErr.ResetAt.IsZero() {
		apiErr.ResetAt = now.Add(wait)
	}
	return apiErr
}

func (p *Provider) chatURLFor(product string) string {
	if p != nil && strings.TrimSpace(p.chatBaseURL) != "" {
		return strings.TrimSpace(p.chatBaseURL)
	}
	if p == nil {
		return ""
	}
	return p.EndpointFor(product)
}

func (p *Provider) chatHeaders(
	auth Auth,
	product, model, conversationID, turnRequestID, deviceToken, deviceTokenError string,
	cfg Config,
) http.Header {
	origin := oauthOriginForRealm(p.realm)
	language := "en-US"
	if p.realm == realmCN {
		language = "zh-CN"
	}

	headers := http.Header{}
	if product == productCLI || (product == productDesktop && desktopChatCompatEnabled()) {
		headers.Set("Content-Type", "application/json")
		headers.Set("Accept", "application/json")
	} else {
		headers.Set("Content-Type", "application/json")
		headers.Set("Accept", "application/json, text/plain, */*")
		headers.Set("X-Requested-With", "XMLHttpRequest")
		headers.Set("Origin", origin)
		headers.Set("Referer", origin+"/")
		headers.Set("X-CodeBuddy-Request", "1")
		headers.Set("Accept-Language", language)
	}

	identity := buildIdentityHeaders(IdentityOptions{
		Realm:            p.realm,
		Product:          product,
		UID:              auth.UID,
		Token:            auth.AccessToken,
		Versions:         cfg.IdentityVersions,
		PrivateData:      cfg.PrivateData,
		ConversationID:   conversationID,
		SessionKey:       "",
		TurnRequestID:    turnRequestID,
		Model:            model,
		EnterpriseID:     auth.EnterpriseID,
		TenantID:         auth.EnterpriseID,
		DeviceToken:      deviceToken,
		DeviceTokenError: deviceTokenError,
	})
	mergeHeaders(headers, identity)
	// v1's urllib sends identity; keep Go's transport from silently negotiating
	// gzip. Response decompression is still supported when an upstream ignores
	// this and returns Content-Encoding: gzip.
	headers.Set("Accept-Encoding", "identity")

	if auth.APIKey || IsAPIKeyToken(auth.AccessToken) {
		headers.Set("X-API-Key", auth.AccessToken)
	}
	return headers
}

func (p *Provider) chatDeviceToken(auth Auth, product string) (string, string) {
	if p == nil || product == productCLI {
		return "", ""
	}
	token := ""
	if p.realm == realmCN {
		token, _ = ResolveDeviceToken(p.accountsDir)
	}
	if product == productVSCode && strings.TrimSpace(auth.EnterpriseID) == "" &&
		strings.TrimSpace(token) == "" {
		return "", vscodeDeviceTokenError
	}
	return strings.TrimSpace(token), ""
}

func chatHeaderTimeout(cfg Config) time.Duration {
	seconds := cfg.TimeoutSeconds
	if seconds <= 0 {
		return chatDefaultHeaderTimeout
	}
	timeout := time.Duration(seconds * float64(time.Second))
	if timeout <= 0 {
		return chatDefaultHeaderTimeout
	}
	return timeout
}

func chatIdleTimeout(cfg Config) time.Duration {
	seconds := cfg.StreamTimeoutSeconds
	if seconds <= 0 {
		return chatDefaultIdleTimeout
	}
	timeout := time.Duration(seconds * float64(time.Second))
	if timeout <= 0 {
		return chatDefaultIdleTimeout
	}
	return timeout
}

func readChatErrorBody(body io.Reader, contentEncoding string) string {
	if body == nil {
		return ""
	}
	var reader io.Reader = body
	if strings.Contains(strings.ToLower(strings.TrimSpace(contentEncoding)), "gzip") {
		if gzipReader, err := gzip.NewReader(body); err == nil {
			defer gzipReader.Close()
			reader = gzipReader
		}
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxChatErrorBody))
	if err != nil && len(raw) == 0 {
		return ""
	}
	return string(raw)
}

func (p *Provider) safeChatError(account *Account, bodyText string) string {
	message := extractChatErrorMessage(bodyText)
	if message == "" {
		message = "upstream request failed"
	}
	if account != nil {
		account.mu.RLock()
		secretsToRedact := accountSecretValuesLocked(account)
		account.mu.RUnlock()
		if deviceToken, _ := ResolveDeviceToken(p.accountsDir); deviceToken != "" {
			secretsToRedact = append(secretsToRedact, deviceToken)
		}
		message = redactAccountText(message, secretsToRedact)
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream request failed"
	}
	return truncateText(message, 300)
}

func extractChatErrorMessage(bodyText string) string {
	text := strings.TrimSpace(bodyText)
	if text == "" {
		return ""
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &root); err == nil && root != nil {
		if raw, ok := root["error"]; ok {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(raw, &nested); err == nil && nested != nil {
				if message := decodeChatString(nested["message"]); message != "" {
					return message
				}
			}
			if message := decodeChatString(raw); message != "" {
				return message
			}
		}
		for _, key := range []string{"msg", "message", "displayMsg", "detail"} {
			if message := decodeChatString(root[key]); message != "" {
				return message
			}
		}
		if nestedRaw, ok := root["data"]; ok {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(nestedRaw, &nested); err == nil && nested != nil {
				for _, key := range []string{"msg", "message", "displayMsg", "detail"} {
					if message := decodeChatString(nested[key]); message != "" {
						return message
					}
				}
			}
		}
	}
	if len([]rune(text)) <= 300 && !strings.HasPrefix(text, "<") {
		return text
	}
	return ""
}

func decodeChatString(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func chatRestrictionIsAccountLevel(bodyText string) bool {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(bodyText)), &root); err == nil && root != nil {
		candidates := []map[string]json.RawMessage{root}
		if raw, ok := root["error"]; ok {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(raw, &nested); err == nil && nested != nil {
				candidates = append(candidates, nested)
			}
		}
		for _, candidate := range candidates {
			for _, key := range []string{"code", "errorCode"} {
				if raw, ok := candidate[key]; ok {
					var number json.Number
					if err := json.Unmarshal(raw, &number); err == nil {
						if parsed, err := number.Int64(); err == nil && parsed == 11140 {
							return true
						}
					}
					if text := decodeChatString(raw); text == "11140" {
						return true
					}
				}
			}
		}
	}
	lower := strings.ToLower(bodyText)
	for _, phrase := range restrictionPhrases {
		if phrase == "" {
			continue
		}
		if phrase[0] < 0x80 {
			if strings.Contains(lower, phrase) {
				return true
			}
		} else if strings.Contains(bodyText, phrase) {
			return true
		}
	}
	return false
}

func chatAccountScopePhrase(bodyText string) string {
	lower := strings.ToLower(bodyText)
	for _, phrase := range accountScopePhrases {
		if phrase == "" {
			continue
		}
		if phrase[0] < 0x80 {
			if chatContainsWord(lower, phrase) {
				return phrase
			}
		} else if strings.Contains(bodyText, phrase) {
			return phrase
		}
	}
	return ""
}

func chatContainsWord(haystack, needle string) bool {
	for offset := 0; offset+len(needle) <= len(haystack); {
		index := strings.Index(haystack[offset:], needle)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(needle)
		leftOK := start == 0 || !chatWordByte(haystack[start-1])
		rightOK := end == len(haystack) || !chatWordByte(haystack[end])
		if leftOK && rightOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func chatWordByte(value byte) bool {
	return (value >= 'a' && value <= 'z') ||
		(value >= 'A' && value <= 'Z') ||
		(value >= '0' && value <= '9') || value == '_'
}

func parseChatRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil {
		wait := when.Sub(now)
		if wait > 0 {
			return wait
		}
	}
	return 0
}

func parseChatResetAt(bodyText string, now time.Time) time.Time {
	text := strings.TrimSpace(bodyText)
	if text == "" {
		return time.Time{}
	}
	if reset := chatResetAtFromJSON(text); !reset.IsZero() {
		return chatClampResetAt(reset, now)
	}
	match := chatResetAtPatternEnglish.FindStringSubmatch(text)
	if match == nil {
		match = chatResetAtPatternBare.FindStringSubmatch(text)
	}
	if match == nil {
		return time.Time{}
	}
	stamp := strings.ReplaceAll(match[1], "T", " ")
	base, err := time.Parse("2006-01-02 15:04:05", stamp)
	if err != nil {
		return time.Time{}
	}
	offsetSeconds := 0
	if tz := chatResetOffsetPattern.FindStringSubmatch(text); tz != nil {
		hours, _ := strconv.Atoi(tz[1])
		minutes := 0
		if tz[2] != "" {
			minutes, _ = strconv.Atoi(tz[2])
		}
		offsetSeconds = hours * 3600
		if hours >= 0 {
			offsetSeconds += minutes * 60
		} else {
			offsetSeconds -= minutes * 60
		}
	}
	return chatClampResetAt(base.Add(-time.Duration(offsetSeconds)*time.Second), now)
}

func chatResetAtFromJSON(text string) time.Time {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &root); err != nil || root == nil {
		return time.Time{}
	}
	for _, key := range []string{"reset_at", "resetAt"} {
		raw, ok := root[key]
		if !ok {
			continue
		}
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			continue
		}
		seconds, err := number.Float64()
		if err != nil || seconds <= 0 {
			continue
		}
		if seconds > 1e11 {
			seconds /= 1000
		}
		return time.Unix(int64(seconds), 0)
	}
	return time.Time{}
}

func chatClampResetAt(value, now time.Time) time.Time {
	max := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	if value.Before(now) || value.After(max) {
		return time.Time{}
	}
	return value
}

// ---------------------------------------------------------------------------
// Request body: raw copy, normalization, product wire shape
// ---------------------------------------------------------------------------

func (p *Provider) buildChatBody(req providers.ChatRequest) (map[string]any, string, error) {
	body := decodeChatRawObject(req.Raw)
	if body == nil {
		body = map[string]any{}
	}
	if _, exists := body["messages"]; !exists && len(req.Messages) > 0 {
		var messages []any
		for _, raw := range req.Messages {
			value, ok := decodeChatRawValue(raw)
			if ok {
				messages = append(messages, value)
			}
		}
		body["messages"] = messages
	}

	for key := range body {
		if strings.HasPrefix(key, "_") {
			delete(body, key)
		}
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		if text, ok := body["model"].(string); ok {
			model = strings.TrimSpace(text)
		}
	}
	model = p.canonicalModelID(model)
	if model == "" {
		return nil, "", newAPIError(KindInvalid, http.StatusBadRequest, "model is required")
	}
	body["model"] = model

	messages, _ := body["messages"].([]any)
	messages = normalizeChatRoles(messages)
	messages = backfillChatReasoning(messages, model, chatThinkingEnabled(body, model))
	if len(messages) == 0 {
		messages = []any{map[string]any{"role": "system", "content": defaultChatSystemPrompt}}
	} else {
		first, _ := messages[0].(map[string]any)
		if first == nil || first["role"] != "system" {
			messages = append([]any{map[string]any{"role": "system", "content": defaultChatSystemPrompt}}, messages...)
		}
	}
	body["messages"] = messages

	if err := p.normalizeChatBody(body, model); err != nil {
		return nil, "", err
	}
	return body, model, nil
}

func decodeChatRawObject(raw map[string]json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		decoded, ok := decodeChatRawValue(value)
		if !ok {
			continue
		}
		out[key] = decoded
	}
	return out
}

func decodeChatRawValue(raw json.RawMessage) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

func (p *Provider) canonicalModelID(model string) string {
	model = strings.TrimSpace(model)
	if p == nil || model == "" {
		return model
	}
	entries := p.snapshotCatalog().entries
	if len(entries) == 0 {
		entries = mergeCatalog(nil, p.realm, false)
	}
	for _, entry := range entries {
		if entry.ID == model {
			return entry.ID
		}
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.ID, model) {
			return entry.ID
		}
	}
	return model
}

func normalizeChatRoles(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		if role, _ := message["role"].(string); role == "developer" {
			copied := cloneMap(message)
			copied["role"] = "system"
			out = append(out, copied)
			continue
		}
		out = append(out, message)
	}
	return out
}

func chatThinkingEnabled(body map[string]any, model string) bool {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek") {
		return false
	}
	if thinking, ok := body["thinking"].(map[string]any); ok {
		if strings.EqualFold(strings.TrimSpace(fmt.Sprint(thinking["type"])), "disabled") {
			return false
		}
	}
	effort := chatReasoningEffort(body)
	return !chatIsThinkingOff(effort)
}

func chatReasoningEffort(body map[string]any) string {
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		if value, ok := body[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func backfillChatReasoning(messages []any, model string, thinkingEnabled bool) []any {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek") {
		return messages
	}
	hasTrace := false
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := message["reasoning"].(string); ok && text != "" {
			hasTrace = true
			break
		}
		if _, ok := message["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !thinkingEnabled && !hasTrace {
		return messages
	}

	out := make([]any, 0, len(messages))
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok || message["role"] != "assistant" {
			out = append(out, item)
			continue
		}
		copied := cloneMap(message)
		reasoning, ok := copied["reasoning_content"].(string)
		if !ok {
			legacy, _ := copied["reasoning"].(string)
			reasoning = legacy
			copied["reasoning_content"] = reasoning
		}
		existing, _ := copied["reasoning"].(string)
		if existing == "" {
			if reasoning != "" {
				copied["reasoning"] = reasoning
			} else {
				copied["reasoning"] = " "
			}
		}
		out = append(out, copied)
	}
	return out
}

func (p *Provider) normalizeChatBody(body map[string]any, model string) error {
	if body == nil {
		return newAPIError(KindInvalid, http.StatusBadRequest, "invalid chat body")
	}
	reasoning := p.reasoningMetaForModel(model)
	mapChatReasoningEffort(body, reasoning)
	translateChatMaxTokens(body)
	clampGPTMaxTokens(body, model)
	normalizeChatToolChoice(body)
	normalizeChatTools(body)
	normalizeChatStop(body)
	if _, exists := body["max_tokens"]; !exists {
		if limit := p.defaultMaxOutputTokens(model); limit > 0 {
			body["max_tokens"] = limit
		}
	}
	applyDeepSeekThinking(body, model, reasoning)
	ensureExplicitChatEffort(body, model, reasoning)

	body["stream"] = true
	if options, ok := body["stream_options"].(map[string]any); ok && options != nil {
		options["include_usage"] = true
		body["stream_options"] = options
	} else {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return nil
}

func promptCacheKeyEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB_PROMPT_CACHE_KEY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// injectChatPromptCacheKey mirrors v1's opt-in prompt_cache_key. The key is
// account-scoped: a shared key could let one account read another's cached
// prefix. An explicit client key is never overwritten.
func injectChatPromptCacheKey(base map[string]any, uid, sessionKey string) map[string]any {
	if base == nil {
		return base
	}
	if existing, ok := base["prompt_cache_key"].(string); ok && strings.TrimSpace(existing) != "" {
		return base
	}
	conversation := strings.TrimSpace(sessionKey)
	for _, field := range []string{"conversation_id", "conversationId"} {
		if value, ok := base[field].(string); ok && strings.TrimSpace(value) != "" {
			conversation = strings.TrimSpace(value)
			break
		}
	}
	uid = strings.TrimSpace(uid)
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		uid8 = "-"
	}
	digest := sha256.Sum256([]byte(uid + "|" + conversation))
	out := make(map[string]any, len(base)+1)
	for key, value := range base {
		out[key] = value
	}
	out["prompt_cache_key"] = "wb2a-" + uid8 + "-" + hex.EncodeToString(digest[:16])
	return out
}

func chatBodyForProduct(base map[string]any, product string) (map[string]any, error) {
	if base == nil {
		return nil, newAPIError(KindInvalid, http.StatusBadRequest, "invalid chat body")
	}
	body := make(map[string]any, len(base))
	for key, value := range base {
		body[key] = value
	}
	if normalizeProduct(product) == productCLI {
		for _, key := range cliBodyDropFields {
			delete(body, key)
		}
		body["cache_retention"] = cliCacheRetention
	}
	return body, nil
}

func marshalCompactJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func translateChatMaxTokens(body map[string]any) {
	alias, exists := body["max_completion_tokens"]
	delete(body, "max_completion_tokens")
	if !exists {
		return
	}
	if _, exists := body["max_tokens"]; exists {
		return
	}
	number, ok := chatPositiveInt(alias)
	if ok {
		body["max_tokens"] = number
	}
}

func clampGPTMaxTokens(body map[string]any, model string) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-") {
		return
	}
	value, exists := body["max_tokens"]
	if !exists {
		return
	}
	number, ok := chatPositiveInt(value)
	if ok && number < 16 {
		body["max_tokens"] = int64(16)
	}
}

func chatPositiveInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			floatValue, floatErr := typed.Float64()
			if floatErr != nil {
				return 0, false
			}
			number = int64(floatValue)
		}
		return number, number > 0
	case float64:
		return int64(typed), typed > 0
	case float32:
		return int64(typed), typed > 0
	case int:
		return int64(typed), typed > 0
	case int64:
		return typed, typed > 0
	case int32:
		return int64(typed), typed > 0
	case string:
		number, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return number, err == nil && number > 0
	default:
		return 0, false
	}
}

func normalizeChatStop(body map[string]any) {
	value, exists := body["stop"]
	if !exists {
		return
	}
	var stops []string
	switch typed := value.(type) {
	case string:
		if typed != "" {
			stops = []string{typed}
		}
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				stops = append(stops, text)
			}
		}
	case []string:
		for _, text := range typed {
			if text != "" {
				stops = append(stops, text)
			}
		}
	}
	if len(stops) == 0 {
		delete(body, "stop")
		return
	}
	body["stop"] = stops
}

func normalizeChatToolChoice(body map[string]any) {
	value, exists := body["tool_choice"]
	if !exists {
		return
	}
	switch typed := value.(type) {
	case string:
		choice := strings.ToLower(strings.TrimSpace(typed))
		switch choice {
		case "auto", "none", "required":
			body["tool_choice"] = choice
		default:
			delete(body, "tool_choice")
		}
	case map[string]any:
		kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(typed["type"])))
		switch kind {
		case "none", "auto", "required":
			body["tool_choice"] = kind
		case "function":
			name := ""
			if function, ok := typed["function"].(map[string]any); ok {
				name, _ = function["name"].(string)
			}
			if strings.TrimSpace(name) == "" {
				name, _ = typed["name"].(string)
			}
			if strings.TrimSpace(name) == "" {
				body["tool_choice"] = "auto"
			} else {
				body["tool_choice"] = strings.TrimSpace(name)
			}
		default:
			delete(body, "tool_choice")
		}
	default:
		delete(body, "tool_choice")
	}
}

func normalizeChatTools(body map[string]any) {
	if functions, ok := body["functions"]; ok {
		if _, hasTools := body["tools"]; !hasTools {
			if list, ok := functions.([]any); ok {
				tools := make([]any, 0, len(list))
				for _, item := range list {
					function, ok := item.(map[string]any)
					if !ok {
						continue
					}
					tools = append(tools, map[string]any{
						"type":     "function",
						"function": cloneMap(function),
					})
				}
				body["tools"] = tools
			}
		}
		delete(body, "functions")
	}

	value, exists := body["tools"]
	if !exists {
		return
	}
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		delete(body, "tools")
		return
	}

	normalized := make([]any, 0, len(list))
	seen := make(map[string]bool)
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var function map[string]any
		if existing, ok := tool["function"].(map[string]any); ok {
			function = cloneMap(existing)
		} else if _, hasName := tool["name"]; hasName {
			description, _ := tool["description"].(string)
			function = map[string]any{
				"name":        tool["name"],
				"description": description,
				"parameters":  tool["parameters"],
			}
			if strict, ok := tool["strict"]; ok {
				function["strict"] = strict
			}
		}
		if function == nil {
			continue
		}
		name, _ := function["name"].(string)
		name = strings.TrimSpace(name)
		if !chatToolNamePattern.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		normalizeChatToolParameters(function)
		normalized = append(normalized, map[string]any{
			"type":     "function",
			"function": function,
		})
	}
	if len(normalized) == 0 {
		delete(body, "tools")
		return
	}
	body["tools"] = normalized
}

func normalizeChatToolParameters(function map[string]any) {
	parameters, ok := function["parameters"].(map[string]any)
	if !ok || parameters == nil {
		function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		return
	}
	copied := cloneMap(parameters)
	if kind, _ := copied["type"].(string); kind != "object" {
		copied["type"] = "object"
	}
	if properties, ok := copied["properties"].(map[string]any); !ok || properties == nil {
		copied["properties"] = map[string]any{}
	}
	normalizeChatSchema(copied)
	function["parameters"] = copied
}

var chatSchemaTypeAliases = map[string]string{
	"str": "string", "text": "string", "int": "integer", "bool": "boolean",
	"dict": "object", "list": "array", "float": "number",
}

var chatSchemaValidTypes = map[string]bool{
	"string": true, "number": true, "integer": true, "boolean": true,
	"array": true, "object": true, "null": true,
}

func normalizeChatSchema(schema map[string]any) {
	if schema == nil {
		return
	}
	for _, key := range []string{"description", "title", "format", "pattern", "items", "default"} {
		if value, exists := schema[key]; exists && value == nil {
			delete(schema, key)
		}
	}
	if value, exists := schema["type"]; exists {
		switch typed := value.(type) {
		case string:
			lower := strings.ToLower(strings.TrimSpace(typed))
			if mapped, ok := chatSchemaTypeAliases[lower]; ok {
				schema["type"] = mapped
			} else if chatSchemaValidTypes[lower] {
				schema["type"] = lower
			} else if _, hasProperties := schema["properties"]; hasProperties {
				schema["type"] = "object"
			} else if _, hasItems := schema["items"]; hasItems {
				schema["type"] = "array"
			} else {
				delete(schema, "type")
			}
		case []any:
			types := make([]any, 0, len(typed))
			for _, item := range typed {
				text := strings.ToLower(strings.TrimSpace(fmt.Sprint(item)))
				if mapped, ok := chatSchemaTypeAliases[text]; ok {
					text = mapped
				}
				if text != "" {
					types = append(types, text)
				}
			}
			if len(types) > 0 {
				schema["type"] = types
			} else {
				delete(schema, "type")
			}
		}
	}
	if schema["type"] == "object" || schema["properties"] != nil {
		if schema["type"] == nil {
			schema["type"] = "object"
		}
		if properties, ok := schema["properties"].(map[string]any); !ok || properties == nil {
			schema["properties"] = map[string]any{}
		}
	}
	if required, exists := schema["required"]; exists {
		switch typed := required.(type) {
		case []any:
			cleaned := make([]any, 0, len(typed))
			for _, item := range typed {
				if item == nil {
					continue
				}
				text := strings.TrimSpace(fmt.Sprint(item))
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
			if strings.TrimSpace(typed) != "" {
				schema["required"] = []any{strings.TrimSpace(typed)}
			} else {
				delete(schema, "required")
			}
		default:
			delete(schema, "required")
		}
	}
	if items, exists := schema["items"]; exists {
		switch typed := items.(type) {
		case []any:
			for _, item := range typed {
				if nested, ok := item.(map[string]any); ok {
					schema["items"] = cloneMap(nested)
					break
				}
			}
			if _, ok := schema["items"].([]any); ok {
				schema["items"] = map[string]any{"type": "string"}
			}
		case map[string]any:
			normalizeChatSchema(typed)
		case nil:
			delete(schema, "items")
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for _, value := range properties {
			if nested, ok := value.(map[string]any); ok {
				normalizeChatSchema(nested)
			}
		}
	}
	for _, key := range []string{"$defs", "definitions"} {
		if definitions, ok := schema[key].(map[string]any); ok {
			for _, value := range definitions {
				if nested, ok := value.(map[string]any); ok {
					normalizeChatSchema(nested)
				}
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if list, ok := schema[key].([]any); ok {
			for _, value := range list {
				if nested, ok := value.(map[string]any); ok {
					normalizeChatSchema(nested)
				}
			}
		}
	}
}

func chatIsThinkingOff(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "none", "disabled", "disable":
		return true
	default:
		return false
	}
}

func (p *Provider) modelMetaFor(model string) map[string]any {
	if p == nil || strings.TrimSpace(model) == "" {
		return nil
	}
	entries := p.snapshotCatalog().entries
	if len(entries) == 0 {
		entries = mergeCatalog(nil, p.realm, false)
	}
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimSpace(entry.ID), strings.TrimSpace(model)) {
			return entry.Meta
		}
	}
	if meta, ok := staticMetaForRealm(p.realm, model); ok {
		return meta
	}
	return nil
}

func (p *Provider) defaultMaxOutputTokens(model string) int64 {
	meta := p.modelMetaFor(model)
	if meta == nil {
		return 0
	}
	number, ok := chatPositiveInt(meta["maxOutputTokens"])
	if !ok {
		return 0
	}
	return number
}

func (p *Provider) reasoningMetaForModel(model string) map[string]any {
	if p == nil || strings.TrimSpace(model) == "" {
		return nil
	}
	entries := p.snapshotCatalog().entries
	if len(entries) == 0 {
		entries = mergeCatalog(nil, p.realm, false)
	}
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimSpace(entry.ID), strings.TrimSpace(model)) {
			if reasoning, ok := entry.Meta["reasoning"].(map[string]any); ok {
				return reasoning
			}
			return nil
		}
	}
	if meta, ok := staticMetaForRealm(p.realm, model); ok {
		if reasoning, ok := meta["reasoning"].(map[string]any); ok {
			return reasoning
		}
	}
	return nil
}

func mapChatReasoningEffort(body map[string]any, reasoning map[string]any) {
	key := ""
	for _, candidate := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, exists := body[candidate]; exists {
			key = candidate
			break
		}
	}
	if key == "" {
		return
	}
	value, ok := body[key].(string)
	if !ok {
		return
	}
	requested := strings.TrimSpace(value)
	if requested == "" {
		return
	}
	if reasoning == nil {
		return
	}
	efforts := chatStringList(reasoning["supportedEfforts"])
	fixed, _ := reasoning["effort"].(string)
	fixed = strings.TrimSpace(fixed)
	replacement := ""
	if chatIsThinkingOff(requested) {
		if canDisable, exists := reasoning["canDisableThinking"].(bool); !exists || canDisable {
			return
		}
		replacement = chatFallbackEffort(reasoning)
	} else if len(efforts) > 0 {
		for _, effort := range efforts {
			if strings.EqualFold(effort, requested) {
				return
			}
		}
		replacement = chatFallbackEffort(reasoning)
	} else if fixed != "" {
		if strings.EqualFold(fixed, requested) {
			return
		}
		replacement = chatFallbackEffort(reasoning)
	} else {
		return
	}
	if replacement == "" || strings.EqualFold(replacement, requested) {
		return
	}
	body[key] = replacement
}

func chatFallbackEffort(reasoning map[string]any) string {
	if value, ok := reasoning["defaultEffort"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	efforts := chatStringList(reasoning["supportedEfforts"])
	if len(efforts) > 0 {
		return efforts[0]
	}
	if value, ok := reasoning["effort"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return ""
}

func chatStringList(value any) []string {
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				out = append(out, text)
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(item); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func applyDeepSeekThinking(body map[string]any, model string, reasoning map[string]any) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek") {
		return
	}
	thinking, _ := body["thinking"].(map[string]any)
	optedOut := false
	if thinking != nil {
		optedOut = strings.EqualFold(strings.TrimSpace(fmt.Sprint(thinking["type"])), "disabled")
	}
	effort := chatReasoningEffort(body)
	if chatIsThinkingOff(effort) {
		optedOut = true
	}
	if optedOut {
		return
	}
	if _, exists := body["thinking"]; !exists {
		body["thinking"] = map[string]any{"type": "enabled"}
	}
	if effort == "" {
		mode := chatDefaultReasoningEffortMode()
		if mode == "off" {
			return
		}
		chosen := chatDefaultEffort(reasoning)
		if chosen == "" && mode == "high" {
			chosen = "high"
		}
		if chosen != "" {
			body["reasoning_effort"] = chosen
		}
	}
}

func chatDefaultReasoningEffortMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB_DEFAULT_REASONING_EFFORT"))) {
	case "off", "none", "0", "false", "no":
		return "off"
	case "catalog", "model", "default":
		return "catalog"
	default:
		return "high"
	}
}

func chatDefaultEffort(reasoning map[string]any) string {
	if reasoning == nil {
		return ""
	}
	value, _ := reasoning["defaultEffort"].(string)
	return strings.TrimSpace(value)
}

func ensureExplicitChatEffort(body map[string]any, model string, reasoning map[string]any) {
	if !strings.EqualFold(strings.TrimSpace(model), "glm-5.1") {
		return
	}
	if thinking, ok := body["thinking"].(map[string]any); ok {
		if strings.EqualFold(strings.TrimSpace(fmt.Sprint(thinking["type"])), "disabled") {
			delete(body, "thinking")
		}
	}
	key := "reasoning_effort"
	if _, exists := body["reasoning_effort"]; !exists {
		if _, ok := body["reasoningEffort"]; ok {
			key = "reasoningEffort"
		}
	}
	if value, ok := body[key].(string); ok && strings.TrimSpace(value) != "" &&
		!chatIsThinkingOff(value) {
		return
	}
	chosen := chatFixedEffort(reasoning)
	if chosen == "" {
		chosen = chatDefaultEffort(reasoning)
	}
	if chosen == "" {
		chosen = "medium"
	}
	body[key] = chosen
}

func chatFixedEffort(reasoning map[string]any) string {
	if reasoning == nil {
		return ""
	}
	if len(chatStringList(reasoning["supportedEfforts"])) > 0 {
		return ""
	}
	value, _ := reasoning["effort"].(string)
	return strings.TrimSpace(value)
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
