package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const maxAdminBodyBytes = 8 << 20

// adminError is the v1-compatible provider error used by the admin surface.
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

func badRequest(message string) *adminError {
	return &adminError{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: message}
}

func serverError(message string) *adminError {
	return &adminError{Status: http.StatusInternalServerError, Type: "server_error", Message: message}
}

func adminErrorFrom(err error) *adminError {
	if err == nil {
		return serverError("raccoon: unknown error")
	}
	if errors.Is(err, ErrAccountNotFound) || errors.Is(err, ErrAccountDeleted) {
		return &adminError{
			Status: http.StatusNotFound, Type: "invalid_request_error", Message: "account not found",
		}
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		status := http.StatusBadGateway
		errType := "invalid_request_error"
		switch apiErr.Kind {
		case KindBadRequest:
			status = http.StatusBadRequest
		case KindAuth:
			status = http.StatusBadGateway
		case KindNotFound:
			status = http.StatusConflict
		case KindRateLimited:
			status = http.StatusTooManyRequests
			errType = "rate_limit_error"
		case KindUnavailable:
			status = http.StatusServiceUnavailable
		case KindTransient, KindUpstream:
			status = http.StatusBadGateway
		}
		message := strings.TrimSpace(apiErr.Message)
		if message == "" {
			message = "raccoon: upstream error"
		}
		return &adminError{Status: status, Type: errType, Message: message}
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "raccoon: request failed"
	}
	return &adminError{Status: http.StatusBadGateway, Type: "invalid_request_error", Message: message}
}

func accountActionError(err error) *adminError {
	if errors.Is(err, ErrAccountNotFound) || errors.Is(err, ErrAccountDeleted) {
		return &adminError{
			Status: http.StatusNotFound, Type: "invalid_request_error", Message: "account not found",
		}
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "account action failed"
	}
	return &adminError{Status: http.StatusInternalServerError, Type: "server_error", Message: message}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
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
		failure = serverError("raccoon: request failed")
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
		message = "raccoon: request failed"
	}
	writeJSON(w, status, map[string]any{
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
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBodyBytes))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]any{}, nil
		}
		return nil, badRequest("invalid JSON body")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, badRequest("invalid JSON body")
	}
	if value == nil {
		return nil, badRequest("expected a JSON object")
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return nil, badRequest("expected a JSON object")
	}
	return payload, nil
}

func payloadText(value any) string {
	return strings.TrimSpace(pointsText(value))
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

func requestPort(r *http.Request) (int, *adminError) {
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return 8788, nil
	}
	portText := ""
	if strings.HasPrefix(host, "[") || strings.Contains(host, ":") {
		_, port, err := net.SplitHostPort(host)
		if err != nil {
			if strings.Contains(err.Error(), "missing port") {
				return 8788, nil
			}
			return 0, badRequest("gateway port must be an integer")
		}
		portText = port
	}
	if portText == "" {
		return 8788, nil
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, badRequest("gateway port must be an integer")
	}
	if port < 1024 || port > 65535 {
		return 0, badRequest("gateway port must be between 1024 and 65535")
	}
	return port, nil
}

