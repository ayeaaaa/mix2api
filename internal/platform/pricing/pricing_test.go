package pricing

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
	"testing"
	"time"
)

func tempDirs(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	accounts := filepath.Join(root, "accounts")
	usage := filepath.Join(root, "usage")
	if err := os.MkdirAll(accounts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(usage, 0o755); err != nil {
		t.Fatal(err)
	}
	return accounts, usage
}

func TestSnapshotLoadsEmbeddedAndFileOverride(t *testing.T) {
	accounts, usage := tempDirs(t)
	t.Setenv("WB_PRICING_FILE", "")
	snapshot, err := Load(accounts, usage)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) <= 10 {
		t.Fatalf("expected the shipped catalogue, got %d models", len(snapshot.Models))
	}
	if snapshot.Meta.USDCNY <= 0 {
		t.Fatalf("missing usd_cny: %+v", snapshot.Meta)
	}
	for id, entry := range snapshot.Models {
		if entry.Currency != "USD" {
			t.Fatalf("%s currency = %q", id, entry.Currency)
		}
		if entry.Unit <= 0 {
			t.Fatalf("%s has no unit", id)
		}
		if entry.Flat.InputCacheMiss == 0 && !entry.Flat.NullInputCacheHit {
			t.Fatalf("%s has no cache-miss rate", id)
		}
		for _, band := range entry.Bands {
			if !ValidBand(band) {
				t.Fatalf("%s has invalid band %+v", id, band)
			}
		}
	}

	override := map[string]any{
		"meta": map[string]any{"usd_cny": 7.0, "base_currency": "CNY"},
		"models": map[string]any{
			"only-model": map[string]any{
				"display": "only-model", "source": "openrouter", "currency": "USD",
				"unit": 1000000, "or_id": "vendor/only-model",
				"flat": map[string]any{"input_cache_hit": 0.1, "input_cache_miss": 1.0, "output": 2.0},
			},
		},
	}
	path := filepath.Join(t.TempDir(), "pricing.json")
	data, _ := json.Marshal(override)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB_PRICING_FILE", path)
	snapshot, err = Load(accounts, usage)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) != 1 {
		t.Fatalf("file override not used: %d models", len(snapshot.Models))
	}
	if _, ok := snapshot.Models["only-model"]; !ok {
		t.Fatal("override model missing")
	}

	// A malformed external file falls back to the embedded copy.
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err = Load(accounts, usage)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Models) <= 10 {
		t.Fatalf("malformed override did not fall back: %d models", len(snapshot.Models))
	}
}

func TestBandSelectionByUTCTime(t *testing.T) {
	entry := Entry{
		Flat: Rates{InputCacheHit: 0.1, InputCacheMiss: 1, Output: 2},
		Bands: []Band{
			{Days: []string{"sat", "sun"}, Start: 0, End: 2400, Flat: Rates{InputCacheHit: 1, InputCacheMiss: 2, Output: 3}},
		},
	}
	saturday := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC).Unix()
	weekday := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC).Unix()
	rates, band := BandFor(entry, float64(saturday), 100)
	if band == nil || *band != 0 || rates.InputCacheMiss != 2 {
		t.Fatalf("weekend band not selected: rates=%+v band=%v", rates, band)
	}
	rates, band = BandFor(entry, float64(weekday), 100)
	if band != nil || rates.InputCacheMiss != 1 {
		t.Fatalf("weekday should fall back to flat: rates=%+v band=%v", rates, band)
	}

	wrapping := Entry{
		Flat: Rates{InputCacheHit: 0.1, InputCacheMiss: 1, Output: 2},
		Bands: []Band{
			{Start: 1500, End: 100, Flat: Rates{InputCacheHit: 0.01, InputCacheMiss: 0.1, Output: 0.2}},
		},
	}
	late := time.Date(2026, 1, 5, 23, 0, 0, 0, time.UTC).Unix()
	early := time.Date(2026, 1, 6, 0, 30, 0, 0, time.UTC).Unix()
	for _, epoch := range []int64{late, early} {
		if _, band := BandFor(wrapping, float64(epoch), 1); band == nil {
			t.Fatalf("wrapping window missed at %v", time.Unix(epoch, 0).UTC())
		}
	}
	noon := time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC).Unix()
	if rates, band := BandFor(wrapping, float64(noon), 1); band != nil || rates.InputCacheMiss != 1 {
		t.Fatalf("outside wrapping window should use flat: %+v %v", rates, band)
	}
}

