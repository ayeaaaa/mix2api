package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// SeriesBucket is one time bucket.
type SeriesBucket struct {
	At               float64 `json:"at"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Credit           float64 `json:"credit"`
}

// SeriesCredit is one credited request in the credit history.
type SeriesCredit struct {
	At          float64 `json:"at"`
	ISO         string  `json:"iso"`
	Model       string  `json:"model"`
	Account     string  `json:"account"`
	Credit      float64 `json:"credit"`
	TotalTokens int64   `json:"total_tokens"`
}

type seriesState struct {
	Minutes map[int64]*SeriesBucket `json:"minutes"`
	Credits []SeriesCredit          `json:"credits"`
	MaxAt   float64                 `json:"max_at"`
	// MinuteComplete is false when the state was adopted from a v1 cache that
	// only carried hour/day buckets. Minute windows then fall back to a scan;
	// hour/day windows can still be served by the hour-start pseudo-minutes.
	MinuteComplete bool `json:"minute_complete"`
}

func newSeriesState() seriesState {
	return seriesState{Minutes: map[int64]*SeriesBucket{}, MinuteComplete: true}
}

func (s *seriesState) fold(row Row) {
	at := number(row, "at")
	if at <= 0 {
		return
	}
	minute := int64(math.Floor(at/60)) * 60
	bucket := s.Minutes[minute]
	if bucket == nil {
		bucket = &SeriesBucket{At: float64(minute)}
		s.Minutes[minute] = bucket
	}
	outcome := RowOutcome(row)
	if outcome == "completed" {
		bucket.Requests++
		bucket.PromptTokens += intNumber(row, "prompt_tokens")
		bucket.CompletionTokens += intNumber(row, "completion_tokens")
		bucket.ReasoningTokens += intNumber(row, "reasoning_tokens")
		bucket.CachedTokens += intNumber(row, "cached_tokens")
		bucket.TotalTokens += intNumber(row, "total_tokens")
	} else {
		bucket.Errors++
	}
	credit := number(row, "credit")
	if outcome != "client_aborted" {
		bucket.Credit += credit
	}
	if credit > 0 {
		s.Credits = append(s.Credits, SeriesCredit{
			At: at, ISO: rowString(row, "iso"), Model: rowString(row, "model"),
			Account: rowString(row, "account"), Credit: credit,
			TotalTokens: intNumber(row, "total_tokens"),
		})
		if len(s.Credits) > 5000 {
			s.Credits = s.Credits[len(s.Credits)-5000:]
		}
	}
	if at > s.MaxAt {
		s.MaxAt = at
	}
}

// seriesPlan resolves the window and bucket size exactly once, so the data
// path and the ETag path cannot disagree about the resolved step.
func (s *Service) seriesPlan(opts SeriesOptions) (Range, time.Time, time.Time, int) {
	window := ParseRange(opts.Range, opts.Since, opts.Until)
	hi := s.now()
	if window.Until != nil {
		hi = *window.Until
	}
	lo := hi.Add(-24 * time.Hour)
	if window.Since != nil {
		lo = *window.Since
	}
	span := hi.Sub(lo).Seconds()
	if span < 1 {
		span = 1
	}
	step := opts.BucketSeconds
	if step > 0 {
		if step < 60 {
			step = 60
		}
	} else {
		switch {
		case span <= 6*3600:
			step = 60
		case span <= 14*86400:
			step = 3600
		default:
			step = 86400
		}
	}
	return window, lo, hi, step
}

// Timeseries returns bucketed token/credit series.
func (s *Service) Timeseries(ctx context.Context, opts SeriesOptions) (map[string]any, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("usage: nil service")
	}
	window, lo, hi, step := s.seriesPlan(opts)
	realm := scopeRealm(opts.Realm)
	pinnedLo := boundKey(window.Since)
	pinnedHi := boundKey(window.Until)
	if pinnedLo == "" {
		pinnedLo = "auto"
	}
	if pinnedHi == "" {
		pinnedHi = "auto"
	}
	cacheKey := fmt.Sprintf("%s|%s|%s|%d", realm, pinnedLo, pinnedHi, step)
	if value, _, ok := s.resultGet("series", cacheKey); ok {
		return value.(map[string]any), nil
	}
	state, err := s.seriesState(ctx, realm)
	if err != nil {
		return nil, err
	}
	buckets, credits, ok := seriesSlice(state, lo, hi, step)
	if !ok {
		buckets, credits, err = s.seriesScan(ctx, realm, lo, hi, step)
		if err != nil {
			return nil, err
		}
	}
	realmLabel := "all"
	if realm != "" {
		realmLabel = realm
	}
	payload := map[string]any{
		"ok":             true,
		"realm":          realmLabel,
		"bucket_seconds": step,
		"since":          float64(lo.Unix()),
		"until":          float64(hi.Unix()),
		"series":         buckets,
		"credits":        credits,
	}
	s.resultPutQuery("series", cacheKey, SeriesETagKeyResolved(opts.Realm, window, step), payload, s.statsTTL())
	return payload, nil
}

func (s *Service) seriesState(ctx context.Context, realm string) (seriesState, error) {
	if state, ok := s.adoptSeries(ctx, realm); ok {
		return state, nil
	}
	state := newSeriesState()
	offset, err := s.scanAll(ctx, func(row Row) error {
		if !s.matchesRealm(row, realm) {
			return nil
		}
		state.fold(row)
		return nil
	})
	if err != nil {
		return seriesState{}, err
	}
	s.saveSeries(realm, state, offset)
	return state, nil
}

func (s *Service) adoptSeries(ctx context.Context, realm string) (seriesState, bool) {
	if s.Cache == nil {
		return seriesState{}, false
	}
	for _, entry := range s.Cache.Entries("series") {
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
		var state seriesState
		if ok {
			encoded, err := json.Marshal(raw)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(encoded, &state); err != nil {
				continue
			}
		} else {
			converted, valid := seriesStateFromV1(entry)
			if !valid {
				continue
			}
			state = converted
		}
		if state.Minutes == nil {
			state.Minutes = map[int64]*SeriesBucket{}
		}
		newOffset, err := s.scan(ctx, s.Store.Paths.Log, offset, func(row Row) error {
			if !s.matchesRealm(row, realm) {
				return nil
			}
			state.fold(row)
			return nil
		})
		if err != nil {
			return seriesState{}, false
		}
		s.saveSeries(realm, state, newOffset)
		return state, true
	}
	return seriesState{}, false
}

func (s *Service) saveSeries(realm string, state seriesState, offset int64) {
	if s.Cache == nil || offset <= 0 {
		return
	}
	position, ok := CachePosition(s.Store.Paths.Log, offset)
	if !ok {
		return
	}
	entry := map[string]any{
		"realm": realm, "realm_fp": CacheFingerprint(s.realmFingerprint()), "data": state,
	}
	for key, value := range position {
		entry[key] = value
	}
	_ = s.Cache.Save("series", entry)
}

// seriesStateFromV1 converts a real v1 series checkpoint entry. v1 stores
// `minutes` as [minute, requests, errors, prompt, completion, reasoning,
// cached, total, credit] rows; older entries without that ring carry
// `days[].hours` instead, which we materialize as hour-start pseudo-minutes.
func seriesStateFromV1(entry map[string]any) (seriesState, bool) {
	state := newSeriesState()
	state.MinuteComplete = false
	if raw, ok := entry["minutes"]; ok && raw != nil {
		rows, ok := raw.([]any)
		if !ok {
			return seriesState{}, false
		}
		for _, row := range rows {
			values, ok := row.([]any)
			if !ok || len(values) < 9 {
				return seriesState{}, false
			}
			minute, ok := numericValue(values[0])
			if !ok {
				return seriesState{}, false
			}
			key := int64(minute) * 60
			bucket := &SeriesBucket{At: float64(key)}
			fields := []*int64{&bucket.Requests, &bucket.Errors, &bucket.PromptTokens,
				&bucket.CompletionTokens, &bucket.ReasoningTokens, &bucket.CachedTokens,
				&bucket.TotalTokens}
			for index, target := range fields {
				value, ok := numericValue(values[index+1])
				if !ok {
					return seriesState{}, false
				}
				*target = int64(value)
			}
			credit, ok := numericValue(values[8])
			if !ok {
				return seriesState{}, false
			}
			bucket.Credit = credit
			state.Minutes[key] = bucket
		}
		state.MinuteComplete = true
	} else if days, ok := entry["days"].(map[string]any); ok {
		for _, rawDay := range days {
			day, ok := rawDay.(map[string]any)
			if !ok {
				continue
			}
			hours, ok := day["hours"].(map[string]any)
			if !ok {
				continue
			}
			for _, rawHour := range hours {
				hour, ok := rawHour.(map[string]any)
				if !ok {
					continue
				}
				bucket, ok := seriesBucketFromMap(hour)
				if !ok {
					continue
				}
				state.Minutes[int64(bucket.At)] = bucket
			}
		}
	}
	credits, _ := entry["credits"].([]any)
	if len(state.Minutes) == 0 && len(credits) == 0 {
		// No usable series data in the entry.
		return seriesState{}, false
	}
	if credits != nil {
		encoded, err := json.Marshal(credits)
		if err != nil || json.Unmarshal(encoded, &state.Credits) != nil {
			return seriesState{}, false
		}
	}
	if maxAt, ok := numericValue(entry["max_at"]); ok {
		state.MaxAt = maxAt
	}
	return state, true
}

func seriesBucketFromMap(raw map[string]any) (*SeriesBucket, bool) {
	at, ok := numericValue(raw["at"])
	if !ok {
		return nil, false
	}
	bucket := &SeriesBucket{At: at}
	for field, target := range map[string]*int64{
		"requests": &bucket.Requests, "errors": &bucket.Errors,
		"prompt_tokens": &bucket.PromptTokens, "completion_tokens": &bucket.CompletionTokens,
		"reasoning_tokens": &bucket.ReasoningTokens, "cached_tokens": &bucket.CachedTokens,
		"total_tokens": &bucket.TotalTokens,
	} {
		value, ok := numericValue(raw[field])
		if !ok {
			return nil, false
		}
		*target = int64(value)
	}
	credit, ok := numericValue(raw["credit"])
	if !ok {
		return nil, false
	}
	bucket.Credit = credit
	return bucket, true
}

// seriesSlice turns minute buckets into the requested grid. It reports false
// when the window/bucket does not align, so the caller falls back to a raw
// scan with the exact v1 origin rule.
func seriesSlice(state seriesState, lo, hi time.Time, step int) ([]SeriesBucket, []SeriesCredit, bool) {
	// Minute buckets cannot apply a sub-minute upper bound; the raw scan is
	// the only path that can keep `at <= hi` exact.
	if hi.Second() != 0 || hi.Nanosecond() != 0 {
		return nil, nil, false
	}
	if step == 60 {
		if !state.MinuteComplete || lo.Second() != 0 || lo.Nanosecond() != 0 {
			return nil, nil, false
		}
	} else if step == 3600 {
		if lo.Unix() != localHourStart(lo).Unix() {
			return nil, nil, false
		}
	} else if step == 86400 {
		if lo.Unix() != localMidnight(lo).Unix() {
			return nil, nil, false
		}
	} else {
		return nil, nil, false
	}
	loUnix := lo.Unix()
	hiUnix := hi.Unix()
	grouped := map[int64]*SeriesBucket{}
	for minute, bucket := range state.Minutes {
		if minute < loUnix || minute > hiUnix {
			continue
		}
		var key int64
		switch step {
		case 60:
			key = minute
		case 3600:
			key = localHourStart(time.Unix(minute, 0)).Unix()
		default:
			key = localMidnight(time.Unix(minute, 0)).Unix()
		}
		target := grouped[key]
		if target == nil {
			target = &SeriesBucket{At: float64(key)}
			grouped[key] = target
		}
		target.Requests += bucket.Requests
		target.Errors += bucket.Errors
		target.PromptTokens += bucket.PromptTokens
		target.CompletionTokens += bucket.CompletionTokens
		target.ReasoningTokens += bucket.ReasoningTokens
		target.CachedTokens += bucket.CachedTokens
		target.TotalTokens += bucket.TotalTokens
		target.Credit += bucket.Credit
	}
	keys := make([]int64, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	buckets := make([]SeriesBucket, 0, len(keys))
	for _, key := range keys {
		buckets = append(buckets, *grouped[key])
	}
	credits := make([]SeriesCredit, 0)
	for _, credit := range state.Credits {
		if credit.At >= float64(loUnix) && credit.At <= float64(hiUnix) {
			credits = append(credits, credit)
		}
	}
	credits = topCredits(credits)
	return buckets, credits, true
}

func (s *Service) seriesScan(ctx context.Context, realm string, lo, hi time.Time, step int) ([]SeriesBucket, []SeriesCredit, error) {
	var origin int64
	switch step {
	case 86400:
		origin = localMidnight(lo).Unix()
	case 3600:
		origin = localHourStart(lo).Unix()
	case 60:
		origin = (lo.Unix() / 60) * 60
	default:
		origin = lo.Unix()
	}
	buckets := map[int64]*SeriesBucket{}
	credits := make([]SeriesCredit, 0)
	_, err := s.scanAll(ctx, func(row Row) error {
		if !s.matchesRealm(row, realm) {
			return nil
		}
		at := number(row, "at")
		if at < float64(lo.Unix()) || at > float64(hi.Unix()) {
			return nil
		}
		key := origin + int64(math.Floor((at-float64(origin))/float64(step)))*int64(step)
		bucket := buckets[key]
		if bucket == nil {
			bucket = &SeriesBucket{At: float64(key)}
			buckets[key] = bucket
		}
		outcome := RowOutcome(row)
		if outcome == "completed" {
			bucket.Requests++
			bucket.PromptTokens += intNumber(row, "prompt_tokens")
			bucket.CompletionTokens += intNumber(row, "completion_tokens")
			bucket.ReasoningTokens += intNumber(row, "reasoning_tokens")
			bucket.CachedTokens += intNumber(row, "cached_tokens")
			bucket.TotalTokens += intNumber(row, "total_tokens")
		} else {
			bucket.Errors++
		}
		credit := number(row, "credit")
		if outcome != "client_aborted" {
			bucket.Credit += credit
		}
		if credit > 0 {
			credits = append(credits, SeriesCredit{
				At: at, ISO: rowString(row, "iso"), Model: rowString(row, "model"),
				Account: rowString(row, "account"), Credit: credit,
				TotalTokens: intNumber(row, "total_tokens"),
			})
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	keys := make([]int64, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]SeriesBucket, 0, len(keys))
	for _, key := range keys {
		out = append(out, *buckets[key])
	}
	return out, topCredits(credits), nil
}

func topCredits(credits []SeriesCredit) []SeriesCredit {
	sort.SliceStable(credits, func(i, j int) bool { return credits[i].At > credits[j].At })
	if len(credits) > 50 {
		credits = credits[:50]
	}
	if credits == nil {
		credits = []SeriesCredit{}
	}
	return credits
}

// SeriesETagKeyResolved identifies a timeseries query by its resolved bucket
// step, so aliases such as bucket=30 and bucket=60 share one validator.
func SeriesETagKeyResolved(realm string, window Range, step int) string {
	return strings.Join([]string{"series", scopeRealm(realm), boundKey(window.Since), boundKey(window.Until), fmt.Sprint(step)}, "\x1f")
}
