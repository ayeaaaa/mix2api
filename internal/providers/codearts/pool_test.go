package codearts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func testAuth(uid string) Auth {
	return Auth{
		UserID:           uid,
		UserName:         "User " + uid,
		DomainID:         "domain-" + uid,
		CloudDragonToken: "cloud-token-" + uid,
		AccessKeyID:      "AK-" + uid,
		SecretAccessKey:  "secret-key-" + uid,
		RefreshToken:     "refresh-token-" + uid,
		Expiration:       time.Now().Add(time.Hour),
		UpdatedAt:        1760000000,
	}
}

func writeTestAuth(t *testing.T, dir, uid, filename string) string {
	t.Helper()
	path := filename
	if path == "" {
		canonical, err := AuthPath(dir, uid)
		if err != nil {
			t.Fatal(err)
		}
		path = canonical
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(dir, "codearts", path)
	}
	if err := SaveAuth(path, testAuth(uid)); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestStateFile(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "codearts", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestPool(t *testing.T, dir string, cfg Config) *Pool {
	t.Helper()
	p, err := NewPool(dir, cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return p
}

func accountInfo(remain, expiring float64) *AccountInfo {
	return &AccountInfo{
		IsCreditPackage: true,
		PackageName:     "体验版",
		EndDate:         "2027-01-01",
		Credits: AccountCredits{
			Total: &CreditTotal{
				Amount:   100,
				Used:     100 - remain,
				Remain:   remain,
				Expiring: expiring,
			},
			Unit: "credit",
		},
		FetchedAt: 1760000000,
	}
}

func poolUID(t *testing.T, a *Account) string {
	t.Helper()
	if a == nil {
		t.Fatal("account is nil")
	}
	return strings.TrimSpace(a.Auth.UserID)
}

func TestPoolLoadsStateAndPreservesManualDisableAndUnknownFields(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u1", "")
	writeTestAuth(t, dir, "u2", "")
	writeTestStateFile(t, dir, `[
		{"name":"u1","err_count":2,"cool_until":0,"disabled":true,
		 "manual_disabled":true,"reason":"manual disable","last_error":"boom",
		 "checkin":{"ok":true,"checked_at":"2026-01-01T10:00:00","credits":100},
		 "note":"keep note","tasks_enabled":false,"future_field":{"nested":[1,2]}},
		{"name":"u-missing","disabled":true,"manual_disabled":true,
		 "note":"orphan note","future_orphan":true}
	]`)

	p := newTestPool(t, dir, Config{MaxConcurrent: 2})
	if got := len(p.Accounts()); got != 2 {
		t.Fatalf("loaded %d accounts, want 2", got)
	}
	if p.Get("u-missing") != nil {
		t.Fatal("state-only account must not appear in the live pool")
	}

	u1 := p.Get("u1")
	if u1 == nil {
		t.Fatal("u1 missing")
	}
	if !u1.State.Disabled || !u1.State.ManualDisabled {
		t.Fatalf("manual disable lost on load: %+v", u1.State)
	}
	if u1.State.Note != "keep note" || u1.State.TasksEnabled {
		t.Fatalf("state fields lost on load: %+v", u1.State)
	}
	if ok, _ := u1.Checkin["ok"].(bool); !ok || u1.Checkin["checked_at"] != "2026-01-01T10:00:00" {
		t.Fatalf("checkin lost on load: %#v", u1.Checkin)
	}

	if err := p.SetNote("u1", "updated note"); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(dir, "codearts", "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data))[0] != '[' {
		t.Fatalf("state was not written as the v1 list shape: %s", data)
	}
	var rawEntries []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawEntries); err != nil {
		t.Fatal(err)
	}
	for _, entry := range rawEntries {
		var name string
		if err := json.Unmarshal(entry["name"], &name); err != nil || name != "u1" {
			continue
		}
		rawCool := bytes.TrimSpace(entry["cool_until"])
		if len(rawCool) == 0 || rawCool[0] == '"' {
			t.Fatalf("cool_until was not saved as a number: %s", entry["cool_until"])
		}
	}
	states, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	saved := states["u1"]
	if !saved.Disabled || !saved.ManualDisabled || saved.Reason != "manual disable" {
		t.Fatalf("manual disable was wiped by save: %+v", saved)
	}
	if saved.Note != "updated note" {
		t.Fatalf("note = %q, want updated note", saved.Note)
	}
	if _, ok := saved.Extra["future_field"]; !ok {
		t.Fatalf("unknown field for loaded account lost: %+v", saved.Extra)
	}
	orphan := states["u-missing"]
	if !orphan.Disabled || !orphan.ManualDisabled || orphan.Note != "orphan note" {
		t.Fatalf("state for missing auth file was wiped: %+v", orphan)
	}
	if _, ok := orphan.Extra["future_orphan"]; !ok {
		t.Fatalf("unknown field for orphan lost: %+v", orphan.Extra)
	}
}

func TestPoolPickSkipsCreditExhaustedAndManualDisabled(t *testing.T) {
	dir := t.TempDir()
	for _, uid := range []string{"u-zero", "u-manual", "u-ok"} {
		writeTestAuth(t, dir, uid, "")
	}
	p := newTestPool(t, dir, Config{MaxConcurrent: 1, CreditsExpiringPriority: true})
	if err := p.Disable("u-manual", ""); err != nil {
		t.Fatal(err)
	}
	p.Get("u-zero").Info = accountInfo(0, 0)

	got, err := p.Pick("", "", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if poolUID(t, got) != "u-ok" {
		t.Fatalf("Pick = %q, want u-ok", poolUID(t, got))
	}

	p.Get("u-ok").Info = accountInfo(0, 0)
	if _, err := p.Pick("", "", nil); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Pick with every usable account exhausted = %v, want ErrNoAccount", err)
	}
}

func TestPoolMarkCreditExhaustedPreservesManualDisable(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u1", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})
	account := p.Get("u1")
	account.Info = accountInfo(100, 0)

	if err := p.Disable("u1", ""); err != nil {
		t.Fatal(err)
	}
	if err := account.MarkCreditExhausted(); err != nil {
		t.Fatal(err)
	}
	if !account.State.Disabled || !account.State.ManualDisabled {
		t.Fatalf("mark credit exhausted resurrected a manual disable: %+v", account.State)
	}
	if !account.CreditExhausted() {
		t.Fatal("CreditExhausted should be true after mark")
	}
	if account.State.LastError != "积分已耗尽" {
		t.Fatalf("last_error = %q", account.State.LastError)
	}

	reloaded := newTestPool(t, dir, Config{MaxConcurrent: 1})
	if !reloaded.Get("u1").State.ManualDisabled {
		t.Fatal("manual disable was not persisted")
	}

	// Marking a healthy account must park it without disabling it.
	writeTestAuth(t, dir, "u2", "")
	p = newTestPool(t, dir, Config{MaxConcurrent: 1})
	healthy := p.Get("u2")
	healthy.Info = accountInfo(10, 0)
	if err := healthy.MarkCreditExhausted(); err != nil {
		t.Fatal(err)
	}
	if healthy.State.Disabled || healthy.State.ManualDisabled {
		t.Fatalf("credit exhaustion must not disable the account: %+v", healthy.State)
	}
	if _, err := p.Pick("", "", nil); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("credit-exhausted account should be skipped, got %v", err)
	}

	healthy.Info = accountInfo(5, 0)
	if !healthy.Healthy(p.now()) {
		t.Fatal("refreshed positive credits should make the account healthy")
	}
}

func TestPoolPickExpiringPriorityAndDeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	for _, uid := range []string{"a", "b", "c"} {
		writeTestAuth(t, dir, uid, "")
	}
	p := newTestPool(t, dir, Config{MaxConcurrent: 1, CreditsExpiringPriority: true})
	p.Get("b").Info = accountInfo(100, 10)
	p.Get("c").Info = accountInfo(100, 1)

	got, err := p.Pick("", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if poolUID(t, got) != "b" {
		t.Fatalf("expiring priority Pick = %q, want b", poolUID(t, got))
	}
	if got, err = p.Pick("", "", map[string]bool{"b": true}); err != nil || poolUID(t, got) != "c" {
		t.Fatalf("second expiring Pick = %v, %v; want c", got, err)
	}

	p.SetConfig(Config{MaxConcurrent: 1, CreditsExpiringPriority: false})
	if got, err = p.Pick("", "", nil); err != nil || poolUID(t, got) != "a" {
		t.Fatalf("disabled expiring priority Pick = %v, %v; want a", got, err)
	}

	// Least recently used loses its advantage once used; UID breaks the rest.
	if !p.Acquire(p.Get("a")) {
		t.Fatal("acquire a")
	}
	p.Release(p.Get("a"))
	if got, err = p.Pick("", "", nil); err != nil || poolUID(t, got) != "b" {
		t.Fatalf("last-used ordering Pick = %v, %v; want b", got, err)
	}
}

func TestPoolSessionAffinityAndBound(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u-a", "")
	writeTestAuth(t, dir, "u-b", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})

	p.BindSession("session-1", "u-b")
	if !p.Acquire(p.Get("u-b")) {
		t.Fatal("acquire u-b")
	}
	p.Release(p.Get("u-b"))

	if got, err := p.Pick("", "session-1", nil); err != nil || poolUID(t, got) != "u-b" {
		t.Fatalf("Pick(session-1) = %v, %v; want u-b", got, err)
	}
	if got, err := p.Pick("", "session-2", nil); err != nil || poolUID(t, got) != "u-a" {
		t.Fatalf("Pick(session-2) = %v, %v; want u-a", got, err)
	}

	if !p.Acquire(p.Get("u-b")) {
		t.Fatal("acquire u-b")
	}
	if got, err := p.Pick("", "session-1", nil); err != nil || poolUID(t, got) != "u-a" {
		t.Fatalf("affinity bypassed concurrency: %v, %v", got, err)
	}
	p.Release(p.Get("u-b"))

	p.BindSession("session-1", "u-a")
	if got, err := p.Pick("", "session-1", nil); err != nil || poolUID(t, got) != "u-a" {
		t.Fatalf("Pick after rebind = %v, %v; want u-a", got, err)
	}

	for i := 0; i < 5000; i++ {
		p.BindSession("bulk-"+string(rune('a'+i%26))+"-"+string(rune('a'+i/26%26))+"-"+string(rune('a'+i/676)), "u-a")
	}
	p.mu.RLock()
	affinitySize := len(p.affinity)
	orderSize := len(p.order)
	p.mu.RUnlock()
	if affinitySize > maxAffinityEntries || orderSize > maxAffinityEntries {
		t.Fatalf("affinity map grew past its bound: %d entries, %d order", affinitySize, orderSize)
	}
}

