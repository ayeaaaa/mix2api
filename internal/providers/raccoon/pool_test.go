package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/accountstate"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func poolExpiry(now time.Time) time.Time { return now.Add(time.Hour) }

// writePoolAuth writes one v1-shaped auth document. A blank name uses the
// canonical AuthPath for uid.
func writePoolAuth(t *testing.T, dir, uid, name string, expires time.Time, extra map[string]any) string {
	t.Helper()
	token := makeUnsignedJWT(t, map[string]any{"sub": uid, "exp": expires.Unix()})
	payload := map[string]any{
		"uid":             uid,
		"access_token":    token,
		"refresh_token":   "refresh-" + uid,
		"office_identity": "personal",
		"office_org_name": "Org " + uid,
		"expires_at":      expires.Unix(),
		"token_type":      "Bearer",
	}
	for key, value := range extra {
		payload[key] = value
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	path := name
	if path == "" {
		canonical, err := AuthPath(dir, uid)
		if err != nil {
			t.Fatal(err)
		}
		path = canonical
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(dir, "raccoon", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writePoolState(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "raccoon", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustPool(t *testing.T, dir string, cfg Config) *Pool {
	t.Helper()
	p, err := NewPool(dir, cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return p
}

func accountUID(t *testing.T, a *Account) string {
	t.Helper()
	if a == nil {
		t.Fatal("account is nil")
	}
	return a.Auth.UID
}

func int64Pointer(value int64) *int64       { return &value }
func float64Pointer(value float64) *float64 { return &value }

func TestPickSkipsDisabledAndZeroReserve(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	expiry := poolExpiry(now)
	writePoolAuth(t, dir, "u-disabled", "", expiry, nil)
	writePoolAuth(t, dir, "u-zero", "", expiry, nil)
	writePoolAuth(t, dir, "u-blocked", "", expiry, nil)

	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	if err := p.Disable("u-disabled", "manual disable"); err != nil {
		t.Fatal(err)
	}

	zero := p.Get("u-zero")
	zero.Credits = &CreditsSnapshot{Remain: 0}
	zero.ReserveCredits = 0

	blocked := p.Get("u-blocked")
	blocked.Credits = &CreditsSnapshot{Remain: 5}
	blocked.ReserveCredits = 10

	got, err := p.Pick("", "", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got != zero {
		t.Fatalf("Pick returned %q, want u-zero", accountUID(t, got))
	}

	if _, err := p.Pick("", "", map[string]bool{"u-zero": true}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Pick with all usable accounts excluded = %v, want ErrNoAccount", err)
	}
}

func TestManualDisableSurvivesApplyLimits(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-manual", "", poolExpiry(now), nil)

	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	if err := p.Disable("u-manual", "manual disable"); err != nil {
		t.Fatal(err)
	}

	p.ApplyLimits(
		map[string]any{
			"reserve_credits":          map[string]any{"global": 10},
			"daily_credit_limit":       map[string]any{"global": 5},
			"daily_token_limit":        map[string]any{"global": 100},
			"model_daily_token_limit":  map[string]any{"global": 50},
			"unrelated_provider_limit": map[string]any{"global": 1},
		},
		map[string]any{
			"tokens":  map[string]any{"u-manual": 100},
			"credits": map[string]any{"u-manual": 5.0},
			"models":  map[string]any{"u-manual": map[string]any{"m1": 50}},
		},
	)

	a := p.Get("u-manual")
	a.Credits = &CreditsSnapshot{Remain: 5}
	if !a.State.Disabled || !a.State.ManualDisabled {
		t.Fatalf("manual disable was lost: %+v", a.State)
	}
	if a.State.DisabledBy != "manual" || a.State.Reason != "manual disable" {
		t.Fatalf("manual disable metadata changed: %+v", a.State)
	}
	if a.State.LastError != "manual disable" {
		t.Fatalf("LastError = %q, want manual disable", a.State.LastError)
	}
	if a.ReserveCredits != 10 || a.DailyCreditLimit != 5 || a.DailyTokenLimit != 100 || a.ModelDailyTokenLimit != 50 {
		t.Fatalf("limits not applied: %+v", a)
	}
	if view := a.View(); !view.Disabled || view.Guard != "reserve" {
		t.Fatalf("view after ApplyLimits = %+v", view)
	}

	if err := p.Enable("u-manual"); err != nil {
		t.Fatal(err)
	}
	if a.State.Disabled || a.State.ManualDisabled || a.State.Reason != "" || a.State.LastError != "" || a.State.ErrCount != 0 || !a.State.CoolUntil.IsZero() {
		t.Fatalf("Enable did not clear state: %+v", a.State)
	}

	// A custom reason survives a reload through state.json.
	if err := p.Disable("u-manual", "operator decision"); err != nil {
		t.Fatal(err)
	}
	reloaded := mustPool(t, dir, Config{MaxConcurrent: 1})
	again := reloaded.Get("u-manual")
	if !again.State.Disabled || !again.State.ManualDisabled || again.State.Reason != "operator decision" {
		t.Fatalf("manual disable did not survive reload: %+v", again.State)
	}
}

func TestAcquireRespectsMaxConcurrent(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-one", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 2})
	a := p.Get("u-one")

	if !p.Acquire(a) || !p.Acquire(a) {
		t.Fatal("two acquisitions should fit max_concurrent=2")
	}
	if p.Acquire(a) {
		t.Fatal("third acquisition exceeded max_concurrent")
	}
	if view := a.View(); view.ActiveConcurrent != 2 {
		t.Fatalf("ActiveConcurrent = %d, want 2", view.ActiveConcurrent)
	}

	p.Release(a)
	p.Release(a)
	p.Release(a)
	if view := a.View(); view.ActiveConcurrent != 0 {
		t.Fatalf("ActiveConcurrent after releases = %d, want 0", view.ActiveConcurrent)
	}
	if !p.Acquire(a) {
		t.Fatal("acquisition after release should succeed")
	}
	if p.Acquire(nil) {
		t.Fatal("Acquire(nil) must be false")
	}
	p.Release(nil)
}

func TestAcquireConcurrent(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-race", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 3})
	a := p.Get("u-race")

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
	)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Acquire(a) {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 3 {
		t.Fatalf("successful acquisitions = %d, want 3", success)
	}
	if view := a.View(); view.ActiveConcurrent != 3 {
		t.Fatalf("ActiveConcurrent = %d, want 3", view.ActiveConcurrent)
	}
	for i := 0; i < 5; i++ {
		p.Release(a)
	}
	if view := a.View(); view.ActiveConcurrent != 0 {
		t.Fatalf("ActiveConcurrent = %d after releases, want 0", view.ActiveConcurrent)
	}
}

func TestSessionAffinity(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-a", "", poolExpiry(now), nil)
	writePoolAuth(t, dir, "u-b", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})

	p.BindSession("session-1", "u-b")
	// Make u-b the most recently used account; affinity must still win.
	if !p.Acquire(p.Get("u-b")) {
		t.Fatal("acquire u-b")
	}
	p.Release(p.Get("u-b"))

	if got, err := p.Pick("", "session-1", nil); err != nil || got.Auth.UID != "u-b" {
		t.Fatalf("Pick(session-1) = %v, %v; want u-b", got, err)
	}
	// A new session goes to the least recently used account.
	if got, err := p.Pick("", "session-2", nil); err != nil || got.Auth.UID != "u-a" {
		t.Fatalf("Pick(session-2) = %v, %v; want u-a", got, err)
	}

	// Affinity does not bypass the concurrency limit.
	if !p.Acquire(p.Get("u-b")) {
		t.Fatal("acquire u-b")
	}
	if got, err := p.Pick("", "session-1", nil); err != nil || got.Auth.UID != "u-a" {
		t.Fatalf("Pick(session-1) with u-b full = %v, %v; want u-a", got, err)
	}
	p.Release(p.Get("u-b"))
	if got, err := p.Pick("", "session-1", nil); err != nil || got.Auth.UID != "u-b" {
		t.Fatalf("Pick(session-1) after release = %v, %v; want u-b", got, err)
	}

	p.BindSession("session-1", "u-a")
	if got, err := p.Pick("", "session-1", nil); err != nil || got.Auth.UID != "u-a" {
		t.Fatalf("Pick(session-1) after rebind = %v, %v; want u-a", got, err)
	}
}

func TestPickPrefersModelCapabilityAndSkipsGuards(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-capable", "", poolExpiry(now), nil)
	writePoolAuth(t, dir, "u-other", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})

	capable := p.Get("u-capable")
	other := p.Get("u-other")
	capable.Models = []providers.Model{{"id": "m1"}}
	other.Models = []providers.Model{{"id": "m2"}}

	if !capable.HasModel("m1") || capable.HasModel("m2") {
		t.Fatal("HasModel did not respect the account catalog")
	}
	if !other.HasModel("") {
		t.Fatal("an empty model must always be accepted")
	}
	if other.HasModel("m1") {
		t.Fatal("u-other claims m1")
	}

	// Capability wins over recency.
	if !p.Acquire(other) {
		t.Fatal("acquire other")
	}
	p.Release(other)
	got, err := p.Pick("m1", "", nil)
	if err != nil || got != capable {
		t.Fatalf("Pick(m1) = %v, %v; want u-capable", got, err)
	}

	// Guards exclude even a capable account.
	capable.DailyTokenLimit = 10
	capable.DailyTokensToday = int64Pointer(10)
	other.DailyTokenLimit = 10
	other.DailyTokensToday = int64Pointer(10)
	if _, err := p.Pick("m1", "", nil); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Pick with guarded capable account = %v, want ErrNoAccount", err)
	}
}

func TestApplyLimitsAndGuards(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-guards", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	a := p.Get("u-guards")
	a.Credits = &CreditsSnapshot{Remain: 100}

	p.ApplyLimits(
		map[string]any{
			"reserve_credits":         map[string]any{"global": 30},
			"daily_credit_limit":      map[string]any{"global": 10},
			"daily_token_limit":       map[string]any{"global": 100},
			"model_daily_token_limit": map[string]any{"global": 50},
		},
		map[string]any{
			"tokens":  map[string]any{"u-guards": 120},
			"credits": map[string]any{"u-guards": 10.5},
			"models":  map[string]any{"u-guards": map[string]any{"m1": 60, "m2": 1}},
		},
	)

	if a.ReserveCredits != 30 || a.DailyCreditLimit != 10 || a.DailyTokenLimit != 100 || a.ModelDailyTokenLimit != 50 {
		t.Fatalf("limits = %d/%d/%d/%d", a.ReserveCredits, a.DailyCreditLimit, a.DailyTokenLimit, a.ModelDailyTokenLimit)
	}
	if a.DailyTokensToday == nil || *a.DailyTokensToday != 120 {
		t.Fatalf("DailyTokensToday = %v, want 120", a.DailyTokensToday)
	}
	if a.DailyCreditsToday == nil || *a.DailyCreditsToday != 10.5 {
		t.Fatalf("DailyCreditsToday = %v, want 10.5", a.DailyCreditsToday)
	}
	if !reflect.DeepEqual(a.ModelDailyTokens, map[string]int64{"m1": 60, "m2": 1}) {
		t.Fatalf("ModelDailyTokens = %#v", a.ModelDailyTokens)
	}

	if !a.DailyLimitBlocked() {
		t.Fatal("daily token limit should be reached")
	}
	if got := a.GuardBlocked("m2"); got != "daily_token" {
		t.Fatalf("GuardBlocked = %q, want daily_token", got)
	}
	if view := a.View(); !view.DailyLimitBlocked || view.Guard != "daily_token" || view.DailyTokensToday == nil || *view.DailyTokensToday != 120 {
		t.Fatalf("daily guard view = %+v", view)
	}
	if a.Ready(now, "m2") {
		t.Fatal("Ready must be false when a guard is active")
	}

	a.DailyTokensToday = int64Pointer(0)
	if got := a.GuardBlocked("m1"); got != "model_daily_token" {
		t.Fatalf("GuardBlocked = %q, want model_daily_token", got)
	}
	a.ModelDailyTokenLimit = 0
	if got := a.GuardBlocked("m1"); got != "daily_credit" {
		t.Fatalf("GuardBlocked = %q, want daily_credit", got)
	}
	if view := a.View(); !view.CreditLimitReached || view.Guard != "daily_credit" {
		t.Fatalf("credit guard view = %+v", view)
	}
	a.DailyCreditLimit = 0
	if a.GuardBlocked("m1") != "" || !a.Ready(now, "m1") {
		t.Fatalf("account should be ready: %+v", a.View())
	}

	a.Credits = &CreditsSnapshot{Remain: 20}
	a.ReserveCredits = 20
	if !a.ReserveBlocked() {
		t.Fatal("balanced reserve should block")
	}
	if got := a.GuardBlocked(""); got != "reserve" {
		t.Fatalf("GuardBlocked = %q, want reserve", got)
	}

	a.ReserveCredits = 0
	a.State.CoolUntil = now.Add(time.Minute)
	if a.Ready(now, "m1") {
		t.Fatal("cooling account must not be ready")
	}
	if got := a.GuardBlocked("m1"); got != "" {
		t.Fatalf("GuardBlocked while cooling = %q, want empty", got)
	}

	a.State.CoolUntil = time.Time{}
	p.ApplyLimits(nil, nil)
	if a.ReserveCredits != 0 || a.DailyCreditLimit != 0 || a.DailyTokenLimit != 0 || a.ModelDailyTokenLimit != 0 {
		t.Fatal("ApplyLimits(nil) did not reset limits")
	}
	if a.DailyTokensToday == nil || *a.DailyTokensToday != 0 || a.DailyCreditsToday == nil || *a.DailyCreditsToday != 0 {
		t.Fatalf("usage was not reset to zero: %v / %v", a.DailyTokensToday, a.DailyCreditsToday)
	}
}

func TestViewKeysRedactionAndProxyMasking(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	expiry := now.Add(90 * time.Second)
	dir := t.TempDir()
	path := writePoolAuth(t, dir, "u-view", "", expiry, map[string]any{
		"proxySlot": "slot-1",
		"code":      "SECRETCODE123",
	})
	p := mustPool(t, dir, Config{MaxConcurrent: 2})
	p.SetProxySlots([]transport.Slot{{
		ID:      "slot-1",
		Name:    "proxy",
		URL:     "http://user:password@proxy.example:8080",
		Enabled: true,
	}})

	a := p.Get("u-view")
	accessToken := a.Auth.AccessToken
	a.State.Reason = "refresh rejected " + accessToken
	a.State.LastError = "Bearer " + accessToken + " code=SECRETCODE123"
	a.State.ErrCount = 2
	a.Credits = &CreditsSnapshot{Available: 12, Remain: 5, Unit: "points"}
	a.PointsTasks = &PointsTasks{}
	view := a.viewAt(now)

	if view.Name != "u-view" || view.UID != "u-view" || view.Realm != "raccoon" {
		t.Fatalf("identity fields = %+v", view)
	}
	if view.Nickname != "Org u-view" {
		t.Fatalf("Nickname = %q", view.Nickname)
	}
	if view.ProxySlot != "slot-1" {
		t.Fatalf("ProxySlot = %q", view.ProxySlot)
	}
	if view.Proxy != "http://<redacted>@proxy.example:8080" {
		t.Fatalf("Proxy = %q, proxy credentials were not masked", view.Proxy)
	}
	if view.ErrCount != 2 || view.ActiveConcurrent != 0 || view.MaxConcurrent != 2 {
		t.Fatalf("counters = %+v", view)
	}
	if view.TokenRemaining != "1m" {
		t.Fatalf("TokenRemaining = %q, want 1m", view.TokenRemaining)
	}
	if want := expiry.In(time.Local).Format("2006-01-02T15:04:05"); view.ExpiresAt != want {
		t.Fatalf("ExpiresAt = %q, want %q", view.ExpiresAt, want)
	}
	if view.Cooling || view.Until != "" {
		t.Fatalf("cooling fields = %v/%q, want false/empty", view.Cooling, view.Until)
	}
	if strings.Contains(view.Reason, accessToken) || strings.Contains(view.LastError, accessToken) {
		t.Fatalf("view leaked a token: reason=%q last_error=%q", view.Reason, view.LastError)
	}
	if strings.Contains(view.LastError, "SECRETCODE123") {
		t.Fatalf("view leaked the authorization code: %q", view.LastError)
	}
	if !strings.Contains(view.Reason, "<redacted>") || !strings.Contains(view.LastError, "<redacted>") {
		t.Fatalf("view did not redact: reason=%q last_error=%q", view.Reason, view.LastError)
	}
	if !view.TasksEnabled {
		t.Fatal("new account should start with tasks_enabled=true")
	}
	if view.Models != 0 || view.Credits == nil || view.PointsTasks == nil {
		t.Fatalf("payload fields = %+v", view)
	}

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, accessToken) || strings.Contains(text, "refresh-u-view") || strings.Contains(text, "user:password") || strings.Contains(text, "SECRETCODE123") {
		t.Fatalf("marshaled view leaked credentials: %s", text)
	}
	for _, key := range []string{
		"name", "uid", "nickname", "realm", "disabled", "cooling", "until",
		"err_count", "reason", "last_error", "active_concurrent",
		"max_concurrent", "token_remaining", "expires_at", "models", "credits",
		"points_tasks", "guard", "reserve_credits", "reserve_blocked",
		"daily_token_limit", "daily_tokens_today", "daily_limit_blocked",
		"daily_credit_limit", "daily_credits_today", "credit_limit_reached",
		"proxy_slot", "proxy", "note", "tasks_enabled",
	} {
		if !strings.Contains(text, `"`+key+`":`) {
			t.Fatalf("view is missing key %q: %s", key, text)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if recent, ok := decoded["credits"].(map[string]any)["recent"].([]any); !ok || len(recent) != 0 {
		t.Fatalf("credits.recent = %#v, want []", decoded["credits"])
	}
	if tasks, ok := decoded["points_tasks"].(map[string]any); !ok {
		t.Fatalf("points_tasks = %#v", decoded["points_tasks"])
	} else if _, ok := tasks["new_user"].([]any); !ok {
		t.Fatalf("points_tasks.new_user = %#v, want []", tasks["new_user"])
	}

	a.State.CoolUntil = now.Add(10 * time.Minute)
	cooling := a.viewAt(now)
	if !cooling.Cooling {
		t.Fatal("Cooling should be true")
	}
	if want := a.State.CoolUntil.In(time.Local).Format("2006-01-02T15:04:05"); cooling.Until != want {
		t.Fatalf("Until = %q, want %q", cooling.Until, want)
	}
	a.State.Disabled = true
	if disabled := a.viewAt(now); disabled.Cooling || disabled.Until != "" {
		t.Fatalf("disabled account must not report cooling: %+v", disabled)
	}

	// The persisted path is the file the pool loaded, not an alias.
	if a.Path != path {
		t.Fatalf("Path = %q, want %q", a.Path, path)
	}
}

func TestStateMigrationAndUnknownFieldsPreserved(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-legacy", "", poolExpiry(now), nil)
	writePoolAuth(t, dir, "u-default", "", poolExpiry(now), nil)
	writePoolAuth(t, dir, "u-off", "", poolExpiry(now), nil)
	writePoolAuth(t, dir, "u-odd", "", poolExpiry(now), nil)
	statePath := writePoolState(t, dir, `{
	  "u-legacy": {
	    "disabled": true,
	    "reason": "manual disable",
	    "cool_until": 1700000000.0,
	    "err_count": 2,
	    "last_error": "boom",
	    "note": "  keep this  ",
	    "active_concurrent": 3,
	    "future_state": {"keep": [1, 2], "n": 7}
	  },
	  "u-default": {"disabled": false},
	  "u-off": {"tasks_enabled": false},
	  "u-odd": {"manual_disabled": true, "disabled": false}
	}`)

	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	legacy := p.Get("u-legacy")
	if !legacy.State.Disabled || !legacy.State.ManualDisabled {
		t.Fatalf("legacy manual disable was not inferred: %+v", legacy.State)
	}
	if legacy.State.DisabledBy != "manual" {
		t.Fatalf("DisabledBy = %q, want manual", legacy.State.DisabledBy)
	}
	if legacy.State.ErrCount != 2 || legacy.State.LastError != "boom" {
		t.Fatalf("error state changed: %+v", legacy.State)
	}
	if !legacy.State.CoolUntil.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("CoolUntil = %v", legacy.State.CoolUntil)
	}
	if legacy.State.Note != "  keep this  " {
		t.Fatalf("Note = %q, want the v1 note preserved without trimming", legacy.State.Note)
	}
	if !legacy.State.TasksEnabled {
		t.Fatal("TasksEnabled should default to true when the key is absent")
	}
	if legacy.State.ActiveConcurrent != 0 || legacy.View().ActiveConcurrent != 0 {
		t.Fatal("ActiveConcurrent should not be restored from disk")
	}
	if !p.Get("u-default").State.TasksEnabled {
		t.Fatal("TasksEnabled should default to true with no state entry")
	}
	if p.Get("u-off").State.TasksEnabled {
		t.Fatal("an explicit tasks_enabled=false was not preserved")
	}
	odd := p.Get("u-odd")
	if !odd.State.ManualDisabled || !odd.State.Disabled || odd.State.DisabledBy != "manual" {
		t.Fatalf("inconsistent manual state was not repaired: %+v", odd.State)
	}

	if err := p.SaveState(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	entry := raw["u-legacy"]
	if entry == nil {
		t.Fatal("u-legacy state was dropped")
	}
	if _, ok := entry["future_state"]; !ok {
		t.Fatalf("unknown state field was dropped: %s", data)
	}
	var manual bool
	if err := json.Unmarshal(entry["manual_disabled"], &manual); err != nil || !manual {
		t.Fatalf("manual_disabled = %s, %v", entry["manual_disabled"], err)
	}
	var by string
	if err := json.Unmarshal(entry["disabled_by"], &by); err != nil || by != "manual" {
		t.Fatalf("disabled_by = %s, %v", entry["disabled_by"], err)
	}
	var active int
	if err := json.Unmarshal(entry["active_concurrent"], &active); err != nil || active != 0 {
		t.Fatalf("active_concurrent = %s, %v; want 0", entry["active_concurrent"], err)
	}
	var tasks bool
	if err := json.Unmarshal(entry["tasks_enabled"], &tasks); err != nil || !tasks {
		t.Fatalf("tasks_enabled = %s, %v; want true", entry["tasks_enabled"], err)
	}
	var extra map[string]any
	if err := json.Unmarshal(entry["future_state"], &extra); err != nil {
		t.Fatalf("future_state = %s: %v", entry["future_state"], err)
	}
	if extra["n"] != float64(7) {
		t.Fatalf("future_state = %#v", extra)
	}
	var saved map[string]accountstate.State
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("saved state is not loadable: %v", err)
	}
	if !saved["u-legacy"].ManualDisabled {
		t.Fatalf("saved state lost manual_disabled: %+v", saved["u-legacy"])
	}
}

func TestStateFileShapeSafety(t *testing.T) {
	now := time.Now()
	shapes := map[string]string{
		"list":             `[{"uid":"u-one","disabled":true}]`,
		"legacy accounts":  `{"accounts":[{"uid":"u-one","disabled":true}]}`,
		"non-object entry": `{"u-one":[]}`,
		"malformed":        `{`,
		"null":             `null`,
	}
	for name, body := range shapes {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writePoolAuth(t, dir, "u-one", "", poolExpiry(now), nil)
			statePath := writePoolState(t, dir, body)
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewPool(dir, Config{MaxConcurrent: 1}); err == nil {
				t.Fatal("NewPool accepted an unsupported state file shape")
			}
			after, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("a rejected state file was modified")
			}
		})
	}

	// A missing state file is the normal fresh-install case.
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-one", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	if p.Get("u-one") == nil {
		t.Fatal("fresh pool did not load its auth file")
	}
}

