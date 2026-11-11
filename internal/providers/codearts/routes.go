package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// adminMaxBodyBytes bounds panel request bodies. Auth documents are small;
	// the limit exists only to stop unbounded allocation.
	adminMaxBodyBytes = 8 << 20

	oauthSessionMissingMessage  = "会话不存在或已过期，请重新发起授权"
	oauthStateMessage           = "授权状态不匹配，请重新发起授权"
	oauthReplayMessage          = "授权会话已失效，请重新发起授权"
	oauthCallbackURLMessage     = "只接受华为返回的 127.0.0.1 OAuth 回调地址"
	oauthCallbackMissingMessage = "回调地址缺少 code 或 secret"
	oauthRedirectMessage        = "授权跳转地址不属于 CodeArts 门户"
	oauthGenericFailureMessage  = "登录失败：回调处理异常。"
)

// adminError is the provider-local wire error used by the admin surface.
type adminError struct {
	Status  int
	Type    string
	Message string
}

func (e *adminError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func badAdminRequest(message string) *adminError {
	return &adminError{
		Status: http.StatusBadRequest, Type: "invalid_request_error", Message: message,
	}
}

func serverAdminError(message string) *adminError {
	return &adminError{
		Status: http.StatusInternalServerError, Type: "server_error", Message: message,
	}
}

// adminErrorFrom maps pool, OAuth, and provider failures onto the v1 panel
// envelope. The admin surface only special-cases NotFound (404) and genuine
// bad requests (400); every other rate-limit, unavailable, credit, upstream, or
// local failure keeps v1's admin default of 502. The generic API's HTTPError
// status mapping remains separate.
func adminErrorFrom(err error) *adminError {
	if err == nil {
		return &adminError{
			Status: http.StatusBadGateway, Type: "invalid_request_error",
			Message: "codearts: request failed",
		}
	}
	var adminErr *adminError
	if errors.As(err, &adminErr) && adminErr != nil {
		return adminErr
	}
	switch {
	case errors.Is(err, ErrAccountNotFound):
		return &adminError{
			Status: http.StatusNotFound, Type: "invalid_request_error", Message: "account not found",
		}
	case errors.Is(err, ErrAuthPathCollision):
		return &adminError{
			Status: http.StatusBadGateway, Type: "invalid_request_error",
			Message: "codearts: auth file belongs to a different account",
		}
	case errors.Is(err, ErrOAuthSession):
		return &adminError{
			Status: http.StatusNotFound, Type: "invalid_request_error",
			Message: oauthSessionMissingMessage,
		}
	case errors.Is(err, ErrOAuthState):
		return badAdminRequest(oauthStateMessage)
	case errors.Is(err, ErrOAuthReplay):
		return &adminError{
			Status: http.StatusBadGateway, Type: "invalid_request_error",
			Message: oauthReplayMessage,
		}
	case errors.Is(err, ErrOAuthCallbackURL):
		return badAdminRequest(oauthCallbackURLMessage)
	case errors.Is(err, ErrOAuthCallback):
		return badAdminRequest(oauthCallbackMissingMessage)
	case errors.Is(err, ErrOAuthRedirect):
		return badAdminRequest(oauthRedirectMessage)
	}

	// The admin surface is stricter than the generic OpenAI API: only a
	// genuine bad request keeps its 400. v1 maps rate limits, unavailable
	// upstreams, credit exhaustion, and every other provider failure to 502.
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		status := http.StatusBadGateway
		if apiErr.Kind == KindBadRequest {
			status = http.StatusBadRequest
		}
		message := strings.TrimSpace(apiErr.SafeMessage())
		if message == "" {
			message = safeCodeArtsAdminError(err)
		}
		return &adminError{Status: status, Type: "invalid_request_error", Message: message}
	}
	var httpErr providers.HTTPError
	if errors.As(err, &httpErr) {
		status := http.StatusBadGateway
		if httpErr.HTTPStatus() == http.StatusBadRequest {
			status = http.StatusBadRequest
		}
		message := strings.TrimSpace(httpErr.SafeMessage())
		if message == "" {
			message = safeCodeArtsAdminError(err)
		}
		return &adminError{Status: status, Type: "invalid_request_error", Message: message}
	}
	return &adminError{
		Status: http.StatusBadGateway, Type: "invalid_request_error",
		Message: safeCodeArtsAdminError(err),
	}
}