func TestPoolAcquireRespectsConcurrencyCap(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u1", "")
	writeTestAuth(t, dir, "u2", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 2})
	account := p.Get("u1")

	if !p.Acquire(account) || !p.Acquire(account) {
		t.Fatal("two acquisitions should fit max_concurrent=2")
	}
	if p.Acquire(account) {
		t.Fatal("third acquisition exceeded max_concurrent")
	}
	p.Release(account)
	if !p.Acquire(account) {
		t.Fatal("release did not free a slot")
	}
	p.Release(account)
	p.Release(account)

	parallel := p.Get("u2")
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Acquire(parallel) {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := successes.Load(); got != 2 {
		t.Fatalf("parallel successful acquisitions = %d, want 2", got)
	}
	for i := 0; i < 2; i++ {
		p.Release(parallel)
	}
	if parallel.activeConcurrent != 0 {
		t.Fatalf("active concurrent = %d, want 0", parallel.activeConcurrent)
	}
}

func TestPoolEnableDisableCooldownNoteAndTasks(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u1", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})

	if err := p.Disable("u1", ""); err != nil {
		t.Fatal(err)
	}
	view := p.Get("u1").View()
	if !view.Disabled || !view.ManualDisabled || view.Reason != "manual disable" {
		t.Fatalf("manual disable view = %+v", view)
	}
	p = newTestPool(t, dir, Config{MaxConcurrent: 1})
	if !p.Get("u1").State.ManualDisabled {
		t.Fatal("manual disable did not survive a reload")
	}

	if err := p.Enable("u1"); err != nil {
		t.Fatal(err)
	}
	p = newTestPool(t, dir, Config{MaxConcurrent: 1})
	if p.Get("u1").State.Disabled || p.Get("u1").State.ManualDisabled {
		t.Fatalf("enable did not persist: %+v", p.Get("u1").State)
	}

	longNote := strings.Repeat("注", 250)
	if err := p.SetNote("u1", "  "+longNote+"  "); err != nil {
		t.Fatal(err)
	}
	if got := p.Get("u1").State.Note; len([]rune(got)) != 200 {
		t.Fatalf("note length = %d runes, want 200", len([]rune(got)))
	}
	if err := p.SetTasksEnabled("u1", false); err != nil {
		t.Fatal(err)
	}

	account := p.Get("u1")
	account.State.CoolUntil = float64(time.Now().Add(2 * time.Minute).Unix())
	if err := p.SaveState(); err != nil {
		t.Fatal(err)
	}
	if !p.Get("u1").View().Cooling {
		t.Fatal("account should be cooling")
	}
	if err := p.ClearCooldown("u1"); err != nil {
		t.Fatal(err)
	}
	p = newTestPool(t, dir, Config{MaxConcurrent: 1})
	account = p.Get("u1")
	if account.State.CoolUntil != 0 || account.State.ErrCount != 0 {
		t.Fatalf("cooldown was not cleared: %+v", account.State)
	}
	if account.State.TasksEnabled || account.State.Note == "" {
		t.Fatalf("tasks/note not persisted: %+v", account.State)
	}
	if err := p.Disable("missing", ""); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("Disable(missing) = %v, want ErrAccountNotFound", err)
	}
}

