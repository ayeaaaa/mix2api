package update

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type memorySettings struct {
	mu   sync.Mutex
	data map[string]any
}

func newMemorySettings() *memorySettings { return &memorySettings{data: map[string]any{}} }
func (m *memorySettings) Load() (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]any{}
	for key, value := range m.data {
		out[key] = value
	}
	return out, nil
}
func (m *memorySettings) Merge(patch map[string]any) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, value := range patch {
		m.data[key] = value
	}
	return m.data, nil
}

type fakeFetcher struct {
	body  string
	err   error
	calls int
}

func (f *fakeFetcher) Fetch(context.Context, string) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.body), nil
}

func TestCheckerStatusShapeAndPersistedState(t *testing.T) {
	settings := newMemorySettings()
	settings.data[CheckEnabledKey] = true
	fetcher := &fakeFetcher{body: `[{"tag_name":"v1.2.3","html_url":"https://github.com/x/y/releases/tag/v1.2.3","published_at":"2026-01-02T03:04:05Z"}]`}
	now := time.Unix(1700000000, 0)
	checker := NewChecker("1.0.0", settings, fetcher, func() time.Time { return now }, nil)
	status := checker.Check(context.Background(), true)
	if !status.Enabled || !status.UpdateAvailable || status.LatestVersion != "1.2.3" || status.LatestTag != "v1.2.3" {
		t.Fatalf("status = %#v", status)
	}
	if status.LastAttempt != "2023-11-14 22:13:20" || status.LastSuccess != status.LastAttempt {
		t.Fatalf("timestamps = %q %q", status.LastAttempt, status.LastSuccess)
	}
	if status.ReleaseURL != "https://github.com/x/y/releases/tag/v1.2.3" || status.PublishedAt == "" {
		t.Fatalf("release fields = %#v", status)
	}
	raw, _ := json.Marshal(settings.data)
	if strings.Contains(string(raw), "github.com") || strings.Contains(string(raw), "tag_name") {
		t.Fatalf("unsafe state persisted: %s", raw)
	}
	if !strings.Contains(string(raw), "latest_version") {
		t.Fatalf("version missing from state: %s", raw)
	}
}

func TestCheckerThrottleAndStartupBudget(t *testing.T) {
	settings := newMemorySettings()
	settings.data[CheckEnabledKey] = false
	fetcher := &fakeFetcher{body: `[]`}
	now := time.Unix(1700000000, 0)
	checker := NewChecker("1.0.0", settings, fetcher, func() time.Time { return now }, nil)
	status := checker.Check(context.Background(), false)
	if fetcher.calls != 0 || status.Msg != "skipped: disabled" {
		t.Fatalf("disabled check = %d %#v", fetcher.calls, status)
	}
	settings.data[CheckEnabledKey] = true
	checker.Check(context.Background(), false)
	if fetcher.calls != 1 {
		t.Fatalf("first due calls = %d", fetcher.calls)
	}
	checker.Check(context.Background(), false)
	if fetcher.calls != 1 {
		t.Fatalf("24h window not enforced: %d", fetcher.calls)
	}
	checker.Check(context.Background(), true)
	if fetcher.calls != 2 {
		t.Fatalf("manual did not bypass: %d", fetcher.calls)
	}
	if !checker.Due(now.Add(24 * time.Hour)) {
		t.Fatal("24h due false")
	}

	// Startup budget is six per hour; the first-ever startup check is allowed.
	settings2 := newMemorySettings()
	settings2.data[CheckEnabledKey] = true
	settings2.data[CheckStateKey] = map[string]any{"last_attempt": float64(now.Unix())}
	fetcher2 := &fakeFetcher{body: `[]`}
	startup := NewChecker("1.0.0", settings2, fetcher2, func() time.Time { return now }, nil)
	for i := 0; i < StartupChecks; i++ {
		if !startup.StartupDue(now) {
			t.Fatalf("startup budget exhausted early at %d", i)
		}
		startup.CheckStartup(context.Background())
	}
	if startup.StartupDue(now) {
		t.Fatal("startup budget was not enforced")
	}
}

func TestCheckerInjectedFetcherFailures(t *testing.T) {
	settings := newMemorySettings()
	settings.data[CheckEnabledKey] = true
	fetcher := &fakeFetcher{err: errors.New("network unavailable")}
	checker := NewChecker("1.0.0", settings, fetcher, time.Now, nil)
	status := checker.Check(context.Background(), true)
	if status.LastError == "" || strings.Contains(status.LastError, "http") {
		t.Fatalf("failure status = %#v", status)
	}
	// A later success keeps serving and replaces the error.
	fetcher.err = nil
	fetcher.body = `[{"tag_name":"v1.0.1"}]`
	status = checker.Check(context.Background(), true)
	if status.LastError != "" || status.LatestVersion != "1.0.1" {
		t.Fatalf("success status = %#v", status)
	}

	// Invalid JSON is short and safe.
	fetcher.body = "{not json"
	status = checker.Check(context.Background(), true)
	if status.LastError != "invalid JSON" {
		t.Fatalf("invalid json error = %q", status.LastError)
	}
}

func TestCheckerStopPrompt(t *testing.T) {
	settings := newMemorySettings()
	checker := NewChecker("1.0.0", settings, &fakeFetcher{body: `[]`}, time.Now, nil)
	done := make(chan struct{})
	go func() {
		checker.Run()
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	checker.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
}
