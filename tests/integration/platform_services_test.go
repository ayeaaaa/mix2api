package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/app"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/logs"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestPlatformLifecycleAndRoutes(t *testing.T) {
	root := t.TempDir()
	setup, err := app.Build(app.Paths{
		AccountsDir: filepath.Join(root, "accounts"),
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err != nil {
		t.Fatal(err)
	}
	platform, err := app.BuildPlatform(setup, app.PlatformOptions{
		Paths:         setup.Paths,
		Version:       "2.0.0-integration",
		PanelPassword: "admin",
		MediaDir:      filepath.Join(root, "media"),
		UpdateDir:     filepath.Join(root, "updates"),
		Home:          root,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := api.New(api.ServerOptions{
		Version:   "2.0.0-integration",
		Providers: setup.Providers,
		Platform:  platform.HTTPServices(),
		PanelAuth: api.RequirePanelAuth("integration-panel"),
	})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage", nil)
	req.Header.Set("X-Panel-Token", "integration-panel")
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage = %d body=%s", rec.Code, rec.Body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	platform.Start(ctx)
	cancel()
	platform.Close()
	platform.Close()
	// A closed log store rejects new appends instead of panicking.
	if err := platform.LogStore.Append(logs.Entry{Level: "info", Msg: "after close"}); err == nil {
		t.Fatal("append after close succeeded")
	}
}

type integrationProvider struct {
	realm string
	calls int
}

func (p *integrationProvider) ID() string    { return "integration" }
func (p *integrationProvider) Realm() string { return p.realm }
func (p *integrationProvider) Overview(context.Context, bool) (any, error) {
	return map[string]any{}, nil
}
func (p *integrationProvider) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *integrationProvider) Models(context.Context, bool) ([]providers.Model, error) {
	return []providers.Model{{"id": "m1", "object": "model"}}, nil
}
func (p *integrationProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	p.calls++
	return &integrationStream{}, nil
}
func (p *integrationProvider) Routes() []providers.Route { return nil }
func (p *integrationProvider) Jobs() []scheduler.Job     { return nil }

type integrationStream struct{ sent bool }

func (s *integrationStream) Next() ([]byte, error) {
	if s.sent {
		return nil, io.EOF
	}
	s.sent = true
	return []byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"), nil
}
func (s *integrationStream) Close() error { return nil }

func TestLauncherAPIKeyFallback(t *testing.T) {
	root := t.TempDir()
	accounts := filepath.Join(root, "accounts")
	if err := os.MkdirAll(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	store := config.New(filepath.Join(accounts, "settings.json"))
	keyStore := keys.New(store)
	provider := &integrationProvider{realm: "intl"}
	server := api.New(api.ServerOptions{
		Providers:    []providers.Provider{provider},
		KeyStore:     keyStore,
		APIKeyAuth:   api.RequireAPIKeyAuth("launcher-key"),
		StaticAPIKey: "launcher-key",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models?realm=intl", nil)
	req.Header.Set("Authorization", "Bearer launcher-key")
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("launcher key = %d body=%s", rec.Code, rec.Body)
	}
}

func TestSchedulerRunnerLifecycle(t *testing.T) {
	var mu sync.Mutex
	runs := 0
	job := schedulerJobFunc(func(context.Context) error {
		mu.Lock()
		runs++
		mu.Unlock()
		return nil
	})
	runner := scheduler.NewRunner([]scheduler.Job{job}, 5*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
	mu.Lock()
	count := runs
	mu.Unlock()
	if count == 0 {
		t.Fatal("scheduler never ran the job")
	}
}

type schedulerJobFunc func(context.Context) error

func (f schedulerJobFunc) Name() string                  { return "test-job" }
func (f schedulerJobFunc) Run(ctx context.Context) error { return f(ctx) }

func TestLogCapsExplicitZeroIsNotDefault(t *testing.T) {
	root := t.TempDir()
	accounts := filepath.Join(root, "accounts")
	if err := os.MkdirAll(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	store := config.New(filepath.Join(accounts, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"log_db_enabled":   true,
		"log_db_max_days":  0,
		"log_db_max_rows":  0,
		"log_db_max_bytes": 0,
	}); err != nil {
		t.Fatal(err)
	}
	service := appSettingsService(store, accounts)
	_, _, maxDays, _ := service.LogCaps()
	if maxDays != 0 {
		t.Fatalf("explicit zero max days = %d", maxDays)
	}
	path := filepath.Join(root, "usage", "logs.sqlite3")
	store2 := logs.OpenCaps(path, true, logs.Caps{MaxDays: 0}, nil)
	defer store2.Close()
	if err := store2.Append(logs.Entry{Level: "info", Msg: "zero caps"}); err != nil {
		t.Fatal(err)
	}
}

func appSettingsService(store *config.Store, accounts string) *settings.Service {
	return settings.New(settings.Service{Store: store, AccountsDir: accounts})
}

func TestNoNetworkInTests(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	blocked := 0
	http.DefaultTransport = integrationRoundTripper(func(req *http.Request) (*http.Response, error) {
		host := req.URL.Hostname()
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			blocked++
			return nil, fmt.Errorf("non-loopback network access to %s", host)
		}
		return old.RoundTrip(req)
	})
	root := t.TempDir()
	setup, err := app.Build(app.Paths{
		AccountsDir: filepath.Join(root, "accounts"),
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err != nil {
		t.Fatal(err)
	}
	platform, err := app.BuildPlatform(setup, app.PlatformOptions{
		Paths: setup.Paths, MediaDir: filepath.Join(root, "media"),
		UpdateDir: filepath.Join(root, "updates"), Home: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	server := api.New(api.ServerOptions{
		Providers: setup.Providers, Platform: platform.HTTPServices(),
		PanelAuth: api.RequirePanelAuth("p"),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	if blocked != 0 {
		t.Fatalf("blocked %d non-loopback requests", blocked)
	}
}

type integrationRoundTripper func(*http.Request) (*http.Response, error)

func (f integrationRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
