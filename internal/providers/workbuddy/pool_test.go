package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ayeaaaa/mix2api-v2/internal/core/accountstate"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func writePoolSettings(t *testing.T, dir string, settings map[string]any) {
	t.Helper()
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

func writePoolAccount(t *testing.T, dir, uid, realm string, fields map[string]any) string {
	t.Helper()
	payload := map[string]any{
		"uid":          uid,
		"accessToken":  "access-" + uid,
		"refreshToken": "refresh-" + uid,
		"realm":        realm,
		"platform":     "CLI",
		"source":       sourceOAuth,
		"expiresAt":    time.Now().Add(time.Hour).Unix(),
	}
	for key, value := range fields {
		payload[key] = value
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal account %s: %v", uid, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir accounts dir: %v", err)
	}
	path := filepath.Join(dir, uid+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write account %s: %v", uid, err)
	}
	return path
}

func newPoolProvider(t *testing.T, realm, dir string) (*Provider, *Pool) {
	t.Helper()
	provider, err := New(realm, config.New(filepath.Join(dir, "settings.json")))
	if err != nil {
		t.Fatalf("New(%s): %v", realm, err)
	}
	pool := provider.NewPool()
	if pool == nil {
		t.Fatal("NewPool returned nil")
	}
	return provider, pool
}

func poolUIDs(accounts []*Account) []string {
	uids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		uids = append(uids, account.Auth.UID)
	}
	sort.Strings(uids)
	return uids
}

func pickUIDs(t *testing.T, pool *Pool, model string, count int) []string {
	t.Helper()
	uids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		account, err := pool.Pick(model, "", nil)
		if err != nil {
			t.Fatalf("Pick %d: %v", i, err)
		}
		uids = append(uids, account.Auth.UID)
	}
	return uids
}

func countPicks(uids []string) map[string]int {
	counts := make(map[string]int, len(uids))
	for _, uid := range uids {
		counts[uid]++
	}
	return counts
}

func poolCredits(days ...float64) map[string]any {
	packages := make([]any, 0, len(days))
	total := 0.0
	for index, day := range days {
		packages = append(packages, map[string]any{
			"name":         fmt.Sprintf("pkg%d", index),
			"package_code": "package",
			"remain":       100,
			"used":         0,
			"size":         100,
			"days_left":    day,
			"no_expiry":    false,
			"is_expired":   day < 0,
		})
		total += 100
	}
	return map[string]any{
		"remain":   total,
		"used":     0,
		"size":     total,
		"packages": packages,
	}
}

func setAccountWindow(t *testing.T, pool *Pool, days int) {
	t.Helper()
	pool.ApplyExpiringWindow(days)
}

func TestPoolLoadFiltersRealmAndSkipsMalformedFiles(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "intl-a", realmIntl, map[string]any{"enabled": false})
	writePoolAccount(t, dir, "intl-b", realmIntl, nil)
	writePoolAccount(t, dir, "cn-a", realmCN, nil)
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"workbuddy":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	provider, pool := newPoolProvider(t, realmIntl, dir)
	if provider.Pool() != pool || provider.NewPool() != pool {
		t.Fatal("Provider.NewPool is not idempotent")
	}
	if err := pool.LoadError(); err != nil {
		t.Fatalf("Pool.LoadError = %v", err)
	}
	got := poolUIDs(pool.Accounts())
	if !reflectEqual(got, []string{"intl-a", "intl-b"}) {
		t.Fatalf("intl accounts = %v, want [intl-a intl-b]", got)
	}
	disabled := pool.Get("intl-a")
	if disabled == nil || disabled.Auth.Enabled {
		t.Fatalf("intl-a should load as disabled: %+v", disabled)
	}
	if !disabled.State.Disabled {
		t.Fatalf("disabled auth was not mirrored into runtime state: %+v", disabled.State)
	}

	_, cnPool := newPoolProvider(t, realmCN, dir)
	if got := poolUIDs(cnPool.Accounts()); !reflectEqual(got, []string{"cn-a"}) {
		t.Fatalf("cn accounts = %v, want [cn-a]", got)
	}
}

func reflectEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestPoolLoadsAccountStateExtrasCreditsAndTier(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "load-a", realmIntl, map[string]any{
		"enabled":        false,
		"tasksEnabled":   true,
		"weight":         7,
		"note":           "  keep me  ",
		"group":          "  Team A  ",
		"disabledModels": []any{" GLM-5.3 ", "glm-5.3", "gpt-6"},
		"proxySlot":      "slot-1",
		"credits":        poolCredits(2, 30),
		"tier": map[string]any{
			"paid":    true,
			"package": "Pro",
		},
		"futureField": map[string]any{"nested": []any{1, 2}},
	})

	_, pool := newPoolProvider(t, realmIntl, dir)
	account := pool.Get("load-a")
	if account == nil {
		t.Fatal("load-a missing")
	}
	if account.Group != "Team A" || account.Weight != 7 {
		t.Fatalf("group/weight = %q/%v, want Team A/7", account.Group, account.Weight)
	}
	if !reflectEqual(account.Auth.DisabledModels, []string{"glm-5.3", "gpt-6"}) {
		t.Fatalf("disabled models = %v", account.Auth.DisabledModels)
	}
	if !account.DisabledModels["glm-5.3"] || account.DisabledModels["gpt-6"] != true {
		t.Fatalf("disabled model map = %v", account.DisabledModels)
	}
	if account.Tier != "Pro" || !account.IsPaidUser() {
		t.Fatalf("tier = %q paid=%v, want Pro/true", account.Tier, account.IsPaidUser())
	}
	if !account.Auth.TasksEnabled {
		t.Fatal("tasksEnabled was not loaded")
	}
	if account.Credits == nil || len(account.Credits.Packages) != 2 {
		t.Fatalf("credits = %+v", account.Credits)
	}
	if days, ok := account.SoonestExpiringDays(); !ok || days != 2 {
		t.Fatalf("soonest expiring days = %v/%v, want 2/true", days, ok)
	}
	if _, ok := account.Auth.Extra["futureField"]; !ok {
		t.Fatalf("unknown extra was not preserved: %v", account.Auth.Extra)
	}
	if account.Auth.Path != filepath.Join(dir, "load-a.json") {
		t.Fatalf("account path = %q", account.Auth.Path)
	}
}

