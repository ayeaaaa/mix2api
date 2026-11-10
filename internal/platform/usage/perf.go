package usage

import (
	"context"
	"fmt"
	"math"
	"sort"
)

type perfBucket struct {
	total   int64
	ok      int64
	err     int64
	aborted int64
	ttft    []float64
	gen     []float64
	wall    []float64
	tps     []float64
	hits    []float64
}

type perfAgg struct {
	global     perfBucket
	byModel    map[string]*perfBucket
	byRealm    map[string]map[string]*perfBucket
	byAcct     map[string]map[string]map[string]*perfBucket
	total      int64
	ok         int64
	err        int64
	aborted    int64
	sampleFrom any
}

func newPerfAgg() *perfAgg {
	return &perfAgg{
		byModel: map[string]*perfBucket{},
		byRealm: map[string]map[string]*perfBucket{},
		byAcct:  map[string]map[string]map[string]*perfBucket{},
	}
}

func (a *perfAgg) bucket(model, realm, account string) (*perfBucket, *perfBucket, *perfBucket) {
	mb := a.byModel[model]
	if mb == nil {
		mb = &perfBucket{}
		a.byModel[model] = mb
	}
	realms := a.byRealm[model]
	if realms == nil {
		realms = map[string]*perfBucket{}
		a.byRealm[model] = realms
	}
	rb := realms[realm]
	if rb == nil {
		rb = &perfBucket{}
		realms[realm] = rb
	}
	accounts := a.byAcct[model]
	if accounts == nil {
		accounts = map[string]map[string]*perfBucket{}
		a.byAcct[model] = accounts
	}
	byAccount := accounts[realm]
	if byAccount == nil {
		byAccount = map[string]*perfBucket{}
		accounts[realm] = byAccount
	}
	ab := byAccount[account]
	if ab == nil {
		ab = &perfBucket{}
		byAccount[account] = ab
	}
	return mb, rb, ab
}

func perfValue(row Row, key string) (float64, bool) {
	value, ok := row[key]
	if !ok || value == nil {
		return 0, false
	}
	number, ok := numericValue(value)
	return number, ok
}

// Perf returns the v1 latency percentiles payload. Percentiles always come
// from raw tail samples; no aggregate approximation is used.
func (s *Service) Perf(ctx context.Context, opts PerfOptions) (map[string]any, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("usage: nil service")
	}
	sample := opts.Sample
	if sample <= 0 {
		sample = 5000
	}
	if sample < 10 {
		sample = 10
	}
	if sample > 20000 {
		sample = 20000
	}
	window := ParseRange(opts.Range, opts.Since, opts.Until)
	realm := scopeRealm(opts.Realm)
	cacheKey := fmt.Sprintf("%d|%s|%s|%s", sample, realm, boundKey(window.Since), boundKey(window.Until))
	if value, _, ok := s.resultGet("perf", cacheKey); ok {
		return value.(map[string]any), nil
	}
	lines, err := tailLines(s.Store.Paths.Log, sample)
	if err != nil {
		return nil, err
	}
	agg := newPerfAgg()
	for _, line := range lines {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		row, ok := decodeUsageLine(line)
		if !ok {
			continue
		}
		if agg.sampleFrom == nil {
			if _, exists := row["at"]; exists {
				agg.sampleFrom = number(row, "at")
			}
		}
		if realm != "" && s.rowRealm(row) != realm {
			continue
		}
		at := number(row, "at")
		if !inRange(at, window) {
			continue
		}
		agg.total++
		model := rowString(row, "model")
		if model == "" {
			model = "unknown"
		}
		modelRealm := s.rowRealm(row)
		account := rowString(row, "account")
		if account == "" {
			account = "(unattributed)"
		}
		mb, rb, ab := agg.bucket(model, modelRealm, account)
		for _, bucket := range []*perfBucket{mb, rb, ab} {
			bucket.total++
		}
		outcome := RowOutcome(row)
		if outcome == "client_aborted" {
			agg.aborted++
			for _, bucket := range []*perfBucket{mb, rb, ab} {
				bucket.aborted++
			}
			if elapsed, ok := perfValue(row, "elapsed_ms"); ok && elapsed != 0 {
				agg.global.wall = append(agg.global.wall, elapsed)
				for _, bucket := range []*perfBucket{mb, rb, ab} {
					bucket.wall = append(bucket.wall, elapsed)
				}
			}
			continue
		}
		if outcome != "completed" {
			agg.err++
			for _, bucket := range []*perfBucket{mb, rb, ab} {
				bucket.err++
			}
			if elapsed, ok := perfValue(row, "elapsed_ms"); ok && elapsed != 0 {
				agg.global.wall = append(agg.global.wall, elapsed)
				for _, bucket := range []*perfBucket{mb, rb, ab} {
					bucket.wall = append(bucket.wall, elapsed)
				}
			}
			continue
		}
		agg.ok++
		for _, bucket := range []*perfBucket{mb, rb, ab} {
			bucket.ok++
		}
		if ttft, ok := perfValue(row, "ttft_ms"); ok {
			agg.global.ttft = append(agg.global.ttft, ttft)
			for _, bucket := range []*perfBucket{mb, rb, ab} {
				bucket.ttft = append(bucket.ttft, ttft)
			}
		}
		if gen, ok := perfValue(row, "gen_ms"); ok {
			agg.global.gen = append(agg.global.gen, gen)
			for _, bucket := range []*perfBucket{mb, rb, ab} {
				bucket.gen = append(bucket.gen, gen)
			}
		}
		if elapsed, ok := perfValue(row, "elapsed_ms"); ok {
			agg.global.wall = append(agg.global.wall, elapsed)
			for _, bucket := range []*perfBucket{mb, rb, ab} {
				bucket.wall = append(bucket.wall, elapsed)
			}
		}
		if tps, ok := perfValue(row, "tokens_per_sec"); ok && tps != 0 {
			agg.global.tps = append(agg.global.tps, tps)
			for _, bucket := range []*perfBucket{mb, rb, ab} {
				bucket.tps = append(bucket.tps, tps)
			}
		}
		if hit, ok := perfValue(row, "cache_hit_pct"); ok {
			agg.global.hits = append(agg.global.hits, hit)
			for _, bucket := range []*perfBucket{mb, rb, ab} {
				bucket.hits = append(bucket.hits, hit)
			}
		}
	}
	payload := map[string]any{
		"sampled":          agg.total,
		"sample_from":      agg.sampleFrom,
		"sample_capped":    len(lines) >= sample,
		"success":          agg.ok,
		"errors":           agg.err,
		"client_aborted":   agg.aborted,
		"success_rate_pct": successRate(agg.ok, agg.err),
		"ttft_ms":          perfBlock(agg.global.ttft),
		"generation_ms":    perfBlock(agg.global.gen),
		"wall_ms":          perfBlock(agg.global.wall),
		"tokens_per_sec":   perfBlock(agg.global.tps),
		"cache_hit_pct":    perfBlock(agg.global.hits),
		"by_model":         perfByModel(agg.byModel),
		"by_model_realm":   perfByRealm(agg.byRealm),
		"by_model_acct":    perfByAccount(agg.byAcct),
	}
	s.resultPutQuery("perf", cacheKey, PerfETagKey(opts.Realm, window, sample), payload, s.statsTTL())
	return payload, nil
}

