package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport/utlsbridge"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/agents"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/limits"
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
)

// PlatformOptions supplies command-level values that are not persisted in
// accounts/settings.json.
type PlatformOptions struct {
	Paths         Paths
	Version       string
	ListenerPort  int
	PanelPassword string

	// Home is the user home used by the agent configurator. Empty resolves
	// os.UserHomeDir lazily.
	Home string

	// MediaDir and UpdateDir override the v1 environment-derived directories.
	// Empty falls back to WB_MEDIA_DIR/"media" and
	// WB_UPDATE_REQUEST_DIR/DefaultRequestDir.
	MediaDir  string
	UpdateDir string

	// LAN reports whether the listener is reachable off-loopback; the update
	// request includes it for the executor.
	LAN bool

	Now    func() time.Time
	Logger func(string)
}

// Platform is the composed non-HTTP application. cmd constructs it after the
// provider setup and wires ServerOptions.Platform; tests use it to exercise
// platform routes without a live provider account.
type Platform struct {
	Paths     Paths
	Store     *config.Store
	Registry  *providers.Registry
	Providers []providers.Provider

	UsageStore    *usage.Store
	UsageCache    *usage.Cache
	UsageService  *usage.Service
	UsageRecorder *usage.Recorder
	Pricing       *pricing.Snapshot
	Refresher     *pricing.Refresher
	LogStore      *logs.LogStore

	Keys          *keys.Store
	PanelPassword *panelplatform.Password
	PanelSessions *panelplatform.Sessions

	Settings *settings.Service
	Realm    *settings.RealmService
	Proxy    *proxy.Slots

	Media    *media.Service
	Agents   *agents.Service
	Probes   *probes.Service
	WebTools *webtools.Service

	Updates        *update.Checker
	UpdateRequests *update.RequestManager
	Limits         *limits.Guard
	Scheduler      *scheduler.Runner

	closeOnce sync.Once
	stopProbe chan struct{}

	lifecycle sync.Mutex
	started   bool
	closed    bool
	runCancel context.CancelFunc
	runWG     sync.WaitGroup
}

