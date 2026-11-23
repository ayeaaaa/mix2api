package providers

import (
	"fmt"
	"reflect"
	"testing"
)

func TestAffinityLongStrategyDefaultsAndProviderOverrides(t *testing.T) {
	def := LoadAffinityOptions(nil)
	if def.LongStrategy != AffinityStrategyCohort || def.PageSize != 3 {
		t.Fatalf("default strategy: %+v", def)
	}
	if len(def.ProviderStrategies) != 0 || len(def.ProviderPageSizes) != 0 {
		t.Fatalf("default provider overrides: %+v", def)
	}

	o := LoadAffinityOptions(map[string]any{
		"affinity_long_strategy": "partition",
		"affinity_page_size":     4,
		"affinity_provider_strategies": map[string]any{
			"workbuddy": "cohort",
			"codearts":  "partition",
			"raccoon":   "",
		},
		"affinity_provider_page_sizes": map[string]any{
			"workbuddy": 5,
			"codearts":  -1,
			"raccoon":   7,
		},
	})
	if got := o.ForProvider("workbuddy"); got.LongStrategy != AffinityStrategyCohort || got.PageSize != 5 {
		t.Fatalf("workbuddy override: %+v", got)
	}
	if got := o.ForProvider("codearts"); got.LongStrategy != AffinityStrategyPartition || got.PageSize != 4 {
		t.Fatalf("codearts inherits page size: %+v", got)
	}
	if got := o.ForProvider("raccoon"); got.LongStrategy != AffinityStrategyPartition || got.PageSize != 7 {
		t.Fatalf("raccoon empty strategy inherits global: %+v", got)
	}
	if got := o.ForProvider("unknown"); got.LongStrategy != AffinityStrategyPartition || got.PageSize != 4 {
		t.Fatalf("unknown provider must inherit global: %+v", got)
	}

	invalid := LoadAffinityOptions(map[string]any{
		"affinity_long_strategy": "weird",
		"affinity_provider_strategies": map[string]any{
			"workbuddy": "weird",
			"bad":       "partition",
		},
		"affinity_provider_page_sizes": map[string]any{
			"workbuddy": 65,
			"codearts":  -2,
			"bad":       3,
		},
	})
	if invalid.LongStrategy != AffinityStrategyCohort {
		t.Fatalf("invalid global strategy not ignored: %+v", invalid)
	}
	if _, ok := invalid.ProviderStrategies["workbuddy"]; ok {
		t.Fatalf("invalid provider strategy kept: %+v", invalid)
	}
	if _, ok := invalid.ProviderStrategies["bad"]; ok {
		t.Fatalf("unknown provider strategy kept: %+v", invalid)
	}
	if _, ok := invalid.ProviderPageSizes["workbuddy"]; ok {
		t.Fatalf("invalid provider page size kept: %+v", invalid)
	}
	if _, ok := invalid.ProviderPageSizes["codearts"]; ok {
		t.Fatalf("negative provider page size other than -1 kept: %+v", invalid)
	}
	if _, ok := invalid.ProviderPageSizes["bad"]; ok {
		t.Fatalf("unknown provider page size kept: %+v", invalid)
	}
}

func TestAffinityStrategyPolicyCohortDefault(t *testing.T) {
	uids := []string{}
	for i := 0; i < 40; i++ {
		uids = append(uids, fmt.Sprintf("u%02d", i))
	}
	o := LoadAffinityOptions(map[string]any{"affinity_page_size": 3})
	r := AffinityRoute{Key: "pfx-0000000100000000", PrefixHash: "pfx-0000000100000000", Mode: "page", Rotate: true}
	p := r.Policy(uids, "intl", "deepseek", o, len(uids))
	if !p.Page || p.Partition {
		t.Fatalf("default long strategy is not cohort: %+v", p)
	}
	if len(p.Cohort) != 3 {
		t.Fatalf("cohort selected %d accounts, want 3", len(p.Cohort))
	}
	if p.Sticky || !p.Rotate {
		t.Fatalf("cohort page should rotate: %+v", p)
	}
	if p.Scope == "" || p.Scope == NormalizeSessionKey("intl/deepseek") {
		t.Fatalf("cohort page scope did not include the conversation: %q", p.Scope)
	}

	// The V1 small-realm guard is partition-only. A three-account realm with
	// the default partition count still rotates in cohort mode.
	small := r.Policy([]string{"a", "b", "c"}, "intl", "deepseek", o, 3)
	if !small.Rotate || small.Sticky || !small.Page || len(small.Cohort) != 3 {
		t.Fatalf("cohort small realm was incorrectly pinned: %+v", small)
	}
}

