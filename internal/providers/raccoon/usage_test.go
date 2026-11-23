package raccoon

import (
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestUsageSnapshotFromStreamIncludingCreditFrame(t *testing.T) {
	stream := &chatStream{
		model: "raccoon-model",
		lastUsage: map[string]any{
			"prompt_tokens": 7, "completion_tokens": 8, "total_tokens": 15,
		},
		lastCredit:  2.25,
		creditKnown: true,
	}
	var reporter providers.UsageSnapshotReporter = stream
	snapshot := reporter.UsageSnapshot()
	if snapshot.Realm != ProviderRealm || snapshot.Model != "raccoon-model" {
		t.Fatalf("attribution = %+v", snapshot)
	}
	if snapshot.TotalTokens != 15 || snapshot.PromptTokens != 7 || snapshot.CompletionTokens != 8 {
		t.Fatalf("tokens = %+v", snapshot)
	}
	if !snapshot.CreditKnown || snapshot.Credit != 2.25 || snapshot.Kind != "chat" {
		t.Fatalf("credit/policy = %+v", snapshot)
	}
}

// Raccoon reports cache hits in prompt_tokens_details.cached_tokens on the
// upstream usage frame. The snapshot must read that nested alias or every
// real hit is recorded as zero.
func TestUsageSnapshotReadsNestedCachedTokens(t *testing.T) {
	stream := &chatStream{
		lastUsage: map[string]any{
			"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110,
			"prompt_tokens_details": map[string]any{"cached_tokens": 80},
		},
	}
	if got := stream.UsageSnapshot().CachedTokens; got != 80 {
		t.Fatalf("CachedTokens = %d, want 80", got)
	}
}
