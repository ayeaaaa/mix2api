package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// KeyBucketBefore marks rows written before the key field existed.
const KeyBucketBefore = "__before_keys__"

// KeyBucketAnon marks rows from a deployment that presented no key.
const KeyBucketAnon = "__no_key__"

// KeyBucketUnknown collapses observed ids the key roster does not know.
const KeyBucketUnknown = "__unknown_key__"

// KeyModelTopN caps the per-key model pills.
const KeyModelTopN = 5

// AnalyticsStat is the v1 summary/account/model/key stat block.
type AnalyticsStat struct {
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Credit           float64 `json:"credit"`
	CostCNY          float64 `json:"cost_cny"`
	TTFTSum          float64 `json:"ttft_sum"`
	TTFTN            int64   `json:"ttft_n"`
	SpeedSum         float64 `json:"speed_sum"`
	SpeedN           int64   `json:"speed_n"`
	ElapsedSum       float64 `json:"elapsed_sum"`
	ElapsedN         int64   `json:"elapsed_n"`
	CacheHitPct      float64 `json:"cache_hit_pct"`
	ReasoningRatio   float64 `json:"reasoning_ratio"`
	TTFTMSAvg        int64   `json:"ttft_ms_avg"`
	SpeedAvg         float64 `json:"speed_avg"`
	ElapsedMSAvg     int64   `json:"elapsed_ms_avg"`
}

// AnalyticsModel is one model's contribution.
type AnalyticsModel struct {
	Requests  int64   `json:"requests"`
	Tokens    int64   `json:"tokens"`
	Reasoning int64   `json:"reasoning"`
	CostCNY   float64 `json:"cost_cny"`
}

// AnalyticsAccount is one upstream account's row.
type AnalyticsAccount struct {
	UID          string                    `json:"uid"`
	Nickname     string                    `json:"nickname"`
	Realm        string                    `json:"realm"`
	Domain       string                    `json:"domain"`
	Window       AnalyticsStat             `json:"window"`
	AllTime      AnalyticsStat             `json:"all_time"`
	WindowModels map[string]AnalyticsModel `json:"window_models"`
	AllModels    map[string]AnalyticsModel `json:"all_models"`
}

// AnalyticsModelRow is one model's row.
type AnalyticsModelRow struct {
	Model   string        `json:"model"`
	Window  AnalyticsStat `json:"window"`
	AllTime AnalyticsStat `json:"all_time"`
}

// AnalyticsKey is one caller API key's row.
type AnalyticsKey struct {
	Key          string                    `json:"key"`
	Name         string                    `json:"name"`
	Realm        string                    `json:"realm"`
	Enabled      bool                      `json:"enabled"`
	Source       string                    `json:"source"`
	Window       AnalyticsStat             `json:"window"`
	AllTime      AnalyticsStat             `json:"all_time"`
	WindowModels map[string]AnalyticsModel `json:"window_models"`
	AllModels    map[string]AnalyticsModel `json:"all_models"`
	Realms       map[string]int64          `json:"realms"`
	LastAt       float64                   `json:"last_at"`
	Models       []map[string]any          `json:"models"`
	ModelsOther  map[string]any            `json:"models_other"`
	CrossRealm   bool                      `json:"cross_realm"`
}

type analyticsMaps struct {
	Summary AnalyticsStat                 `json:"summary"`
	Accts   map[string]*AnalyticsAccount  `json:"accts"`
	Models  map[string]*AnalyticsModelRow `json:"models"`
	Keys    map[string]*AnalyticsKey      `json:"keys"`
}

func newAnalyticsMaps() analyticsMaps {
	return analyticsMaps{
		Accts:  map[string]*AnalyticsAccount{},
		Models: map[string]*AnalyticsModelRow{},
		Keys:   map[string]*AnalyticsKey{},
	}
}

func (m analyticsMaps) empty() bool {
	return len(m.Accts) == 0 && len(m.Models) == 0 && len(m.Keys) == 0 && m.Summary == (AnalyticsStat{})
}

