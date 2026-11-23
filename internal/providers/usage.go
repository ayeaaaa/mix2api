package providers

import "encoding/json"

// BestCachedTokens returns the first positive cache-hit alias in an
// OpenAI-compatible usage object. Providers receive the raw upstream frame:
// some upstreams put the count in prompt_tokens_details.cached_tokens, others
// use one of the legacy aliases. A zero alias never shadows a later positive
// one.
func BestCachedTokens(usage map[string]any) int64 {
	if usage == nil {
		return 0
	}
	for _, candidate := range []any{
		nestedUsageValue(usage, "prompt_tokens_details", "cached_tokens"),
		usage["prompt_cache_hit_tokens"],
		usage["cache_read_input_tokens"],
		usage["cached_tokens"],
		nestedUsageValue(usage, "input_tokens_details", "cached_tokens"),
		nestedUsageValue(usage, "completion_tokens_details", "cached_tokens"),
	} {
		if number, ok := usageNumberValue(candidate); ok && number > 0 {
			return int64(number)
		}
	}
	return 0
}

func nestedUsageValue(usage map[string]any, keys ...string) any {
	var current any = usage
	for _, key := range keys {
		next, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = next[key]
	}
	return current
}

func usageNumberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	default:
		return 0, false
	}
}