func perfBlock(values []float64) any {
	if len(values) == 0 {
		return nil
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	count := len(ordered)
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	maxValue := ordered[count-1]
	rank := func(q float64) float64 {
		index := int(roundHalfEven((q / 100) * float64(count-1)))
		if index < 0 {
			index = 0
		}
		if index >= count {
			index = count - 1
		}
		return ordered[index]
	}
	return map[string]any{
		"avg":     round1(sum / float64(count)),
		"p50":     rank(50),
		"p90":     rank(90),
		"p99":     rank(99),
		"max":     maxValue,
		"samples": count,
	}
}

func successRate(ok, err int64) any {
	if ok+err == 0 {
		return nil
	}
	return round1(float64(ok) * 100 / float64(ok+err))
}

func perfByModel(source map[string]*perfBucket) map[string]any {
	out := map[string]any{}
	for model, bucket := range source {
		out[model] = perfBucketMap(bucket)
	}
	return out
}

func perfByRealm(source map[string]map[string]*perfBucket) map[string]any {
	out := map[string]any{}
	for model, realms := range source {
		inner := map[string]any{}
		for realm, bucket := range realms {
			inner[realm] = perfBucketMap(bucket)
		}
		out[model] = inner
	}
	return out
}

func perfByAccount(source map[string]map[string]map[string]*perfBucket) map[string]any {
	out := map[string]any{}
	for model, realms := range source {
		inner := map[string]any{}
		for realm, accounts := range realms {
			accountMap := map[string]any{}
			for account, bucket := range accounts {
				accountMap[account] = perfBucketMap(bucket)
			}
			inner[realm] = accountMap
		}
		out[model] = inner
	}
	return out
}

func perfBucketMap(bucket *perfBucket) map[string]any {
	return map[string]any{
		"requests":         bucket.total,
		"errors":           bucket.err,
		"client_aborted":   bucket.aborted,
		"success_rate_pct": successRate(bucket.ok, bucket.err),
		"ttft_ms":          perfBlock(bucket.ttft),
		"generation_ms":    perfBlock(bucket.gen),
		"wall_ms":          perfBlock(bucket.wall),
		"tokens_per_sec":   perfBlock(bucket.tps),
		"cache_hit_pct":    perfBlock(bucket.hits),
	}
}

func roundHalfEven(value float64) float64 {
	floor := math.Floor(value)
	diff := value - floor
	if diff > 0.5 {
		return floor + 1
	}
	if diff < 0.5 {
		return floor
	}
	if math.Mod(floor, 2) == 0 {
		return floor
	}
	return floor + 1
}
