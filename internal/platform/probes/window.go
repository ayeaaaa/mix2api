package probes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Window is one registered 429 reset instant.
type Window struct {
	UID        string  `json:"uid"`
	Model      string  `json:"model"`
	ResetAt    float64 `json:"reset_at"`
	Attempts   int     `json:"attempts"`
	LastTry    float64 `json:"last_try"`
	LastResult string  `json:"last_result"`
	Cap        bool    `json:"cap"`
	CapAt      float64 `json:"cap_at"`
}

// WindowState persists accounts/model_windows.json.
type WindowState struct {
	Dir     string
	Now     func() time.Time
	Enabled func() bool

	mu      sync.Mutex
	windows map[string]Window
}

// NewWindowState loads the state file, ignoring corruption.
func NewWindowState(dir string) *WindowState {
	state := &WindowState{Dir: dir, Now: time.Now, windows: map[string]Window{}}
	state.load()
	return state
}

func windowKey(uid, model string) string { return uid + "\x00" + model }

func (s *WindowState) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
}

func (s *WindowState) loadLocked() {
	raw, err := os.ReadFile(filepath.Join(s.Dir, WindowStateFile))
	if err != nil {
		return
	}
	var payload struct {
		Version int      `json:"version"`
		Windows []Window `json:"windows"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return
	}
	now := s.now().Unix()
	for _, window := range payload.Windows {
		if window.UID == "" || window.Model == "" || window.ResetAt <= 0 {
			continue
		}
		if int64(window.ResetAt) < now-int64(WindowMaxAge/time.Second) {
			continue
		}
		s.windows[windowKey(window.UID, window.Model)] = window
	}
}

func (s *WindowState) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *WindowState) enabled() bool {
	if s != nil && s.Enabled != nil {
		return s.Enabled()
	}
	return true
}

// Schedule registers a reset window. Duplicate identical instants are a no-op
// except for upgrading the cap marker.
func (s *WindowState) Schedule(uid, model string, resetAt time.Time, cap bool, capAt time.Time, now time.Time) bool {
	if s == nil || !s.enabled() || uid == "" || model == "" || !resetAt.After(now) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeReloadLocked()
	key := windowKey(uid, model)
	existing, ok := s.windows[key]
	if ok && absFloat(existing.ResetAt-resetTime(resetAt)) < 1.0 {
		if cap && !existing.Cap {
			existing.Cap = true
			existing.CapAt = float64(capAt.Unix())
			s.windows[key] = existing
			s.saveLocked()
			return true
		}
		return false
	}
	entry := Window{UID: uid, Model: model, ResetAt: resetTime(resetAt)}
	if cap {
		entry.Cap = true
		entry.CapAt = float64(capAt.Unix())
	}
	s.windows[key] = entry
	s.saveLocked()
	return true
}

func resetTime(value time.Time) float64 { return float64(value.UnixNano()) / 1e9 }

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

// Drop removes one window.
func (s *WindowState) Drop(uid, model string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := windowKey(uid, model)
	if _, ok := s.windows[key]; !ok {
		return false
	}
	delete(s.windows, key)
	s.saveLocked()
	return true
}

// HasWindow reports whether quota scan must defer to the window probe.
func (s *WindowState) HasWindow(uid, model string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeReloadLocked()
	_, ok := s.windows[windowKey(uid, model)]
	return ok
}

// Get returns a copy of one window.
func (s *WindowState) Get(uid, model string) (Window, bool) {
	if s == nil {
		return Window{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeReloadLocked()
	window, ok := s.windows[windowKey(uid, model)]
	return window, ok
}

// Due returns due windows in reset order.
func (s *WindowState) Due(now time.Time, limit int) []Window {
	if s == nil {
		return nil
	}
	if limit <= 0 {
		limit = WindowBatchLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeReloadLocked()
	out := []Window{}
	for _, window := range s.windows {
		if window.ResetAt <= resetTime(now) {
			out = append(out, window)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResetAt < out[j].ResetAt })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Snapshot returns panel rows ordered by reset time.
func (s *WindowState) Snapshot(now time.Time) []map[string]any {
	if s == nil {
		return []map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeReloadLocked()
	rows := []Window{}
	for _, window := range s.windows {
		rows = append(rows, window)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ResetAt < rows[j].ResetAt })
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"uid": row.UID, "uid8": first8(row.UID), "model": row.Model,
			"reset_at": row.ResetAt, "reset_in": maxInt(0, int(row.ResetAt-resetTime(now))),
			"attempts": row.Attempts, "last_result": row.LastResult,
			"cap": row.Cap, "cap_at": row.CapAt,
		})
	}
	return out
}

// SecondsUntilDue returns the nearest reset delay, or (0,false).
func (s *WindowState) SecondsUntilDue(now time.Time) (float64, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeReloadLocked()
	if len(s.windows) == 0 {
		return 0, false
	}
	soonest := 0.0
	first := true
	for _, window := range s.windows {
		if first || window.ResetAt < soonest {
			soonest, first = window.ResetAt, false
		}
	}
	value := soonest - resetTime(now)
	if value < 0 {
		value = 0
	}
	return value, true
}

func (s *WindowState) Update(window Window, resetAt time.Time, attempts int, result string, now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := windowKey(window.UID, window.Model)
	current, ok := s.windows[key]
	if !ok {
		return false
	}
	current.ResetAt = resetTime(resetAt)
	current.Attempts = attempts
	current.LastTry = resetTime(now)
	current.LastResult = truncate(result, 200)
	s.windows[key] = current
	s.saveLocked()
	return true
}

func (s *WindowState) maybeReloadLocked() {
	// A single process owns the file; retaining the in-memory map is the v1
	// behavior. The hook exists so future panel writes can trigger reload.
}

func (s *WindowState) saveLocked() {
	list := make([]Window, 0, len(s.windows))
	for _, window := range s.windows {
		list = append(list, window)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ResetAt < list[j].ResetAt })
	payload := struct {
		Version int      `json:"version"`
		SavedAt string   `json:"saved_at"`
		Windows []Window `json:"windows"`
	}{Version: StateVersion, SavedAt: s.now().Format("2006-01-02 15:04:05"), Windows: list}
	_ = atomicWriteJSON(filepath.Join(s.Dir, WindowStateFile), payload)
}

func first8(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
