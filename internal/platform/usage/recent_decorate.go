package usage

import (
	"context"
	"fmt"
	"math"
)

// Recent returns the v1 /usage/recent page with key names and the full cost
// decoration block. Unpriced rows leave every cost field nil, never 0.
func (s *Service) Recent(ctx context.Context, opts RecentOptions) (RecentPage, error) {
	if s == nil || s.Store == nil {
		return RecentPage{}, fmt.Errorf("usage: nil service")
	}
	page, err := s.Store.Recent(opts)
	if err != nil {
		return RecentPage{}, err
	}
	page.USDCNY = s.usdCNY()
	pricer := s.newRowPricer()
	for _, row := range page.Rows {
		select {
		case <-ctx.Done():
			return RecentPage{}, ctx.Err()
		default:
		}
		mapped := Row(row)
		row["key_name"] = s.keyDisplay(stringValue(mapped, "key"))
		cost := s.costForWith(pricer, row)
		if !cost.Known {
			row["cost_cny"] = nil
			row["cost_band"] = nil
			row["cost_source"] = nil
			row["cost_source_at"] = nil
			row["cost_backfilled"] = false
			row["cost_rates"] = nil
			row["cost_unit"] = nil
			row["cost_currency"] = nil
			row["cost_usd_cny"] = nil
			row["cost_or_id"] = nil
			row["cost_via"] = nil
			row["cost_inherited_from"] = nil
			row["cost_override_from"] = nil
			row["cost_band_note"] = nil
			row["cost_via_derived"] = nil
			continue
		}
		row["cost_cny"] = math.Round(cost.CNY*1_000_000) / 1_000_000
		if cost.Band != nil {
			row["cost_band"] = *cost.Band
		} else {
			row["cost_band"] = nil
		}
		row["cost_source"] = cost.Source
		if cost.SourceAtEpoch > 0 {
			row["cost_source_at"] = cost.SourceAtEpoch
		} else {
			row["cost_source_at"] = nil
		}
		row["cost_backfilled"] = cost.Backfilled
		if cost.Rates != nil {
			row["cost_rates"] = cost.Rates
		} else {
			row["cost_rates"] = nil
		}
		row["cost_unit"] = cost.Unit
		row["cost_currency"] = cost.Currency
		row["cost_usd_cny"] = cost.USDCNY
		row["cost_or_id"] = cost.ORID
		row["cost_via"] = cost.Via
		row["cost_inherited_from"] = cost.InheritedFrom
		row["cost_override_from"] = cost.OverrideFrom
		row["cost_band_note"] = cost.BandNote
		row["cost_via_derived"] = cost.ViaDerived
	}
	return page, nil
}
