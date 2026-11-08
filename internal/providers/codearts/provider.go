package codearts

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	// ProviderID and ProviderRealm are the v1 wire values.
	ProviderID    = "codearts"
	ProviderRealm = "codearts"

	// healthCacheTTL is v1's five-second /health cache window.
	healthCacheTTL = 5 * time.Second

	// overviewInfoTimeout bounds each best-effort statistics refresh performed
	// by the overview route. v1 uses the same eight-second budget.
	overviewInfoTimeout = 8 * time.Second

	// disabledAdminMessage is the exact v1 explanation returned by the admin
	// overview when the realm is disabled.
	disabledAdminMessage = "CodeArts 出口未启用：请到设置页填写 Base URL 和 API Key。"
)

// Provider is one CodeArts realm instance. It owns the concrete pool,
// provider configuration, shared core transport, token manager, and OAuth
// session table. The generic API only sees providers.Provider.
type Provider struct {
	store       *config.Store
	accountsDir string

	mu           sync.RWMutex
	cfg          Config
	pool         *Pool
	transport    *transport.Client
	tokenManager *TokenManager

	// oauthSessions is provider-owned so tests and alternate compositions can
	// inject an isolated table. New always creates one.
	oauthSessions *OAuthSessions
	oauthPortMu   sync.RWMutex
	oauthPort     int

	healthMu    sync.Mutex
	healthCache healthCacheEntry

	// catalogHTTPTimeout bounds each upstream model-discovery call. Tests may
	// lower it; zero means the package default.
	catalogHTTPTimeout time.Duration

	// modelFetchJoinHook is a test seam invoked when a discovery call joins an
	// already-running per-account flight. It is nil in production.
	modelFetchJoinHook func()

	// chatRetryDelay is the pause before retrying a CodeArts concurrency
	// rejection on the same account. Zero means the v1 two-second delay; tests
	// may shorten it.
	chatRetryDelay time.Duration
}

type healthCacheEntry struct {
	at    time.Time
	value providers.Health
}

var _ providers.Provider = (*Provider)(nil)
var _ providers.HTTPError = (*APIError)(nil)

// New returns the production CodeArts provider. The accounts directory is the
// directory containing the shared settings file, matching v1's layout.
//
// Task 9 composition must create exactly one Provider/Pool per accounts
// directory in a process: Pool locks are instance-local, so two Pools over the
// same state.json would still race with each other.
func New(store *config.Store) (*Provider, error) {
	if store == nil || strings.TrimSpace(store.Path) == "" {
		return nil, errors.New("codearts: config store is required")
	}
	accountsDir := filepath.Dir(store.Path)
	if strings.TrimSpace(accountsDir) == "" {
		return nil, errors.New("codearts: accounts directory is empty")
	}

	cfg := LoadConfig(store)
	pool, err := NewPool(accountsDir, cfg)
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(cfg.TimeoutSeconds * float64(time.Second))
	if timeout <= 0 {
		timeout = defaultTokenRefreshTimeout
	}
	client := transport.NewClient(transport.ClientOptions{})
	manager := NewTokenManager(client, TokenManagerOptions{
		Pool:     pool,
		Timeout:  timeout,
		Now:      func() time.Time { return pool.nowTime() },
		ProxyURL: func(account *Account) string { return account.ProxyURL() },
	})

	provider := &Provider{
		store:              store,
		accountsDir:        accountsDir,
		cfg:                cfg,
		pool:               pool,
		transport:          client,
		tokenManager:       manager,
		oauthSessions:      NewOAuthSessions(),
		catalogHTTPTimeout: catalogHTTPTimeout,
	}
	if _, err := provider.applySettings(); err != nil {
		return nil, err
	}
	return provider, nil
}

// NewPool returns the provider-owned account pool. It exists for route and
// scheduler wiring in later tasks; accounts are loaded once by New.
func (p *Provider) NewPool() *Pool {
	if p == nil {
		return nil
	}
	return p.pool
}

