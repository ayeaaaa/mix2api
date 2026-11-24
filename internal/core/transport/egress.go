package transport

import (
	"hash/fnv"
	"sync"
	"time"
)

// EgressTracker is the in-process per-egress transient-failure backoff used by
// the panel's "egress_backoff_enabled" setting. It is keyed by the normalized
// proxy URL ("" means direct). Only transient failures (transport errors and
// 502/503/504) are recorded; any successful request clears the entry.
type EgressTracker struct {
	mu       sync.Mutex
	now      func() time.Time
	until    map[string]time.Time
	step     map[string]int
	last     map[string]time.Time
	maxItems int
}

// NewEgressTracker returns a tracker with the v1 backoff shape: 1s, 2s, 4s …
// capped at 15s, with bounded state.
func NewEgressTracker() *EgressTracker {
	return &EgressTracker{
		now:      time.Now,
		until:    make(map[string]time.Time),
		step:     make(map[string]int),
		last:     make(map[string]time.Time),
		maxItems: 4096,
	}
}

// DefaultEgress is the process-wide tracker consumed by provider pools.
var DefaultEgress = NewEgressTracker()

// Allowed reports whether an egress may currently receive requests.
func (t *EgressTracker) Allowed(proxyURL string) bool {
	if t == nil {
		return true
	}
	key := normalizeProxyURL(proxyURL)
	t.mu.Lock()
	defer t.mu.Unlock()
	deadline, ok := t.until[key]
	if !ok {
		return true
	}
	now := t.now()
	if now.Before(deadline) {
		return false
	}
	delete(t.until, key)
	// Keep the failure count through a short burst so consecutive failures
	// keep escalating. A long quiet period resets the ladder.
	if last, ok := t.last[key]; !ok || now.Sub(last) > time.Minute {
		delete(t.step, key)
		delete(t.last, key)
	}
	return true
}

// Success clears the backoff for an egress after any successful request.
func (t *EgressTracker) Success(proxyURL string) {
	if t == nil {
		return
	}
	key := normalizeProxyURL(proxyURL)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.until, key)
	delete(t.step, key)
	delete(t.last, key)
}

// Transient records one transient failure and extends the egress backoff.
func (t *EgressTracker) Transient(proxyURL string) {
	if t == nil {
		return
	}
	key := normalizeProxyURL(proxyURL)
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.until) >= t.maxItems {
		t.sweepLocked()
		if len(t.until) >= t.maxItems {
			for k := range t.until {
				delete(t.until, k)
				delete(t.step, k)
				delete(t.last, k)
				break
			}
		}
	}
	next := t.step[key] + 1
	t.step[key] = next
	now := t.now()
	t.last[key] = now
	delay := time.Second << uint(minInt(next-1, 20))
	if delay > 15*time.Second {
		delay = 15 * time.Second
	}
	delay += egressJitter(key, next)
	if delay > 15*time.Second {
		delay = 15 * time.Second
	}
	t.until[key] = now.Add(delay)
}

func (t *EgressTracker) sweepLocked() {
	now := t.now()
	for key, deadline := range t.until {
		if !now.Before(deadline) {
			delete(t.until, key)
			delete(t.step, key)
			delete(t.last, key)
		}
	}
}

func egressJitter(key string, step int) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{byte(step), byte(step >> 8), byte(step >> 16), byte(step >> 24)})
	return time.Duration(h.Sum32()%251) * time.Millisecond
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Package-level convenience wrappers used by provider pools.
func EgressAllowed(proxyURL string) bool  { return DefaultEgress.Allowed(proxyURL) }
func NoteEgressSuccess(proxyURL string)   { DefaultEgress.Success(proxyURL) }
func NoteEgressTransient(proxyURL string) { DefaultEgress.Transient(proxyURL) }
