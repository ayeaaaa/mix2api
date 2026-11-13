package workbuddy

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// defaultAdmissionWait mirrors v1's WB_CHAT_SLOT_WAIT default. A request
	// that cannot take a chat permit in this window returns busy/503.
	defaultAdmissionWait = 30 * time.Second
)

// ErrAdmissionBusy means the process-wide chat limiter could not free a permit
// inside its bounded wait. Provider.OpenChat converts it to HTTP 503.
var ErrAdmissionBusy = errors.New("workbuddy: chat admission wait timed out")

// Admission is one process-wide WorkBuddy chat limiter. app.Build creates one
// instance and calls Provider.SetAdmission on both realm providers, so intl,
// cn, and the opt-in fallback share the v1 global chat ceiling instead of each
// realm getting its own cap.
//
// Limit growth wakes waiters immediately. Shrinking is deliberately ignored:
// v1's slot ceiling only grows, and revoking an already-issued permit would
// break the release accounting.
type Admission struct {
	mu     sync.Mutex
	limit  int
	active int
	wait   time.Duration
	notify chan struct{}
}

// NewAdmission returns a limiter with the v1 30-second bounded wait.
func NewAdmission(limit int) *Admission {
	return NewAdmissionWithWait(limit, defaultAdmissionWait)
}

// NewAdmissionWithWait is NewAdmission with an explicit wait budget. Tests use
// a short budget to exercise saturation without sleeping for 30 seconds.
func NewAdmissionWithWait(limit int, wait time.Duration) *Admission {
	if limit < minMaxConcurrent {
		limit = minMaxConcurrent
	}
	if wait <= 0 {
		wait = defaultAdmissionWait
	}
	return &Admission{
		limit:  limit,
		wait:   wait,
		notify: make(chan struct{}),
	}
}

// Limit returns the current ceiling.
func (a *Admission) Limit() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.limit
}

// InFlight returns the number of permits currently held.
func (a *Admission) InFlight() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active
}

// SetLimit raises the ceiling and wakes every waiter. Values below the current
// limit are ignored, matching v1's grow-only slot sizing.
func (a *Admission) SetLimit(limit int) {
	if a == nil {
		return
	}
	if limit < minMaxConcurrent {
		limit = minMaxConcurrent
	}
	a.mu.Lock()
	if limit > a.limit {
		a.limit = limit
		a.wakeLocked()
	}
	a.mu.Unlock()
}

// Acquire waits up to the configured budget for one permit. Context
// cancellation wins over the budget so a disconnected client does not keep a
// waiter alive.
func (a *Admission) Acquire(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(a.wait)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.mu.Lock()
		if a.notify == nil {
			a.notify = make(chan struct{})
		}
		if a.active < a.limit {
			a.active++
			a.mu.Unlock()
			return nil
		}
		notify := a.notify
		a.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notify:
		case <-timer.C:
			return ErrAdmissionBusy
		}
	}
}

// Release returns one permit and wakes all waiters.
func (a *Admission) Release() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.active > 0 {
		a.active--
	}
	a.wakeLocked()
	a.mu.Unlock()
}

func (a *Admission) wakeLocked() {
	if a.notify == nil {
		a.notify = make(chan struct{})
		return
	}
	close(a.notify)
	a.notify = make(chan struct{})
}

// SetAdmission replaces this provider's chat limiter. app.Build passes the same
// *Admission to both realm instances; nil restores a provider-local limiter.
func (p *Provider) SetAdmission(admission *Admission) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.admission = admission
	p.mu.Unlock()
}

func (p *Provider) currentAdmission() *Admission {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	admission := p.admission
	p.mu.RUnlock()
	if admission != nil {
		return admission
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.admission == nil {
		p.admission = NewAdmission(p.cfg.MaxConcurrent)
	}
	return p.admission
}

// ChatAdmission returns the limiter this provider currently uses. App
// composition tests use identity to prove intl/cn share one process-wide cap.
func (p *Provider) ChatAdmission() *Admission {
	return p.currentAdmission()
}

// admittedChatStream keeps a shared permit for the stream lifetime and releases
// it on Close, EOF, or any terminal read error. Close is idempotent.
type admittedChatStream struct {
	providers.ChatStream
	release func()
	once    sync.Once
}

func newAdmittedChatStream(stream providers.ChatStream, release func()) providers.ChatStream {
	if stream == nil {
		if release != nil {
			release()
		}
		return nil
	}
	return &admittedChatStream{ChatStream: stream, release: release}
}

func (s *admittedChatStream) Next() ([]byte, error) {
	frame, err := s.ChatStream.Next()
	if err != nil {
		s.releaseOnce()
	}
	return frame, err
}

func (s *admittedChatStream) Close() error {
	err := s.ChatStream.Close()
	s.releaseOnce()
	return err
}

// StreamUsage forwards the WorkBuddy-specific usage extension through the
// admission wrapper so usage writers do not need to know about the permit.
func (s *admittedChatStream) StreamUsage() StreamUsage {
	if reporter, ok := s.ChatStream.(StreamUsageReporter); ok {
		return reporter.StreamUsage()
	}
	return StreamUsage{}
}

func (s *admittedChatStream) releaseOnce() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.release != nil {
			s.release()
		}
	})
}
