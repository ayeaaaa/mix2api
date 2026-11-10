package limits

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func dailyRow(at time.Time, realm, account, model string, tokens, credit float64, outcome string) usage.Row {
	row := usage.Row{
		"at": at.Unix(), "realm": realm, "account": account, "model": model,
		"total_tokens": tokens, "outcome": outcome,
	}
	if credit != 0 {
		row["credit"] = credit
	}
	return row
}

func localMidnight(t *testing.T) time.Time {
	t.Helper()
	now := time.Now()
	year, month, day := now.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, now.Location())
}

func TestFoldPerRealmAndDay(t *testing.T) {
	start := localMidnight(t)
	end := start.AddDate(0, 0, 1)
	rows := []usage.Row{
		dailyRow(start.Add(-time.Second), "intl", "u1", "m1", 1, 1, "completed"),
		dailyRow(start, "intl", "u1", "m1", 10, 0.5, "completed"),
		dailyRow(start.Add(time.Hour), "intl", "u1", "m1", 10, 0.5, "completed"),
		dailyRow(start.Add(2*time.Hour), "intl", "u1", "m1", 999, 9, "client_aborted"),
		dailyRow(start.Add(3*time.Hour), "intl", "u1", "m2", 5, 0, "failed"),
		dailyRow(start.Add(4*time.Hour), "cn", "u1", "m3", 7, 0.25, "completed"),
		dailyRow(start.Add(5*time.Hour), "codearts", "u1", "m4", 11, 0.75, "completed"),
		dailyRow(start.Add(6*time.Hour), "raccoon", "u2", "m5", 13, 2, "completed"),
		dailyRow(start.Add(7*time.Hour), "", "", "m6", 100, 4, "completed"),
		dailyRow(end, "intl", "u1", "m1", 1000, 10, "completed"),
	}
	intl := FoldRealm(rows, "intl", start, end)
	if intl.Tokens["u1"] != 25 {
		t.Fatalf("intl tokens = %d, want 25", intl.Tokens["u1"])
	}
	if intl.Credits["u1"] != 1.0 {
		t.Fatalf("intl credits = %v, want 1", intl.Credits["u1"])
	}
	if intl.Models["u1"]["m1"] != 20 || intl.Models["u1"]["m2"] != 5 {
		t.Fatalf("intl model fold = %+v", intl.Models["u1"])
	}
	cn := FoldRealm(rows, "cn", start, end)
	if cn.Tokens["u1"] != 7 || cn.Credits["u1"] != 0.25 {
		t.Fatalf("cn fold = %+v", cn)
	}
	codearts := FoldRealm(rows, "codearts", start, end)
	if codearts.Tokens["u1"] != 11 {
		t.Fatalf("codearts fold = %+v", codearts)
	}
	raccoon := FoldRealm(rows, "raccoon", start, end)
	if raccoon.Tokens["u2"] != 13 {
		t.Fatalf("raccoon fold = %+v", raccoon)
	}
	all := FoldAll(rows, start, end)
	if all["intl"].Tokens["u1"] != 25 || all["cn"].Tokens["u1"] != 7 {
		t.Fatalf("same uid merged across realms: %+v", all)
	}
	if all[""].Tokens[""] != 0 {
		t.Fatalf("row without account was counted: %+v", all[""])
	}
	// Fold itself expects a single realm's rows; FoldRealm/FoldAll are the
	// grouping entry points.
	if Fold(rows, start, end).Tokens["u1"] != 43 {
		t.Fatalf("unfiltered Fold = %+v", Fold(rows, start, end))
	}
}

type fakeProvider struct {
	id     string
	realm  string
	calls  int
	values map[string]any
	usage  map[string]any
}

func (p *fakeProvider) ID() string                                  { return p.id }
func (p *fakeProvider) Realm() string                               { return p.realm }
func (p *fakeProvider) Overview(context.Context, bool) (any, error) { return nil, nil }
func (p *fakeProvider) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *fakeProvider) Models(context.Context, bool) ([]providers.Model, error) {
	return nil, nil
}
func (p *fakeProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, errors.New("not implemented")
}
func (p *fakeProvider) Routes() []providers.Route { return nil }
func (p *fakeProvider) Jobs() []scheduler.Job     { return nil }

