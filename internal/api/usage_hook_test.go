package api

import (
	"io"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type hookTestStream struct {
	snapshot providers.UsageSnapshot
}

func (s *hookTestStream) Next() ([]byte, error) { return nil, io.EOF }
func (s *hookTestStream) Close() error          { return nil }
func (s *hookTestStream) UsageSnapshot() providers.UsageSnapshot {
	return s.snapshot
}

type bareTestStream struct{}

func (bareTestStream) Next() ([]byte, error) { return nil, io.EOF }
func (bareTestStream) Close() error          { return nil }

func TestProviderUsageSnapshots(t *testing.T) {
	bare := newUsageTracker(withAttribution(bareTestStream{}, "codearts"), time.Now())
	snapshot := bare.Snapshot()
	if snapshot.Account != "" {
		t.Fatalf("provider without a hook fabricated an account: %+v", snapshot)
	}
	if snapshot.Realm != "codearts" {
		t.Fatalf("provider realm = %q", snapshot.Realm)
	}

	reported := newUsageTracker(withAttribution(&hookTestStream{snapshot: providers.UsageSnapshot{
		Account: "uid-1", Model: "m1", TotalTokens: 9, Credit: 1.25, CreditKnown: true,
	}}, "intl"), time.Now())
	snapshot = reported.Snapshot()
	if snapshot.Account != "uid-1" || snapshot.Realm != "intl" || snapshot.TotalTokens != 9 {
		t.Fatalf("reported snapshot = %+v", snapshot)
	}
	if !snapshot.CreditKnown || snapshot.Credit != 1.25 {
		t.Fatalf("reported credit = %+v", snapshot)
	}
}