func TestLengthBandsTakeTightestMatch(t *testing.T) {
	entry := Entry{
		Flat: Rates{InputCacheHit: 0.1, InputCacheMiss: 1, Output: 2},
		Bands: []Band{
			{MinPromptTokens: 100, Flat: Rates{InputCacheMiss: 11}},
			{MinPromptTokens: 1000, Flat: Rates{InputCacheMiss: 111}},
		},
	}
	rates, band := BandFor(entry, 0, 1500)
	if band == nil || *band != 1 || rates.InputCacheMiss != 111 {
		t.Fatalf("tightest band not chosen: %+v %v", rates, band)
	}
	rates, band = BandFor(entry, 0, 500)
	if band == nil || *band != 0 || rates.InputCacheMiss != 11 {
		t.Fatalf("lower band not chosen: %+v %v", rates, band)
	}
	rates, band = BandFor(entry, 0, 50)
	if band != nil || rates.InputCacheMiss != 1 {
		t.Fatalf("below lowest threshold should use base: %+v %v", rates, band)
	}
}

func TestComputeRowCacheSplitAndUSDConversion(t *testing.T) {
	accounts, usage := tempDirs(t)
	snapshot := &Snapshot{
		Meta:        Meta{USDCNY: 7},
		Models:      map[string]Entry{"m": {Currency: "USD", Unit: 1_000_000, Flat: Rates{InputCacheHit: 1, InputCacheMiss: 2, Output: 3}}},
		accountsDir: accounts,
		usageDir:    usage,
	}
	row := map[string]any{
		"model": "m", "at": float64(0), "prompt_tokens": 1000,
		"cached_tokens": 400, "completion_tokens": 500,
	}
	cost := snapshot.CostForRow(row)
	if !cost.Known || cost.Disabled {
		t.Fatalf("unexpected cost state: %+v", cost)
	}
	want := (600*2.0 + 400*1.0 + 500*3.0) / 1_000_000 * 7
	if diff := cost.CNY - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cny = %v, want %v", cost.CNY, want)
	}
	if cost.Rates == nil || cost.Rates.Output != 3 {
		t.Fatalf("rates not exposed: %+v", cost.Rates)
	}
}

func TestUnknownModelNeverGuesses(t *testing.T) {
	accounts, usage := tempDirs(t)
	snapshot := &Snapshot{Meta: Meta{USDCNY: 7}, Models: map[string]Entry{}, accountsDir: accounts, usageDir: usage}
	cost := snapshot.CostForRow(map[string]any{"model": "missing", "prompt_tokens": 1000})
	if cost.Known || cost.CNY != 0 || cost.Rates != nil || cost.Band != nil {
		t.Fatalf("unknown model was guessed: %+v", cost)
	}
}