// safeCodeArtsAdminError is the only formatter for panel messages. Provider
// APIError/TokenEndpointError values are already redacted; local errors are
// never echoed because they may contain absolute paths or permission details.
func safeCodeArtsAdminError(err error) string {
	if err == nil {
		return "codearts: request failed"
	}
	var adminErr *adminError
	if errors.As(err, &adminErr) && adminErr != nil {
		if message := strings.TrimSpace(adminErr.Message); message != "" {
			return message
		}
		return "codearts: request failed"
	}
	var httpErr providers.HTTPError
	if errors.As(err, &httpErr) {
		if message := strings.TrimSpace(httpErr.SafeMessage()); message != "" {
			return message
		}
		return "codearts: request failed"
	}
	var tokenErr *TokenEndpointError
	if errors.As(err, &tokenErr) && tokenErr != nil {
		return truncateDisplayText(tokenErr.Error(), 300)
	}
	log.Printf("codearts: admin internal error: %v", err)
	return "codearts: request failed"
}

func writeAdminJSON(w http.ResponseWriter, status int, value any) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		body := []byte(`{"error":{"message":"response encoding failed","type":"server_error","code":500}}`)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(body)
		return
	}
	body := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeAdminError(w http.ResponseWriter, failure *adminError) {
	if failure == nil {
		failure = serverAdminError("codearts: request failed")
	}
	status := failure.Status
	if status == 0 {
		status = http.StatusBadGateway
	}
	errType := failure.Type
	if errType == "" {
		errType = "invalid_request_error"
	}
	message := failure.Message
	if message == "" {
		message = "codearts: request failed"
	}
	writeAdminJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    status,
		},
	})
}

func readAdminPayload(w http.ResponseWriter, r *http.Request) (map[string]any, *adminError) {
	if r == nil || r.Body == nil {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]any{}, nil
		}
		return nil, badAdminRequest("invalid JSON body")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, badAdminRequest("invalid JSON body")
	}
	if value == nil {
		return nil, badAdminRequest("expected a JSON object")
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return nil, badAdminRequest("expected a JSON object")
	}
	return payload, nil
}

func adminText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func adminTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case json.Number:
		number, err := typed.Float64()
		return err == nil && number != 0
	case float64:
		return typed != 0
	case string:
		return typed != ""
	default:
		return true
	}
}

