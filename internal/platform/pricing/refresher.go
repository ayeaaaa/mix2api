package pricing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Refresh scheduling defaults and bounds, matching v1 wb_settings.
const (
	DefaultRefreshMinutes = 5.0
	MaxRefreshMinutes     = 43200.0
)

var settingsMu sync.Mutex

// ClampRefreshMinutes bounds an interval like v1: a month is the practical
// horizon a panel can show, and a negative value means "off".
func ClampRefreshMinutes(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > MaxRefreshMinutes {
		return MaxRefreshMinutes
	}
	return value
}

// RefreshMinutes reads pricing_refresh_minutes from accounts/settings.json.
// A legacy pricing_refresh_hours value is converted once, written back under
// the minutes key and removed. Zero disables refreshes; anything unreadable
// falls back to five minutes.
func RefreshMinutes(accountsDir string) float64 {
	settingsMu.Lock()
	defer settingsMu.Unlock()

	path := filepath.Join(accountsDir, "settings.json")
	settings := loadSettingsMap(path)
	raw, hasMinutes := settings["pricing_refresh_minutes"]
	if hasMinutes {
		return normalizeRefreshValue(raw)
	}
	legacy, hasHours := settings["pricing_refresh_hours"]
	if !hasHours {
		return DefaultRefreshMinutes
	}
	delete(settings, "pricing_refresh_hours")
	if number, ok := asFloat(legacy); ok {
		minutes := ClampRefreshMinutes(number * 60)
		settings["pricing_refresh_minutes"] = minutes
		_ = writeSettingsMap(path, settings)
		return minutes
	}
	_ = writeSettingsMap(path, settings)
	return DefaultRefreshMinutes
}

// SetRefreshMinutes persists a clamped interval. Invalid input leaves the
// stored value untouched and returns it.
func SetRefreshMinutes(accountsDir string, value any) float64 {
	current := RefreshMinutes(accountsDir)
	number, ok := asFloat(value)
	if !ok {
		return current
	}
	number = ClampRefreshMinutes(number)

	settingsMu.Lock()
	defer settingsMu.Unlock()
	path := filepath.Join(accountsDir, "settings.json")
	settings := loadSettingsMap(path)
	settings["pricing_refresh_minutes"] = number
	if err := writeSettingsMap(path, settings); err != nil {
		return current
	}
	return number
}

func normalizeRefreshValue(value any) float64 {
	number, ok := asFloat(value)
	if !ok {
		return DefaultRefreshMinutes
	}
	if number <= 0 {
		return 0
	}
	return ClampRefreshMinutes(number)
}

func loadSettingsMap(path string) map[string]any {
	settings := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		return settings
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&settings); err != nil || settings == nil {
		return map[string]any{}
	}
	return settings
}