func TestPoolAddRemoveAndAuthPathCollision(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})

	first := testAuth("a/b")
	first.ProxySlot = "slot-1"
	account, err := p.Add(first)
	if err != nil {
		t.Fatal(err)
	}
	if account.Auth.UserID != "a/b" {
		t.Fatalf("added uid = %q", account.Auth.UserID)
	}
	if err := p.Disable("a/b", "manual disable"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNote("a/b", "keep me"); err != nil {
		t.Fatal(err)
	}

	// Re-import preserves manual state, notes, and the selected proxy slot.
	reimported := testAuth("a/b")
	if _, err := p.Add(reimported); err != nil {
		t.Fatal(err)
	}
	account = p.Get("a/b")
	if !account.State.ManualDisabled || account.State.Note != "keep me" {
		t.Fatalf("re-import lost manual state: %+v", account.State)
	}
	if account.Auth.ProxySlot != "slot-1" {
		t.Fatalf("re-import lost proxy slot: %q", account.Auth.ProxySlot)
	}

	// a/b and a_b sanitize to the same on-disk filename; the second import must
	// not overwrite the first account's token document.
	if _, err := p.Add(testAuth("a_b")); !errors.Is(err, ErrAuthPathCollision) {
		t.Fatalf("colliding Add = %v, want ErrAuthPathCollision", err)
	}
	if p.Get("a_b") != nil {
		t.Fatal("colliding account entered the pool")
	}
	path, err := AuthPath(dir, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UserID != "a/b" {
		t.Fatalf("collision overwrote the auth file with uid %q", stored.UserID)
	}

	if err := p.Remove("a/b"); err != nil {
		t.Fatal(err)
	}
	if p.Get("a/b") != nil {
		t.Fatal("removed account still in the pool")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("auth file still exists after Remove: %v", err)
	}
	states, err := LoadState(filepath.Join(dir, "codearts", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := states["a/b"]; ok {
		t.Fatal("state entry still exists after Remove")
	}
	if err := p.Remove("a/b"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("second Remove = %v, want ErrAccountNotFound", err)
	}
}

func TestPoolStateWrapperRoundTripPreservesUnknownAndMalformedAuthState(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u1", "")
	malformed := filepath.Join(dir, "codearts", "codearts-u-broken.json")
	if err := os.WriteFile(malformed, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestStateFile(t, dir, `{"accounts":[
		{"name":"u1","err_count":0,"cool_until":1234.5,"disabled":false,
		 "manual_disabled":false,"reason":"","last_error":"","checkin":null,
		 "note":"","tasks_enabled":true,"future_u1":"keep"},
		{"name":"u-broken","disabled":true,"manual_disabled":true,
		 "note":"broken auth state","future_broken":{"x":1}}
	]}`)

	p := newTestPool(t, dir, Config{MaxConcurrent: 1})
	if len(p.Accounts()) != 1 {
		t.Fatalf("malformed auth file should be skipped, got %d accounts", len(p.Accounts()))
	}
	if err := p.SaveState(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "codearts", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		t.Fatalf("state was not written as a bare v1 list: %s", data)
	}
	if bytes.Contains(trimmed, []byte(`"accounts"`)) {
		t.Fatalf("state kept the wrapper: %s", data)
	}

	states, err := LoadState(filepath.Join(dir, "codearts", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	u1 := states["u1"]
	if u1.CoolUntil != 1234.5 {
		t.Fatalf("cool_until = %v, want numeric 1234.5", u1.CoolUntil)
	}
	if _, ok := u1.Extra["future_u1"]; !ok {
		t.Fatalf("unknown u1 field lost: %+v", u1.Extra)
	}
	broken := states["u-broken"]
	if !broken.Disabled || !broken.ManualDisabled || broken.Note != "broken auth state" {
		t.Fatalf("malformed-auth state was wiped: %+v", broken)
	}
	if _, ok := broken.Extra["future_broken"]; !ok {
		t.Fatalf("unknown broken-auth field lost: %+v", broken.Extra)
	}
}

func TestPoolPickModelCapabilityAndCooling(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u-capable", "")
	writeTestAuth(t, dir, "u-other", "")
	writeTestAuth(t, dir, "u-cool", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})

	capable := p.Get("u-capable")
	other := p.Get("u-other")
	cool := p.Get("u-cool")
	capable.Models = []providers.Model{{"id": "m1"}}
	other.Models = []providers.Model{{"id": "m2"}}
	cool.State.CoolUntil = float64(time.Now().Add(time.Minute).Unix())

	if !capable.HasModel("m1") || capable.HasModel("m2") {
		t.Fatal("HasModel did not honor the account catalog")
	}
	if !other.HasModel("") {
		t.Fatal("an empty model must always be accepted")
	}
	if !cool.HasModel("m1") {
		t.Fatal("an undiscovered catalog must accept the model")
	}

	// Capability wins over recency.
	if !p.Acquire(other) {
		t.Fatal("acquire other")
	}
	p.Release(other)
	got, err := p.Pick("m1", "", nil)
	if err != nil || poolUID(t, got) != "u-capable" {
		t.Fatalf("Pick(m1) = %v, %v; want u-capable", got, err)
	}

	// Without a capable account the pool falls back to model-unknown accounts,
	// matching v1, but never to a cooling account.
	got, err = p.Pick("m3", "", nil)
	if err != nil {
		t.Fatalf("fallback Pick: %v", err)
	}
	if poolUID(t, got) == "u-cool" {
		t.Fatal("cooling account was selected")
	}
	if _, err = p.Pick("m1", "", map[string]bool{"u-capable": true, "u-other": true}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Pick while only cooling remains = %v, want ErrNoAccount", err)
	}
}

func TestPoolViewKeysMaskingAndSecretRedaction(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	path := writeTestAuth(t, dir, "u-view", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 2})
	p.ApplyProxySlots(transport.Slots{{
		ID:      "slot-1",
		URL:     "http://user:password@proxy.example:8080",
		Enabled: true,
	}})
	account := p.Get("u-view")
	account.Auth.ProxySlot = "slot-1"
	if err := p.SetProxySlot("u-view", "slot-1"); err != nil {
		t.Fatal(err)
	}
	account.Info = accountInfo(12, 3)
	account.Checkin = CheckinState{"ok": true, "checked_at": "2026-01-01T10:00:00"}
	account.State.Reason = "refresh rejected " + account.Auth.CloudDragonToken
	account.State.LastError = "Bearer " + account.Auth.RefreshToken + " secret=" + account.Auth.SecretAccessKey
	account.State.ErrCount = 2

	view := account.viewAt(now)
	if view.Name != "u-view" || view.UID != "u-view" || view.Nickname != "User u-view" || view.UserName != "User u-view" {
		t.Fatalf("identity fields = %+v", view)
	}
	if view.DefaultModel != "" || view.Package != "体验版" {
		t.Fatalf("defaults = %+v", view)
	}
	if view.ProxySlot != "slot-1" || view.Proxy != "http://<redacted>@proxy.example:8080" {
		t.Fatalf("proxy fields = %q/%q", view.ProxySlot, view.Proxy)
	}
	if view.Credits == nil || view.Credits.Remain != 12 || view.Credits.Expiring != 3 || view.Credits.Unit != "credit" {
		t.Fatalf("credits view = %+v", view.Credits)
	}
	if view.Checkin == nil || view.Checkin["ok"] != true {
		t.Fatalf("checkin view = %+v", view.Checkin)
	}
	if view.ErrCount != 2 || view.MaxConcurrent != 2 || view.ActiveConcurrent != 0 {
		t.Fatalf("counters = %+v", view)
	}
	if strings.Contains(view.Reason, account.Auth.CloudDragonToken) ||
		strings.Contains(view.LastError, account.Auth.RefreshToken) ||
		strings.Contains(view.LastError, account.Auth.SecretAccessKey) {
		t.Fatalf("view leaked a secret: reason=%q last_error=%q", view.Reason, view.LastError)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{
		account.Auth.CloudDragonToken,
		account.Auth.RefreshToken,
		account.Auth.SecretAccessKey,
		"user:password",
	} {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("marshaled view leaked %q: %s", secret, text)
		}
	}
	for _, key := range []string{
		"name", "uid", "nickname", "user_name", "default_model", "credits",
		"package", "checkin", "disabled", "manual_disabled", "cooling", "until",
		"err_count", "reason", "last_error", "active_concurrent", "max_concurrent",
		"token_remaining", "expires_at", "models", "proxy_slot", "proxy", "note",
		"tasks_enabled",
	} {
		if !strings.Contains(text, `"`+key+`":`) {
			t.Fatalf("view is missing key %q: %s", key, text)
		}
	}
	if account.Path != path {
		t.Fatalf("account path = %q, want %q", account.Path, path)
	}

	account.State.CoolUntil = float64(now.Add(10 * time.Minute).Unix())
	cooling := account.viewAt(now)
	if !cooling.Cooling || cooling.Until == "" {
		t.Fatalf("cooling view = %+v", cooling)
	}
	account.State.Disabled = true
	disabled := account.viewAt(now)
	if disabled.Cooling || disabled.Until != "" {
		t.Fatalf("disabled account must not report cooling: %+v", disabled)
	}
}

func TestProviderNewCreatesPool(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	writeTestAuth(t, dir, "u1", "")

	provider, err := New(store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pool := provider.NewPool()
	if pool == nil {
		t.Fatal("Provider.NewPool returned nil")
	}
	if got := len(pool.Accounts()); got != 1 {
		t.Fatalf("provider pool loaded %d accounts, want 1", got)
	}
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) should fail")
	}
	if _, err := New(config.New("")); err == nil {
		t.Fatal("New with an empty store path should fail")
	}
}

func TestAccountInfoSnapshotHelpers(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u1", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})
	account := p.Get("u1")

	info := accountInfo(0, 7)
	account.SetInfo(info)
	if !account.CreditExhausted() {
		t.Fatal("SetInfo did not install a zero-credit snapshot")
	}
	if got := account.ExpiringCreditRemain(); got != 7 {
		t.Fatalf("ExpiringCreditRemain = %v, want 7", got)
	}

	// SetInfo must keep a private copy: a later caller mutation cannot race
	// with the scheduler or change the installed snapshot.
	info.Credits.Total.Remain = 50
	info.Credits.Total.Expiring = 3
	if !account.CreditExhausted() || account.ExpiringCreditRemain() != 7 {
		t.Fatal("SetInfo retained the caller-owned snapshot")
	}

	account.SetInfo(accountInfo(10, 0))
	if account.CreditExhausted() || !account.Healthy(p.nowTime()) {
		t.Fatal("positive snapshot should unpark the account")
	}

	account.SetInfo(accountInfo(0, 5))
	account.ClearCreditExhausted()
	if account.CreditExhausted() || !account.Healthy(p.nowTime()) {
		t.Fatal("ClearCreditExhausted should drop the zero-credit snapshot")
	}
	account.SetInfo(nil)
	if account.CreditExhausted() {
		t.Fatal("nil snapshot must not be exhausted")
	}

	// Race-detector coverage for the locked helpers.
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				account.SetInfo(accountInfo(float64(j%2), float64(j%5)))
				_ = account.ExpiringCreditRemain()
				_ = account.Healthy(time.Now())
				_ = account.View()
				if j%3 == 0 {
					account.ClearCreditExhausted()
				}
			}
		}()
	}
	wg.Wait()
}

