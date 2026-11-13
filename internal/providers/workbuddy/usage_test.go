package workbuddy

import (
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestUsageSnapshotFromStreamAndAdmission(t *testing.T) {
	stream := &chatStream{usage: StreamUsage{
		Realm: "intl", Account: "uid-1", Model: "m1",
		PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30,
		CachedTokens: 3, ReasoningTokens: 4,
		Credit: 1.5, CreditKnown: true, CostPolicy: "credits",
	}}
	snapshot := stream.UsageSnapshot()
	if snapshot.Realm != "intl" || snapshot.Account != "uid-1" || snapshot.Model != "m1" {
		t.Fatalf("attribution = %+v", snapshot)
	}
	if snapshot.TotalTokens != 30 || snapshot.PromptTokens != 10 || snapshot.CompletionTokens != 20 {
		t.Fatalf("tokens = %+v", snapshot)
	}
	if !snapshot.CreditKnown || snapshot.Credit != 1.5 {
		t.Fatalf("credit = %+v", snapshot)
	}
	if snapshot.Kind != "chat" || snapshot.CostPolicy != "credits" {
		t.Fatalf("policy = %+v", snapshot)
	}

	admitted := newAdmittedChatStream(stream, func() {})
	reporter, ok := admitted.(providers.UsageSnapshotReporter)
	if !ok {
		t.Fatal("admission wrapper does not expose UsageSnapshotReporter")
	}
	if got := reporter.UsageSnapshot(); got.Account != "uid-1" || got.TotalTokens != 30 {
		t.Fatalf("admitted snapshot = %+v", got)
	}
}
