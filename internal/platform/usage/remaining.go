package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LimitWindowSeconds is the upstream free-quota window.
const LimitWindowSeconds = 24 * 3600

const (
	estimateSchema       = 1
	estimateKeepDays     = 3
	estimateSegmentDays  = 2
	remainingKeepSeconds = 26 * 3600
	remainingMaxEvents   = 1000
)

var estimateWeights = []int{6, 3, 1}

type limitEvent struct {
	At      float64 `json:"at"`
	Reset   float64 `json:"reset"`
	Account string  `json:"account"`
	Model   string  `json:"model"`
	Realm   string  `json:"realm"`
	Sample  int64   `json:"sample"`
	Src     string  `json:"src"`
}

type usageBuffer struct {
	At     []float64 `json:"at"`
	Tokens []int64   `json:"tokens"`
}

type remainingState struct {
	Events []limitEvent            `json:"events"`
	Usage  map[string]*usageBuffer `json:"usage"`
	Realms map[string]string       `json:"realms,omitempty"`
}

func newRemainingState() *remainingState {
	return &remainingState{Usage: map[string]*usageBuffer{}, Realms: map[string]string{}}
}

func pairKey(uid, model string) string { return uid + "\x1f" + model }

func (s *remainingState) buffer(uid, model string) *usageBuffer {
	key := pairKey(uid, model)
	buffer := s.Usage[key]
	if buffer == nil {
		buffer = &usageBuffer{}
		s.Usage[key] = buffer
	}
	return buffer
}

func (s *remainingState) addEvent(event limitEvent) {
	if event.Account == "" || event.Model == "" || event.Reset <= 0 {
		return
	}
	for index := range s.Events {
		existing := &s.Events[index]
		if existing.Account == event.Account && existing.Model == event.Model && existing.Reset == event.Reset {
			// The journal's sample is computed at cap time; a log-derived
			// duplicate may have seen fewer rows. Keep the better observation.
			if event.Sample > existing.Sample {
				existing.Sample = event.Sample
			}
			if event.At > existing.At {
				existing.At = event.At
			}
			if existing.Realm == "" {
				existing.Realm = event.Realm
			}
			return
		}
	}
	s.Events = append(s.Events, event)
	if len(s.Events) > remainingMaxEvents {
		s.Events = s.Events[len(s.Events)-remainingMaxEvents:]
	}
}

func (s *remainingState) anchors(uid, model string) map[float64]float64 {
	anchors := map[float64]float64{}
	for _, event := range s.Events {
		if event.Account != uid || event.Model != model {
			continue
		}
		if old, ok := anchors[event.Reset]; !ok || event.At > old {
			anchors[event.Reset] = event.At
		}
	}
	return anchors
}

func windowStart(anchors map[float64]float64, reset float64) float64 {
	previous := 0.0
	for other := range anchors {
		if other < reset && other > previous {
			previous = other
		}
	}
	start := reset - LimitWindowSeconds
	if previous > start {
		return previous
	}
	return start
}

func appendSample(buffer *usageBuffer, at float64, tokens int64) {
	buffer.At = append(buffer.At, at)
	buffer.Tokens = append(buffer.Tokens, tokens)
}

func rangeSum(buffer *usageBuffer, lo, hi float64) int64 {
	if buffer == nil || len(buffer.At) == 0 {
		return 0
	}
	var total int64
	for index, at := range buffer.At {
		if at >= lo && at <= hi {
			total += buffer.Tokens[index]
		}
	}
	return total
}

func (s *remainingState) windowSample(uid, model string, reset, at, start float64) int64 {
	buffer := s.Usage[pairKey(uid, model)]
	return rangeSum(buffer, start, at)
}

func (s *remainingState) foldUsageRow(row Row, now float64) {
	uid := rowString(row, "account")
	model := rowString(row, "model")
	if uid == "" || model == "" {
		return
	}
	at := number(row, "at")
	if at <= 0 {
		return
	}
	if intNumber(row, "status") == 429 {
		reset := parseRateLimitReset(rowString(row, "message"))
		if reset == nil {
			return
		}
		anchors := s.anchors(uid, model)
		start := windowStart(anchors, *reset)
		s.addEvent(limitEvent{
			At: at, Reset: *reset, Account: uid, Model: model,
			Realm:  rowString(row, "realm"),
			Sample: s.windowSample(uid, model, *reset, at, start), Src: "log",
		})
		return
	}
	if truthy(row["error"]) || truthy(row["usage_missing"]) || RowOutcome(row) == "client_aborted" {
		return
	}
	if realm := rowString(row, "realm"); realm != "" {
		if s.Realms == nil {
			s.Realms = map[string]string{}
		}
		s.Realms[uid] = realm
	}
	buffer := s.buffer(uid, model)
	appendSample(buffer, at, intNumber(row, "total_tokens"))
	cutoff := at - remainingKeepSeconds
	index := 0
	for index < len(buffer.At) && buffer.At[index] < cutoff {
		index++
	}
	if index > 0 {
		buffer.At = append([]float64(nil), buffer.At[index:]...)
		buffer.Tokens = append([]int64(nil), buffer.Tokens[index:]...)
	}
}