// BuildPlatform composes every platform service over the already-built
// provider setup. Exactly one settings store, provider registry and usage
// directory are shared by all services.
func BuildPlatform(setup *Setup, opts PlatformOptions) (*Platform, error) {
	if setup == nil {
		return nil, errNilSetup
	}
	paths := setup.Paths
	if strings.TrimSpace(opts.Paths.AccountsDir) != "" {
		paths.AccountsDir = opts.Paths.AccountsDir
	}
	if strings.TrimSpace(opts.Paths.UsageDir) != "" {
		paths.UsageDir = opts.Paths.UsageDir
	}
	paths = normalizePaths(paths)
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = func(string) {}
	}

	usagePaths, err := usage.ResolvePaths(paths.AccountsDir, paths.UsageDir)
	if err != nil {
		return nil, err
	}
	if err := usagePaths.EnsureDir(); err != nil {
		return nil, err
	}
	usageStore := usage.New(usagePaths)
	usageCache := usage.NewCache(usagePaths.Dir)

	pricingSnapshot, err := pricing.Load(paths.AccountsDir, usagePaths.Dir)
	if err != nil {
		// Pricing is optional; an embedded snapshot is normally available, but
		// a build without it must still serve chat.
		pricingSnapshot = nil
	}

	keyStore := keys.New(setup.Store)
	providerList := append([]providers.Provider(nil), setup.Providers...)
	proxySlots := proxy.New(setup.Store, providerList)
	password := panelplatform.NewPassword(setup.Store)
	// PANEL_PASSWORD is the launcher bootstrap. It only replaces a fresh-install
	// default; a password already changed in the panel is never overwritten by a
	// stale environment value.
	if bootstrap := strings.TrimSpace(opts.PanelPassword); bootstrap != "" && bootstrap != panelplatform.DefaultPassword && password.IsDefault() {
		if err := password.Set(bootstrap); err != nil {
			opts.Logger("panel: bootstrap password failed: " + err.Error())
		}
	}
	sessions := panelplatform.NewSessions(panelplatform.SessionTTL)
	realm := settings.NewRealm(setup.Store, paths.AccountsDir, providerList)

	versions := opts.Version
	if strings.TrimSpace(versions) == "" {
		versions = "2.0.0-dev"
	}
	settingsService := settings.New(settings.Service{
		Store:                  setup.Store,
		Keys:                   keyStore,
		Providers:              providerList,
		PanelPasswordIsDefault: func() bool { return password.IsDefault() },
		SlotViews:              func() []map[string]any { return proxySlots.Views() },
		AccountsDir:            paths.AccountsDir,
		UsageDir:               usagePaths.Dir,
		SettingsFile:           setup.Store.Path,
		Project:                "mix2api",
		Version:                versions,
		Now:                    opts.Now,
	})

	logPath := filepath.Join(usagePaths.Dir, "logs.sqlite3")
	enabled, maxRows, maxDays, maxBytes := settingsService.LogCaps()
	logStore := logs.OpenCaps(logPath, enabled, logs.Caps{
		MaxRows: maxRows, MaxDays: maxDays, MaxBytes: maxBytes,
	}, nil)

	mediaDir := platformEnvDir(opts.MediaDir, "WB_MEDIA_DIR", "media")
	tasks := media.NewTaskStore(media.TaskStoreOptions{
		Dir:       mediaDir,
		Now:       opts.Now,
		Logger:    opts.Logger,
		Autostart: true,
	})
	recorder := &usage.Recorder{Store: usageStore, Now: opts.Now}
	mediaService := media.NewService(media.ServiceOptions{
		ConfigFunc: func() media.Config { return media.LoadFromStore(setup.Store) },
		Providers:  providerList,
		Tasks:      tasks,
		Usage:      recorder,
		UsageLog:   usagePaths.Log,
		Now:        opts.Now,
		Logger:     opts.Logger,
	})

	agentsService := agents.New(agents.Service{
		AccountsDir: paths.AccountsDir,
		Home:        opts.Home,
		Providers:   providerList,
		Now:         opts.Now,
	})

	webTools := webtools.New()
	webTools.Logger = opts.Logger
	webTools.Enabled = func() bool { return settingsBool(setup.Store, "local_web_tools", false) }

	probeProviders := allProbeProviders(providerList)
	probesService := probes.NewService(probes.Service{
		Providers: probeProviders,
		Window:    probes.NewWindowState(paths.AccountsDir),
		Quota:     probes.NewQuotaState(paths.AccountsDir),
		WatchModels: func() []string {
			return stringListSetting(setup.Store, "quota_scan_models")
		},
		Enabled: func() bool { return settingsBool(setup.Store, "window_probe", true) },
		Now:     opts.Now,
		Logger:  opts.Logger,
	})

	updateChecker := update.NewChecker(versions, setup.Store, nil, opts.Now, opts.Logger)
	root, _ := os.Executable()
	if root != "" {
		root = filepath.Dir(root)
	}
	requestManager := update.NewRequestManager(update.RequestManagerOptions{
		Executor: update.NewExecutor(platformEnvDir(opts.UpdateDir, "WB_UPDATE_REQUEST_DIR", update.DefaultRequestDir)),
		Now:      opts.Now,
		Port:     opts.ListenerPort,
		Root:     root,
		LAN:      opts.LAN,
	})

	guard := limits.NewGuard(setup.Store, providerList, usagePaths.Log)
	guard.Now = opts.Now
	guard.Logger = opts.Logger
	recorder.AfterRecord = func(ctx context.Context, _ usage.RecordInput) {
		guard.Apply(ctx, false)
	}

	usageService := usage.NewService(usageStore, pricingSnapshot, usageCache)
	usageService.Rollup = usage.NewRollup(usagePaths.Dir, pricingSnapshot)
	usageService.Now = opts.Now
	usageService.StartedAt = opts.Now()
	usageService.AccountsFunc, usageService.CurrentAccountFunc = usageAccountInventory(providerList)
	usageService.DefaultRealm = realm.Active(context.Background())
	usageService.DefaultRealmFunc = func() string { return realm.Active(context.Background()) }
	usageService.KeyName = func(id string) string {
		entries, err := keyStore.List(false)
		if err != nil {
			return id
		}
		for _, entry := range entries {
			if entry.ID == id {
				if strings.TrimSpace(entry.Name) != "" {
					return entry.Name
				}
				return id
			}
		}
		return id
	}

	platform := &Platform{
		Paths:          paths,
		Store:          setup.Store,
		Registry:       setup.Registry,
		Providers:      providerList,
		UsageStore:     usageStore,
		UsageCache:     usageCache,
		UsageService:   usageService,
		UsageRecorder:  recorder,
		Pricing:        pricingSnapshot,
		LogStore:       logStore,
		Keys:           keyStore,
		PanelPassword:  password,
		PanelSessions:  sessions,
		Settings:       settingsService,
		Realm:          realm,
		Proxy:          proxySlots,
		Media:          mediaService,
		Agents:         agentsService,
		Probes:         probesService,
		WebTools:       webTools,
		Updates:        updateChecker,
		UpdateRequests: requestManager,
		Limits:         guard,
		stopProbe:      make(chan struct{}),
	}
	platform.Scheduler = scheduler.NewRunner(providerJobs(providerList), scheduler.DefaultInterval, opts.Logger)
	platform.Refresher = pricing.NewRefresher(pricing.RefresherOptions{
		Snapshot: pricingSnapshot,
		Interval: func() float64 {
			if !settingsBool(setup.Store, "pricing_enabled", true) {
				return 0
			}
			return readFloatSetting(setup.Store, "pricing_refresh_minutes", pricing.DefaultRefreshMinutes)
		},
		Logger: opts.Logger,
		Now:    opts.Now,
	})

	settingsService.OnLimitsChanged = func(ctx context.Context, _ []string) {
		guard.Apply(ctx, true)
	}
	settingsService.OnLogSettingsChanged = func(context.Context) {
		enabled, rows, days, bytes := settingsService.LogCaps()
		if err := logStore.Configure(enabled, rows, days, bytes); err != nil {
			opts.Logger("logs: reconfigure failed: " + err.Error())
		}
	}

	return platform, nil
}

