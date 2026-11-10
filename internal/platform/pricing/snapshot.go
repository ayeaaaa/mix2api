// Package pricing owns the v1.6.5-compatible OpenRouter price snapshot,
// conditional bands, price policies/timeline, markup calibration and the
// per-row equivalent-token cost calculation.
//
// The package is provider-neutral. It only reads local files and, when the
// caller injects an HTTP client, the OpenRouter model catalogue. No request
// path in this package performs network I/O.
package pricing

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Meta is the snapshot header. The v1 file also carries sources/note; they are
// preserved through Metadata for panel display.
type Meta struct {
	USDCNY       float64           `json:"usd_cny"`
	GeneratedAt  string            `json:"generated_at,omitempty"`
	BaseCurrency string            `json:"base_currency,omitempty"`
	Sources      map[string]string `json:"sources,omitempty"`
	Note         string            `json:"note,omitempty"`
}

// Rates is one per-unit price table. Unit is the entry's token unit (1M in the
// shipped snapshot), so every value is a price per Unit tokens.
type Rates struct {
	InputCacheHit  float64 `json:"input_cache_hit"`
	InputCacheMiss float64 `json:"input_cache_miss"`
	Output         float64 `json:"output"`

	// NullInputCacheHit records that the source explicitly carried null for
	// input_cache_hit. v1 falls back to the cache-miss rate in that case; a
	// literal 0 is a real price and must not be confused with null.
	NullInputCacheHit bool `json:"-"`
}

// UnmarshalJSON keeps the null/zero distinction for the cache-hit rate.
func (r *Rates) UnmarshalJSON(data []byte) error {
	var raw map[string]*float64
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.InputCacheHit = 0
	r.InputCacheMiss = 0
	r.Output = 0
	// An absent cache-hit key behaves like null in v1: _pick falls back to the
	// cache-miss rate. An explicit 0 is a real price and must stay 0.
	r.NullInputCacheHit = true
	if value, ok := raw["input_cache_hit"]; ok {
		if value == nil {
			r.NullInputCacheHit = true
		} else {
			r.InputCacheHit = *value
			r.NullInputCacheHit = false
		}
	}
	if value, ok := raw["input_cache_miss"]; ok && value != nil {
		r.InputCacheMiss = *value
	}
	if value, ok := raw["output"]; ok && value != nil {
		r.Output = *value
	}
	return nil
}

// MarshalJSON writes null back for a source that carried one.
func (r Rates) MarshalJSON() ([]byte, error) {
	type wire struct {
		InputCacheHit  *float64 `json:"input_cache_hit"`
		InputCacheMiss float64  `json:"input_cache_miss"`
		Output         float64  `json:"output"`
	}
	var hit *float64
	if !r.NullInputCacheHit {
		value := r.InputCacheHit
		hit = &value
	}
	return json.Marshal(wire{InputCacheHit: hit, InputCacheMiss: r.InputCacheMiss, Output: r.Output})
}

// Band is one conditional price. A band carries exactly one condition: a
// minimum prompt length or a UTC weekday/time window. The shipped snapshot
// never mixes the two.
type Band struct {
	Days            []string `json:"days,omitempty"`
	Start           int      `json:"start,omitempty"`
	End             int      `json:"end,omitempty"`
	MinPromptTokens int64    `json:"min_prompt_tokens,omitempty"`
	Flat            Rates    `json:"flat"`

	// Presence flags preserve the source object's exact key set. v1's policy
	// id hashes the band dictionaries as written (for example a time band has
	// an explicit "start": 0 and "days": null), and omitting a zero-valued key
	// would produce a different id.
	daysSet  bool
	startSet bool
	endSet   bool
	minSet   bool
}