func TestPricingDisabled(t *testing.T) {
	accounts, usage := tempDirs(t)
	settings := map[string]any{"pricing_enabled": false, "future_key": "kept"}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(accounts, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := &Snapshot{
		Meta:        Meta{USDCNY: 7},
		Models:      map[string]Entry{"m": {Currency: "USD", Unit: 1_000_000, Flat: Rates{InputCacheMiss: 1}}},
		accountsDir: accounts, usageDir: usage,
	}
	cost := snapshot.CostForRow(map[string]any{"model": "m", "prompt_tokens": 10})
	if !cost.Disabled || cost.Known || cost.CNY != 0 || cost.Rates != nil {
		t.Fatalf("disabled pricing leaked details: %+v", cost)
	}
	if id, err := snapshot.EnsurePolicy("m"); err != nil || id != "" {
		t.Fatalf("disabled pricing minted a policy: %q %v", id, err)
	}
	if _, err := os.Stat(filepath.Join(usage, policiesName)); !os.IsNotExist(err) {
		t.Fatalf("policy file was written while disabled: %v", err)
	}
	// Settings unknown keys survive.
	var stored map[string]any
	decoder := json.NewDecoder(mustOpen(t, filepath.Join(accounts, "settings.json")))
	decoder.UseNumber()
	_ = decoder.Decode(&stored)
	if stored["future_key"] != "kept" {
		t.Fatalf("settings unknown key lost: %+v", stored)
	}
}

func TestMarkupFactor(t *testing.T) {
	accounts, usage := tempDirs(t)
	snapshot := &Snapshot{
		Meta:        Meta{USDCNY: 1},
		Models:      map[string]Entry{"m": {Currency: "USD", Unit: 1_000_000, Flat: Rates{InputCacheMiss: 2}}},
		accountsDir: accounts, usageDir: usage,
	}
	base := snapshot.CostForRow(map[string]any{"model": "m", "prompt_tokens": 1_000_000}).CNY
	write := func(value string) {
		if err := os.WriteFile(filepath.Join(usage, markupName), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"m": 2.5}`)
	scaled := snapshot.CostForRow(map[string]any{"model": "m", "prompt_tokens": 1_000_000}).CNY
	if scaled != base*2.5 {
		t.Fatalf("markup not applied: %v vs %v", scaled, base*2.5)
	}
	for _, invalid := range []string{`{"m": 0}`, `{"m": -1}`, `{"m": "x"}`, `{broken`} {
		write(invalid)
		if got := snapshot.CostForRow(map[string]any{"model": "m", "prompt_tokens": 1_000_000}).CNY; got != base {
			t.Fatalf("invalid markup %q changed price: %v", invalid, got)
		}
	}
}

func TestPolicyTimelineLookup(t *testing.T) {
	accounts, usage := tempDirs(t)
	snapshot := &Snapshot{Meta: Meta{USDCNY: 1}, Models: map[string]Entry{}, accountsDir: accounts, usageDir: usage}
	first := Entry{Currency: "USD", Unit: 1_000_000, Flat: Rates{InputCacheMiss: 1}}
	second := Entry{Currency: "USD", Unit: 1_000_000, Flat: Rates{InputCacheMiss: 2}}
	id1 := policyID("m", first.Flat, nil, 1)
	id2 := policyID("m", second.Flat, nil, 1)
	policies := []map[string]any{
		{"id": id1, "model": "m", "currency": "USD", "unit": 1_000_000, "usd_cny": 1, "flat": first.Flat, "first_seen": 200},
		{"id": id2, "model": "m", "currency": "USD", "unit": 1_000_000, "usd_cny": 1, "flat": second.Flat, "first_seen": 300},
	}
	if err := appendJSONL(filepath.Join(usage, policiesName), policies); err != nil {
		t.Fatal(err)
	}
	if err := appendJSONL(filepath.Join(usage, timelineName), []map[string]any{
		{"at": 100, "policies": map[string]string{"other": id1}},
		{"at": 200, "policies": map[string]string{"m": id1}},
		{"at": 300, "policies": map[string]string{"m": id2}},
	}); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"model": "m", "at": 250, "prompt_tokens": 1_000_000}
	cost := snapshot.CostForRow(row)
	if !cost.Known || cost.CNY != 1 || cost.Policy != id1 || cost.Backfilled {
		t.Fatalf("row at T did not use policy in force: %+v", cost)
	}
	row["at"] = 350
	cost = snapshot.CostForRow(row)
	if cost.CNY != 2 || cost.Policy != id2 {
		t.Fatalf("row after T2 did not use second policy: %+v", cost)
	}
	// A row before the model first appears in the timeline gets the first
	// later policy, marked backfilled.
	row["at"] = 50
	cost = snapshot.CostForRow(row)
	if !cost.Backfilled || cost.Policy != id1 {
		t.Fatalf("missing policy did not backfill: %+v", cost)
	}
	if snapshot.CurrentPolicyID("m") != id2 {
		t.Fatalf("current policy = %q, want %q", snapshot.CurrentPolicyID("m"), id2)
	}
	pricer := snapshot.NewRowPricer()
	for _, at := range []float64{50, 250, 350} {
		row["at"] = at
		want := snapshot.CostForRow(row)
		got := pricer.CostForRow(row)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("RowPricer at %v = %+v, want %+v", at, got, want)
		}
	}
	// Timeline is a public defensive copy; mutating it must not poison the
	// immutable view used by per-row pricing.
	timeline := snapshot.Timeline()
	timeline[1].Policies["m"] = "mutated"
	if got := snapshot.Timeline()[1].Policies["m"]; got != id1 {
		t.Fatalf("Timeline returned shared state: %q", got)
	}
	// Repeated EnsurePolicy is idempotent and does not rewrite old rows.
	before, _ := os.ReadFile(filepath.Join(usage, policiesName))
	if _, err := snapshot.EnsurePolicy("m"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(usage, policiesName))
	if string(before) != string(after) {
		t.Fatal("EnsurePolicy rewrote the policy table")
	}
}

func TestOpenRouterNormalization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"vendor/base","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000005"},
			 "overrides":[{"prompt":"0.000003","completion":"0.000004","min_prompt_tokens":1000}]},
			{"id":"vendor/other","pricing":{"prompt":"0.000001","completion":"0.000001"}}
		]}`))
	}))
	defer server.Close()
	models, err := FetchOpenRouter(context.Background(), server.Client(), server.URL+"/models")
	if err != nil {
		t.Fatal(err)
	}
	base, ok := models["vendor/base"]
	if !ok || base.Prompt != 0.000001 || base.InputCacheRead != 0.0000005 {
		t.Fatalf("pricing not normalized: %+v", base)
	}
	entry := entryFor("base", "vendor/base", models, "", false)
	if entry.Flat.InputCacheMiss != 1 || entry.Flat.Output != 2 || entry.Flat.InputCacheHit != 0.5 {
		t.Fatalf("per-million conversion wrong: %+v", entry.Flat)
	}
	if len(entry.Bands) != 1 || entry.Bands[0].MinPromptTokens != 1000 {
		t.Fatalf("band not preserved: %+v", entry.Bands)
	}
	accounts, usage := tempDirs(t)
	snapshot := &Snapshot{Meta: Meta{USDCNY: 7}, Models: map[string]Entry{}, accountsDir: accounts, usageDir: usage}
	doc, unpriced, overridden := snapshot.BuildSnapshot(models, nil, []string{"base", "base-lkeap", "base-f", "base-x", "base-taiji"}, true)
	if _, ok := doc.Models["base-lkeap"]; !ok {
		t.Fatalf("variant suffix should inherit: unpriced=%v overridden=%v", unpriced, overridden)
	}
	if _, ok := doc.Models["base-taiji"]; !ok {
		// base-taiji strips to "base", which resolves uniquely.
		t.Fatalf("base-taiji should inherit: %v", unpriced)
	}
	for _, name := range []string{"base-f", "base-x"} {
		if _, ok := doc.Models[name]; ok {
			t.Fatalf("%s must not inherit", name)
		}
	}
	if _, ok := doc.Models["base"]; !ok {
		t.Fatalf("baseline model missing: %+v", doc.Models)
	}
}

