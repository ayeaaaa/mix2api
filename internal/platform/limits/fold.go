// Package limits folds today's usage rows and publishes the shared daily
// guards to providers that opt in through providers.LimitApplier.
package limits

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
)

// Folds is one provider realm's daily counters. Keys are account uids.
//
// Fold expects rows from a single provider realm; callers that have a mixed
// ledger must group by realm first (FoldRealm/FoldAll do that) so the same uid
// on two exits is never merged.
type Folds struct {
	Tokens  map[string]int64
	Credits map[string]float64
	Models  map[string]map[string]int64
}

// Empty reports whether the fold holds no accounts.
func (f Folds) Empty() bool {
	return len(f.Tokens) == 0 && len(f.Credits) == 0 && len(f.Models) == 0
}

func newFolds() Folds {
	return Folds{
		Tokens:  map[string]int64{},
		Credits: map[string]float64{},
		Models:  map[string]map[string]int64{},
	}
}

// Fold folds one realm's rows. Only rows whose at timestamp is inside
// [dayStart, dayEnd) are counted; a zero dayEnd leaves the upper bound open.
//
// The rules match v1's _scan_daily_usage exactly: client_aborted rows are
// skipped whole, rows without an account are skipped, total_tokens is added to
// the account's token count, a non-zero credit is added to its credit count
// (including media rows), and per-model tokens are keyed by uid/model.
func Fold(rows []usage.Row, dayStart, dayEnd time.Time) Folds {
	folds := newFolds()
	for _, row := range rows {
		foldRow(&folds, row, dayStart, dayEnd)
	}
	return folds
}

// FoldRealm folds only rows attributed to realm. Rows are grouped by the same
// realm resolution the usage readers use, so legacy rows fall back to the
// model's home realm and a hand-written realm value is not silently dropped.
func FoldRealm(rows []usage.Row, realm string, dayStart, dayEnd time.Time) Folds {
	folds := newFolds()
	for _, row := range rows {
		if usage.RowRealm(row) != realm {
			continue
		}
		foldRow(&folds, row, dayStart, dayEnd)
	}
	return folds
}

// FoldAll folds every realm in one pass over rows. The returned map always
// contains the realms observed in the rows.
func FoldAll(rows []usage.Row, dayStart, dayEnd time.Time) map[string]Folds {
	out := map[string]Folds{}
	for _, row := range rows {
		realm := usage.RowRealm(row)
		folds, ok := out[realm]
		if !ok {
			folds = newFolds()
			out[realm] = folds
		}
		foldRow(&folds, row, dayStart, dayEnd)
		// newFolds maps are reference values; a fold that only adds entries
		// mutates the stored value. Assignment keeps this correct even if the
		// maps are replaced later.
		out[realm] = folds
	}
	return out
}

func foldRow(folds *Folds, row usage.Row, dayStart, dayEnd time.Time) {
	if folds == nil || row == nil {
		return
	}
	at, ok := usage.RowAt(row)
	if !ok {
		return
	}
	when := time.Unix(0, int64(at*float64(time.Second)))
	if when.Before(dayStart) {
		return
	}
	if !dayEnd.IsZero() && !when.Before(dayEnd) {
		return
	}
	if usage.RowOutcome(row) == "client_aborted" {
		return
	}
	account := usage.RowAccount(row)
	if account == "" {
		return
	}
	tokens := int64(numeric(row["total_tokens"]))
	folds.Tokens[account] += tokens
	if credit := numeric(row["credit"]); credit != 0 {
		folds.Credits[account] += credit
	}
	if model := usage.RowModel(row); model != "" {
		perModel := folds.Models[account]
		if perModel == nil {
			perModel = map[string]int64{}
			folds.Models[account] = perModel
		}
		perModel[model] += tokens
	}
}

func numeric(value any) float64 {
	number, ok := usageNumber(value)
	if !ok {
		return 0
	}
	return number
}

// usageNumber mirrors usage.numericValue without exporting the helper. It
// accepts json.Number, float and integer spellings and numeric strings.
func usageNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, false
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