func TestAffinityStrategyPolicyPartition(t *testing.T) {
	uids := []string{}
	for i := 0; i < 40; i++ {
		uids = append(uids, fmt.Sprintf("u%02d", i))
	}
	o := LoadAffinityOptions(map[string]any{"affinity_long_strategy": "partition", "affinity_page_size": 3})
	r := AffinityRoute{Key: "pfx-0000000100000000", PrefixHash: "pfx-0000000100000000", Mode: "page", Rotate: true}
	p := r.Policy(uids, "intl", "deepseek", o, len(uids))
	if !p.Page || !p.Partition {
		t.Fatalf("partition strategy not marked: %+v", p)
	}
	want := []string{"u01", "u04", "u07", "u10", "u13", "u16", "u19", "u22", "u25", "u28", "u31", "u34", "u37"}
	if !reflect.DeepEqual(p.Cohort, want) {
		t.Fatalf("V1 partition 1/3: %v want %v", p.Cohort, want)
	}
	if p.Scope != NormalizeSessionKey("page/intl/1/3") {
		t.Fatalf("partition scope: %q", p.Scope)
	}

	small := r.Policy([]string{"a", "b", "c"}, "intl", "deepseek", o, 3)
	if small.Rotate || !small.Sticky || small.Page {
		t.Fatalf("partition small realm should be sticky: %+v", small)
	}
}

func TestAffinityStrategyZeroPageKey(t *testing.T) {
	msgs := sessionRaw(t,
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "ok"},
	)
	cohort := LoadAffinityOptions(map[string]any{"affinity_max_messages": 1, "affinity_page_size": 0})
	if r := ResolveAffinity(ChatRequest{Messages: msgs}, cohort); r.Key == "" || r.Mode != "rotation" || !r.Rotate {
		t.Fatalf("cohort zero page should retain the derived key: %+v", r)
	}
	partition := LoadAffinityOptions(map[string]any{"affinity_max_messages": 1, "affinity_page_size": 0, "affinity_long_strategy": "partition"})
	if r := ResolveAffinity(ChatRequest{Messages: msgs}, partition); r.Key != "" || r.Mode != "rotation" || !r.Rotate {
		t.Fatalf("partition zero page should release the derived key: %+v", r)
	}
}

func TestAffinityStrategyPerProviderChangesPolicy(t *testing.T) {
	uids := []string{}
	for i := 0; i < 40; i++ {
		uids = append(uids, fmt.Sprintf("u%02d", i))
	}
	o := LoadAffinityOptions(map[string]any{
		"affinity_long_strategy": "cohort",
		"affinity_page_size":     3,
		"affinity_provider_strategies": map[string]any{
			"workbuddy": "partition",
		},
		"affinity_provider_page_sizes": map[string]any{
			"workbuddy": 5,
		},
	})
	r := AffinityRoute{Key: "pfx-0000000100000000", PrefixHash: "pfx-0000000100000000", Mode: "page", Rotate: true}
	workbuddy := r.Policy(uids, "intl", "deepseek", o.ForProvider("workbuddy"), len(uids))
	if !workbuddy.Page || !workbuddy.Partition || len(workbuddy.Cohort) != 8 {
		t.Fatalf("workbuddy provider override not applied: %+v", workbuddy)
	}
	codearts := r.Policy(uids, "codearts", "deepseek", o.ForProvider("codearts"), len(uids))
	if !codearts.Page || codearts.Partition || len(codearts.Cohort) != 3 {
		t.Fatalf("codearts should inherit global cohort: %+v", codearts)
	}
}

func TestAffinityPolicyUsesV1PriorityOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    AffinityPolicy
		want bool
	}{
		{"partition page", AffinityPolicy{Partition: true, Page: true, Rotate: true}, true},
		{"partition zero page with model cohort", AffinityPolicy{Partition: true, Page: false, Rotate: true}, true},
		{"partition model cohort", AffinityPolicy{Partition: true, Page: false, Rotate: false}, false},
		{"partition sticky", AffinityPolicy{Partition: true, Page: false, Rotate: false, Sticky: true}, false},
		{"cohort page", AffinityPolicy{Page: true, Rotate: true}, false},
		{"cohort zero page", AffinityPolicy{Rotate: true}, false},
	} {
		if got := tc.p.UsesV1PriorityOrder(); got != tc.want {
			t.Fatalf("%s: UsesV1PriorityOrder() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