func forceFromQuery(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// requireAdminEnabled applies current settings and enforces v1's disabled-realm
// admin gate. Import and OAuth routes deliberately do not call this: they are
// the v2 bootstrap/re-enable path.
func (p *Provider) requireAdminEnabled() *adminError {
	cfg, err := p.applySettings()
	if err != nil {
		return adminErrorFrom(err)
	}
	if !cfg.Enabled {
		return &adminError{
			Status: http.StatusServiceUnavailable, Type: "invalid_request_error",
			Message: disabledAdminMessage,
		}
	}
	return nil
}

// RouteNotFoundMessage is consumed by the generic API route adapter so an
// unknown CodeArts path answers with provider semantics.
func (p *Provider) RouteNotFoundMessage() string {
	return "unknown codearts endpoint"
}

// Routes returns the complete v1 admin/OAuth route list. Panel authentication
// is declared through AuthPanel and enforced by the API middleware; the
// browser OAuth callback is public and validates its own state.
func (p *Provider) Routes() []providers.Route {
	panel := providers.AuthPanel
	return []providers.Route{
		{Method: http.MethodGet, Path: "/codearts/overview", Auth: panel, Handler: p.handleOverview},
		{Method: http.MethodGet, Path: "/codearts/accounts", Auth: panel, Handler: p.handleAccounts},
		{Method: http.MethodPost, Path: "/codearts/accounts/import", Auth: panel, Handler: p.handleImport},
		{Method: http.MethodPost, Path: "/codearts/accounts/oauth/start", Auth: panel, Handler: p.handleOAuthStart},
		{Method: http.MethodPost, Path: "/codearts/accounts/oauth/poll", Auth: panel, Handler: p.handleOAuthPoll},
		{Method: http.MethodPost, Path: "/codearts/accounts/oauth/callback", Auth: panel, Handler: p.handleOAuthCallback},
		{Method: http.MethodPost, Path: "/codearts/accounts/enable", Auth: panel, Handler: p.handleAccountAction("enable")},
		{Method: http.MethodPost, Path: "/codearts/accounts/disable", Auth: panel, Handler: p.handleAccountAction("disable")},
		{Method: http.MethodPost, Path: "/codearts/accounts/clear-cooldown", Auth: panel, Handler: p.handleAccountAction("clear-cooldown")},
		{Method: http.MethodPost, Path: "/codearts/accounts/note", Auth: panel, Handler: p.handleAccountAction("note")},
		{Method: http.MethodPost, Path: "/codearts/accounts/tasks", Auth: panel, Handler: p.handleAccountAction("tasks")},
		{Method: http.MethodPost, Path: "/codearts/accounts/test", Auth: panel, Handler: p.handleAccountAction("test")},
		{Method: http.MethodPost, Path: "/codearts/accounts/proxy", Auth: panel, Handler: p.handleAccountAction("proxy")},
		{Method: http.MethodPost, Path: "/codearts/accounts/delete", Auth: panel, Handler: p.handleAccountAction("delete")},
		{Method: http.MethodPost, Path: "/codearts/accounts/checkin", Auth: panel, Handler: p.handleCheckin},
		{Method: http.MethodPost, Path: "/codearts/checkin", Auth: panel, Handler: p.handleCheckin},
		{Method: http.MethodPost, Path: "/codearts/credits/refresh", Auth: panel, Handler: p.handleCreditsRefresh},
		{Method: http.MethodPost, Path: "/codearts/keepalive", Auth: panel, Handler: p.handleKeepalive},
		{Method: http.MethodPost, Path: "/codearts/models/refresh", Auth: panel, Handler: p.handleModelsRefresh},
		{Method: http.MethodPost, Path: "/codearts/test", Auth: panel, Handler: p.handleConnectionTest},
		{Method: http.MethodGet, Path: "/oauth/callback", Auth: providers.AuthNone, Handler: p.handleCallbackGET},
	}
}

func (p *Provider) handleOverview(w http.ResponseWriter, r *http.Request) {
	value, err := p.Overview(r.Context(), forceFromQuery(r))
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	writeAdminJSON(w, http.StatusOK, value)
}

func (p *Provider) handleAccounts(w http.ResponseWriter, r *http.Request) {
	value, err := p.Overview(r.Context(), forceFromQuery(r))
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	overview, _ := value.(map[string]any)
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"provider": ProviderID,
		"accounts": overview["accounts"],
		"stats":    overview["stats"],
		"models":   overview["models"],
		"base_url": overview["base_url"],
	})
}

func (p *Provider) handleImport(w http.ResponseWriter, r *http.Request) {
	payload, failure := readAdminPayload(w, r)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	authPayload := payload
	if nested, present := payload["auth"]; present {
		object, ok := nested.(map[string]any)
		if !ok {
			writeAdminError(w, badAdminRequest("expected a JSON object"))
			return
		}
		authPayload = object
	}
	auth, failure := authFromAdminPayload(authPayload, p.nowTime())
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	account, err := p.saveAccountAndEnable(auth)
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	view := account.View()
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     view.UID,
		"account": view,
		"enabled": true,
	})
}