func TestPoolNoteSuccessAndNoteError(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writeTestAuth(t, dir, "u1", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})
	p.now = func() time.Time { return now }
	account := p.Get("u1")

	if err := p.NoteError(account, "first failure", false, false); err != nil {
		t.Fatal(err)
	}
	if account.State.LastError != "first failure" || account.State.ErrCount != 1 || account.State.CoolUntil != 0 {
		t.Fatalf("ordinary error state = %+v", account.State)
	}
	if err := p.NoteError(account, "second failure", false, false); err != nil {
		t.Fatal(err)
	}
	if account.State.ErrCount != 2 {
		t.Fatalf("err_count after second failure = %d, want 2", account.State.ErrCount)
	}
	if err := p.NoteError(account, "third failure", false, false); err != nil {
		t.Fatal(err)
	}
	if account.State.ErrCount != 0 {
		t.Fatalf("err_count should reset after threshold, got %d", account.State.ErrCount)
	}
	if want := unixSeconds(now.Add(600 * time.Second)); account.State.CoolUntil != want {
		t.Fatalf("cool_until = %v, want %v", account.State.CoolUntil, want)
	}
	if err := p.NoteSuccess(account); err != nil {
		t.Fatal(err)
	}
	if account.State.ErrCount != 0 || account.State.CoolUntil != 0 || account.State.LastError != "" {
		t.Fatalf("NoteSuccess did not clear transient state: %+v", account.State)
	}
	cleared, err := LoadState(filepath.Join(dir, "codearts", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cleared["u1"].ErrCount != 0 || cleared["u1"].CoolUntil != 0 || cleared["u1"].LastError != "" {
		t.Fatalf("NoteSuccess was not persisted: %+v", cleared["u1"])
	}

	if err := p.NoteError(account, "soft failure", true, false); err != nil {
		t.Fatal(err)
	}
	if want := unixSeconds(now.Add(60 * time.Second)); account.State.CoolUntil != want {
		t.Fatalf("soft cool_until = %v, want %v", account.State.CoolUntil, want)
	}
	if account.State.ErrCount != 0 {
		t.Fatalf("soft error must not increment err_count, got %d", account.State.ErrCount)
	}

	// Terminal failure on a normal account disables it with the v1 reason.
	if err := p.NoteError(account, "auth dead", false, true); err != nil {
		t.Fatal(err)
	}
	if !account.State.Disabled || account.State.ManualDisabled || account.State.Reason != "auth dead" {
		t.Fatalf("terminal error state = %+v", account.State)
	}
	reloaded := newTestPool(t, dir, Config{MaxConcurrent: 1})
	if !reloaded.Get("u1").State.Disabled || reloaded.Get("u1").State.ManualDisabled {
		t.Fatalf("terminal disable was not persisted: %+v", reloaded.Get("u1").State)
	}

	// A manual disable must never be overwritten by a terminal failure.
	if err := reloaded.Enable("u1"); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Disable("u1", "manual disable"); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.NoteError(reloaded.Get("u1"), "token rejected", false, true); err != nil {
		t.Fatal(err)
	}
	manual := reloaded.Get("u1")
	if !manual.State.ManualDisabled || !manual.State.Disabled || manual.State.Reason != "manual disable" {
		t.Fatalf("terminal error overwrote manual disable: %+v", manual.State)
	}
	if manual.State.LastError != "token rejected" {
		t.Fatalf("terminal error did not record last_error: %+v", manual.State)
	}
	again := newTestPool(t, dir, Config{MaxConcurrent: 1})
	if !again.Get("u1").State.ManualDisabled || again.Get("u1").State.Reason != "manual disable" {
		t.Fatalf("manual terminal state was not persisted: %+v", again.Get("u1").State)
	}

	// Error messages are capped to v1's 200-rune note limit.
	if err := again.NoteError(again.Get("u1"), strings.Repeat("错", 250), false, false); err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(again.Get("u1").State.LastError)); got != 200 {
		t.Fatalf("last_error length = %d runes, want 200", got)
	}
}

