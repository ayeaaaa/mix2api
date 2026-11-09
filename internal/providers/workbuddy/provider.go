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
// implementation but own separate configuration and account pools filtered to
// the instance realm.
type Provider struct {
	realm       string
	store       *config.Store
	accountsDir string

	mu  sync.RWMutex
	cfg Config

	// peer is the sibling WorkBuddy realm provider used only for the opt-in
	// intl→cn fallback. SetPeer wires it from app composition (Task 11).
	peer *Provider

	// chatBaseURL and chatDoer are same-package test seams for the chat path.
	// Their zero values use the account product endpoint and the provider
	// transport respectively.
	chatBaseURL string
	chatDoer    ContextHTTPDoer

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

	// loginTakenSlots is an optional composition callback returning proxy slot
	// ids that belong to another provider (for example CodeArts-bound slots).
	// Auto login selection excludes them so one credential is not bound to two
	// providers. It is called without the OAuth lock held.
	loginTakenSlots func() []string

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

	// tokenMu guards tokenFlights, this provider's per-account single-flight
	// refresh table. tokenBaseURL, tokenTimeout, tokenNow, and tokenJoinHook
	// are test seams; their zero values use the realm's desktop endpoint, 30
	// seconds, time.Now, and no hook respectively.
	tokenMu       sync.Mutex
	tokenFlights  map[*Account]*tokenRefreshFlight
	tokenBaseURL  string
	tokenTimeout  time.Duration
	tokenNow      func() time.Time
	tokenJoinHook func()

	// catalogMu serializes catalog refreshes for this provider. The state and
	// manual-refresh cooldown are guarded separately so read paths never wait
	// behind an upstream request. catalogDoer, catalogBaseURL,
	// catalogDesktopPath, catalogNow, catalogTimeout, and catalogCooldown are
	// same-package test seams; their zero values use the provider transport,
	// the realm desktop endpoint, the desktop cache path, time.Now, 10s, and
	// the v1 30s manual-refresh cooldown respectively.
	catalogMu          sync.Mutex
	catalogStateMu     sync.RWMutex
	catalogState       catalogSnapshot
	catalogLastRefresh time.Time
	catalogDoer        ContextHTTPDoer
	catalogBaseURL     string
	catalogDesktopPath func(realm string) string
	catalogNow         func() time.Time
	catalogTimeout     time.Duration
	catalogCooldown    time.Duration

	// creditsDoer, creditsBillingBaseURL, creditsCheckinBaseURL,
	// creditsTimeout, and creditsNow are same-package test seams for the
	// billing sweep. Their zero values use the provider transport, the realm's
	// billing/chat endpoints, 15s, and time.Now respectively.
	creditsDoer           ContextHTTPDoer
	creditsBillingBaseURL string
	creditsCheckinBaseURL string
	creditsTimeout        time.Duration
	creditsNow            func() time.Time

	// creditsMu guards creditFlights, this provider's per-account single-flight
	// table for the credits sweep. Concurrent callers share one upstream fetch
	// and one credential write for the same account. creditJoinHook is a test
	// seam invoked when a caller joins an in-flight refresh.
	creditsMu      sync.Mutex
	creditFlights  map[*Account]*creditRefreshFlight
	creditJoinHook func()

	// creditsSweepMu prevents a second multi-account panel sweep from piling
	// onto the same accounts. A single-account refresh never takes this lock.
	creditsSweepMu sync.Mutex

	// Task 8 seams. tasksDoer/tasks*BaseURL/tasksTimeout/tasksNow/tasksSleep
	// isolate the checkin, daily, growth, and fun paths from the shared chat and
	// billing clients; tests point them at one httptest server. tasksGap,
	// tasksBackoff, tasksBackoffMax, tasksBudget, tasksTurnWait, and
	// tasksPollInterval default to v1's anti-risk pacing and web retry budgets.
	tasksDoer           ContextHTTPDoer
	tasksChatBaseURL    string
	tasksBillingBaseURL string
	tasksWebBaseURL     string
	tasksTimeout        time.Duration
	tasksNow            func() time.Time
	tasksSleep          func(time.Duration)
	tasksGap            time.Duration
	tasksBackoff        time.Duration
	tasksBackoffMax     time.Duration
	tasksBudget         time.Duration
	tasksTurnWait       time.Duration
	tasksPollInterval   time.Duration

	// Admin surface state. healthCache is a five-second /health cache; every
	// mutating route invalidates it. desktopDirs is a test seam for desktop
	// credential discovery; nil uses the platform defaults.
	healthMu     sync.Mutex
	healthCache  wbHealthCacheEntry
	desktopDirs  []string
	activityPath string

	// Scheduler state. jobs are built lazily on the intl provider; CN exposes
	// none so the core cannot schedule the same realm twice. schedulerEnabled
	// is a pointer so an unset provider defaults to enabled.
	jobsOnce         sync.Once
	jobs             []*scheduledJob
	jobsNow          func() time.Time
	schedulerMu      sync.Mutex
	schedulerEnabled *bool
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
		realm:             normalized,
		store:             store,
		accountsDir:       accountsDir,
		cfg:               LoadConfig(store, normalized),
		transport:         transport.NewClient(transport.ClientOptions{RefuseRedirects: true}),
		tasksGap:          tasksGapDefault,
		tasksBackoff:      tasksBackoffDefault,
		tasksBackoffMax:   tasksBackoffMax,
		tasksBudget:       tasksBudgetDefault,
		tasksTurnWait:     tasksTurnWait,
		tasksPollInterval: tasksPollDefault,
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
