// Package api exposes the generic mix2api HTTP surface.
package api

import (
	"context"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/agents"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/logs"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/media"
	panelplatform "github.com/ayeaaaa/mix2api-v2/internal/platform/panel"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/probes"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/proxy"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/webtools"
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

	// KeyStore enables the platform API-key gate. When set, it replaces the
	// opaque APIKeyAuth middleware for generic /v1 and provider AuthAPIKey
	// routes and supplies per-key realm/model binding plus concurrency limits.
	// KeyStore and Keys are aliases; either may be set.
	KeyStore *keys.Store
	Keys     *keys.Store
	// KeyLimiter tracks per-key in-flight requests. Nil uses a server-local
	// limiter.
	KeyLimiter *keys.Limiter

	// PanelSessions and PanelPassword enable the dynamic panel login/session
	// flow. When PanelAuth is nil, PanelSessions supplies the default gate.
	PanelSessions *panelplatform.Sessions
	PanelPassword *panelplatform.Password
	// PanelPasswords is an alias for PanelPassword.
	PanelPasswords *panelplatform.Password

	// Settings, Realm, and ProxySlots mount the panel settings, realm, and
	// proxy-slot groups. Settings may own the key store itself.
	Settings        *settings.Service
	SettingsService *settings.Service
	Realm           *settings.RealmService
	ProxySlots      *proxy.Slots
	Proxy           *proxy.Slots
	// ProxyProbe and ProxyDiscover are injected network seams for the two
	// panel routes that would otherwise contact configured exits.
	ProxyProbe    func(ctx context.Context, url string) (map[string]any, error)
	ProxyDiscover func(ctx context.Context) []map[string]any

	// Usage and LogStore mount the already-built usage and runtime-log route
	// groups behind the panel-prefix gate.
	Usage    *usage.Service
	UsageSvc *usage.Service
	LogStore *logs.LogStore
	Logs     *logs.LogStore

	// Media mounts the OpenAI-compatible image/video task surface. Nil keeps the
	// routes absent (404), matching a build without the media service.
	Media *media.Service

	// WebTools enables gateway-local web_search/web_fetch rounds in Responses
	// and Chat. Nil means the client tools pass through untouched.
	WebTools *webtools.Service

	// Agents mounts the local agent configurator panel routes.
	Agents *agents.Service

	// Probes mounts the panel quota-scan trigger.
	Probes *probes.Service

	// Updates and UpdateRequests mount the read-only update status plus the
	// validated install/rollback request bridge.
	Updates        *update.Checker
	UpdateRequests *update.RequestManager

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

	// DefaultMaxOutputTokens supplies the catalog maxOutputTokens for a model.
	// Responses and Messages use it when the client omits the limit; v1 read
	// the same value from its model catalog. Nil leaves the limit unset so the
	// provider's own default applies.
	DefaultMaxOutputTokens func(model string) int

	// AnthropicMessagesEnabled reports the panel opt-in for the Anthropic
	// bridge. A true result enables it; env WB_ENABLE_ANTHROPIC_MESSAGES is
	// also honored. Nil means only the environment controls it.
	AnthropicMessagesEnabled func() bool

	// PreserveRequestContract enables the opt-in responses.text.format mapping
	// (v1 WB_PRESERVE_REQUEST_CONTRACT). The environment flag is also honored.
	PreserveRequestContract bool

	// ProtocolObserver is the provider-neutral seam for the usage recorder.
	// Task 12 wires the real recorder here; tests use it to pin terminal
	// outcomes such as client_aborted without touching upstream state.
	ProtocolObserver func(ProtocolObservation)
}

