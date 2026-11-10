package usage

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/pricing"
)

// RecordInput is one finished request. It deliberately has no prompt, header
// or secret field: the ledger stores attribution and token counts only.
type RecordInput struct {
	At             time.Time
	Model          string
	RequestedModel string
	Realm          string
	Account        string
	KeyID          string
	Kind           string
	Outcome        string
	Stream         bool
	Usage          map[string]any
	ElapsedMS      int64
	TTFTMS         int64
	GenMS          int64
	Effort         string
	Media          map[string]any
}

// Recorder writes the v1 usage row shape.
type Recorder struct {
	Store *Store
	Now   func() time.Time
}

// Record appends exactly one row. An append failure is returned so the caller
// can log it; it must never change the client response.
func (r *Recorder) Record(ctx context.Context, in RecordInput) error {
	if r == nil || r.Store == nil {
		return fmt.Errorf("usage: nil recorder")
	}
	_ = ctx
	at := in.At
	if at.IsZero() {
		if r.Now != nil {
			at = r.Now()
		} else {
			at = time.Now()
		}
	}
	row := map[string]any{
		"at":         float64(at.UnixNano()) / 1e9,
		"iso":        at.Local().Format("2006-01-02T15:04:05"),
		"model":      in.Model,
		"stream":     in.Stream,
		"outcome":    defaultRecordOutcome(in.Outcome),
		"kind":       defaultRecordKind(in.Kind),
		"elapsed_ms": in.ElapsedMS,
		"ttft_ms":    in.TTFTMS,
		"gen_ms":     in.GenMS,
	}
	if in.RequestedModel != "" && in.RequestedModel != in.Model {
		row["requested_model"] = in.RequestedModel
	}
	usageMissing := len(in.Usage) == 0
	if usageMissing {
		row["usage_missing"] = true
	} else {
		copyUsageFields(row, in.Usage)
	}
	if in.Account != "" {
		row["account"] = in.Account
	}
	realm := in.Realm
	if realm == "" && r.Store != nil {
		realm = DetectModelRealm(in.Model)
	}
	row["realm"] = realm
	row["key"] = in.KeyID

	switch {
	case row["kind"] != "chat":
		row["cost_policy"] = "media"
	case realm == "codearts" || realm == "raccoon":
		row["cost_policy"] = realm + ":unpriced"
	default:
		row["cost_policy"] = pricing.CurrentPolicyID(in.Model)
	}
	if in.Effort != "" {
		row["reasoning_effort"] = in.Effort
	}
	for _, field := range []string{"seconds", "resolution", "aspect_ratio", "image_to_video"} {
		if value, ok := in.Media[field]; ok && value != nil {
			row[field] = value
		}
	}
	completion := fieldFloat(in.Usage, "completion_tokens")
	if in.GenMS > 0 {
		row["tokens_per_sec"] = math.Round(completion/(float64(in.GenMS)/1000)*100) / 100
	}
	prompt := fieldFloat(in.Usage, "prompt_tokens")
	if prompt > 0 {
		row["cache_hit_pct"] = math.Round(bestCachedUsageTokens(in.Usage)*100/prompt*10) / 10
	}
	return r.Store.Append(row)
}

func defaultRecordOutcome(outcome string) string {
	if outcome == "" {
		return "completed"
	}
	return outcome
}

func defaultRecordKind(kind string) string {
	if kind == "" {
		return "chat"
	}
	return kind
}

func copyUsageFields(row map[string]any, usage map[string]any) {
	// _extract_usage in v1 always emits the full six-field block, with zero
	// defaults, even when an upstream omitted some keys.
	for _, field := range []string{
		"prompt_tokens", "completion_tokens", "reasoning_tokens",
		"cached_tokens", "total_tokens", "credit",
	} {
		row[field] = int64(0)
	}
	for _, field := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if value, ok := numericValue(usage[field]); ok {
			row[field] = int64(value)
		}
	}
	reasoning := usage["reasoning_tokens"]
	if reasoning == nil {
		reasoning = nestedUsageValue(usage, "completion_tokens_details", "reasoning_tokens")
	}
	if value, ok := numericValue(reasoning); ok {
		row["reasoning_tokens"] = int64(value)
	}
	if value, ok := numericValue(usage["credit"]); ok {
		row["credit"] = value
	}
	row["cached_tokens"] = int64(bestCachedUsageTokens(usage))
}

func nestedUsageValue(usage map[string]any, key, nested string) any {
	value, ok := usage[key].(map[string]any)
	if !ok {
		return nil
	}
	return value[nested]
}

// bestCachedUsageTokens mirrors v1 _best_cached_tokens: first positive alias
// wins, and zero aliases never shadow a real hit.
func bestCachedUsageTokens(usage map[string]any) float64 {
	for _, value := range []any{
		nestedUsageValue(usage, "prompt_tokens_details", "cached_tokens"),
		usage["prompt_cache_hit_tokens"],
		usage["cache_read_input_tokens"],
		usage["cached_tokens"],
		nestedUsageValue(usage, "input_tokens_details", "cached_tokens"),
		nestedUsageValue(usage, "completion_tokens_details", "cached_tokens"),
	} {
		if number, ok := numericValue(value); ok && number > 0 {
			return number
		}
	}
	return 0
}

func fieldFloat(usage map[string]any, field string) float64 {
	if usage == nil {
		return 0
	}
	value, ok := numericValue(usage[field])
	if !ok {
		return 0
	}
	return value
}
