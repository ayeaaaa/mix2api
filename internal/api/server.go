// Package api exposes the generic mix2api HTTP surface.
package api

import (
	"net/http"
	"reflect"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// ServerOptions configures the generic API server.
type ServerOptions struct {
	Addr    string
	Version string

	// Providers are mounted under their declared routes and are reachable
	// through the generic /v1/chat/completions and /v1/models endpoints.
	Providers []providers.Provider

	// PanelAuth wraps every AuthPanel route and its top-level provider subtree.
	// Nil keeps the routes declared-only: the AuthPanel metadata is present but
	// enforcement is left to the platform layer, which owns session/password
	// storage. Full panel and API-key storage is later platform work.
	PanelAuth func(http.Handler) http.Handler

	// APIKeyAuth optionally wraps the generic /v1 endpoints. As with PanelAuth,
	// nil is the declared-only scaffold; paid key storage is not implemented
	// here.
	APIKeyAuth func(http.Handler) http.Handler
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

// New returns a generic API server handler.
func New(opts ServerOptions) *Server {
	s := &Server{
		opts:      opts,
		providers: normalizeProviders(opts.Providers),
		mux:       http.NewServeMux(),
	}
	s.mount()
	return s
}

// NewServer returns an HTTP server exposing the generic health, models, chat,
// and provider routes.
func NewServer(opts ServerOptions) *http.Server {
	s := New(opts)
	return &http.Server{
		Addr:    opts.Addr,
		Handler: s.Handler(),
	}
}

func (s *Server) mount() {
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.Handle("/v1/chat/completions", s.protectAPI(http.HandlerFunc(s.handleChat)))
	s.mux.Handle("/v1/models", s.protectAPI(http.HandlerFunc(s.handleModels)))
	for _, provider := range s.providers {
		RegisterProviderRoutes(s.mux, provider)
	}
}

// Handler returns the mux with optional panel authentication applied to every
// AuthPanel provider subtree.
func (s *Server) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	return wrapPanelAuth(s.mux, panelProtectedPrefixes(s.providers), s.opts.PanelAuth)
}

func (s *Server) protectAPI(handler http.Handler) http.Handler {
	if s == nil || s.opts.APIKeyAuth == nil {
		return handler
	}
	return s.opts.APIKeyAuth(handler)
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
