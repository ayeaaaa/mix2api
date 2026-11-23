package providers

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	AffinityStrategyCohort    = "cohort"
	AffinityStrategyPartition = "partition"
)

// AffinityProviders is the fixed set of provider families that accept
// per-provider strategy/page-size overrides.
var AffinityProviders = []string{"workbuddy", "codearts", "raccoon"}

// AffinityOptions are shared by the panel and all prefix-aware providers.
// Model cohorts are opt-in preferences, never account authorization rules.
type AffinityOptions struct {
	PrefixEnabled      bool
	StableKey          bool
	MaxMessages        int
	PageSize           int
	LongStrategy       string
	ModelPoolSizes     map[string]int
	ProviderStrategies map[string]string
	ProviderPageSizes  map[string]int
}

func LoadAffinityOptions(settings map[string]any) AffinityOptions {
	o := AffinityOptions{
		PrefixEnabled:      true,
		MaxMessages:        400,
		PageSize:           3,
		LongStrategy:       AffinityStrategyCohort,
		ModelPoolSizes:     map[string]int{},
		ProviderStrategies: map[string]string{},
		ProviderPageSizes:  map[string]int{},
	}
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
	if strategy, ok := settings["affinity_long_strategy"].(string); ok {
		strategy = strings.ToLower(strings.TrimSpace(strategy))
		if strategy == AffinityStrategyCohort || strategy == AffinityStrategyPartition {
			o.LongStrategy = strategy
		}
	}
	if strategies, ok := settings["affinity_provider_strategies"].(map[string]any); ok {
		for provider, raw := range strategies {
			provider = normalizeAffinityProvider(provider)
			if !affinityProviderKnown(provider) {
				continue
			}
			strategy, _ := raw.(string)
			strategy = strings.ToLower(strings.TrimSpace(strategy))
			if strategy == "" || strategy == AffinityStrategyCohort || strategy == AffinityStrategyPartition {
				o.ProviderStrategies[provider] = strategy
			}
		}
	}
	if sizes, ok := settings["affinity_provider_page_sizes"].(map[string]any); ok {
		for provider, raw := range sizes {
			provider = normalizeAffinityProvider(provider)
			if !affinityProviderKnown(provider) {
				continue
			}
			if n, ok := affinityProviderPageSize(raw); ok {
				o.ProviderPageSizes[provider] = n
			}
		}
	}
	if sizes, ok := settings["affinity_model_pool_sizes"].(map[string]any); ok {
		for key, raw := range sizes {
			if n, ok := affinityInteger(raw); ok && n > 0 {
				o.ModelPoolSizes[strings.ToLower(strings.TrimSpace(key))] = affinityBound(n, 64)
			}
		}
	}
	return o
}

func normalizeAffinityProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func affinityProviderKnown(provider string) bool {
	for _, known := range AffinityProviders {
		if provider == known {
			return true
		}
	}
	return false
}

// ForProvider resolves optional per-provider strategy and page-size overrides.
// Missing or empty values inherit the global setting. A page-size override of
// -1 also inherits; 0 remains a meaningful V1/cohort value.
func (o AffinityOptions) ForProvider(provider string) AffinityOptions {
	provider = normalizeAffinityProvider(provider)
	out := o
	if strategy := out.ProviderStrategies[provider]; strategy != "" {
		out.LongStrategy = strategy
	}
	if n, ok := out.ProviderPageSizes[provider]; ok && n >= 0 {
		out.PageSize = n
	}
	return out
}