// ProtocolObservation describes one completed Responses or Messages request.
// Usage is the raw OpenAI usage block when the upstream supplied one.
type ProtocolObservation struct {
	Protocol string
	Model    string
	Stream   bool
	Outcome  string
	Status   int
	Usage    map[string]any
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

	keyStore   *keys.Store
	keyLimiter *keys.Limiter
	sessions   *panelplatform.Sessions
	password   *panelplatform.Password
	settings   *settings.Service
	realm      *settings.RealmService
	proxySlots *proxy.Slots

	defaultTokens func(string) int
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
	s.keyStore = firstKeyStore(opts.KeyStore, opts.Keys)
	s.settings = opts.Settings
	if s.settings == nil {
		s.settings = opts.SettingsService
	}
	if s.keyStore == nil && s.settings != nil {
		s.keyStore = s.settings.Keys
	}
	s.proxySlots = opts.ProxySlots
	if s.proxySlots == nil {
		s.proxySlots = opts.Proxy
	}
	s.realm = opts.Realm
	s.keyLimiter = opts.KeyLimiter
	if s.keyLimiter == nil && s.keyStore != nil {
		s.keyLimiter = keys.NewLimiter()
	}
	s.sessions = opts.PanelSessions
	s.password = firstPassword(opts.PanelPassword, opts.PanelPasswords)
	if s.settings != nil {
		if s.settings.Keys == nil && s.keyStore != nil {
			s.settings.Keys = s.keyStore
		}
		if s.settings.PanelPasswordIsDefault == nil && s.password != nil {
			s.settings.PanelPasswordIsDefault = func() bool { return s.password.IsDefault() }
		}
		if s.settings.SlotViews == nil && s.proxySlots != nil {
			s.settings.SlotViews = func() []map[string]any { return s.proxySlots.Views() }
		}
		if s.settings.Store != nil && strings.TrimSpace(s.settings.SettingsFile) == "" {
			s.settings.SettingsFile = s.settings.Store.Path
		}
	}
	if opts.DefaultMaxOutputTokens != nil {
		s.defaultTokens = cachedModelTokens(opts.DefaultMaxOutputTokens)
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

func firstKeyStore(values ...*keys.Store) *keys.Store {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstPassword(values ...*panelplatform.Password) *panelplatform.Password {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func cachedModelTokens(lookup func(string) int) func(string) int {
	var mu sync.Mutex
	cache := map[string]int{}
	return func(model string) int {
		model = strings.TrimSpace(model)
		if model == "" {
			return 0
		}
		mu.Lock()
		if value, ok := cache[model]; ok {
			mu.Unlock()
			return value
		}
		mu.Unlock()
		value := lookup(model)
		if value > 0 {
			// A zero is usually a transient catalog miss; do not freeze it for
			// the lifetime of the process.
			mu.Lock()
			cache[model] = value
			mu.Unlock()
		}
		return value
	}
}

func (s *Server) defaultMaxOutputTokens(model string) int {
	if s == nil || s.defaultTokens == nil {
		return 0
	}
	return s.defaultTokens(model)
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
	for _, path := range []string{"/responses", "/v1/responses", "/responses/compact", "/v1/responses/compact"} {
		s.mux.Handle(path, s.protectAPI(http.HandlerFunc(s.handleResponses)))
	}
	for _, path := range []string{"/messages", "/v1/messages"} {
		s.mux.Handle(path, s.protectMessages(http.HandlerFunc(s.handleMessages)))
	}
	for _, path := range []string{"/messages/count_tokens", "/v1/messages/count_tokens"} {
		s.mux.Handle(path, s.protectMessages(http.HandlerFunc(s.handleMessagesCountTokens)))
	}
	s.mountPlatformRoutes()
	for _, provider := range s.providers {
		registerProviderRoutes(s.mux, provider, s.decorateProviderRoute)
	}
}

func (s *Server) mountPlatformRoutes() {
	if s == nil {
		return
	}
	usageService := s.opts.Usage
	if usageService == nil {
		usageService = s.opts.UsageSvc
	}
	if usageService != nil {
		NewUsageHandlers(usageService, nil).Register(s.mux)
	}
	logStore := s.opts.LogStore
	if logStore == nil {
		logStore = s.opts.Logs
	}
	if logStore != nil {
		NewLogsHandlers(logStore, nil).Register(s.mux)
	}
	settingsService := s.settings
	if settingsService != nil {
		NewSettingsHandlers(settingsService, s.keyStore, s.panelGate()).Register(s.mux)
		if s.realm != nil {
			NewRealmHandlers(s.realm, s.panelGate(), s.apiGate()).Register(s.mux)
		}
	}
	slots := s.proxySlots
	if slots != nil {
		NewProxyHandlers(slots, s.panelGate(), s.opts.ProxyProbe, s.opts.ProxyDiscover).Register(s.mux)
	}
	if s.password != nil || s.sessions != nil || s.keyStore != nil {
		NewPanelHandlers(s.password, s.sessions, s.keyStore, s.raccoonStatus, s.panelGate()).Register(s.mux)
	}
	if s.opts.Media != nil {
		NewMediaHandlers(s.opts.Media, s).Register(s.mux)
	}
	if s.opts.Agents != nil {
		NewAgentHandlers(s.opts.Agents, s.panelGate()).Register(s.mux)
	}
	if s.opts.Probes != nil {
		NewQuotaHandlers(s.opts.Probes, s.panelGate()).Register(s.mux)
	}
	if s.opts.Updates != nil || s.opts.UpdateRequests != nil {
		updates := NewUpdateHandlers(s.opts.Updates, s.opts.UpdateRequests, s.opts.Version, s.panelGate())
		updates.Addr = s.opts.Addr
		updates.Port = s.opts.ListenerPort
		if s.settings != nil && s.settings.Store != nil {
			updates.KeepBackups = func() int {
				data, err := s.settings.Store.Load()
				if err != nil {
					return 1
				}
				if value, ok := data["keep_backups"].(float64); ok {
					return int(value)
				}
				return 1
			}
		}
		updates.Register(s.mux)
	}
}

// platformPanelPrefixes are the v1 top-level groups owned by platform routes.
// The whole subtree is gated, including unknown paths, matching v1's
// _is_panel_route behavior.
var platformPanelPrefixes = []string{
	"/accounts", "/usage", "/v1/usage", "/activity", "/tasks", "/scheduler",
	"/settings", "/updates", "/logs", "/console", "/pricing", "/agents",
	"/codearts", "/raccoon",
}

func (s *Server) panelGate() func(http.Handler) http.Handler {
	if s == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	if s.sessions != nil && s.opts.PanelAuth != nil {
		sessionGate := panelplatform.RequireSession(s.sessions)
		staticGate := s.opts.PanelAuth
		return func(next http.Handler) http.Handler {
			withSession := sessionGate(next)
			withStatic := staticGate(next)
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if s.sessions.Valid(panelplatform.TokenFromRequest(r)) {
					withSession.ServeHTTP(w, r)
					return
				}
				withStatic.ServeHTTP(w, r)
			})
		}
	}
	if s.sessions != nil {
		return panelplatform.RequireSession(s.sessions)
	}
	if s.opts.PanelAuth != nil {
		return s.opts.PanelAuth
	}
	return RequirePanelAuth("")
}

func (s *Server) apiGate() func(http.Handler) http.Handler {
	if s == nil {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "invalid api key")
			})
		}
	}
	if s.keyStore != nil {
		return s.requireKeyAuth
	}
	if s.opts.APIKeyAuth != nil {
		return s.opts.APIKeyAuth
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "invalid api key")
		})
	}
}

