package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
)

// SettingsHandlers owns GET /settings, POST /settings/save and the reveal
// endpoint. The gate is the panel session middleware; a nil gate fails closed
// in production composition.
type SettingsHandlers struct {
	Service *settings.Service
	Keys    *keys.Store
	Gate    func(http.Handler) http.Handler
}

// NewSettingsHandlers builds the settings route group.
func NewSettingsHandlers(service *settings.Service, keyStore *keys.Store, gate func(http.Handler) http.Handler) *SettingsHandlers {
	return &SettingsHandlers{Service: service, Keys: keyStore, Gate: gate}
}

// Register mounts the v1 settings routes.
func (h *SettingsHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	register := func(path string, method string, handler http.HandlerFunc) {
		var wrapped http.Handler = handler
		if h.Gate != nil {
			wrapped = h.Gate(wrapped)
		}
		if method == http.MethodPost {
			wrapped = methodPOST(wrapped)
		} else {
			wrapped = methodGET(wrapped)
		}
		mux.Handle(path, wrapped)
	}
	register("/settings", http.MethodGet, h.handleView)
	register("/settings/save", http.MethodPost, h.handleSave)
	register("/settings/reveal", http.MethodGet, h.handleReveal)
}

func (h *SettingsHandlers) handleView(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "settings service unavailable")
		return
	}
	view, err := h.Service.View(r.Context())
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "settings load failed")
		return
	}
	writeJSONResponse(w, http.StatusOK, view)
}

func (h *SettingsHandlers) handleSave(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "settings service unavailable")
		return
	}
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	decoder.UseNumber()
	var patch map[string]any
	if err := decoder.Decode(&patch); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	if patch == nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "expected a JSON object")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	reply, err := h.Service.Save(r.Context(), patch)
	if err != nil {
		status := http.StatusBadRequest
		if strings.HasPrefix(err.Error(), "settings: save") || strings.Contains(err.Error(), "settings: load") {
			status = http.StatusInternalServerError
		}
		writeV1Error(w, status, "invalid_request_error", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, reply)
}

func (h *SettingsHandlers) handleReveal(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "settings service unavailable")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	key, ok := h.Service.RevealKey(id)
	if !ok {
		writeV1Error(w, http.StatusNotFound, "invalid_request_error", "no such key")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"id": id, "key": key})
}

// RealmHandlers owns GET/POST /realm.
type RealmHandlers struct {
	Service *settings.RealmService
	Gate    func(http.Handler) http.Handler
}

// NewRealmHandlers builds the realm route group.
func NewRealmHandlers(service *settings.RealmService, gate func(http.Handler) http.Handler) *RealmHandlers {
	return &RealmHandlers{Service: service, Gate: gate}
}

// Register mounts the realm routes.
func (h *RealmHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil || h.Service == nil {
		return
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			h.handleGet(w, r)
			return
		}
		if r.Method == http.MethodPost {
			h.handlePost(w, r)
			return
		}
		w.Header().Set("Allow", "GET, POST")
		writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
	var wrapped http.Handler = http.HandlerFunc(handler)
	if h.Gate != nil {
		wrapped = h.Gate(wrapped)
	}
	mux.Handle("/realm", wrapped)
}

func (h *RealmHandlers) handleGet(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"current": h.Service.Active(r.Context()),
		"options": h.Service.Options(),
	})
}

func (h *RealmHandlers) handlePost(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	realm := strings.TrimSpace(textField(payload["realm"]))
	if err := h.Service.Set(r.Context(), realm); err != nil {
		message := err.Error()
		if message == "" {
			message = "realm must be intl or cn"
		}
		writeV1Error(w, http.StatusBadRequest, "invalid_request_error", message)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"ok": true, "current": h.Service.Active(r.Context()), "persisted": true,
	})
}

func textField(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}
