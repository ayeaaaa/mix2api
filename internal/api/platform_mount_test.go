package api

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/logs"
	panelplatform "github.com/ayeaaaa/mix2api-v2/internal/platform/panel"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
)

func TestPlatformUsageLogsMount(t *testing.T) {
	dir := t.TempDir()
	usageService := usage.NewService(usage.New(usage.Paths{
		Dir:           dir,
		Log:           filepath.Join(dir, "usage.jsonl"),
		LimitEvents:   filepath.Join(dir, "limit-events.jsonl"),
		EstimateDaily: filepath.Join(dir, "estimate-daily.json"),
		RecentIndex:   filepath.Join(dir, "usage-recent-index.json"),
	}), nil, nil)
	logStore := logs.Open(filepath.Join(dir, "logs.sqlite3"), false, logs.Caps{}, nil)
	t.Cleanup(func() { _ = logStore.Close() })
	sessions := panelplatform.NewSessions(0)
	token := sessions.Create()
	server := New(ServerOptions{
		Usage:         usageService,
		LogStore:      logStore,
		PanelSessions: sessions,
	})

	for _, path := range []string{"/usage", "/v1/usage", "/usage/recent", "/usage/analytics", "/usage/perf", "/usage/timeseries", "/usage/by-account", "/usage/remaining", "/logs"} {
		rec := doRequest(server, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without panel token = %d", path, rec.Code)
		}
	}
	rec := doRequest(server, http.MethodGet, "/logs", "", map[string]string{"X-Panel-Token": token})
	if rec.Code != http.StatusOK {
		t.Fatalf("/logs = %d body=%s", rec.Code, rec.Body)
	}
	rec = doRequest(server, http.MethodGet, "/usage", "", map[string]string{"X-Panel-Token": token})
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusUnauthorized {
		t.Fatalf("/usage mounted but status = %d body=%s", rec.Code, rec.Body)
	}
	// Unknown paths under a guarded prefix fail closed, not open.
	rec = doRequest(server, http.MethodGet, "/usage/not-a-route", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown usage subtree = %d", rec.Code)
	}
}

func TestDefaultMaxOutputTokensZeroNotCached(t *testing.T) {
	value := 0
	calls := 0
	server := New(ServerOptions{DefaultMaxOutputTokens: func(string) int {
		calls++
		return value
	}})
	if got := server.defaultMaxOutputTokens("m2"); got != 0 {
		t.Fatalf("first = %d", got)
	}
	value = 5
	if got := server.defaultMaxOutputTokens("m2"); got != 5 {
		t.Fatalf("zero was cached: %d", got)
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestDefaultMaxOutputTokensCached(t *testing.T) {
	calls := 0
	server := New(ServerOptions{DefaultMaxOutputTokens: func(model string) int {
		calls++
		if model == "m1" {
			return 123
		}
		return 0
	}})
	if got := server.defaultMaxOutputTokens("m1"); got != 123 {
		t.Fatalf("first lookup = %d", got)
	}
	if got := server.defaultMaxOutputTokens("m1"); got != 123 {
		t.Fatalf("cached lookup = %d", got)
	}
	if calls != 1 {
		t.Fatalf("lookup calls = %d", calls)
	}
	if got := server.defaultMaxOutputTokens(""); got != 0 {
		t.Fatalf("empty model = %d", got)
	}
}