func (s *remainingState) mergeFileEvents(buffer []limitEvent, usage map[string]*usageBuffer) {
	for _, event := range buffer {
		if event.Sample <= 0 {
			start := windowStart(s.anchors(event.Account, event.Model), event.Reset)
			var source *usageBuffer
			if usage != nil {
				source = usage[pairKey(event.Account, event.Model)]
			}
			if source == nil {
				source = s.Usage[pairKey(event.Account, event.Model)]
			}
			event.Sample = rangeSum(source, start, event.At)
		}
		event.Src = "file"
		s.addEvent(event)
	}
}

func (s *remainingState) trim() {
	for key, buffer := range s.Usage {
		if len(buffer.At) == 0 {
			delete(s.Usage, key)
			continue
		}
		cutoff := buffer.At[len(buffer.At)-1] - remainingKeepSeconds
		index := 0
		for index < len(buffer.At) && buffer.At[index] < cutoff {
			index++
		}
		if index > 0 {
			buffer.At = append([]float64(nil), buffer.At[index:]...)
			buffer.Tokens = append([]int64(nil), buffer.Tokens[index:]...)
		}
	}
}

// Remaining returns the v1 /usage/remaining estimate.
func (s *Service) Remaining(ctx context.Context) (map[string]any, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("usage: nil service")
	}
	now := s.now()
	s.remainingMu.Lock()
	if s.remainingData != nil && s.remainingTTL() > 0 && now.Sub(s.remainingAt) < s.remainingTTL() {
		data := s.remainingData
		s.remainingMu.Unlock()
		return data, nil
	}
	s.remainingMu.Unlock()

	state, logOffset, fileOffset, err := s.remainingState(ctx)
	if err != nil {
		return nil, err
	}
	payload := s.remainingPayload(state, now)
	archived := s.archiveRemaining(state, now)
	if archived != nil {
		s.syncEstimateDaily(archived, now)
	}
	s.saveRemaining(state, logOffset, fileOffset)
	_ = ctx

	s.remainingMu.Lock()
	s.remainingData = payload
	s.remainingAt = now
	s.remainingMu.Unlock()
	s.resultPutQuery("remaining", "all", RemainingETagKey(), payload, s.remainingTTL())
	return payload, nil
}

func (s *Service) remainingState(ctx context.Context) (*remainingState, int64, int64, error) {
	if state, logOffset, fileOffset, ok := s.adoptRemaining(ctx); ok {
		return state, logOffset, fileOffset, nil
	}
	state := newRemainingState()
	logOffset, err := s.scan(ctx, s.Store.Paths.Log, 0, func(row Row) error {
		state.foldUsageRow(row, float64(s.now().Unix()))
		return nil
	})
	if err != nil {
		return nil, 0, 0, err
	}
	fileOffset, err := s.scanLimitEvents(state)
	if err != nil {
		return nil, 0, 0, err
	}
	state.trim()
	return state, logOffset, fileOffset, nil
}

func (s *Service) scanLimitEvents(state *remainingState) (int64, error) {
	path := s.Store.Paths.LimitEvents
	events := make([]limitEvent, 0)
	offset, err := s.scan(context.Background(), path, 0, func(row Row) error {
		at := number(row, "at")
		reset := number(row, "reset")
		account := rowString(row, "uid")
		if account == "" {
			account = rowString(row, "account")
		}
		model := rowString(row, "model")
		if account == "" || model == "" || reset <= 0 {
			return nil
		}
		events = append(events, limitEvent{
			At: at, Reset: reset, Account: account, Model: model,
			Realm: rowString(row, "realm"), Sample: intNumber(row, "sample"),
		})
		return nil
	})
	if err != nil {
		return 0, nil
	}
	state.mergeFileEvents(events, nil)
	return offset, nil
}

func (s *Service) adoptRemaining(ctx context.Context) (*remainingState, int64, int64, bool) {
	if s.Cache == nil {
		return nil, 0, 0, false
	}
	for _, entry := range s.Cache.Entries("remaining") {
		logOffset, ok := ValidateCachePosition(entry, s.Store.Paths.Log)
		if !ok {
			continue
		}
		raw, ok := entry["data"].(map[string]any)
		var state remainingState
		if ok {
			encoded, err := json.Marshal(raw)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(encoded, &state); err != nil {
				continue
			}
		} else {
			converted, valid := remainingStateFromV1(entry)
			if !valid {
				continue
			}
			state = converted
		}
		if state.Usage == nil {
			state.Usage = map[string]*usageBuffer{}
		}
		if state.Realms == nil {
			state.Realms = map[string]string{}
		}
		fileOffset := int64(0)
		if fileEntry, ok := entry["file"].(map[string]any); ok {
			if offset, valid := ValidateCachePosition(fileEntry, s.Store.Paths.LimitEvents); valid {
				fileOffset = offset
			}
		}
		var fileRows []limitEvent
		newFile, fileErr := s.scan(ctx, s.Store.Paths.LimitEvents, fileOffset, func(row Row) error {
			if event := limitEventFromRow(row); event != nil {
				fileRows = append(fileRows, *event)
			}
			return nil
		})
		if fileErr == nil {
			fileOffset = newFile
			state.mergeFileEvents(fileRows, state.Usage)
		}
		newLog, err := s.scan(ctx, s.Store.Paths.Log, logOffset, func(row Row) error {
			state.foldUsageRow(row, float64(s.now().Unix()))
			return nil
		})
		if err != nil {
			return nil, 0, 0, false
		}
		state.trim()
		return &state, newLog, fileOffset, true
	}
	return nil, 0, 0, false
}

