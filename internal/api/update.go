package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
)

// UpdateHandlers owns the panel update status and request routes. Actual
// installation remains the privileged executor's job.
type UpdateHandlers struct {
	Checker     *update.Checker
	Requests    *update.RequestManager
	Version     string
	Addr        string
	Port        int
	KeepBackups func() int
	Gate        func(http.Handler) http.Handler
}

// NewUpdateHandlers builds the update route group.
func NewUpdateHandlers(checker *update.Checker, requests *update.RequestManager, version string, gate func(http.Handler) http.Handler) *UpdateHandlers {
	return &UpdateHandlers{Checker: checker, Requests: requests, Version: version, Gate: gate}
}

// Register mounts the five v1 update routes behind the panel gate.
func (h *UpdateHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	register := func(path string, handler http.HandlerFunc, post bool) {
		var wrapped http.Handler = handler
		if h.Gate != nil {
			wrapped = h.Gate(wrapped)
		}
		if post {
			wrapped = methodPOST(wrapped)
		} else {
			wrapped = methodGET(wrapped)
		}
		mux.Handle(path, wrapped)
	}
	register("/updates", h.handleStatus, false)
	register("/updates/backups", h.handleBackups, false)
	register("/updates/check", h.handleCheck, true)
	register("/updates/install", h.handleInstall, true)
	register("/updates/rollback", h.handleRollback, true)
}

func (h *UpdateHandlers) statusPayload() map[string]any {
	payload := map[string]any{}
	if h.Checker != nil {
		payload = statusToMap(h.Checker.Status())
	} else {
		payload = map[string]any{
			"current_version": h.Version, "latest_version": nil, "latest_tag": "",
			"update_available": false, "enabled": false, "checking": false,
			"last_attempt": nil, "last_success": nil, "last_error": "",
			"release_url": "", "published_at": "", "msg": "更新检查未运行",
		}
	}
	manager := h.Requests
	if manager == nil {
		manager = update.NewRequestManager(update.RequestManagerOptions{})
	}
	installation := manager.Status()
	payload["install_enabled"] = manager.Enabled()
	payload["installation"] = installation
	return payload
}

func (h *UpdateHandlers) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, http.StatusOK, h.statusPayload())
}

func (h *UpdateHandlers) handleBackups(w http.ResponseWriter, r *http.Request) {
	manager := h.Requests
	if manager == nil {
		manager = update.NewRequestManager(update.RequestManagerOptions{})
	}
	inventory := manager.Backups()
	payload := map[string]any{
		"source": inventory.Source, "backups": inventory.Backups,
		"current_version": h.currentVersion(), "rollback_enabled": manager.Enabled(),
		"keep_backups": h.keepBackups(), "keep_backups_max": update.KeepBackupsMax,
	}
	if inventory.Msg != "" {
		payload["msg"] = inventory.Msg
	}
	writeJSONResponse(w, http.StatusOK, payload)
}

func (h *UpdateHandlers) handleCheck(w http.ResponseWriter, r *http.Request) {
	drainUpdateBody(r)
	if h.Checker == nil {
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": false, "msg": "更新检查未运行"})
		return
	}
	status := h.Checker.Check(r.Context(), true)
	payload := statusToMap(status)
	manager := h.Requests
	if manager == nil {
		manager = update.NewRequestManager(update.RequestManagerOptions{})
	}
	payload["install_enabled"] = manager.Enabled()
	payload["installation"] = manager.Status()
	payload["ok"] = true
	writeJSONResponse(w, http.StatusOK, payload)
}

func (h *UpdateHandlers) handleInstall(w http.ResponseWriter, r *http.Request) {
	drainUpdateBody(r)
	if h.Checker == nil {
		writeJSONResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "msg": "更新检查未运行"})
		return
	}
	status := h.Checker.Status()
	if status.LastError != "" {
		writeJSONResponse(w, http.StatusConflict, map[string]any{"ok": false, "msg": "请先成功检查更新"})
		return
	}
	if !status.UpdateAvailable {
		writeJSONResponse(w, http.StatusConflict, map[string]any{"ok": false, "msg": "当前没有可安装的新版本"})
		return
	}
	if status.LatestTag == "" || status.ReleaseURL == "" {
		writeJSONResponse(w, http.StatusConflict, map[string]any{"ok": false, "msg": "请先点击立即检查更新"})
		return
	}
	manager := h.Requests
	if manager == nil {
		writeJSONResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "msg": "更新检查未运行"})
		return
	}
	host, _, _ := net.SplitHostPort(strings.TrimSpace(h.listenAddr()))
	lan := host != "" && host != "127.0.0.1" && host != "::1" && host != "localhost"
	installation, err := manager.Install(status.LatestTag, status.CurrentVersion, h.listenPort(), lan, "")
	if err != nil {
		writeJSONResponse(w, http.StatusConflict, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	writeJSONResponse(w, http.StatusAccepted, queuedPayload(installation))
}

func (h *UpdateHandlers) handleRollback(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		if errors.Is(err, io.EOF) {
			writeJSONResponse(w, http.StatusConflict, map[string]any{"ok": false, "msg": "请选择要回退的版本"})
			return
		}
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	version := ""
	if payload != nil {
		version, _ = payload["version"].(string)
	}
	version = strings.TrimSpace(version)
	if version == "" {
		writeJSONResponse(w, http.StatusConflict, map[string]any{"ok": false, "msg": "请选择要回退的版本"})
		return
	}
	manager := h.Requests
	if manager == nil {
		writeJSONResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "msg": "更新检查未运行"})
		return
	}
	host, _, _ := net.SplitHostPort(strings.TrimSpace(h.listenAddr()))
	lan := host != "" && host != "127.0.0.1" && host != "::1" && host != "localhost"
	installation, err := manager.Rollback(version, h.currentVersion(), h.listenPort(), lan)
	if err != nil {
		writeJSONResponse(w, http.StatusConflict, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	writeJSONResponse(w, http.StatusAccepted, queuedPayload(installation))
}

func (h *UpdateHandlers) currentVersion() string {
	if h != nil && h.Checker != nil {
		return h.Checker.Status().CurrentVersion
	}
	if h != nil {
		return h.Version
	}
	return ""
}

func (h *UpdateHandlers) keepBackups() int {
	if h != nil && h.KeepBackups != nil {
		return update.ClampKeepBackups(h.KeepBackups())
	}
	return update.KeepBackupsDefault
}

func (h *UpdateHandlers) listenAddr() string {
	if h != nil {
		return h.Addr
	}
	return ""
}

func (h *UpdateHandlers) listenPort() int {
	if h != nil && h.Port > 0 {
		return h.Port
	}
	if h != nil && h.Requests != nil && h.Requests.Port > 0 {
		return h.Requests.Port
	}
	return 8788
}

func statusToMap(status update.Status) map[string]any {
	raw, _ := json.Marshal(status)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{}
	}
	if out["latest_version"] == "" {
		out["latest_version"] = nil
	}
	return out
}

func queuedPayload(installation update.Installation) map[string]any {
	return map[string]any{
		"ok": true, "state": installation.State, "version": installation.Version,
		"message": installation.Message,
	}
}

func drainUpdateBody(r *http.Request) {
	if r == nil || r.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
}