func (m analyticsMaps) clone() analyticsMaps {
	out := newAnalyticsMaps()
	out.Summary = m.Summary
	for uid, entry := range m.Accts {
		copy := *entry
		copy.WindowModels = cloneModelMap(entry.WindowModels)
		copy.AllModels = cloneModelMap(entry.AllModels)
		out.Accts[uid] = &copy
	}
	for id, entry := range m.Models {
		copy := *entry
		out.Models[id] = &copy
	}
	for id, entry := range m.Keys {
		copy := *entry
		copy.WindowModels = cloneModelMap(entry.WindowModels)
		copy.AllModels = cloneModelMap(entry.AllModels)
		copy.Realms = map[string]int64{}
		for realm, count := range entry.Realms {
			copy.Realms[realm] = count
		}
		out.Keys[id] = &copy
	}
	return out
}

func cloneModelMap(source map[string]AnalyticsModel) map[string]AnalyticsModel {
	out := map[string]AnalyticsModel{}
	for id, value := range source {
		out[id] = value
	}
	return out
}

func feedAnalytics(stat *AnalyticsStat, row Row, isErr bool, cost float64) {
	if isErr {
		stat.Errors++
	} else {
		stat.Requests++
	}
	stat.PromptTokens += intNumber(row, "prompt_tokens")
	stat.CompletionTokens += intNumber(row, "completion_tokens")
	stat.ReasoningTokens += intNumber(row, "reasoning_tokens")
	stat.CachedTokens += intNumber(row, "cached_tokens")
	stat.TotalTokens += intNumber(row, "total_tokens")
	stat.Credit += number(row, "credit")
	stat.CostCNY += cost
	if value := number(row, "ttft_ms"); value != 0 {
		stat.TTFTSum += value
		stat.TTFTN++
	}
	if value := number(row, "tokens_per_sec"); value != 0 {
		stat.SpeedSum += value
		stat.SpeedN++
	}
	if value := number(row, "elapsed_ms"); value != 0 {
		stat.ElapsedSum += value
		stat.ElapsedN++
	}
}

func bumpAnalyticsModels(target map[string]AnalyticsModel, model string, row Row, isErr bool, cost float64) {
	if isErr {
		return
	}
	entry := target[model]
	entry.Requests++
	entry.Tokens += intNumber(row, "total_tokens")
	entry.Reasoning += intNumber(row, "reasoning_tokens")
	entry.CostCNY += cost
	target[model] = entry
}

