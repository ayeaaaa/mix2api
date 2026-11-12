// Package api exposes the generic mix2api HTTP surface.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/agents"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/logs"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/media"
	panelplatform "github.com/ayeaaaa/mix2api-v2/internal/platform/panel"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/pricing"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/probes"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/proxy"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/webtools"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
	"github.com/ayeaaaa/mix2api-v2/internal/web"
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

	// Pricing mounts the panel pricing status, refresh and runtime mapping.
	Pricing               *pricing.Snapshot
	PricingRefresher      *pricing.Refresher
	PricingRefreshMinutes func() float64
	PricingVariantInherit func() bool

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

	// StaticAPIKey is the legacy launcher key, when configured. It is used only
	// by the agent configurator to hand a local client a usable key; it is never
	// written to logs or API responses.
	StaticAPIKey string

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

	// Platform is the composed platform service set. New merges it into the
	// individual options above; either form is valid for embedders/tests.
	Platform *PlatformServices

	// RecordUsage appends exactly one usage row for a finished Chat, Responses
	// or Messages request and then publishes the daily guards. A failure is
	// logged and never changes the client response.
	RecordUsage func(context.Context, usage.RecordInput) error

	// Shutdown is the panel's "close gateway" action. The command injects a
	// graceful http.Server shutdown. Nil keeps the route mounted but returns a
	// service-unavailable error instead of pretending the process stopped.
	Shutdown func()
}

// ProtocolObservation describes one completed Chat, Responses or Messages
// request. Usage is the raw OpenAI usage block when the upstream supplied one.
// Realm/Account/timing fields are filled from the provider stream's optional
// UsageSnapshotReporter; a provider without the hook leaves them empty.
type ProtocolObservation struct {
	Protocol       string
	Model          string
	RequestedModel string
	Realm          string
	Account        string
	Kind           string
	Stream         bool
	Outcome        string
	Status         int
	Usage          map[string]any
	ElapsedMS      int64
	TTFTMS         int64
	GenMS          int64
	Effort         string
	Error          bool
	Message        string
	Free           bool
	CostPolicy     string
}

