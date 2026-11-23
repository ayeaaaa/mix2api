package settings

import (
	"context"
	"fmt"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"path/filepath"
	"testing"
)

func TestAffinitySettingsSaveAndDefaults(t *testing.T) {
	s := New(Service{Store: config.New(filepath.Join(t.TempDir(), "settings.json"))})
	v, err := s.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v["affinity_by_prefix"] != true || v["affinity_page_size"] != 3 || v["affinity_max_messages"] != 400 || v["remaining_priority_supported"] != false {
		t.Fatalf("defaults/capabilities: %v", v)
	}
	patch := map[string]any{"affinity_by_prefix": false, "affinity_stable_key_enabled": true, "affinity_page_size": 4, "affinity_max_messages": 123, "affinity_model_pool_sizes": map[string]any{"intl/deepseek-v4.1-flash-sg": 3}}
	if _, err := s.Save(context.Background(), patch); err != nil {
		t.Fatal(err)
	}
	v, err = s.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v["affinity_by_prefix"] != false || v["affinity_page_size"] != 4 || v["affinity_max_messages"] != 123 || v["affinity_stable_key_enabled"] != true {
		t.Fatalf("save not used: %v", v)
	}
	if _, err := s.Save(context.Background(), map[string]any{"affinity_model_pool_sizes": map[string]any{"cn/other": 2}}); err != nil {
		t.Fatal(err)
	}
	data, _ := s.Store.Load()
	sizes := data["affinity_model_pool_sizes"].(map[string]any)
	if len(sizes) != 2 {
		t.Fatalf("partial save discarded other realm: %v", sizes)
	}
	for _, bad := range []map[string]any{
		{"affinity_by_prefix": "false"}, {"affinity_page_size": -1}, {"affinity_page_size": 65}, {"affinity_page_size": 1.5},
		{"affinity_model_pool_sizes": []any{3}}, {"affinity_model_pool_sizes": map[string]any{"deepseek": 3}}, {"affinity_model_pool_sizes": map[string]any{"intl/deepseek": true}},
	} {
		if _, err := s.Save(context.Background(), bad); err == nil {
			t.Fatalf("invalid affinity setting accepted: %v", bad)
		}
	}
}

func TestAffinityMergedRulesCapAndDeleteAreAtomic(t *testing.T) {
	s := New(Service{Store: config.New(filepath.Join(t.TempDir(), "settings.json"))})
	rules := map[string]any{}
	for i := 0; i < 256; i++ {
		rules[fmt.Sprintf("intl/m%d", i)] = 3
	}
	if _, err := s.Save(context.Background(), map[string]any{"affinity_model_pool_sizes": rules}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(context.Background(), map[string]any{"affinity_model_pool_sizes": map[string]any{"intl/new": 3}, "pricing_enabled": false}); err == nil {
		t.Fatal("merged cap was not enforced")
	}
	v, _ := s.View(context.Background())
	if v["pricing_enabled"] != true {
		t.Fatal("rejected save partially changed settings")
	}
	if _, err := s.Save(context.Background(), map[string]any{"affinity_model_pool_sizes": map[string]any{"intl/m0": 0, "intl/new": 3}}); err != nil {
		t.Fatal(err)
	}
	v, _ = s.View(context.Background())
	r := v["affinity_model_pool_sizes"].(map[string]any)
	if len(r) != 256 || r["intl/m0"] != nil || r["intl/new"] != 3 {
		t.Fatalf("deleted rule was not pruned: %v", r)
	}
	if _, err := s.Save(context.Background(), map[string]any{"affinity_model_pool_sizes": map[string]any{"intl/X": 3, "INTL/x": 2}}); err == nil {
		t.Fatal("ambiguous normalized keys accepted")
	}
	// Full UI forms may carry 256 active rules plus explicit zero removals.
	full := map[string]any{}
	for i := 1; i < 256; i++ {
		full[fmt.Sprintf("intl/m%d", i)] = 3
	}
	full["intl/new"] = 0
	full["intl/replacement"] = 3
	if _, err := s.Save(context.Background(), map[string]any{"affinity_model_pool_sizes": full}); err != nil {
		t.Fatalf("full form with removal rejected: %v", err)
	}
}