// authFromAdminPayload accepts the flat v1 auth document or the same object
// nested under "auth". The pool still enforces canonical-path ownership, so a
// sanitized filename collision can never overwrite another account.
func authFromAdminPayload(payload map[string]any, now time.Time) (Auth, *adminError) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Auth{}, badAdminRequest("invalid auth payload")
	}
	var auth Auth
	if err := json.Unmarshal(raw, &auth); err != nil {
		return Auth{}, badAdminRequest("invalid auth payload")
	}
	auth.UserID = strings.TrimSpace(auth.UserID)
	auth.UserName = strings.TrimSpace(auth.UserName)
	auth.CloudDragonToken = strings.TrimSpace(auth.CloudDragonToken)
	if auth.UserID == "" {
		return Auth{}, badAdminRequest("auth payload requires user_id")
	}
	if auth.CloudDragonToken == "" {
		return Auth{}, badAdminRequest("auth payload requires cloud_dragon_token")
	}
	if auth.UpdatedAt == 0 {
		auth.UpdatedAt = now.Unix()
	}
	return auth, nil
}

// saveAccountAndEnable imports/replaces an account and enables the realm. The
// pool verifies the canonical auth file's embedded ownership before writing;
// settings are projected afterward so the new account sees current proxy slots
// and concurrency values.
func (p *Provider) saveAccountAndEnable(auth Auth) (*Account, error) {
	if _, err := p.applySettings(); err != nil {
		return nil, err
	}
	account, err := p.pool.Add(auth)
	if err != nil {
		return nil, err
	}
	if p.store != nil {
		if _, err := SaveConfig(p.store, map[string]any{"enabled": true}); err != nil {
			return nil, err
		}
	}
	if _, err := p.applySettings(); err != nil {
		return nil, err
	}
	p.invalidateHealth()
	return account, nil
}

func (p *Provider) handleAccountAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload, failure := readAdminPayload(w, r)
		if failure != nil {
			writeAdminError(w, failure)
			return
		}
		if failure := p.requireAdminEnabled(); failure != nil {
			writeAdminError(w, failure)
			return
		}
		uid := adminText(payload["uid"])
		if uid == "" {
			writeAdminError(w, badAdminRequest("uid required"))
			return
		}

		var result map[string]any
		var actionErr error
		switch action {
		case "enable":
			actionErr = p.pool.Enable(uid)
			result = map[string]any{"ok": true, "message": "已启用 " + uid}
		case "disable":
			reason := adminText(payload["reason"])
			if reason == "" {
				reason = "manual disable"
			}
			actionErr = p.pool.Disable(uid, reason)
			result = map[string]any{"ok": true, "message": "已停用 " + uid}
		case "clear-cooldown":
			actionErr = p.pool.ClearCooldown(uid)
			result = map[string]any{"ok": true, "message": "已清冷却 " + uid}
		case "note":
			actionErr = p.pool.SetNote(uid, adminText(payload["note"]))
			if actionErr == nil {
				if account := p.pool.Get(uid); account != nil {
					result = map[string]any{"ok": true, "uid": uid, "note": account.View().Note}
				}
			}
		case "tasks":
			enabled := true
			if value, present := payload["enabled"]; present {
				enabled = adminTruthy(value)
			}
			actionErr = p.pool.SetTasksEnabled(uid, enabled)
			if actionErr == nil {
				if account := p.pool.Get(uid); account != nil {
					result = map[string]any{
						"ok": true, "uid": uid, "tasks_enabled": account.View().TasksEnabled,
					}
				}
			}
		case "proxy":
			actionErr = p.pool.SetProxySlot(uid, adminText(payload["slot_id"]))
			if actionErr == nil {
				if account := p.pool.Get(uid); account != nil {
					view := account.View()
					result = map[string]any{
						"ok": true, "uid": view.UID,
						"proxy_slot": view.ProxySlot,
						"proxy":      view.Proxy,
						"message":    "出口已更新",
					}
				}
			}
		case "test":
			result, failure = p.testAccount(r.Context(), uid)
			if failure != nil {
				writeAdminError(w, failure)
				return
			}
		case "delete":
			if p.pool.Get(uid) == nil {
				writeAdminError(w, adminErrorFrom(ErrAccountNotFound))
				return
			}
			if err := p.pool.Remove(uid); err != nil {
				if errors.Is(err, ErrAccountNotFound) {
					writeAdminError(w, adminErrorFrom(err))
					return
				}
				log.Printf("codearts: remove auth files for %s: %v", uid, err)
				writeAdminError(w, &adminError{
					Status: http.StatusInternalServerError, Type: "server_error",
					Message: "账号文件删除失败，请检查账号文件权限",
				})
				return
			}
			p.invalidateHealth()
			result = map[string]any{
				"ok": true, "deleted": true, "message": "已删除 " + uid,
			}
		}
		if actionErr != nil {
			writeAdminError(w, adminErrorFrom(actionErr))
			return
		}
		if result == nil {
			result = map[string]any{"ok": true}
		}
		if action != "delete" {
			p.invalidateHealth()
		}
		writeAdminJSON(w, http.StatusOK, result)
	}
}

