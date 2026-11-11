package pricing

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	policiesName  = "pricing-policies.jsonl"
	timelineName  = "pricing-timeline.jsonl"
	overridesName = "pricing-overrides.json"
	markupName    = "pricing-markup.json"
)

var fileLocks sync.Map

func fileLock(path string) *sync.Mutex {
	value, _ := fileLocks.LoadOrStore(path, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		cleanup()
		return err
	}
	if _, err := temp.Write(bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))); err != nil {
		_ = temp.Close()
		cleanup()
		return err
	}
	if _, err := temp.Write([]byte("\n")); err != nil {
		_ = temp.Close()
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// policiesRef returns the immutable cached policy table. Callers must not
// mutate the returned map or its values; use Policies for a defensive copy.
func (s *Snapshot) policiesRef() map[string]Policy {
	if s == nil {
		return nil
	}
	path := filepath.Join(s.usageDir, policiesName)
	key := fileKey(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.policiesKey == key && s.policies != nil {
		return s.policies
	}
	out := map[string]Policy{}
	for _, row := range readJSONLLines(path) {
		data, err := json.Marshal(row)
		if err != nil {
			continue
		}
		var policy Policy
		if err := json.Unmarshal(data, &policy); err != nil || policy.ID == "" {
			continue
		}
		policy.raw = row
		out[policy.ID] = policy
	}
	s.policiesKey = key
	s.policies = out
	return out
}

// Policies returns the immutable-looking policy table, de-duplicated by id.
func (s *Snapshot) Policies() map[string]Policy {
	return clonePolicies(s.policiesRef())
}

func clonePolicies(source map[string]Policy) map[string]Policy {
	out := make(map[string]Policy, len(source))
	for id, policy := range source {
		out[id] = policy
	}
	return out
}

// policyByID uses the cached table without exposing a mutable copy.
func (s *Snapshot) policyByID(id string) (Policy, bool) {
	if s == nil || id == "" {
		return Policy{}, false
	}
	policy, ok := s.policiesRef()[id]
	return policy, ok
}

// timelineRef returns the immutable cached timeline. Callers must not mutate
// the returned rows or their policy maps; use Timeline for a defensive copy.
func (s *Snapshot) timelineRef() []TimelineRow {
	if s == nil {
		return nil
	}
	path := filepath.Join(s.usageDir, timelineName)
	key := fileKey(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timelineKey == key && s.timeline != nil {
		return s.timeline
	}
	rows := make([]TimelineRow, 0)
	for _, row := range readJSONLLines(path) {
		data, err := json.Marshal(row)
		if err != nil {
			continue
		}
		var item TimelineRow
		if err := json.Unmarshal(data, &item); err != nil || item.Policies == nil {
			continue
		}
		rows = append(rows, item)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At < rows[j].At })
	s.timelineKey = key
	s.timeline = rows
	return rows
}

// Timeline returns the policy timeline, oldest first. The returned value is a
// defensive copy; hot pricing paths use timelineRef instead.
func (s *Snapshot) Timeline() []TimelineRow {
	return cloneTimeline(s.timelineRef())
}

func cloneTimeline(source []TimelineRow) []TimelineRow {
	out := make([]TimelineRow, 0, len(source))
	for _, row := range source {
		policies := make(map[string]string, len(row.Policies))
		for model, id := range row.Policies {
			policies[model] = id
		}
		out = append(out, TimelineRow{At: row.At, Policies: policies})
	}
	return out
}

func readJSONLLines(path string) []map[string]any {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	rows := make([]map[string]any, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		var row map[string]any
		if err := decoder.Decode(&row); err != nil || row == nil {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// currentAssignment returns the last timeline row.
func (s *Snapshot) currentAssignment() (float64, map[string]string) {
	timeline := s.timelineRef()
	if len(timeline) == 0 {
		return 0, map[string]string{}
	}
	last := timeline[len(timeline)-1]
	assignment := make(map[string]string, len(last.Policies))
	for model, id := range last.Policies {
		assignment[model] = id
	}
	return last.At, assignment
}

// CurrentPolicyID returns the policy in force for model. A model without a
// policy is priced on demand from the last successful OpenRouter reading; no
// network call is ever made here. With pricing disabled nothing is minted.
func (s *Snapshot) CurrentPolicyID(model string) string {
	if s == nil || !s.Enabled() {
		return ""
	}
	_, assignment := s.currentAssignment()
	if id := assignment[model]; id != "" {
		return id
	}
	id, _ := s.EnsurePolicy(model)
	return id
}

// CurrentPolicyID is the package-level form using the active snapshot.
func CurrentPolicyID(model string) string { return Active().CurrentPolicyID(model) }

// EnsurePolicy registers an on-demand policy only when the model resolves
// against the last successful OpenRouter index. Repeated calls are idempotent
// because policies are keyed by a content hash.
func (s *Snapshot) EnsurePolicy(model string) (string, error) {
	if s == nil || !s.Enabled() {
		return "", nil
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", nil
	}
	models, byNorm := s.liveIndex()
	if len(models) == 0 {
		return "", nil
	}
	inherited := ""
	if base, ok := inheritedFrom(model, models, byNorm); ok {
		inherited = base
	}
	ref, viaOverride := s.resolve(model, models, byNorm)
	if ref == "" {
		return "", nil
	}
	entry := entryFor(model, ref, models, inherited, viaOverride)
	doc := &Snapshot{
		Meta:   Meta{BaseCurrency: "CNY", USDCNY: s.USDCNY()},
		Models: map[string]Entry{model: entry},
	}
	added, _, assignment := s.recordPolicies(doc, float64(timeNowUnix()), true)
	if added > 0 {
		s.logf("按需补价：%s → %s（策略 %s）", model, ref, assignment[model])
	}
	return assignment[model], nil
}

// EnsurePolicy is the package-level form using the active snapshot.
func EnsurePolicy(model string) (string, error) { return Active().EnsurePolicy(model) }

// RecordPolicies appends a fetched snapshot to the policy table and timeline.
// It returns (new policy count, timeline changed, model→policy assignment).
// Existing policy rows are never rewritten.
func (s *Snapshot) RecordPolicies(doc *Snapshot, at float64, merge bool) (int, bool, map[string]string) {
	if s == nil || doc == nil {
		return 0, false, map[string]string{}
	}
	return s.recordPolicies(doc, at, merge)
}

func (s *Snapshot) recordPolicies(doc *Snapshot, at float64, merge bool) (int, bool, map[string]string) {
	lock := fileLock(filepath.Join(s.usageDir, policiesName))
	lock.Lock()
	defer lock.Unlock()

	usd := 7.10
	if doc.Meta.USDCNY > 0 {
		usd = doc.Meta.USDCNY
	}
	known := s.Policies()
	fresh := make([]Policy, 0)
	_, previous := s.currentAssignment()
	assignment := map[string]string{}
	if merge {
		for model, id := range previous {
			assignment[model] = id
		}
	}
	for model, entry := range doc.Models {
		id := policyID(model, entry.Flat, entry.Bands, usd)
		assignment[model] = id
		if _, ok := known[id]; ok {
			continue
		}
		policy := Policy{
			ID:            id,
			Model:         model,
			ORID:          entry.ORID,
			Currency:      defaultString(entry.Currency, "USD"),
			Unit:          entry.Unit,
			USDCNY:        usd,
			Flat:          entry.Flat,
			Bands:         entry.Bands,
			Via:           defaultString(entry.Via, "direct"),
			InheritedFrom: entry.InheritedFrom,
			OverrideFrom:  entry.OverrideFrom,
			FirstSeen:     at,
		}
		if policy.Unit <= 0 {
			policy.Unit = 1_000_000
		}
		known[id] = policy
		fresh = append(fresh, policy)
	}
	if len(fresh) > 0 {
		if err := appendJSONL(filepath.Join(s.usageDir, policiesName), policiesToRows(fresh)); err != nil {
			return 0, false, assignment
		}
		s.mu.Lock()
		s.policiesKey = ""
		s.policies = nil
		s.mu.Unlock()
	}
	changed := false
	if len(assignment) > 0 && !sameAssignment(assignment, previous) {
		row := map[string]any{"at": at, "policies": assignment}
		if err := appendJSONL(filepath.Join(s.usageDir, timelineName), []map[string]any{row}); err != nil {
			return len(fresh), false, assignment
		}
		s.mu.Lock()
		s.timelineKey = ""
		s.timeline = nil
		s.mu.Unlock()
		changed = true
	}
	return len(fresh), changed, assignment
}

func policiesToRows(policies []Policy) []map[string]any {
	rows := make([]map[string]any, 0, len(policies))
	for _, policy := range policies {
		if policy.raw != nil {
			row := make(map[string]any, len(policy.raw)+2)
			for key, value := range policy.raw {
				row[key] = value
			}
			row["id"] = policy.ID
			row["model"] = policy.Model
			rows = append(rows, row)
			continue
		}
		data, _ := json.Marshal(policy)
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var row map[string]any
		if err := decoder.Decode(&row); err == nil {
			rows = append(rows, row)
		}
	}
	return rows
}

func appendJSONL(path string, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, row := range rows {
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(row); err != nil {
			return err
		}
		if _, err := file.Write(buffer.Bytes()); err != nil {
			return err
		}
	}
	return file.Sync()
}

func sameAssignment(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// policyRefAt resolves which policy a moment in the past should use, matching
// v1's backfill rules. An empty timeline yields ("", false).
func (s *Snapshot) policyRefAt(epoch float64, model string) (string, bool) {
	return policyRefAtTimeline(s.timelineRef(), epoch, model)
}

// PolicyLabel formats a policy's first-seen wall clock like v1's panel.
func PolicyLabel(policy Policy) string {
	if policy.FirstSeen <= 0 {
		return ""
	}
	return formatLocal(policy.FirstSeen)
}

// PrunePolicies drops policies nothing references. Kept are the current
// assignment, every id a usage row references, caller pins, and the newest
// policy of a model that would otherwise lose its last row.
func (s *Snapshot) PrunePolicies(protect map[string]bool) []string {
	if s == nil {
		return nil
	}
	lock := fileLock(filepath.Join(s.usageDir, policiesName))
	lock.Lock()
	defer lock.Unlock()
	policies := s.Policies()
	if len(policies) == 0 {
		return nil
	}
	_, assignment := s.currentAssignment()
	keep := map[string]bool{}
	for _, id := range assignment {
		keep[id] = true
	}
	for id := range protect {
		keep[id] = true
	}
	for id := range referencedPolicyIDs(filepath.Join(s.usageDir, "usage.jsonl")) {
		keep[id] = true
	}
	byModel := map[string][]string{}
	for id, policy := range policies {
		byModel[policy.Model] = append(byModel[policy.Model], id)
	}
	for _, ids := range byModel {
		found := false
		for _, id := range ids {
			if keep[id] {
				found = true
				break
			}
		}
		if found {
			continue
		}
		newest := ids[0]
		for _, id := range ids[1:] {
			if policies[id].FirstSeen > policies[newest].FirstSeen {
				newest = id
			}
		}
		keep[newest] = true
	}
	removed := make([]string, 0)
	for id := range policies {
		if !keep[id] {
			removed = append(removed, id)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	sort.Strings(removed)
	temp := filepath.Join(s.usageDir, policiesName+".tmp")
	file, err := os.Create(temp)
	if err != nil {
		return nil
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(temp)
		}
	}()
	writer := bufio.NewWriter(file)
	for _, id := range sortedPolicyIDs(policies, keep) {
		policy := policies[id]
		var data []byte
		if policy.raw != nil {
			data, _ = json.Marshal(policy.raw)
		} else {
			data, _ = json.Marshal(policy)
		}
		if _, err := writer.Write(append(data, '\n')); err != nil {
			return nil
		}
	}
	if err := writer.Flush(); err != nil {
		return nil
	}
	if err := file.Close(); err != nil {
		return nil
	}
	if err := os.Rename(temp, filepath.Join(s.usageDir, policiesName)); err != nil {
		return nil
	}
	success = true
	s.mu.Lock()
	s.policiesKey = ""
	s.policies = nil
	s.mu.Unlock()
	return removed
}

func sortedPolicyIDs(policies map[string]Policy, keep map[string]bool) []string {
	ids := make([]string, 0, len(policies))
	for id := range policies {
		if keep[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func referencedPolicyIDs(path string) map[string]bool {
	out := map[string]bool{}
	file, err := os.Open(path)
	if err != nil {
		return out
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"cost_policy"`)) {
			continue
		}
		var row map[string]any
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&row); err != nil {
			continue
		}
		if id := asString(row["cost_policy"]); id != "" {
			out[id] = true
		}
	}
	return out
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func timeNowUnix() int64 { return nowUnix() }

var nowUnix = func() int64 { return timeNow().Unix() }

var timeNow = func() time.Time { return time.Now() }

func formatLocal(epoch float64) string {
	return time.Unix(int64(epoch), 0).Local().Format("2006-01-02 15:04")
}

// logf is deliberately a no-op hook: the refresher installs a logger; the
// pricing package must not depend on any logging framework.
func (s *Snapshot) logf(format string, args ...any) { _ = fmt.Sprintf(format, args...) }

var errNoPolicies = errors.New("pricing: no policies")
