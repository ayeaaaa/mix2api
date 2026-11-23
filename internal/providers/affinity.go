package providers

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// AffinityOptions are shared by the panel and all prefix-aware providers.
// Model cohorts are opt-in preferences, never account authorization rules.
type AffinityOptions struct {
	PrefixEnabled  bool
	StableKey      bool
	MaxMessages    int
	PageSize       int
	ModelPoolSizes map[string]int
}

func LoadAffinityOptions(settings map[string]any) AffinityOptions {
	o := AffinityOptions{PrefixEnabled: true, MaxMessages: 400, PageSize: 3, ModelPoolSizes: map[string]int{}}
	if raw := strings.TrimSpace(os.Getenv("WB_AFFINITY_BY_PREFIX")); raw != "" {
		switch strings.ToLower(raw) {
		case "0", "false", "no", "off":
			o.PrefixEnabled = false
		}
	}
	for _, entry := range []struct {
		key, env string
		target   *int
		max      int
	}{
		{"affinity_max_messages", "WB_AFFINITY_MAX_MSGS", &o.MaxMessages, 100000},
		{"affinity_page_size", "WB_AFFINITY_PAGE_SIZE", &o.PageSize, 64},
	} {
		if n, err := strconv.Atoi(os.Getenv(entry.env)); err == nil {
			*entry.target = affinityBound(n, entry.max)
		}
		if n, ok := affinityInteger(settings[entry.key]); ok {
			*entry.target = affinityBound(n, entry.max)
		}
	}
	if b, ok := settings["affinity_by_prefix"].(bool); ok {
		o.PrefixEnabled = b
	}
	o.StableKey, _ = settings["affinity_stable_key_enabled"].(bool)
	if sizes, ok := settings["affinity_model_pool_sizes"].(map[string]any); ok {
		for key, raw := range sizes {
			if n, ok := affinityInteger(raw); ok {
				o.ModelPoolSizes[strings.ToLower(strings.TrimSpace(key))] = affinityBound(n, 64)
			}
		}
	}
	return o
}

func AffinitySettingsView(settings map[string]any) map[string]any {
	o := LoadAffinityOptions(settings)
	sizes := map[string]any{}
	for key, n := range o.ModelPoolSizes {
		sizes[key] = n
	}
	return map[string]any{"affinity_by_prefix": o.PrefixEnabled, "affinity_stable_key_enabled": o.StableKey, "affinity_max_messages": o.MaxMessages, "affinity_page_size": o.PageSize, "affinity_model_pool_sizes": sizes, "remaining_priority_supported": false}
}

// ValidateAffinityPatch validates only keys present so saves remain merge-safe.
func ValidateAffinityPatch(patch map[string]any) (map[string]any, error) {
	clean := map[string]any{}
	for _, key := range []string{"affinity_by_prefix", "affinity_stable_key_enabled"} {
		if raw, exists := patch[key]; exists {
			b, ok := raw.(bool)
			if !ok {
				return nil, fmt.Errorf("%s must be true or false", key)
			}
			clean[key] = b
		}
	}
	for key, max := range map[string]int{"affinity_max_messages": 100000, "affinity_page_size": 64} {
		if raw, exists := patch[key]; exists {
			n, ok := affinityInteger(raw)
			if !ok || n < 0 || n > max {
				return nil, fmt.Errorf("%s must be an integer 0-%d", key, max)
			}
			clean[key] = n
		}
	}
	if raw, exists := patch["affinity_model_pool_sizes"]; exists {
		sizes, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("affinity_model_pool_sizes must be an object")
		}
		if len(sizes) > 256 {
			return nil, fmt.Errorf("affinity_model_pool_sizes supports at most 256 entries")
		}
		out := map[string]any{}
		for key, value := range sizes {
			key = strings.ToLower(strings.TrimSpace(key))
			realm, model, _ := strings.Cut(key, "/")
			if (realm != "intl" && realm != "cn" && realm != "codearts" && realm != "raccoon") || strings.TrimSpace(model) == "" || len(key) > 256 {
				return nil, fmt.Errorf("affinity model key must be realm/model")
			}
			n, ok := affinityInteger(value)
			if !ok || n < 0 || n > 64 {
				return nil, fmt.Errorf("affinity model pool size must be an integer 0-64")
			}
			out[key] = n
		}
		clean["affinity_model_pool_sizes"] = out
	}
	return clean, nil
}

func affinityInteger(raw any) (int, bool) {
	switch n := raw.(type) {
	case int:
		return n, true
	case int64:
		if n >= 0 && n <= 100000 {
			return int(n), true
		}
	case float64:
		if n >= 0 && n <= 100000 && n == float64(int(n)) {
			return int(n), true
		}
	case json.Number:
		i, err := n.Int64()
		if err == nil && i >= 0 && i <= 100000 {
			return int(i), true
		}
	}
	return 0, false
}

func affinityBound(n, max int) int {
	if n < 0 {
		return 0
	}
	if n > max {
		return max
	}
	return n
}