func TestPoolPickSkipsManualDisabledCoolingAndExcluded(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	writePoolAccount(t, dir, "b", realmIntl, nil)
	writePoolAccount(t, dir, "c", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)

	if err := pool.Disable("a", "operator"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	account := pool.Get("a")
	account.mu.Lock()
	account.Auth.Note = "note access-a and device-secret-value"
	account.Auth.Extra = map[string]json.RawMessage{
		"deviceToken": json.RawMessage(`"device-secret-value"`),
	}
	account.State.CoolUntil = time.Now().Add(time.Hour)
	account.State.LastError = "token access-a device-secret-value"
	account.State.Reason = "reason access-a"
	account.mu.Unlock()

	if got := pickUIDs(t, pool, "m", 12); strings.Contains(strings.Join(got, ","), "a") {
		t.Fatalf("disabled/cooling account was picked: %v", got)
	}
	if _, err := pool.Pick("m", "", map[string]bool{"b": true, "c": true}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("Pick with all usable accounts excluded = %v, want ErrNoAccount", err)
	}
	view := account.View()
	for field, value := range map[string]string{
		"note":      view.Note,
		"lastError": view.LastError,
		"reason":    view.Reason,
	} {
		if strings.Contains(value, "access-a") || strings.Contains(value, "device-secret-value") {
			t.Fatalf("view %s leaked a secret: %q", field, value)
		}
	}

	if err := pool.ClearCooldown("a"); err != nil {
		t.Fatalf("ClearCooldown: %v", err)
	}
	if account.State.CoolUntil.After(time.Now()) || account.State.LastError != "" || account.State.ErrCount != 0 {
		t.Fatalf("ClearCooldown left state: %+v", account.State)
	}
}

func TestPoolPickSkipsDisabledModelCaseInsensitively(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, map[string]any{
		"disabledModels": []any{" GLM-5.3 "},
	})
	writePoolAccount(t, dir, "b", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)

	if got := pickUIDs(t, pool, "glm-5.3", 6); !reflectEqual(countsOnly(got), []string{"b"}) {
		t.Fatalf("blocked model picks = %v, want only b", got)
	}
	if got := pickUIDs(t, pool, "glm-5.4", 6); len(countPicks(got)) != 2 {
		t.Fatalf("unrelated model should use both accounts: %v", got)
	}
	if pool.CountReady("glm-5.3") != 1 || pool.CountReady("glm-5.4") != 2 {
		t.Fatalf("CountReady = %d/%d, want 1/2", pool.CountReady("glm-5.3"), pool.CountReady("glm-5.4"))
	}
	if err := pool.SetDisabledModels("a", []string{" GPT-6 ", "gpt-6"}); err != nil {
		t.Fatalf("SetDisabledModels: %v", err)
	}
	if got := pool.Get("a").Auth.DisabledModels; !reflectEqual(got, []string{"gpt-6"}) {
		t.Fatalf("disabled models = %v", got)
	}
}

func countsOnly(uids []string) []string {
	counts := countPicks(uids)
	out := make([]string, 0, len(counts))
	for uid := range counts {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}

func TestPoolAccountConcurrencyCapsPickAndAcquire(t *testing.T) {
	dir := t.TempDir()
	writePoolSettings(t, dir, map[string]any{
		"workbuddy": map[string]any{
			realmIntl: map[string]any{"account_max_concurrent": 1},
		},
	})
	writePoolAccount(t, dir, "a", realmIntl, nil)
	writePoolAccount(t, dir, "b", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)

	a := pool.Get("a")
	if a.State.MaxConcurrent != 1 {
		t.Fatalf("max concurrent = %d, want 1", a.State.MaxConcurrent)
	}
	if !pool.Acquire(a) {
		t.Fatal("first acquire should succeed")
	}
	if pool.Acquire(a) {
		t.Fatal("second acquire exceeded the cap")
	}
	if got := pickUIDs(t, pool, "m", 4); !reflectEqual(countsOnly(got), []string{"b"}) {
		t.Fatalf("full account was picked: %v", got)
	}
	pool.Release(a)
	pool.Release(a)
	if got := a.State.ActiveConcurrent; got != 0 {
		t.Fatalf("active concurrent = %d, want 0", got)
	}
}

func TestPoolConcurrentAcquireIsAtomic(t *testing.T) {
	dir := t.TempDir()
	writePoolSettings(t, dir, map[string]any{
		"workbuddy": map[string]any{
			realmIntl: map[string]any{"account_max_concurrent": 3},
		},
	})
	writePoolAccount(t, dir, "a", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)
	a := pool.Get("a")

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
	)
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if pool.Acquire(a) {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if success != 3 {
		t.Fatalf("successful acquires = %d, want 3", success)
	}
	if got := a.State.ActiveConcurrent; got != 3 {
		t.Fatalf("active concurrent = %d, want 3", got)
	}
	for i := 0; i < 5; i++ {
		pool.Release(a)
	}
	if got := a.State.ActiveConcurrent; got != 0 {
		t.Fatalf("active concurrent after release = %d, want 0", got)
	}
}

func TestPoolSessionAffinityAndRecentHistory(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	writePoolAccount(t, dir, "b", realmIntl, nil)
	writePoolAccount(t, dir, "c", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)

	pool.BindSession("session-1", "b")
	account, err := pool.Pick("m", "session-1", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if account.Auth.UID != "b" {
		t.Fatalf("affinity pick = %q, want b", account.Auth.UID)
	}

	pool.affinity.bind("session-2", "c")
	pool.affinity.bind("session-2", "a")
	b := pool.Get("a")
	b.mu.Lock()
	b.State.CoolUntil = time.Now().Add(time.Hour)
	b.mu.Unlock()
	account, err = pool.Pick("m", "session-2", nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if account.Auth.UID != "c" {
		t.Fatalf("warm fallback = %q, want c", account.Auth.UID)
	}

	account, err = pool.Pick("m", "session-2", map[string]bool{"c": true})
	if err != nil {
		t.Fatalf("Pick excluding c: %v", err)
	}
	if account.Auth.UID == "c" || account.Auth.UID == "a" {
		t.Fatalf("excluded/cooling account picked: %q", account.Auth.UID)
	}
}

func TestPoolManualWeightsUseSmoothWeightedRoundRobin(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "light", realmIntl, nil)
	writePoolAccount(t, dir, "two", realmIntl, nil)
	writePoolAccount(t, dir, "four", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)

	if err := pool.SetWeight("two", 2); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetWeight("four", 4); err != nil {
		t.Fatal(err)
	}
	got := pickUIDs(t, pool, "m", 6)
	counts := countPicks(got)
	if counts["two"] != 2 || counts["four"] != 4 || counts["light"] != 0 {
		t.Fatalf("weighted picks = %v (counts %v), want 2/4/0", got, counts)
	}
	if got[0] != "four" {
		t.Fatalf("first weighted pick = %q, want four", got[0])
	}

	otherDir := t.TempDir()
	writePoolAccount(t, otherDir, "a", realmIntl, nil)
	writePoolAccount(t, otherDir, "b", realmIntl, nil)
	_, equalPool := newPoolProvider(t, realmIntl, otherDir)
	if got := pickUIDs(t, equalPool, "m", 4); !reflectEqual(got, []string{"a", "b", "a", "b"}) {
		t.Fatalf("equal weight rotation = %v, want [a b a b]", got)
	}
}

func TestPoolExpiringCreditsPriority(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "urgent", realmIntl, map[string]any{"credits": poolCredits(1)})
	writePoolAccount(t, dir, "later", realmIntl, map[string]any{"credits": poolCredits(5)})
	writePoolAccount(t, dir, "outside", realmIntl, map[string]any{"credits": poolCredits(30)})
	writePoolAccount(t, dir, "unknown", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)
	setAccountWindow(t, pool, 7)

	got := pickUIDs(t, pool, "m", 60)
	counts := countPicks(got)
	if counts["outside"] != 0 || counts["unknown"] != 0 {
		t.Fatalf("non-expiring accounts were preferred: %v", counts)
	}
	if counts["urgent"] == 0 || counts["later"] == 0 {
		t.Fatalf("expiring window did not rotate: %v", counts)
	}
	if counts["urgent"] <= counts["later"] {
		t.Fatalf("urgent account did not receive the larger share: %v", counts)
	}

	allDir := t.TempDir()
	writePoolAccount(t, allDir, "a", realmIntl, map[string]any{"credits": poolCredits(1)})
	writePoolAccount(t, allDir, "b", realmIntl, map[string]any{"credits": poolCredits(30)})
	_, allPool := newPoolProvider(t, realmIntl, allDir)
	setAccountWindow(t, allPool, 0)
	if got := pickUIDs(t, allPool, "m", 12); len(countPicks(got)) != 2 {
		t.Fatalf("window=0 should not prioritise: %v", got)
	}
}

func TestPoolRemainingUsagePriorityHook(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	writePoolAccount(t, dir, "b", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)

	pool.ApplyRemainingWeights(map[string]map[string]int{
		"a": {"m": 3, "other": 1},
		"b": {"m": 2},
	})
	got := pickUIDs(t, pool, "m", 5)
	if counts := countPicks(got); counts["a"] != 3 || counts["b"] != 2 {
		t.Fatalf("remaining priority counts = %v, want 3/2", counts)
	}
	if got := pickUIDs(t, pool, "other", 6); len(countPicks(got)) != 2 {
		t.Fatalf("absent remaining weights should fall back to rotation: %v", got)
	}
}

func TestPoolGuardHookAndProOnlyFilter(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, map[string]any{
		"tier": map[string]any{"paid": true, "package": "Pro"},
	})
	writePoolAccount(t, dir, "b", realmIntl, map[string]any{
		"tier": map[string]any{"paid": false, "package": "Free"},
	})
	_, pool := newPoolProvider(t, realmIntl, dir)

	pool.SetGuardCheck(func(account *Account, model string) bool {
		return account.Auth.UID == "a"
	})
	if got := pickUIDs(t, pool, "m", 4); !reflectEqual(countsOnly(got), []string{"b"}) {
		t.Fatalf("guard hook did not block a: %v", got)
	}
	pool.SetGuardCheck(nil)

	pool.SetProOnlyModels([]string{"pro-model"})
	if got := pickUIDs(t, pool, "pro-model", 6); !reflectEqual(countsOnly(got), []string{"a"}) {
		t.Fatalf("pro-only picks = %v, want only a", got)
	}
	if got := pickUIDs(t, pool, "free-model", 4); len(countPicks(got)) != 2 {
		t.Fatalf("non-pro model should use both accounts: %v", got)
	}
}

