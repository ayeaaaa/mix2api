package pricing

import "math"

// Estimate is a batch pricing result used by aggregate views and tooling.
type Estimate struct {
	CNY     float64
	Priced  int
	Missing map[string]int
}

// SumRows prices a batch of rows and reports unpriced models separately.
// Unpriced rows never contribute a guessed zero price; they are counted in
// Missing so the panel can name what the snapshot does not cover.
func (s *Snapshot) SumRows(rows []map[string]any) Estimate {
	out := Estimate{Missing: map[string]int{}}
	for _, row := range rows {
		cost := s.CostForRow(row)
		if cost.Known {
			out.CNY += cost.CNY
			out.Priced++
			continue
		}
		if cost.Disabled {
			continue
		}
		model := asString(row["model"])
		out.Missing[model]++
	}
	return out
}

// SumRows is the package-level form using the active snapshot.
func SumRows(rows []map[string]any) Estimate { return Active().SumRows(rows) }

// RoundWire rounds only at the wire boundary, matching v1's six-decimal
// presentation of a row cost.
func RoundWire(value float64) float64 {
	return math.Round(value*1_000_000) / 1_000_000
}
