package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestPlatformComposition(t *testing.T) {
	root := t.TempDir()
	setup, err := Build(Paths{
		AccountsDir: filepath.Join(root, "accounts"),
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	platform, err := BuildPlatform(setup, PlatformOptions{
		Paths:     setup.Paths,
		Version:   "2.0.0-test",
		MediaDir:  filepath.Join(root, "media"),
		UpdateDir: filepath.Join(root, "updates"),
		Home:      root,
	})
	if err != nil {
		t.Fatalf("BuildPlatform: %v", err)
	}
	defer platform.Close()

	checks := map[string]bool{
		"usage":    platform.UsageService != nil && platform.UsageStore != nil,
		"pricing":  platform.Pricing != nil,
		"logs":     platform.LogStore != nil,
		"keys":     platform.Keys != nil,
		"panel":    platform.PanelPassword != nil && platform.PanelSessions != nil,
		"settings": platform.Settings != nil,
		"realm":    platform.Realm != nil,
		"proxy":    platform.Proxy != nil,
		"media":    platform.Media != nil,
		"agents":   platform.Agents != nil,
		"webtools": platform.WebTools != nil,
		"probes":   platform.Probes != nil,
		"updates":  platform.Updates != nil && platform.UpdateRequests != nil,
		"limits":   platform.Limits != nil,
		"jobs":     platform.Scheduler != nil && len(platform.Scheduler.Jobs) > 0,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s service was not composed", name)
		}
	}
	if platform.Store != setup.Store {
		t.Fatal("composition replaced the shared settings store")
	}
	if platform.Registry != setup.Registry {
		t.Fatal("composition replaced the shared provider registry")
	}
	if platform.Settings.Keys != platform.Keys {
		t.Fatal("settings and API-key store are not shared")
	}
	if len(platform.Probes.Providers) != 2 {
		t.Fatalf("probe providers = %d, want WorkBuddy intl and cn", len(platform.Probes.Providers))
	}
	realms := map[string]bool{}
	for _, probe := range platform.Probes.Providers {
		if typed, ok := probe.(interface{ Realm() string }); ok {
			realms[typed.Realm()] = true
		}
	}
	if !realms["intl"] || !realms["cn"] {
		t.Fatalf("probe realms = %v, want intl and cn", realms)
	}
	if platform.LogStore.Path() != filepath.Join(setup.Paths.UsageDir, "logs.sqlite3") {
		t.Fatalf("log path = %q", platform.LogStore.Path())
	}
	if _, err := os.Stat(setup.Paths.UsageDir); err != nil {
		t.Fatalf("usage directory not created: %v", err)
	}
	// Mounting the full route set would panic on a duplicate ServeMux pattern.
	_ = api.New(api.ServerOptions{
		Providers: setup.Providers,
		Platform:  platform.HTTPServices(),
		PanelAuth: api.RequirePanelAuth("test-panel"),
		Version:   "2.0.0-test",
	})
	// Closing twice must stay safe, and a closed log store must be visible.
	platform.Close()
	platform.Close()
}

func TestGuardCallSites(t *testing.T) {
	root := t.TempDir()
	store := config.New(filepath.Join(root, "accounts", "settings.json"))
	if err := os.MkdirAll(filepath.Join(root, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Merge(map[string]any{
		"limits": map[string]any{
			"daily_token_limit": map[string]any{"global": 100},
		},
	}); err != nil {
		t.Fatal(err)
	}
	provider := &appLimitProvider{realm: "intl"}
	registry := providers.NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	setup := &Setup{
		Paths:     Paths{AccountsDir: filepath.Join(root, "accounts"), UsageDir: filepath.Join(root, "usage")},
		Store:     store,
		Registry:  registry,
		Providers: []providers.Provider{provider},
	}
	platform, err := BuildPlatform(setup, PlatformOptions{
		Paths:     setup.Paths,
		MediaDir:  filepath.Join(root, "media"),
		UpdateDir: filepath.Join(root, "updates"),
		Home:      root,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if platform.UsageRecorder.AfterRecord == nil {
		t.Fatal("recorder has no guard hook")
	}
	err = platform.RecordUsage(context.Background(), usage.RecordInput{
		Model: "m1", Realm: "intl", Account: "u1", Outcome: "completed",
		Usage: map[string]any{"total_tokens": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.applied == 0 {
		t.Fatal("recording did not publish the daily guard")
	}
	if provider.usage == nil {
		t.Fatal("guard did not receive the folded counters")
	}
	if _, err := os.Stat(filepath.Join(setup.Paths.UsageDir, "usage.jsonl")); err != nil {
		t.Fatalf("usage row not appended: %v", err)
	}
	appliedAfterRecord := provider.applied
	if _, err := platform.Settings.Save(context.Background(), map[string]any{"ui_language": "en"}); err != nil {
		t.Fatal(err)
	}
	if provider.applied != appliedAfterRecord {
		t.Fatal("unrelated settings save forced a guard scan")
	}
	if _, err := platform.Settings.Save(context.Background(), map[string]any{"daily_token_limit": 200}); err != nil {
		t.Fatal(err)
	}
	if provider.applied <= appliedAfterRecord {
		t.Fatal("limit settings save did not refresh the guard")
	}
}

type appLimitProvider struct {
	realm   string
	applied int
	usage   map[string]any
	jobs    []scheduler.Job
}

func (p *appLimitProvider) ID() string    { return "app-limit" }
func (p *appLimitProvider) Realm() string { return p.realm }
func (p *appLimitProvider) Overview(context.Context, bool) (any, error) {
	return nil, nil
}
func (p *appLimitProvider) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *appLimitProvider) Models(context.Context, bool) ([]providers.Model, error) {
	return nil, nil
}
func (p *appLimitProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, errors.New("not implemented")
}
func (p *appLimitProvider) Routes() []providers.Route { return nil }
func (p *appLimitProvider) Jobs() []scheduler.Job     { return p.jobs }
func (p *appLimitProvider) ApplyLimits(_ map[string]any, usage map[string]any) {
	p.applied++
	p.usage = usage
}

func TestPlatformCloseCancelsBackgroundLoops(t *testing.T) {
	root := t.TempDir()
	store := config.New(filepath.Join(root, "accounts", "settings.json"))
	if err := os.MkdirAll(filepath.Join(root, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	stopped := make(chan struct{})
	job := appBlockingJob{started: started, stopped: stopped}
	provider := &appLimitProvider{realm: "intl", jobs: []scheduler.Job{job}}
	registry := providers.NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	platform, err := BuildPlatform(&Setup{
		Paths:     Paths{AccountsDir: filepath.Join(root, "accounts"), UsageDir: filepath.Join(root, "usage")},
		Store:     store,
		Registry:  registry,
		Providers: []providers.Provider{provider},
	}, PlatformOptions{
		Paths:     Paths{AccountsDir: filepath.Join(root, "accounts"), UsageDir: filepath.Join(root, "usage")},
		MediaDir:  filepath.Join(root, "media"),
		UpdateDir: filepath.Join(root, "updates"),
		Home:      root,
	})
	if err != nil {
		t.Fatal(err)
	}
	platform.Scheduler.Interval = 5 * time.Millisecond
	platform.Start(context.Background())
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler job did not start")
	}
	platform.Close()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler job was not cancelled by Close")
	}
}

type appBlockingJob struct {
	started chan struct{}
	stopped chan struct{}
}

func (j appBlockingJob) Name() string { return "blocking" }
func (j appBlockingJob) Run(ctx context.Context) error {
	select {
	case j.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	close(j.stopped)
	return ctx.Err()
}
