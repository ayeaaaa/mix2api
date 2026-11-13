package doubao

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const maxAdminBody = 8 << 20

func (p *Provider) routes() []providers.Route {
	panel := providers.AuthPanel
	return []providers.Route{
		{Method: http.MethodGet, Path: "/doubao/overview", Auth: panel, Handler: p.handleOverview},
		{Method: http.MethodGet, Path: "/doubao/accounts", Auth: panel, Handler: p.handleAccounts},
		{Method: http.MethodPost, Path: "/doubao/accounts/import", Auth: panel, Handler: p.handleImport},
		{Method: http.MethodPost, Path: "/doubao/accounts/enable", Auth: panel, Handler: p.handleAccountAction("enable")},
		{Method: http.MethodPost, Path: "/doubao/accounts/disable", Auth: panel, Handler: p.handleAccountAction("disable")},
		{Method: http.MethodPost, Path: "/doubao/accounts/delete", Auth: panel, Handler: p.handleAccountAction("delete")},
		{Method: http.MethodPost, Path: "/doubao/accounts/note", Auth: panel, Handler: p.handleAccountAction("note")},
		{Method: http.MethodPost, Path: "/doubao/accounts/proxy", Auth: panel, Handler: p.handleAccountAction("proxy")},
		{Method: http.MethodPost, Path: "/doubao/accounts/probe", Auth: panel, Handler: p.handleCheckinProbe},
		{Method: http.MethodPost, Path: "/doubao/accounts/test", Auth: panel, Handler: p.handleAccountAction("test")},
		{Method: http.MethodPost, Path: "/doubao/accounts/checkin", Auth: panel, Handler: p.handleAccountCheckin},
		{Method: http.MethodPost, Path: "/doubao/checkin", Auth: panel, Handler: p.handleCheckinAll},
		{Method: http.MethodPost, Path: "/doubao/credits/refresh", Auth: panel, Handler: p.handleCreditsRefresh},
		{Method: http.MethodPost, Path: "/doubao/tasks/refresh", Auth: panel, Handler: p.handleTasksRefresh},
		{Method: http.MethodPost, Path: "/doubao/models/refresh", Auth: panel, Handler: p.handleModelsRefresh},
		{Method: http.MethodPost, Path: "/doubao/test", Auth: panel, Handler: p.handleConnectionTest},
	}
}

func (p *Provider) handleOverview(w http.ResponseWriter, r *http.Request) {
	value, err := p.Overview(r.Context(), forceQuery(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (p *Provider) handleAccounts(w http.ResponseWriter, r *http.Request) {
	value, err := p.Overview(r.Context(), false)
	if err != nil {
		writeError(w, err)
		return
	}
	overview, _ := value.(map[string]any)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": ProviderID, "accounts": overview["accounts"],
		"stats": overview["stats"], "models": overview["models"],
		"base_url": overview["base_url"], "aid": overview["aid"],
	})
}

func (p *Provider) handleImport(w http.ResponseWriter, r *http.Request) {
	payload, err := readPayload(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	cookies := stringMap(payload["cookies"])
	device := stringMap(payload["device"])
	if len(cookies) == 0 {
		writeError(w, &APIError{Kind: KindBadRequest, Message: "cookies is required"})
		return
	}
	enabled := true
	if value, ok := payload["enabled"].(bool); ok {
		enabled = value
	}
	account, err := p.pool.Add(cookies, device, textValue(payload["uid"]), textValue(payload["nickname"]), enabled)
	if err != nil {
		writeError(w, &APIError{Kind: KindBadRequest, Message: "import failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": account.view()})
}

func (p *Provider) handleAccountAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		payload, err := readPayload(w, r)
		if err != nil {
			writeError(w, err)
			return
		}
		uid := textValue(payload["uid"])
		if action == "import" {
			p.handleImport(w, r)
			return
		}
		var actionErr error
		switch action {
		case "enable":
			actionErr = p.pool.Enable(uid)
		case "disable":
			actionErr = p.pool.Disable(uid)
		case "delete":
			actionErr = p.pool.Remove(uid)
		case "note":
			actionErr = p.pool.SetNote(uid, textValue(payload["note"]))
		case "proxy":
			actionErr = p.pool.SetProxySlot(uid, textValue(payload["proxy_slot"]))
		case "test":
			if p.pool.account(uid) == nil {
				actionErr = errors.New("doubao: account not found")
			}
		default:
			actionErr = errors.New("doubao: unknown action")
		}
		if actionErr != nil {
			writeError(w, actionErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (p *Provider) handleCheckinProbe(w http.ResponseWriter, r *http.Request) {
	payload, err := readPayload(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	uid := textValue(payload["uid"])
	query := textValue(payload["query"])
	ua := textValue(payload["ua"])
	if err := p.pool.SetCheckinProbe(uid, query, ua); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "checkin_probe": true})
}

func (p *Provider) handleAccountCheckin(w http.ResponseWriter, r *http.Request) {
	payload, err := readPayload(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	uid := textValue(payload["uid"])
	result, err := p.Checkin(r.Context(), uid)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *Provider) handleCheckinAll(w http.ResponseWriter, r *http.Request) {
	results, err := p.CheckinAll(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

func (p *Provider) handleCreditsRefresh(w http.ResponseWriter, r *http.Request) {
	payload, err := readPayload(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	data, err := p.RefreshCredits(r.Context(), textValue(payload["uid"]))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": data})
}

func (p *Provider) handleTasksRefresh(w http.ResponseWriter, r *http.Request) {
	payload, err := readPayload(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	uid := textValue(payload["uid"])
	status, err := p.CheckinStatus(r.Context(), uid)
	if err != nil {
		writeError(w, err)
		return
	}
	account := p.pool.account(uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "status": status, "tasks": account.Tasks, "credits": account.Credits,
	})
}

func (p *Provider) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": ListModels()})
}

func (p *Provider) handleConnectionTest(w http.ResponseWriter, r *http.Request) {
	cfg := p.currentConfig()
	stats := p.pool.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": cfg.Enabled && stats.Healthy > 0, "enabled": cfg.Enabled,
		"accounts": stats.Total, "healthy": stats.Healthy,
		"base_url": cfg.BaseURL, "aid": cfg.AID, "models": len(models),
	})
}

func readPayload(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	if r == nil || r.Body == nil {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]any{}, nil
		}
		return nil, &APIError{Kind: KindBadRequest, Message: "invalid JSON body"}
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return nil, &APIError{Kind: KindBadRequest, Message: "expected a JSON object"}
	}
	return payload, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	message := "doubao: request failed"
	errType := "invalid_request_error"
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		status = apiErr.HTTPStatus()
		if text := strings.TrimSpace(apiErr.Message); text != "" {
			message = text
		}
		errType = apiErr.ErrorType()
	} else if strings.Contains(strings.ToLower(err.Error()), "not found") {
		status = http.StatusNotFound
		message = "account not found"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": message, "type": errType, "code": status,
	}})
}

func stringMap(value any) map[string]string {
	raw, _ := value.(map[string]any)
	out := make(map[string]string, len(raw))
	for key, item := range raw {
		if text, ok := item.(string); ok {
			out[key] = text
		}
	}
	return out
}

func forceQuery(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}