func (o AffinityOptions) ModelPoolSize(realm, model string) int {
	return o.ModelPoolSizes[strings.ToLower(strings.TrimSpace(realm)+"/"+strings.TrimSpace(model))]
}

// AffinityRoute keeps the effective upstream key separate from pool rotation.
// PrefixHash is privacy-safe; Key may be a client identifier and MUST NOT be logged.
type AffinityRoute struct {
	Key          string
	PrefixHash   string
	MessageCount int
	Mode         string
	Rotate       bool
}

func ResolveAffinity(req ChatRequest, o AffinityOptions) AffinityRoute {
	msgs := ChatRequestMessages(req)
	r := AffinityRoute{MessageCount: len(msgs), Mode: "rotation"}
	if key := strings.TrimSpace(req.SessionKey); key != "" {
		r.Key = NormalizeSessionKey(key)
		r.Mode = "explicit"
		return r
	}
	if !o.PrefixEnabled {
		return r
	}
	r.Key = DerivePrefixSessionKey(msgs, o.StableKey)
	if r.Key == "" {
		return r
	}
	r.PrefixHash = r.Key
	r.Mode = "prefix"
	if o.MaxMessages > 0 && len(msgs) > o.MaxMessages && o.PageSize != 1 {
		r.Rotate = true
		r.Mode = "page"
		if o.PageSize == 0 {
			r.Mode = "rotation"
		}
	}
	return r
}

// AffinityCohort uses rendezvous ranking: stable under load-order changes and
// temporary account state, and selecting exactly size members (not size groups).
func AffinityCohort(uids []string, seed string, size int) []string {
	type ranked struct {
		uid   string
		score [32]byte
	}
	items := make([]ranked, 0, len(uids))
	seen := map[string]bool{}
	for _, uid := range uids {
		if uid != "" && !seen[uid] {
			seen[uid] = true
			items = append(items, ranked{uid, sha256.Sum256([]byte(seed + "\x00" + uid))})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		c := bytes.Compare(items[i].score[:], items[j].score[:])
		if c == 0 {
			return items[i].uid < items[j].uid
		}
		return c > 0
	})
	if size > 0 && len(items) > size {
		items = items[:size]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.uid)
	}
	return out
}

type AffinityPolicy struct {
	Cohort []string
	Scope  string
	Rotate bool
}

type AffinityDiagnostics struct {
	Mode         string
	PrefixSHA    string
	MessageCount int
	PoolSize     int
	Fallback     bool
}

func (r AffinityRoute) Diagnostics(p AffinityPolicy, uid string) AffinityDiagnostics {
	d := AffinityDiagnostics{Mode: r.Mode, PrefixSHA: strings.TrimPrefix(r.PrefixHash, "pfx-"), MessageCount: r.MessageCount, PoolSize: len(p.Cohort)}
	if len(p.Cohort) > 0 {
		d.Fallback = true
		for _, member := range p.Cohort {
			if member == uid {
				d.Fallback = false
				break
			}
		}
	}
	return d
}

func (r AffinityRoute) Policy(authorized []string, realm, model string, o AffinityOptions) AffinityPolicy {
	p := AffinityPolicy{Rotate: r.Rotate, Scope: realm + "/" + model}
	if r.Mode == "explicit" {
		return p
	}
	uids := authorized
	if size := o.ModelPoolSize(realm, model); size > 0 {
		p.Cohort = AffinityCohort(uids, "model/"+p.Scope, size)
		uids = p.Cohort
	}
	if r.Mode == "page" {
		p.Scope += "/" + r.PrefixHash
		p.Cohort = AffinityCohort(uids, p.Scope, o.PageSize)
	}
	return p
}

// AffinityRotation is a bounded, per-pool set of cohort cursors. Readiness
// changes do not redefine membership or reset another model's turn.
type AffinityRotation struct {
	mu      sync.Mutex
	cursors map[string]int
}

func (r *AffinityRotation) Candidates(eligible []string, p AffinityPolicy) []string {
	if len(p.Cohort) == 0 {
		return eligible
	}
	allowed := map[string]bool{}
	for _, uid := range eligible {
		allowed[uid] = true
	}
	if !p.Rotate {
		out := []string{}
		for _, uid := range p.Cohort {
			if allowed[uid] {
				out = append(out, uid)
			}
		}
		if len(out) > 0 {
			return out
		}
		return eligible
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cursors == nil {
		r.cursors = map[string]int{}
	}
	key := p.Scope + "\x00" + strings.Join(p.Cohort, "\x00")
	start := r.cursors[key] % len(p.Cohort)
	for i := 0; i < len(p.Cohort); i++ {
		idx := (start + i) % len(p.Cohort)
		uid := p.Cohort[idx]
		if allowed[uid] {
			if _, ok := r.cursors[key]; !ok && len(r.cursors) >= 4096 {
				r.cursors = map[string]int{}
			}
			r.cursors[key] = (idx + 1) % len(p.Cohort)
			return []string{uid}
		}
	}
	return eligible
}