// remainingStateFromV1 converts the v1 checkpoint shape: events plus
// per-(uid,model) prefix-sum buffers ({at, cum, head, base}). The prefix sums
// are expanded back into individual samples; a decreasing prefix is corrupt
// and rejects the entry (cold rescan).
func remainingStateFromV1(entry map[string]any) (remainingState, bool) {
	state := remainingState{Usage: map[string]*usageBuffer{}, Realms: map[string]string{}}
	if rawEvents, ok := entry["events"].([]any); ok {
		encoded, err := json.Marshal(rawEvents)
		if err != nil || json.Unmarshal(encoded, &state.Events) != nil {
			return remainingState{}, false
		}
	}
	rawUsage, ok := entry["usage"].(map[string]any)
	if !ok {
		return state, true
	}
	for key, raw := range rawUsage {
		pair, ok := raw.(map[string]any)
		if !ok {
			return remainingState{}, false
		}
		ats, okAts := pair["at"].([]any)
		cums, okCums := pair["cum"].([]any)
		if !okAts || !okCums || len(ats) != len(cums) {
			return remainingState{}, false
		}
		head, _ := cacheInt(pair["head"])
		if head < 0 || head > int64(len(ats)) {
			return remainingState{}, false
		}
		base, _ := cacheInt(pair["base"])
		previous := float64(base)
		if head > 0 {
			value, ok := numericValue(cums[head-1])
			if !ok {
				return remainingState{}, false
			}
			previous = value
		}
		buffer := &usageBuffer{}
		for index := int(head); index < len(ats); index++ {
			at, ok := numericValue(ats[index])
			if !ok {
				return remainingState{}, false
			}
			cum, ok := numericValue(cums[index])
			if !ok || cum < previous {
				return remainingState{}, false
			}
			appendSample(buffer, at, int64(cum-previous))
			previous = cum
		}
		state.Usage[key] = buffer
	}
	return state, true
}

func limitEventFromRow(row Row) *limitEvent {
	account := rowString(row, "uid")
	if account == "" {
		account = rowString(row, "account")
	}
	model := rowString(row, "model")
	reset := number(row, "reset")
	if account == "" || model == "" || reset <= 0 {
		return nil
	}
	return &limitEvent{
		At: number(row, "at"), Reset: reset, Account: account, Model: model,
		Realm: rowString(row, "realm"), Sample: intNumber(row, "sample"),
	}
}

func (s *Service) saveRemaining(state *remainingState, logOffset, fileOffset int64) {
	if s.Cache == nil || logOffset <= 0 {
		return
	}
	position, ok := CachePosition(s.Store.Paths.Log, logOffset)
	if !ok {
		return
	}
	entry := map[string]any{"data": state}
	for key, value := range position {
		entry[key] = value
	}
	if fileOffset > 0 {
		if filePosition, ok := CachePosition(s.Store.Paths.LimitEvents, fileOffset); ok {
			entry["file"] = filePosition
		}
	}
	_ = s.Cache.Save("remaining", entry)
}

