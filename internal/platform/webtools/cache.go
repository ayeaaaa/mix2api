package webtools

import (
	"context"
	"sync"
	"time"
)

type cacheEntry struct {
	expires time.Time
	value   string
}

type ttlCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	max     int
}

func newTTLCache(max int) *ttlCache {
	return &ttlCache{entries: map[string]cacheEntry{}, max: max}
}

func (c *ttlCache) get(key string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !entry.expires.After(now) {
		delete(c.entries, key)
		return "", false
	}
	return entry.value, true
}

func (c *ttlCache) put(key, value string, ttl time.Duration, now time.Time) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{expires: now.Add(ttl), value: value}
	for len(c.entries) > c.max {
		var oldestKey string
		var oldest time.Time
		for candidate, entry := range c.entries {
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = candidate, entry.expires
			}
		}
		delete(c.entries, oldestKey)
	}
}

func (c *ttlCache) clear() {
	c.mu.Lock()
	c.entries = map[string]cacheEntry{}
	c.mu.Unlock()
}

type flight struct {
	done chan struct{}
}

func (s *Service) waitFlight(ctx context.Context, key string) (func(), bool) {
	s.flightMu.Lock()
	if existing, ok := s.flights[key]; ok {
		s.flightMu.Unlock()
		select {
		case <-existing.done:
		case <-ctx.Done():
		}
		return nil, false
	}
	current := &flight{done: make(chan struct{})}
	s.flights[key] = current
	s.flightMu.Unlock()
	return func() {
		s.flightMu.Lock()
		delete(s.flights, key)
		s.flightMu.Unlock()
		close(current.done)
	}, true
}