func TestRefreshIntervalMigration(t *testing.T) {
	accounts, _ := tempDirs(t)
	writeSettings := func(value any) {
		data, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(accounts, "settings.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSettings(map[string]any{"pricing_refresh_hours": 6, "keep": true})
	if got := RefreshMinutes(accounts); got != 360 {
		t.Fatalf("hours migration = %v", got)
	}
	var stored map[string]any
	decoder := json.NewDecoder(mustOpen(t, filepath.Join(accounts, "settings.json")))
	decoder.UseNumber()
	_ = decoder.Decode(&stored)
	if stored["pricing_refresh_hours"] != nil || mustFloat(stored["pricing_refresh_minutes"]) != 360 {
		t.Fatalf("migration not written back: %+v", stored)
	}
	if stored["keep"] != true {
		t.Fatalf("unknown settings key lost: %+v", stored)
	}
	writeSettings(map[string]any{"pricing_refresh_hours": 0})
	if got := RefreshMinutes(accounts); got != 0 {
		t.Fatalf("legacy 0 should disable, got %v", got)
	}
	writeSettings(map[string]any{"pricing_refresh_hours": 720})
	if got := RefreshMinutes(accounts); got != 43200 {
		t.Fatalf("cap = %v", got)
	}
	writeSettings(map[string]any{"pricing_refresh_hours": "soon"})
	if got := RefreshMinutes(accounts); got != DefaultRefreshMinutes {
		t.Fatalf("invalid fallback = %v", got)
	}
	writeSettings(map[string]any{"pricing_refresh_minutes": -5})
	if got := RefreshMinutes(accounts); got != 0 {
		t.Fatalf("negative minutes = %v", got)
	}
	writeSettings(map[string]any{"pricing_refresh_minutes": 15, "pricing_refresh_hours": 6})
	if got := RefreshMinutes(accounts); got != 15 {
		t.Fatalf("minutes precedence = %v", got)
	}
	if got := SetRefreshMinutes(accounts, 1e9); got != 43200 {
		t.Fatalf("setter cap = %v", got)
	}
	if got := SetRefreshMinutes(accounts, -3); got != 0 {
		t.Fatalf("setter negative = %v", got)
	}
	if got := SetRefreshMinutes(accounts, "x"); got != 0 {
		t.Fatalf("setter invalid should keep current, got %v", got)
	}
}

func TestNextRefreshWait(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if got := NextRefreshWait(now, time.Time{}, 5); got != 5*time.Minute {
		t.Fatalf("first wait = %v", got)
	}
	if got := NextRefreshWait(now, now.Add(-2*time.Minute), 5); got != 3*time.Minute {
		t.Fatalf("interval wait = %v", got)
	}
	if got := NextRefreshWait(now, now.Add(-10*time.Minute), 5); got != 0 {
		t.Fatalf("overdue wait = %v", got)
	}
	if got := NextRefreshWait(now, time.Time{}, 0); got != 0 {
		t.Fatalf("disabled wait = %v", got)
	}

	accounts, usage := tempDirs(t)
	snapshot, err := Load(accounts, usage)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	refresher := NewRefresher(RefresherOptions{
		Snapshot: snapshot,
		Interval: func() float64 { return 0.001 },
		Fetch: func(ctx context.Context) (map[string]OpenRouterPrice, error) {
			calls++
			return map[string]OpenRouterPrice{"vendor/base": {Prompt: 0.000001, Completion: 0.000002}}, nil
		},
	})
	done := make(chan struct{})
	go func() {
		refresher.Run(context.Background())
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	refresher.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher stop was not prompt")
	}
	if calls == 0 {
		t.Fatal("refresher never ran")
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func mustFloat(value any) float64 {
	number, _ := asFloat(value)
	return number
}

func TestPolicyIDsMatchV1(t *testing.T) {
	accounts, usage := tempDirs(t)
	t.Setenv("WB_PRICING_FILE", "")
	snapshot, err := Load(accounts, usage)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"deepseek-v4.1-flash-sg": "b5d87bff661b",
		"hy4-preview":            "8928d1d8713f",
		"gpt-6-astra":            "7014b582a4ad",
		"hunyuan-chat":           "4d1762405f48",
		"minimax-m3.1-flash":     "7c35f27434aa",
	}
	for model, want := range cases {
		entry, ok := snapshot.Models[model]
		if !ok {
			t.Fatalf("missing model %s", model)
		}
		if got := policyID(model, entry.Flat, entry.Bands, snapshot.Meta.USDCNY); got != want {
			t.Fatalf("%s policy id = %s, want %s", model, got, want)
		}
	}
}

func TestInputCacheHitExplicitZeroAndMissingFallback(t *testing.T) {
	accounts, usage := tempDirs(t)
	snapshot := &Snapshot{
		Meta: Meta{USDCNY: 1}, Models: map[string]Entry{
			"zero": {Currency: "USD", Unit: 1_000_000, Flat: Rates{InputCacheHit: 0, InputCacheMiss: 1, Output: 2}},
		}, accountsDir: accounts, usageDir: usage,
	}
	row := map[string]any{"model": "zero", "prompt_tokens": 1000, "cached_tokens": 400}
	cost := snapshot.CostForRow(row)
	if !cost.Known || cost.CNY != 0.0006 {
		t.Fatalf("explicit zero cache-hit was billed as miss: %+v", cost)
	}
	if err := os.WriteFile(filepath.Join(usage, "pricing-policies.jsonl"), []byte(`{"id":"p1","model":"m","currency":"USD","unit":1000000,"usd_cny":1,"flat":{"input_cache_miss":1,"output":2},"first_seen":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usage, "pricing-timeline.jsonl"), []byte(`{"at":1,"policies":{"m":"p1"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := snapshot.CostForRow(map[string]any{"model": "m", "prompt_tokens": 1000, "cached_tokens": 400})
	if !missing.Known || missing.CNY != 0.001 {
		t.Fatalf("missing cache-hit should fall back to miss: %+v", missing)
	}
}

func TestEnsurePolicyMintsAndIsIdempotent(t *testing.T) {
	accounts, usage := tempDirs(t)
	snapshot := &Snapshot{Meta: Meta{USDCNY: 7.1}, Models: map[string]Entry{}, accountsDir: accounts, usageDir: usage}
	snapshot.RememberOpenRouter(map[string]OpenRouterPrice{
		"vendor/m": {Prompt: 0.000001, Completion: 0.000002, InputCacheReadPresent: true, InputCacheRead: 0.0000005},
	})
	first, err := snapshot.EnsurePolicy("m")
	if err != nil || first == "" {
		t.Fatalf("EnsurePolicy = %q %v", first, err)
	}
	second, err := snapshot.EnsurePolicy("m")
	if err != nil || second != first {
		t.Fatalf("EnsurePolicy not idempotent: %q -> %q (%v)", first, second, err)
	}
	policies, _ := os.ReadFile(filepath.Join(usage, policiesName))
	timeline, _ := os.ReadFile(filepath.Join(usage, timelineName))
	if len(policies) == 0 || len(timeline) == 0 {
		t.Fatalf("policy files were not minted: policies=%q timeline=%q", policies, timeline)
	}
	if !strings.Contains(string(policies), `"bands":null`) {
		t.Fatalf("policy row must carry v1 bands:null: %s", policies)
	}
	cost := snapshot.CostForRow(map[string]any{"model": "m", "prompt_tokens": 1_000_000})
	if !cost.Known || cost.CNY <= 0 || cost.Policy != first {
		t.Fatalf("minted policy is not usable: %+v", cost)
	}
}

func TestRefresherFailureBacksOff(t *testing.T) {
	accounts, usage := tempDirs(t)
	snapshot, err := Load(accounts, usage)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	refresher := NewRefresher(RefresherOptions{
		Snapshot: snapshot,
		Interval: func() float64 { return 0.001 },
		RetryMin: 20 * time.Millisecond,
		RetryMax: 40 * time.Millisecond,
		Fetch: func(context.Context) (map[string]OpenRouterPrice, error) {
			calls++
			return nil, errors.New("offline")
		},
	})
	done := make(chan struct{})
	go func() { refresher.Run(context.Background()); close(done) }()
	time.Sleep(120 * time.Millisecond)
	refresher.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher did not stop")
	}
	if calls == 0 {
		t.Fatal("refresher never attempted a fetch")
	}
	if calls > 8 {
		t.Fatalf("failure hot loop: %d calls", calls)
	}
	if failures := refresher.Status()["failures"].(int64); failures == 0 {
		t.Fatalf("failure count not recorded: %+v", refresher.Status())
	}
}

func TestRefresherDisabledDoesNotFetch(t *testing.T) {
	accounts, usage := tempDirs(t)
	snapshot, err := Load(accounts, usage)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	refresher := NewRefresher(RefresherOptions{
		Snapshot: snapshot,
		Interval: func() float64 { return 0 },
		Fetch: func(context.Context) (map[string]OpenRouterPrice, error) {
			calls++
			return nil, nil
		},
	})
	go refresher.Run(context.Background())
	time.Sleep(80 * time.Millisecond)
	refresher.Stop()
	if calls != 0 {
		t.Fatalf("disabled refresher fetched %d times", calls)
	}
}

func TestRefresherBusyManualRunOnceDoesNotChurn(t *testing.T) {
	accounts, usage := tempDirs(t)
	snapshot, err := Load(accounts, usage)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	refresher := NewRefresher(RefresherOptions{
		Snapshot: snapshot,
		Interval: func() float64 { return 0.001 },
		RetryMin: 20 * time.Millisecond,
		RetryMax: 40 * time.Millisecond,
		Fetch: func(context.Context) (map[string]OpenRouterPrice, error) {
			close(started)
			<-release
			return nil, errors.New("offline")
		},
	})
	firstDone := make(chan struct{})
	go func() { _ = refresher.RunOnce(context.Background()); close(firstDone) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("manual refresh never started")
	}
	secondStart := time.Now()
	err = refresher.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("busy RunOnce = %v", err)
	}
	if refresher.Status()["failures"].(int64) != 0 {
		t.Fatalf("busy RunOnce incremented failures: %+v", refresher.Status())
	}
	if elapsed := time.Since(secondStart); elapsed > 200*time.Millisecond {
		t.Fatalf("busy RunOnce blocked for %v", elapsed)
	}
	status := refresher.Status()
	if status["last_attempt"].(int64) == 0 {
		t.Fatalf("busy attempt not recorded: %+v", status)
	}
	if status["failures"].(int64) != 0 || status["busy"] != true {
		t.Fatalf("busy overlap counted as failure: %+v", status)
	}
	if status["next_run"].(int64) < status["last_attempt"].(int64) {
		t.Fatalf("busy overlap scheduled before its attempt: %+v", status)
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("manual refresh did not finish")
	}
}

func TestRefresherSuccessThenFailureAndDisabled(t *testing.T) {
	accounts, usage := tempDirs(t)
	snapshot, err := Load(accounts, usage)
	if err != nil {
		t.Fatal(err)
	}
	mode := "ok"
	refresher := NewRefresher(RefresherOptions{
		Snapshot: snapshot,
		Interval: func() float64 { return 5 },
		Fetch: func(context.Context) (map[string]OpenRouterPrice, error) {
			if mode == "fail" {
				return nil, errors.New("offline")
			}
			return map[string]OpenRouterPrice{
				"openai/gpt-6-astra": {Prompt: 0.000001, Completion: 0.000002, InputCacheReadPresent: true, InputCacheRead: 0.0000005},
			}, nil
		},
	})
	if err := refresher.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	success := refresher.Status()
	if success["last_run"].(int64) == 0 || success["failures"].(int64) != 0 {
		t.Fatalf("success status = %+v", success)
	}
	successRun := success["last_run"].(int64)
	time.Sleep(2 * time.Millisecond)
	mode = "fail"
	if err := refresher.RunOnce(context.Background()); err == nil {
		t.Fatal("failure did not surface")
	}
	failed := refresher.Status()
	if failed["last_run"].(int64) != successRun {
		t.Fatalf("last_run must stay on the last success: %+v", failed)
	}
	if failed["last_attempt"].(int64) == 0 || failed["failures"].(int64) != 1 {
		t.Fatalf("failure attempt not recorded: %+v", failed)
	}
	settings, _ := json.Marshal(map[string]any{"pricing_enabled": false})
	if err := os.WriteFile(filepath.Join(accounts, "settings.json"), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := refresher.RunOnce(context.Background()); err == nil {
		t.Fatal("disabled RunOnce did not fail")
	}
	disabled := refresher.Status()
	if disabled["last_run"].(int64) != successRun {
		t.Fatalf("disabled attempt changed last_run: %+v", disabled)
	}
	if disabled["failures"].(int64) != 2 {
		t.Fatalf("disabled attempt not counted: %+v", disabled)
	}
	if disabled["last_attempt"].(int64) == failed["last_attempt"].(int64) {
		t.Fatalf("disabled attempt did not advance last_attempt: %+v", disabled)
	}
}