func AffinitySettingsView(settings map[string]any) map[string]any {
	o := LoadAffinityOptions(settings)
	sizes := map[string]any{}
	for key, n := range o.ModelPoolSizes {
		sizes[key] = n
	}
	strategies := map[string]any{}
	for key, strategy := range o.ProviderStrategies {
		strategies[key] = strategy
	}
	providerPages := map[string]any{}
	for key, n := range o.ProviderPageSizes {
		providerPages[key] = n
	}
	return map[string]any{
		"affinity_by_prefix":           o.PrefixEnabled,
		"affinity_stable_key_enabled":  o.StableKey,
		"affinity_max_messages":        o.MaxMessages,
		"affinity_page_size":           o.PageSize,
		"affinity_long_strategy":       o.LongStrategy,
		"affinity_provider_strategies": strategies,
		"affinity_provider_page_sizes": providerPages,
		"affinity_model_pool_sizes":    sizes,
		"remaining_priority_supported": false,
	}
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
	if raw, exists := patch["affinity_long_strategy"]; exists {
		strategy, ok := raw.(string)
		strategy = normalizeAffinityProvider(strategy)
		if !ok || (strategy != AffinityStrategyCohort && strategy != AffinityStrategyPartition) {
			return nil, fmt.Errorf("affinity_long_strategy must be cohort or partition")
		}
		clean["affinity_long_strategy"] = strategy
	}
	if raw, exists := patch["affinity_provider_strategies"]; exists {
		strategies, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("affinity_provider_strategies must be an object")
		}
		if len(strategies) > 8 {
			return nil, fmt.Errorf("affinity_provider_strategies supports at most 8 entries")
		}
		out := map[string]any{}
		for key, value := range strategies {
			provider := normalizeAffinityProvider(key)
			if !affinityProviderKnown(provider) {
				return nil, fmt.Errorf("unknown affinity provider: %s", key)
			}
			if _, duplicate := out[provider]; duplicate {
				return nil, fmt.Errorf("duplicate affinity provider: %s", provider)
			}
			strategy, ok := value.(string)
			strategy = normalizeAffinityProvider(strategy)
			if !ok || (strategy != "" && strategy != AffinityStrategyCohort && strategy != AffinityStrategyPartition) {
				return nil, fmt.Errorf("affinity provider strategy must be empty, cohort, or partition")
			}
			out[provider] = strategy
		}
		clean["affinity_provider_strategies"] = out
	}
	if raw, exists := patch["affinity_provider_page_sizes"]; exists {
		sizes, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("affinity_provider_page_sizes must be an object")
		}
		if len(sizes) > 8 {
			return nil, fmt.Errorf("affinity_provider_page_sizes supports at most 8 entries")
		}
		out := map[string]any{}
		for key, value := range sizes {
			provider := normalizeAffinityProvider(key)
			if !affinityProviderKnown(provider) {
				return nil, fmt.Errorf("unknown affinity provider: %s", key)
			}
			if _, duplicate := out[provider]; duplicate {
				return nil, fmt.Errorf("duplicate affinity provider: %s", provider)
			}
			n, ok := affinityProviderPageSize(value)
			if !ok {
				return nil, fmt.Errorf("affinity provider page size must be -1 (inherit) or an integer 0-64")
			}
			out[provider] = n
		}
		clean["affinity_provider_page_sizes"] = out
	}
	if raw, exists := patch["affinity_model_pool_sizes"]; exists {
		sizes, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("affinity_model_pool_sizes must be an object")
		}
		if len(sizes) > 1024 {
			return nil, fmt.Errorf("affinity_model_pool_sizes patch supports at most 1024 entries including removals")
		}
		out := map[string]any{}
		for key, value := range sizes {
			key = strings.ToLower(strings.TrimSpace(key))
			if _, duplicate := out[key]; duplicate {
				return nil, fmt.Errorf("duplicate affinity model key: %s", key)
			}
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

// affinityProviderPageSize accepts -1 as the explicit "inherit global" value.
// A real 0 is still meaningful (whole-pool rotation / V1 release).
func affinityProviderPageSize(raw any) (int, bool) {
	valid := func(n int64) bool { return n >= -1 && n <= 64 }
	switch n := raw.(type) {
	case int:
		if valid(int64(n)) {
			return n, true
		}
	case int64:
		if valid(n) {
			return int(n), true
		}
	case float64:
		if n != n || n < -1 || n > 64 {
			break
		}
		if n == float64(int64(n)) {
			return int(n), true
		}
	case json.Number:
		i, err := n.Int64()
		if err == nil && valid(i) {
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
			if o.LongStrategy == AffinityStrategyPartition {
				r.Key = ""
			}
		}
	}
	return r
}

// ResolveAffinityForBody hashes provider-normalized/sanitized messages, not
// volatile client representations. Explicit IDs still come only from req.
func ResolveAffinityForBody(req ChatRequest, body map[string]any, o AffinityOptions) AffinityRoute {
	if msgs, ok := body["messages"]; ok {
		if raw, err := json.Marshal(msgs); err == nil {
			req.Raw = map[string]json.RawMessage{"messages": raw}
		}
	}
	return ResolveAffinity(req, o)
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
	Cohort    []string
	Scope     string
	Rotate    bool
	Sticky    bool
	Page      bool
	Partition bool
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

func (r AffinityRoute) Policy(authorized []string, realm, model string, o AffinityOptions, realmCounts ...int) AffinityPolicy {
	p := AffinityPolicy{
		Rotate:    r.Rotate,
		Scope:     NormalizeSessionKey(strings.ToLower(strings.TrimSpace(realm) + "/" + strings.TrimSpace(model))),
		Page:      r.Mode == "page",
		Partition: o.LongStrategy == AffinityStrategyPartition,
	}
	if r.Mode == "explicit" {
		return p
	}
	uids := authorized
	if size := o.ModelPoolSize(realm, model); size > 0 {
		p.Cohort = AffinityCohort(uids, "model/"+p.Scope, size)
		uids = p.Cohort
	}
	if r.Mode == "page" {
		if o.LongStrategy == AffinityStrategyPartition {
			realmCount := len(authorized)
			if len(realmCounts) > 0 {
				realmCount = realmCounts[0]
			}
			// V1 restores normal affinity when the partition parameter covers
			// the full realm. A small key allow-list in a larger realm is
			// different: _page_slice returns that whole allowed subset, still
			// without pinning.
			if o.PageSize >= realmCount {
				p.Rotate = false
				p.Sticky = true
				p.Page = false
				return p
			}
			index := 0
			digest := strings.TrimPrefix(r.PrefixHash, "pfx-")
			if len(digest) >= 8 && o.PageSize > 1 {
				if seed, err := strconv.ParseUint(digest[:8], 16, 32); err == nil {
					index = int(seed % uint64(o.PageSize))
				}
			}
			p.Cohort = AffinityPartition(uids, index, o.PageSize)
			// V1's page cursor is shared by conversations/models on the same page.
			p.Scope = NormalizeSessionKey(fmt.Sprintf("page/%s/%d/%d", strings.ToLower(realm), index, o.PageSize))
			return p
		}
		// Default cohort strategy keeps the v2.8.16 behavior: the conversation
		// is bounded to accountCount, not V1's partition count.
		p.Scope += "/" + r.PrefixHash
		p.Cohort = AffinityCohort(uids, p.Scope, o.PageSize)
	}
	return p
}

// AffinityPartition matches V1 rank-modulo membership. Rank over the static
// authorized snapshot, before temporary readiness/exclusions are applied.
func AffinityPartition(uids []string, index, count int) []string {
	ordered := []string{}
	seen := map[string]bool{}
	for _, uid := range uids {
		if uid != "" && !seen[uid] {
			ordered = append(ordered, uid)
			seen[uid] = true
		}
	}
	sort.Strings(ordered)
	if count <= 1 || count >= len(ordered) {
		return ordered
	}
	out := []string{}
	for rank, uid := range ordered {
		if rank%count == index%count {
			out = append(out, uid)
		}
	}
	return out
}

func (p AffinityPolicy) HasEligibleMember(eligible []string) bool {
	members := map[string]bool{}
	for _, uid := range p.Cohort {
		members[uid] = true
	}
	for _, uid := range eligible {
		if members[uid] {
			return true
		}
	}
	return false
}

// AffinityRotation is a bounded, per-pool set of cohort cursors. Readiness
// changes do not redefine membership or reset another model's turn.
type AffinityRotation struct {
	mu      sync.Mutex
	cursors map[string]int
	recent  map[string]*list.Element
	order   list.List
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
		r.recent = map[string]*list.Element{}
	}
	keySource := p.Scope
	if !strings.HasPrefix(p.Scope, "page/") {
		keySource += "\x00" + strings.Join(p.Cohort, "\x00")
	}
	key := NormalizeSessionKey(keySource)
	start := r.cursors[key] % len(p.Cohort)
	for i := 0; i < len(p.Cohort); i++ {
		idx := (start + i) % len(p.Cohort)
		uid := p.Cohort[idx]
		if allowed[uid] {
			if _, ok := r.cursors[key]; !ok && len(r.cursors) >= 4096 {
				oldest := r.order.Front()
				oldKey := oldest.Value.(string)
				delete(r.cursors, oldKey)
				delete(r.recent, oldKey)
				r.order.Remove(oldest)
			}
			if entry := r.recent[key]; entry != nil {
				r.order.MoveToBack(entry)
			} else {
				r.recent[key] = r.order.PushBack(key)
			}
			r.cursors[key] = (idx + 1) % len(p.Cohort)
			return []string{uid}
		}
	}
	return eligible
}