func TestPoolStateWritesCannotOverwriteLaterMutation(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u1", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})

	entered := make(chan struct{})
	release := make(chan struct{})
	p.stateWriteHook = func() {
		p.stateWriteHook = nil
		close(entered)
		<-release
	}

	saveDone := make(chan error, 1)
	go func() { saveDone <- p.SaveState() }()
	<-entered

	disableDone := make(chan error, 1)
	noteDone := make(chan error, 1)
	go func() { disableDone <- p.Disable("u1", "manual disable") }()
	go func() { noteDone <- p.SetNote("u1", "later note") }()

	// Both mutations block on stateMu while the older snapshot waits to write.
	close(release)
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	if err := <-disableDone; err != nil {
		t.Fatal(err)
	}
	if err := <-noteDone; err != nil {
		t.Fatal(err)
	}

	states, err := LoadState(filepath.Join(dir, "codearts", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	state := states["u1"]
	if !state.Disabled || !state.ManualDisabled || state.Note != "later note" {
		t.Fatalf("older save overwrote later mutations: %+v", state)
	}
}

func TestPoolAddReappliesRuntimeSettings(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})
	slot := transport.Slots{{
		ID:      "slot-1",
		URL:     "http://user:password@proxy.example:8080",
		Enabled: true,
	}}

	// Simulate a settings change in the window after Add built the account
	// from the old config and before it joined/reapplied the pool.
	p.addAccountHook = func(*Account) {
		p.addAccountHook = nil
		p.SetConfig(Config{MaxConcurrent: 4, CreditsExpiringPriority: true})
		p.ApplyProxySlots(slot)
	}
	auth := testAuth("u1")
	auth.ProxySlot = "slot-1"
	added, err := p.Add(auth)
	if err != nil {
		t.Fatal(err)
	}
	if view := added.View(); view.MaxConcurrent != 4 || view.ProxySlot != "slot-1" {
		t.Fatalf("Add did not reapply config/proxy: %+v", view)
	}
	if added.ProxyURL() != "http://user:password@proxy.example:8080" {
		t.Fatalf("Add proxy URL = %q", added.ProxyURL())
	}

	// Re-import must reapply the same way after replacing credentials.
	slot2 := transport.Slots{{
		ID:      "slot-1",
		URL:     "http://other:secret@proxy2.example:9090",
		Enabled: true,
	}}
	p.addAccountHook = func(*Account) {
		p.addAccountHook = nil
		p.SetConfig(Config{MaxConcurrent: 5, CreditsExpiringPriority: true})
		p.ApplyProxySlots(slot2)
	}
	if _, err := p.Add(testAuth("u1")); err != nil {
		t.Fatal(err)
	}
	reimported := p.Get("u1")
	if view := reimported.View(); view.MaxConcurrent != 5 || view.ProxySlot != "slot-1" {
		t.Fatalf("re-import did not reapply config/proxy: %+v", view)
	}
	if reimported.ProxyURL() != "http://other:secret@proxy2.example:9090" {
		t.Fatalf("re-import proxy URL = %q", reimported.ProxyURL())
	}
}

