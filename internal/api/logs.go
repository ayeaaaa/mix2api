package api

import (
	"bytes"
	"net/http"
	"strconv"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/logs"
)

// LogsHandlers owns the panel runtime-log endpoints. It is mounted separately
// from the provider router so the composition layer's panel-auth middleware can
// guard the whole /logs prefix.
type LogsHandlers struct {
	Store *logs.LogStore

	// Gate is the panel-prefix middleware. A nil gate is allowed only when the
	// caller mounts these handlers behind its own already-authenticated mux.
	Gate func(http.Handler) http.Handler

	// Now is injectable for deterministic export filenames.
	Now func() time.Time
}

// NewLogsHandlers builds the logs route group.
func NewLogsHandlers(store *logs.LogStore, gate func(http.Handler) http.Handler) *LogsHandlers {
	return &LogsHandlers{Store: store, Gate: gate}
}

// Register mounts the v1 log routes. It deliberately does not attach to
// api.Server; composition owns that wiring.
func (h *LogsHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	routes := []struct {
		path    string
		method  string
		handler http.HandlerFunc
	}{
		{"/logs", http.MethodGet, h.handleLogs},
		{"/logs/export", http.MethodGet, h.handleExport},
		{"/logs/clear", http.MethodPost, h.handleClear},
	}
	for _, route := range routes {
		var handler http.Handler = route.handler
		if route.method == http.MethodPost {
			handler = methodPOST(handler)
		} else {
			handler = methodGET(handler)
		}
		if h.Gate != nil {
			handler = h.Gate(handler)
		}
		mux.Handle(route.path, handler)
	}
}

func methodPOST(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *LogsHandlers) handleLogs(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "log store unavailable")
		return
	}
	query := r.URL.Query()
	limit := 200
	if raw := query.Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	sinceID := int64(0)
	if raw := query.Get("since_id"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			sinceID = parsed
		}
	}
	result, err := h.Store.Query(logs.Query{
		Limit:   limit,
		Level:   query.Get("level"),
		Tag:     query.Get("tag"),
		Search:  query.Get("search"),
		SinceID: sinceID,
	})
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "log query failed")
		return
	}

	payload := map[string]any{
		"total":  result.Total,
		"logs":   result.Logs,
		"max_id": result.MaxID,
		"mode":   h.Store.Mode(),
	}
	if h.Store.Enabled() {
		stats, statsErr := h.Store.Stats()
		if statsErr != nil {
			stats = logs.Stats{}
		}
		payload["stored"] = stats.Rows
		payload["db_bytes"] = stats.Bytes
	}
	writeJSONResponse(w, http.StatusOK, payload)
}

func (h *LogsHandlers) handleExport(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "log store unavailable")
		return
	}
	// v1 exports the newest 5000 rows regardless of the panel's current
	// filters.
	result, err := h.Store.Query(logs.Query{Limit: 5000})
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "log export failed")
		return
	}
	var body bytes.Buffer
	if err := logs.WriteExport(&body, result); err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "log export failed")
		return
	}
	filename := "wb-proxy-" + h.now().Format("20060102-150405") + ".log"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}

func (h *LogsHandlers) handleClear(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "log store unavailable")
		return
	}
	if err := h.Store.Clear(); err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "log clear failed")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *LogsHandlers) now() time.Time {
	if h != nil && h.Now != nil {
		return h.Now()
	}
	return time.Now()
}