// Pool returns the provider-owned account pool.
func (p *Provider) Pool() *Pool {
	return p.NewPool()
}

// TokenManager returns the provider-owned token refresh manager. It is built
// during New and performs no network I/O until Ensure or Keepalive runs.
func (p *Provider) TokenManager() *TokenManager {
	if p == nil {
		return nil
	}
	return p.tokenManager
}

// Config returns the current CodeArts configuration snapshot.
func (p *Provider) Config() Config {
	if p == nil {
		return Config{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// SetListenerPort injects the actual HTTP listener port used by OAuth redirect
// URLs. The URL must never be derived from the request Host header because a
// reverse proxy may send a portless or foreign Host.
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

// applySettings reloads the shared settings, projects config/proxy slots onto
// the pool, and returns the clamped provider config. Missing settings are
// defaults; only a real read error is returned. Route handlers call this before
// acting so a settings save is visible without a restart.
func (p *Provider) applySettings() (Config, error) {
	if p == nil || p.store == nil {
		return Config{}, errors.New("codearts: config store is required")
	}
	settings, err := p.store.Load()
	if err != nil {
		return Config{}, fmt.Errorf("codearts: load settings: %w", err)
	}
	raw, _ := settings["codearts"].(map[string]any)
	cfg := normalizeConfig(raw)
	p.mu.Lock()
	previousEnabled := p.cfg.Enabled
	p.cfg = cfg
	p.mu.Unlock()
	if previousEnabled != cfg.Enabled {
		// A settings save toggled the realm. The 5s health cache would
		// otherwise report the opposite online state to the overview.
		p.invalidateHealth()
	}
	if p.pool != nil {
		p.pool.SetConfig(cfg)
		p.pool.ApplyProxySlots(transport.SlotsFromSettings(settings))
	}
	return cfg, nil
}

func (p *Provider) nowTime() time.Time {
	if p == nil || p.pool == nil {
		return time.Now()
	}
	return p.pool.nowTime()
}

func (p *Provider) invalidateHealth() {
	if p == nil {
		return
	}
	p.healthMu.Lock()
	p.healthCache = healthCacheEntry{}
	p.healthMu.Unlock()
}

// ID returns the stable provider implementation identifier.
func (p *Provider) ID() string { return ProviderID }

// Realm returns the unique realm this instance serves.
func (p *Provider) Realm() string { return ProviderRealm }

// Jobs returns no background jobs in this slice; scheduler wiring is added
// later without changing the provider contract.
func (p *Provider) Jobs() []scheduler.Job { return nil }

// Health returns the small v1-compatible health block consumed by /health.
// Values are cached for five seconds; force bypasses the cache. Account
// mutations performed through the admin routes invalidate the cache so a panel
// refresh sees the new count immediately.
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

	now := p.nowTime()
	p.healthMu.Lock()
	if !force && !p.healthCache.at.IsZero() &&
		now.Sub(p.healthCache.at) < healthCacheTTL {
		value := cloneHealth(p.healthCache.value)
		p.healthMu.Unlock()
		return value, nil
	}
	p.healthMu.Unlock()

	stats := PoolStats{}
	if p.pool != nil {
		stats = p.pool.Stats()
	}
	value := providers.Health{
		"ok":         cfg.Enabled,
		"configured": cfg.Enabled,
		"accounts":   stats.Total,
		"healthy":    stats.Healthy,
		"base_url":   snapHost(),
	}
	p.healthMu.Lock()
	p.healthCache = healthCacheEntry{at: now, value: cloneHealth(value)}
	p.healthMu.Unlock()
	return value, nil
}

// Overview returns the v1 admin snapshot. Statistics refreshes are best effort
// for every account, including disabled ones (v1 aggregates their package/credit
// snapshots), models are the account-info/account-catalog union with the v1 seed
// fallback, and no credential enters the payload.
func (p *Provider) Overview(ctx context.Context, force bool) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := p.applySettings()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, &adminError{
			Status:  http.StatusServiceUnavailable,
			Type:    "invalid_request_error",
			Message: disabledAdminMessage,
		}
	}

	for _, account := range p.pool.Accounts() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		infoCtx, cancel := context.WithTimeout(ctx, overviewInfoTimeout)
		_, _ = p.FetchInfo(infoCtx, account, force)
		cancel()
	}

	health, err := p.Health(ctx, force)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"service":  ProviderID,
		"region":   "cn",
		"online":   health["ok"] == true,
		"base_url": snapHost(),
		"stats":    p.statsPayload(),
		"accounts": p.pool.List(),
		"models":   p.overviewModels(),
		"health":   health,
		"schedule": map[string]any{
			"watch": map[string]any{"enabled": false},
		},
	}, nil
}