func TestPoolGroupsAndBatchActions(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, map[string]any{"group": " Team A ", "enabled": false})
	writePoolAccount(t, dir, "b", realmIntl, map[string]any{"group": "Team A"})
	writePoolAccount(t, dir, "c", realmIntl, map[string]any{"group": "Team B"})
	writePoolAccount(t, dir, "none", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)

	groups := pool.Groups()
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want two", groups)
	}
	if groups[0].Name != "Team A" || groups[0].Count != 2 || groups[0].Enabled != 1 {
		t.Fatalf("Team A summary = %+v", groups[0])
	}
	if got := poolUIDs(pool.GroupAccounts("Team A")); !reflectEqual(got, []string{"a", "b"}) {
		t.Fatalf("Team A accounts = %v", got)
	}
	changed, err := pool.SetGroupEnabled("Team A", true)
	if err != nil {
		t.Fatalf("SetGroupEnabled: %v", err)
	}
	if !reflectEqual(changed, []string{"a", "b"}) {
		t.Fatalf("changed = %v", changed)
	}
	if !pool.Get("a").Auth.Enabled || !pool.Get("a").Auth.TasksEnabled {
		t.Fatalf("batch enable did not update a: %+v", pool.Get("a").Auth)
	}
	if !pool.Get("c").Auth.Enabled {
		t.Fatal("batch enable touched another group")
	}
	changed, err = pool.SetGroupEnabled("Team A", false)
	if err != nil {
		t.Fatalf("SetGroupEnabled: %v", err)
	}
	if !reflectEqual(changed, []string{"a", "b"}) || pool.Get("b").Auth.Enabled {
		t.Fatalf("batch disable failed: changed=%v b=%+v", changed, pool.Get("b").Auth)
	}
	if changed, err := pool.SetGroupEnabled("", false); err != nil || len(changed) != 0 {
		t.Fatalf("empty group action = %v/%v", changed, err)
	}
	if err := pool.SetGroup("none", strings.Repeat("x", 200)); err != nil {
		t.Fatal(err)
	}
	if got := len(pool.Get("none").Group); got != 64 {
		t.Fatalf("group length = %d, want 64", got)
	}
	if err := pool.SetGroup("none", "  Team C  "); err != nil {
		t.Fatal(err)
	}
	if pool.Get("none").Group != "Team C" {
		t.Fatalf("group = %q, want Team C", pool.Get("none").Group)
	}
}

func TestPoolSettersPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)

	if err := pool.SetNote("a", "  batch  "); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetTasks("a", false); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetWeight("a", 250); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetGroup("a", "  Team A  "); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetDisabledModels("a", []string{" GLM-5.3 ", "glm-5.3"}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetTier("a", map[string]any{"paid": true, "package": "Max"}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetWeight("a", 0); err != nil {
		t.Fatal(err)
	}
	if pool.Get("a").Auth.Weight != 1 {
		t.Fatalf("weight after zero = %d, want 1", pool.Get("a").Auth.Weight)
	}
	if err := pool.SetWeight("a", 12); err != nil {
		t.Fatal(err)
	}

	if err := pool.SetNote("missing", "x"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("SetNote missing = %v, want ErrAccountNotFound", err)
	}
	if _, err := pool.Pick("m", "", nil); err != nil {
		t.Fatalf("Pick after tasks disable: %v", err)
	}

	_, reloaded := newPoolProvider(t, realmIntl, dir)
	again := reloaded.Get("a")
	if again == nil {
		t.Fatal("reloaded account missing")
	}
	if again.Auth.Note != "batch" || again.Auth.TasksEnabled || again.Auth.Weight != 12 {
		t.Fatalf("reloaded note/tasks/weight = %q/%v/%d", again.Auth.Note, again.Auth.TasksEnabled, again.Auth.Weight)
	}
	if again.Auth.Group != "Team A" || again.Tier != "Max" || !again.IsPaidUser() {
		t.Fatalf("reloaded group/tier = %q/%q paid=%v", again.Auth.Group, again.Tier, again.IsPaidUser())
	}
	if !reflectEqual(again.Auth.DisabledModels, []string{"glm-5.3"}) {
		t.Fatalf("reloaded disabled models = %v", again.Auth.DisabledModels)
	}
}

func TestPoolManualDisableEnableAndPersistence(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	writePoolAccount(t, dir, "b", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)

	if err := pool.Disable("a", "operator decision"); err != nil {
		t.Fatal(err)
	}
	view := pool.Get("a").View()
	if view.Enabled || view.TasksEnabled || !strings.Contains(view.LastError, "operator decision") {
		t.Fatalf("disable view = %+v", view)
	}
	if got := pickUIDs(t, pool, "m", 6); !reflectEqual(countsOnly(got), []string{"b"}) {
		t.Fatalf("disabled account picked: %v", got)
	}

	_, reloaded := newPoolProvider(t, realmIntl, dir)
	if reloaded.Get("a").Auth.Enabled {
		t.Fatal("manual disable did not persist")
	}
	if err := reloaded.Enable("a"); err != nil {
		t.Fatal(err)
	}
	reloadedA := reloaded.Get("a")
	if !reloadedA.Auth.Enabled || !reloadedA.Auth.TasksEnabled {
		t.Fatalf("enable did not restore auth: %+v", reloadedA.Auth)
	}
	if reloadedA.State.Disabled || reloadedA.State.ManualDisabled || !reloadedA.State.CoolUntil.IsZero() {
		t.Fatalf("enable left runtime state: %+v", reloadedA.State)
	}
	if got := pickUIDs(t, reloaded, "m", 6); len(countPicks(got)) != 2 {
		t.Fatalf("enabled account never picked: %v", got)
	}
}

func TestPoolProxyMaskingAndNoTokenExposure(t *testing.T) {
	dir := t.TempDir()
	writePoolSettings(t, dir, map[string]any{
		"proxy_slots": []any{
			map[string]any{
				"id":      "p1",
				"url":     "http://alice:supersecret@proxy.example:8080",
				"enabled": true,
			},
		},
	})
	path := writePoolAccount(t, dir, "a", realmIntl, map[string]any{
		"accessToken":  "access-token-very-secret",
		"refreshToken": "refresh-token-very-secret",
	})
	_, pool := newPoolProvider(t, realmIntl, dir)

	if err := pool.SetProxySlot("a", "p1"); err != nil {
		t.Fatalf("SetProxySlot: %v", err)
	}
	account := pool.Get("a")
	if got := account.ProxyURL(); !strings.Contains(got, "supersecret") {
		t.Fatalf("runtime proxy = %q, want raw URL", got)
	}
	view := account.View()
	if strings.Contains(view.Proxy, "supersecret") || !strings.Contains(view.Proxy, "<redacted>") {
		t.Fatalf("view proxy = %q, want masked", view.Proxy)
	}
	if view.CredentialMask != "" {
		t.Fatalf("OAuth credential mask = %q, want empty", view.CredentialMask)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, secret := range []string{"access-token-very-secret", "refresh-token-very-secret", "supersecret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("public view leaked %q: %s", secret, text)
		}
	}
	if strings.Contains(text, "accessToken") || strings.Contains(text, "refreshToken") {
		t.Fatalf("public view contains token fields: %s", text)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["proxySlot"] != "p1" {
		t.Fatalf("proxySlot = %v, want p1", saved["proxySlot"])
	}

	_, reloaded := newPoolProvider(t, realmIntl, dir)
	if got := reloaded.Get("a").ProxyURL(); !strings.Contains(got, "supersecret") {
		t.Fatalf("reloaded runtime proxy = %q", got)
	}

	reloaded.ApplyProxySlots(nil)
	if got := reloaded.Get("a").ProxyURL(); got != "" {
		t.Fatalf("proxy after slot removal = %q, want direct", got)
	}
	reloaded.ApplyProxySlots(transport.Slots{{ID: "p1", URL: "http://bob:another@proxy.example:8080", Enabled: true}})
	if got := reloaded.Get("a").ProxyURL(); !strings.Contains(got, "another") {
		t.Fatalf("proxy after slot update = %q", got)
	}
}

func TestPoolSaveAccountPreservesUnknownExtrasAndHidesRuntimeState(t *testing.T) {
	dir := t.TempDir()
	path := writePoolAccount(t, dir, "a", realmIntl, map[string]any{
		"futureField": map[string]any{"nested": []any{1, 2}},
		"credits":     poolCredits(3),
		"modelCaps":   map[string]any{"m": map[string]any{"at": 1, "reset": 2}},
	})
	_, pool := newPoolProvider(t, realmIntl, dir)
	account := pool.Get("a")
	account.mu.Lock()
	account.State.CoolUntil = time.Now().Add(time.Hour)
	account.State.LastError = "runtime only"
	account.State.ErrCount = 2
	account.mu.Unlock()

	if err := pool.SetNote("a", "persisted note"); err != nil {
		t.Fatal(err)
	}
	account.mu.Lock()
	account.Group = "Direct Group"
	account.Weight = 9
	account.DisabledModels = map[string]bool{"direct-model": true}
	account.mu.Unlock()
	if err := pool.SaveAccount(account); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("saved account is not JSON: %v", err)
	}
	for _, key := range []string{"futureField", "credits", "modelCaps", "note"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("saved account lost %q: %s", key, data)
		}
	}
	for _, key := range []string{"lastError", "lastErrorDetail", "cooldownUntil"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("saved account persisted runtime field %q: %s", key, data)
		}
	}

	_, reloaded := newPoolProvider(t, realmIntl, dir)
	again := reloaded.Get("a")
	if _, ok := again.Auth.Extra["futureField"]; !ok {
		t.Fatalf("extra field lost after reload: %v", again.Auth.Extra)
	}
	if again.Auth.Note != "persisted note" {
		t.Fatalf("note after reload = %q", again.Auth.Note)
	}
	if again.Auth.Group != "Direct Group" || again.Auth.Weight != 9 {
		t.Fatalf("mirrors after reload = %q/%d", again.Auth.Group, again.Auth.Weight)
	}
	if !reflectEqual(again.Auth.DisabledModels, []string{"direct-model"}) {
		t.Fatalf("disabled models after reload = %v", again.Auth.DisabledModels)
	}
	if again.Credits == nil || len(again.Credits.Packages) != 1 {
		t.Fatalf("credits after reload = %+v", again.Credits)
	}
	if !again.State.CoolUntil.IsZero() || again.State.LastError != "" {
		t.Fatalf("runtime state came back from disk: %+v", again.State)
	}
}

