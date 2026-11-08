package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// ProviderID and ProviderRealm are the v1 wire values for the Raccoon
	// vertical slice.
	ProviderID    = "raccoon"
	ProviderRealm = "raccoon"

	// LoginPath redeems a browser authorization code for an account.
	LoginPath = "/api/web/auth/v1/login_with_authorization_code"

	// OAuthTTLSeconds, OAUTHMaxSessions, state bytes, and session-id bytes
	// match v1's loopback OAuth table.
	OAuthTTLSeconds  = 300
	OAUTHMaxSessions = 64
	oauthStateBytes  = 32
	oauthIDBytes     = 8

	// healthCacheTTL is v1's five-second /health cache window.
	healthCacheTTL = 5 * time.Second

	// overviewPointsTimeout bounds the best-effort point/task polling done by
	// the overview route. v1 uses the same eight-second budget per call.
	overviewPointsTimeout = 8 * time.Second
	overviewClaimTimeout  = 20 * time.Second
)

// Provider is one Raccoon realm instance. It owns the concrete Raccoon pool,
// configuration, shared transport, token manager, and loopback OAuth session
// table. The generic API/core only sees providers.Provider.
type Provider struct {
	store       *config.Store
	accountsDir string

	pool *Pool

	apiMu        sync.RWMutex
	baseURL      string
	httpClient   *HTTPClient
	tokenManager *TokenManager

	nowMu sync.RWMutex
	now   func() time.Time

	oauthPortMu sync.RWMutex
	oauthPort   int

	healthMu    sync.Mutex
	healthCache healthCacheEntry

	oauthMu       sync.Mutex
	oauthSessions map[string]*oauthSession
}

type healthCacheEntry struct {
	at    time.Time
	key   healthCacheKey
	value providers.Health
}

type healthCacheKey struct {
	dir          string
	baseURL      string
	enabled      bool
	timeout      float64
	stream       float64
	modelsTTL    float64
	max          int
	total        int
	healthy      int
	disabled     int
	cooling      int
	limited      int
	catalog      string
	catalogAt    int64
	catalogCount int
}

type raccoonCatalogStatus struct {
	Source    string
	FetchedAt time.Time
	Accounts  int
	Warning   string
	Models    int
}

var _ providers.Provider = (*Provider)(nil)
var _ providers.HTTPError = (*APIError)(nil)

// New returns the production Raccoon provider. The accounts directory is the
// directory containing the shared settings file, matching v1's layout. The
// shared core transport is wired into both the JSON HTTP client and the token
// manager; the transport itself has no whole-request timeout so chat streams
// are bounded only by the configured stream timeout.
func New(store *config.Store) (*Provider, error) {
	if store == nil || strings.TrimSpace(store.Path) == "" {
		return nil, errors.New("raccoon: config store is required")
	}
	accountsDir := filepath.Dir(store.Path)
	if strings.TrimSpace(accountsDir) == "" {
		return nil, errors.New("raccoon: accounts directory is empty")
	}

	cfg := LoadConfig(store)
	pool, err := NewPool(accountsDir, cfg)
	if err != nil {
		return nil, err
	}

	provider := &Provider{
		store:         store,
		accountsDir:   accountsDir,
		pool:          pool,
		baseURL:       strings.TrimRight(defaultBaseURL(), "/"),
		now:           time.Now,
		oauthSessions: make(map[string]*oauthSession),
	}
	sharedTransport := transport.NewClient(transport.ClientOptions{})
	provider.wireTransport(sharedTransport, cfg)
	if _, err := provider.applySettings(); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *Provider) wireTransport(shared *transport.Client, cfg Config) {
	baseURL := p.baseURL
	client := NewHTTPClient(shared, HTTPClientOptions{
		BaseURL: baseURL,
		Timeout: configTimeout(cfg),
	})
	manager := p.pool.NewTokenManager(shared, TokenManagerOptions{BaseURL: baseURL})

	p.apiMu.Lock()
	p.httpClient = client
	p.tokenManager = manager
	p.apiMu.Unlock()

	p.pool.SetHTTPClient(client)
	p.pool.SetTokenManager(manager)
}

