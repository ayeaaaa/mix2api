package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
)

// UsageHandlers owns the panel usage endpoints. It is mounted separately from
// the provider router so the panel-auth middleware from the composition layer
// can guard the whole group.
type UsageHandlers struct {
	Service *usage.Service

	// Gate is the panel-prefix middleware. A nil gate is allowed only when the
	// caller mounts the handlers behind its own already-authenticated mux
	// (tests); production composition must supply it.
	Gate func(http.Handler) http.Handler
}

// NewUsageHandlers builds the usage route group.
func NewUsageHandlers(service *usage.Service, gate func(http.Handler) http.Handler) *UsageHandlers {
	return &UsageHandlers{Service: service, Gate: gate}
}

// Register mounts the v1 usage routes. Unknown paths under /usage are not
// registered here; the composition layer's panel-prefix gate covers them.
func (h *UsageHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	routes := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/usage", h.handleSnapshot},
		{"/v1/usage", h.handleSnapshot},
		{"/usage/recent", h.handleRecent},
		{"/usage/analytics", h.handleAnalytics},
		{"/usage/perf", h.handlePerf},
		{"/usage/timeseries", h.handleTimeseries},
		{"/usage/by-account", h.handleByAccount},
		{"/usage/remaining", h.handleRemaining},
	}
	for _, route := range routes {
		handler := http.Handler(route.handler)
		if h.Gate != nil {
			handler = h.Gate(handler)
		}
		mux.Handle(route.path, methodGET(handler))
	}
}

func methodGET(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *UsageHandlers) realm(r *http.Request) string {
	realm := strings.TrimSpace(r.URL.Query().Get("realm"))
	if realm == "" {
		realm = strings.TrimSpace(r.Header.Get("X-Realm"))
	}
	if realm == "" && h.Service != nil {
		realm = h.Service.DefaultRealm
	}
	return realm
}

type usageWindow struct {
	Range string
	Since *time.Time
	Until *time.Time
}

// analyticsRealm deliberately does not fall back to the service default:
// v1 analytics reports both realms unless the caller names one.
func (h *UsageHandlers) analyticsRealm(r *http.Request) string {
	realm := strings.TrimSpace(r.URL.Query().Get("realm"))
	if realm == "" {
		realm = strings.TrimSpace(r.Header.Get("X-Realm"))
	}
	return realm
}

func (h *UsageHandlers) window(r *http.Request) usageWindow {
	rawRange := r.URL.Query().Get("range")
	resolved := usage.ParseRangeQuery(rawRange,
		r.URL.Query().Get("since"), r.URL.Query().Get("until"))
	return usageWindow{Range: rawRange, Since: resolved.Since, Until: resolved.Until}
}

func (h *UsageHandlers) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "usage service unavailable")
		return
	}
	window := h.window(r)
	opts := usage.SnapshotOptions{Realm: h.realm(r), Range: window.Range, Since: window.Since, Until: window.Until}
	before := h.Service.SnapshotETag(opts)
	data, err := h.Service.Snapshot(r.Context(), opts)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "usage snapshot failed")
		return
	}
	h.writeCached(w, r, stableETag(before, h.Service.SnapshotETag(opts)), data)
}

func (h *UsageHandlers) handleRecent(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "usage service unavailable")
		return
	}
	limit := parseQueryInt(r, "limit", 100)
	page := parseQueryInt(r, "page", 1)
	pageData, err := h.Service.Recent(r.Context(), usage.RecentOptions{
		Limit: limit, Page: page, Realm: h.realm(r),
	})
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "usage recent failed")
		return
	}
	writeJSONResponse(w, http.StatusOK, pageData)
}

func (h *UsageHandlers) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "usage service unavailable")
		return
	}
	window := h.window(r)
	opts := usage.AnalyticsOptions{Realm: h.analyticsRealm(r), Range: window.Range, Since: window.Since, Until: window.Until}
	before := h.Service.AnalyticsETag(opts)
	data, err := h.Service.Analytics(r.Context(), opts)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "usage analytics failed")
		return
	}
	h.writeCached(w, r, stableETag(before, h.Service.AnalyticsETag(opts)), data)
}

func (h *UsageHandlers) handlePerf(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "usage service unavailable")
		return
	}
	window := h.window(r)
	sample := parseQueryInt(r, "sample", 5000)
	if sample < 10 {
		sample = 10
	}
	if sample > 20000 {
		sample = 20000
	}
	opts := usage.PerfOptions{Realm: h.realm(r), Range: window.Range, Since: window.Since, Until: window.Until, Sample: sample}
	before := h.Service.PerfETag(opts)
	data, err := h.Service.Perf(r.Context(), opts)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "usage perf failed")
		return
	}
	h.writeCached(w, r, stableETag(before, h.Service.PerfETag(opts)), data)
}

func (h *UsageHandlers) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "usage service unavailable")
		return
	}
	window := h.window(r)
	bucket := parseQueryInt(r, "bucket", 0)
	opts := usage.SeriesOptions{Realm: h.realm(r), Range: window.Range, Since: window.Since, Until: window.Until, BucketSeconds: bucket}
	before := h.Service.SeriesETag(opts)
	data, err := h.Service.Timeseries(r.Context(), opts)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "usage timeseries failed")
		return
	}
	h.writeCached(w, r, stableETag(before, h.Service.SeriesETag(opts)), data)
}

func (h *UsageHandlers) handleByAccount(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "usage service unavailable")
		return
	}
	before := h.Service.ByAccountETag()
	data, err := h.Service.ByAccount(r.Context())
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "usage by-account failed")
		return
	}
	h.writeCached(w, r, stableETag(before, h.Service.ByAccountETag()), data)
}

func (h *UsageHandlers) handleRemaining(w http.ResponseWriter, r *http.Request) {
	if h.Service == nil {
		writeV1Error(w, http.StatusServiceUnavailable, "server_error", "usage service unavailable")
		return
	}
	before := h.Service.RemainingETag()
	data, err := h.Service.Remaining(r.Context())
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "usage remaining failed")
		return
	}
	h.writeCached(w, r, stableETag(before, h.Service.RemainingETag()), data)
}

func stableETag(before, after string) string {
	if before != "" && before != after {
		return ""
	}
	return after
}

func (h *UsageHandlers) writeCached(w http.ResponseWriter, r *http.Request, etag string, data any) {
	if etag != "" {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if ifNoneMatch(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	writeJSONResponse(w, http.StatusOK, data)
}

func ifNoneMatch(header, etag string) bool {
	for _, token := range strings.Split(header, ",") {
		token = strings.TrimSpace(token)
		if token == "*" || token == etag {
			return true
		}
		if strings.HasPrefix(token, "W/") && strings.TrimSpace(token[2:]) == etag {
			return true
		}
	}
	return false
}

func parseQueryInt(r *http.Request, name string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
