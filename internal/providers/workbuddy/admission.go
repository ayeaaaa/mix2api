package workbuddy

import (
	"context"
	"sync"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// acquireChatAdmission waits for one provider-wide chat permit. The limit is
// read from the live config on every call, so a panel edit applies to new
// requests without rebuilding the provider. A canceled context releases the
// waiter without consuming a permit.
func (p *Provider) acquireChatAdmission(ctx context.Context, limit int) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if limit < minMaxConcurrent {
		limit = minMaxConcurrent
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.admissionMu.Lock()
		if p.admissionNotify == nil {
			p.admissionNotify = make(chan struct{})
		}
		if p.admissionActive < limit {
			p.admissionActive++
			p.admissionMu.Unlock()
			return nil
		}
		notify := p.admissionNotify
		p.admissionMu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notify:
		}
	}
}

// releaseChatAdmission returns one permit and wakes every waiter. The channel
// is replaced under the lock so a release cannot be missed between a waiter's
// capacity check and its select.
func (p *Provider) releaseChatAdmission() {
	if p == nil {
		return
	}
	p.admissionMu.Lock()
	if p.admissionActive > 0 {
		p.admissionActive--
	}
	if p.admissionNotify == nil {
		p.admissionNotify = make(chan struct{})
	} else {
		close(p.admissionNotify)
		p.admissionNotify = make(chan struct{})
	}
	p.admissionMu.Unlock()
}

// admittedChatStream keeps the provider permit for the stream's lifetime and
// releases it on Close, EOF, or any terminal read error. Close is idempotent;
// the API layer and tests may both close after EOF.
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
