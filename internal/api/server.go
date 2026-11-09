// Package api exposes the generic mix2api HTTP surface.
package api

import (
	"log"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// ServerOptions configures the generic API server.
type ServerOptions struct {
	Addr    string
	Version string

	// ListenerPort is the actual bound port. It is injected into providers
	// that need a loopback OAuth callback (Raccoon) and must never be inferred
	// from the request Host header.
	ListenerPort int

	// Providers are mounted under their declared routes and are reachable
	// through the generic /v1/chat/completions and /v1/models endpoints.
	Providers []providers.Provider

	// PanelAuth wraps every AuthPanel route and its top-level provider subtree.
	// Nil fails closed for panel routes; it never leaves them open.
	PanelAuth func(http.Handler) http.Handler

	// APIKeyAuth wraps the generic /v1 endpoints and provider routes declared
	// AuthAPIKey. Nil fails closed with 401.
	APIKeyAuth func(http.Handler) http.Handler

	// APIKeyRealm optionally reports the realm bound to the request's API key.
	// A non-empty result is an operator-written exit policy: it outranks
	// X-Realm/query/body hints (including realm=all) and suppresses provider
	// realm fallback. The command currently exposes one global
	// MIX2API_API_KEY_REALM value, so every key shares the same binding;
	// per-key/multi-key realm configuration is out of scope for this slice.
	// Nil means keys do not bind a realm.
	APIKeyRealm func(*http.Request) string
}

// healthResponse is the wire shape of GET /health. The original {ok,version}
// fields stay stable; provider health is additive under a neutral realm map.
type healthResponse struct {
	OK        bool           `json:"ok"`
	Version   string         `json:"version"`
	Providers map[string]any `json:"providers,omitempty"`
}

// Server owns the generic API mux and its provider set. NewServer returns the
// corresponding http.Server; the exported type and methods also let callers
// mount a single handler directly in tests or future runtimes.
type Server struct {
	opts      ServerOptions
	providers []providers.Provider
	mux       *http.ServeMux
}

// listenerPortSetter is the optional provider contract used to inject the
// actual listener port for provider-owned loopback OAuth flows.
type listenerPortSetter interface {
	SetListenerPort(int)
}

// New returns a generic API server handler.
func New(opts ServerOptions) *Server {
	s := &Server{
		opts:      opts,
		providers: normalizeProviders(opts.Providers),
		mux:       http.NewServeMux(),
	}
	if opts.ListenerPort > 0 {
		for _, provider := range s.providers {
			if setter, ok := provider.(listenerPortSetter); ok {
				setter.SetListenerPort(opts.ListenerPort)
			}
		}
	}
	s.mount()
	return s
}

// NewServer returns an HTTP server exposing the generic health, models, chat,
// and provider routes. Header read timeouts are bounded; write timeouts are
// deliberately zero because chat responses are long-lived SSE streams.
func NewServer(opts ServerOptions) *http.Server {
	s := New(opts)
	return &http.Server{
		Addr:              opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

func (s *Server) mount() {
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.Handle("/v1/chat/completions", s.protectAPI(http.HandlerFunc(s.handleChat)))
	s.mux.Handle("/v1/models", s.protectAPI(http.HandlerFunc(s.handleModels)))
	for _, provider := range s.providers {
		registerProviderRoutes(s.mux, provider, s.decorateProviderRoute)
	}
}

// Handler returns the mux with panic recovery, optional panel authentication
// applied to every AuthPanel provider subtree, and fail-closed behavior when a
// panel middleware is not configured.
func (s *Server) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	panel := wrapPanelAuth(s.mux, panelProtectedPrefixes(s.providers), s.opts.PanelAuth)
	return recoveryHandler(panel)
}

func (s *Server) protectAPI(handler http.Handler) http.Handler {
	if s == nil || s.opts.APIKeyAuth == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "invalid api key")
		})
	}
	return s.opts.APIKeyAuth(handler)
}

// decorateProviderRoute applies AuthAPIKey to provider-declared routes. Panel
// routes are guarded at the top-level subtree so unknown paths fail closed too.
func (s *Server) decorateProviderRoute(auth providers.AuthClass, handler http.HandlerFunc) http.HandlerFunc {
	if auth != providers.AuthAPIKey {
		return handler
	}
	if s.opts.APIKeyAuth == nil {
		return func(w http.ResponseWriter, r *http.Request) {
			writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "invalid api key")
		}
	}
	protected := s.opts.APIKeyAuth(handler)
	return protected.ServeHTTP
}

// apiKeyBoundRealm returns the concrete provider realm bound to the request's
// API key. An empty result means the key is unbound. An unknown realm (or the
// WorkBuddy-only discovery alias "all") is an operator configuration error and
// is rejected instead of silently falling back to model matching.
func (s *Server) apiKeyBoundRealm(r *http.Request) (string, *apiFailure) {
	if s == nil || s.opts.APIKeyRealm == nil {
		return "", nil
	}
	realm := strings.ToLower(strings.TrimSpace(s.opts.APIKeyRealm(r)))
	if realm == "" {
		return "", nil
	}
	if realm == "all" || s.providerForRealm(realm) == nil {
		return "", localFailure(http.StatusBadRequest, "invalid api key realm: "+realm)
	}
	return realm, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	response := healthResponse{OK: true, Version: s.opts.Version}
	if len(s.providers) > 0 {
		response.Providers = make(map[string]any, len(s.providers))
		force := queryBool(r, "force")
		for _, provider := range s.providers {
			realm := providerRealm(provider)
			health, err := provider.Health(r.Context(), force)
			if err != nil {
				// A provider failure must not turn /health red or leak the
				// provider's internal error (which may contain local paths).
				response.Providers[realm] = map[string]any{"ok": false}
				continue
			}
			if health == nil {
				health = providers.Health{"ok": true}
			}
			response.Providers[realm] = health
		}
	}
	writeJSONResponse(w, http.StatusOK, response)
}

// recoveryHandler turns a panic into a 500 error envelope when no response has
// been written. A panic after SSE headers can only close the already-corrupt
// response; it must not be converted into a second HTTP status line.
func recoveryHandler(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked := &responseStateWriter{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("api: panic serving %s %s: %v", r.Method, r.URL.Path, recovered)
				if !tracked.wrote {
					writeV1Error(tracked, http.StatusInternalServerError, "server_error", "internal server error")
				}
			}
		}()
		next.ServeHTTP(tracked, r)
	})
}

type responseStateWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *responseStateWriter) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseStateWriter) Write(data []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(data)
}

func (w *responseStateWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseStateWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func normalizeProviders(list []providers.Provider) []providers.Provider {
	if len(list) == 0 {
		return nil
	}
	out := make([]providers.Provider, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, provider := range list {
		if isNilProvider(provider) {
			continue
		}
		realm := providerRealm(provider)
		if realm == "" || seen[realm] {
			continue
		}
		seen[realm] = true
		out = append(out, provider)
	}
	return out
}

func providerRealm(provider providers.Provider) string {
	if provider == nil {
		return ""
	}
	return strings.TrimSpace(provider.Realm())
}

func isNilProvider(provider providers.Provider) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
