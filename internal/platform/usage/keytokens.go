package usage

import (
	"context"
	"time"
)

const keyTokensCacheTTL = 15 * time.Second

// KeyTokensToday returns the total tokens recorded for one API key since the
// local midnight. It folds the same ledger rows as the account token view,
// including failed rows whose account is empty; the key field is checked
// before any account filter. A short cache keeps a per-request guard from
// rescanning the ledger for every chat call.
func (s *Service) KeyTokensToday(ctx context.Context, keyID string) (int64, error) {
	if s == nil || s.Store == nil || keyID == "" {
		return 0, nil
	}
	now := s.now()
	day := now.Format("2006-01-02")
	s.keyTokensMu.Lock()
	if s.keyTokensDay == day && s.keyTokens != nil && now.Sub(s.keyTokensAt) < keyTokensCacheTTL {
		value := s.keyTokens[keyID]
		s.keyTokensMu.Unlock()
		return value, nil
	}
	s.keyTokensMu.Unlock()

	totals := make(map[string]int64)
	_, err := s.scanAll(ctx, func(row Row) error {
		at, ok := RowAt(row)
		if !ok || time.Unix(int64(at), 0).In(now.Location()).Format("2006-01-02") != day {
			return nil
		}
		key := RowKey(row)
		if key == "" {
			return nil
		}
		totals[key] += intNumber(row, "total_tokens")
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.keyTokensMu.Lock()
	s.keyTokensDay = day
	s.keyTokensAt = now
	s.keyTokens = totals
	value := totals[keyID]
	s.keyTokensMu.Unlock()
	return value, nil
}
