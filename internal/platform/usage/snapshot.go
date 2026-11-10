package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// ModelBucket is the v1 by_model / by_model_realm / by_model_acct shape.
type ModelBucket struct {
	Requests         int64            `json:"requests"`
	Accounts         map[string]int64 `json:"accounts"`
	CostCNY          float64          `json:"cost_cny"`
	PromptTokens     int64            `json:"prompt_tokens"`
	CompletionTokens int64            `json:"completion_tokens"`
	ReasoningTokens  int64            `json:"reasoning_tokens"`
	CachedTokens     int64            `json:"cached_tokens"`
	TotalTokens      int64            `json:"total_tokens"`
	Credit           float64          `json:"credit"`
}

// UsageStats is the v1 /usage summary fold.
type UsageStats struct {
	Requests         int64                                        `json:"requests"`
	Errors           int64                                        `json:"errors"`
	PromptTokens     int64                                        `json:"prompt_tokens"`
	CompletionTokens int64                                        `json:"completion_tokens"`
	ReasoningTokens  int64                                        `json:"reasoning_tokens"`
	CachedTokens     int64                                        `json:"cached_tokens"`
	TotalTokens      int64                                        `json:"total_tokens"`
	Credit           float64                                      `json:"credit"`
	CostCNY          float64                                      `json:"cost_cny"`
	CostMissing      map[string]int64                             `json:"cost_missing"`
	ByModel          map[string]ModelBucket                       `json:"by_model"`
	ByModelRealm     map[string]map[string]ModelBucket            `json:"by_model_realm"`
	ByModelAcct      map[string]map[string]map[string]ModelBucket `json:"by_model_acct"`
	TTFTMSSum        float64                                      `json:"ttft_ms_sum"`
	TTFTSamples      int64                                        `json:"ttft_samples"`
	GenMSSum         float64                                      `json:"gen_ms_sum"`
	GenSamples       int64                                        `json:"gen_samples"`
	WallMSSum        float64                                      `json:"wall_ms_sum"`
	WallSamples      int64                                        `json:"wall_samples"`
}

func newUsageStats() UsageStats {
	return UsageStats{
		CostMissing:  map[string]int64{},
		ByModel:      map[string]ModelBucket{},
		ByModelRealm: map[string]map[string]ModelBucket{},
		ByModelAcct:  map[string]map[string]map[string]ModelBucket{},
	}
}

func newModelBucket() ModelBucket {
	return ModelBucket{Accounts: map[string]int64{}}
}

var usageTokenFields = []string{
	"prompt_tokens", "completion_tokens", "reasoning_tokens", "cached_tokens", "total_tokens",
}

func addBucketTokens(bucket *ModelBucket, row Row) {
	bucket.Requests++
	bucket.PromptTokens += intNumber(row, "prompt_tokens")
	bucket.CompletionTokens += intNumber(row, "completion_tokens")
	bucket.ReasoningTokens += intNumber(row, "reasoning_tokens")
	bucket.CachedTokens += intNumber(row, "cached_tokens")
	bucket.TotalTokens += intNumber(row, "total_tokens")
	bucket.Credit += number(row, "credit")
}

func foldCost(stats *UsageStats, cost costLike, model string) {
	if cost.Known {
		stats.CostCNY += cost.CNY
		return
	}
	if cost.Disabled {
		return
	}
	if stats.CostMissing == nil {
		stats.CostMissing = map[string]int64{}
	}
	if model == "" {
		model = "unknown"
	}
	stats.CostMissing[model]++
}

// costLike avoids importing pricing at every call site while keeping the
// three fields the fold needs explicit.
type costLike struct {
	Known    bool
	Disabled bool
	CNY      float64
}

