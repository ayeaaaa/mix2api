package codearts

import (
	"encoding/json"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// UsageSnapshot exposes the CodeArts stream's tracked usage map through the
// provider-neutral hook. The stream already records the upstream usage chunk;
// this adapter does not re-parse frames.
func (s *CodeArtsStream) UsageSnapshot() providers.UsageSnapshot {
	if s == nil {
		return providers.UsageSnapshot{}
	}
	s.usageMu.Lock()
	usage := cloneCodeArtsUsage(s.usage)
	s.usageMu.Unlock()
	account := ""
	if uid, ok := currentAccountUID(s.account); ok {
		account = strings.TrimSpace(uid)
	}
	snapshot := providers.UsageSnapshot{
		Realm:            ProviderRealm,
		Account:          account,
		Model:            strings.TrimSpace(s.model),
		PromptTokens:     codeArtsUsageInt(usage, "prompt_tokens"),
		CompletionTokens: codeArtsUsageInt(usage, "completion_tokens"),
		TotalTokens:      codeArtsUsageInt(usage, "total_tokens"),
		CachedTokens:     codeArtsUsageInt(usage, "cached_tokens"),
		ReasoningTokens:  codeArtsUsageInt(usage, "reasoning_tokens"),
		Kind:             "chat",
	}
	if snapshot.TotalTokens == 0 {
		snapshot.TotalTokens = snapshot.PromptTokens + snapshot.CompletionTokens
	}
	if credit, ok := codeArtsUsageFloat(usage["credit"]); ok {
		snapshot.Credit = credit
		snapshot.CreditKnown = true
	}
	return snapshot
}

func currentAccountUID(account *Account) (string, bool) {
	if account == nil {
		return "", false
	}
	return account.currentUID()
}

func cloneCodeArtsUsage(usage map[string]any) map[string]any {
	if len(usage) == 0 {
		return nil
	}
	out := make(map[string]any, len(usage))
	for key, value := range usage {
		out[key] = value
	}
	return out
}

func codeArtsUsageInt(usage map[string]any, key string) int64 {
	value, ok := codeArtsUsageFloat(usage[key])
	if !ok {
		return 0
	}
	return int64(value)
}

func codeArtsUsageFloat(value any) (float64, bool) {
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

var _ providers.UsageSnapshotReporter = (*CodeArtsStream)(nil)