// foldAnalyticsRow folds one row into the all-time or window half. The key
// roster fields (realms/last_at) only move on the all-time fold.
func (s *Service) foldAnalyticsRow(row Row, realm string, maps *analyticsMaps, window bool, cost float64) {
	outcome := RowOutcome(row)
	if outcome == "client_aborted" {
		return
	}
	isErr := outcome != "completed"
	model := rowString(row, "model")
	if model == "" {
		model = "(unknown)"
	}
	account := rowString(row, "account")
	if account == "" {
		account = "(unattributed)"
	}
	keyID := ""
	if _, exists := row["key"]; exists {
		keyID = rowString(row, "key")
		if keyID == "" {
			keyID = KeyBucketAnon
		}
	} else {
		keyID = KeyBucketBefore
	}

	if window {
		feedAnalytics(&maps.Summary, row, isErr, cost)
		entry := maps.Accts[account]
		if entry == nil {
			entry = &AnalyticsAccount{
				UID: account, Nickname: account,
				Realm:        rowString(row, "realm"),
				WindowModels: map[string]AnalyticsModel{},
				AllModels:    map[string]AnalyticsModel{},
			}
			maps.Accts[account] = entry
		}
		feedAnalytics(&entry.Window, row, isErr, cost)
		bumpAnalyticsModels(entry.WindowModels, model, row, isErr, cost)
		modelEntry := maps.Models[model]
		if modelEntry == nil {
			modelEntry = &AnalyticsModelRow{Model: model}
			maps.Models[model] = modelEntry
		}
		feedAnalytics(&modelEntry.Window, row, isErr, cost)
		keyEntry := maps.Keys[keyID]
		if keyEntry == nil {
			keyEntry = &AnalyticsKey{
				Key: keyID, WindowModels: map[string]AnalyticsModel{},
				AllModels: map[string]AnalyticsModel{}, Realms: map[string]int64{},
			}
			maps.Keys[keyID] = keyEntry
		}
		feedAnalytics(&keyEntry.Window, row, isErr, cost)
		bumpAnalyticsModels(keyEntry.WindowModels, model, row, isErr, cost)
		return
	}

	feedAnalytics(&maps.Summary, row, isErr, cost)
	entry := maps.Accts[account]
	if entry == nil {
		entry = &AnalyticsAccount{
			UID: account, Nickname: account,
			Realm:        rowString(row, "realm"),
			WindowModels: map[string]AnalyticsModel{},
			AllModels:    map[string]AnalyticsModel{},
		}
		maps.Accts[account] = entry
	}
	feedAnalytics(&entry.AllTime, row, isErr, cost)
	bumpAnalyticsModels(entry.AllModels, model, row, isErr, cost)
	modelEntry := maps.Models[model]
	if modelEntry == nil {
		modelEntry = &AnalyticsModelRow{Model: model}
		maps.Models[model] = modelEntry
	}
	feedAnalytics(&modelEntry.AllTime, row, isErr, cost)
	keyEntry := maps.Keys[keyID]
	if keyEntry == nil {
		keyEntry = &AnalyticsKey{
			Key: keyID, WindowModels: map[string]AnalyticsModel{},
			AllModels: map[string]AnalyticsModel{}, Realms: map[string]int64{},
		}
		maps.Keys[keyID] = keyEntry
	}
	keyRealm := s.rowRealm(row)
	keyEntry.Realms[keyRealm]++
	at := number(row, "at")
	if at > keyEntry.LastAt {
		keyEntry.LastAt = at
	}
	feedAnalytics(&keyEntry.AllTime, row, isErr, cost)
	bumpAnalyticsModels(keyEntry.AllModels, model, row, isErr, cost)
}

func finalizeAnalyticsStat(stat *AnalyticsStat) {
	if stat.PromptTokens > 0 {
		stat.CacheHitPct = round1(float64(stat.CachedTokens) * 100 / float64(stat.PromptTokens))
	}
	if stat.CompletionTokens > 0 {
		stat.ReasoningRatio = round1(float64(stat.ReasoningTokens) * 100 / float64(stat.CompletionTokens))
	}
	if stat.TTFTN > 0 {
		stat.TTFTMSAvg = int64(round0(stat.TTFTSum / float64(stat.TTFTN)))
	}
	if stat.SpeedN > 0 {
		stat.SpeedAvg = round1(stat.SpeedSum / float64(stat.SpeedN))
	}
	if stat.ElapsedN > 0 {
		stat.ElapsedMSAvg = int64(round0(stat.ElapsedSum / float64(stat.ElapsedN)))
	}
}