func TestPoolAddAndRemovePreserveStableFields(t *testing.T) {
	dir := t.TempDir()
	_, pool := newPoolProvider(t, realmIntl, dir)
	addedAt := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)

	first := Auth{
		UID:          "new-account",
		AccessToken:  "first-token",
		RefreshToken: "first-refresh",
		Realm:        realmIntl,
		AddedAt:      addedAt,
		Note:         "keep",
		Group:        "Team A",
		Weight:       5,
		DisabledModels: []string{
			"glm-5.3",
		},
		ProxySlot: "slot-1",
		Tier:      json.RawMessage(`{"paid":true,"package":"Pro"}`),
	}
	first.SetEnabled(false)
	first.SetTasksEnabled(true)
	account, err := pool.Add(first)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if account.Auth.UID != "new-account" {
		t.Fatalf("added uid = %q", account.Auth.UID)
	}
	if _, err := os.Stat(filepath.Join(dir, "new-account.json")); err != nil {
		t.Fatalf("added file missing: %v", err)
	}

	second := Auth{
		UID:          "new-account",
		AccessToken:  "second-token",
		RefreshToken: "second-refresh",
		Realm:        realmIntl,
	}
	second.SetEnabled(true)
	account, err = pool.Add(second)
	if err != nil {
		t.Fatalf("re-Add: %v", err)
	}
	if !account.Auth.AddedAt.Equal(addedAt) {
		t.Fatalf("addedAt = %v, want %v", account.Auth.AddedAt, addedAt)
	}
	if account.Auth.Note != "keep" || account.Auth.Group != "Team A" ||
		account.Auth.Weight != 5 || account.Auth.ProxySlot != "slot-1" {
		t.Fatalf("stable fields lost: %+v", account.Auth)
	}
	if len(account.Auth.Tier) == 0 || !reflectEqual(account.Auth.DisabledModels, []string{"glm-5.3"}) {
		t.Fatalf("tier/disabled models lost: %+v", account.Auth)
	}
	if account.Auth.AccessToken != "second-token" {
		t.Fatalf("new token was not stored: %q", account.Auth.AccessToken)
	}

	pool.BindSession("session", "new-account")
	if err := pool.Remove("new-account"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if pool.Get("new-account") != nil {
		t.Fatal("removed account still in Get")
	}
	if _, err := os.Stat(filepath.Join(dir, "new-account.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed account file still exists: %v", err)
	}
	if err := pool.Remove("new-account"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("Remove missing = %v, want ErrAccountNotFound", err)
	}
	if got := pool.affinity.recent("session"); len(got) != 0 {
		t.Fatalf("affinity retained removed uid: %v", got)
	}
}

func TestPoolDeterministicFallbackRotationAndNoStarvation(t *testing.T) {
	dir := t.TempDir()
	for _, uid := range []string{"d", "b", "a", "c"} {
		writePoolAccount(t, dir, uid, realmIntl, nil)
	}
	_, pool := newPoolProvider(t, realmIntl, dir)
	want := []string{"a", "b", "c", "d", "a", "b", "c", "d"}
	if got := pickUIDs(t, pool, "m", len(want)); !reflectEqual(got, want) {
		t.Fatalf("fallback rotation = %v, want %v", got, want)
	}

	_, fresh := newPoolProvider(t, realmIntl, dir)
	if got := pickUIDs(t, fresh, "m", len(want)); !reflectEqual(got, want) {
		t.Fatalf("fresh fallback rotation = %v, want %v", got, want)
	}

	_, fairness := newPoolProvider(t, realmIntl, dir)
	counts := countPicks(pickUIDs(t, fairness, "m", 100))
	for _, uid := range []string{"a", "b", "c", "d"} {
		if counts[uid] != 25 {
			t.Fatalf("starvation for %s: %v", uid, counts)
		}
	}

	_, excluded := newPoolProvider(t, realmIntl, dir)
	exclude := map[string]bool{"b": true}
	seen := map[string]bool{}
	for i := 0; i < 24; i++ {
		account, err := excluded.Pick("m", "", exclude)
		if err != nil {
			t.Fatal(err)
		}
		if account.Auth.UID == "b" {
			t.Fatal("excluded account picked")
		}
		seen[account.Auth.UID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("excluded rotation starved accounts: %v", seen)
	}
}

func TestPoolStatsListTasksAndReadiness(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, map[string]any{"tasksEnabled": false})
	writePoolAccount(t, dir, "b", realmIntl, map[string]any{"tasksEnabled": false})
	writePoolAccount(t, dir, "c", realmIntl, map[string]any{"enabled": false, "tasksEnabled": true})
	_, pool := newPoolProvider(t, realmIntl, dir)

	b := pool.Get("b")
	b.mu.Lock()
	b.State.CoolUntil = time.Now().Add(time.Hour)
	b.mu.Unlock()

	stats := pool.Stats()
	if stats.Total != 3 || stats.Disabled != 1 || stats.Cooling != 1 || stats.Healthy != 1 {
		t.Fatalf("stats = %+v, want total=3 disabled=1 cooling=1 healthy=1", stats)
	}
	if got := poolUIDs(pool.Accounts()); !reflectEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("accounts = %v", got)
	}
	views := pool.List()
	if len(views) != 3 || views[0].UID != "a" || views[2].UID != "c" {
		t.Fatalf("list = %+v", views)
	}
	if got := poolUIDs(pool.TaskAccounts()); !reflectEqual(got, []string{"c"}) {
		t.Fatalf("task accounts = %v, want [c]", got)
	}
	if pool.CountReady("m") != 1 {
		t.Fatalf("CountReady = %d, want 1", pool.CountReady("m"))
	}
	if pool.Stats().Healthy+pool.Stats().Cooling+pool.Stats().Disabled != pool.Stats().Total {
		t.Fatalf("stats buckets do not add up: %+v", pool.Stats())
	}
}

func TestPoolTierPersistenceAndUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := writePoolAccount(t, dir, "a", realmIntl, map[string]any{
		"unknownTop": "keep",
	})
	_, pool := newPoolProvider(t, realmIntl, dir)
	if err := pool.SetTier("a", map[string]any{"paid": false, "package": "Free"}); err != nil {
		t.Fatal(err)
	}
	if pool.Get("a").Tier != "Free" || pool.Get("a").IsPaidUser() {
		t.Fatalf("tier = %q paid=%v", pool.Get("a").Tier, pool.Get("a").IsPaidUser())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved["unknownTop"]; !ok {
		t.Fatalf("unknown top field lost: %s", raw)
	}
	if _, ok := saved["tier"]; !ok {
		t.Fatalf("tier was not persisted: %s", raw)
	}
}

func TestAccountStateHealthyBoundaries(t *testing.T) {
	now := time.Unix(1000, 0)
	account := &Account{Auth: Auth{UID: "u", AccessToken: "t", Enabled: true}}
	if !account.Healthy(now) {
		t.Fatal("default account should be healthy")
	}
	account.State = accountstate.State{Disabled: true}
	if account.Healthy(now) {
		t.Fatal("runtime-disabled account should be unhealthy")
	}
	account.State = accountstate.State{CoolUntil: now.Add(time.Second)}
	if account.Healthy(now) {
		t.Fatal("cooling account should be unhealthy")
	}
	account.State = accountstate.State{CoolUntil: now}
	if !account.Healthy(now) {
		t.Fatal("cooldown ending exactly now should be healthy")
	}
	account.Auth.Enabled = false
	if account.Healthy(now) {
		t.Fatal("manual disabled account should be unhealthy")
	}
}