func writeSettingsMap(path string, settings map[string]any) error {
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
	if err := encoder.Encode(settings); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		cleanup()
		return err
	}
	if _, err := temp.Write(buffer.Bytes()); err != nil {
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

// NextRefreshWait returns how long the loop should wait before its next run.
// A zero lastRun means the loop has not run yet, so a full interval is used.
func NextRefreshWait(now, lastRun time.Time, intervalMinutes float64) time.Duration {
	interval := ClampRefreshMinutes(intervalMinutes)
	if interval <= 0 {
		return 0
	}
	duration := time.Duration(interval * float64(time.Minute))
	if lastRun.IsZero() {
		return duration
	}
	remaining := duration - now.Sub(lastRun)
	if remaining < 0 {
		return 0
	}
	return remaining
}

var errRefreshBusy = errors.New("pricing: refresh already running")

// FetchFunc fetches the normalized OpenRouter catalogue.
type FetchFunc func(ctx context.Context) (map[string]OpenRouterPrice, error)

// LogFunc receives short, secret-free refresh messages.
type LogFunc func(message string)

// RefresherOptions wires the background price refresher. Snapshot, Fetch and
// Interval are required; everything else has a safe default.
type RefresherOptions struct {
	Snapshot *Snapshot
	Fetch    FetchFunc
	Interval func() float64
	Logger   LogFunc
	Now      func() time.Time

	// ExtraIDs supplies live catalogue ids the bundled snapshot does not know.
	// It is optional and must never block or perform network I/O.
	ExtraIDs func() []string

	// RetryMin and RetryMax bound the failure backoff. Zero selects the v1-like
	// defaults (30s and 15m); tests may shrink them.
	RetryMin time.Duration
	RetryMax time.Duration
}

// Refresher periodically appends a fresh OpenRouter version to the policy
// history. Runs never overlap, failures keep the previous prices, and the
// loop never blocks a request.
type Refresher struct {
	snapshot *Snapshot
	fetch    FetchFunc
	interval func() float64
	logger   LogFunc
	now      func() time.Time
	extraIDs func() []string

	runMu       sync.Mutex
	mu          sync.Mutex
	stop        chan struct{}
	stopped     bool
	lastRun     time.Time
	lastAttempt time.Time
	busy        bool
	nextAttempt time.Time
	failures    int64
	retryMin    time.Duration
	retryMax    time.Duration
	lastErr     string
	models      int
	extra       int
	unpriced    []string
	logs        []string
}

// NewRefresher builds a refresher. It does not start until Run is called.
func NewRefresher(opts RefresherOptions) *Refresher {
	refresher := &Refresher{
		snapshot: opts.Snapshot,
		fetch:    opts.Fetch,
		interval: opts.Interval,
		logger:   opts.Logger,
		now:      opts.Now,
		extraIDs: opts.ExtraIDs,
		retryMin: opts.RetryMin,
		retryMax: opts.RetryMax,
		stop:     make(chan struct{}),
	}
	if refresher.retryMin <= 0 {
		refresher.retryMin = 30 * time.Second
	}
	if refresher.retryMax <= 0 {
		refresher.retryMax = 15 * time.Minute
	}
	if refresher.retryMax < refresher.retryMin {
		refresher.retryMax = refresher.retryMin
	}
	if refresher.snapshot == nil {
		refresher.snapshot = Active()
	}
	if refresher.fetch == nil {
		refresher.fetch = func(ctx context.Context) (map[string]OpenRouterPrice, error) {
			return FetchOpenRouter(ctx, &http.Client{Timeout: 45 * time.Second}, OpenRouterURL)
		}
	}
	return refresher
}

// Stop asks the loop to exit promptly. It is safe to call more than once.
func (r *Refresher) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		close(r.stop)
	}
	r.mu.Unlock()
}

func (r *Refresher) stoppedNow() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *Refresher) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Refresher) intervalMinutes() float64 {
	if r.interval != nil {
		return ClampRefreshMinutes(r.interval())
	}
	if r.snapshot != nil {
		return RefreshMinutes(r.snapshot.accountsDir)
	}
	return DefaultRefreshMinutes
}

// Run blocks until the context is cancelled or Stop is called. The first
// cycle waits one interval before fetching, so a hot restart does not hammer
// OpenRouter. Failures back off exponentially: the loop never retries in a
// tight cycle just because a fetch failed or the switch is off.
func (r *Refresher) Run(ctx context.Context) {
	if r == nil {
		return
	}
	for {
		if err := ctx.Err(); err != nil || r.stoppedNow() {
			return
		}
		interval := r.intervalMinutes()
		wait := r.nextWait(r.clock(), interval)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		if interval <= 0 {
			r.mu.Lock()
			r.lastAttempt = r.clock()
			r.mu.Unlock()
			continue
		}
		if err := r.RunOnce(ctx); err != nil && !errors.Is(err, errRefreshBusy) {
			r.logf("pricing refresh failed: %s", err.Error())
		}
	}
}

func (r *Refresher) nextWait(now time.Time, intervalMinutes float64) time.Duration {
	r.mu.Lock()
	nextAttempt := r.nextAttempt
	lastAttempt := r.lastAttempt
	r.mu.Unlock()
	if nextAttempt.After(now) {
		return nextAttempt.Sub(now)
	}
	wait := NextRefreshWait(now, lastAttempt, intervalMinutes)
	if intervalMinutes <= 0 {
		// Disabled: poll the setting occasionally so a panel change is picked
		// up without a wake-up channel.
		if wait <= 0 {
			wait = 30 * time.Second
		}
	}
	return wait
}

func (r *Refresher) noteBusy(at time.Time) {
	r.mu.Lock()
	r.lastAttempt = at
	r.busy = true
	r.mu.Unlock()
}

func (r *Refresher) noteAttempt(at time.Time, success bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastAttempt = at
	r.busy = false
	if success {
		r.lastRun = at
		r.failures = 0
		r.nextAttempt = time.Time{}
		return
	}
	r.failures++
	backoff := r.retryMin
	for count := int64(1); count < r.failures && backoff < r.retryMax; count++ {
		backoff *= 2
		if backoff > r.retryMax {
			backoff = r.retryMax
		}
	}
	r.nextAttempt = at.Add(backoff)
}