func TestPoolPickLeastActiveBeforeExpiringPriority(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u-expiring", "")
	writeTestAuth(t, dir, "u-normal", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 2, CreditsExpiringPriority: true})
	expiring := p.Get("u-expiring")
	expiring.Info = accountInfo(100, 9)

	// v1 rank is (active_concurrent, urgent, last_used, uid): a busy expiring
	// account loses to an idle non-expiring account even though priority is on.
	if !p.Acquire(expiring) {
		t.Fatal("acquire expiring account")
	}
	got, err := p.Pick("", "", nil)
	if err != nil || poolUID(t, got) != "u-normal" {
		t.Fatalf("Pick with busy expiring account = %v, %v; want u-normal", got, err)
	}
	p.Release(expiring)

	// Within the same concurrency tier, expiring credits now win.
	got, err = p.Pick("", "", nil)
	if err != nil || poolUID(t, got) != "u-expiring" {
		t.Fatalf("Pick within same concurrency tier = %v, %v; want u-expiring", got, err)
	}
}

func TestPoolAuthCanonicalPrecedenceAndAliasRemoval(t *testing.T) {
	dir := t.TempDir()
	canonicalPath, err := AuthPath(dir, "u1")
	if err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(dir, "codearts", "codearts-alias.json")
	canonical := testAuth("u1")
	canonical.UserName = "Canonical"
	alias := testAuth("u1")
	alias.UserName = "Alias"
	alias.CloudDragonToken = "alias-token"
	if err := SaveAuth(aliasPath, alias); err != nil {
		t.Fatal(err)
	}
	if err := SaveAuth(canonicalPath, canonical); err != nil {
		t.Fatal(err)
	}

	p := newTestPool(t, dir, Config{MaxConcurrent: 1})
	account := p.Get("u1")
	if account == nil {
		t.Fatal("u1 not loaded")
	}
	if account.Auth.UserName != "Canonical" || account.Path != canonicalPath {
		t.Fatalf("canonical auth did not win: name=%q path=%q", account.Auth.UserName, account.Path)
	}

	// An alias-only account must be removable through its alias file.
	aliasDir := t.TempDir()
	aliasOnly := filepath.Join(aliasDir, "codearts", "codearts-alias.json")
	if err := SaveAuth(aliasOnly, testAuth("u-alias")); err != nil {
		t.Fatal(err)
	}
	aliasPool := newTestPool(t, aliasDir, Config{MaxConcurrent: 1})
	if got := aliasPool.Get("u-alias"); got == nil || got.Path != aliasOnly {
		t.Fatalf("alias account not loaded from its file: %+v", got)
	}
	if err := aliasPool.Remove("u-alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(aliasOnly); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("alias auth file still exists after Remove: %v", err)
	}
}

