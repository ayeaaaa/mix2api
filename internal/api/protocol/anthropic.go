package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
)

// AnthropicErrorType maps an HTTP status to the Anthropic error.type
// vocabulary used by v1.6.5.
func AnthropicErrorType(status int) string {
	switch status {
	case 400, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// AnthropicError builds the native error envelope.
func AnthropicError(status int, message, errType string) map[string]any {
	if strings.TrimSpace(errType) == "" {
		errType = AnthropicErrorType(status)
	}
	if strings.TrimSpace(message) == "" {
		message = "upstream error"
	}
	return map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	}
}

// AnthropicUsage maps an OpenAI usage block to the Messages shape. The
// upstream prompt_tokens already includes cached tokens; Anthropic clients add
// cache_read + cache_creation to input_tokens, so v1 reports only the
// uncached remainder as input_tokens.
func AnthropicUsage(usage map[string]any) map[string]any {
	out := map[string]any{
		"input_tokens":                int64(0),
		"cache_creation_input_tokens": int64(0),
		"cache_read_input_tokens":     int64(0),
		"output_tokens":               int64(0),
	}
	if usage == nil {
		return out
	}
	promptTokens, _ := Int64(usage["prompt_tokens"])
	outputTokens, _ := Int64(usage["completion_tokens"])
	cacheRead, _ := Int64(usage["prompt_cache_hit_tokens"])
	if cacheRead == 0 {
		cacheRead = BestCachedTokens(usage)
	}
	cacheCreation, _ := Int64(usage["prompt_cache_write_tokens"])
	out["output_tokens"] = outputTokens
	out["cache_read_input_tokens"] = cacheRead
	out["cache_creation_input_tokens"] = cacheCreation
	inputTokens := promptTokens - cacheRead - cacheCreation
	if inputTokens < 0 {
		inputTokens = 0
	}
	out["input_tokens"] = inputTokens
	if serviceTier, ok := usage["service_tier"].(string); ok && serviceTier != "" {
		out["service_tier"] = serviceTier
	}
	return out
}

// BestCachedTokens mirrors v1 _best_cached_tokens: the first positive cache
// alias wins, so a zero alias never shadows a real hit.
func BestCachedTokens(usage map[string]any) int64 {
	if usage == nil {
		return 0
	}
	candidates := []any{
		nestedValue(usage, "prompt_tokens_details", "cached_tokens"),
		usage["prompt_cache_hit_tokens"],
		usage["cache_read_input_tokens"],
		usage["cached_tokens"],
		nestedValue(usage, "input_tokens_details", "cached_tokens"),
		nestedValue(usage, "completion_tokens_details", "cached_tokens"),
	}
	for _, candidate := range candidates {
		number, ok := Float(candidate)
		if ok && number > 0 {
			return int64(number)
		}
	}
	return 0
}

func nestedValue(object map[string]any, keys ...string) any {
	var current any = object
	for _, key := range keys {
		next, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = next[key]
	}
	return current
}

// AnthropicStopReason maps an OpenAI finish_reason to the native vocabulary.
func AnthropicStopReason(finish string) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter", "refusal":
		return "refusal"
	default:
		return "end_turn"
	}
}

// ParseToolInput turns OpenAI tool arguments into an Anthropic tool_use input
// object. A non-JSON string is preserved under "raw"; a non-object JSON value
// is preserved under "value", matching v1.
func ParseToolInput(value any) any {
	switch typed := value.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return typed
	case string:
		if strings.TrimSpace(typed) == "" {
			return map[string]any{}
		}
		decoder := json.NewDecoder(strings.NewReader(typed))
		decoder.UseNumber()
		var parsed any
		if err := decoder.Decode(&parsed); err != nil {
			return map[string]any{"raw": typed}
		}
		if object, ok := parsed.(map[string]any); ok {
			return object
		}
		return map[string]any{"value": parsed}
	default:
		return map[string]any{"value": typed}
	}
}

// JSONText serializes a value compactly without HTML escaping. Strings pass
// through unchanged, nil becomes "{}" (the v1 helper's behavior), and values
// with no JSON representation degrade to "{}".
func JSONText(value any) string {
	switch typed := value.(type) {
	case nil:
		return "{}"
	case string:
		return typed
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}
