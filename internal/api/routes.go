// Package api exposes the generic mix2api HTTP surface.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// RegisterProviderRoutes mounts a provider's declared routes on mux. AuthClass
// is metadata for the caller's middleware; this adapter deliberately does not
// authenticate or inspect provider payloads. Panel/API-key enforcement remains
// the API/Task 14 middleware's obligation. Known paths are mounted with their
// method, and path groups with multiple routes get a sub-tree handler so
// unknown or method-mismatched paths still return the v1 JSON error envelope.
func RegisterProviderRoutes(mux *http.ServeMux, provider providers.Provider) {
	if mux == nil || provider == nil {
		return
	}
	notFoundMessage := "not found"
	if messenger, ok := provider.(interface{ RouteNotFoundMessage() string }); ok {
		if message := strings.TrimSpace(messenger.RouteNotFoundMessage()); message != "" {
			notFoundMessage = message
		}
	}
	registerRoutes(mux, provider.Routes(), notFoundMessage)
}

// RegisterRoutes is RegisterProviderRoutes without the Provider wrapper.
func RegisterRoutes(mux *http.ServeMux, routes []providers.Route) {
	registerRoutes(mux, routes, "not found")
}

func registerRoutes(mux *http.ServeMux, routes []providers.Route, notFoundMessage string) {
	if mux == nil {
		return
	}

	table := newProviderRouteTable(routes, notFoundMessage)
	groupCount := make(map[string]int)
	for _, route := range routes {
		path := strings.TrimSpace(route.Path)
		if path == "" || route.Handler == nil {
			continue
		}
		prefix := routePrefix(path)
		if prefix != "" {
			groupCount[prefix]++
		}
		pattern := path
		if method := strings.TrimSpace(route.Method); method != "" {
			pattern = strings.ToUpper(method) + " " + path
		}
		mux.HandleFunc(pattern, route.Handler)
	}

	prefixes := make([]string, 0, len(groupCount))
	for prefix, count := range groupCount {
		if count > 1 && !table.hasPath(prefix) {
			prefixes = append(prefixes, prefix)
		}
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		mux.HandleFunc(prefix, table.ServeHTTP)
	}
}

func routePrefix(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return ""
	}
	segment := strings.SplitN(trimmed, "/", 2)[0]
	if segment == "" {
		return ""
	}
	return "/" + segment + "/"
}

type providerRouteTable struct {
	byPath          map[string]map[string]http.HandlerFunc
	notFoundMessage string
}

func newProviderRouteTable(routes []providers.Route, notFoundMessage string) *providerRouteTable {
	if strings.TrimSpace(notFoundMessage) == "" {
		notFoundMessage = "not found"
	}
	table := &providerRouteTable{
		byPath:          make(map[string]map[string]http.HandlerFunc),
		notFoundMessage: notFoundMessage,
	}
	for _, route := range routes {
		path := strings.TrimSpace(route.Path)
		if path == "" || route.Handler == nil {
			continue
		}
		method := strings.ToUpper(strings.TrimSpace(route.Method))
		if method == "" {
			method = "*"
		}
		if table.byPath[path] == nil {
			table.byPath[path] = make(map[string]http.HandlerFunc)
		}
		table.byPath[path][method] = route.Handler
	}
	return table
}

func (t *providerRouteTable) hasPath(path string) bool {
	if t == nil {
		return false
	}
	_, ok := t.byPath[path]
	return ok
}

func (t *providerRouteTable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if t == nil || r == nil {
		writeProviderRouteError(w, http.StatusNotFound, "not found")
		return
	}
	methods, knownPath := t.byPath[r.URL.Path]
	if !knownPath {
		writeProviderRouteError(w, http.StatusNotFound, t.notFoundMessage)
		return
	}
	handler := methods[strings.ToUpper(r.Method)]
	if handler == nil {
		handler = methods["*"]
	}
	if handler == nil {
		writeProviderRouteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	handler(w, r)
}

func writeProviderRouteError(w http.ResponseWriter, status int, message string) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"code":    status,
		},
	})
	body := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
