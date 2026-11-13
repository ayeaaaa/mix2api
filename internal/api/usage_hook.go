package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// usageTracker wraps a provider chat stream to capture TTFT and to forward the
// optional UsageSnapshotReporter. It never changes a frame.
type usageTracker struct {
	stream providers.ChatStream
	start  time.Time
	now    func() time.Time

	mu    sync.Mutex
	first time.Time
}

func newUsageTracker(stream providers.ChatStream, start time.Time) *usageTracker {
	if stream == nil {
		return nil
	}
	return &usageTracker{stream: stream, start: start, now: time.Now}
}

func (t *usageTracker) Next() ([]byte, error) {
	if t == nil || t.stream == nil {
		return nil, nil
	}
	frame, err := t.stream.Next()
	if len(frame) > 0 {
		t.mu.Lock()
		if t.first.IsZero() {
			t.first = t.now()
		}
		t.mu.Unlock()
	}
	return frame, err
}

func (t *usageTracker) Close() error {
	if t == nil || t.stream == nil {
		return nil
	}
	return t.stream.Close()
}

// Snapshot forwards the provider's neutral usage hook. A provider without the
// hook yields an empty snapshot; the API then records the provider realm with
// an empty account.
func (t *usageTracker) Snapshot() providers.UsageSnapshot {
	if t == nil || t.stream == nil {
		return providers.UsageSnapshot{}
	}
	if reporter, ok := t.stream.(providers.UsageSnapshotReporter); ok {
		return reporter.UsageSnapshot()
	}
	return providers.UsageSnapshot{}
}

// UsageSnapshot makes a tracker itself a reporter so nested wrappers and tests
// see the same attribution.
func (t *usageTracker) UsageSnapshot() providers.UsageSnapshot {
	return t.Snapshot()
}

// Timing returns elapsed, TTFT and generation milliseconds. TTFT is only
// reported for streams; a non-stream aggregation has no first-frame boundary.
func (t *usageTracker) Timing(stream bool) (int64, int64, int64) {
	if t == nil {
		return 0, 0, 0
	}
	t.mu.Lock()
	first := t.first
	t.mu.Unlock()
	end := t.now()
	elapsed := end.Sub(t.start)
	if elapsed < 0 {
		elapsed = 0
	}
	if !stream || first.IsZero() {
		return elapsed.Milliseconds(), 0, 0
	}
	ttft := first.Sub(t.start)
	if ttft < 0 {
		ttft = 0
	}
	gen := end.Sub(first)
	if gen < 0 {
		gen = 0
	}
	return elapsed.Milliseconds(), ttft.Milliseconds(), gen.Milliseconds()
}

// ElapsedMS reports wall time since the request started.
func (t *usageTracker) ElapsedMS() int64 {
	if t == nil {
		return 0
	}
	elapsed := t.now().Sub(t.start)
	if elapsed < 0 {
		return 0
	}
	return elapsed.Milliseconds()
}

