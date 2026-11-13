package workbuddy

import (
	"strings"
	"sync"
	"time"
)

// WorkBuddy model tiers: the upstream /v3/config payload carries a
// modelTiers[] array naming the user type each model needs. v1 kept the map
// per realm and used it for two things: the scheduler refused free accounts
// for standard-tier models, and /v1/models labelled the same gate. This
// implementation keeps the identical v1 rules:
//
//   - only entries with requiredUserType or tier participate;
//   - enabled:false entries are skipped;
//   - a "订阅优先" badge without accessMode:exclusive is a scheduling
//     preference, not a gate (measured free accounts can call those models);
//   - unknown non-empty user types are treated as paid/standard;
//   - a payload without modelTiers leaves the previous map untouched.
const (
	tierFree     = "free"
	tierStandard = "standard"
)

var freeUserTypes = map[string]bool{
	"free":               true,
	"personal_free_user": true,
	"personal-free-user": true,
}

var tierState = struct {
	sync.RWMutex
	byRealm    map[string]map[string]string
	ingestedAt map[string]time.Time
}{
	byRealm:    make(map[string]map[string]string),
	ingestedAt: make(map[string]time.Time),
}

// resetTierState drops every realm's tier map. It exists for same-package
// tests, matching v1 wb_tiers.reset().
func resetTierState() {
	tierState.Lock()
	tierState.byRealm = make(map[string]map[string]string)
	tierState.ingestedAt = make(map[string]time.Time)
	tierState.Unlock()
}

// ingestModelTiers records the tier map carried by one /v3/config payload.
// A payload without modelTiers returns false and preserves the previous map;
// an explicit empty array clears the realm's map.
func ingestModelTiers(realm string, payload map[string]any) bool {
	if payload == nil {
		return false
	}
	root := payload
	if data, ok := payload["data"].(map[string]any); ok {
		root = data
	}
	rawEntries, ok := anyList(root["modelTiers"])
	if !ok {
		return false
	}

	out := make(map[string]string)
	for _, raw := range rawEntries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if enabled, ok := entry["enabled"].(bool); ok && !enabled {
			continue
		}
		if priorityOnlyTier(entry) {
			continue
		}
		tier, ok := tierOf(entry["requiredUserType"])
		if !ok {
			tier, ok = tierOf(entry["tier"])
		}
		if !ok {
			continue
		}
		rawIDs, ok := anyList(entry["modelIds"])
		if !ok {
			continue
		}
		for _, rawID := range rawIDs {
			model, ok := rawID.(string)
			if !ok {
				continue
			}
			model = strings.TrimSpace(model)
			if model != "" {
				out[model] = tier
			}
		}
	}

	realm = strings.TrimSpace(realm)
	tierState.Lock()
	tierState.byRealm[realm] = out
	tierState.ingestedAt[realm] = time.Now()
	tierState.Unlock()
	return true
}

// ingest is the v1-shaped alias for ingestModelTiers.
func ingest(realm string, payload map[string]any) bool {
	return ingestModelTiers(realm, payload)
}

// snapshot is the v1-shaped alias for tierSnapshot.
func snapshot(realm string) map[string]string {
	return tierSnapshot(realm)
}

// priorityOnlyTier mirrors v1: an exclusive access mode is a real gate, while
// the subscription-priority badge alone is only a scheduling preference.
func priorityOnlyTier(entry map[string]any) bool {
	if strings.EqualFold(strings.TrimSpace(metaText(entry["accessMode"])), "exclusive") {
		return false
	}
	labels := make([]string, 0, 2)
	switch badge := entry["badge"].(type) {
	case map[string]any:
		labels = append(labels, metaText(badge["label"]), metaText(badge["labelEn"]))
	case string:
		labels = append(labels, badge)
	}
	for _, label := range labels {
		text := strings.TrimSpace(label)
		if text == "" {
			continue
		}
		if strings.Contains(text, "订阅优先") {
			return true
		}
		switch strings.ToLower(text) {
		case "priority", "subscription priority":
			return true
		}
	}
	return false
}

func tierOf(raw any) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(metaText(raw)))
	if value == "" {
		return "", false
	}
	if freeUserTypes[value] {
		return tierFree, true
	}
	return tierStandard, true
}

// tierSnapshot returns a copy of one realm's map, or the merged map when
// realm is empty.
func tierSnapshot(realm string) map[string]string {
	realm = strings.TrimSpace(realm)
	tierState.RLock()
	defer tierState.RUnlock()
	if realm != "" {
		value := tierState.byRealm[realm]
		out := make(map[string]string, len(value))
		for model, tier := range value {
			out[model] = tier
		}
		return out
	}
	out := make(map[string]string)
	for _, mapping := range tierState.byRealm {
		for model, tier := range mapping {
			out[model] = tier
		}
	}
	return out
}

// ingestedAt returns when the realm's tier map was last refreshed. With an
// empty realm it returns the latest stamp across realms. The bool is false
// when no map has ever been ingested.
func ingestedAt(realm string) (time.Time, bool) {
	realm = strings.TrimSpace(realm)
	tierState.RLock()
	defer tierState.RUnlock()
	if realm != "" {
		stamp, ok := tierState.ingestedAt[realm]
		return stamp, ok
	}
	var newest time.Time
	found := false
	for _, stamp := range tierState.ingestedAt {
		if !found || stamp.After(newest) {
			newest = stamp
			found = true
		}
	}
	return newest, found
}

// requiresPro reports whether a model is gated to paid accounts. The optional
// realm keeps intl and cn independent; when the named realm has never
// ingested a map, v1's fallback across known realms is preserved. Unknown
// models return false so a fresh install keeps advertising its catalogue.
func requiresPro(modelID string, realm ...string) bool {
	model := strings.TrimSpace(modelID)
	if model == "" {
		return false
	}
	named := ""
	if len(realm) > 0 {
		named = strings.TrimSpace(realm[0])
	}

	tierState.RLock()
	defer tierState.RUnlock()
	if named != "" {
		if mapping, ok := tierState.byRealm[named]; ok {
			return mapping[model] == tierStandard
		}
	}
	for _, mapping := range tierState.byRealm {
		if mapping[model] == tierStandard {
			return true
		}
	}
	return false
}

// creditsIsFree is v1's exact free-multiplier test. Missing or unparsable
// values are not free, so an unknown model stays under the credit guard.
func creditsIsFree(value any) bool {
	switch strings.ToLower(strings.TrimSpace(metaText(value))) {
	case "x0.00", "x0", "0", "0.00":
		return true
	default:
		return false
	}
}

// isFreeModel is the static-catalog answer for callers that do not have a
// provider/catalog snapshot. The live answer should use Provider.FreeModelIDs.
func isFreeModel(modelID string, realm ...string) bool {
	r := realmIntl
	if len(realm) > 0 {
		r = realmOrDefault(realm[0])
	}
	meta, ok := staticMetaForRealm(r, modelID)
	return ok && creditsIsFree(meta["credits"])
}

func anyList(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case []map[string]any:
		out := make([]any, len(typed))
		for i, entry := range typed {
			out[i] = entry
		}
		return out, true
	case []string:
		out := make([]any, len(typed))
		for i, entry := range typed {
			out[i] = entry
		}
		return out, true
	default:
		return nil, false
	}
}