// RecordUsage appends one usage row. The recorder's AfterRecord hook publishes
// the daily guards; its failure never changes a response.
func (p *Platform) RecordUsage(ctx context.Context, in usage.RecordInput) error {
	if p == nil || p.UsageRecorder == nil {
		return nil
	}
	return p.UsageRecorder.Record(ctx, in)
}

// HTTPServices projects the platform into the narrow API composition view.
func (p *Platform) HTTPServices() *api.PlatformServices {
	if p == nil {
		return nil
	}
	return &api.PlatformServices{
		Usage:                  p.UsageService,
		LogStore:               p.LogStore,
		Pricing:                p.Pricing,
		PricingRefresher:       p.Refresher,
		PricingRefreshMinutes:  p.PricingRefreshMinutes,
		PricingVariantInherit:  p.PricingVariantInherit,
		Keys:                   p.Keys,
		Settings:               p.Settings,
		Realm:                  p.Realm,
		Proxy:                  p.Proxy,
		Media:                  p.Media,
		Agents:                 p.Agents,
		Probes:                 p.Probes,
		Updates:                p.Updates,
		UpdateRequests:         p.UpdateRequests,
		WebTools:               p.WebTools,
		PanelSessions:          p.PanelSessions,
		PanelPassword:          p.PanelPassword,
		RecordUsage:            p.RecordUsage,
		DefaultMaxOutputTokens: p.DefaultMaxOutputTokens,
	}
}

