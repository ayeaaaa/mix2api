package api

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	panelplatform "github.com/ayeaaaa/mix2api-v2/internal/platform/panel"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// PanelHandlers owns the public status/login endpoints and the authenticated
// logout/password endpoints.
type PanelHandlers struct {
	Password     *panelplatform.Password
	Sessions     *panelplatform.Sessions
	Keys         *keys.Store
	Status       *panelplatform.Status
	Gate         func(http.Handler) http.Handler
	Limiter      *panelplatform.LoginLimiter
	FailureDelay time.Duration
	Now          func() time.Time
}

// NewPanelHandlers builds the panel route group. raccoon supplies the masked
// status section without importing a concrete provider.
func NewPanelHandlers(password *panelplatform.Password, sessions *panelplatform.Sessions, keyStore *keys.Store, raccoon func() map[string]any, gate func(http.Handler) http.Handler) *PanelHandlers {
	status := &panelplatform.Status{Password: password, Sessions: sessions}
	if keyStore != nil {
		status.APIKeySet = func() bool {
			entries, err := keyStore.List(false)
			return err == nil && len(entries) > 0
		}
	}
	status.Raccoon = raccoon
	return &PanelHandlers{
		Password: password, Sessions: sessions, Keys: keyStore, Status: status, Gate: gate,
		FailureDelay: 500 * time.Millisecond,
	}
}

// Register mounts the v1 panel routes. Status and login are public; logout and
// password change require a panel session.
func (h *PanelHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	public := func(path string, method string, handler http.HandlerFunc) {
		var wrapped http.Handler = handler
		if method == http.MethodPost {
			wrapped = methodPOST(wrapped)
		} else {
			wrapped = methodGET(wrapped)
		}
		mux.Handle(path, wrapped)
	}
	protected := func(path string, handler http.HandlerFunc) {
		var wrapped http.Handler = handler
		if h.Gate != nil {
			wrapped = h.Gate(wrapped)
		}
		wrapped = methodPOST(wrapped)
		mux.Handle(path, wrapped)
	}
	public("/panel/status", http.MethodGet, h.handleStatus)
	public("/panel/login", http.MethodPost, h.handleLogin)
	protected("/panel/logout", h.handleLogout)
	protected("/panel/password", h.handlePassword)
}

func (h *PanelHandlers) handleStatus(w http.ResponseWriter, r *http.Request) {
	view := map[string]any{}
	if h.Status != nil {
		view = h.Status.View(panelplatform.TokenFromRequest(r))
	}
	writeJSONResponse(w, http.StatusOK, view)
}

func (h *PanelHandlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	if h.Password == nil || h.Sessions == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "panel authentication unavailable")
		return
	}
	peer := peerIP(r.RemoteAddr)
	client := panelplatform.LoginClientKey(peer, r.Header)
	limiter := h.limiter()
	if allowed, reset := limiter.Check(client); !allowed {
		wait := time.Until(reset)
		seconds := int(wait.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		message := "too many login attempts, please wait " + strconv.Itoa(seconds) + "s"
		writeV1ErrorWithRetry(w, http.StatusTooManyRequests, "rate_limit_error", message, wait, reset)
		return
	}
	payload, ok := decodePanelObject(w, r)
	if !ok {
		return
	}
	password := textField(payload["password"])
	valid, err := h.Password.Verify(password)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if !valid {
		limiter.Fail(client)
		h.delayFailure()
		writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "invalid panel password")
		return
	}
	limiter.Clear(client)
	token := h.Sessions.Create()
	if token == "" {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "panel session unavailable")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"ok": true, "token": token,
		"using_default_password": h.Password.IsDefault(),
	})
}

func (h *PanelHandlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	if h.Sessions != nil {
		h.Sessions.Revoke(panelplatform.TokenFromRequest(r))
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *PanelHandlers) handlePassword(w http.ResponseWriter, r *http.Request) {
	if h.Password == nil || h.Sessions == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "panel authentication unavailable")
		return
	}
	payload, ok := decodePanelObject(w, r)
	if !ok {
		return
	}
	current := textField(payload["current"])
	newPassword := textField(payload["new"])
	valid, err := h.Password.Verify(current)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if !valid {
		writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "current password is wrong")
		return
	}
	if len(newPassword) < 4 {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "new password must be at least 4 characters")
		return
	}
	if err := h.Password.Set(newPassword); err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if newPassword != panelplatform.DefaultPassword {
		h.Sessions.RevokeAll()
	}
	token := h.Sessions.Create()
	if token == "" {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "panel session unavailable")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"ok": true, "token": token,
		"using_default_password": newPassword == panelplatform.DefaultPassword,
	})
}

func (h *PanelHandlers) limiter() *panelplatform.LoginLimiter {
	if h.Limiter == nil {
		h.Limiter = panelplatform.NewLoginLimiter()
		if h.Now != nil {
			h.Limiter.Now = h.Now
		}
	}
	return h.Limiter
}

func (h *PanelHandlers) delayFailure() {
	if h.FailureDelay <= 0 {
		return
	}
	time.Sleep(h.FailureDelay)
}

func peerIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err == nil && host != "" {
		return host
	}
	if strings.TrimSpace(remoteAddr) == "" {
		return "127.0.0.1"
	}
	return strings.TrimSpace(remoteAddr)
}

// raccoonStatus returns the masked Raccoon section from whichever provider
// implements SettingsContributor, without importing the provider package.
func (s *Server) raccoonStatus() map[string]any {
	for _, provider := range s.providers {
		if !strings.EqualFold(provider.ID(), "raccoon") {
			continue
		}
		contributor, ok := provider.(providers.SettingsContributor)
		if !ok {
			continue
		}
		view, err := contributor.SettingsView(context.Background())
		if err != nil {
			return map[string]any{"configured": false, "error": err.Error()}
		}
		if section, ok := view["raccoon"].(map[string]any); ok {
			return section
		}
		return view
	}
	return map[string]any{"configured": false}
}