// UnmarshalJSON records which optional band keys the source carried.
func (b *Band) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if value, ok := raw["days"]; ok {
		b.daysSet = true
		if string(bytes.TrimSpace(value)) != "null" {
			if err := json.Unmarshal(value, &b.Days); err != nil {
				return err
			}
		}
	}
	if value, ok := raw["start"]; ok {
		b.startSet = true
		if err := json.Unmarshal(value, &b.Start); err != nil {
			return err
		}
	}
	if value, ok := raw["end"]; ok {
		b.endSet = true
		if err := json.Unmarshal(value, &b.End); err != nil {
			return err
		}
	}
	if value, ok := raw["min_prompt_tokens"]; ok {
		b.minSet = true
		if err := json.Unmarshal(value, &b.MinPromptTokens); err != nil {
			return err
		}
	}
	if value, ok := raw["flat"]; ok {
		if err := json.Unmarshal(value, &b.Flat); err != nil {
			return err
		}
	} else {
		b.Flat = Rates{}
	}
	return nil
}

// MarshalJSON preserves the source key set, including explicit zero/null
// fields that omitempty would otherwise drop.
func (b Band) MarshalJSON() ([]byte, error) {
	out := map[string]any{"flat": b.Flat}
	if b.daysSet || b.Days != nil {
		out["days"] = b.Days
	}
	if b.minSet || b.MinPromptTokens != 0 {
		out["min_prompt_tokens"] = b.MinPromptTokens
	}
	if b.startSet || b.Start != 0 {
		out["start"] = b.Start
	}
	if b.endSet || b.End != 0 {
		out["end"] = b.End
	}
	return json.Marshal(out)
}

// Entry is one hub model's price declaration.
type Entry struct {
	Display       string  `json:"display"`
	Source        string  `json:"source"`
	Currency      string  `json:"currency"`
	Unit          float64 `json:"unit"`
	ORID          string  `json:"or_id,omitempty"`
	Via           string  `json:"via,omitempty"`
	InheritedFrom string  `json:"inherited_from,omitempty"`
	OverrideFrom  string  `json:"override_from,omitempty"`
	Flat          Rates   `json:"flat"`
	Bands         []Band  `json:"bands,omitempty"`
}

// Cost is the estimated equivalent-token cost of one usage row.
//
// Known=false never means zero: it means the model is unpriced. Disabled=true
// means the pricing master switch is off, which aggregates must report as
// "feature off", not as "missing a price". Rates and Band are diagnostic.
type Cost struct {
	Known         bool
	Disabled      bool
	CNY           float64
	Band          *int
	Rates         *Rates
	Unit          float64
	Currency      string
	USDCNY        float64
	ORID          string
	Source        string
	SourceAt      string
	SourceAtEpoch float64
	Via           string
	InheritedFrom string
	OverrideFrom  string
	Backfilled    bool
	Policy        string

	// Additive diagnostics used by the panel's recent-request hover card.
	BandNote   string
	ViaDerived bool
	Markup     float64
}

// Snapshot is one loaded price catalogue plus the local policy/markup state.
// It is safe for concurrent use.
type Snapshot struct {
	Meta   Meta             `json:"meta"`
	Models map[string]Entry `json:"models"`

	accountsDir string
	usageDir    string

	mu           sync.Mutex
	enabled      *bool
	enabledKey   string
	markup       map[string]float64
	markupKey    string
	overrides    map[string]string
	overridesKey string
	policies     map[string]Policy
	policiesKey  string
	timeline     []TimelineRow
	timelineKey  string

	liveMu   sync.RWMutex
	live     map[string]OpenRouterPrice
	liveNorm map[string][]string
	liveAt   time.Time
}

// Policy is one immutable price history row.
type Policy struct {
	ID            string  `json:"id"`
	Model         string  `json:"model"`
	ORID          string  `json:"or_id,omitempty"`
	Currency      string  `json:"currency,omitempty"`
	Unit          float64 `json:"unit,omitempty"`
	USDCNY        float64 `json:"usd_cny,omitempty"`
	Flat          Rates   `json:"flat"`
	Bands         []Band  `json:"bands,omitempty"`
	Via           string  `json:"via,omitempty"`
	InheritedFrom string  `json:"inherited_from,omitempty"`
	OverrideFrom  string  `json:"override_from,omitempty"`
	FirstSeen     float64 `json:"first_seen,omitempty"`

	raw map[string]any
}

// TimelineRow is the append-only "which policy was in force" record.
type TimelineRow struct {
	At       float64           `json:"at"`
	Policies map[string]string `json:"policies"`
}

