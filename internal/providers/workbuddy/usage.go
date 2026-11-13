package workbuddy

import (
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// UsageSnapshot exposes the WorkBuddy stream's existing usage/finish state
// through the provider-neutral hook. The stream already tracks the upstream
// usage frame, so this adapter never re-parses a frame.
func (s *chatStream) UsageSnapshot() providers.UsageSnapshot {
	if s == nil {
		return providers.UsageSnapshot{}
	}
	usage := s.StreamUsage()
	snapshot := providers.UsageSnapshot{
		Realm:            strings.TrimSpace(usage.Realm),
		Account:          strings.TrimSpace(usage.Account),
		Model:            strings.TrimSpace(usage.Model),
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		CachedTokens:     usage.CachedTokens,
		ReasoningTokens:  usage.ReasoningTokens,
		Credit:           usage.Credit,
		CreditKnown:      usage.CreditKnown,
		Free:             usage.Free,
		CostPolicy:       strings.TrimSpace(usage.CostPolicy),
		Kind:             "chat",
	}
	if snapshot.TotalTokens == 0 {
		if usage.Tokens > 0 {
			snapshot.TotalTokens = usage.Tokens
		} else {
			snapshot.TotalTokens = snapshot.PromptTokens + snapshot.CompletionTokens
		}
	}
	if snapshot.CostPolicy == "" {
		snapshot.CostPolicy = "credits"
	}
	return snapshot
}

// UsageSnapshot forwards the generic usage hook through the admission wrapper
// so the API does not need to know about the shared chat permit.
func (s *admittedChatStream) UsageSnapshot() providers.UsageSnapshot {
	if s == nil {
		return providers.UsageSnapshot{}
	}
	if reporter, ok := s.ChatStream.(providers.UsageSnapshotReporter); ok {
		return reporter.UsageSnapshot()
	}
	return providers.UsageSnapshot{}
}

var (
	_ providers.UsageSnapshotReporter = (*chatStream)(nil)
	_ providers.UsageSnapshotReporter = (*admittedChatStream)(nil)
	_ providers.LimitApplier          = (*Provider)(nil)
)
