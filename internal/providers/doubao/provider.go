package doubao

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
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
	ProviderID    = "doubao"
	ProviderRealm = "doubao"
)

// Provider is one Doubao Work exit. It owns configuration, the cookie pool,
// the HTTP client, and conversation state.
type Provider struct {
	store       *config.Store
	accountsDir string
	pool        *Pool
	httpClient  *http.Client

	clientMu     sync.Mutex
	proxyClients map[string]*http.Client

	cfgMu sync.RWMutex
	cfg   Config

	convMu        sync.Mutex
	conversations map[string]conversationEntry

	nowMu sync.RWMutex
	now   func() time.Time
}

var _ providers.Provider = (*Provider)(nil)

func New(store *config.Store) (*Provider, error) {
	if store == nil || strings.TrimSpace(store.Path) == "" {
		return nil, errors.New("doubao: config store is required")
	}
	accountsDir := filepath.Dir(store.Path)
	if strings.TrimSpace(accountsDir) == "" {
		return nil, errors.New("doubao: accounts directory is empty")
	}
	cfg := LoadConfig(store)
	pool, err := NewPool(accountsDir, cfg)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		store: store, accountsDir: accountsDir, pool: pool,
		httpClient: defaultHTTPClient(), cfg: cfg,
		proxyClients:  map[string]*http.Client{},
		conversations: map[string]conversationEntry{},
		now:           time.Now,
	}
	_, _ = p.applySettings()
	return p, nil
}

func (p *Provider) ID() string    { return ProviderID }
func (p *Provider) Realm() string { return ProviderRealm }
func (p *Provider) Pool() *Pool   { return p.pool }

func (p *Provider) currentConfig() Config {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.cfg
}

func (p *Provider) applySettings() (Config, error) {
	cfg := LoadConfig(p.store)
	p.cfgMu.Lock()
	p.cfg = cfg
	p.cfgMu.Unlock()
	p.pool.SetConfig(cfg)
	return cfg, nil
}

func (p *Provider) SetHTTPClient(client *http.Client) {
	if client == nil {
		client = defaultHTTPClient()
	}
	p.httpClient = client
}

func newHTTPTransport(proxyURL string) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("doubao: default transport is not an *http.Transport")
	}
	tr := base.Clone()
	// Doubao requests go direct unless the account is bound to a proxy slot.
	tr.Proxy = nil
	if raw := strings.TrimSpace(proxyURL); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(parsed)
	}
	// The reference Node client talks HTTP/1.1. Doubao's SSE endpoint can
	// leave the HTTP/2 response body unflushed, so force HTTP/1.1 here.
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tr.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	tr.DisableCompression = true
	// Doubao's IPv6 edge resets TLS connections from this host. The reference
	// Node client uses IPv4, so pin the dialer to tcp4 for parity.
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", addr)
	}
	return tr, nil
}

func defaultHTTPClient() *http.Client {
	tr, err := newHTTPTransport("")
	if err != nil {
		// Tests and embedders may replace the default transport with a
		// blocking RoundTripper. Keep it intact rather than panicking.
		return &http.Client{Transport: http.DefaultTransport}
	}
	return &http.Client{Transport: tr}
}

func (p *Provider) proxyURL(account *Account) string {
	if p == nil || p.store == nil || account == nil {
		return ""
	}
	account.mu.Lock()
	slot := account.ProxySlot
	account.mu.Unlock()
	if strings.TrimSpace(slot) == "" {
		return ""
	}
	data, err := p.store.Load()
	if err != nil {
		return ""
	}
	return transport.SlotsFromSettings(data).URL(slot)
}

func (p *Provider) clientForProxy(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		if p.httpClient != nil {
			return p.httpClient, nil
		}
		return defaultHTTPClient(), nil
	}
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.proxyClients == nil {
		p.proxyClients = map[string]*http.Client{}
	}
	if client := p.proxyClients[proxyURL]; client != nil {
		return client, nil
	}
	tr, err := newHTTPTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: tr}
	p.proxyClients[proxyURL] = client
	return client, nil
}

func (p *Provider) SetNow(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	p.nowMu.Lock()
	p.now = now
	p.nowMu.Unlock()
	p.pool.mu.Lock()
	p.pool.now = now
	p.pool.mu.Unlock()
}

func (p *Provider) nowTime() time.Time {
	p.nowMu.RLock()
	defer p.nowMu.RUnlock()
	return p.now()
}

func (p *Provider) Models(ctx context.Context, force bool) ([]providers.Model, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	p.applySettings()
	cfg := p.currentConfig()
	if !cfg.Enabled {
		return nil, &APIError{Kind: KindUnavailable, Message: "Doubao 出口未启用：请在设置中打开 doubao.enabled。"}
	}
	return ListModels(), nil
}

func (p *Provider) Health(ctx context.Context, force bool) (providers.Health, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	p.applySettings()
	cfg := p.currentConfig()
	stats := p.pool.Stats()
	return providers.Health{
		"ok": cfg.Enabled, "configured": cfg.Enabled, "enabled": cfg.Enabled,
		"base_url": cfg.BaseURL, "aid": cfg.AID, "mode": "http",
		"accounts": stats.Total, "healthy": stats.Healthy,
		"disabled": stats.Disabled, "cooling": stats.Cooling,
		"models": len(models),
	}, nil
}

func (p *Provider) Overview(ctx context.Context, force bool) (any, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	cfg := p.currentConfig()
	stats := p.pool.Stats()
	accounts := make([]map[string]any, 0, stats.Total)
	for _, account := range p.pool.Accounts() {
		accounts = append(accounts, account.view())
	}
	modelViews := make([]map[string]any, 0, len(models))
	for _, m := range models {
		modelViews = append(modelViews, map[string]any{"id": m.ID, "name": m.Name, "kind": m.Kind})
	}
	return map[string]any{
		"provider": ProviderID, "service": ProviderID, "id": ProviderID,
		"realm": ProviderRealm, "online": cfg.Enabled && stats.Healthy > 0,
		"base_url": cfg.BaseURL, "aid": cfg.AID, "mode": "http",
		"stats": stats, "accounts": accounts, "models": modelViews,
	}, nil
}

func (p *Provider) Routes() []providers.Route { return p.routes() }

func (p *Provider) Jobs() []scheduler.Job {
	if p == nil {
		return nil
	}
	return []scheduler.Job{&checkinJob{provider: p}}
}

func (p *Provider) AccountSummaries(ctx context.Context) ([]providers.AccountSummary, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return p.pool.AccountSummaries(), nil
}

func (p *Provider) RealmAvailable() bool {
	if p == nil {
		return false
	}
	cfg, err := p.applySettings()
	return err == nil && cfg.Enabled
}

func (p *Provider) CatalogProvenance() map[string]any {
	return map[string]any{
		"source": "doubao", "fetched_at": float64(p.nowTime().Unix()),
		"accounts": p.pool.Stats().Total, "warning": "",
	}
}

func (p *Provider) Capabilities() providers.Capabilities { return providers.Capabilities{} }

func (p *Provider) RouteNotFoundMessage() string { return "unknown doubao endpoint" }