func accountSafeError(err error, account *Account, limit int) string {
	auth := Auth{}
	if account != nil {
		auth = creditsAccountAuth(account)
	}
	message := redactedCreditsError(err, auth, limit)
	if strings.TrimSpace(message) == "" {
		return "codearts: request failed"
	}
	return message
}

func (p *Provider) testAccount(ctx context.Context, uid string) (map[string]any, *adminError) {
	account := p.pool.Get(uid)
	if account == nil {
		return nil, adminErrorFrom(ErrAccountNotFound)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if p.tokenManager != nil {
		tokenCtx, cancel := context.WithTimeout(ctx, creditInfoTimeout)
		err := p.tokenManager.Ensure(tokenCtx, account)
		cancel()
		if err != nil {
			return map[string]any{
				"ok": false, "uid": uid, "message": accountSafeError(err, account, creditErrorMessageRunes),
			}, nil
		}
	}

	modelsCtx, cancel := context.WithTimeout(ctx, creditInfoTimeout)
	models, err := p.FetchModels(modelsCtx, account, true)
	cancel()
	if err != nil {
		return map[string]any{
			"ok": false, "uid": uid, "message": accountSafeError(err, account, creditErrorMessageRunes),
		}, nil
	}

	infoCtx, cancel := context.WithTimeout(ctx, creditInfoTimeout)
	info, err := p.fetchInfo(infoCtx, account, true)
	cancel()
	if err != nil {
		return map[string]any{
			"ok": false, "uid": uid, "message": accountSafeError(err, account, creditErrorMessageRunes),
		}, nil
	}

	var credits any
	if info != nil && info.Credits.Total != nil {
		credits = info.Credits.Total.Remain
	}
	return map[string]any{
		"ok":      true,
		"uid":     uid,
		"models":  len(models),
		"credits": credits,
		"message": "连接正常",
	}, nil
}

func (p *Provider) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if _, failure := readAdminPayload(w, r); failure != nil {
		writeAdminError(w, failure)
		return
	}
	if _, err := p.applySettings(); err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	if p.oauthSessions == nil {
		writeAdminError(w, serverAdminError("codearts: OAuth sessions are not configured"))
		return
	}
	port := p.listenerPort()
	if port < 1024 || port > 65535 {
		writeAdminError(w, badAdminRequest("gateway port must be between 1024 and 65535"))
		return
	}
	start, err := p.oauthSessions.Begin(p.store, port)
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	writeAdminJSON(w, http.StatusOK, start)
}

func (p *Provider) handleOAuthPoll(w http.ResponseWriter, r *http.Request) {
	payload, failure := readAdminPayload(w, r)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	if _, err := p.applySettings(); err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	poll, err := p.oauthPoll(adminText(payload["session_id"]))
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	writeAdminJSON(w, http.StatusOK, poll)
}

