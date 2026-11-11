package pricing

import "sort"

// RowPricer is an immutable pricing view for one batch/scan. It loads the
// timeline, policy table, markup and override maps once so a full usage scan
// does not stat or lock the same local files for every row.
type RowPricer struct {
	enabled   bool
	usdCNY    float64
	models    map[string]Entry
	policies  map[string]Policy
	timeline  []TimelineRow
	markup    map[string]float64
	overrides map[string]string
}

// NewRowPricer snapshots the current local pricing inputs. The returned value
// is read-only and safe to share across a scan; later file updates affect the
// next pricer, not this one.
func (s *Snapshot) NewRowPricer() *RowPricer {
	if s == nil {
		return &RowPricer{usdCNY: 1}
	}
	return &RowPricer{
		enabled:   s.Enabled(),
		usdCNY:    s.USDCNY(),
		models:    s.Models,
		policies:  s.policiesRef(),
		timeline:  s.timelineRef(),
		markup:    s.markupRef(),
		overrides: s.overridesRef(),
	}
}

// CostForRow prices one row against the immutable view.
func (p *RowPricer) CostForRow(row map[string]any) Cost {
	if len(row) == 0 {
		return Cost{}
	}
	if p == nil || !p.enabled {
		return Cost{Disabled: true, Source: "builtin"}
	}
	model := asString(row["model"])
	pid := asString(row["cost_policy"])
	if pid == "codearts:unpriced" || pid == "raccoon:unpriced" {
		return Cost{Source: pid}
	}
	backfilled := false
	if pid == "" {
		pid, backfilled = policyRefAtTimeline(p.timeline, asFloatDefault(row["at"]), model)
	}
	markup := p.markup[model]
	if markup <= 0 {
		markup = 1
	}
	if policy, ok := p.policies[pid]; ok {
		entry := policyEntry(policy)
		usdCNY := policy.USDCNY
		if usdCNY <= 0 {
			usdCNY = p.usdCNY
		}
		cost, rates, band := costForRowFromEntry(row, entry, usdCNY)
		cost.CNY *= markup
		cost.Markup = markup
		cost.Rates = rates
		cost.Band = band
		cost.Source = pid
		cost.Policy = pid
		cost.SourceAt = formatEpoch(policy.FirstSeen)
		cost.SourceAtEpoch = policy.FirstSeen
		cost.Backfilled = backfilled
		cost.BandNote = BandNote(entry, band)
		fillViaWith(&cost, entry, model, p.overrides)
		return cost
	}
	entry, ok := p.models[model]
	if !ok {
		cost := Cost{Source: "builtin", Backfilled: false}
		cost.Markup = markup
		return cost
	}
	cost, rates, band := costForRowFromEntry(row, entry, p.usdCNY)
	cost.CNY *= markup
	cost.Markup = markup
	cost.Rates = rates
	cost.Band = band
	cost.Source = "builtin"
	cost.SourceAt = ""
	cost.Backfilled = false
	cost.BandNote = BandNote(entry, band)
	fillViaWith(&cost, entry, model, p.overrides)
	return cost
}

// policyRefAtTimeline is the immutable-timeline form of Snapshot.policyRefAt.
func policyRefAtTimeline(timeline []TimelineRow, epoch float64, model string) (string, bool) {
	if len(timeline) == 0 {
		return "", false
	}
	index := sort.Search(len(timeline), func(i int) bool { return timeline[i].At > epoch }) - 1
	if index < 0 {
		index = 0
	}
	if id := timeline[index].Policies[model]; id != "" {
		return id, false
	}
	for _, row := range timeline {
		if row.At < epoch {
			continue
		}
		if id := row.Policies[model]; id != "" {
			return id, true
		}
	}
	for position := index; position >= 0; position-- {
		if id := timeline[position].Policies[model]; id != "" {
			return id, true
		}
	}
	return "", false
}
