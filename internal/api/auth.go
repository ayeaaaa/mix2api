package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// RequirePanelAuth returns the default cookie-less panel middleware. The token
// is accepted in X-Panel-Token or Authorization: Bearer; it is never stored in
// a cookie. Unsafe methods also reject cross-site Origin/Sec-Fetch-Site values
// so a browser page on another origin cannot drive panel mutations when the
// deployment relies only on the header token.
func RequirePanelAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if next == nil {
			next = http.NotFoundHandler()
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !safePanelOrigin(r) {
				writeV1Error(w, http.StatusForbidden, "invalid_request_error", "cross-origin panel request rejected")
				return
			}
			supplied := panelTokenFromRequest(r)
			if !constantTimeTokenEqual(supplied, token) {
				writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "panel password required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAPIKeyAuth returns the default API-key middleware. It accepts the
// spellings v1 clients actually send: Authorization (with or without Bearer),
// x-api-key/api-key/x-auth-token, and the query fallbacks used by the panel.
func RequireAPIKeyAuth(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if next == nil {
			next = http.NotFoundHandler()
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !constantTimeTokenEqual(apiKeyFromRequest(r), key) {
				writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "invalid api key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// constantTimeTokenEqual compares credentials without data-dependent early
// exits. Empty configured tokens never match: fail closed.
func constantTimeTokenEqual(supplied, configured string) bool {
	if configured == "" {
		return false
	}
	supplied = strings.TrimSpace(supplied)
	configured = strings.TrimSpace(configured)
	suppliedHash := sha256.Sum256([]byte(supplied))
	configuredHash := sha256.Sum256([]byte(configured))
	return subtle.ConstantTimeCompare(suppliedHash[:], configuredHash[:]) == 1
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	scheme, value, found := strings.Cut(header, " ")
	if found && strings.EqualFold(scheme, "bearer") {
		header = strings.TrimSpace(value)
	}
	if len(header) >= 2 && header[0] == header[len(header)-1] &&
		(header[0] == '"' || header[0] == '\'') {
		header = strings.TrimSpace(header[1 : len(header)-1])
	}
	return header
}

func panelTokenFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if token := strings.TrimSpace(r.Header.Get("X-Panel-Token")); token != "" {
		return token
	}
	return bearerToken(r.Header.Get("Authorization"))
}

func apiKeyFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if token := bearerToken(r.Header.Get("Authorization")); token != "" {
		return token
	}
	for _, name := range []string{"X-API-Key", "Api-Key", "X-Auth-Token"} {
		if token := strings.TrimSpace(r.Header.Get(name)); token != "" {
			return token
		}
	}
	query := r.URL.Query()
	for _, name := range []string{"key", "api_key", "api-key"} {
		if token := strings.TrimSpace(query.Get(name)); token != "" {
			return token
		}
	}
	return ""
}

// safePanelOrigin rejects obvious cross-site panel mutations. Request without
// an Origin header (CLI, server-to-server) are allowed; only a present and
// foreign Origin or an explicit cross-site Sec-Fetch-Site is rejected.
func safePanelOrigin(r *http.Request) bool {
	if r == nil {
		return true
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site == "cross-site" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	requestHost := strings.TrimSpace(r.Host)
	if requestHost == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, requestHost)
}

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

// wrapPanelAuth protects every AuthPanel route and its top-level subtree. A nil
// middleware fails closed: the AuthPanel metadata is not a request to skip
// authentication, and an unconfigured platform must not expose the panel.
func wrapPanelAuth(base http.Handler, prefixes []string, middleware func(http.Handler) http.Handler) http.Handler {
	if base == nil {
		base = http.NotFoundHandler()
	}
	if len(prefixes) == 0 {
		return base
	}
	guarded := middleware
	if guarded == nil {
		guarded = RequirePanelAuth("")
	}
	protected := guarded(base)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if matchesPanelPrefix(r.URL.Path, prefixes) {
			protected.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}