// statsPayload extends the v1 scheduler counters with the aggregated
// statistics/plugin credit totals. limited is a v2 extension that counts
// accounts at their per-account concurrency cap; it overlaps healthy so the v1
// counter semantics stay unchanged.
func (p *Provider) statsPayload() map[string]any {
	stats := PoolStats{}
	remain := float64(0)
	total := float64(0)
	used := float64(0)
	limited := 0
	if p.pool != nil {
		stats = p.pool.Stats()
		now := p.nowTime()
		for _, account := range p.pool.Accounts() {
			account.mu.Lock()
			disabled := account.removed || account.State.Disabled || account.State.ManualDisabled
			cooling := !disabled && account.State.CoolUntil > unixSeconds(now)
			atCap := !disabled && !cooling && account.maxConcurrent > 0 &&
				account.activeConcurrent >= account.maxConcurrent
			var bucket *CreditTotal
			if account.Info != nil && account.Info.Credits.Total != nil {
				copied := *account.Info.Credits.Total
				bucket = &copied
			}
			account.mu.Unlock()
			if atCap {
				limited++
			}
			if bucket != nil {
				remain += bucket.Remain
				total += bucket.Amount
				used += bucket.Used
			}
		}
	}
	return map[string]any{
		"total":         stats.Total,
		"healthy":       stats.Healthy,
		"disabled":      stats.Disabled,
		"cooling":       stats.Cooling,
		"limited":       limited,
		"credits":       roundCredits(remain),
		"credits_total": roundCredits(total),
		"credits_used":  roundCredits(used),
	}
}

func roundCredits(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Round(value*100) / 100
}

// overviewModels returns v1's info-first model union. An account with a
// non-empty statistics model list contributes id/name/description rows; a
// catalog-only account contributes its id. Duplicate ids are case-insensitive.
// The cold fallback is the local seed list, exactly like v1.
func (p *Provider) overviewModels() []map[string]any {
	out := make([]map[string]any, 0)
	seen := make(map[string]bool)
	add := func(id, name, description string) {
		id = strings.TrimSpace(id)
		key := strings.ToLower(id)
		if id == "" || seen[key] {
			return
		}
		seen[key] = true
		entry := map[string]any{"id": id}
		if name != "" || description != "" {
			if name == "" {
				name = id
			}
			entry["name"] = name
			entry["description"] = description
		}
		out = append(out, entry)
	}

	if p.pool != nil {
		for _, account := range p.pool.Accounts() {
			account.mu.Lock()
			removed := account.removed
			info := account.Info
			var infoModels []ModelInfo
			if info != nil {
				infoModels = append([]ModelInfo(nil), info.Models...)
			}
			catalogModels := cloneModels(account.Models)
			account.mu.Unlock()
			if removed {
				continue
			}
			if len(infoModels) > 0 {
				for _, item := range infoModels {
					add(item.ID, item.Name, item.Description)
				}
				continue
			}
			for _, item := range catalogModels {
				add(modelText(item["id"]), "", "")
			}
		}
	}
	if len(out) == 0 {
		for _, id := range seedModels {
			add(id, "", "")
		}
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