// RunOnce performs one non-overlapping refresh.
func (r *Refresher) RunOnce(ctx context.Context) error {
	if r == nil {
		return errors.New("pricing: nil refresher")
	}
	if !r.runMu.TryLock() {
		// A long manual refresh already owns the work. This is not a fetch
		// failure: record the attempt, mark busy, and let the loop wait a
		// normal interval instead of incrementing the failure backoff.
		r.noteBusy(r.clock())
		return errRefreshBusy
	}
	defer r.runMu.Unlock()

	if r.snapshot == nil || !r.snapshot.Enabled() {
		r.noteAttempt(r.clock(), false)
		return errors.New("pricing disabled")
	}
	models, err := r.fetch(ctx)
	if err != nil {
		r.recordFailure("fetch error")
		r.noteAttempt(r.clock(), false)
		return errors.New("fetch error")
	}
	extra := []string{}
	if r.extraIDs != nil {
		extra = r.extraIDs()
	}
	doc, unpriced, _ := r.snapshot.BuildSnapshot(models, r.snapshot, extra, r.snapshot.VariantInherit())
	if len(doc.Models) == 0 {
		r.recordFailure("no models matched")
		r.noteAttempt(r.clock(), false)
		return errors.New("no models matched")
	}
	at := float64(r.clock().UnixNano()) / 1e9
	added, _, assignment := r.snapshot.RecordPolicies(doc, at, false)
	if added > 0 {
		protect := map[string]bool{}
		for _, id := range assignment {
			protect[id] = true
		}
		r.snapshot.PrunePolicies(protect)
	}
	r.snapshot.RememberOpenRouter(models)
	now := r.clock()
	r.mu.Lock()
	r.lastErr = ""
	r.models = len(doc.Models)
	r.extra = len(extra)
	r.unpriced = append([]string(nil), unpriced...)
	r.logs = appendCapped(r.logs, "已取价：模型 "+fmt.Sprint(len(doc.Models))+" 个，新增策略 "+fmt.Sprint(added)+" 条")
	r.mu.Unlock()
	r.noteAttempt(now, true)
	return nil
}

func (r *Refresher) recordFailure(reason string) {
	r.mu.Lock()
	r.lastErr = reason
	r.logs = appendCapped(r.logs, "取价失败："+reason)
	r.mu.Unlock()
}

func appendCapped(lines []string, line string) []string {
	lines = append(lines, line)
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return lines
}

func (r *Refresher) logf(format string, args ...any) {
	message := strings.TrimSpace(fmt.Sprintf(format, args...))
	if message == "" {
		return
	}
	if r.logger != nil {
		r.logger(message)
	}
	r.mu.Lock()
	r.logs = appendCapped(r.logs, message)
	r.mu.Unlock()
}

// Status is the panel-facing refresher state.
func (r *Refresher) Status() map[string]any {
	if r == nil {
		return map[string]any{"enabled": false}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	interval := r.intervalMinutes()
	master := r.snapshot != nil && r.snapshot.Enabled()
	nextRun := r.nextAttempt
	if nextRun.IsZero() && interval > 0 && !r.lastAttempt.IsZero() {
		nextRun = r.lastAttempt.Add(time.Duration(interval * float64(time.Minute)))
	}
	lastRun := int64(0)
	if !r.lastRun.IsZero() {
		lastRun = r.lastRun.Unix()
	}
	lastAttempt := int64(0)
	if !r.lastAttempt.IsZero() {
		lastAttempt = r.lastAttempt.Unix()
	}
	nextRunUnix := int64(0)
	if !nextRun.IsZero() {
		nextRunUnix = nextRun.Unix()
	}
	return map[string]any{
		"interval_minutes": interval,
		"enabled":          master && interval > 0,
		"master_enabled":   master,
		"last_run":         lastRun,
		"last_attempt":     lastAttempt,
		"next_run":         nextRunUnix,
		"failures":         r.failures,
		"busy":             r.busy,
		"last_error":       r.lastErr,
		"last_models":      r.models,
		"last_extra":       r.extra,
		"last_unpriced":    append([]string(nil), r.unpriced...),
		"logs":             append([]string(nil), r.logs...),
	}
}
