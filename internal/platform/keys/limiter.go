package keys

import "sync"

// Limiter tracks in-flight requests per API-key id. A key with a non-positive
// limit is not tracked and therefore never blocks.
type Limiter struct {
	mu     sync.Mutex
	active map[string]int
	limits map[string]int
}

// NewLimiter returns an empty limiter.
func NewLimiter() *Limiter {
	return &Limiter{active: map[string]int{}, limits: map[string]int{}}
}

// SetLimit records the limit used by Acquire when the caller does not pass an
// explicit value. Passing 0 or a negative number removes the limit.
func (l *Limiter) SetLimit(entryID string, limit int) {
	if l == nil || entryID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 {
		delete(l.limits, entryID)
		return
	}
	l.limits[entryID] = limit
}

// Acquire takes one slot for entryID. The optional limit argument overrides
// SetLimit for this call. The returned release function is safe to call more
// than once and must be invoked on every path, including client aborts.
func (l *Limiter) Acquire(entryID string, explicitLimit ...int) (func(), bool) {
	if l == nil || entryID == "" {
		return func() {}, true
	}
	l.mu.Lock()
	limit := 0
	if len(explicitLimit) > 0 {
		limit = explicitLimit[0]
	} else {
		limit = l.limits[entryID]
	}
	if limit <= 0 {
		l.mu.Unlock()
		return func() {}, true
	}
	if l.active[entryID] >= limit {
		l.mu.Unlock()
		return func() {}, false
	}
	if l.active == nil {
		l.active = map[string]int{}
	}
	l.active[entryID]++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			current := l.active[entryID]
			if current <= 1 {
				delete(l.active, entryID)
			} else {
				l.active[entryID] = current - 1
			}
			l.mu.Unlock()
		})
	}, true
}

// InFlight returns the number of slots currently held for entryID.
func (l *Limiter) InFlight(entryID string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active[entryID]
}

// Reset drops all counters. It is intended for tests and shutdown.
func (l *Limiter) Reset() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.active = map[string]int{}
	l.limits = map[string]int{}
	l.mu.Unlock()
}