// Routes returns the complete v1 admin/OAuth route list. Panel authentication
// is declared through AuthPanel and enforced by the API middleware; the OAuth
// browser callback is public and validates its own state.
func (p *Provider) Routes() []providers.Route {
	panel := providers.AuthPanel
	return []providers.Route{
		{Method: http.MethodGet, Path: "/raccoon/overview", Auth: panel, Handler: p.handleOverview},
		{Method: http.MethodGet, Path: "/raccoon/accounts", Auth: panel, Handler: p.handleAccounts},
		{Method: http.MethodPost, Path: "/raccoon/accounts/import", Auth: panel, Handler: p.handleImport},
		{Method: http.MethodPost, Path: "/raccoon/accounts/enable", Auth: panel, Handler: p.handleAccountAction("enable")},
		{Method: http.MethodPost, Path: "/raccoon/accounts/disable", Auth: panel, Handler: p.handleAccountAction("disable")},
		{Method: http.MethodPost, Path: "/raccoon/accounts/delete", Auth: panel, Handler: p.handleAccountAction("delete")},
		{Method: http.MethodPost, Path: "/raccoon/accounts/clear-cooldown", Auth: panel, Handler: p.handleAccountAction("clear-cooldown")},
		{Method: http.MethodPost, Path: "/raccoon/accounts/proxy", Auth: panel, Handler: p.handleAccountAction("proxy")},
		{Method: http.MethodPost, Path: "/raccoon/accounts/note", Auth: panel, Handler: p.handleAccountAction("note")},
		{Method: http.MethodPost, Path: "/raccoon/accounts/tasks", Auth: panel, Handler: p.handleAccountAction("tasks")},
		{Method: http.MethodPost, Path: "/raccoon/accounts/test", Auth: panel, Handler: p.handleAccountAction("test")},
		{Method: http.MethodPost, Path: "/raccoon/accounts/grant-desktop", Auth: panel, Handler: p.handleAccountAction("grant-desktop")},
		{Method: http.MethodPost, Path: "/raccoon/accounts/grant-mobile", Auth: panel, Handler: p.handleAccountAction("grant-mobile")},
		{Method: http.MethodPost, Path: "/raccoon/credits/refresh", Auth: panel, Handler: p.handleCreditsRefresh},
		{Method: http.MethodPost, Path: "/raccoon/keepalive", Auth: panel, Handler: p.handleKeepalive},
		{Method: http.MethodPost, Path: "/raccoon/models/refresh", Auth: panel, Handler: p.handleModelsRefresh},
		{Method: http.MethodPost, Path: "/raccoon/test", Auth: panel, Handler: p.handleConnectionTest},
		{Method: http.MethodPost, Path: "/raccoon/accounts/oauth/start", Auth: panel, Handler: p.handleOAuthStart},
		{Method: http.MethodPost, Path: "/raccoon/accounts/oauth/poll", Auth: panel, Handler: p.handleOAuthPoll},
		{Method: http.MethodPost, Path: "/raccoon/accounts/oauth/callback", Auth: panel, Handler: p.handleOAuthCallback},
		{Method: http.MethodGet, Path: "/auth/callback", Auth: providers.AuthNone, Handler: p.handleCallbackGET},
	}
}

func (p *Provider) handleOverview(w http.ResponseWriter, r *http.Request) {
	value, err := p.Overview(r.Context(), forceFromQuery(r))
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (p *Provider) handleAccounts(w http.ResponseWriter, r *http.Request) {
	value, err := p.Overview(r.Context(), false)
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	overview, _ := value.(map[string]any)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": ProviderID,
		"accounts": overview["accounts"],
		"stats":    overview["stats"],
		"models":   overview["models"],
		"base_url": overview["base_url"],
		"health":   overview["health"],
	})
}

func (p *Provider) handleImport(w http.ResponseWriter, r *http.Request) {
	payload, failure := readAdminPayload(w, r)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	authPayload := payload
	if nested, ok := payload["auth"].(map[string]any); ok {
		authPayload = nested
	}
	auth, failure := authFromPayload(authPayload, p.nowTime())
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	account, err := p.saveAndEnable(auth)
	if err != nil {
		writeAdminError(w, &adminError{
			Status: http.StatusBadGateway, Type: "invalid_request_error",
			Message: truncateRunes(redactText(err.Error()), 300),
		})
		return
	}
	view := account.View()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     view.UID,
		"account": view,
		"enabled": true,
	})
}

// saveAndEnable persists the auth file, enables the realm, then adds the
// account to the pool. The auth file is rolled back if the enable write fails,
// matching v1 _save_and_enable.
func (p *Provider) saveAndEnable(auth Auth) (*Account, error) {
	cleanAuth(&auth)
	if auth.AccessToken == "" {
		return nil, errors.New("raccoon: auth requires access_token")
	}
	if auth.UID == "" {
		auth.UID = auth.uidFromCredentials()
	}
	if auth.UID == "" {
		return nil, errors.New("raccoon: auth requires uid")
	}
	path, err := AuthPath(p.accountsDir, auth.UID)
	if err != nil {
		return nil, err
	}

	var backup []byte
	hadBackup := false
	if data, readErr := os.ReadFile(path); readErr == nil {
		backup = data
		hadBackup = true
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("raccoon: read existing auth file: %w", readErr)
	}

	if err := SaveAuth(path, auth); err != nil {
		return nil, err
	}
	saved, err := LoadAuth(path)
	if err != nil {
		restoreAuthFile(path, backup, hadBackup)
		return nil, err
	}
	cfg, err := SaveConfig(p.store, map[string]any{"enabled": true})
	if err != nil {
		restoreAuthFile(path, backup, hadBackup)
		return nil, err
	}
	p.pool.SetConfig(cfg)

	account, err := p.pool.Add(saved)
	if err != nil {
		return nil, err
	}
	return account, nil
}