func (p *Provider) oauthPoll(sessionID string) (OAuthPoll, error) {
	if p.oauthSessions == nil {
		return OAuthPoll{}, errors.New("codearts: OAuth sessions are not configured")
	}
	poll, err := p.oauthSessions.Poll(p.store, sessionID)
	if err != nil {
		return OAuthPoll{}, err
	}
	// Ticket fallback completion persists the auth file inside Poll but has no
	// Auth value to return. Reload that account so the panel can use it without
	// a restart. Best effort: a poll response is still valid if the reload
	// fails, and the next poll retries.
	if poll.Status == "done" && poll.Account != nil {
		if err := p.syncOAuthAccount(poll.Account.UID); err != nil {
			log.Printf("codearts: reload OAuth account %s: %v", poll.Account.UID, err)
		}
	}
	return poll, nil
}

func (p *Provider) syncOAuthAccount(uid string) error {
	uid = strings.TrimSpace(uid)
	if uid == "" || p.pool == nil {
		return nil
	}
	// Always reload from the saved auth document. A completed ticket fallback
	// may be a re-login of an existing UID, in which case the in-memory
	// credentials are stale until this reimport. Running the same path again on
	// a later poll also retries after a partial add/enable failure.
	path, err := AuthPath(p.accountsDir, uid)
	if err != nil {
		return err
	}
	auth, err := LoadAuth(path)
	if err != nil {
		return err
	}
	_, err = p.saveAccountAndEnable(auth)
	return err
}

func (p *Provider) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	payload, failure := readAdminPayload(w, r)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	result, failure := p.oauthCallback(
		adminText(payload["session_id"]),
		adminText(payload["callback_url"]),
		adminText(payload["code"]),
		adminText(payload["state"]),
	)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	writeAdminJSON(w, http.StatusOK, oauthResultPayload(result))
}

// oauthCallback accepts v1's session_id+callback_url shape and the direct
// code/state fields used by newer panel builds. A successful exchange always
// adds the returned Auth to the pool and enables the realm.
func (p *Provider) oauthCallback(
	sessionID, callbackURL, code, state string,
) (OAuthResult, *adminError) {
	if p.oauthSessions == nil {
		return OAuthResult{}, serverAdminError("codearts: OAuth sessions are not configured")
	}
	var result OAuthResult
	var err error
	switch {
	case strings.TrimSpace(callbackURL) != "":
		result, err = p.oauthSessions.CallbackURL(p.store, sessionID, callbackURL)
	case strings.TrimSpace(code) == "" && strings.TrimSpace(state) == "":
		return OAuthResult{}, badAdminRequest(oauthCallbackMissingMessage)
	default:
		result, err = p.oauthSessions.Callback(p.store, sessionID, code, state)
	}
	if err != nil {
		return OAuthResult{}, adminErrorFrom(err)
	}
	if result.Auth != nil {
		if _, saveErr := p.saveAccountAndEnable(*result.Auth); saveErr != nil {
			return OAuthResult{}, adminErrorFrom(saveErr)
		}
	}
	return result, nil
}

// oauthResultPayload keeps v1's public callback shape. The Auth value is
// excluded by OAuthResult's json:"-" tag; the account identity is duplicated
// under account for the legacy dashboard plus uid/nickname for newer clients.
func oauthResultPayload(result OAuthResult) map[string]any {
	out := map[string]any{"ok": result.OK}
	if result.Status != "" {
		out["status"] = result.Status
	}
	if result.Message != "" {
		out["message"] = result.Message
	}
	if result.SessionID != "" {
		out["session_id"] = result.SessionID
	}
	if result.AuthURL != "" {
		out["auth_url"] = result.AuthURL
	}
	if result.NextURL != "" {
		out["next_url"] = result.NextURL
	}
	if result.UID != "" {
		out["uid"] = result.UID
		out["nickname"] = result.Nickname
		out["account"] = map[string]any{
			"uid": result.UID, "nickname": result.Nickname,
		}
	}
	return out
}

