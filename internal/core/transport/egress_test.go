package transport

import (
	"strconv"
	"testing"
	"time"
)

func TestEgressTrackerBackoffAndReset(t *testing.T) {
	tracker := NewEgressTracker()
	now := time.Unix(1000, 0)
	tracker.now = func() time.Time { return now }
	if !tracker.Allowed("http://proxy:1") {
		t.Fatal("fresh tracker blocked")
	}
	tracker.Transient("http://proxy:1")
	if tracker.Allowed("http://proxy:1") {
		t.Fatal("transient failure did not block")
	}
	now = now.Add(1300 * time.Millisecond)
	if !tracker.Allowed("http://proxy:1") {
		t.Fatal("1s backoff did not expire")
	}
	tracker.Transient("http://proxy:1")
	tracker.Transient("http://proxy:1")
	now = now.Add(1500 * time.Millisecond)
	if tracker.Allowed("http://proxy:1") {
		t.Fatal("second failure should back off longer than 1s")
	}
	tracker.Success("http://proxy:1")
	if !tracker.Allowed("http://proxy:1") {
		t.Fatal("success did not clear")
	}
	if !tracker.Allowed("") {
		t.Fatal("direct egress blocked by proxy failure")
	}
}

func TestEgressTrackerCapAndSweep(t *testing.T) {
	tracker := NewEgressTracker()
	tracker.maxItems = 8
	now := time.Unix(2000, 0)
	tracker.now = func() time.Time { return now }
	for i := 0; i < 20; i++ {
		tracker.Transient("http://proxy:" + strconv.Itoa(i))
	}
	tracker.mu.Lock()
	size := len(tracker.until)
	tracker.mu.Unlock()
	if size > tracker.maxItems {
		t.Fatalf("tracker state unbounded: %d", size)
	}
	now = now.Add(16 * time.Second)
	tracker.mu.Lock()
	tracker.sweepLocked()
	remaining := len(tracker.until)
	tracker.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expired entries not swept: %d", remaining)
	}
}

func TestEgressTrackerEscalatesAndCaps(t *testing.T) {
	tracker := NewEgressTracker()
	now := time.Unix(3000, 0)
	tracker.now = func() time.Time { return now }
	tracker.Transient("http://proxy:1")
	now = now.Add(1300 * time.Millisecond)
	if !tracker.Allowed("http://proxy:1") {
		t.Fatal("first backoff did not expire")
	}
	tracker.Transient("http://proxy:1")
	now = now.Add(2300 * time.Millisecond)
	if !tracker.Allowed("http://proxy:1") {
		t.Fatal("second backoff did not expire")
	}
	tracker.Transient("http://proxy:1")
	now = now.Add(250 * time.Millisecond)
	if tracker.Allowed("http://proxy:1") {
		t.Fatal("third failure should escalate beyond 250ms")
	}
	for i := 0; i < 10; i++ {
		tracker.Transient("http://proxy:1")
	}
	tracker.mu.Lock()
	delay := tracker.until["http://proxy:1"].Sub(now)
	tracker.mu.Unlock()
	if delay > 15*time.Second {
		t.Fatalf("backoff cap exceeded: %v", delay)
	}
	tracker.Success("http://proxy:1")
	tracker.mu.Lock()
	_, hasStep := tracker.step["http://proxy:1"]
	tracker.mu.Unlock()
	if hasStep {
		t.Fatal("success did not reset the failure ladder")
	}
}