var (
	active atomic.Pointer[Snapshot]

	loadMu      sync.Mutex
	loadedKey   string
	loadedMeta  Meta
	loadedModel map[string]Entry
	loadedErr   error
)

// Load returns the active snapshot. Precedence matches v1: WB_PRICING_FILE,
// <install>/pricing/pricing.json, then the embedded copy. A malformed external
// file falls back to the embedded copy instead of failing startup.
func Load(accountsDir, usageDir string) (*Snapshot, error) {
	doc, err := loadDocument()
	if err != nil {
		return nil, err
	}
	snapshot := &Snapshot{
		Meta:        doc.meta,
		Models:      doc.models,
		accountsDir: accountsDir,
		usageDir:    usageDir,
	}
	active.Store(snapshot)
	return snapshot, nil
}

// Active returns the most recently loaded snapshot. It may be nil before Load.
func Active() *Snapshot { return active.Load() }

type document struct {
	meta   Meta
	models map[string]Entry
}

func loadDocument() (document, error) {
	path := candidateFile()
	key := "embedded"
	if path != "" {
		if info, err := os.Stat(path); err == nil {
			key = fmt.Sprintf("%s|%d|%d", path, info.ModTime().UnixNano(), info.Size())
		}
	}

	loadMu.Lock()
	defer loadMu.Unlock()
	if key == loadedKey && (loadedModel != nil || loadedErr != nil) {
		return document{meta: loadedMeta, models: loadedModel}, loadedErr
	}

	if path != "" {
		if doc, err := readDocument(path); err == nil {
			loadedKey, loadedMeta, loadedModel, loadedErr = key, doc.meta, doc.models, nil
			return doc, nil
		}
	}
	doc, err := readEmbedded()
	if err != nil {
		loadedKey, loadedMeta, loadedModel, loadedErr = key, Meta{}, nil, err
		return document{}, err
	}
	loadedKey, loadedMeta, loadedModel, loadedErr = key, doc.meta, doc.models, nil
	return doc, nil
}

func candidateFile() string {
	if env := strings.TrimSpace(os.Getenv("WB_PRICING_FILE")); env != "" {
		return env
	}
	for _, base := range installDirs() {
		path := filepath.Join(base, "pricing", "pricing.json")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func installDirs() []string {
	var dirs []string
	if executable, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(executable))
	}
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, cwd)
	}
	return dirs
}

func readDocument(path string) (document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return document{}, err
	}
	return decodeDocument(data)
}

