package raccoon

import (
	"encoding/json"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// UsageSnapshot exposes the Raccoon stream's tracked usage and post-DONE
// credit frame through the provider-neutral hook. The stream already parses
// both values; this adapter never re-reads the upstream.
func (s *chatStream) UsageSnapshot() providers.UsageSnapshot {
	if s == nil {
		return providers.UsageSnapshot{}
	}
	s.usageMu.Lock()
	usage := cloneUsage(s.lastUsage)
	credit := s.lastCredit
	creditKnown := s.creditKnown
	s.usageMu.Unlock()
	account := ""
	if uid, ok := currentAccountUID(s.account); ok {
		account = strings.TrimSpace(uid)
	}
	snapshot := providers.UsageSnapshot{
		Realm:            ProviderRealm,
		Account:          account,
		Model:            strings.TrimSpace(s.model),
		PromptTokens:     raccoonUsageInt(usage, "prompt_tokens"),
		CompletionTokens: raccoonUsageInt(usage, "completion_tokens"),
		TotalTokens:      raccoonUsageInt(usage, "total_tokens"),
		CachedTokens:     raccoonUsageInt(usage, "cached_tokens"),
		ReasoningTokens:  raccoonUsageInt(usage, "reasoning_tokens"),
		Credit:           credit,
		CreditKnown:      creditKnown,
		Kind:             "chat",
		CostPolicy:       "raccoon:unpriced",
	}
	if snapshot.TotalTokens == 0 {
		snapshot.TotalTokens = snapshot.PromptTokens + snapshot.CompletionTokens
	}
	return snapshot
}

func currentAccountUID(account *Account) (string, bool) {
	if account == nil {
		return "", false
	}
	return account.currentUID()
}

func raccoonUsageInt(usage map[string]any, key string) int64 {
	value, ok := raccoonUsageFloat(usage[key])
	if !ok {
		return 0
	}
	return int64(value)
}

func raccoonUsageFloat(value any) (float64, bool) {
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
	default:
		return 0, false
	}
}

var _ providers.UsageSnapshotReporter = (*chatStream)(nil)