// Handler returns the mux with panic recovery, optional panel authentication
// applied to every AuthPanel provider subtree, and fail-closed behavior when a
// panel middleware is not configured.
func (s *Server) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	prefixes := append([]string(nil), platformPanelPrefixes...)
	prefixes = append(prefixes, panelProtectedPrefixes(s.providers)...)
	panel := wrapPanelAuth(s.mux, prefixes, s.panelGate())
	return recoveryHandler(panel)
}

func (s *Server) protectAPI(handler http.Handler) http.Handler {
	return s.apiGate()(handler)
}

// decorateProviderRoute applies the API-key gate to provider-declared routes.
// Panel routes are guarded at the top-level subtree so unknown paths fail
// closed too.
func (s *Server) decorateProviderRoute(auth providers.AuthClass, handler http.HandlerFunc) http.HandlerFunc {
	if auth != providers.AuthAPIKey {
		return handler
	}
	return s.apiGate()(handler).ServeHTTP
}

// apiKeyBoundRealm returns the concrete provider realm bound to the request's
// API key. An empty result means the key is unbound. An unknown realm (or the
// WorkBuddy-only discovery alias "all") is an operator configuration error and
// is rejected instead of silently falling back to model matching.
func (s *Server) apiKeyBoundRealm(r *http.Request) (string, *apiFailure) {
	if entry := KeyEntryFromContext(r); entry != nil {
		if realm := keys.BoundRealm(entry); realm != "" {
			if realm == "all" || s.providerForRealm(realm) == nil {
				return "", localFailure(http.StatusBadRequest, "invalid api key realm: "+realm)
			}
			return realm, nil
		}
	}
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
					if isMessagesPath(r.URL.Path) {
						writeAnthropicError(tracked, http.StatusInternalServerError, "internal server error", "api_error", time.Time{})
					} else {
						writeV1Error(tracked, http.StatusInternalServerError, "server_error", "internal server error")
					}
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

func isMessagesPath(path string) bool {
	return path == "/messages" || strings.HasPrefix(path, "/messages/") ||
		path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/")
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

func (s *Server) observeProtocol(observation ProtocolObservation) {
	if s == nil || s.opts.ProtocolObserver == nil {
		return
	}
	if observation.Usage != nil {
		copied := make(map[string]any, len(observation.Usage))
		for key, value := range observation.Usage {
			copied[key] = value
		}
		observation.Usage = copied
	}
	s.opts.ProtocolObserver(observation)
}