func decodeDocument(data []byte) (document, error) {
	var raw struct {
		Meta   Meta             `json:"meta"`
		Models map[string]Entry `json:"models"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&raw); err != nil {
		return document{}, err
	}
	if raw.Models == nil {
		return document{}, errors.New("pricing: snapshot has no models object")
	}
	return document{meta: raw.Meta, models: raw.Models}, nil
}

func normalizeEntry(entry Entry) Entry {
	if entry.Currency == "" {
		entry.Currency = "USD"
	}
	if entry.Unit <= 0 {
		entry.Unit = 1_000_000
	}
	if entry.Display == "" {
		entry.Display = entry.ORID
	}
	return entry
}

// USDCNY returns the snapshot conversion rate. A missing rate behaves like
// 1.0, exactly as v1's usd_cny() helper did.
func (s *Snapshot) USDCNY() float64 {
	if s == nil || s.Meta.USDCNY <= 0 {
		return 1
	}
	return s.Meta.USDCNY
}

// Fingerprint identifies the complete local pricing input set. Aggregates use
// it to invalidate checkpoints when a price, policy, timeline, markup or the
// master switch changes.
func (s *Snapshot) Fingerprint() string {
	if s == nil {
		return "nil"
	}
	var builder strings.Builder
	if s.Enabled() {
		builder.WriteString("on")
	} else {
		builder.WriteString("off")
	}
	for _, path := range s.localFiles() {
		builder.WriteByte('|')
		builder.WriteString(fileKey(path))
	}
	if path := candidateFile(); path != "" {
		builder.WriteString("|pricing:")
		builder.WriteString(fileKey(path))
	}
	return builder.String()
}

func (s *Snapshot) localFiles() []string {
	if s == nil {
		return nil
	}
	return []string{
		filepath.Join(s.usageDir, policiesName),
		filepath.Join(s.usageDir, timelineName),
		filepath.Join(s.usageDir, overridesName),
		filepath.Join(s.usageDir, markupName),
	}
}

func fileKey(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return path + ":missing"
	}
	return fmt.Sprintf("%s:%d:%d", path, info.ModTime().UnixNano(), info.Size())
}

// Enabled reports the pricing master switch. An absent or unreadable
// settings.json leaves pricing enabled, matching v1.
func (s *Snapshot) Enabled() bool {
	if s == nil {
		return false
	}
	path := filepath.Join(s.accountsDir, "settings.json")
	key := fileKey(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enabledKey == key && s.enabled != nil {
		return *s.enabled
	}
	value := true
	if data, err := os.ReadFile(path); err == nil {
		var settings map[string]any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&settings); err == nil {
			if raw, ok := settings["pricing_enabled"]; ok {
				flag, ok := raw.(bool)
				value = ok && flag
			}
		}
	}
	s.enabledKey = key
	s.enabled = &value
	return value
}

// MarkupFor returns the per-model calibration factor. Invalid, negative, zero
// or corrupt entries fall back to 1.0 and never scale a price.
func (s *Snapshot) MarkupFor(model string) float64 {
	if s == nil {
		return 1
	}
	path := filepath.Join(s.usageDir, markupName)
	key := fileKey(path)
	s.mu.Lock()
	if s.markupKey != key || s.markup == nil {
		s.markup = readMarkup(path)
		s.markupKey = key
	}
	factor := s.markup[model]
	s.mu.Unlock()
	if factor <= 0 {
		return 1
	}
	return factor
}

func readMarkup(path string) map[string]float64 {
	out := map[string]float64{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return out
	}
	for model, value := range raw {
		factor, ok := asFloat(value)
		model = strings.TrimSpace(model)
		if ok && model != "" && factor > 0 {
			out[model] = factor
		}
	}
	return out
}

// RuntimeOverrides returns the panel-written hub → OpenRouter id map.
func (s *Snapshot) RuntimeOverrides() map[string]string {
	if s == nil {
		return map[string]string{}
	}
	path := filepath.Join(s.usageDir, overridesName)
	key := fileKey(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overridesKey == key && s.overrides != nil {
		return cloneStrings(s.overrides)
	}
	out := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		var raw map[string]any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&raw); err == nil {
			for model, value := range raw {
				ref := strings.TrimSpace(asString(value))
				model = strings.TrimSpace(model)
				if model != "" && ref != "" {
					out[model] = ref
				}
			}
		}
	}
	s.overridesKey = key
	s.overrides = out
	return cloneStrings(out)
}

// SaveRuntimeOverride writes one panel mapping; an empty OpenRouter id removes
// it. Unknown keys in the existing file are preserved.
func (s *Snapshot) SaveRuntimeOverride(model, orID string) error {
	if s == nil {
		return errors.New("pricing: nil snapshot")
	}
	model = strings.TrimSpace(model)
	orID = strings.TrimSpace(orID)
	if model == "" {
		return errors.New("pricing: empty model")
	}
	path := filepath.Join(s.usageDir, overridesName)
	lock := fileLock(path)
	lock.Lock()
	defer lock.Unlock()

	current := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&current); err != nil {
			current = map[string]any{}
		}
	}
	if orID == "" {
		delete(current, model)
	} else {
		current[model] = orID
	}
	if err := writeJSONAtomic(path, current, 0o600); err != nil {
		return err
	}
	return nil
}

func cloneStrings(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// costForRowFromEntry prices one row against one entry and one CNY rate.
func costForRowFromEntry(row map[string]any, entry Entry, usdCNY float64) (Cost, *Rates, *int) {
	entry = normalizeEntry(entry)
	prompt := asFloatDefault(row["prompt_tokens"])
	cached := bestCachedTokens(row)
	if cached > prompt {
		cached = prompt
	}
	uncached := prompt - cached
	completion := asFloatDefault(row["completion_tokens"])
	unit := entry.Unit
	if unit <= 0 {
		unit = 1_000_000
	}
	factor := 1.0
	if strings.EqualFold(entry.Currency, "USD") {
		factor = usdCNY
		if factor <= 0 {
			factor = 1
		}
	}
	table, band := BandFor(entry, asFloatDefault(row["at"]), prompt)
	miss := table.InputCacheMiss
	hit := table.InputCacheHit
	if table.NullInputCacheHit {
		hit = miss
	}
	cny := (uncached*miss + cached*hit + completion*table.Output) / unit * factor
	cost := Cost{
		Known:         true,
		CNY:           cny,
		Band:          band,
		Unit:          unit,
		Currency:      entry.Currency,
		USDCNY:        usdCNY,
		ORID:          entry.ORID,
		Via:           entry.Via,
		InheritedFrom: entry.InheritedFrom,
		OverrideFrom:  entry.OverrideFrom,
	}
	rates := table
	return cost, &rates, band
}

func bestCachedTokens(row map[string]any) float64 {
	for _, value := range []any{
		nestedValue(row, "prompt_tokens_details", "cached_tokens"),
		row["prompt_cache_hit_tokens"],
		row["cache_read_input_tokens"],
		row["cached_tokens"],
		nestedValue(row, "input_tokens_details", "cached_tokens"),
		nestedValue(row, "completion_tokens_details", "cached_tokens"),
	} {
		if number, ok := asFloat(value); ok && number > 0 {
			return number
		}
	}
	return 0
}

func nestedValue(row map[string]any, key, nested string) any {
	value, ok := row[key].(map[string]any)
	if !ok {
		return nil
	}
	return value[nested]
}

// CostForRow prices one row against the active snapshot.
func CostForRow(row map[string]any) Cost { return Active().CostForRow(row) }

// CostForRow prices one row. The row's own policy reference wins; a row
// written before references existed falls back to the timeline, and a model
// with no policy at all is priced from the factory snapshot.
func (s *Snapshot) CostForRow(row map[string]any) Cost {
	if len(row) == 0 {
		return Cost{}
	}
	if s == nil || !s.Enabled() {
		return Cost{Disabled: true, Source: "builtin"}
	}
	model := asString(row["model"])
	pid := asString(row["cost_policy"])
	if pid == "codearts:unpriced" || pid == "raccoon:unpriced" {
		return Cost{Source: pid}
	}
	backfilled := false
	if pid == "" {
		pid, backfilled = s.policyRefAt(asFloatDefault(row["at"]), model)
	}
	markup := s.MarkupFor(model)
	if policy, ok := s.policyByID(pid); ok {
		entry := policyEntry(policy)
		usdCNY := policy.USDCNY
		if usdCNY <= 0 {
			usdCNY = s.USDCNY()
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
		s.fillVia(&cost, entry, model)
		return cost
	}
	entry, ok := s.Models[model]
	if !ok {
		cost := Cost{Source: "builtin", Backfilled: false}
		cost.Markup = markup
		return cost
	}
	cost, rates, band := costForRowFromEntry(row, entry, s.USDCNY())
	cost.CNY *= markup
	cost.Markup = markup
	cost.Rates = rates
	cost.Band = band
	cost.Source = "builtin"
	cost.SourceAt = ""
	cost.Backfilled = false
	cost.BandNote = BandNote(entry, band)
	s.fillVia(&cost, entry, model)
	return cost
}

func (s *Snapshot) fillVia(cost *Cost, entry Entry, model string) {
	via := strings.TrimSpace(entry.Via)
	if via == "variant" || via == "override" || via == "direct" {
		cost.Via = via
		cost.InheritedFrom = entry.InheritedFrom
		cost.OverrideFrom = entry.OverrideFrom
		return
	}
	mapping := s.RuntimeOverrides()[model]
	if mapping == "" {
		mapping = SourceOverrides[model]
	}
	if mapping != "" && mapping == entry.ORID {
		cost.Via = "override"
		cost.OverrideFrom = model
		cost.ViaDerived = true
		return
	}
	cost.Via = "direct"
	cost.ViaDerived = false
}

// policyEntry turns a stored policy back into the entry shape the cost engine
// expects. Policy history rows carry no display/source fields by design.
func policyEntry(policy Policy) Entry {
	return Entry{
		Display:       policy.Model,
		Source:        "openrouter",
		Currency:      policy.Currency,
		Unit:          policy.Unit,
		ORID:          policy.ORID,
		Via:           policy.Via,
		InheritedFrom: policy.InheritedFrom,
		OverrideFrom:  policy.OverrideFrom,
		Flat:          policy.Flat,
		Bands:         policy.Bands,
	}
}

// BandFor picks the conditional rate table for one row. Length bands use the
// tightest applicable threshold; time bands use the last applicable window,
// matching the source's non-overlapping ordering. No match returns the flat
// table and a nil index.
func BandFor(entry Entry, epoch float64, promptTokens float64) (Rates, *int) {
	var best *int
	bestThreshold := int64(-1)
	day, hhmm := utcParts(epoch)
	for index := range entry.Bands {
		band := &entry.Bands[index]
		if band.MinPromptTokens > 0 {
			if otherCondition(band) || promptTokens < float64(band.MinPromptTokens) {
				continue
			}
			if band.MinPromptTokens >= bestThreshold {
				bestThreshold = band.MinPromptTokens
				position := index
				best = &position
			}
			continue
		}
		if !timeCondition(band) || !bandApplies(band, day, hhmm) {
			continue
		}
		position := index
		best = &position
	}
	if best != nil {
		return entry.Bands[*best].Flat, best
	}
	return entry.Flat, nil
}

func otherCondition(band *Band) bool {
	if band == nil {
		return false
	}
	return len(band.Days) > 0 || band.Start != 0 || band.End != 0
}

func timeCondition(band *Band) bool {
	if band == nil {
		return false
	}
	return len(band.Days) > 0 || band.Start != 0 || band.End != 0
}

func bandApplies(band *Band, day string, hhmm int) bool {
	if band == nil {
		return false
	}
	if len(band.Days) > 0 {
		found := false
		for _, candidate := range band.Days {
			if strings.EqualFold(strings.TrimSpace(candidate), day) ||
				strings.EqualFold(strings.TrimSpace(candidate), weekdayLong(day)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	start := band.Start
	end := band.End
	if end == 0 {
		end = 2400
	}
	if start == end {
		// Equal start/end is not a valid window; never let it match silently.
		return false
	}
	if end < start {
		return hhmm >= start || hhmm < end
	}
	return hhmm >= start && hhmm < end
}

var weekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
var weekdayLongNames = []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}

func utcParts(epoch float64) (string, int) {
	when := time.Unix(int64(epoch), 0).UTC()
	day := weekdays[int(when.Weekday()+6)%7]
	return day, when.Hour()*100 + when.Minute()
}

func weekdayLong(day string) string {
	for index, short := range weekdays {
		if short == day {
			return weekdayLongNames[index]
		}
	}
	return ""
}

// BandNote explains why a row landed in a conditional band.
func BandNote(entry Entry, index *int) string {
	if index == nil || *index < 0 || *index >= len(entry.Bands) {
		return ""
	}
	band := entry.Bands[*index]
	if band.MinPromptTokens > 0 {
		return "输入长度 ≥ " + commaInt(band.MinPromptTokens) + " tokens"
	}
	window := hhmmString(band.Start) + "–" + hhmmString(bandEnd(band)) + " UTC"
	if len(band.Days) > 0 {
		names := make([]string, 0, len(band.Days))
		for _, day := range band.Days {
			if name := weekdayCN(day); name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			return "每周 " + strings.Join(names, "/") + " " + window
		}
	}
	return "每天 " + window
}

var weekdayCNNames = map[string]string{
	"mon": "周一", "tue": "周二", "wed": "周三", "thu": "周四",
	"fri": "周五", "sat": "周六", "sun": "周日",
}

func weekdayCN(day string) string { return weekdayCNNames[strings.ToLower(strings.TrimSpace(day))] }

func hhmmString(value int) string {
	return fmt.Sprintf("%02d:%02d", value/100, value%100)
}

func bandEnd(band Band) int {
	if band.End == 0 {
		return 2400
	}
	return band.End
}

func commaInt(value int64) string {
	text := strconv.FormatInt(value, 10)
	var out strings.Builder
	for index, char := range text {
		if index > 0 && (len(text)-index)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(char)
	}
	return out.String()
}

// policyID is the content hash v1 uses to de-duplicate policies. The rate is
// part of the hash because it decides the CNY figure just as much as the
// dollar prices do.
func policyID(model string, flat Rates, bands []Band, usdCNY float64) string {
	var builder strings.Builder
	builder.WriteString(`{"bands": `)
	if len(bands) == 0 {
		builder.WriteString("null")
	} else {
		builder.WriteByte('[')
		for index, band := range bands {
			if index > 0 {
				builder.WriteString(", ")
			}
			writePolicyBand(&builder, band)
		}
		builder.WriteByte(']')
	}
	builder.WriteString(`, "flat": `)
	writePolicyRates(&builder, flat)
	builder.WriteString(`, "model": `)
	builder.WriteString(pyQuote(model))
	builder.WriteString(`, "usd_cny": `)
	builder.WriteString(pyFloat(math.RoundToEven(usdCNY*1_000_000) / 1_000_000))
	builder.WriteByte('}')
	sum := sha1.Sum([]byte(builder.String()))
	return hex.EncodeToString(sum[:])[:12]
}

// writePolicyBand mirrors Python json.dumps(sort_keys=True) for one band.
func writePolicyBand(builder *strings.Builder, band Band) {
	builder.WriteByte('{')
	first := true
	field := func(name string) {
		if !first {
			builder.WriteString(", ")
		}
		first = false
		builder.WriteString(pyQuote(name))
		builder.WriteString(": ")
	}
	if band.daysSet || band.Days != nil {
		field("days")
		if band.Days == nil {
			builder.WriteString("null")
		} else {
			builder.WriteByte('[')
			for index, day := range band.Days {
				if index > 0 {
					builder.WriteString(", ")
				}
				builder.WriteString(pyQuote(day))
			}
			builder.WriteByte(']')
		}
	}
	if band.endSet || band.End != 0 {
		field("end")
		builder.WriteString(strconv.Itoa(band.End))
	}
	field("flat")
	writePolicyRates(builder, band.Flat)
	if band.minSet || band.MinPromptTokens != 0 {
		field("min_prompt_tokens")
		builder.WriteString(strconv.FormatInt(band.MinPromptTokens, 10))
	}
	if band.startSet || band.Start != 0 {
		field("start")
		builder.WriteString(strconv.Itoa(band.Start))
	}
	builder.WriteByte('}')
}

func writePolicyRates(builder *strings.Builder, rates Rates) {
	builder.WriteString(`{"input_cache_hit": `)
	if rates.NullInputCacheHit {
		builder.WriteString("null")
	} else {
		builder.WriteString(pyFloat(rates.InputCacheHit))
	}
	builder.WriteString(`, "input_cache_miss": `)
	builder.WriteString(pyFloat(rates.InputCacheMiss))
	builder.WriteString(`, "output": `)
	builder.WriteString(pyFloat(rates.Output))
	builder.WriteByte('}')
}

func pyQuote(value string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(buffer.String(), "\n")
}

// pyFloat formats a float the way Python's repr/json.dumps does: a decimal
// point for ordinary values, scientific notation below 1e-4 or at 1e16+.
func pyFloat(value float64) string {
	if value == 0 {
		if math.Signbit(value) {
			return "-0.0"
		}
		return "0.0"
	}
	absolute := math.Abs(value)
	exponent := int(math.Floor(math.Log10(absolute)))
	if exponent >= -4 && exponent < 16 {
		text := strconv.FormatFloat(value, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	text := strconv.FormatFloat(value, 'e', -1, 64)
	// Go renders e-05/e+16 exactly like Python's repr for these magnitudes.
	return text
}

func formatEpoch(value float64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// numeric helpers shared by the package.
func asFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func asFloatDefault(value any) float64 {
	number, _ := asFloat(value)
	return number
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

// sortModels returns model ids in stable lexical order (used by refreshers and
// tests that need deterministic output from map-backed snapshots).
func sortModels(models map[string]Entry) []string {
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