func TestMalformedSafetyStateFailsClosedThroughPool(t *testing.T) {
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-safety", "", poolExpiry(time.Now()), nil)
	statePath := writePoolState(t, dir, `{"u-safety":{"disabled":"false","manual_disabled":0}}`)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	account := p.Get("u-safety")
	if account == nil {
		t.Fatal("account disappeared")
	}
	account.mu.Lock()
	state := account.State
	account.mu.Unlock()
	if state.Healthy(time.Now()) || !state.Disabled || !state.ManualDisabled {
		t.Fatalf("malformed safety fields opened the account: %+v", state)
	}
	if err := p.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("fail-closed state was not rewritten safely")
	}
	if strings.Contains(string(after), `"disabled":"false"`) {
		t.Fatalf("malformed safety value survived save: %s", after)
	}
}

func TestStateForMissingAuthFileIsPreserved(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-live", "", poolExpiry(now), nil)
	writePoolState(t, dir, `{
	  "u-gone": {
	    "disabled": true,
	    "manual_disabled": true,
	    "disabled_by": "manual",
	    "reason": "manual disable",
	    "note": "keep me",
	    "tasks_enabled": false,
	    "future_state": {"keep": true}
	  }
	}`)

	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	if err := p.Disable("u-live", "manual disable"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	gone := saved["u-gone"]
	if gone == nil {
		t.Fatalf("state for a temporarily missing auth file was wiped: %s", data)
	}
	if _, ok := gone["future_state"]; !ok {
		t.Fatalf("unknown field was wiped: %s", data)
	}
	var note string
	if err := json.Unmarshal(gone["note"], &note); err != nil || note != "keep me" {
		t.Fatalf("note = %s, %v", gone["note"], err)
	}

	// Re-importing the account restores its preserved state.
	token := makeUnsignedJWT(t, map[string]any{"sub": "u-gone", "exp": poolExpiry(now).Unix()})
	restored, err := p.Add(Auth{
		UID:          "u-gone",
		AccessToken:  token,
		RefreshToken: "refresh-u-gone",
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.State.Note != "keep me" || !restored.State.ManualDisabled || !restored.State.Disabled {
		t.Fatalf("preserved state was not restored: %+v", restored.State)
	}
	if restored.State.TasksEnabled {
		t.Fatal("preserved tasks_enabled=false was not restored")
	}
}

func TestSaveStateRefusesChangedStateShape(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-live", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})

	statePath := writePoolState(t, dir, `[{"uid":"u-live","note":"external writer"}]`)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Disable("u-live", "manual disable"); err == nil {
		t.Fatal("SaveState overwrote a list-shaped state file")
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a refused state save modified the file")
	}
}

func TestLoadAuthFilesDeterministicAndSafe(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	canonical := writePoolAuth(t, dir, "u-a", "", poolExpiry(now), nil)
	writePoolAuth(t, dir, "u-b", "", poolExpiry(now), nil)
	// Duplicate UID under an earlier-sorting alias: the canonical path wins.
	writePoolAuth(t, dir, "u-a", "raccoon-0alias.json", poolExpiry(now), map[string]any{"office_org_name": "Alias Org"})
	// A valid document whose filename does not match its embedded UID: the
	// embedded UID is authoritative, not the filename.
	writePoolAuth(t, dir, "u-c", "raccoon-alias.json", poolExpiry(now), nil)

	folder := filepath.Join(dir, "raccoon")
	if err := os.WriteFile(filepath.Join(folder, "raccoon-broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "raccoon-notoken.json"), []byte(`{"uid":"u-x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "not-an-auth.json"), []byte(`{"uid":"u-y","access_token":"opaque-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	list := p.List()
	if len(list) != 3 {
		t.Fatalf("loaded %d accounts, want 3: %+v", len(list), list)
	}
	if list[0].UID != "u-a" || list[1].UID != "u-b" || list[2].UID != "u-c" {
		t.Fatalf("account order = %q, %q, %q", list[0].UID, list[1].UID, list[2].UID)
	}
	if p.Get("u-a").Path != canonical {
		t.Fatalf("duplicate UID loaded %q, want canonical %q", p.Get("u-a").Path, canonical)
	}
	if p.Get("u-a").Auth.OfficeOrgName != "Org u-a" {
		t.Fatal("the alias, not the canonical file, was loaded")
	}
	if p.Get("u-x") != nil || p.Get("u-y") != nil {
		t.Fatal("malformed or non-raccoon auth files were loaded")
	}
	if got := p.Get("u-c"); got == nil || !strings.HasSuffix(got.Path, "raccoon-alias.json") {
		t.Fatalf("embedded UID alias was not loaded: %+v", got)
	}
}

func TestAddRefusesAuthPathCollision(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	existingPath := writePoolAuth(t, dir, "a_b", "", poolExpiry(now), nil)
	before, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatal(err)
	}

	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	token := makeUnsignedJWT(t, map[string]any{"sub": "a/b", "exp": poolExpiry(now).Unix()})
	_, err = p.Add(Auth{
		UID:          "a/b",
		AccessToken:  token,
		RefreshToken: "refresh-a/b",
		ExpiresAt:    poolExpiry(now),
	})
	if err == nil {
		t.Fatal("Add overwrote a different account's auth file")
	}
	if !errors.Is(err, ErrAuthPathCollision) {
		t.Fatalf("Add error = %v, want ErrAuthPathCollision", err)
	}
	after, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("colliding Add modified the existing auth file")
	}
	if p.Get("a/b") != nil {
		t.Fatal("colliding Add created an account")
	}
	if p.Get("a_b") == nil || p.Get("a_b").Auth.OfficeOrgName != "Org a_b" {
		t.Fatal("existing account changed")
	}
}

func TestRemoveDoesNotDeleteCollidingCanonicalAuth(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	otherPath := writePoolAuth(t, dir, "a_b", "", poolExpiry(now), nil)
	aliasPath := writePoolAuth(t, dir, "a/b", "raccoon-a-slash.json", poolExpiry(now), nil)

	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	if p.Get("a_b") == nil || p.Get("a/b") == nil {
		t.Fatal("colliding accounts were not both loaded")
	}
	if err := p.Remove("a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatalf("Remove deleted a different UID's canonical auth file: %v", err)
	}
	if _, err := os.Stat(aliasPath); !os.IsNotExist(err) {
		t.Fatalf("Remove left the account's alias auth file: %v", err)
	}
	if p.Get("a_b") == nil {
		t.Fatal("Remove removed the wrong account")
	}
}

func TestSetProxySlotPersistsAndResolves(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-proxy", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	p.SetProxySlots([]transport.Slot{
		{ID: "slot-1", URL: "http://u:p@one.example:8080", Enabled: true},
		{ID: "slot-off", URL: "http://two.example:8080", Enabled: false},
	})

	if err := p.SetProxySlot("u-proxy", "slot-1"); err != nil {
		t.Fatal(err)
	}
	a := p.Get("u-proxy")
	if view := a.View(); view.ProxySlot != "slot-1" || view.Proxy != "http://<redacted>@one.example:8080" {
		t.Fatalf("view after bind = %+v", view)
	}
	if got := p.ProxyURL(a); got != "http://u:p@one.example:8080" {
		t.Fatalf("ProxyURL = %q", got)
	}
	loaded, err := LoadAuth(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProxySlot != "slot-1" {
		t.Fatalf("persisted proxySlot = %q", loaded.ProxySlot)
	}

	if err := p.SetProxySlot("u-proxy", "slot-off"); err != nil {
		t.Fatal(err)
	}
	if got := a.View().Proxy; got != "" {
		t.Fatalf("disabled slot proxy = %q, want empty", got)
	}
	if err := p.SetProxySlot("u-proxy", ""); err != nil {
		t.Fatal(err)
	}
	if got := a.View().ProxySlot; got != "" {
		t.Fatalf("cleared proxySlot = %q", got)
	}
	if err := p.SetProxySlot("missing", "slot-1"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("SetProxySlot missing = %v", err)
	}
}

func TestNoteErrorAndSuccess(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-note", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	a := p.Get("u-note")

	if err := p.NoteError(a, "upstream said "+a.Auth.AccessToken, false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(a.State.LastError, a.Auth.AccessToken) || !strings.Contains(a.State.LastError, "<redacted>") {
		t.Fatalf("LastError = %q", a.State.LastError)
	}
	if a.State.ErrCount != 1 {
		t.Fatalf("ErrCount = %d", a.State.ErrCount)
	}
	if err := p.NoteError(a, "soft failure", true, false); err != nil {
		t.Fatal(err)
	}
	if !a.State.CoolUntil.After(now) {
		t.Fatalf("soft failure did not set a cooldown: %v", a.State.CoolUntil)
	}
	if err := p.NoteError(a, "terminal failure", false, true); err != nil {
		t.Fatal(err)
	}
	if !a.State.Disabled || a.State.ManualDisabled || a.State.DisabledBy != "auth" {
		t.Fatalf("terminal failure state = %+v", a.State)
	}

	if err := p.NoteSuccess(a); err != nil {
		t.Fatal(err)
	}
	if a.State.ErrCount != 0 || a.State.LastError != "" || !a.State.CoolUntil.IsZero() {
		t.Fatalf("NoteSuccess did not clear transient state: %+v", a.State)
	}

	if err := p.Disable("u-note", "manual disable"); err != nil {
		t.Fatal(err)
	}
	if err := p.NoteError(a, "another terminal error", false, true); err != nil {
		t.Fatal(err)
	}
	if !a.State.ManualDisabled || a.State.Reason != "manual disable" || a.State.DisabledBy != "manual" {
		t.Fatalf("terminal error changed a manual disable: %+v", a.State)
	}
}

func TestPoolMutationsPersistAndAddRemove(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	p := mustPool(t, dir, Config{MaxConcurrent: 1})

	token := makeUnsignedJWT(t, map[string]any{"sub": "u-added", "exp": poolExpiry(now).Unix()})
	added, err := p.Add(Auth{
		UID:           "u-added",
		AccessToken:   token,
		RefreshToken:  "refresh-u-added",
		OfficeOrgName: "Added Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	if added.Auth.UID != "u-added" || !added.State.TasksEnabled {
		t.Fatalf("added account = %+v", added)
	}
	if p.Get("u-added") != added {
		t.Fatal("Get did not return the added account")
	}
	reloaded := mustPool(t, dir, Config{MaxConcurrent: 1})
	if reloaded.Get("u-added") == nil {
		t.Fatal("added account did not persist")
	}

	if err := p.SetNote("u-added", "  a note  "); err != nil {
		t.Fatal(err)
	}
	if got := p.Get("u-added").State.Note; got != "a note" {
		t.Fatalf("Note = %q", got)
	}
	if err := p.SetTasksEnabled("u-added", false); err != nil {
		t.Fatal(err)
	}
	if p.Get("u-added").State.TasksEnabled {
		t.Fatal("SetTasksEnabled(false) was not applied")
	}
	if err := p.ClearCooldown("u-added"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNote("missing", "x"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("SetNote missing = %v", err)
	}

	if err := p.Remove("u-added"); err != nil {
		t.Fatal(err)
	}
	if p.Get("u-added") != nil {
		t.Fatal("Remove left the account in the pool")
	}
	if _, err := os.Stat(added.Path); !os.IsNotExist(err) {
		t.Fatalf("Remove left the auth file: %v", err)
	}
	if err := p.Disable("u-added", "x"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("Disable after Remove = %v", err)
	}
}

func TestAccountPublicPredicates(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-pred", "", now.Add(90*time.Second), map[string]any{"proxySlot": "slot-1"})
	p := mustPool(t, dir, Config{MaxConcurrent: 3})
	a := p.Get("u-pred")

	if !a.Healthy(now) {
		t.Fatal("a fresh account should be healthy")
	}
	if !a.Ready(now, "") {
		t.Fatal("a fresh account should be ready")
	}
	if a.ExpiresAt().IsZero() || a.TokenRemaining(now) != 90*time.Second {
		t.Fatalf("token timing: expires=%v remaining=%v", a.ExpiresAt(), a.TokenRemaining(now))
	}
	if a.ProxySlot() != "slot-1" {
		t.Fatalf("ProxySlot = %q", a.ProxySlot())
	}

	a.State.CoolUntil = now
	if !a.Healthy(now) {
		t.Fatal("a cooldown ending exactly now should be healthy")
	}
	a.State.CoolUntil = now.Add(time.Second)
	if a.Healthy(now) || a.Ready(now, "") {
		t.Fatal("a future cooldown must not be healthy/ready")
	}
	a.State.CoolUntil = time.Time{}

	a.State.ManualDisabled = true
	if a.Healthy(now) {
		t.Fatal("ManualDisabled must imply not healthy")
	}
	a.State.ManualDisabled = false

	a.ModelDailyTokenLimit = 10
	a.ModelDailyTokens = map[string]int64{"m1": 10}
	if !a.ModelDailyLimitBlocked("m1") || a.ModelDailyLimitBlocked("m2") {
		t.Fatal("model daily guard mismatch")
	}
	if a.CreditLimitReached() || a.CreditLimitBlocked("any-model") {
		t.Fatal("credit guard should be off with no limit")
	}
	a.DailyCreditLimit = 5
	a.DailyCreditsToday = float64Pointer(5)
	if !a.CreditLimitReached() || !a.CreditLimitBlocked("any-model") {
		t.Fatal("credit guard mismatch")
	}

	a.DailyTokenLimit = 100
	a.DailyTokensToday = int64Pointer(99)
	if a.DailyLimitBlocked() {
		t.Fatal("daily guard blocked below the limit")
	}
	a.DailyTokensToday = int64Pointer(100)
	if !a.DailyLimitBlocked() {
		t.Fatal("daily guard did not block at the limit")
	}

	if a.TokenRemaining(now.Add(2*time.Hour)) != 0 {
		t.Fatal("an expired token should have zero remaining time")
	}
	var nilAccount *Account
	if nilAccount.Healthy(now) || nilAccount.Ready(now, "") || nilAccount.ReserveBlocked() || nilAccount.GuardBlocked("") != "" {
		t.Fatal("nil account predicates should be false/empty")
	}
	if nilAccount.View().UID != "" || nilAccount.ProxySlot() != "" || nilAccount.TokenRemaining(now) != 0 {
		t.Fatal("nil account view helpers should be empty")
	}
}

func TestAddReimportPreservesManualState(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-reimport", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})

	if err := p.Disable("u-reimport", "manual disable"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNote("u-reimport", "keep note"); err != nil {
		t.Fatal(err)
	}

	token := makeUnsignedJWT(t, map[string]any{"sub": "u-reimport", "exp": poolExpiry(now).Unix()})
	existing, err := p.Add(Auth{
		UID:           "u-reimport",
		AccessToken:   token,
		RefreshToken:  "refresh-new",
		OfficeOrgName: "New Org",
	})
	if err != nil {
		t.Fatal(err)
	}
	if existing.State.Note != "keep note" {
		t.Fatalf("note lost on re-import: %q", existing.State.Note)
	}
	if !existing.State.ManualDisabled || !existing.State.Disabled || existing.State.Reason != "manual disable" {
		t.Fatalf("manual disable lost on re-import: %+v", existing.State)
	}
	if existing.Auth.OfficeOrgName != "New Org" || existing.Auth.RefreshToken != "refresh-new" {
		t.Fatalf("credentials were not replaced: %+v", existing.Auth)
	}

	// A non-manual account recovers on re-import.
	if err := p.Enable("u-reimport"); err != nil {
		t.Fatal(err)
	}
	existing.State.ErrCount = 2
	existing.State.LastError = "old"
	existing.State.Reason = "old reason"
	if _, err := p.Add(Auth{
		UID:          "u-reimport",
		AccessToken:  token,
		RefreshToken: "refresh-newer",
	}); err != nil {
		t.Fatal(err)
	}
	if existing.State.Disabled || existing.State.ErrCount != 0 || existing.State.LastError != "" || existing.State.Reason != "" {
		t.Fatalf("re-import did not clear non-manual error state: %+v", existing.State)
	}
}

func TestTokenRefreshPreservesPoolManualDisable(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-refresh", "", now.Add(60*time.Second), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})

	newToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-refresh",
		"exp": now.Add(time.Hour).Unix(),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"access_token": newToken},
		})
	}))
	defer server.Close()

	tm := p.NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
		BaseURL: server.URL,
		Now:     func() time.Time { return now },
		Timeout: 2 * time.Second,
	})

	if err := p.Disable("u-refresh", "manual disable"); err != nil {
		t.Fatal(err)
	}
	account := p.Get("u-refresh")

	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = account.View()
				_ = p.Stats()
				_ = p.List()
			}
		}
	}()

	if err := tm.Refresh(context.Background(), account); err != nil {
		close(done)
		wg.Wait()
		t.Fatalf("Refresh: %v", err)
	}
	close(done)
	wg.Wait()

	if account.Auth.AccessToken != newToken {
		t.Fatal("refresh did not publish the new access token")
	}
	if !account.State.ManualDisabled || !account.State.Disabled || account.State.Reason != "manual disable" {
		t.Fatalf("refresh cleared a manual disable: %+v", account.State)
	}
	if view := account.View(); !view.Disabled || view.Reason != "manual disable" {
		t.Fatalf("view after refresh = %+v", view)
	}
	reloaded := mustPool(t, dir, Config{MaxConcurrent: 1})
	if !reloaded.Get("u-refresh").State.ManualDisabled {
		t.Fatal("manual disable did not persist across refresh and reload")
	}
}

func TestTokenRefreshRemapsAffinityAndStateUID(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-old", "", now.Add(60*time.Second), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})

	original := p.Get("u-old")
	p.BindSession("session-rotate", "u-old")

	newToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-new",
		"exp": now.Add(time.Hour).Unix(),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"access_token": newToken},
		})
	}))
	defer server.Close()

	tm := p.NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
		BaseURL: server.URL,
		Now:     func() time.Time { return now },
		Timeout: 2 * time.Second,
	})

	if err := tm.Refresh(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	if p.Get("u-old") != nil {
		t.Fatal("the old UID is still resolvable after the JWT subject rotated")
	}
	if got := p.Get("u-new"); got != original {
		t.Fatal("UID rotation replaced the canonical account pointer")
	}
	if original.State.UID != "u-new" || original.View().UID != "u-new" {
		t.Fatalf("runtime UID was not updated: state=%q view=%q", original.State.UID, original.View().UID)
	}
	if got, err := p.Pick("", "session-rotate", nil); err != nil || got != original {
		t.Fatalf("session affinity was not remapped: %v, %v", got, err)
	}

	data, err := os.ReadFile(p.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved["u-new"]; !ok {
		t.Fatalf("state was not saved under the rotated UID: %s", data)
	}
	if _, ok := saved["u-old"]; ok {
		t.Fatalf("state still contains the old UID: %s", data)
	}
}

func TestPoolStatsAndSetConfig(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-disabled", "", poolExpiry(now), nil)
	writePoolAuth(t, dir, "u-cooling", "", poolExpiry(now), nil)
	writePoolAuth(t, dir, "u-limited", "", poolExpiry(now), nil)
	writePoolAuth(t, dir, "u-healthy", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})

	if err := p.Disable("u-disabled", "manual disable"); err != nil {
		t.Fatal(err)
	}
	p.Get("u-cooling").State.CoolUntil = now.Add(time.Hour)
	p.Get("u-limited").ReserveCredits = 1
	p.Get("u-limited").Credits = &CreditsSnapshot{Remain: 1}

	stats := p.Stats()
	want := PoolStats{Total: 4, Healthy: 1, Disabled: 1, Cooling: 1, Limited: 1}
	if stats != want {
		t.Fatalf("Stats = %+v, want %+v", stats, want)
	}

	p.SetConfig(Config{MaxConcurrent: 5})
	for _, a := range p.Accounts() {
		if a.View().MaxConcurrent != 5 {
			t.Fatalf("%s MaxConcurrent = %d", a.Auth.UID, a.View().MaxConcurrent)
		}
	}
}
