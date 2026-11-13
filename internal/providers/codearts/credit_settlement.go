package codearts

import (
	"context"
	"math"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const codeartsCreditSampleTimeout = 5 * time.Second

// sampleCreditUsed returns the account's cumulative used-credit counter. The
// baseline lookup uses the normal 60-second account-info cache; settlement
// forces a fresh sample so the delta includes the request that just finished.
func (p *Provider) sampleCreditUsed(ctx context.Context, account *Account, force bool) (float64, bool) {
	if p == nil || account == nil {
		return 0, false
	}
	if p.creditSample != nil {
		return p.creditSample(ctx, account, force)
	}
	info, err := p.FetchInfo(ctx, account, force)
	if err != nil || info == nil || info.Credits.Total == nil {
		return 0, false
	}
	return info.Credits.Total.Used, true
}

// takeCreditBaseline samples the cumulative used-credit counter before a chat
// POST. A lower sample is adopted as a package-renewal reset only when this
// request is alone; a stale sample must not erase concurrent charges.
func (p *Provider) takeCreditBaseline(ctx context.Context, account *Account) *float64 {
	if p == nil || account == nil {
		return nil
	}
	// Use the already-cached account snapshot for the baseline. Fetching the
	// statistics endpoint before every chat would add latency and make the
	// chat path depend on a second upstream call; the first request after a
	// restart therefore settles no credit, and later requests use the snapshot
	// refreshed by the previous settlement.
	info := account.infoSnapshot()
	if info == nil || info.Credits.Total == nil {
		return nil
	}
	used := info.Credits.Total.Used
	account.mu.Lock()
	active := account.activeConcurrent
	account.mu.Unlock()

	account.creditMu.Lock()
	if !account.creditSettledKnown || (used < account.creditSettledUsed && active <= 1) {
		account.creditSettledUsed = used
		account.creditSettledKnown = true
		account.creditExplicitPending = 0
	}
	account.creditMu.Unlock()
	value := used
	return &value
}

// settleCredit turns the account-wide cumulative used-credit delta into this
// request's credit. It is idempotent and best effort: sampling failures leave
// the usage row untouched, and an explicit upstream credit is not counted
// again in the next delta.
func (s *CodeArtsStream) settleCredit(snapshot *providers.UsageSnapshot) {
	if s == nil || snapshot == nil {
		return
	}
	s.creditMu.Lock()
	defer s.creditMu.Unlock()
	if s.creditFinalized {
		if s.creditKnown {
			snapshot.Credit = s.creditDelta
			snapshot.CreditKnown = true
		}
		return
	}
	s.creditFinalized = true
	if s.creditBaseline == nil || s.account == nil || s.provider == nil {
		return
	}
	if snapshot.CreditKnown && snapshot.Credit > 0 {
		s.creditKnown = true
		s.creditDelta = snapshot.Credit
		s.account.creditMu.Lock()
		if s.account.creditSettledKnown {
			s.account.creditExplicitPending += snapshot.Credit
		}
		s.account.creditMu.Unlock()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), codeartsCreditSampleTimeout)
	defer cancel()
	used, ok := s.provider.sampleCreditUsed(ctx, s.account, true)
	if !ok {
		return
	}

	s.account.creditMu.Lock()
	settled := s.account.creditSettledUsed
	if !s.account.creditSettledKnown {
		settled = *s.creditBaseline
		s.account.creditSettledUsed = settled
		s.account.creditSettledKnown = true
	}
	if used < settled {
		s.account.creditMu.Unlock()
		return
	}
	unaccounted := used - settled
	pending := s.account.creditExplicitPending
	delta := unaccounted
	if unaccounted >= pending {
		delta = unaccounted - pending
		pending = 0
	}
	s.account.creditSettledUsed = used
	s.account.creditExplicitPending = pending
	s.account.creditMu.Unlock()

	if delta <= 0 {
		return
	}
	snapshot.Credit = math.Round(delta*10000) / 10000
	snapshot.CreditKnown = true
	s.creditKnown = true
	s.creditDelta = snapshot.Credit
}