// healthResponse is the wire shape of GET /health. The original {ok,version}
// fields stay stable; v1's pool/key/media blocks are restored beside the v2
// additive providers map.
type healthResponse struct {
	OK                        bool           `json:"ok"`
	Version                   string         `json:"version"`
	Realm                     string         `json:"realm"`
	CryptoOK                  bool           `json:"crypto_ok"`
	Accounts                  int            `json:"accounts"`
	AccountsReady             int            `json:"accounts_ready"`
	AccountsByRealm           map[string]int `json:"accounts_by_realm"`
	AccountsReadyByRealm      map[string]int `json:"accounts_ready_by_realm"`
	AccountsReadyCurrentRealm int            `json:"accounts_ready_current_realm"`
	ModelsReady               map[string]any `json:"models_ready"`
	APIKeyRequired            bool           `json:"api_key_required"`
	CodeArts                  map[string]any `json:"codearts"`
	Raccoon                   map[string]any `json:"raccoon"`
	Media                     map[string]any `json:"media"`
	UID                       any            `json:"uid"`
	Domain                    any            `json:"domain"`
	Issuer                    any            `json:"issuer"`
	CredentialFile            any            `json:"credential_file"`
	ExpiresAt                 any            `json:"expires_at"`
	KeyAccountsReady          int            `json:"key_accounts_ready"`
	KeyAccountsBound          int            `json:"key_accounts_bound"`
	Providers                 map[string]any `json:"providers,omitempty"`
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
	s.mergePlatform(opts.Platform)
	s.keyStore = firstKeyStore(s.keyStore, s.opts.KeyStore, s.opts.Keys)
	if s.settings == nil {
		s.settings = s.opts.Settings
	}
	if s.settings == nil {
		s.settings = s.opts.SettingsService
	}
	if s.keyStore == nil && s.settings != nil {
		s.keyStore = s.settings.Keys
	}
	if s.proxySlots == nil {
		s.proxySlots = s.opts.ProxySlots
	}
	if s.proxySlots == nil {
		s.proxySlots = s.opts.Proxy
	}
	if s.realm == nil {
		s.realm = s.opts.Realm
	}
	s.keyLimiter = s.opts.KeyLimiter
	if s.keyLimiter == nil && s.keyStore != nil {
		s.keyLimiter = keys.NewLimiter()
	}
	if s.sessions == nil {
		s.sessions = s.opts.PanelSessions
	}
	if s.password == nil {
		s.password = firstPassword(s.opts.PanelPassword, s.opts.PanelPasswords)
	}
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
	if s.opts.DefaultMaxOutputTokens != nil {
		s.defaultTokens = cachedModelTokens(s.opts.DefaultMaxOutputTokens)
	}
	if s.opts.ListenerPort > 0 {
		for _, provider := range s.providers {
			if setter, ok := provider.(listenerPortSetter); ok {
				setter.SetListenerPort(s.opts.ListenerPort)
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
	s.mountDashboard()
	for _, path := range []string{"/chat/completions", "/v1/chat/completions", "/completions", "/v1/completions"} {
		s.mux.Handle(path, s.protectAPI(http.HandlerFunc(s.handleChat)))
	}
	for _, path := range []string{"/models", "/v1/models"} {
		s.mux.Handle(path, s.protectAPI(http.HandlerFunc(s.handleModels)))
	}
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
	NewDashboardHandlers(s.panelGate(), s.opts.Shutdown).Register(s.mux)
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
	if s.opts.Pricing != nil {
		pricingHandlers := NewPricingHandlers(s.opts.Pricing, s.opts.PricingRefresher, s.panelGate())
		pricingHandlers.RefreshMinutes = s.opts.PricingRefreshMinutes
		pricingHandlers.VariantInherit = s.opts.PricingVariantInherit
		pricingHandlers.Register(s.mux)
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
		agentsHandlers := NewAgentHandlers(s.opts.Agents, s.panelGate())
		agentsHandlers.Server = s
		agentsHandlers.Register(s.mux)
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
				switch value := data["keep_backups"].(type) {
				case float64:
					return int(value)
				case int:
					return value
				case json.Number:
					number, _ := value.Int64()
					return int(number)
				case string:
					number, _ := strconv.Atoi(strings.TrimSpace(value))
					return number
				default:
					return 1
				}
			}
		}
		updates.Register(s.mux)
	}
}

// mountDashboard serves the embedded v1 dashboard at its exact v1 paths. It is
// intentionally not a catch-all: the root pattern uses Go 1.22's {$} so an
// unknown path still reaches the normal 404/provider route table.
func (s *Server) mountDashboard() {
	if s == nil || s.mux == nil {
		return
	}
	handler := web.Handler()
	for _, pattern := range []string{
		"/{$}", "/dashboard", "/dashboard.html", "/ui", "/classic", "/classic/{$}", "/favicon.ico",
	} {
		s.mux.Handle(pattern, handler)
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
	handler := recoveryHandler(platformHeaders(panel, s.opts.Version))
	if store := s.runtimeLogStore(); store != nil {
		handler = logRequests(handler, store)
	}
	return handler
}

func (s *Server) runtimeLogStore() *logs.LogStore {
	if s == nil {
		return nil
	}
	if s.opts.LogStore != nil {
		return s.opts.LogStore
	}
	return s.opts.Logs
}

// logRequests appends one v1-shaped request line after the response completes.
// The durable/memory store still owns filtering, redaction, and retention.
func logRequests(next http.Handler, store *logs.LogStore) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	if store == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked := &statusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(tracked, r)
		status := tracked.status
		if status == 0 {
			status = http.StatusOK
		}
		line := r.Method + " " + r.URL.RequestURI() + " " + r.Proto
		_ = store.Append(logs.Entry{Msg: fmt.Sprintf("%q %d -", line, status)})
	})
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

