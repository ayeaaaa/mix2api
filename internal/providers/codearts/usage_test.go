package codearts

import (
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestUsageSnapshotFromStream(t *testing.T) {
	stream := &CodeArtsStream{
		model: "codearts-model",
		usage: map[string]any{
			"prompt_tokens": 11, "completion_tokens": 22, "total_tokens": 33,
			"cached_tokens": 4, "reasoning_tokens": 5, "credit": 0.75,
		},
	}
	var reporter providers.UsageSnapshotReporter = stream
	snapshot := reporter.UsageSnapshot()
	if snapshot.Realm != ProviderRealm || snapshot.Model != "codearts-model" {
		t.Fatalf("attribution = %+v", snapshot)
	}
	if snapshot.TotalTokens != 33 || snapshot.CachedTokens != 4 || snapshot.ReasoningTokens != 5 {
		t.Fatalf("tokens = %+v", snapshot)
	}
	if !snapshot.CreditKnown || snapshot.Credit != 0.75 || snapshot.Kind != "chat" {
		t.Fatalf("credit/policy = %+v", snapshot)
	}
}
