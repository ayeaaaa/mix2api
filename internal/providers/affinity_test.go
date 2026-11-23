package providers

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestAffinityOptionsAndRoute(t *testing.T) {
	t.Setenv("WB_AFFINITY_BY_PREFIX", "0")
	t.Setenv("WB_AFFINITY_MAX_MSGS", "10")
	t.Setenv("WB_AFFINITY_PAGE_SIZE", "4")
	opts := LoadAffinityOptions(nil)
	if opts.PrefixEnabled || opts.MaxMessages != 10 || opts.PageSize != 4 {
		t.Fatalf("env defaults: %+v", opts)
	}
	opts = LoadAffinityOptions(map[string]any{"affinity_by_prefix": true, "affinity_max_messages": 400, "affinity_page_size": 3})
	base := sessionRaw(t, map[string]any{"role": "system", "content": "sys"}, map[string]any{"role": "user", "content": "hi"})
	long := append([]json.RawMessage(nil), base...)
	for len(long) < 401 {
		long = append(long, sessionRaw(t, map[string]any{"role": "assistant", "content": "later"})...)
	}
	if r := ResolveAffinity(ChatRequest{Messages: long, SessionKey: "explicit"}, opts); r.Mode != "explicit" || r.Key != "explicit" || r.Rotate {
		t.Fatalf("explicit overwritten: %+v", r)
	}
	if r := ResolveAffinity(ChatRequest{Messages: base}, opts); r.Mode != "prefix" || r.Rotate {
		t.Fatalf("short route: %+v", r)
	}
	if r := ResolveAffinity(ChatRequest{Messages: long}, opts); r.Mode != "page" || !r.Rotate {
		t.Fatalf("long route: %+v", r)
	}
	opts.PageSize = 0
	if r := ResolveAffinity(ChatRequest{Messages: long}, opts); !r.Rotate || r.Mode != "rotation" {
		t.Fatalf("zero page: %+v", r)
	}
	opts.MaxMessages = 0
	if r := ResolveAffinity(ChatRequest{Messages: long}, opts); r.Rotate {
		t.Fatalf("zero threshold: %+v", r)
	}
}

func TestAffinityCursorChurnKeepsRecentlyUsedPage(t *testing.T) {
	var rot AffinityRotation
	p := AffinityPolicy{Scope: "active", Cohort: []string{"a", "b", "c"}, Rotate: true}
	rot.Candidates(p.Cohort, p)
	for i := 0; i < 4095; i++ {
		rot.Candidates(p.Cohort, AffinityPolicy{Scope: fmt.Sprint(i), Cohort: p.Cohort, Rotate: true})
	}
	if got := rot.Candidates(p.Cohort, p); got[0] != "b" {
		t.Fatal(got)
	}
	rot.Candidates(p.Cohort, AffinityPolicy{Scope: "new", Cohort: p.Cohort, Rotate: true})
	if got := rot.Candidates(p.Cohort, p); got[0] != "c" {
		t.Fatalf("just-used page reset: %v", got)
	}
	if len(rot.cursors) > 4096 {
		t.Fatal("cursor map exceeded cap")
	}
}

func TestAffinityPolicyCaseAndScopeBound(t *testing.T) {
	uids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	o := LoadAffinityOptions(map[string]any{"affinity_model_pool_sizes": map[string]any{"codearts/glm-5.2": 3}})
	r := AffinityRoute{Mode: "prefix", Key: "pfx-key"}
	a, b := r.Policy(uids, "codearts", "GLM-5.2", o), r.Policy(uids, "codearts", "glm-5.2", o)
	if !reflect.DeepEqual(a.Cohort, b.Cohort) {
		t.Fatalf("equivalent model cohorts differ: %v %v", a, b)
	}
	if got := r.Policy(uids, "intl", strings.Repeat("m", 100000), o); len(got.Scope) > MaxSessionKeyBytes {
		t.Fatalf("unbounded routing key: %d", len(got.Scope))
	}
}

func TestAffinityCohortIsExactlyThreeAndStable(t *testing.T) {
	uids := []string{}
	for i := 0; i < 40; i++ {
		uids = append(uids, fmt.Sprintf("account-%02d", i))
	}
	a := AffinityCohort(uids, "conversation/model", 3)
	if len(a) != 3 {
		t.Fatalf("size 3 selected %d of 40 accounts", len(a))
	}
	for i, j := 0, len(uids)-1; i < j; i, j = i+1, j-1 {
		uids[i], uids[j] = uids[j], uids[i]
	}
	if b := AffinityCohort(uids, "conversation/model", 3); !reflect.DeepEqual(a, b) {
		t.Fatalf("load order changed membership: %v vs %v", a, b)
	}
	if got := AffinityCohort(uids, "conversation/model", 0); len(got) != 40 {
		t.Fatalf("0 should not limit: %v", got)
	}
	if got := AffinityCohort(uids[:2], "conversation/model", 3); len(got) != 2 {
		t.Fatalf("small pool: %v", got)
	}
}

func TestAffinityRotationPreservesCursorAndFallsBack(t *testing.T) {
	var rot AffinityRotation
	policy := AffinityPolicy{Cohort: []string{"a", "b", "c"}, Scope: "model/page", Rotate: true}
	for i, want := range []string{"a", "b", "c", "a", "b", "c"} {
		got := rot.Candidates([]string{"a", "b", "c", "d"}, policy)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("turn %d: %v want %s", i, got, want)
		}
		rot.Candidates([]string{"x", "y"}, AffinityPolicy{Cohort: []string{"x", "y"}, Scope: "other/page", Rotate: true})
	}
	if got := rot.Candidates([]string{"d"}, policy); !reflect.DeepEqual(got, []string{"d"}) {
		t.Fatalf("no eligible cohort fallback: %v", got)
	}
}

func TestAffinityPreferenceModelCohortAndExplicitKey(t *testing.T) {
	o := LoadAffinityOptions(map[string]any{"affinity_model_pool_sizes": map[string]any{"intl/deepseek": 3}})
	uids := []string{"a", "b", "c", "d", "e", "f"}
	r := AffinityRoute{Key: "first", Mode: "prefix"}
	p := r.Policy(uids, "intl", "deepseek", o)
	if len(p.Cohort) != 3 {
		t.Fatalf("model cohort %v", p)
	}
	r.Key = "second"
	if !reflect.DeepEqual(p.Cohort, r.Policy(uids, "intl", "deepseek", o).Cohort) {
		t.Fatal("new conversation reshuffled model cohort")
	}
	r.Mode = "explicit"
	if p := r.Policy(uids, "intl", "deepseek", o); len(p.Cohort) != 0 {
		t.Fatalf("explicit sessions limited: %v", p)
	}
}
