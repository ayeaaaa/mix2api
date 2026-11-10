package api

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// DashboardHandlers owns the v1 host-control routes the embedded panel still
// exposes. They are panel-only and never touch the process directly: Shutdown
// is an injected seam so the command can stop its own HTTP server while tests
// and embedders stay in control.
type DashboardHandlers struct {
	Gate     func(http.Handler) http.Handler
	Shutdown func()

	once sync.Once
}

// NewDashboardHandlers builds the console/state, console/toggle and shutdown
// route group.
func NewDashboardHandlers(gate func(http.Handler) http.Handler, shutdown func()) *DashboardHandlers {
	return &DashboardHandlers{Gate: gate, Shutdown: shutdown}
}

// Register mounts the v1 dashboard control routes. All three are panel-gated;
// an unconfigured gate fails closed through the server's panelGate.
func (h *DashboardHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	gated := func(next http.HandlerFunc) http.Handler {
		var handler http.Handler = next
		if h.Gate != nil {
			handler = h.Gate(handler)
		}
		return handler
	}
	mux.Handle("/console/state", methodGET(gated(h.handleState)))
	mux.Handle("/console/toggle", methodPOST(gated(h.handleToggle)))
	mux.Handle("/shutdown", methodPOST(gated(h.handleShutdown)))
}

func (h *DashboardHandlers) handleState(w http.ResponseWriter, r *http.Request) {
	// v1 replaced a spawned console window with a web log tab. The reply keeps
	// the same shape so an old panel build can still render its button.
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"supported": true,
		"mode":      "web",
		"visible":   false,
	})
}

func (h *DashboardHandlers) handleToggle(w http.ResponseWriter, r *http.Request) {
	drainDashboardBody(r)
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"ok":      true,
		"mode":    "web",
		"url":     "/dashboard?tab=logs",
		"visible": false,
	})
}

func (h *DashboardHandlers) handleShutdown(w http.ResponseWriter, r *http.Request) {
	drainDashboardBody(r)
	if h.Shutdown == nil {
		// Composition without a shutdown seam stays honest instead of saying
		// the process is stopping when it is not.
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "shutdown unavailable")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "shutting_down": true})
	h.once.Do(func() {
		time.AfterFunc(400*time.Millisecond, h.Shutdown)
	})
}

func drainDashboardBody(r *http.Request) {
	if r == nil || r.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
}