// FirstChunkAt reports the first upstream frame time for v1's chat response.
func (t *usageTracker) FirstChunkAt() time.Time {
	if t == nil {
		return time.Time{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.first
}

// usageMapFromSnapshot converts a provider snapshot into the raw v1 usage
// block the recorder understands. Nil means the provider omitted usage.
func usageMapFromSnapshot(snapshot providers.UsageSnapshot) map[string]any {
	out := map[string]any{}
	if snapshot.PromptTokens > 0 {
		out["prompt_tokens"] = snapshot.PromptTokens
	}
	if snapshot.CompletionTokens > 0 {
		out["completion_tokens"] = snapshot.CompletionTokens
	}
	if snapshot.TotalTokens > 0 {
		out["total_tokens"] = snapshot.TotalTokens
	}
	if snapshot.ReasoningTokens > 0 {
		out["reasoning_tokens"] = snapshot.ReasoningTokens
	}
	if snapshot.CachedTokens > 0 {
		out["cached_tokens"] = snapshot.CachedTokens
	}
	if snapshot.CreditKnown {
		out["credit"] = snapshot.Credit
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyTracker fills attribution/timing/usage on an observation from a tracked
// stream. Existing semantic values (a protocol's own usage map) win.
func applyTracker(observation *ProtocolObservation, stream providers.ChatStream) {
	if observation == nil || stream == nil {
		return
	}
	tracker, ok := stream.(*usageTracker)
	if !ok {
		if reporter, ok := stream.(providers.UsageSnapshotReporter); ok {
			applySnapshot(observation, reporter.UsageSnapshot())
		}
		return
	}
	applySnapshot(observation, tracker.Snapshot())
	observation.ElapsedMS, observation.TTFTMS, observation.GenMS = tracker.Timing(observation.Stream)
}

func applySnapshot(observation *ProtocolObservation, snapshot providers.UsageSnapshot) {
	if observation == nil {
		return
	}
	if model := strings.TrimSpace(snapshot.Model); model != "" && model != observation.Model {
		// The protocol already knows which model the client requested; a
		// provider-reported alias may legitimately differ. Only derive
		// RequestedModel when no protocol supplied one, and never overwrite a
		// canonical exit-qualified value with the alias.
		if observation.RequestedModel == "" && observation.Model != "" {
			observation.RequestedModel = observation.Model
		}
		observation.Model = model
	}
	if snapshot.Free {
		observation.Free = true
	}
	if policy := strings.TrimSpace(snapshot.CostPolicy); policy != "" {
		observation.CostPolicy = policy
	}
	if observation.Realm == "" {
		observation.Realm = snapshot.Realm
	}
	if observation.Account == "" {
		observation.Account = snapshot.Account
	}
	if observation.Kind == "" {
		observation.Kind = snapshot.Kind
	}
	if observation.Usage == nil {
		observation.Usage = usageMapFromSnapshot(snapshot)
	}
}

// keyIDFromRequest returns the settings id of the API key that paid for r.
// Panel sessions and unauthenticated installs carry no key id.
func keyIDFromRequest(r *http.Request) string {
	entry := KeyEntryFromContext(r)
	if entry == nil {
		return ""
	}
	return entry.ID
}

// attributedStream supplies the provider realm when a stream implementation
// does not report one itself. Existing reporter values win; only empty fields
// are filled.
type attributedStream struct {
	providers.ChatStream
	realm string
	kind  string
}

func withAttribution(stream providers.ChatStream, realm string) providers.ChatStream {
	if stream == nil {
		return nil
	}
	return &attributedStream{ChatStream: stream, realm: realm, kind: "chat"}
}

func (s *attributedStream) UsageSnapshot() providers.UsageSnapshot {
	if s == nil || s.ChatStream == nil {
		return providers.UsageSnapshot{}
	}
	snapshot := providers.UsageSnapshot{}
	if reporter, ok := s.ChatStream.(providers.UsageSnapshotReporter); ok {
		snapshot = reporter.UsageSnapshot()
	}
	if snapshot.Realm == "" {
		snapshot.Realm = s.realm
	}
	if snapshot.Kind == "" {
		snapshot.Kind = s.kind
	}
	return snapshot
}

// snapshotStream wraps a synthetic stream (for example a completed local web
// round) with attribution the API can record.
type snapshotStream struct {
	providers.ChatStream
	snapshot providers.UsageSnapshot
}

func newSnapshotStream(stream providers.ChatStream, snapshot providers.UsageSnapshot) providers.ChatStream {
	if stream == nil {
		return nil
	}
	return &snapshotStream{ChatStream: stream, snapshot: snapshot}
}

func (s *snapshotStream) UsageSnapshot() providers.UsageSnapshot {
	if s == nil {
		return providers.UsageSnapshot{}
	}
	return s.snapshot
}

func snapshotFromChat(chat map[string]any, realm, model string) providers.UsageSnapshot {
	snapshot := providers.UsageSnapshot{Realm: realm, Model: model, Kind: "chat"}
	if chat == nil {
		return snapshot
	}
	if usage, ok := chat["usage"].(map[string]any); ok {
		snapshot.PromptTokens = int64(apiUsageNumber(usage["prompt_tokens"]))
		snapshot.CompletionTokens = int64(apiUsageNumber(usage["completion_tokens"]))
		snapshot.TotalTokens = int64(apiUsageNumber(usage["total_tokens"]))
		snapshot.CachedTokens = int64(apiUsageNumber(usage["cached_tokens"]))
		snapshot.ReasoningTokens = int64(apiUsageNumber(usage["reasoning_tokens"]))
		if credit, ok := apiUsageFloat(usage["credit"]); ok {
			snapshot.Credit = credit
			snapshot.CreditKnown = true
		}
	}
	if snapshot.TotalTokens == 0 {
		snapshot.TotalTokens = snapshot.PromptTokens + snapshot.CompletionTokens
	}
	return snapshot
}

func apiUsageNumber(value any) float64 {
	number, _ := apiUsageFloat(value)
	return number
}

func apiUsageFloat(value any) (float64, bool) {
	switch typed := value.(type) {
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
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	}
	return 0, false
}

var (
	_ providers.UsageSnapshotReporter = (*usageTracker)(nil)
	_ providers.UsageSnapshotReporter = (*snapshotStream)(nil)
	_ providers.UsageSnapshotReporter = (*attributedStream)(nil)
)