func configTimeout(cfg Config) time.Duration {
	seconds := cfg.TimeoutSeconds
	if seconds <= 0 {
		seconds = defaultTimeoutSeconds
	}
	return time.Duration(seconds * float64(time.Second))
}

// SetHTTPClient injects the shared JSON client. It also forwards the client to
// the pool. Intended for tests and alternative wiring; production New wires one
// automatically.
func (p *Provider) SetHTTPClient(client *HTTPClient) {
	if p == nil {
		return
	}
	p.apiMu.Lock()
	p.httpClient = client
	p.apiMu.Unlock()
	p.pool.SetHTTPClient(client)
}

// SetTokenManager injects the refresh manager used by the pool. Intended for
// tests; production New creates one through Pool.NewTokenManager.
func (p *Provider) SetTokenManager(manager *TokenManager) {
	if p == nil {
		return
	}
	p.apiMu.Lock()
	p.tokenManager = manager
	p.apiMu.Unlock()
	p.pool.SetTokenManager(manager)
}

// SetNow overrides the provider, pool, and token-manager clocks. Intended for
// deterministic tests and always set before concurrent use.
func (p *Provider) SetNow(now func() time.Time) {
	if p == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	p.nowMu.Lock()
	p.now = now
	p.nowMu.Unlock()
	p.pool.now = now
	p.apiMu.RLock()
	manager := p.tokenManager
	p.apiMu.RUnlock()
	if manager != nil {
		manager.now = now
	}
}

// SetListenerPort injects the actual HTTP listener port used by the command
// or server composition. OAuth redirect URLs must never be derived from the
// request Host header: a reverse proxy may send a portless or foreign Host.
func (p *Provider) SetListenerPort(port int) {
	if p == nil {
		return
	}
	p.oauthPortMu.Lock()
	p.oauthPort = port
	p.oauthPortMu.Unlock()
}

func (p *Provider) listenerPort() int {
	if p == nil {
		return 0
	}
	p.oauthPortMu.RLock()
	port := p.oauthPort
	p.oauthPortMu.RUnlock()
	return port
}

// SetCreditLookbackEnabled toggles v1's post-DONE per-request bill lookup.
// Production leaves it enabled; offline fixtures without a bill endpoint
// disable it to keep stream completion immediate.
func (p *Provider) SetCreditLookbackEnabled(enabled bool) {
	if p == nil || p.pool == nil {
		return
	}
	p.pool.SetCreditLookbackEnabled(enabled)
}

// SetBaseURL moves the Raccoon API host. The authorize page host is separate
// and remains the public xiaohuanxiong.com page (or WB_RACCOON_AUTHORIZE_URL).
func (p *Provider) SetBaseURL(baseURL string) {
	if p == nil {
		return
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return
	}
	p.apiMu.Lock()
	defer p.apiMu.Unlock()
	p.baseURL = baseURL
	if p.httpClient != nil {
		p.httpClient.baseURL = baseURL
	}
	if p.tokenManager != nil && p.tokenManager.http != nil {
		p.tokenManager.http.baseURL = baseURL
	}
}

func (p *Provider) currentHTTPClient() *HTTPClient {
	if p == nil {
		return nil
	}
	p.apiMu.RLock()
	defer p.apiMu.RUnlock()
	return p.httpClient
}

func (p *Provider) currentBaseURL() string {
	if p == nil {
		return ""
	}
	p.apiMu.RLock()
	defer p.apiMu.RUnlock()
	return p.baseURL
}