func TestPoolAPIKeyViewMasksCredential(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "key-a", realmCN, map[string]any{
		"accessToken": "ck_abcdefgh.supersecretvalue",
		"apiKey":      true,
		"source":      sourceAPIKey,
	})
	_, pool := newPoolProvider(t, realmCN, dir)
	view := pool.Get("key-a").View()
	if !view.APIKey {
		t.Fatal("API key flag missing from view")
	}
	if strings.Contains(view.CredentialMask, "supersecretvalue") {
		t.Fatalf("credential mask leaked the key: %q", view.CredentialMask)
	}
	if view.CredentialMask != "ck_abcdefgh.***" {
		t.Fatalf("credential mask = %q", view.CredentialMask)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "supersecretvalue") {
		t.Fatalf("API key leaked through view: %s", data)
	}
	if view.ExpiresAt != 0 || view.TokenExpired {
		t.Fatalf("static key view expiry = %d/%v", view.ExpiresAt, view.TokenExpired)
	}
	if view.CanCheckin || view.CanReportActivity {
		t.Fatalf("API key view advertises personal tasks: checkin=%v report=%v", view.CanCheckin, view.CanReportActivity)
	}
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal raw JSON: %v", err)
	}
	return json.RawMessage(data)
}

func TestPoolRemainingWeightsInterleavedModelsKeepRotationState(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	writePoolAccount(t, dir, "b", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)
	pool.ApplyRemainingWeights(map[string]map[string]int{
		"a": {"m": 3, "n": 2},
		"b": {"m": 2, "n": 3},
	})

	counts := map[string]map[string]int{
		"m": {},
		"n": {},
	}
	for round := 0; round < 10; round++ {
		for _, model := range []string{"m", "n"} {
			account, err := pool.Pick(model, "", nil)
			if err != nil {
				t.Fatalf("Pick(%s): %v", model, err)
			}
			counts[model][account.Auth.UID]++
		}
	}
	if counts["m"]["a"] != 6 || counts["m"]["b"] != 4 {
		t.Fatalf("model m counts = %v, want a=6 b=4", counts["m"])
	}
	if counts["n"]["a"] != 4 || counts["n"]["b"] != 6 {
		t.Fatalf("model n counts = %v, want a=4 b=6", counts["n"])
	}
}

func TestPoolProOnlyExplicitFreeTierBeatsPackageLabel(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "paid", realmIntl, map[string]any{
		"tier": map[string]any{"paid": true, "package": "Pro"},
	})
	writePoolAccount(t, dir, "free-pro-label", realmIntl, map[string]any{
		"tier":    map[string]any{"paid": false, "package": "Pro"},
		"credits": map[string]any{"remain": 100, "is_paid_user": true},
	})
	writePoolAccount(t, dir, "unknown-pro-label", realmIntl, map[string]any{
		"tier": map[string]any{"package": "Pro"},
	})
	_, pool := newPoolProvider(t, realmIntl, dir)

	free := pool.Get("free-pro-label")
	if free.Tier != "Pro" || free.IsPaidUser() {
		t.Fatalf("explicit free tier = %q paid=%v, want Pro/false", free.Tier, free.IsPaidUser())
	}
	unknown := pool.Get("unknown-pro-label")
	if unknown.IsPaidUser() {
		t.Fatalf("package label alone granted PRO: tier=%q", unknown.Tier)
	}
	pool.SetProOnlyModels([]string{"pro-model"})
	if got := pickUIDs(t, pool, "pro-model", 6); !reflectEqual(countsOnly(got), []string{"paid"}) {
		t.Fatalf("pro-only picks = %v, want only paid", got)
	}
}