func (s *Service) foldSnapshotRow(stats *UsageStats, row Row, realm string) {
	if !s.matchesRealm(row, realm) {
		return
	}
	outcome := RowOutcome(row)
	cost := s.costFor(row)
	model := rowString(row, "model")
	if outcome != "completed" {
		stats.Errors++
		if outcome != "client_aborted" {
			stats.Credit += number(row, "credit")
			foldCost(stats, costLike{cost.Known, cost.Disabled, cost.CNY}, model)
		}
		return
	}
	stats.Requests++
	stats.PromptTokens += intNumber(row, "prompt_tokens")
	stats.CompletionTokens += intNumber(row, "completion_tokens")
	stats.ReasoningTokens += intNumber(row, "reasoning_tokens")
	stats.CachedTokens += intNumber(row, "cached_tokens")
	stats.TotalTokens += intNumber(row, "total_tokens")
	stats.Credit += number(row, "credit")
	foldCost(stats, costLike{cost.Known, cost.Disabled, cost.CNY}, model)
	if value := number(row, "ttft_ms"); value != 0 {
		stats.TTFTMSSum += value
		stats.TTFTSamples++
	}
	if value := number(row, "gen_ms"); value != 0 {
		stats.GenMSSum += value
		stats.GenSamples++
	}
	if value := number(row, "elapsed_ms"); value != 0 {
		stats.WallMSSum += value
		stats.WallSamples++
	}

	modelKey := model
	if modelKey == "" {
		modelKey = "unknown"
	}
	realmKey := s.rowRealm(row)
	account := rowString(row, "account")
	accountKey := account
	if accountKey == "" {
		accountKey = "(unattributed)"
	}
	per := stats.ByModel[modelKey]
	if per.Accounts == nil {
		per.Accounts = map[string]int64{}
	}
	addBucketTokens(&per, row)
	if cost.Known {
		per.CostCNY += cost.CNY
	}
	if account != "" {
		per.Accounts[account]++
	}
	stats.ByModel[modelKey] = per

	realms := stats.ByModelRealm[modelKey]
	if realms == nil {
		realms = map[string]ModelBucket{}
		stats.ByModelRealm[modelKey] = realms
	}
	perRealm := realms[realmKey]
	if perRealm.Accounts == nil {
		perRealm.Accounts = map[string]int64{}
	}
	addBucketTokens(&perRealm, row)
	if cost.Known {
		perRealm.CostCNY += cost.CNY
	}
	if account != "" {
		perRealm.Accounts[account]++
	}
	realms[realmKey] = perRealm

	accounts := stats.ByModelAcct[modelKey]
	if accounts == nil {
		accounts = map[string]map[string]ModelBucket{}
		stats.ByModelAcct[modelKey] = accounts
	}
	byAccount := accounts[realmKey]
	if byAccount == nil {
		byAccount = map[string]ModelBucket{}
		accounts[realmKey] = byAccount
	}
	perAccount := byAccount[accountKey]
	if perAccount.Accounts == nil {
		perAccount.Accounts = map[string]int64{}
	}
	addBucketTokens(&perAccount, row)
	if cost.Known {
		perAccount.CostCNY += cost.CNY
	}
	if account != "" {
		perAccount.Accounts[account]++
	}
	byAccount[accountKey] = perAccount
}

// Snapshot returns the v1 usage summary API view.
func (s *Service) Snapshot(ctx context.Context, opts SnapshotOptions) (map[string]any, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("usage: nil service")
	}
	window := ParseRange(opts.Range, opts.Since, opts.Until)
	realm := scopeRealm(opts.Realm)
	cacheKey := fmt.Sprintf("%s|%s|%s", realm, boundKey(window.Since), boundKey(window.Until))
	if value, _, ok := s.resultGet("snapshot", cacheKey); ok {
		return value.(map[string]any), nil
	}
	stats, err := s.snapshotStats(ctx, realm, window)
	if err != nil {
		return nil, err
	}
	s.resultPutQuery("snapshot", cacheKey, SnapshotETagKey(opts.Realm, window), stats, s.statsTTL())
	return stats, nil
}