func (p *Provider) handleCallbackGET(w http.ResponseWriter, r *http.Request) {
	status, body := p.oauthCallbackGET(r.URL.Query())
	raw := []byte(body)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func (p *Provider) oauthCallbackGET(query url.Values) (int, string) {
	value := func(key string) string {
		return strings.TrimSpace(query.Get(key))
	}
	ticketID := value("ticket_id")
	secret := value("secret")
	state := value("state")
	if ticketID == "" && secret == "" && state == "" {
		return http.StatusBadRequest, callbackHTML(
			"回调缺少会话标识，请回到看板粘贴完整回调地址。",
		)
	}

	host := "127.0.0.1"
	if port := p.listenerPort(); port > 0 {
		host = fmt.Sprintf("127.0.0.1:%d", port)
	}
	callbackURL := (&url.URL{
		Scheme:   "http",
		Host:     host,
		Path:     "/oauth/callback",
		RawQuery: query.Encode(),
	}).String()
	result, failure := p.oauthCallback("", callbackURL, "", "")
	if failure != nil {
		return http.StatusBadRequest, callbackHTML(publicOAuthFailureMessage(failure))
	}
	if result.Status == "continue" && result.NextURL != "" {
		return http.StatusOK,
			`<meta http-equiv="refresh" content="0;url=` + html.EscapeString(result.NextURL) +
				`"><h3>正在继续华为云授权…</h3>`
	}
	return http.StatusOK, callbackHTML("CodeArts 登录成功，可以关闭此页面。")
}

func callbackHTML(message string) string {
	return `<!doctype html><meta charset="utf-8"><body style="font-family:sans-serif;padding:32px;line-height:1.7">` +
		"<h3>" + html.EscapeString(message) + "</h3></body>"
}

func publicOAuthFailureMessage(failure *adminError) string {
	if failure == nil {
		return oauthGenericFailureMessage
	}
	switch failure.Message {
	case oauthSessionMissingMessage:
		return "登录会话不存在或已过期，请回到看板重新发起。"
	case oauthCallbackMissingMessage:
		return "回调地址缺少 code 或 secret。"
	case oauthStateMessage, oauthReplayMessage, oauthCallbackURLMessage, oauthRedirectMessage:
		return "登录失败：" + failure.Message
	default:
		return oauthGenericFailureMessage
	}
}

func (p *Provider) handleCreditsRefresh(w http.ResponseWriter, r *http.Request) {
	if _, failure := readAdminPayload(w, r); failure != nil {
		writeAdminError(w, failure)
		return
	}
	if failure := p.requireAdminEnabled(); failure != nil {
		writeAdminError(w, failure)
		return
	}
	result, err := p.RefreshCredits(r.Context())
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	writeAdminJSON(w, http.StatusOK, result)
}

func (p *Provider) handleCheckin(w http.ResponseWriter, r *http.Request) {
	payload, failure := readAdminPayload(w, r)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	if failure := p.requireAdminEnabled(); failure != nil {
		writeAdminError(w, failure)
		return
	}
	uid := adminText(payload["uid"])
	var result CheckinResult
	var err error
	if uid == "" {
		result, err = p.CheckinAll(r.Context())
	} else {
		result, err = p.Checkin(r.Context(), uid)
	}
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	p.invalidateHealth()
	writeAdminJSON(w, http.StatusOK, result)
}

func (p *Provider) handleKeepalive(w http.ResponseWriter, r *http.Request) {
	if _, failure := readAdminPayload(w, r); failure != nil {
		writeAdminError(w, failure)
		return
	}
	if failure := p.requireAdminEnabled(); failure != nil {
		writeAdminError(w, failure)
		return
	}
	if p.tokenManager == nil {
		writeAdminError(w, serverAdminError("codearts: token manager is not configured"))
		return
	}
	refreshed, err := p.tokenManager.Keepalive(r.Context())
	if err != nil && r.Context().Err() != nil {
		writeAdminError(w, adminErrorFrom(r.Context().Err()))
		return
	}
	// v1 deliberately swallows per-account refresh errors and reports a
	// successful sweep; the count still shows how many accounts renewed.
	accounts := 0
	if p.pool != nil {
		accounts = len(p.pool.Accounts())
	}
	p.invalidateHealth()
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"refreshed": refreshed,
		"message": fmt.Sprintf(
			"已检查 %d 个账号，续期 %d 个", accounts, refreshed,
		),
	})
}

