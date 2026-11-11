package load

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// requireLoopbackURL keeps the manual baseline on loopback. The v1 harness and
// the external oracle fake both use this guard before any request is made.
func requireLoopbackURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("host %q is not loopback", host)
	}
	return nil
}

func rssKB(pid int) uint64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		value, _ := strconv.ParseUint(fields[1], 10, 64)
		return value
	}
	return 0
}

// procCPU returns cumulative user+system CPU seconds from /proc/<pid>/stat.
// Linux reports those fields in clock ticks; 100 Hz is the standard value on
// the test host and is also what the harness records in its evidence.
func procCPU(pid int) float64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	text := string(raw)
	end := strings.LastIndex(text, ")")
	if end < 0 || end+1 >= len(text) {
		return 0
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 13 {
		return 0
	}
	utime, _ := strconv.ParseFloat(fields[11], 64)
	stime, _ := strconv.ParseFloat(fields[12], 64)
	return (utime + stime) / 100
}

type rssSampler struct {
	stop chan struct{}
	done chan struct{}
	peak atomic.Uint64
}

func startRSSSampler(pid int) *rssSampler {
	sampler := &rssSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(sampler.done)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sampler.stop:
				return
			case <-ticker.C:
				value := rssKB(pid)
				if value > sampler.peak.Load() {
					sampler.peak.Store(value)
				}
			}
		}
	}()
	return sampler
}

func (s *rssSampler) Stop() uint64 {
	if s == nil {
		return 0
	}
	close(s.stop)
	<-s.done
	return s.peak.Load()
}

func writeLoadEvidence(t *testing.T, evidence map[string]any) {
	t.Helper()
	raw, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("LOAD_EVIDENCE %s", raw)
	if value := os.Getenv("MIX2API_LOAD_EVIDENCE"); value != "" {
		if err := os.WriteFile(value, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func countJSONLines(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, nil
}
