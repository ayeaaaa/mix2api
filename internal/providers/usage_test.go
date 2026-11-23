package providers

import (
	"encoding/json"
	"testing"
)

func TestBestCachedTokensAliases(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]any
		want  int64
	}{
		{"prompt_tokens_details", map[string]any{
			"prompt_tokens_details": map[string]any{"cached_tokens": json.Number("80")},
		}, 80},
		{"prompt_cache_hit_tokens", map[string]any{"prompt_cache_hit_tokens": 70}, 70},
		{"cache_read_input_tokens", map[string]any{"cache_read_input_tokens": int64(60)}, 60},
		{"cached_tokens", map[string]any{"cached_tokens": 50}, 50},
		{"input_tokens_details", map[string]any{
			"input_tokens_details": map[string]any{"cached_tokens": 40},
		}, 40},
		{"completion_tokens_details", map[string]any{
			"completion_tokens_details": map[string]any{"cached_tokens": 30},
		}, 30},
		{"zero_does_not_shadow", map[string]any{
			"prompt_tokens_details": map[string]any{"cached_tokens": 0},
			"cached_tokens":         25,
		}, 25},
		{"missing", map[string]any{}, 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BestCachedTokens(tc.usage); got != tc.want {
				t.Fatalf("BestCachedTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}
