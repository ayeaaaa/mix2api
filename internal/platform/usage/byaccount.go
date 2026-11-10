package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// ByAccountBucket is the whole-log per-account fold.
type ByAccountBucket struct {
	Account          string           `json:"account"`
	Requests         int64            `json:"requests"`
	PromptTokens     int64            `json:"prompt_tokens"`
	CompletionTokens int64            `json:"completion_tokens"`
	ReasoningTokens  int64            `json:"reasoning_tokens"`
	CachedTokens     int64            `json:"cached_tokens"`
	TotalTokens      int64            `json:"total_tokens"`
	Models           map[string]int64 `json:"models"`
}

func (s *Service) foldByAccount(buckets map[string]*ByAccountBucket, row Row) {
	if truthy(row["error"]) {
		return
	}
	account := rowString(row, "account")
	if account == "" {
		account = "(unattributed)"
	}
	bucket := buckets[account]
	if bucket == nil {
		bucket = &ByAccountBucket{Account: account, Models: map[string]int64{}}
		buckets[account] = bucket
	}
	bucket.Requests++
	bucket.PromptTokens += intNumber(row, "prompt_tokens")
	bucket.CompletionTokens += intNumber(row, "completion_tokens")
	bucket.ReasoningTokens += intNumber(row, "reasoning_tokens")
	bucket.CachedTokens += intNumber(row, "cached_tokens")
	bucket.TotalTokens += intNumber(row, "total_tokens")
	model := rowString(row, "model")
	if model == "" {
		model = "?"
	}
	if bucket.Models == nil {
		bucket.Models = map[string]int64{}
	}
	bucket.Models[model]++
}

// ByAccount aggregates the whole log per account.
func (s *Service) ByAccount(ctx context.Context) (map[string]any, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("usage: nil service")
	}
	cacheKey := "all"
	if value, _, ok := s.resultGet("by_account", cacheKey); ok {
		return value.(map[string]any), nil
	}
	buckets, err := s.byAccountBuckets(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]*ByAccountBucket, 0, len(buckets))
	for _, bucket := range buckets {
		rows = append(rows, bucket)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].TotalTokens != rows[j].TotalTokens {
			return rows[i].TotalTokens > rows[j].TotalTokens
		}
		return rows[i].Account < rows[j].Account
	})
	accounts := make([]map[string]any, 0, len(rows))
	for _, bucket := range rows {
		models := topAccountModels(bucket.Models, 5)
		accounts = append(accounts, map[string]any{
			"account": bucket.Account, "requests": bucket.Requests,
			"prompt_tokens": bucket.PromptTokens, "completion_tokens": bucket.CompletionTokens,
			"reasoning_tokens": bucket.ReasoningTokens, "cached_tokens": bucket.CachedTokens,
			"total_tokens": bucket.TotalTokens, "models": models,
		})
	}
	payload := map[string]any{"accounts": accounts}
	s.resultPut("by_account", cacheKey, payload, s.statsTTL())
	return payload, nil
}

func (s *Service) byAccountBuckets(ctx context.Context) (map[string]*ByAccountBucket, error) {
	if buckets, ok := s.adoptByAccount(ctx); ok {
		return buckets, nil
	}
	buckets := map[string]*ByAccountBucket{}
	if err := s.scanAll(ctx, func(row Row) error {
		s.foldByAccount(buckets, row)
		return nil
	}); err != nil {
		return nil, err
	}
	s.saveByAccount(buckets, s.logSize())
	return buckets, nil
}

func (s *Service) adoptByAccount(ctx context.Context) (map[string]*ByAccountBucket, bool) {
	if s.Cache == nil {
		return nil, false
	}
	for _, entry := range s.Cache.Entries("by_account") {
		offset, ok := ValidateCachePosition(entry, s.Store.Paths.Log)
		if !ok {
			continue
		}
		raw, ok := entry["data"].(map[string]any)
		if !ok {
			raw, ok = entry["buckets"].(map[string]any)
		}
		if !ok {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var buckets map[string]*ByAccountBucket
		if err := json.Unmarshal(encoded, &buckets); err != nil {
			continue
		}
		if buckets == nil {
			buckets = map[string]*ByAccountBucket{}
		}
		for _, bucket := range buckets {
			if bucket.Models == nil {
				bucket.Models = map[string]int64{}
			}
		}
		newOffset, err := s.scan(ctx, s.Store.Paths.Log, offset, func(row Row) error {
			s.foldByAccount(buckets, row)
			return nil
		})
		if err != nil {
			return nil, false
		}
		s.saveByAccount(buckets, newOffset)
		return buckets, true
	}
	return nil, false
}

func (s *Service) saveByAccount(buckets map[string]*ByAccountBucket, offset int64) {
	if s.Cache == nil || offset <= 0 {
		return
	}
	position, ok := CachePosition(s.Store.Paths.Log, offset)
	if !ok {
		return
	}
	entry := map[string]any{"data": buckets}
	for key, value := range position {
		entry[key] = value
	}
	_ = s.Cache.Save("by_account", entry)
}

func topAccountModels(models map[string]int64, limit int) [][]any {
	type pair struct {
		id    string
		count int64
	}
	items := make([]pair, 0, len(models))
	for id, count := range models {
		items = append(items, pair{id, count})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].id < items[j].id
	})
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([][]any, 0, len(items))
	for _, item := range items {
		out = append(out, []any{item.id, item.count})
	}
	return out
}
