package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// panelProtectedPrefixes derives the top-level path groups owned by routes
// declared AuthPanel. Task 13's review contract requires the whole provider
// subtree (including unknown paths) to sit behind panel authentication, not
// only the exact routes the provider declared.
func panelProtectedPrefixes(list []providers.Provider) []string {
	if len(list) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	for _, provider := range list {
		if provider == nil {
			continue
		}
		for _, route := range provider.Routes() {
			if route.Auth != providers.AuthPanel {
				continue
			}
			path := strings.TrimSpace(route.Path)
			if path == "" {
				continue
			}
			path = "/" + strings.TrimLeft(path, "/")
			segment := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0]
			if segment == "" {
				continue
			}
			seen["/"+segment] = true
		}
	}
	prefixes := make([]string, 0, len(seen))
	for prefix := range seen {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	return prefixes
}

func panelPathMatches(path, prefix string) bool {
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
}

func matchesPanelPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if panelPathMatches(path, prefix) {
			return true
		}
	}
	return false
}

// wrapPanelAuth protects every AuthPanel route and its top-level subtree when a
// middleware is configured. A nil middleware is the declared-only scaffold:
// Route.Auth stays metadata for tests and future platform wiring, but no
// credential storage or enforcement exists in this slice.
func wrapPanelAuth(base http.Handler, prefixes []string, middleware func(http.Handler) http.Handler) http.Handler {
	if middleware == nil || len(prefixes) == 0 {
		return base
	}
	guarded := middleware(base)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if matchesPanelPrefix(r.URL.Path, prefixes) {
			guarded.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}
