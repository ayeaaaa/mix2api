package usage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/pricing"
)

type testEnv struct {
	Root     string
	Accounts string
	Usage    string
	Paths    Paths
	Store    *Store
	Pricing  *pricing.Snapshot
	Cache    *Cache
	Now      time.Time
}

func newTestEnv(t *testing.T, rows []map[string]any) *testEnv {
	t.Helper()
	root := t.TempDir()
	accounts := filepath.Join(root, "accounts")
	usageDir := filepath.Join(root, "usage")
	if err := os.MkdirAll(accounts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(usageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pricingFile := filepath.Join(root, "pricing.json")
	doc := map[string]any{
		"meta": map[string]any{"usd_cny": 7.0, "base_currency": "CNY"},
		"models": map[string]any{
			"m": map[string]any{
				"display": "m", "source": "openrouter", "currency": "USD", "unit": 1000000,
				"or_id": "vendor/m",
				"flat":  map[string]any{"input_cache_hit": 1.0, "input_cache_miss": 2.0, "output": 3.0},
			},
		},
	}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(pricingFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB_PRICING_FILE", pricingFile)
	t.Setenv("WB_USAGE_CACHE", "0")
	snapshot, err := pricing.Load(accounts, usageDir)
	if err != nil {
		t.Fatal(err)
	}
	paths := pathsFor(usageDir)
	store := New(paths)
	for _, row := range rows {
		if err := store.Append(row); err != nil {
			t.Fatal(err)
		}
	}
	return &testEnv{
		Root: root, Accounts: accounts, Usage: usageDir,
		Paths: paths, Store: store, Pricing: snapshot,
		Cache: NewCache(usageDir), Now: time.Now(),
	}
}

func (env *testEnv) service() *Service {
	return &Service{
		Store: env.Store, Pricing: env.Pricing, Cache: env.Cache,
		Now: func() time.Time { return env.Now }, StartedAt: env.Now.Add(-time.Hour),
	}
}

func row(at float64, model, realm, account string, prompt, completion int64, extra map[string]any) map[string]any {
	row := map[string]any{
		"at": at, "iso": time.Unix(int64(at), 0).Local().Format("2006-01-02T15:04:05"),
		"model": model, "prompt_tokens": prompt, "completion_tokens": completion,
		"total_tokens": prompt + completion, "realm": realm, "key": "",
	}
	if account != "" {
		row["account"] = account
	}
	for key, value := range extra {
		row[key] = value
	}
	return row
}

func mustMap(t *testing.T, value any) map[string]any {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		return typed
	}
	return decodeJSONMap(t, value)
}

func mustNumber(t *testing.T, value any) float64 {
	t.Helper()
	number, ok := numericValue(value)
	if !ok {
		t.Fatalf("not a number: %#v", value)
	}
	return number
}

func decodeJSONMap(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