func TestPoolSetProxySlotAndRemoveRefuseForeignAuthFile(t *testing.T) {
	dir := t.TempDir()
	foreignPath, err := AuthPath(dir, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAuth(foreignPath, testAuth("a/b")); err != nil {
		t.Fatal(err)
	}
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})

	// A sanitized filename collision makes account a_b believe it owns the
	// file written for a/b. Neither SetProxySlot nor Remove may touch it.
	foreignAccount := &Account{
		Auth:          testAuth("a_b"),
		State:         AccountState{Name: "a_b", TasksEnabled: true},
		Path:          foreignPath,
		pool:          p,
		maxConcurrent: 1,
	}
	p.mu.Lock()
	p.accounts = append(p.accounts, foreignAccount)
	p.mu.Unlock()

	if err := p.SetProxySlot("a_b", "slot-1"); !errors.Is(err, ErrAuthPathCollision) {
		t.Fatalf("SetProxySlot collision = %v, want ErrAuthPathCollision", err)
	}
	stored, err := LoadAuth(foreignPath)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UserID != "a/b" || stored.ProxySlot != "" {
		t.Fatalf("collision target was modified: %+v", stored)
	}

	if err := p.Remove("a_b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreignPath); err != nil {
		t.Fatalf("Remove deleted a foreign uid's auth file: %v", err)
	}
	stored, err = LoadAuth(foreignPath)
	if err != nil || stored.UserID != "a/b" {
		t.Fatalf("foreign auth file changed after Remove: %+v, %v", stored, err)
	}
	if p.Get("a/b") == nil {
		t.Fatal("foreign Remove also removed the legitimate a/b account")
	}
}