func (p *Provider) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if _, failure := readAdminPayload(w, r); failure != nil {
		writeAdminError(w, failure)
		return
	}
	if failure := p.requireAdminEnabled(); failure != nil {
		writeAdminError(w, failure)
		return
	}
	writeAdminJSON(w, http.StatusOK, p.refreshModels(r.Context()))
}

// invalidateModels drops every account's adopted catalogue and marks any
// in-flight discovery stale so a forced refresh cannot join or republish the
// old set. Clearing modelsFlight under modelsMu makes the next forced refresh
// start a fresh discovery; the detached leader can still finish for callers
// already waiting, but its epoch mismatch prevents it from storing results.
func (p *Provider) invalidateModels() {
	if p == nil || p.pool == nil {
		return
	}
	for _, account := range p.pool.Accounts() {
		// Lock order matches ensureModels/reimport: modelsMu then mu.
		account.modelsMu.Lock()
		account.mu.Lock()
		account.Models = nil
		account.modelBenefit = nil
		account.modelsFetchedAt = time.Time{}
		account.modelsAttemptedAt = time.Time{}
		account.modelsEpoch++
		account.modelsFlight = nil
		account.mu.Unlock()
		account.modelsMu.Unlock()
	}
}

// refreshModels invalidates the catalog, force-fetches every enabled account,
// and reports the v1 {ok, models, catalog} envelope. Discovery failures keep
// the official/benefit seed fallback instead of failing the route, matching
// v1's best-effort refresh.
func (p *Provider) refreshModels(ctx context.Context) map[string]any {
	if ctx == nil {
		ctx = context.Background()
	}
	p.invalidateModels()
	entries, err := p.Models(ctx, true)
	ok := err == nil
	return map[string]any{
		"ok":      ok,
		"models":  len(entries),
		"catalog": p.modelsMeta(),
	}
}

// modelsMeta mirrors v1 models_meta without exposing credentials.
func (p *Provider) modelsMeta() map[string]any {
	accounts := 0
	var fetchedAt time.Time
	if p.pool != nil {
		accountList := p.pool.Accounts()
		accounts = len(accountList)
		for _, account := range accountList {
			account.mu.Lock()
			stamp := account.modelsFetchedAt
			account.mu.Unlock()
			if stamp.After(fetchedAt) {
				fetchedAt = stamp
			}
		}
	}
	meta := map[string]any{
		"source":     "codearts",
		"fetched_at": nil,
		"stale":      false,
		"accounts":   accounts,
	}
	if !fetchedAt.IsZero() {
		meta["fetched_at"] = unixSeconds(fetchedAt)
	}
	return meta
}

// CatalogProvenance returns the v1 /v1/models catalog block.
func (p *Provider) CatalogProvenance() map[string]any {
	if p == nil {
		return nil
	}
	return p.modelsMeta()
}

// RealmAvailable reports whether the CodeArts exit is enabled, matching v1's
// conditional /realm option.
func (p *Provider) RealmAvailable() bool {
	if p == nil {
		return false
	}
	cfg, err := p.applySettings()
	return err == nil && cfg.Enabled
}

func (p *Provider) handleConnectionTest(w http.ResponseWriter, r *http.Request) {
	if _, failure := readAdminPayload(w, r); failure != nil {
		writeAdminError(w, failure)
		return
	}
	if failure := p.requireAdminEnabled(); failure != nil {
		writeAdminError(w, failure)
		return
	}
	health, err := p.Health(r.Context(), true)
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	if health["ok"] != true {
		writeAdminJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "health": health,
		})
		return
	}
	value, err := p.Overview(r.Context(), true)
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	overview, _ := value.(map[string]any)
	models, _ := overview["models"].([]map[string]any)
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"health": health,
		"stats":  overview["stats"],
		"models": len(models),
	})
}