// PricingRefreshMinutes is the panel-configured refresh interval.
func (p *Platform) PricingRefreshMinutes() float64 {
	if p == nil {
		return pricing.DefaultRefreshMinutes
	}
	return readFloatSetting(p.Store, "pricing_refresh_minutes", pricing.DefaultRefreshMinutes)
}

// PricingVariantInherit reports whether a model may inherit a suffix-stripped
// base price. Missing reads as on.
func (p *Platform) PricingVariantInherit() bool {
	if p == nil {
		return true
	}
	return settingsBool(p.Store, "pricing_variant_inherit", true)
}

// AnthropicMessagesEnabled reads the panel opt-in. The API also honors the
// WB_ENABLE_ANTHROPIC_MESSAGES environment variable.
func (p *Platform) AnthropicMessagesEnabled() bool {
	if p == nil {
		return false
	}
	return settingsBool(p.Store, "enable_anthropic_messages", false)
}

// DefaultMaxOutputTokens returns the catalog max_output for a model. The API
// server caches positive values for the process lifetime, so a provider
// catalog is not scanned on every request.
func (p *Platform) DefaultMaxOutputTokens(model string) int {
	model = strings.TrimSpace(model)
	if p == nil || model == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, provider := range p.Providers {
		models, err := provider.Models(ctx, false)
		if err != nil {
			continue
		}
		for _, entry := range models {
			id, _ := entry["id"].(string)
			if !strings.EqualFold(strings.TrimSpace(id), model) {
				continue
			}
			for _, key := range []string{"max_output", "max_output_tokens", "maxOutputTokens"} {
				if value, ok := positiveInt(entry[key]); ok {
					return value
				}
			}
		}
	}
	return 0
}

// Start launches the optional background loops. Callers invoke it only after
// the listener is ready; Close stops everything and is idempotent.
func (p *Platform) Start(ctx context.Context) {
	if p == nil {
		return
	}
	p.lifecycle.Lock()
	if p.started || p.closed {
		p.lifecycle.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.started = true
	p.runCancel = cancel
	start := func(run func()) {
		p.runWG.Add(1)
		go func() {
			defer p.runWG.Done()
			run()
		}()
	}
	if p.Refresher != nil {
		start(func() { p.Refresher.Run(runCtx) })
	}
	if p.Updates != nil {
		start(func() { p.Updates.Run() })
		start(func() {
			<-runCtx.Done()
			p.Updates.Stop()
		})
	}
	if p.Scheduler != nil {
		start(func() { p.Scheduler.Run(runCtx) })
	}
	if p.Probes != nil {
		start(func() { p.runProbes(runCtx) })
	}
	p.lifecycle.Unlock()
}

func (p *Platform) runProbes(ctx context.Context) {
	window := time.NewTicker(probeWindowInterval)
	quota := time.NewTicker(probeQuotaInterval)
	defer window.Stop()
	defer quota.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopProbe:
			return
		case <-window.C:
			p.Probes.RunWindow(ctx)
		case <-quota.C:
			_, _ = p.Probes.RunQuota(ctx, false)
		}
	}
}

const (
	probeWindowInterval = 45 * time.Second
	probeQuotaInterval  = 5 * time.Minute
)

// Close releases the services with owned goroutines/DB handles. It is safe to
// call more than once.
func (p *Platform) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.lifecycle.Lock()
		p.closed = true
		cancel := p.runCancel
		p.lifecycle.Unlock()
		if cancel != nil {
			cancel()
		}
		if p.stopProbe != nil {
			close(p.stopProbe)
		}
		if p.Refresher != nil {
			p.Refresher.Stop()
		}
		if p.Updates != nil {
			p.Updates.Stop()
		}
		// Stop owns scheduler/probe/refresher loops before their stores close.
		// A slow optional network fetch gets a bounded grace period.
		done := make(chan struct{})
		go func() {
			p.runWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		if p.Media != nil && p.Media.Tasks != nil {
			p.Media.Tasks.Stop()
		}
		if p.LogStore != nil {
			_ = p.LogStore.Close()
		}
		_ = utlsbridge.CloseDefault()
	})
}

