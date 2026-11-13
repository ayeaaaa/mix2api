package codearts

import (
	"context"
	"testing"
)

func codeartsCreditProvider(used float64, ok bool) *Provider {
	return &Provider{creditSample: func(context.Context, *Account, bool) (float64, bool) {
		return used, ok
	}}
}

func codeartsCreditStream(provider *Provider, account *Account, baseline float64, usage map[string]any) *CodeArtsStream {
	return &CodeArtsStream{
		provider:       provider,
		account:        account,
		creditBaseline: &baseline,
		usage:          usage,
	}
}

func TestCodeArtsCreditSettlementUsesDelta(t *testing.T) {
	account := &Account{}
	account.creditSettledKnown = true
	account.creditSettledUsed = 10
	stream := codeartsCreditStream(codeartsCreditProvider(10.25, true), account, 10,
		map[string]any{"total_tokens": 5})

	snapshot := stream.UsageSnapshot()
	if !snapshot.CreditKnown || snapshot.Credit != 0.25 {
		t.Fatalf("credit = %+v, want 0.25", snapshot)
	}
	if again := stream.UsageSnapshot(); again.Credit != 0.25 || !again.CreditKnown {
		t.Fatalf("second snapshot = %+v, want idempotent 0.25", again)
	}
	if account.creditSettledUsed != 10.25 {
		t.Fatalf("settled used = %v, want 10.25", account.creditSettledUsed)
	}
}

func TestCodeArtsCreditSettlementKeepsExplicitCreditOnce(t *testing.T) {
	account := &Account{}
	account.creditSettledKnown = true
	account.creditSettledUsed = 10
	first := codeartsCreditStream(codeartsCreditProvider(10.25, true), account, 10,
		map[string]any{"total_tokens": 5, "credit": 0.5})
	if got := first.UsageSnapshot(); !got.CreditKnown || got.Credit != 0.5 {
		t.Fatalf("explicit credit = %+v, want 0.5", got)
	}
	if account.creditExplicitPending != 0.5 {
		t.Fatalf("pending explicit credit = %v, want 0.5", account.creditExplicitPending)
	}

	second := codeartsCreditStream(codeartsCreditProvider(10.75, true), account, 10,
		map[string]any{"total_tokens": 5})
	if got := second.UsageSnapshot(); !got.CreditKnown || got.Credit != 0.25 {
		t.Fatalf("settled credit = %+v, want 0.25 after explicit 0.5", got)
	}
	if account.creditExplicitPending != 0 {
		t.Fatalf("pending explicit credit = %v, want 0", account.creditExplicitPending)
	}
}

func TestCodeArtsCreditSettlementSampleFailureIsBestEffort(t *testing.T) {
	account := &Account{}
	stream := codeartsCreditStream(codeartsCreditProvider(0, false), account, 10,
		map[string]any{"total_tokens": 5})
	if got := stream.UsageSnapshot(); got.CreditKnown {
		t.Fatalf("credit = %+v, want unknown after sample failure", got)
	}
}

func TestCodeArtsCreditSettlementConcurrentHighWater(t *testing.T) {
	account := &Account{}
	account.creditSettledKnown = true
	account.creditSettledUsed = 20
	provider := codeartsCreditProvider(20.5, true)
	first := codeartsCreditStream(provider, account, 20, map[string]any{"total_tokens": 5})
	second := codeartsCreditStream(provider, account, 20, map[string]any{"total_tokens": 5})

	if got := first.UsageSnapshot(); !got.CreditKnown || got.Credit != 0.5 {
		t.Fatalf("first credit = %+v, want 0.5", got)
	}
	if got := second.UsageSnapshot(); got.CreditKnown || got.Credit != 0 {
		t.Fatalf("second credit = %+v, want 0 after high-water settlement", got)
	}
}