func restoreAuthFile(path string, backup []byte, existed bool) {
	if existed {
		_ = secrets.WritePrivate(path, backup)
		return
	}
	_ = os.Remove(path)
}

func (p *Provider) handleAccountAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload, failure := readAdminPayload(w, r)
		if failure != nil {
			writeAdminError(w, failure)
			return
		}
		uid := payloadText(payload["uid"])
		if uid == "" {
			writeAdminError(w, badRequest("uid required"))
			return
		}
		_, _ = p.applySettings()

		var result map[string]any
		var actionErr error
		switch action {
		case "enable":
			actionErr = p.pool.Enable(uid)
			result = map[string]any{"ok": true, "uid": uid, "message": "已启用 " + uid}
		case "disable":
			reason := payloadText(payload["reason"])
			if reason == "" {
				reason = "manual disable"
			}
			actionErr = p.pool.Disable(uid, reason)
			result = map[string]any{"ok": true, "uid": uid, "message": "已停用 " + uid}
		case "clear-cooldown":
			actionErr = p.pool.ClearCooldown(uid)
			result = map[string]any{"ok": true, "uid": uid, "message": "已清冷却 " + uid}
		case "note":
			actionErr = p.pool.SetNote(uid, payloadText(payload["note"]))
			if actionErr == nil {
				if account := p.pool.Get(uid); account != nil {
					result = map[string]any{"ok": true, "uid": uid, "note": account.View().Note}
				}
			}
		case "tasks":
			enabled := true
			if value, ok := payload["enabled"]; ok {
				enabled = pointsTruthy(value)
			}
			actionErr = p.pool.SetTasksEnabled(uid, enabled)
			if actionErr == nil {
				if account := p.pool.Get(uid); account != nil {
					result = map[string]any{
						"ok": true, "uid": uid,
						"tasks_enabled": account.View().TasksEnabled,
					}
				}
			}
		case "proxy":
			actionErr = p.pool.SetProxySlot(uid, payloadText(payload["slot_id"]))
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
		case "grant-desktop":
			result, failure = p.grantLoginPoints(r.Context(), uid, false)
			if failure != nil {
				writeAdminError(w, failure)
				return
			}
		case "grant-mobile":
			result, failure = p.grantLoginPoints(r.Context(), uid, true)
			if failure != nil {
				writeAdminError(w, failure)
				return
			}
		case "delete":
			if p.pool.Get(uid) == nil {
				writeAdminError(w, &adminError{
					Status: http.StatusNotFound, Type: "invalid_request_error",
					Message: "account not found",
				})
				return
			}
			if err := p.pool.Remove(uid); err != nil {
				if errors.Is(err, ErrAccountNotFound) || errors.Is(err, ErrAccountDeleted) {
					writeAdminError(w, &adminError{
						Status: http.StatusNotFound, Type: "invalid_request_error",
						Message: "account not found",
					})
					return
				}
				writeAdminError(w, &adminError{
					Status: http.StatusInternalServerError, Type: "server_error",
					Message: "账号文件删除失败，账号仍保留；请检查 accounts/raccoon 的文件权限",
				})
				return
			}
			result = map[string]any{
				"ok": true, "deleted": true, "message": "已删除 " + uid,
			}
		}
		if actionErr != nil {
			writeAdminError(w, accountActionError(actionErr))
			return
		}
		if result == nil {
			result = map[string]any{"ok": true, "uid": uid}
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func (p *Provider) testAccount(ctx context.Context, uid string) (map[string]any, *adminError) {
	account := p.pool.Get(uid)
	if account == nil {
		return nil, &adminError{
			Status: http.StatusNotFound, Type: "invalid_request_error", Message: "account not found",
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := p.fetchAccountCatalog(ctx, account); err != nil {
		currentUID, _ := account.currentUID()
		return map[string]any{
			"ok":      false,
			"uid":     currentUID,
			"message": redactedPointsError(err, account, maxPointsErrorRunes),
		}, nil
	}
	creditsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	credits, _ := p.pool.FetchCredits(creditsCtx, account, true)
	cancel()
	view := account.View()
	seconds := int64(account.TokenRemaining(p.nowTime()) / time.Second)
	return map[string]any{
		"ok":              true,
		"uid":             view.UID,
		"models":          view.Models,
		"credits":         credits,
		"token_remaining": formatTokenRemaining(seconds),
		"token_seconds":   seconds,
		"message":         "连接正常",
	}, nil
}

func (p *Provider) fetchAccountCatalog(ctx context.Context, account *Account) ([]providers.Model, error) {
	client := p.pool.HTTPClient()
	if client == nil {
		return nil, errors.New("raccoon: catalog http client is not configured")
	}
	tokens := p.pool.TokenManager()
	return p.pool.fetchAccountCatalog(ctx, account, client, tokens)
}

func (p *Provider) grantLoginPoints(ctx context.Context, uid string, mobile bool) (map[string]any, *adminError) {
	account := p.pool.Get(uid)
	if account == nil {
		return nil, &adminError{
			Status: http.StatusNotFound, Type: "invalid_request_error", Message: "account not found",
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var granted *GrantResult
	var err error
	rewardName := "桌面端登录奖励"
	if mobile {
		rewardName = "移动端登录奖励"
		granted, err = p.pool.GrantMobileReward(ctx, account)
	} else {
		granted, err = p.pool.GrantDesktopReward(ctx, account)
	}
	if err != nil {
		failure := adminErrorFrom(err)
		failure.Message = redactedPointsError(err, account, maxPointsErrorRunes)
		return nil, failure
	}

	var credits *CreditsSnapshot
	var tasks *PointsTasks
	warnings := make([]string, 0, 2)
	creditsCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	credits, creditsErr := p.pool.FetchCredits(creditsCtx, account, true)
	cancel()
	if creditsErr != nil {
		warnings = append(warnings,
			redactedPointsError(creditsErr, account, maxPointsErrorRunes))
	}
	tasksCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	tasks, tasksErr := p.pool.FetchTasks(tasksCtx, account, true)
	cancel()
	if tasksErr != nil {
		warnings = append(warnings,
			redactedPointsError(tasksErr, account, maxPointsErrorRunes))
	}

	var message string
	if granted.Granted {
		message = fmt.Sprintf("已领取 %d %s", granted.Points, rewardName)
	} else {
		message = rewardName + "已领取过"
	}
	if len(warnings) > 0 {
		message += "；部分数据刷新失败：" + strings.Join(warnings, "；")
	}
	currentUID, _ := account.currentUID()
	return map[string]any{
		"ok":      true,
		"uid":     currentUID,
		"granted": granted.Granted,
		"points":  granted.Points,
		"message": message,
		"credits": credits,
		"tasks":   tasks,
	}, nil
}

func (p *Provider) handleCreditsRefresh(w http.ResponseWriter, r *http.Request) {
	payload, failure := readAdminPayload(w, r)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	_, _ = p.applySettings()
	results, err := p.pool.RefreshCredits(r.Context(), payloadText(payload["uid"]))
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	ok := true
	for _, row := range results {
		if !row.OK {
			ok = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       ok,
		"accounts": results,
		"message":  fmt.Sprintf("已刷新 %d 个账号的积分", len(results)),
	})
}

func (p *Provider) handleKeepalive(w http.ResponseWriter, r *http.Request) {
	if _, failure := readAdminPayload(w, r); failure != nil {
		writeAdminError(w, failure)
		return
	}
	_, _ = p.applySettings()
	result, err := p.keepalive(r.Context())
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *Provider) keepalive(ctx context.Context) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	accounts := p.pool.Accounts()
	now := p.nowTime()
	tokens := p.pool.TokenManager()
	refreshed := 0
	errorList := make([]string, 0)

	for _, account := range accounts {
		account.mu.Lock()
		disabled := account.State.Disabled || account.State.ManualDisabled
		account.mu.Unlock()
		if disabled {
			continue
		}
		if !account.refreshDue(now) && !account.tokenIsExpired(now) {
			continue
		}
		nickname := account.View().Nickname
		if tokens == nil {
			errorList = append(errorList, nickname+": raccoon: token manager is not configured")
			continue
		}
		generation := account.credentialGeneration()
		if err := tokens.Ensure(ctx, account); err != nil {
			errorList = append(errorList,
				nickname+": "+redactedPointsError(err, account, 160))
			continue
		}
		if account.credentialGeneration() != generation {
			refreshed++
		}
	}

	var target *Account
	for _, account := range accounts {
		account.mu.Lock()
		disabled := account.State.Disabled || account.State.ManualDisabled
		account.mu.Unlock()
		if !disabled && account.Healthy(now) {
			target = account
			break
		}
	}
	if target == nil {
		for _, account := range accounts {
			account.mu.Lock()
			disabled := account.State.Disabled || account.State.ManualDisabled
			account.mu.Unlock()
			if !disabled {
				target = account
				break
			}
		}
	}

	models := len(seedCatalogModels())
	catalog := map[string]any{"source": "seed", "models": models}
	if target != nil {
		catalogCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := p.fetchAccountCatalog(catalogCtx, target)
		cancel()
		view := target.View()
		if err != nil {
			errorList = append(errorList,
				"catalog: "+redactedPointsError(err, target, 160))
			if view.Models > 0 {
				models = view.Models
				catalog = map[string]any{"source": "raccoon", "models": models}
			}
		} else {
			models = view.Models
			catalog = map[string]any{"source": "raccoon", "models": models}
		}
	}

	ok := len(errorList) == 0
	if len(errorList) > 3 {
		errorList = errorList[:3]
	}
	return map[string]any{
		"ok":        ok,
		"accounts":  len(accounts),
		"refreshed": refreshed,
		"models":    models,
		"catalog":   catalog,
		"errors":    errorList,
		"message": fmt.Sprintf(
			"已检查 %d 个账号，续期 %d 个，模型 %d 个",
			len(accounts), refreshed, models,
		),
	}, nil
}

func (p *Provider) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if _, failure := readAdminPayload(w, r); failure != nil {
		writeAdminError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, p.refreshModels(r.Context()))
}

func (p *Provider) handleConnectionTest(w http.ResponseWriter, r *http.Request) {
	if _, failure := readAdminPayload(w, r); failure != nil {
		writeAdminError(w, failure)
		return
	}
	cfg, err := p.applySettings()
	if err != nil {
		writeAdminError(w, adminErrorFrom(err))
		return
	}
	if !cfg.Enabled {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             false,
			"models":         0,
			"catalog_source": "",
			"message":        "小浣熊出口未启用：请先在设置页启用，或导入 auth.json。",
		})
		return
	}
	data := p.refreshModels(r.Context())
	ok, _ := data["ok"].(bool)
	if !ok {
		message, _ := data["message"].(string)
		if strings.TrimSpace(message) == "" {
			message = "上游模型目录不可用"
		}
		writeAdminError(w, &adminError{
			Status: http.StatusBadGateway, Type: "invalid_request_error", Message: message,
		})
		return
	}
	catalog, _ := data["catalog"].(map[string]any)
	source, _ := catalog["source"].(string)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"models":         data["models"],
		"catalog_source": source,
		"message":        "连接正常",
	})
}

func (p *Provider) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if _, failure := readAdminPayload(w, r); failure != nil {
		writeAdminError(w, failure)
		return
	}
	port, failure := requestPort(r)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	result, failure := p.oauthStart(port)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *Provider) handleOAuthPoll(w http.ResponseWriter, r *http.Request) {
	payload, failure := readAdminPayload(w, r)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	result, failure := p.oauthPoll(payloadText(payload["session_id"]))
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *Provider) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	payload, failure := readAdminPayload(w, r)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	result, failure := p.oauthCallback(
		r.Context(),
		payloadText(payload["session_id"]),
		payloadText(payload["callback_url"]),
		payloadText(payload["code"]),
		payloadText(payload["state"]),
	)
	if failure != nil {
		writeAdminError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *Provider) handleCallbackGET(w http.ResponseWriter, r *http.Request) {
	status, body := p.oauthCallbackGET(r.Context(), r.URL.Query())
	raw := []byte(body)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