func (p *Provider) nowTime() time.Time {
	if p == nil {
		return time.Now()
	}
	p.nowMu.RLock()
	now := p.now
	p.nowMu.RUnlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

// applySettings reloads the shared settings, projects proxy slots and limits
// onto the pool, and returns the clamped provider config. Missing settings are
// defaults; only a real read error is returned.
func (p *Provider) applySettings() (Config, error) {
	if p == nil || p.store == nil {
		return Config{}, errors.New("raccoon: config store is required")
	}
	settings, err := p.store.Load()
	if err != nil {
		return Config{}, fmt.Errorf("raccoon: load settings: %w", err)
	}
	raw, _ := settings["raccoon"].(map[string]any)
	cfg := normalizeConfig(raw)
	p.pool.SetConfig(cfg)
	p.pool.SetProxySlots(transport.SlotsFromSettings(settings))

	limits, _ := settings["limits"].(map[string]any)
	if limits == nil {
		limits = map[string]any{}
		for _, key := range []string{
			"reserve_credits", "daily_credit_limit",
			"daily_token_limit", "model_daily_token_limit",
		} {
			if value, ok := settings[key]; ok {
				limits[key] = value
			}
		}
	}
	p.pool.ApplyLimits(limits, nil)
	return cfg, nil
}

// ID returns the stable provider implementation identifier.
func (p *Provider) ID() string { return ProviderID }

// Realm returns the unique realm this instance serves.
func (p *Provider) Realm() string { return ProviderRealm }

// Models delegates discovery to the provider-owned catalog. A disabled realm
// is a 503 even when seed models are known; an enabled realm with no accounts
// keeps v1's seed fallback.
func (p *Provider) Models(ctx context.Context, force bool) ([]providers.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, _ := p.applySettings()
	if !cfg.Enabled {
		return nil, &APIError{
			Kind:    KindUnavailable,
			Status:  http.StatusServiceUnavailable,
			Message: disabledChatMessage,
		}
	}
	return p.pool.Models(ctx, force)
}

// OpenChat delegates streaming chat to the provider-owned pool.
func (p *Provider) OpenChat(ctx context.Context, req providers.ChatRequest) (providers.ChatStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	_, _ = p.applySettings()
	return p.pool.OpenChat(ctx, req)
}

// Jobs returns no background jobs in this slice; scheduling is added later.
func (p *Provider) Jobs() []scheduler.Job { return nil }

// Health returns the small v1-compatible health block consumed by /health.
// The five-second cache key includes the config and pool/catalog state, so an
// import, disable, or refresh is reflected without explicit invalidation.
func (p *Provider) Health(ctx context.Context, force bool) (providers.Health, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := p.applySettings()
	if err != nil {
		return nil, err
	}
	stats := p.pool.Stats()
	catalog := p.catalogStatus()
	key := healthCacheKey{
		dir:          p.accountsDir,
		baseURL:      p.currentBaseURL(),
		enabled:      cfg.Enabled,
		timeout:      cfg.TimeoutSeconds,
		stream:       cfg.StreamTimeoutSeconds,
		modelsTTL:    cfg.ModelsTTLSeconds,
		max:          cfg.MaxConcurrent,
		total:        stats.Total,
		healthy:      stats.Healthy,
		disabled:     stats.Disabled,
		cooling:      stats.Cooling,
		limited:      stats.Limited,
		catalog:      catalog.Source,
		catalogAt:    catalog.FetchedAt.UnixNano(),
		catalogCount: catalog.Models,
	}
	now := p.nowTime()

	p.healthMu.Lock()
	if !force && p.healthCache.value != nil &&
		p.healthCache.key == key && now.Sub(p.healthCache.at) < healthCacheTTL {
		value := cloneHealth(p.healthCache.value)
		p.healthMu.Unlock()
		return value, nil
	}
	p.healthMu.Unlock()

	value := providers.Health{
		"ok":             cfg.Enabled,
		"configured":     cfg.Enabled,
		"accounts":       stats.Total,
		"healthy":        stats.Healthy,
		"base_url":       p.currentBaseURL(),
		"catalog_source": catalog.Source,
		"models":         catalog.Models,
	}
	p.healthMu.Lock()
	p.healthCache = healthCacheEntry{at: now, key: key, value: cloneHealth(value)}
	p.healthMu.Unlock()
	return value, nil
}

// Overview returns the v1 admin snapshot. Point and task polling is best
// effort; disabled accounts are not polled. Models and health stay in their
// existing shapes and no credential enters the payload.
func (p *Provider) Overview(ctx context.Context, force bool) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := p.applySettings()
	if err != nil {
		return nil, err
	}

	for _, account := range p.pool.Accounts() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		account.mu.Lock()
		disabled := account.State.Disabled || account.State.ManualDisabled
		account.mu.Unlock()
		if disabled {
			continue
		}

		creditsCtx, cancel := context.WithTimeout(ctx, overviewPointsTimeout)
		_, _ = p.pool.FetchCredits(creditsCtx, account, force)
		cancel()
		tasksCtx, cancel := context.WithTimeout(ctx, overviewPointsTimeout)
		_, _ = p.pool.FetchTasks(tasksCtx, account, force)
		cancel()
		if cfg.AutoClaimLogin {
			claimCtx, cancel := context.WithTimeout(ctx, overviewClaimTimeout)
			_, _ = p.pool.AutoClaim(claimCtx, account)
			cancel()
		}
	}

	models, err := p.pool.Models(ctx, false)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	health, err := p.Health(ctx, force)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"provider":         ProviderID,
		"service":          ProviderID,
		"online":           health["ok"] == true,
		"base_url":         p.currentBaseURL(),
		"auto_claim_login": cfg.AutoClaimLogin,
		"stats":            p.pool.Stats(),
		"accounts":         p.pool.List(),
		"health":           health,
		"models":           overviewModels(models),
	}, nil
}