func TestPoolViewCheckinRedactionAndFixedCreditUnit(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writeTestAuth(t, dir, "u1", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 1})
	account := p.Get("u1")
	info := accountInfo(5, 1)
	info.Credits.Unit = "points"
	account.SetInfo(info)
	token := account.Auth.CloudDragonToken
	account.Checkin = CheckinState{
		"ok":           true,
		"checked_at":   "2026-01-01T10:00:00",
		"message":      "claimed with " + token,
		"access_token": "checkin-access-token",
		"campaigns": []any{map[string]any{
			"campaign_id": float64(7),
			"ok":          true,
			"title":       "每日签到",
		}},
		"nested": map[string]any{
			"refresh_token": "checkin-refresh-token",
			"note":          "keep me",
			"items": []any{map[string]any{
				"secret_key": "nested-secret-value",
				"title":      "nested campaign",
			}},
		},
	}

	view := account.viewAt(now)
	if view.Credits == nil || view.Credits.Unit != "credit" {
		t.Fatalf("credits unit = %+v, want credit", view.Credits)
	}
	if view.Checkin["ok"] != true || view.Checkin["checked_at"] != "2026-01-01T10:00:00" {
		t.Fatalf("expected checkin fields lost: %#v", view.Checkin)
	}
	if view.Checkin["access_token"] != "<redacted>" {
		t.Fatalf("access_token = %#v", view.Checkin["access_token"])
	}
	message, _ := view.Checkin["message"].(string)
	if strings.Contains(message, token) {
		t.Fatalf("checkin message leaked the token: %q", message)
	}
	campaigns, ok := view.Checkin["campaigns"].([]any)
	if !ok || len(campaigns) != 1 {
		t.Fatalf("campaigns = %#v", view.Checkin["campaigns"])
	}
	nested, ok := view.Checkin["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested = %#v", view.Checkin["nested"])
	}
	if nested["refresh_token"] != "<redacted>" || nested["note"] != "keep me" {
		t.Fatalf("nested fields = %#v", nested)
	}
	items, ok := nested["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("nested items = %#v", nested["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["secret_key"] != "<redacted>" || item["title"] != "nested campaign" {
		t.Fatalf("nested item = %#v", items[0])
	}

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		token,
		"checkin-access-token",
		"checkin-refresh-token",
		"nested-secret-value",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("view leaked %q: %s", secret, encoded)
		}
	}
	// Redaction applies only to the public copy, never to the persisted map.
	if account.Checkin["access_token"] != "checkin-access-token" {
		t.Fatalf("view redaction mutated the stored checkin: %#v", account.Checkin)
	}
}