var errNilSetup = platformError("app: platform requires provider setup")

type platformError string

func (e platformError) Error() string { return string(e) }

func providerJobs(list []providers.Provider) []scheduler.Job {
	out := make([]scheduler.Job, 0)
	for _, provider := range list {
		out = append(out, provider.Jobs()...)
	}
	return out
}

func allProbeProviders(list []providers.Provider) []providers.ProbeProvider {
	out := make([]providers.ProbeProvider, 0)
	for _, provider := range list {
		if probe, ok := provider.(providers.ProbeProvider); ok {
			out = append(out, probe)
		}
	}
	return out
}

func settingsBool(store *config.Store, key string, fallback bool) bool {
	if store == nil {
		return fallback
	}
	data, err := store.Load()
	if err != nil {
		return fallback
	}
	value, present := data[key]
	if !present {
		return fallback
	}
	boolean, ok := value.(bool)
	if !ok {
		return fallback
	}
	return boolean
}

func stringListSetting(store *config.Store, key string) []string {
	if store == nil {
		return nil
	}
	data, err := store.Load()
	if err != nil {
		return nil
	}
	raw, ok := data[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, strings.TrimSpace(text))
	}
	return out
}

func readFloatSetting(store *config.Store, key string, fallback float64) float64 {
	if store == nil {
		return fallback
	}
	data, err := store.Load()
	if err != nil {
		return fallback
	}
	value, ok := data[key]
	if !ok {
		return fallback
	}
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		if err == nil {
			return number
		}
	case float64:
		return typed
	case int:
		return float64(typed)
	}
	return fallback
}

func platformEnvDir(explicit, envName, fallback string) string {
	if value := strings.TrimSpace(explicit); value != "" {
		return filepath.Clean(value)
	}
	return envDir(envName, fallback)
}

func positiveInt(value any) (int, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		if err == nil && number > 0 {
			return int(number), true
		}
	case float64:
		if typed > 0 {
			return int(typed), true
		}
	case int:
		if typed > 0 {
			return typed, true
		}
	case int64:
		if typed > 0 {
			return int(typed), true
		}
	}
	return 0, false
}

// usageAccountInventory adapts provider-owned account pools to the panel's
// v1 usage summary. Only providers that implement AccountInventory contribute
// rows, so the Raccoon/CodeArts usage payloads stay unchanged.
func usageAccountInventory(providerList []providers.Provider) (func() map[string]usage.AccountInfo, func(string) usage.AccountInfo) {
	collect := func() []providers.AccountSummary {
		byUID := make(map[string]providers.AccountSummary)
		for _, provider := range providerList {
			inventory, ok := provider.(providers.AccountInventory)
			if !ok {
				continue
			}
			rows, err := inventory.AccountSummaries(context.Background())
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
	accounts := func() map[string]usage.AccountInfo {
		rows := collect()
		out := make(map[string]usage.AccountInfo, len(rows))
		for _, row := range rows {
			out[row.UID] = usage.AccountInfo{
				UID: row.UID, Nickname: row.Nickname, Realm: row.Realm,
				Domain: row.Domain, Issuer: row.Issuer,
				CredentialFile: row.CredentialFile, ExpiresAt: row.ExpiresAt,
			}
		}
		return out
	}
	current := func(realm string) usage.AccountInfo {
		rows := collect()
		info := usage.AccountInfo{Accounts: len(rows)}
		for _, row := range rows {
			if row.Ready {
				info.AccountsReady++
			}
		}
		realm = strings.ToLower(strings.TrimSpace(realm))
		if realm == "all" {
			realm = ""
		}
		for _, row := range rows {
			if realm != "" && row.Realm != realm {
				continue
			}
			info.UID = row.UID
			info.Nickname = row.Nickname
			info.Realm = row.Realm
			info.Domain = row.Domain
			info.Issuer = row.Issuer
			info.CredentialFile = row.CredentialFile
			info.ExpiresAt = row.ExpiresAt
			break
		}
		return info
	}
	return accounts, current
}