func TestPoolSetProxySlotEmptyClearsLegacyAndDisabledFallsBack(t *testing.T) {
	dir := t.TempDir()
	writePoolSettings(t, dir, map[string]any{
		"proxy_slots": []any{
			map[string]any{"id": "enabled", "url": "http://slotuser:slotpass@slot.example:8080", "enabled": true},
			map[string]any{"id": "disabled", "url": "http://disableduser:disabledpass@slot.example:8080", "enabled": false},
		},
	})
	legacy := "http://legacyuser:legacypass@legacy.example:8080"
	writePoolAccount(t, dir, "direct", realmIntl, map[string]any{"proxy": legacy})
	writePoolAccount(t, dir, "fallback", realmIntl, map[string]any{"proxy": legacy})
	_, pool := newPoolProvider(t, realmIntl, dir)

	if got := pool.Get("direct").ProxyURL(); got != legacy {
		t.Fatalf("legacy proxy at load = %q, want %q", got, legacy)
	}
	if err := pool.SetProxySlot("direct", "enabled"); err != nil {
		t.Fatalf("SetProxySlot enabled: %v", err)
	}
	if got := pool.Get("direct").ProxyURL(); !strings.Contains(got, "slotpass") {
		t.Fatalf("slot proxy = %q", got)
	}
	if err := pool.SetNote("direct", "proxy survives credential saves"); err != nil {
		t.Fatal(err)
	}
	if got := pool.Get("direct").ProxyURL(); !strings.Contains(got, "slotpass") {
		t.Fatalf("credential save lost the runtime proxy: %q", got)
	}
	if err := pool.SetProxySlot("direct", ""); err != nil {
		t.Fatalf("SetProxySlot direct: %v", err)
	}
	if got := pool.Get("direct").ProxyURL(); got != "" {
		t.Fatalf("explicit direct proxy = %q, want empty", got)
	}
	if err := pool.SetNote("direct", "still direct"); err != nil {
		t.Fatal(err)
	}
	if got := pool.Get("direct").ProxyURL(); got != "" {
		t.Fatalf("credential save resurrected a direct proxy: %q", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "direct.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved["proxy"]; ok {
		t.Fatalf("explicit direct left legacy proxy on disk: %s", data)
	}

	if err := pool.SetProxySlot("fallback", "disabled"); err != nil {
		t.Fatalf("SetProxySlot disabled: %v", err)
	}
	if got := pool.Get("fallback").ProxyURL(); got != legacy {
		t.Fatalf("disabled slot fallback = %q, want legacy", got)
	}
	data, err = os.ReadFile(filepath.Join(dir, "fallback.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytesContains(data, "legacy.example") {
		t.Fatalf("disabled slot discarded the legacy fallback: %s", data)
	}

	_, reloaded := newPoolProvider(t, realmIntl, dir)
	if got := reloaded.Get("direct").ProxyURL(); got != "" {
		t.Fatalf("direct did not survive reload: %q", got)
	}
	if got := reloaded.Get("fallback").ProxyURL(); got != legacy {
		t.Fatalf("disabled-slot fallback did not survive reload: %q", got)
	}
}

func bytesContains(data []byte, text string) bool {
	return strings.Contains(string(data), text)
}

func TestPoolApplyExpiringWindowCoversAccountsAddedLater(t *testing.T) {
	dir := t.TempDir()
	_, pool := newPoolProvider(t, realmIntl, dir)
	pool.ApplyExpiringWindow(7)

	add := func(uid string, days float64) *Account {
		t.Helper()
		auth := Auth{
			UID:         uid,
			AccessToken: "token-" + uid,
			Realm:       realmIntl,
			Extra: map[string]json.RawMessage{
				"credits": mustRawJSON(t, poolCredits(days)),
			},
		}
		account, err := pool.Add(auth)
		if err != nil {
			t.Fatalf("Add(%s): %v", uid, err)
		}
		return account
	}

	urgent := add("added-urgent", 1)
	outside := add("added-outside", 30)
	if urgent.ExpiringWindowDays != 7 || outside.ExpiringWindowDays != 7 {
		t.Fatalf("added window = %d/%d, want 7/7", urgent.ExpiringWindowDays, outside.ExpiringWindowDays)
	}
	if got := pickUIDs(t, pool, "m", 8); !reflectEqual(countsOnly(got), []string{"added-urgent"}) {
		t.Fatalf("added expiring account was not prioritised: %v", got)
	}
}

func TestPoolManualWeightOutranksExpiringAndRemaining(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "heavy", realmIntl, map[string]any{
		"weight":  5,
		"credits": poolCredits(1),
	})
	writePoolAccount(t, dir, "soon", realmIntl, map[string]any{
		"credits": poolCredits(1),
	})
	writePoolAccount(t, dir, "later", realmIntl, map[string]any{
		"credits": poolCredits(30),
	})
	_, pool := newPoolProvider(t, realmIntl, dir)
	pool.ApplyExpiringWindow(7)
	pool.ApplyRemainingWeights(map[string]map[string]int{
		"soon":  {"m": 9},
		"later": {"m": 2},
	})
	if got := pickUIDs(t, pool, "m", 8); !reflectEqual(countsOnly(got), []string{"heavy"}) {
		t.Fatalf("manual weight did not outrank automatic priorities: %v", got)
	}
}

func TestPoolSetConfigCopiesProductsAndAppliesNewCap(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	writePoolAccount(t, dir, "b", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)

	cfg := Config{
		AccountMaxConcurrent: 1,
		AutoSwitchProducts:   []string{"workbuddy", "cli"},
	}
	pool.SetConfig(cfg)
	cfg.AutoSwitchProducts[0] = "mutated"
	got := pool.Config()
	if got.AccountMaxConcurrent != 1 || !reflectEqual(got.AutoSwitchProducts, []string{"workbuddy", "cli"}) {
		t.Fatalf("pool config = %+v (products copied?)", got)
	}
	got.AutoSwitchProducts[0] = "returned-mutation"
	if again := pool.Config(); !reflectEqual(again.AutoSwitchProducts, []string{"workbuddy", "cli"}) {
		t.Fatalf("returned products slice aliases pool state: %v", again.AutoSwitchProducts)
	}
	for _, account := range pool.Accounts() {
		if account.State.MaxConcurrent != 1 {
			t.Fatalf("%s max concurrent = %d, want 1", account.Auth.UID, account.State.MaxConcurrent)
		}
	}
	a := pool.Get("a")
	if !pool.Acquire(a) || pool.Acquire(a) {
		t.Fatalf("new cap was not enforced: active=%d", a.State.ActiveConcurrent)
	}
	pool.Release(a)

	added, err := pool.Add(Auth{UID: "late", AccessToken: "late-token", Realm: realmIntl})
	if err != nil {
		t.Fatalf("Add late: %v", err)
	}
	if added.State.MaxConcurrent != 1 {
		t.Fatalf("new account max concurrent = %d, want 1", added.State.MaxConcurrent)
	}
}

func TestPoolTaskActiveIndependentOfDispatch(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, map[string]any{"enabled": false, "tasksEnabled": true})
	_, pool := newPoolProvider(t, realmIntl, dir)
	account := pool.Get("a")
	if account.Auth.Enabled {
		t.Fatal("fixture should be dispatch-disabled")
	}
	if !account.TaskActive() {
		t.Fatal("tasks should remain active while dispatch is disabled")
	}
	if got := poolUIDs(pool.TaskAccounts()); !reflectEqual(got, []string{"a"}) {
		t.Fatalf("task accounts = %v, want [a]", got)
	}
	if err := pool.SetTasks("a", false); err != nil {
		t.Fatal(err)
	}
	if account.TaskActive() || len(pool.TaskAccounts()) != 0 {
		t.Fatal("SetTasks(false) did not stop task participation")
	}
}

func TestPoolAddRejectsCrossRealmAccount(t *testing.T) {
	dir := t.TempDir()
	_, pool := newPoolProvider(t, realmIntl, dir)
	_, err := pool.Add(Auth{UID: "cn-account", AccessToken: "token", Realm: realmCN})
	if !errors.Is(err, ErrRealmMismatch) {
		t.Fatalf("Add cn into intl pool = %v, want ErrRealmMismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "cn-account.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cross-realm Add wrote a file: %v", statErr)
	}
}

func TestPoolRemoveDeletesAliasesWithoutTouchingOtherRealm(t *testing.T) {
	dir := t.TempDir()
	canonical := writePoolAccount(t, dir, "shared", realmIntl, nil)

	writeAlias := func(name, realm string) string {
		t.Helper()
		payload := map[string]any{
			"uid":         "shared",
			"accessToken": "access-" + name,
			"realm":       realm,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	intlAlias := writeAlias("intl-alias", realmIntl)
	cnPath := writeAlias("cn-shared", realmCN)

	_, pool := newPoolProvider(t, realmIntl, dir)
	if got := len(pool.Accounts()); got != 1 {
		t.Fatalf("intl pool count = %d, want 1 after alias dedupe", got)
	}
	_, cnPool := newPoolProvider(t, realmCN, dir)
	if cnPool.Get("shared") == nil {
		t.Fatal("cn account missing before remove")
	}
	if err := pool.Remove("shared"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, path := range []string{canonical, intlAlias} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("intl file %s survived remove: %v", path, err)
		}
	}
	if _, err := os.Stat(cnPath); err != nil {
		t.Fatalf("cn file was touched by intl remove: %v", err)
	}
	_, reloadedCN := newPoolProvider(t, realmCN, dir)
	if reloadedCN.Get("shared") == nil {
		t.Fatal("remove of intl account deleted the cn account")
	}
}

func TestPoolSetNoteTruncatesRunes(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)
	long := strings.Repeat("界", 500)
	if err := pool.SetNote("a", long); err != nil {
		t.Fatal(err)
	}
	note := pool.Get("a").Auth.Note
	if utf8.RuneCountInString(note) != 200 {
		t.Fatalf("note rune length = %d, want 200", utf8.RuneCountInString(note))
	}
}

func TestPoolSetTierStringIsExplicitAndPersistent(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)
	if err := pool.SetTier("a", "pro"); err != nil {
		t.Fatal(err)
	}
	if pool.Get("a").Tier != "pro" || !pool.Get("a").IsPaidUser() {
		t.Fatalf("string tier = %q paid=%v", pool.Get("a").Tier, pool.Get("a").IsPaidUser())
	}
	pool.SetProOnlyModels([]string{"pro-model"})
	if got := pickUIDs(t, pool, "pro-model", 4); !reflectEqual(countsOnly(got), []string{"a"}) {
		t.Fatalf("string tier did not grant PRO: %v", got)
	}
	_, reloaded := newPoolProvider(t, realmIntl, dir)
	if again := reloaded.Get("a"); again.Tier != "pro" || !again.IsPaidUser() {
		t.Fatalf("reloaded string tier = %q paid=%v", again.Tier, again.IsPaidUser())
	}
}