func (s *Service) snapshotStats(ctx context.Context, realm string, window Range) (map[string]any, error) {
	if window.Since == nil && window.Until == nil {
		if stats, ok := s.adoptSnapshot(ctx, realm); ok {
			return stats, nil
		}
	}
	stats := newUsageStats()
	offset, err := s.scanAll(ctx, func(row Row) error {
		if !inRange(number(row, "at"), window) {
			return nil
		}
		s.foldSnapshotRow(&stats, row, realm)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if window.Since == nil && window.Until == nil {
		s.saveSnapshotAt(realm, stats, offset)
	}
	return s.snapshotMap(stats, realm), nil
}

func (s *Service) adoptSnapshot(ctx context.Context, realm string) (map[string]any, bool) {
	if s.Cache == nil {
		return nil, false
	}
	entries := s.Cache.Entries("snapshot")
	for _, entry := range entries {
		entryRealm, _ := entry["realm"].(string)
		if entryRealm != realm {
			continue
		}
		if !s.cacheFingerprintOK(entry) {
			continue
		}
		offset, ok := ValidateCachePosition(entry, s.Store.Paths.Log)
		if !ok {
			continue
		}
		raw, ok := entry["data"].(map[string]any)
		if !ok {
			raw, ok = entry["snap"].(map[string]any)
		}
		if !ok {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var stats UsageStats
		if err := json.Unmarshal(encoded, &stats); err != nil {
			continue
		}
		if stats.CostMissing == nil {
			stats.CostMissing = map[string]int64{}
		}
		if stats.ByModel == nil {
			stats.ByModel = map[string]ModelBucket{}
		}
		if stats.ByModelRealm == nil {
			stats.ByModelRealm = map[string]map[string]ModelBucket{}
		}
		if stats.ByModelAcct == nil {
			stats.ByModelAcct = map[string]map[string]map[string]ModelBucket{}
		}
		newOffset, err := s.scan(ctx, s.Store.Paths.Log, offset, func(row Row) error {
			s.foldSnapshotRow(&stats, row, realm)
			return nil
		})
		if err != nil {
			return nil, false
		}
		s.saveSnapshotAt(realm, stats, newOffset)
		return s.snapshotMap(stats, realm), true
	}
	return nil, false
}

func (s *Service) saveSnapshotAt(realm string, stats UsageStats, offset int64) {
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
		"data":     stats,
	}
	for key, value := range position {
		entry[key] = value
	}
	_ = s.Cache.Save("snapshot", entry)
}

func (s *Service) snapshotMap(stats UsageStats, realm string) map[string]any {
	out := map[string]any{
		"requests":          stats.Requests,
		"errors":            stats.Errors,
		"prompt_tokens":     stats.PromptTokens,
		"completion_tokens": stats.CompletionTokens,
		"reasoning_tokens":  stats.ReasoningTokens,
		"cached_tokens":     stats.CachedTokens,
		"total_tokens":      stats.TotalTokens,
		"credit":            stats.Credit,
		"cost_cny":          stats.CostCNY,
		"cost_missing":      stats.CostMissing,
		"by_model":          stats.ByModel,
		"by_model_realm":    stats.ByModelRealm,
		"by_model_acct":     stats.ByModelAcct,
		"ttft_ms_sum":       stats.TTFTMSSum,
		"ttft_samples":      stats.TTFTSamples,
		"gen_ms_sum":        stats.GenMSSum,
		"gen_samples":       stats.GenSamples,
		"wall_ms_sum":       stats.WallMSSum,
		"wall_samples":      stats.WallSamples,
	}
	s.decorateSnapshotMap(out, realm)
	return out
}

func (s *Service) decorateSnapshot(stats *map[string]any, realm string) {
	s.decorateSnapshotMap(*stats, realm)
}

func (s *Service) decorateSnapshotMap(out map[string]any, realm string) {
	started := float64(s.started().UnixNano()) / 1e9
	out["started"] = started
	out["since"] = formatISO(started)
	out["log_file"] = s.Store.Paths.Log
	if realm == "" {
		out["realm"] = "all"
	} else {
		out["realm"] = realm
	}
	out["usd_cny"] = s.usdCNY()
	accounts := map[string]any{}
	for uid, info := range s.Accounts {
		accounts[uid] = map[string]any{"nickname": info.Nickname, "realm": info.Realm}
	}
	out["accounts_map"] = accounts
	current := s.CurrentAccount
	out["account"] = map[string]any{
		"uid":             current.UID,
		"domain":          current.Domain,
		"issuer":          current.Issuer,
		"credential_file": current.CredentialFile,
		"expires_at":      current.ExpiresAt,
		"accounts":        current.Accounts,
		"accounts_ready":  current.AccountsReady,
	}
}

func boundKey(value *time.Time) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value.Unix())
}

func statFile(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func sortModelBuckets(source map[string]ModelBucket) []string {
	ids := make([]string, 0, len(source))
	for id := range source {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