// Analytics returns the v1 compute_usage_analytics payload.
func (s *Service) Analytics(ctx context.Context, opts AnalyticsOptions) (map[string]any, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("usage: nil service")
	}
	window := ParseRange(opts.Range, opts.Since, opts.Until)
	realm := scopeRealm(opts.Realm)
	cacheKey := fmt.Sprintf("%s|%s|%s", realm, boundKey(window.Since), boundKey(window.Until))
	if value, _, ok := s.resultGet("analytics", cacheKey); ok {
		return value.(map[string]any), nil
	}

	allMaps, err := s.analyticsAllTime(ctx, realm)
	if err != nil {
		return nil, err
	}
	var winMaps analyticsMaps
	if window.Since == nil && window.Until == nil {
		winMaps = allMaps.clone()
		mirrorAllTimeToWindow(&winMaps)
	} else {
		winMaps = newAnalyticsMaps()
		if err := s.scanAll(ctx, func(row Row) error {
			if !s.matchesRealm(row, realm) || !inRange(number(row, "at"), window) {
				return nil
			}
			cost := s.costFor(row)
			cny := 0.0
			if cost.Known {
				cny = cost.CNY
			}
			s.foldAnalyticsRow(row, realm, &winMaps, true, cny)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	allCopy := allMaps.clone()
	finalizeAnalyticsMaps(&allCopy)
	finalizeAnalyticsMaps(&winMaps)
	payload := s.analyticsPayload(allCopy, winMaps, realm, window)
	s.resultPut("analytics", cacheKey, payload, s.statsTTL())
	return payload, nil
}

func (s *Service) analyticsAllTime(ctx context.Context, realm string) (analyticsMaps, error) {
	if maps, ok := s.adoptAnalytics(ctx, realm); ok {
		return maps, nil
	}
	maps := newAnalyticsMaps()
	if err := s.scanAll(ctx, func(row Row) error {
		if !s.matchesRealm(row, realm) {
			return nil
		}
		cost := s.costFor(row)
		cny := 0.0
		if cost.Known {
			cny = cost.CNY
		}
		s.foldAnalyticsRow(row, realm, &maps, false, cny)
		return nil
	}); err != nil {
		return analyticsMaps{}, err
	}
	s.saveAnalytics(realm, maps, s.logSize())
	return maps, nil
}

func (s *Service) adoptAnalytics(ctx context.Context, realm string) (analyticsMaps, bool) {
	if s.Cache == nil {
		return analyticsMaps{}, false
	}
	for _, entry := range s.Cache.Entries("analytics") {
		entryRealm, _ := entry["realm"].(string)
		if entryRealm != realm {
			continue
		}
		if fingerprint, _ := entry["pricing"].(string); fingerprint != CacheFingerprint(s.pricingFingerprint()) {
			continue
		}
		if fingerprint, _ := entry["realm_fp"].(string); fingerprint != CacheFingerprint(s.realmFingerprint()) {
			continue
		}
		offset, ok := ValidateCachePosition(entry, s.Store.Paths.Log)
		if !ok {
			continue
		}
		raw, ok := entry["data"].(map[string]any)
		if !ok {
			raw, ok = entry["maps"].(map[string]any)
		}
		if !ok {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var maps analyticsMaps
		if err := json.Unmarshal(encoded, &maps); err != nil {
			continue
		}
		if maps.Accts == nil {
			maps.Accts = map[string]*AnalyticsAccount{}
		}
		if maps.Models == nil {
			maps.Models = map[string]*AnalyticsModelRow{}
		}
		if maps.Keys == nil {
			maps.Keys = map[string]*AnalyticsKey{}
		}
		for _, entry := range maps.Accts {
			if entry.WindowModels == nil {
				entry.WindowModels = map[string]AnalyticsModel{}
			}
			if entry.AllModels == nil {
				entry.AllModels = map[string]AnalyticsModel{}
			}
		}
		for _, entry := range maps.Keys {
			if entry.WindowModels == nil {
				entry.WindowModels = map[string]AnalyticsModel{}
			}
			if entry.AllModels == nil {
				entry.AllModels = map[string]AnalyticsModel{}
			}
			if entry.Realms == nil {
				entry.Realms = map[string]int64{}
			}
		}
		newOffset, err := s.scan(ctx, s.Store.Paths.Log, offset, func(row Row) error {
			if !s.matchesRealm(row, realm) {
				return nil
			}
			cost := s.costFor(row)
			cny := 0.0
			if cost.Known {
				cny = cost.CNY
			}
			s.foldAnalyticsRow(row, realm, &maps, false, cny)
			return nil
		})
		if err != nil {
			return analyticsMaps{}, false
		}
		s.saveAnalytics(realm, maps, newOffset)
		return maps, true
	}
	return analyticsMaps{}, false
}

func (s *Service) saveAnalytics(realm string, maps analyticsMaps, offset int64) {
	if s.Cache == nil || offset <= 0 {
		return
	}
	position, ok := CachePosition(s.Store.Paths.Log, offset)
	if !ok {
		return
	}
	entry := map[string]any{
		"realm":    realm,
		"pricing":  CacheFingerprint(s.pricingFingerprint()),
		"realm_fp": CacheFingerprint(s.realmFingerprint()),
		"data":     maps,
	}
	for key, value := range position {
		entry[key] = value
	}
	_ = s.Cache.Save("analytics", entry)
}

func mirrorAllTimeToWindow(maps *analyticsMaps) {
	for _, entry := range maps.Accts {
		entry.Window = entry.AllTime
		entry.WindowModels = cloneModelMap(entry.AllModels)
	}
	for _, entry := range maps.Models {
		entry.Window = entry.AllTime
	}
	for _, entry := range maps.Keys {
		entry.Window = entry.AllTime
		entry.WindowModels = cloneModelMap(entry.AllModels)
	}
}

func finalizeAnalyticsMaps(maps *analyticsMaps) {
	finalizeAnalyticsStat(&maps.Summary)
	for _, entry := range maps.Accts {
		finalizeAnalyticsStat(&entry.Window)
		finalizeAnalyticsStat(&entry.AllTime)
	}
	for _, entry := range maps.Models {
		finalizeAnalyticsStat(&entry.Window)
		finalizeAnalyticsStat(&entry.AllTime)
	}
	for _, entry := range maps.Keys {
		finalizeAnalyticsStat(&entry.Window)
		finalizeAnalyticsStat(&entry.AllTime)
	}
}

func (s *Service) analyticsPayload(allMaps, winMaps analyticsMaps, realm string, window Range) map[string]any {
	accounts := make([]map[string]any, 0, len(allMaps.Accts))
	for uid, entry := range allMaps.Accts {
		win := winMaps.Accts[uid]
		if win == nil {
			win = &AnalyticsAccount{UID: uid, WindowModels: map[string]AnalyticsModel{}}
		}
		accounts = append(accounts, map[string]any{
			"uid":           uid,
			"nickname":      entry.Nickname,
			"realm":         entry.Realm,
			"domain":        entry.Domain,
			"window":        win.Window,
			"all_time":      entry.AllTime,
			"window_models": win.WindowModels,
			"all_models":    entry.AllModels,
		})
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		left := accounts[i]["window"].(AnalyticsStat)
		right := accounts[j]["window"].(AnalyticsStat)
		if left.TotalTokens != right.TotalTokens {
			return left.TotalTokens > right.TotalTokens
		}
		return accounts[i]["all_time"].(AnalyticsStat).TotalTokens > accounts[j]["all_time"].(AnalyticsStat).TotalTokens
	})

	models := make([]map[string]any, 0, len(allMaps.Models))
	for id, entry := range allMaps.Models {
		win := winMaps.Models[id]
		if win == nil {
			win = &AnalyticsModelRow{Model: id}
		}
		models = append(models, map[string]any{
			"model": id, "window": win.Window, "all_time": entry.AllTime,
		})
	}
	sort.SliceStable(models, func(i, j int) bool {
		left := models[i]["window"].(AnalyticsStat)
		right := models[j]["window"].(AnalyticsStat)
		if left.TotalTokens != right.TotalTokens {
			return left.TotalTokens > right.TotalTokens
		}
		return models[i]["all_time"].(AnalyticsStat).TotalTokens > models[j]["all_time"].(AnalyticsStat).TotalTokens
	})

	keys := s.buildKeyRows(allMaps, winMaps, realm)
	windowSince := any(nil)
	windowUntil := any(nil)
	if window.Since != nil {
		windowSince = float64(window.Since.Unix())
	}
	if window.Until != nil {
		windowUntil = float64(window.Until.Unix())
	}
	realmLabel := "all"
	if realm != "" {
		realmLabel = realm
	}
	return map[string]any{
		"window":          map[string]any{"since": windowSince, "until": windowUntil},
		"realm":           realmLabel,
		"usd_cny":         s.usdCNY(),
		"summary":         map[string]any{"window": winMaps.Summary, "all_time": allMaps.Summary},
		"all_time":        allMaps.Summary,
		"key_model_top_n": KeyModelTopN,
		"accounts":        accounts,
		"models":          models,
		"keys":            keys,
	}
}

func (s *Service) buildKeyRows(allMaps, winMaps analyticsMaps, realm string) []map[string]any {
	type keyRow struct {
		key      string
		name     string
		declared string
		enabled  bool
		source   string
		window   AnalyticsStat
		allTime  AnalyticsStat
		winMod   map[string]AnalyticsModel
		allMod   map[string]AnalyticsModel
		realms   map[string]int64
		lastAt   float64
	}
	rows := map[string]*keyRow{}
	add := func(id, name, declared string, enabled bool, source string) *keyRow {
		rows[id] = &keyRow{
			key: id, name: name, declared: declared, enabled: enabled, source: source,
			winMod: map[string]AnalyticsModel{}, allMod: map[string]AnalyticsModel{},
			realms: map[string]int64{},
		}
		return rows[id]
	}
	if s.Keys != nil {
		for _, key := range s.Keys {
			if key.ID == "" {
				continue
			}
			if realm != "" && key.Realm != "" && key.Realm != realm {
				continue
			}
			add(key.ID, defaultAnalyticsName(key.Name, key.ID), key.Realm, key.Enabled, "panel")
		}
	}
	for id, entry := range allMaps.Keys {
		switch id {
		case KeyBucketBefore, KeyBucketAnon:
			row := rows[id]
			if row == nil {
				label := "(切换前)"
				if id == KeyBucketAnon {
					label = "(无 key)"
				}
				row = add(id, label, "", false, "bucket")
			}
			row.window = winStat(winMaps.Keys, id)
			row.allTime = entry.AllTime
			row.winMod = winModels(winMaps.Keys, id)
			row.allMod = entry.AllModels
			row.realms = entry.Realms
			row.lastAt = entry.LastAt
		default:
			row := rows[id]
			if row != nil {
				row.window = winStat(winMaps.Keys, id)
				row.allTime = entry.AllTime
				row.winMod = winModels(winMaps.Keys, id)
				row.allMod = entry.AllModels
				row.realms = entry.Realms
				row.lastAt = entry.LastAt
				continue
			}
			if s.Keys == nil {
				row = add(id, s.keyDisplay(id), "", true, "log")
				row.window = winStat(winMaps.Keys, id)
				row.allTime = entry.AllTime
				row.winMod = winModels(winMaps.Keys, id)
				row.allMod = entry.AllModels
				row.realms = entry.Realms
				row.lastAt = entry.LastAt
				continue
			}
			unknown := rows[KeyBucketUnknown]
			if unknown == nil {
				unknown = add(KeyBucketUnknown, "(未知 key)", "", false, "bucket")
			}
			unknown.window = addAnalyticsStat(unknown.window, winStat(winMaps.Keys, id))
			unknown.allTime = addAnalyticsStat(unknown.allTime, entry.AllTime)
			mergeAnalyticsModels(unknown.winMod, winModels(winMaps.Keys, id))
			mergeAnalyticsModels(unknown.allMod, entry.AllModels)
			for realmName, count := range entry.Realms {
				unknown.realms[realmName] += count
			}
			if entry.LastAt > unknown.lastAt {
				unknown.lastAt = entry.LastAt
			}
		}
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row.source == "panel" && !row.enabled && row.window.Requests == 0 && row.window.Errors == 0 {
			continue
		}
		models, other := topModels(row.winMod)
		crossRealm := 0
		for name := range row.realms {
			if name != "" {
				crossRealm++
			}
		}
		out = append(out, map[string]any{
			"key": row.key, "name": row.name, "realm": row.declared,
			"enabled": row.enabled, "source": row.source,
			"window": row.window, "all_time": row.allTime,
			"window_models": row.winMod, "all_models": row.allMod,
			"realms": row.realms, "last_at": row.lastAt,
			"models": models, "models_other": other,
			"cross_realm": crossRealm > 1,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		left := out[i]["window"].(AnalyticsStat)
		right := out[j]["window"].(AnalyticsStat)
		if left.TotalTokens != right.TotalTokens {
			return left.TotalTokens > right.TotalTokens
		}
		if out[i]["all_time"].(AnalyticsStat).TotalTokens != out[j]["all_time"].(AnalyticsStat).TotalTokens {
			return out[i]["all_time"].(AnalyticsStat).TotalTokens > out[j]["all_time"].(AnalyticsStat).TotalTokens
		}
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	return out
}

func winStat(source map[string]*AnalyticsKey, id string) AnalyticsStat {
	if entry := source[id]; entry != nil {
		return entry.Window
	}
	return AnalyticsStat{}
}

func winModels(source map[string]*AnalyticsKey, id string) map[string]AnalyticsModel {
	if entry := source[id]; entry != nil {
		return entry.WindowModels
	}
	return map[string]AnalyticsModel{}
}

func addAnalyticsStat(left, right AnalyticsStat) AnalyticsStat {
	left.Requests += right.Requests
	left.Errors += right.Errors
	left.PromptTokens += right.PromptTokens
	left.CompletionTokens += right.CompletionTokens
	left.ReasoningTokens += right.ReasoningTokens
	left.CachedTokens += right.CachedTokens
	left.TotalTokens += right.TotalTokens
	left.Credit += right.Credit
	left.CostCNY += right.CostCNY
	left.TTFTSum += right.TTFTSum
	left.TTFTN += right.TTFTN
	left.SpeedSum += right.SpeedSum
	left.SpeedN += right.SpeedN
	left.ElapsedSum += right.ElapsedSum
	left.ElapsedN += right.ElapsedN
	return left
}

func mergeAnalyticsModels(target map[string]AnalyticsModel, source map[string]AnalyticsModel) {
	for id, value := range source {
		existing := target[id]
		existing.Requests += value.Requests
		existing.Tokens += value.Tokens
		existing.Reasoning += value.Reasoning
		existing.CostCNY += value.CostCNY
		target[id] = existing
	}
}

func defaultAnalyticsName(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

func topModels(source map[string]AnalyticsModel) ([]map[string]any, map[string]any) {
	type pair struct {
		id    string
		value AnalyticsModel
	}
	items := make([]pair, 0, len(source))
	for id, value := range source {
		items = append(items, pair{id, value})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].value.Tokens != items[j].value.Tokens {
			return items[i].value.Tokens > items[j].value.Tokens
		}
		if items[i].value.Requests != items[j].value.Requests {
			return items[i].value.Requests > items[j].value.Requests
		}
		return items[i].id < items[j].id
	})
	head := make([]map[string]any, 0, KeyModelTopN)
	for index := 0; index < len(items) && index < KeyModelTopN; index++ {
		head = append(head, map[string]any{
			"model": items[index].id, "requests": items[index].value.Requests,
			"tokens": items[index].value.Tokens, "reasoning": items[index].value.Reasoning,
		})
	}
	if len(items) <= KeyModelTopN {
		return head, nil
	}
	var requests, tokens, reasoning int64
	for _, item := range items[KeyModelTopN:] {
		requests += item.value.Requests
		tokens += item.value.Tokens
		reasoning += item.value.Reasoning
	}
	return head, map[string]any{
		"model": "(其他)", "other": true, "models": len(items) - KeyModelTopN,
		"requests": requests, "tokens": tokens, "reasoning": reasoning,
	}
}

func round1(value float64) float64 { return round0(value*10) / 10 }

func round0(value float64) float64 {
	if value >= 0 {
		return float64(int64(value + 0.5))
	}
	return float64(int64(value - 0.5))
}

func (s *Service) logSize() int64 {
	if s == nil || s.Store == nil {
		return 0
	}
	file, err := os.Open(s.Store.Paths.Log)
	if err != nil {
		return 0
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return 0
	}
	buffer := make([]byte, 1)
	for position := info.Size() - 1; position >= 0; position-- {
		if _, err := file.ReadAt(buffer, position); err != nil {
			return 0
		}
		if buffer[0] == '\n' {
			return position + 1
		}
	}
	return 0
}