func TestPoolTokenExpiredStatsViewAndHumanDelta(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	writePoolAccount(t, dir, "b", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)
	a := pool.Get("a")
	a.SetTokenExpired(true)

	view := a.View()
	if !view.TokenExpired || !view.NeedRelogin || view.ExpiresIn != "expired" {
		t.Fatalf("token-expired view = %+v", view)
	}
	stats := pool.Stats()
	if stats.Total != 2 || stats.Healthy != 1 || stats.Disabled != 0 || stats.Cooling != 0 {
		t.Fatalf("token-expired stats = %+v, want total=2 healthy=1", stats)
	}
	if pool.CountReady("m") != 1 {
		t.Fatalf("CountReady with token-expired account = %d, want 1", pool.CountReady("m"))
	}
	a.SetTokenExpired(false)
	if a.View().TokenExpired {
		t.Fatal("token-expired flag did not clear")
	}

	now := time.Unix(1_700_000_000, 0)
	a.mu.Lock()
	a.State.CoolUntil = now.Add(100 * time.Millisecond)
	a.mu.Unlock()
	short := a.viewAt(now)
	if !short.InCooldown || short.CooldownFor != nil {
		t.Fatalf("sub-second cooldown = inCooldown:%v cooldownFor:%v, want true/nil", short.InCooldown, short.CooldownFor)
	}
	a.mu.Lock()
	a.State.CoolUntil = now.Add(2 * time.Second)
	a.mu.Unlock()
	if got := a.viewAt(now).CooldownFor; got != 2 {
		t.Fatalf("2s cooldownFor = %v, want 2", got)
	}

	for _, test := range []struct {
		delta time.Duration
		want  string
	}{
		{-time.Second, "expired"},
		{10 * time.Second, "0 min"},
		{90 * time.Minute, "1.5 hours"},
		{5 * time.Hour, "5.0 hours"},
		{36 * time.Hour, "2 days"},
		{48 * time.Hour, "2 days"},
	} {
		if got := humanDelta(test.delta); got != test.want {
			t.Fatalf("humanDelta(%v) = %q, want %q", test.delta, got, test.want)
		}
	}
}

func TestPoolHealthStatsIgnoreTokenExpired(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "a", realmIntl, nil)
	writePoolAccount(t, dir, "b", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)
	pool.Get("a").SetTokenExpired(true)
	if got := pool.Stats().Healthy; got != 1 {
		t.Fatalf("healthy = %d, want 1", got)
	}
}

func TestPoolConcurrentAddAndTokenRefresh(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "existing", realmIntl, map[string]any{
		"expiresAt": time.Now().Add(time.Minute).Unix(),
	})
	provider, pool := newPoolProvider(t, realmIntl, dir)
	existing := pool.Get("existing")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"rotated-access","refreshToken":"rotated-refresh","expiresAt":4102444800000}}`))
	}))
	defer server.Close()
	provider.tokenBaseURL = server.URL
	provider.tokenTimeout = 2 * time.Second

	const workers = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = provider.ForceRefresh(context.Background(), existing)
		}()
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			auth := Auth{
				UID:          fmt.Sprintf("new-%02d", index),
				AccessToken:  "access",
				RefreshToken: "refresh",
				Realm:        realmIntl,
			}
			auth.SetEnabled(true)
			if _, err := pool.Add(auth); err != nil {
				t.Errorf("Add new-%02d: %v", index, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if got := len(pool.Accounts()); got != workers+1 {
		t.Fatalf("accounts = %d, want %d", got, workers+1)
	}
}

func TestSessionAffinityChurnKeepsOrderBounded(t *testing.T) {
	affinity := newSessionAffinity(time.Hour, 8, 3)
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("churn-%03d", i)
		affinity.bind(key, "u-a")
		affinity.unbind(key)
		if i%2 == 0 {
			affinity.bind(key, "u-a")
			affinity.demote(key, "u-a")
		}
	}
	if len(affinity.bindings) != 0 || len(affinity.order) != 0 {
		t.Fatalf("churn retained bindings=%d order=%d", len(affinity.bindings), len(affinity.order))
	}

	long := strings.Repeat("s", 32*1024)
	affinity.bind(long, "u-long")
	if len(affinity.bindings) != 1 || len(affinity.order) != 1 {
		t.Fatalf("long key bindings=%d order=%d", len(affinity.bindings), len(affinity.order))
	}
	if key := affinity.order[0]; len(key) > providers.MaxSessionKeyBytes || key == long {
		t.Fatalf("long session key was not capped/hashed: len=%d", len(key))
	}
	if got := affinity.recent(long); len(got) != 1 || got[0] != "u-long" {
		t.Fatalf("long key recent = %v", got)
	}

	now := time.Unix(1_700_000_000, 0)
	affinity.now = func() time.Time { return now }
	affinity.bind("expires", "u-exp")
	now = now.Add(2 * time.Hour)
	if got := affinity.recent("expires"); len(got) != 0 {
		t.Fatalf("expired affinity = %v", got)
	}
	if len(affinity.order) != 1 || affinity.order[0] == "expires" {
		t.Fatalf("expired key left in order: %v", affinity.order)
	}
}

func TestSetGroupEnabledSeedsRestrictionWindow(t *testing.T) {
	dir := t.TempDir()
	until := float64(time.Now().Add(time.Hour).Unix())
	writePoolAccount(t, dir, "restricted", realmIntl, map[string]any{
		"group": "team", "enabled": false, "restrictedUntil": until,
	})
	_, pool := newPoolProvider(t, realmIntl, dir)
	account := pool.Get("restricted")
	account.mu.Lock()
	account.State.CoolUntil = time.Time{}
	account.mu.Unlock()
	if _, err := pool.SetGroupEnabled("team", true); err != nil {
		t.Fatalf("SetGroupEnabled: %v", err)
	}
	account.mu.RLock()
	coolUntil := account.State.CoolUntil
	enabled := account.Auth.Enabled
	account.mu.RUnlock()
	if !enabled {
		t.Fatal("group enable did not persist enabled state")
	}
	if !coolUntil.After(time.Now()) {
		t.Fatalf("group enable cleared persisted restriction window: coolUntil=%v", coolUntil)
	}
}

func TestAdmissionWaitUsesSchedulingGuardHook(t *testing.T) {
	dir := t.TempDir()
	writePoolAccount(t, dir, "guarded", realmIntl, nil)
	_, pool := newPoolProvider(t, realmIntl, dir)
	pool.SetGuardCheck(func(*Account, string) bool { return true })
	wait, reason := pool.AdmissionWait("m", nil)
	if wait <= 0 || reason == "" {
		t.Fatalf("guard hook admission wait = %v/%q, want a wait", wait, reason)
	}
}