func (s *Service) remainingPayload(state *remainingState, now time.Time) map[string]any {
	nowEpoch := float64(now.Unix())
	accounts := s.remainingAccounts(state)
	realmOf := map[string]string{}
	for _, account := range accounts {
		realmOf[account.UID] = account.Realm
	}
	budgets := s.remainingBudgets(state, realmOf, now)
	floor := activityFloor(nowEpoch)
	active := map[string]bool{}
	for key, buffer := range state.Usage {
		if len(buffer.At) == 0 {
			continue
		}
		uid, _, _ := strings.Cut(key, "\x1f")
		realm := realmOf[uid]
		if buffer.At[len(buffer.At)-1] >= floor {
			active[realm+"\x1f"+strings.SplitN(key, "\x1f", 2)[1]] = true
		}
	}
	for _, event := range state.Events {
		realm := event.Realm
		if realm == "" {
			realm = realmOf[event.Account]
		}
		if math.Max(event.At, event.Reset) >= floor {
			active[realm+"\x1f"+event.Model] = true
		}
	}
	for _, account := range accounts {
		for model, cap := range account.ModelCaps {
			if math.Max(cap.At, cap.Reset) >= floor {
				active[account.Realm+"\x1f"+model] = true
			}
		}
	}

	rows := make([]map[string]any, 0)
	totals := map[string]map[string]any{}
	for _, account := range accounts {
		uid := account.UID
		realm := account.Realm
		models := map[string]bool{}
		for key := range budgets {
			parts := strings.SplitN(key, "\x1f", 2)
			if len(parts) == 2 && parts[0] == realm && active[key] {
				models[parts[1]] = true
			}
		}
		for key := range state.Usage {
			parts := strings.SplitN(key, "\x1f", 2)
			if len(parts) == 2 && parts[0] == uid && active[realm+"\x1f"+parts[1]] {
				models[parts[1]] = true
			}
		}
		// Credential model_caps and limit events are cap anchors too. A pair
		// cooling only because its credential remembers the reset must remain
		// visible even when the journal row and usage buffer were pruned.
		for model := range account.ModelCaps {
			if active[realm+"\x1f"+model] {
				models[model] = true
			}
		}
		for _, event := range state.Events {
			if event.Account == uid && active[realm+"\x1f"+event.Model] {
				models[event.Model] = true
			}
		}
		names := make([]string, 0, len(models))
		for model := range models {
			names = append(names, model)
		}
		sort.Strings(names)
		for _, model := range names {
			anchors := state.anchors(uid, model)
			applyCaps(anchors, account, model)
			buffer := state.Usage[pairKey(uid, model)]
			var windowStartValue float64
			var used int64
			var resetAt *float64
			cooling := false
			estimated := false
			segments := make([]map[string]any, 0)
			future := futureReset(anchors, nowEpoch)
			past := pastReset(anchors, nowEpoch)
			if future != nil {
				reset := *future
				start := windowStart(anchors, reset)
				eventAt := anchorAt(anchors, reset)
				if eventAt <= 0 {
					eventAt = reset
				}
				end := math.Min(nowEpoch, eventAt)
				used = rangeSum(buffer, start, end)
				windowStartValue = start
				resetAt = &reset
				cooling = true
				segments = append(segments, map[string]any{
					"start": start, "end": end, "used": used,
					"capped": true, "current": true,
					"start_iso": isoAt(start), "end_iso": isoAt(end),
				})
			} else if past != nil {
				reset := *past
				start := math.Max(reset, nowEpoch-LimitWindowSeconds)
				used = rangeSum(buffer, start, nowEpoch)
				windowStartValue = start
				resetAt = &reset
				estimated = reset < nowEpoch-LimitWindowSeconds
				if !estimated {
					if at := anchorAt(anchors, reset); at > 0 {
						previousEnd := math.Min(at, reset)
						previousStart := windowStart(anchors, reset)
						segments = append(segments, map[string]any{
							"start": previousStart, "end": previousEnd,
							"used":   rangeSum(buffer, previousStart, previousEnd),
							"capped": true, "current": false,
							"start_iso": isoAt(previousStart), "end_iso": isoAt(previousEnd),
						})
					}
					segments = append(segments, map[string]any{
						"start": reset, "end": nowEpoch, "used": used,
						"capped": false, "current": true,
						"start_iso": isoAt(reset), "end_iso": isoAt(nowEpoch),
					})
				}
			} else {
				start := nowEpoch - LimitWindowSeconds
				used = rangeSum(buffer, start, nowEpoch)
				windowStartValue = start
				estimated = true
			}
			budgetKey := realm + "\x1f" + model
			budget, hasBudget := budgets[budgetKey]
			var remaining any
			var usedPct any
			if cooling {
				remaining = int64(0)
				if hasBudget && budget.Avg > 0 {
					usedPct = round1(float64(used) * 100 / float64(budget.Avg))
				}
			} else if hasBudget {
				value := budget.Avg - used
				if value < 0 {
					value = 0
				}
				remaining = value
				if budget.Avg > 0 {
					usedPct = round1(float64(used) * 100 / float64(budget.Avg))
				}
			} else {
				remaining = nil
			}
			var budgetAvg any
			var budgetMin any
			var budgetMax any
			var samples int64
			if hasBudget {
				budgetAvg = budget.Avg
				budgetMin = budget.Min
				budgetMax = budget.Max
				samples = budget.N
			}
			row := map[string]any{
				"uid": uid, "nickname": account.Nickname, "realm": realm, "model": model,
				"used": used, "budget": budgetAvg, "budget_min": budgetMin,
				"budget_max": budgetMax, "samples": samples,
				"remaining": remaining, "used_pct": usedPct,
				"window_start": windowStartValue, "window_iso": isoAt(windowStartValue),
				"reset_at": resetAt, "reset_iso": nil,
				"cooling": cooling, "estimated": estimated, "segments": segments,
			}
			if resetAt != nil {
				row["reset_iso"] = isoAt(*resetAt)
			}
			rows = append(rows, row)
			total := totals[budgetKey]
			if total == nil {
				total = map[string]any{
					"realm": realm, "model": model, "total_remaining": nil,
					"budget": budgetAvg, "pairs": int64(0), "available": int64(0),
					"cooling": int64(0), "unknown": int64(0), "estimated": int64(0),
				}
				totals[budgetKey] = total
			}
			total["pairs"] = total["pairs"].(int64) + 1
			if cooling {
				total["cooling"] = total["cooling"].(int64) + 1
			}
			if estimated {
				total["estimated"] = total["estimated"].(int64) + 1
			}
			if remaining == nil {
				total["unknown"] = total["unknown"].(int64) + 1
			} else {
				value := remaining.(int64)
				if total["total_remaining"] == nil {
					total["total_remaining"] = int64(0)
				}
				total["total_remaining"] = total["total_remaining"].(int64) + value
				if !cooling {
					total["available"] = total["available"].(int64) + 1
				}
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		leftNil := rows[i]["remaining"] == nil
		rightNil := rows[j]["remaining"] == nil
		if leftNil != rightNil {
			return !leftNil
		}
		if !leftNil && rows[i]["remaining"].(int64) != rows[j]["remaining"].(int64) {
			return rows[i]["remaining"].(int64) < rows[j]["remaining"].(int64)
		}
		if rows[i]["realm"].(string) != rows[j]["realm"].(string) {
			return rows[i]["realm"].(string) < rows[j]["realm"].(string)
		}
		if rows[i]["nickname"].(string) != rows[j]["nickname"].(string) {
			return rows[i]["nickname"].(string) < rows[j]["nickname"].(string)
		}
		return rows[i]["model"].(string) < rows[j]["model"].(string)
	})
	modelRows := make([]map[string]any, 0, len(totals))
	for _, total := range totals {
		modelRows = append(modelRows, total)
	}
	sort.SliceStable(modelRows, func(i, j int) bool {
		leftNil := modelRows[i]["total_remaining"] == nil
		rightNil := modelRows[j]["total_remaining"] == nil
		if leftNil != rightNil {
			return !leftNil
		}
		if !leftNil && modelRows[i]["total_remaining"].(int64) != modelRows[j]["total_remaining"].(int64) {
			return modelRows[i]["total_remaining"].(int64) > modelRows[j]["total_remaining"].(int64)
		}
		if modelRows[i]["realm"].(string) != modelRows[j]["realm"].(string) {
			return modelRows[i]["realm"].(string) < modelRows[j]["realm"].(string)
		}
		return modelRows[i]["model"].(string) < modelRows[j]["model"].(string)
	})

	budgetRows := make([]map[string]any, 0, len(budgets))
	for key, budget := range budgets {
		parts := strings.SplitN(key, "\x1f", 2)
		realm := ""
		model := key
		if len(parts) == 2 {
			realm, model = parts[0], parts[1]
		}
		samples := remainingSampleRows(state.Events, realm, model)
		budgetRows = append(budgetRows, map[string]any{
			"realm": realm, "model": model, "avg": budget.Avg,
			"min": budget.Min, "max": budget.Max, "n": budget.N,
			"days": budget.Days, "active": active[key],
			"samples_list": samples,
		})
	}
	sort.SliceStable(budgetRows, func(i, j int) bool {
		if budgetRows[i]["realm"].(string) != budgetRows[j]["realm"].(string) {
			return budgetRows[i]["realm"].(string) < budgetRows[j]["realm"].(string)
		}
		return budgetRows[i]["model"].(string) < budgetRows[j]["model"].(string)
	})
	return map[string]any{
		"window_seconds":     LimitWindowSeconds,
		"generated_at":       nowEpoch,
		"generated_iso":      isoAt(nowEpoch),
		"activity_floor":     floor,
		"activity_floor_iso": isoAt(floor),
		"budgets":            budgetRows,
		"models":             modelRows,
		"rows":               rows,
	}
}

func applyCaps(anchors map[float64]float64, account RemainingAccount, model string) {
	cap, ok := account.ModelCaps[model]
	if !ok || cap.Reset <= 0 {
		return
	}
	if old, exists := anchors[cap.Reset]; !exists || cap.At > old {
		anchors[cap.Reset] = cap.At
	}
}

func futureReset(anchors map[float64]float64, now float64) *float64 {
	var best *float64
	for reset := range anchors {
		if reset <= now {
			continue
		}
		if best == nil || reset < *best {
			value := reset
			best = &value
		}
	}
	return best
}

func pastReset(anchors map[float64]float64, now float64) *float64 {
	var best *float64
	for reset := range anchors {
		if reset > now {
			continue
		}
		if best == nil || reset > *best {
			value := reset
			best = &value
		}
	}
	return best
}

func anchorAt(anchors map[float64]float64, reset float64) float64 { return anchors[reset] }

func activityFloor(now float64) float64 {
	when := time.Unix(int64(now), 0).Local()
	midnight := time.Date(when.Year(), when.Month(), when.Day(), 0, 0, 0, 0, time.Local).Unix()
	return float64(midnight - LimitWindowSeconds)
}

type remainingBudget struct {
	Avg  int64
	Min  int64
	Max  int64
	N    int64
	Days []map[string]any
}

func (s *Service) remainingAccounts(state *remainingState) []RemainingAccount {
	if s.RemainingAccounts != nil {
		out := make([]RemainingAccount, 0)
		for _, account := range s.RemainingAccounts {
			if account.Enabled && account.UID != "" {
				out = append(out, account)
			}
		}
		return out
	}
	seen := map[string]RemainingAccount{}
	for key := range state.Usage {
		uid, _, _ := strings.Cut(key, "\x1f")
		if uid == "" {
			continue
		}
		seen[uid] = RemainingAccount{UID: uid, Nickname: uid, Realm: state.Realms[uid], Enabled: true}
	}
	for _, event := range state.Events {
		account := seen[event.Account]
		account.UID = event.Account
		account.Nickname = event.Account
		account.Enabled = true
		if account.Realm == "" {
			account.Realm = event.Realm
		}
		seen[event.Account] = account
	}
	out := make([]RemainingAccount, 0, len(seen))
	for _, account := range seen {
		out = append(out, account)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

func (s *Service) remainingBudgets(state *remainingState, realmOf map[string]string, now time.Time) map[string]remainingBudget {
	days := estimateDayKeys(now)
	today := days[0]
	type perAccount map[string][]int64
	type perDay map[string]map[string]perAccount
	samples := perDay{}
	archivedN := map[string]map[string]int64{}
	archivedMin := map[string]map[string]int64{}
	archivedMax := map[string]map[string]int64{}
	for _, event := range state.Events {
		if event.Sample <= 0 || localDayKey(event.At) != today {
			continue
		}
		realm := event.Realm
		if realm == "" {
			realm = realmOf[event.Account]
		}
		key := realm + "\x1f" + event.Model
		if samples[key] == nil {
			samples[key] = map[string]perAccount{}
		}
		if samples[key][today] == nil {
			samples[key][today] = perAccount{}
		}
		samples[key][today][event.Account] = append(samples[key][today][event.Account], event.Sample)
	}
	daily := readEstimateDaily(s.Store.Paths.EstimateDaily)
	for key := range samples {
		for _, day := range days[1:] {
			record := daily[day]
			if record == nil {
				continue
			}
			budgets, _ := record["budgets"].(map[string]any)
			entry, _ := budgets[key].(map[string]any)
			if !budgetEntryOK(entry) {
				continue
			}
			if samples[key] == nil {
				samples[key] = map[string]perAccount{}
			}
			samples[key][day] = perAccount{"": []int64{int64(numberValueAny(entry["avg"]))}}
			if archivedN[key] == nil {
				archivedN[key] = map[string]int64{}
				archivedMin[key] = map[string]int64{}
				archivedMax[key] = map[string]int64{}
			}
			archivedN[key][day] = int64(numberValueAny(entry["n"]))
			archivedMin[key][day] = int64(numberValueAny(entry["min"]))
			archivedMax[key][day] = int64(numberValueAny(entry["max"]))
		}
	}
	// Also import archived pairs that have no event in the current in-memory
	// window at all (for example after a restart).
	for _, day := range days[1:] {
		record := daily[day]
		if record == nil {
			continue
		}
		if budgets, ok := record["budgets"].(map[string]any); ok {
			for key, raw := range budgets {
				entry, _ := raw.(map[string]any)
				if !budgetEntryOK(entry) {
					continue
				}
				if samples[key] == nil {
					samples[key] = map[string]perAccount{}
				}
				if samples[key][day] == nil {
					samples[key][day] = perAccount{"": []int64{int64(numberValueAny(entry["avg"]))}}
				}
				if archivedN[key] == nil {
					archivedN[key] = map[string]int64{}
					archivedMin[key] = map[string]int64{}
					archivedMax[key] = map[string]int64{}
				}
				archivedN[key][day] = int64(numberValueAny(entry["n"]))
				archivedMin[key][day] = int64(numberValueAny(entry["min"]))
				archivedMax[key][day] = int64(numberValueAny(entry["max"]))
			}
		}
	}

	out := map[string]remainingBudget{}
	for key, byDay := range samples {
		rows := make([]map[string]any, 0)
		var weighted, weight, minValue, maxValue, totalN int64
		hasValue := false
		for index, day := range days {
			accounts := byDay[day]
			if len(accounts) == 0 {
				continue
			}
			means := make([]int64, 0, len(accounts))
			for _, values := range accounts {
				if len(values) == 0 {
					continue
				}
				if day != today {
					means = append(means, values[0])
					continue
				}
				var sum int64
				for _, value := range values {
					sum += value
				}
				means = append(means, int64(math.Round(float64(sum)/float64(len(values)))))
			}
			if len(means) == 0 {
				continue
			}
			var sum int64
			minCurrent := means[0]
			maxCurrent := means[0]
			for _, value := range means {
				sum += value
				if value < minCurrent {
					minCurrent = value
				}
				if value > maxCurrent {
					maxCurrent = value
				}
			}
			avg := int64(math.Round(float64(sum) / float64(len(means))))
			dayN := int64(len(means))
			if day != today {
				avg = int64(numberValueAny(perAccountValue(byDay[day], 0)))
				if archivedMin[key] != nil && archivedMin[key][day] != 0 {
					minCurrent = archivedMin[key][day]
				}
				if archivedMax[key] != nil && archivedMax[key][day] != 0 {
					maxCurrent = archivedMax[key][day]
				}
				if archivedN[key] != nil && archivedN[key][day] > 0 {
					dayN = archivedN[key][day]
				}
			}
			w := int64(estimateWeights[index])
			weighted += w * avg
			weight += w
			if !hasValue || minCurrent < minValue {
				minValue = minCurrent
			}
			if !hasValue || maxCurrent > maxValue {
				maxValue = maxCurrent
			}
			hasValue = true
			totalN += dayN
			rows = append(rows, map[string]any{"day": day, "w": w, "avg": avg, "n": dayN})
		}
		if weight == 0 {
			continue
		}
		out[key] = remainingBudget{
			Avg: int64(math.Round(float64(weighted) / float64(weight))),
			Min: minValue, Max: maxValue, N: totalN, Days: rows,
		}
	}
	return out
}

func perAccountValue(values map[string][]int64, fallback int64) int64 {
	for _, samples := range values {
		if len(samples) > 0 {
			return samples[0]
		}
	}
	return fallback
}

func remainingSampleRows(events []limitEvent, realm, model string) []map[string]any {
	rows := make([]map[string]any, 0)
	for _, event := range events {
		eventRealm := event.Realm
		if eventRealm != realm || event.Model != model {
			continue
		}
		rows = append(rows, map[string]any{
			"account": event.Account, "at": event.At, "at_iso": isoAt(event.At),
			"reset": event.Reset, "reset_iso": isoAt(event.Reset), "sample": event.Sample,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left := rows[i]["at"].(float64)
		right := rows[j]["at"].(float64)
		if left != right {
			return left < right
		}
		return rows[i]["account"].(string) < rows[j]["account"].(string)
	})
	return rows
}

func estimateDayKeys(now time.Time) []string {
	keys := make([]string, 0, estimateKeepDays)
	for offset := 0; offset < estimateKeepDays; offset++ {
		keys = append(keys, localDayKey(float64(localMidnight(now).AddDate(0, 0, -offset).Unix())))
	}
	return keys
}

func localDayKey(epoch float64) string {
	return time.Unix(int64(epoch), 0).Local().Format("2006-01-02")
}

func dayKeyStart(key string) (float64, bool) {
	when, err := time.ParseInLocation("2006-01-02", key, time.Local)
	if err != nil {
		return 0, false
	}
	return float64(when.Unix()), true
}

func readEstimateDaily(path string) map[string]map[string]any {
	out := map[string]map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var document struct {
		Schema int                       `json:"schema"`
		Days   map[string]map[string]any `json:"days"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil || document.Schema != estimateSchema || document.Days == nil {
		return out
	}
	for day, record := range document.Days {
		if _, ok := dayKeyStart(day); !ok || record == nil {
			continue
		}
		out[day] = record
	}
	return out
}

func writeEstimateDaily(path string, days map[string]map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload := map[string]any{"schema": estimateSchema, "days": days}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func budgetEntryOK(entry map[string]any) bool {
	if entry == nil || numberValueAny(entry["n"]) <= 0 {
		return false
	}
	for _, field := range []string{"avg", "min", "max"} {
		if _, ok := numericValue(entry[field]); !ok {
			return false
		}
	}
	return true
}

func numberValueAny(value any) float64 {
	number, _ := numericValue(value)
	return number
}

func (s *Service) archiveRemaining(state *remainingState, now time.Time) map[string]map[string]any {
	today := localDayKey(float64(now.Unix()))
	s.remainingMu.Lock()
	stale := s.remainingDay
	s.remainingDay = today
	s.remainingMu.Unlock()
	if stale == today {
		return nil
	}
	day := stale
	if day == "" || day >= today {
		day = localDayKey(float64(localMidnight(now).AddDate(0, 0, -1).Unix()))
	}
	accounts := s.remainingAccounts(state)
	realmOf := map[string]string{}
	for _, account := range accounts {
		realmOf[account.UID] = account.Realm
	}
	record := s.archiveRemainingDay(state, day, realmOf)
	if record == nil {
		return nil
	}
	return map[string]map[string]any{day: record}
}

func (s *Service) archiveRemainingDay(state *remainingState, day string, realmOf map[string]string) map[string]any {
	start, ok := dayKeyStart(day)
	if !ok {
		return nil
	}
	end := start + 86400
	events := make([]limitEvent, 0)
	for _, event := range state.Events {
		if localDayKey(event.At) == day {
			events = append(events, event)
		}
	}
	samples := map[string]map[string][]int64{}
	for _, event := range events {
		if event.Sample <= 0 {
			continue
		}
		realm := event.Realm
		if realm == "" {
			realm = realmOf[event.Account]
		}
		key := realm + "\x1f" + event.Model
		if samples[key] == nil {
			samples[key] = map[string][]int64{}
		}
		samples[key][event.Account] = append(samples[key][event.Account], event.Sample)
	}
	budgets := map[string]any{}
	for key, accounts := range samples {
		means := make([]int64, 0, len(accounts))
		for _, values := range accounts {
			var sum int64
			for _, value := range values {
				sum += value
			}
			means = append(means, int64(math.Round(float64(sum)/float64(len(values)))))
		}
		if len(means) == 0 {
			continue
		}
		var sum, minValue, maxValue int64
		minValue, maxValue = means[0], means[0]
		for _, value := range means {
			sum += value
			if value < minValue {
				minValue = value
			}
			if value > maxValue {
				maxValue = value
			}
		}
		budgets[key] = map[string]any{
			"avg": int64(math.Round(float64(sum) / float64(len(means)))),
			"min": minValue, "max": maxValue, "n": int64(len(means)),
		}
	}
	segments := map[string]any{}
	for key, buffer := range state.Usage {
		total := rangeSumOpen(buffer, start, end)
		if total <= 0 {
			continue
		}
		uid, model, _ := strings.Cut(key, "\x1f")
		var post int64
		for _, event := range events {
			if event.Account != uid || event.Model != model {
				continue
			}
			if event.Reset < start || event.Reset >= end {
				continue
			}
			next := end
			for _, other := range events {
				if other.Account == uid && other.Model == model && other.At > event.Reset && other.At < next {
					next = other.At
				}
			}
			post += rangeSumOpen(buffer, event.Reset, math.Min(next, end))
		}
		segments[key] = map[string]any{"pre": total - post, "post": post}
	}
	if len(budgets) == 0 && len(segments) == 0 {
		return nil
	}
	return map[string]any{"budgets": budgets, "segments": segments}
}

func rangeSumOpen(buffer *usageBuffer, lo, hi float64) int64 {
	if buffer == nil {
		return 0
	}
	var total int64
	for index, at := range buffer.At {
		if at >= lo && at < hi {
			total += buffer.Tokens[index]
		}
	}
	return total
}

func (s *Service) syncEstimateDaily(archived map[string]map[string]any, now time.Time) {
	if len(archived) == 0 {
		return
	}
	path := s.Store.Paths.EstimateDaily
	days := readEstimateDaily(path)
	changed := false
	for day, record := range archived {
		if _, exists := days[day]; !exists {
			days[day] = record
			changed = true
		}
	}
	keep := map[string]bool{}
	for _, day := range estimateDayKeys(now) {
		keep[day] = true
	}
	segmentDays := map[string]bool{}
	for _, day := range estimateDayKeys(now)[:estimateSegmentDays] {
		segmentDays[day] = true
	}
	pruned := map[string]map[string]any{}
	for day, record := range days {
		if !keep[day] {
			changed = true
			continue
		}
		if !segmentDays[day] {
			if _, ok := record["segments"]; ok {
				copy := map[string]any{}
				for key, value := range record {
					copy[key] = value
				}
				delete(copy, "segments")
				record = copy
				changed = true
			}
		}
		pruned[day] = record
	}
	if changed {
		_ = writeEstimateDaily(path, pruned)
	}
}

var resetPattern = regexp.MustCompile(`(?:reset at|将在)\s*(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`)
var resetFallbackPattern = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})\s*UTC[+-]\d{1,2}`)
var timezonePattern = regexp.MustCompile(`UTC([+-]\d{1,2})(?::?(\d{2}))?`)

// parseRateLimitReset pulls the reset wall clock out of a 429 body. Both the
// English and Chinese upstream phrasings are accepted; unparseable bodies
// return nil so a cap event is simply not invented.
func parseRateLimitReset(detail string) *float64 {
	if detail == "" {
		return nil
	}
	match := resetPattern.FindStringSubmatch(detail)
	if match == nil {
		match = resetFallbackPattern.FindStringSubmatch(detail)
	}
	if match == nil {
		return nil
	}
	stamp := strings.Replace(match[1], "T", " ", 1)
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", stamp, time.UTC)
	if err != nil {
		return nil
	}
	offset := 0
	if zone := timezonePattern.FindStringSubmatch(detail); zone != nil {
		hours, _ := strconv.Atoi(zone[1])
		minutes := 0
		if zone[2] != "" {
			minutes, _ = strconv.Atoi(zone[2])
		}
		offset = hours*3600 + minutes*60
		if hours < 0 {
			offset = hours*3600 - minutes*60
		}
	}
	value := float64(parsed.Unix() - int64(offset))
	return &value
}