func overviewModels(models []providers.Model) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, item := range models {
		id := strings.TrimSpace(catalogText(item["id"]))
		if id == "" {
			continue
		}
		name := strings.TrimSpace(catalogText(item["name"]))
		if name == "" {
			name = id
		}
		out = append(out, map[string]any{"id": id, "name": name})
	}
	return out
}

func cloneHealth(value providers.Health) providers.Health {
	if value == nil {
		return nil
	}
	out := make(providers.Health, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

// catalogStatus mirrors v1 models_meta without network I/O: the union of
// account catalogs, falling back to the shared last-good cache, then seeds.
func (p *Provider) catalogStatus() raccoonCatalogStatus {
	accounts := p.pool.Accounts()
	union := p.pool.accountCatalogUnion()
	if len(union) == 0 {
		union = p.pool.cachedCatalog()
	}
	status := raccoonCatalogStatus{Accounts: len(accounts)}
	if len(union) > 0 && len(accounts) > 0 {
		status.Source = "raccoon"
		status.Models = len(union)
		p.pool.catalogMu.Lock()
		status.FetchedAt = p.pool.catalog.fetchedAt
		p.pool.catalogMu.Unlock()
		return status
	}
	status.Source = "seed"
	status.Models = len(seedCatalogModels())
	if len(accounts) == 0 {
		status.Warning = "no Raccoon accounts configured; using built-in seed models"
	} else {
		status.Warning = "upstream catalog unavailable; using built-in seed models"
	}
	return status
}

func catalogPayload(status raccoonCatalogStatus) map[string]any {
	var fetched any
	if !status.FetchedAt.IsZero() {
		fetched = unixFloat(status.FetchedAt)
	}
	return map[string]any{
		"source":     status.Source,
		"fetched_at": fetched,
		"accounts":   status.Accounts,
		"warning":    status.Warning,
	}
}

// invalidateCatalog drops every account's adopted catalog and the shared
// cache, matching v1 invalidate_models.
func (p *Provider) invalidateCatalog() {
	p.pool.catalogFetchMu.Lock()
	defer p.pool.catalogFetchMu.Unlock()
	p.pool.catalogMu.Lock()
	p.pool.catalog = catalogCache{}
	p.pool.catalogMu.Unlock()
	for _, account := range p.pool.Accounts() {
		account.mu.Lock()
		account.Models = nil
		account.mu.Unlock()
	}
}

// refreshModels invalidates the catalog, refetches enabled accounts, and
// reports v1's {ok, models, catalog, message} envelope.
func (p *Provider) refreshModels(ctx context.Context) map[string]any {
	if ctx == nil {
		ctx = context.Background()
	}
	_, _ = p.applySettings()
	p.invalidateCatalog()
	entries, err := p.pool.Models(ctx, false)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return map[string]any{
				"ok": false, "models": 0,
				"catalog": catalogPayload(p.catalogStatus()),
				"message": ctxErr.Error(),
			}
		}
		entries = nil
	}
	status := p.catalogStatus()
	ok := status.Source == "raccoon"
	message := status.Warning
	if ok {
		message = "模型目录已刷新"
	}
	if message == "" {
		message = "上游模型目录不可用"
	}
	return map[string]any{
		"ok":      ok,
		"models":  len(entries),
		"catalog": catalogPayload(status),
		"message": message,
	}
}