type limitProvider struct {
	fakeProvider
}

func (p *limitProvider) ApplyLimits(values map[string]any, usage map[string]any) {
	p.calls++
	p.values = values
	p.usage = usage
}

func settingsStore(t *testing.T, values map[string]any) *config.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "accounts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := config.New(filepath.Join(dir, "settings.json"))
	if values != nil {
		if _, err := store.Merge(values); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func groupedLimits(entries map[string]any) map[string]any {
	return map[string]any{"limits": entries}
}

func TestApplyLimitsWorkBuddyOnly(t *testing.T) {
	store := settingsStore(t, groupedLimits(map[string]any{
		"daily_token_limit":    map[string]any{"global": 100, "intl": 150, "cn": nil},
		"reserve_credits":      map[string]any{"global": 10, "intl": 20},
		"expiring_window_days": map[string]any{"global": 4},
	}))
	workbuddy := &limitProvider{fakeProvider: fakeProvider{id: "workbuddy", realm: "intl"}}
	codearts := &fakeProvider{id: "codearts", realm: "codearts"}
	guard := NewGuard(store, []providers.Provider{workbuddy, codearts}, "")
	guard.LoadRows = func(context.Context) ([]usage.Row, error) {
		return []usage.Row{dailyRow(time.Now(), "intl", "u1", "m1", 40, 1, "completed")}, nil
	}
	if applied := guard.Apply(context.Background(), false); applied != 1 {
		t.Fatalf("applied = %d, want 1", applied)
	}
	entry := workbuddy.values["daily_token_limit"].(map[string]any)
	if entry["global"] != int64(100) || entry["intl"] != int64(150) {
		t.Fatalf("values = %+v", workbuddy.values)
	}
	if entry["cn"] != int64(100) {
		t.Fatalf("cn override did not inherit global: %+v", entry)
	}
	if reserve := workbuddy.values["reserve_credits"].(map[string]any); reserve["intl"] != int64(20) {
		t.Fatalf("reserve override lost: %+v", reserve)
	}
	if window := workbuddy.values["expiring_window_days"].(map[string]any); window["global"] != int64(4) {
		t.Fatalf("expiring window = %+v", window)
	}
	if workbuddy.usage == nil || workbuddy.usage["tokens"].(map[string]any)["u1"] != int64(40) {
		t.Fatalf("usage = %+v", workbuddy.usage)
	}
	if codearts.calls != 0 {
		t.Fatal("non-limit provider was called")
	}
}

func TestGuardDisableClearsLimits(t *testing.T) {
	store := settingsStore(t, groupedLimits(map[string]any{
		"daily_token_limit":       map[string]any{"global": 0, "intl": 0, "cn": 0},
		"daily_credit_limit":      map[string]any{"global": 0, "intl": 0, "cn": 0},
		"reserve_credits":         map[string]any{"global": 0},
		"model_daily_token_limit": map[string]any{"global": 0},
		"expiring_window_days":    map[string]any{"global": 0},
	}))
	workbuddy := &limitProvider{fakeProvider: fakeProvider{id: "workbuddy", realm: "intl"}}
	guard := NewGuard(store, []providers.Provider{workbuddy}, "")
	scans := 0
	guard.LoadRows = func(context.Context) ([]usage.Row, error) {
		scans++
		return nil, nil
	}
	if applied := guard.Apply(context.Background(), false); applied != 0 {
		t.Fatalf("hot path applied = %d, want 0", applied)
	}
	if scans != 0 {
		t.Fatal("hot path scanned with every limit disabled")
	}
	if applied := guard.Apply(context.Background(), true); applied != 1 {
		t.Fatalf("refresh applied = %d, want 1", applied)
	}
	if scans != 0 {
		t.Fatal("refresh with disabled limits scanned")
	}
	if workbuddy.usage != nil {
		t.Fatalf("disabled refresh fabricated usage: %+v", workbuddy.usage)
	}
	entry := workbuddy.values["daily_token_limit"].(map[string]any)
	if entry["global"] != int64(0) {
		t.Fatalf("disabled values not zero: %+v", workbuddy.values)
	}
}

func TestGuardUnknownUsageDoesNotPark(t *testing.T) {
	store := settingsStore(t, groupedLimits(map[string]any{
		"daily_token_limit": map[string]any{"global": 100},
	}))
	workbuddy := &limitProvider{fakeProvider: fakeProvider{id: "workbuddy", realm: "intl"}}
	guard := NewGuard(store, []providers.Provider{workbuddy}, "")
	guard.LoadRows = func(context.Context) ([]usage.Row, error) {
		return nil, errors.New("boom")
	}
	if applied := guard.Apply(context.Background(), false); applied != 1 {
		t.Fatalf("applied = %d", applied)
	}
	if workbuddy.usage != nil {
		t.Fatalf("fold error fabricated usage: %+v", workbuddy.usage)
	}
	if guard.LastError() == "" {
		t.Fatal("fold error was not retained for diagnostics")
	}
	// A second call must retry rather than freeze an empty fold.
	guard.LoadRows = func(context.Context) ([]usage.Row, error) {
		return []usage.Row{dailyRow(time.Now(), "intl", "u1", "m1", 3, 0, "completed")}, nil
	}
	guard.Apply(context.Background(), false)
	if workbuddy.usage == nil || workbuddy.usage["tokens"].(map[string]any)["u1"] != int64(3) {
		t.Fatalf("recovery fold = %+v", workbuddy.usage)
	}
}

func TestGuardRefresh(t *testing.T) {
	store := settingsStore(t, groupedLimits(map[string]any{
		"daily_token_limit": map[string]any{"global": 100},
	}))
	workbuddy := &limitProvider{fakeProvider: fakeProvider{id: "workbuddy", realm: "intl"}}
	guard := NewGuard(store, []providers.Provider{workbuddy}, "")
	guard.TTL = time.Hour
	rows := []usage.Row{dailyRow(time.Now(), "intl", "u1", "m1", 5, 0, "completed")}
	scans := 0
	guard.LoadRows = func(context.Context) ([]usage.Row, error) {
		scans++
		return rows, nil
	}
	guard.Apply(context.Background(), false)
	guard.Apply(context.Background(), false)
	if scans != 1 {
		t.Fatalf("request path scans = %d, want TTL cache hit", scans)
	}
	rows = append(rows, dailyRow(time.Now(), "intl", "u1", "m1", 7, 0, "completed"))
	guard.Apply(context.Background(), true)
	if scans != 2 {
		t.Fatalf("refresh scans = %d, want 2", scans)
	}
	if workbuddy.usage["tokens"].(map[string]any)["u1"] != int64(12) {
		t.Fatalf("refresh usage = %+v", workbuddy.usage)
	}
}

func TestGuardLegacyFlatLimits(t *testing.T) {
	store := settingsStore(t, map[string]any{"daily_token_limit": 42})
	workbuddy := &limitProvider{fakeProvider: fakeProvider{id: "workbuddy", realm: "cn"}}
	guard := NewGuard(store, []providers.Provider{workbuddy}, "")
	guard.LoadRows = func(context.Context) ([]usage.Row, error) { return nil, nil }
	guard.Apply(context.Background(), false)
	entry := workbuddy.values["daily_token_limit"].(map[string]any)
	if entry["global"] != int64(42) || entry["cn"] != int64(42) {
		t.Fatalf("legacy limits = %+v", workbuddy.values)
	}
}

func TestGuardNoLimitHotPathTouchesNothing(t *testing.T) {
	store := settingsStore(t, nil)
	workbuddy := &limitProvider{fakeProvider: fakeProvider{id: "workbuddy", realm: "intl"}}
	guard := NewGuard(store, []providers.Provider{workbuddy}, "")
	scans := 0
	guard.LoadRows = func(context.Context) ([]usage.Row, error) {
		scans++
		return nil, nil
	}
	if applied := guard.Apply(context.Background(), false); applied != 0 {
		t.Fatalf("no-limit hot path applied = %d", applied)
	}
	if workbuddy.calls != 0 || scans != 0 {
		t.Fatalf("no-limit hot path touched provider/scans: calls=%d scans=%d", workbuddy.calls, scans)
	}
	if applied := guard.Apply(context.Background(), true); applied != 1 {
		t.Fatalf("refresh applied = %d", applied)
	}
	if scans != 0 {
		t.Fatal("disabled refresh scanned the ledger")
	}
}
