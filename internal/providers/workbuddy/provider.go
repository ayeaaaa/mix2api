package workbuddy

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

const (
	// ProviderID prefixes the stable provider instance identifiers.
	ProviderID = "workbuddy"

	// ProviderIDIntl and ProviderIDCN are the per-realm IDs returned by ID.
	ProviderIDIntl = "workbuddy-intl"
	ProviderIDCN   = "workbuddy-cn"

	// ProviderRealmIntl and ProviderRealmCN are the unique realm keys.
	ProviderRealmIntl = realmIntl
	ProviderRealmCN   = realmCN
)

// Provider is one WorkBuddy realm instance. The two instances share the same
// implementation but own separate configuration; account pools are added by a
// later task and will be filtered to the instance realm.
type Provider struct {
	realm       string
	store       *config.Store
	accountsDir string

	mu  sync.RWMutex
	cfg Config

	// transport is the shared core HTTP client wired in by the auth/chat
	// slices. New creates it but performs no network I/O.
	transport *transport.Client

	// oauthMu guards this provider's browser-login session table. Sessions
	// store the upstream state, realm, platform, and selected proxy slot; they
	// never store tokens beyond what the completed Auth is about to save.
	oauthMu       sync.Mutex
	oauthSessions map[string]*oauthSession
	oauthReserved map[string]bool
	oauthNow      func() time.Time
	oauthClient   oauthDoer
	oauthBaseURL  string

	// OAuth request budgets. Zero uses the v1-compatible defaults (30s start
	// and token polls, 15s profile lookup); tests can shorten them.
	oauthStartTimeout   time.Duration
	oauthTokenTimeout   time.Duration
	oauthProfileTimeout time.Duration

	// poolOnce/pool own this realm's account pool. New initializes it eagerly
	// so an unreadable accounts directory is reported at construction;
	// NewPool is idempotent for later callers.
	poolOnce sync.Once
	pool     *Pool
	poolErr  error
}

// New returns the WorkBuddy provider for realm. The shared settings file is
// supplied by the caller, matching the Raccoon/CodeArts composition pattern;
// the accounts directory is the directory containing that settings file.
func New(realm string, store *config.Store) (*Provider, error) {
	normalized, ok := normalizeRealm(realm)
	if !ok {
		return nil, fmt.Errorf("workbuddy: invalid realm %q", realm)
	}
	if store == nil || strings.TrimSpace(store.Path) == "" {
		return nil, errors.New("workbuddy: config store is required")
	}
	accountsDir := filepath.Dir(store.Path)
	if strings.TrimSpace(accountsDir) == "" {
		return nil, errors.New("workbuddy: accounts directory is empty")
	}

	provider := &Provider{
		realm:       normalized,
		store:       store,
		accountsDir: accountsDir,
		cfg:         LoadConfig(store, normalized),
		transport:   transport.NewClient(transport.ClientOptions{RefuseRedirects: true}),
	}
	provider.NewPool()
	if provider.poolErr != nil {
		return nil, provider.poolErr
	}
	return provider, nil
}

// ID returns the stable provider instance identifier.
func (p *Provider) ID() string {
	if p == nil {
		return ProviderID
	}
	if p.realm == realmCN {
		return ProviderIDCN
	}
	return ProviderIDIntl
}

// Realm returns the unique realm this instance serves.
func (p *Provider) Realm() string {
	if p == nil {
		return ""
	}
	return p.realm
}

// Config returns the current WorkBuddy configuration snapshot.
func (p *Provider) Config() Config {
	if p == nil {
		return Config{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	cfg := p.cfg
	cfg.AutoSwitchProducts = append([]string(nil), cfg.AutoSwitchProducts...)
	return cfg
}

// AccountsDir returns the provider-owned accounts directory (the directory of
// the shared settings file).
func (p *Provider) AccountsDir() string {
	if p == nil {
		return ""
	}
	return p.accountsDir
}

// EndpointFor returns this realm's chat base URL for product.
func (p *Provider) EndpointFor(product string) string {
	if p == nil {
		return ""
	}
	return endpointFor(p.realm, product)
}

// DomainFor returns this realm's X-Domain fallback for product.
func (p *Provider) DomainFor(product string) string {
	if p == nil {
		return ""
	}
	return domainFor(p.realm, product)
}

// DesktopUA returns the desktop User-Agent for this realm, including any
// settings-provided version overrides.
func (p *Provider) DesktopUA() string {
	if p == nil {
		return ""
	}
	return desktopUAWithVersions(p.realm, p.Config().IdentityVersions)
}

// DesktopVersion returns the desktop X-IDE-Version for this realm, including
// any settings-provided version override.
func (p *Provider) DesktopVersion() string {
	if p == nil {
		return ""
	}
	return desktopVersionWithVersions(p.realm, p.Config().IdentityVersions)
}

// IdentityHeaders builds this realm's identity headers, applying the
// provider's stored version overrides and private-data preference.
func (p *Provider) IdentityHeaders(options IdentityOptions) http.Header {
	if p == nil {
		return http.Header{}
	}
	cfg := p.Config()
	options.Realm = p.realm
	options.Versions = cfg.IdentityVersions
	if strings.TrimSpace(options.PrivateData) == "" {
		options.PrivateData = cfg.PrivateData
	}
	return buildIdentityHeaders(options)
}