const serverProduct = "mix2api"

// platformHeaders adds the v2 Server identity and v1-compatible CORS headers.
// Management prefixes are deliberately excluded: the dashboard is same-origin
// and a stray page must not be able to read account/usage replies.
func platformHeaders(next http.Handler, version string) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	header := serverProduct
	if strings.TrimSpace(version) != "" {
		header += "/" + strings.TrimSpace(version)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", header)
		allowed := corsOriginAllowed(r.URL.Path)
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions && allowed {
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var corsManagementPrefixes = []string{
	"/v1/usage", "/usage", "/accounts", "/settings", "/tasks", "/scheduler",
	"/panel", "/logs", "/codearts", "/raccoon", "/agents", "/updates",
	"/pricing", "/activity", "/console",
}

var corsAPIPrefixes = []string{"/v1", "/chat", "/completions", "/models", "/responses", "/messages"}

func corsOriginAllowed(path string) bool {
	if path == "" {
		return false
	}
	for _, prefix := range corsManagementPrefixes {
		if panelPathMatches(path, prefix) {
			return false
		}
	}
	for _, prefix := range corsAPIPrefixes {
		if panelPathMatches(path, prefix) {
			return true
		}
	}
	return false
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
// API key. An empty result means the key is unbound. An unknown realm, the
// WorkBuddy-only discovery alias "all", or the gateway scope "gateway" is an
// operator configuration error and is rejected instead of silently falling
// back to model matching.
func (s *Server) apiKeyBoundRealm(r *http.Request) (string, *apiFailure) {
	if entry := KeyEntryFromContext(r); entry != nil {
		if realm := keys.BoundRealm(entry); realm != "" {
			if reservedModelScope(realm) || s.providerForRealm(realm) == nil {
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
	if reservedModelScope(realm) || s.providerForRealm(realm) == nil {
		return "", localFailure(http.StatusBadRequest, "invalid api key realm: "+realm)
	}
	return realm, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	realm := s.activeRealm(r.Context())
	response := healthResponse{
		OK: true, Version: s.opts.Version, Realm: realm,
		CryptoOK:             s.cryptoAvailable(),
		AccountsByRealm:      map[string]int{},
		AccountsReadyByRealm: map[string]int{},
		ModelsReady:          map[string]any{},
		APIKeyRequired:       s.authRequired(),
		CodeArts:             map[string]any{"configured": false, "ok": false},
		Raccoon:              map[string]any{"configured": false, "ok": false},
		Media:                s.mediaHealth(),
		UID:                  nil, Domain: nil, Issuer: nil,
		CredentialFile: nil, ExpiresAt: nil,
	}
	if len(s.providers) > 0 {
		response.Providers = make(map[string]any, len(s.providers))
		force := queryBool(r, "force")
		for _, provider := range s.providers {
			realmName := providerRealm(provider)
			health, err := provider.Health(r.Context(), force)
			if err != nil {
				// A provider failure must not turn /health red or leak the
				// provider's internal error (which may contain local paths).
				response.Providers[realmName] = map[string]any{"ok": false}
				continue
			}
			if health == nil {
				health = providers.Health{"ok": true}
			}
			response.Providers[realmName] = health
			switch realmName {
			case "codearts":
				response.CodeArts = v1CodeArtsHealth(health)
			case "raccoon":
				response.Raccoon = copyHealth(health)
			case "intl", "cn":
				// v1's pool readiness only covers the WorkBuddy pool.
				accounts := healthInt(health["accounts"])
				ready := healthInt(health["healthy"])
				response.Accounts += accounts
				response.AccountsReady += ready
				if accounts > 0 {
					response.AccountsByRealm[realmName] = accounts
					response.AccountsReadyByRealm[realmName] = ready
				}
				if realmName == realm {
					response.AccountsReadyCurrentRealm = ready
				}
			}
		}
	}
	if entry, ok := s.healthKeyAccess(r); ok {
		summaries := s.accountSummaries(r.Context())
		if len(summaries) > 0 {
			representative := summaries[0]
			response.UID = representative.UID
			response.Domain = representative.Domain
			response.Issuer = representative.Issuer
			response.CredentialFile = representative.CredentialFile
			response.ExpiresAt = representative.ExpiresAt
		}
		response.KeyAccountsReady, response.KeyAccountsBound = keyAccountCounts(entry, summaries, realm)
	}
	writeJSONResponse(w, http.StatusOK, response)
}

// healthKeyAccess mirrors v1's /health identity gate: a panel session, a
// matching API key, or a deployment with auth_disabled may see the account
// identity block. A key-authenticated request also supplies the key-readiness
// counts; panel/no-key access leaves them at zero, as v1 did.
func (s *Server) healthKeyAccess(r *http.Request) (*keys.Entry, bool) {
	if s == nil {
		return nil, false
	}
	if s.sessions != nil && s.sessions.Valid(panelSessionToken(r)) {
		return nil, true
	}
	if s.keyStore != nil {
		entries, err := s.keyStore.List(false)
		if err == nil {
			if entry, ok := keys.Match(entries, apiKeyFromRequest(r)); ok {
				return entry, true
			}
			if !hasEnabledKeys(entries) && s.staticAPIKeyPasses(r) {
				return &keys.Entry{ID: "launcher", Name: "启动参数", Key: apiKeyFromRequest(r), Enabled: true}, true
			}
		}
	} else if s.staticAPIKeyPasses(r) {
		return &keys.Entry{ID: "launcher", Name: "启动参数", Enabled: true}, true
	}
	if !s.authRequired() {
		return nil, true
	}
	return nil, false
}

// accountSummaries collects every provider that exposes its live pool. The
// rows are deduplicated by UID and sorted so the representative account is
// deterministic; v1's pool order was effectively the sorted account files.
func (s *Server) accountSummaries(ctx context.Context) []providers.AccountSummary {
	if s == nil {
		return nil
	}
	byUID := make(map[string]providers.AccountSummary)
	for _, provider := range s.providers {
		inventory, ok := provider.(providers.AccountInventory)
		if !ok {
			continue
		}
		rows, err := inventory.AccountSummaries(ctx)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if strings.TrimSpace(row.UID) == "" {
				continue
			}
			byUID[row.UID] = row
		}
	}
	out := make([]providers.AccountSummary, 0, len(byUID))
	for _, row := range byUID {
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// keyAccountCounts is v1 key_readiness: bound is the explicit allow-list size
// (or every account in the key's realm), and ready counts only accounts that
// can serve right now inside that realm/allow-list.
func keyAccountCounts(entry *keys.Entry, summaries []providers.AccountSummary, activeRealm string) (ready, bound int) {
	if entry == nil {
		return 0, 0
	}
	realm := keys.BoundRealm(entry)
	if realm == "" {
		realm = activeRealm
	}
	allowed := make(map[string]bool, len(entry.Accounts))
	for _, uid := range entry.Accounts {
		if uid = strings.TrimSpace(uid); uid != "" {
			allowed[uid] = true
		}
	}
	if len(allowed) > 0 {
		bound = len(allowed)
	}
	for _, account := range summaries {
		if account.Realm != realm {
			continue
		}
		if len(allowed) == 0 {
			bound++
		}
		if !account.Ready {
			continue
		}
		if len(allowed) > 0 && !allowed[account.UID] {
			continue
		}
		ready++
	}
	return ready, bound
}

// v1CodeArtsHealth keeps v1's small unconfigured block. A configured provider
// keeps the full health payload; the v2 providers map remains additive.
func v1CodeArtsHealth(value providers.Health) map[string]any {
	out := copyHealth(value)
	if out["configured"] == true {
		return out
	}
	for key := range out {
		switch key {
		case "configured", "base_url", "error":
		default:
			delete(out, key)
		}
	}
	return out
}

func copyHealth(value providers.Health) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

func healthInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		number, _ := typed.Int64()
		return int(number)
	default:
		return 0
	}
}

func (s *Server) activeRealm(ctx context.Context) string {
	if s != nil && s.realm != nil {
		if realm := strings.TrimSpace(s.realm.Active(ctx)); realm != "" {
			return realm
		}
	}
	return "intl"
}

func (s *Server) globalKeySet() bool {
	return s != nil && strings.TrimSpace(s.opts.StaticAPIKey) != ""
}

func (s *Server) authRequired() bool {
	if s == nil {
		return false
	}
	if s.keyStore != nil {
		return s.keyStore.AuthRequired()
	}
	return s.globalKeySet()
}

func (s *Server) cryptoAvailable() bool {
	if s == nil || s.password == nil {
		return true
	}
	ok, _ := s.password.CryptoAvailable()
	return ok
}

func (s *Server) mediaHealth() map[string]any {
	cfg := media.DefaultConfig()
	slots := 4
	inflight := 0
	if s != nil && s.opts.Media != nil {
		cfg = s.opts.Media.Config()
		slots = s.opts.Media.Slots()
		inflight = s.opts.Media.Inflight()
	}
	return map[string]any{
		"enabled": cfg.Enabled, "realm": cfg.Realm, "identity": cfg.Identity,
		"slots": slots, "inflight": inflight,
		"video_concurrency": cfg.VideoConcurrency,
	}
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
		// "all" and "gateway" are discovery/routing scopes, never providers;
		// accepting them here would let providerForRealm claim a reserved name.
		if realm == "" || reservedModelScope(realm) || seen[realm] {
			continue
		}
		seen[realm] = true
		out = append(out, provider)
	}
	return out
}

// reservedModelScope reports the names that select a model-discovery scope
// rather than a concrete provider: "all" is WorkBuddy-only and "gateway" spans
// every exit. Both are reserved so providerForRealm can never return a provider
// for them.
func reservedModelScope(realm string) bool {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case "all", "gateway":
		return true
	default:
		return false
	}
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

func (s *Server) observeProtocol(r *http.Request, observation ProtocolObservation) {
	if s == nil {
		return
	}
	if observation.Usage != nil {
		copied := make(map[string]any, len(observation.Usage))
		for key, value := range observation.Usage {
			copied[key] = value
		}
		observation.Usage = copied
	}
	if s.opts.RecordUsage != nil {
		ctx := context.Background()
		if r != nil {
			ctx = r.Context()
		}
		in := usage.RecordInput{
			At:             time.Now(),
			Model:          observation.Model,
			RequestedModel: observation.RequestedModel,
			Realm:          observation.Realm,
			Account:        observation.Account,
			KeyID:          keyIDFromRequest(r),
			Kind:           observation.Kind,
			Outcome:        observation.Outcome,
			Stream:         observation.Stream,
			Usage:          observation.Usage,
			ElapsedMS:      observation.ElapsedMS,
			TTFTMS:         observation.TTFTMS,
			GenMS:          observation.GenMS,
			Effort:         observation.Effort,
			Error:          observation.Error,
			Status:         observation.Status,
			Message:        observation.Message,
			Free:           observation.Free,
			CostPolicy:     observation.CostPolicy,
		}
		if in.Kind == "" {
			in.Kind = "chat"
		}
		if err := s.opts.RecordUsage(ctx, in); err != nil {
			log.Printf("api: usage record failed: %v", err)
		}
	}
	if s.opts.ProtocolObserver != nil {
		s.opts.ProtocolObserver(observation)
	}
}
