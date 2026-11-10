package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	DefaultRepository   = "ayeaaaa/mix2api"
	ScheduledInterval   = 24 * time.Hour
	StartupWindow       = time.Hour
	StartupChecks       = 6
	StartupStampsKept   = 12
	FirstDelay          = 30 * time.Second
	PollInterval        = 5 * time.Minute
	RequestTimeout      = 10 * time.Second
	MaxReleaseBody      = 2 << 20
	CheckStateKey       = "update_check"
	LegacyCheckStateKey = "update_check_state"
	CheckEnabledKey     = "update_check_enabled"
	ProxySlotKey        = "update_check_proxy_slot"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
var releaseTagPattern = regexp.MustCompile(`/releases/tag/([^/?#]+)`)

// Settings is the narrow config.Store contract the checker persists through.
type Settings interface {
	Load() (map[string]any, error)
	Merge(patch map[string]any) (map[string]any, error)
}

// Fetcher returns the raw GitHub releases document for a URL.
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// ProxyFetcher is an optional fetcher extension used by deployments that pin
// release checks to a proxy slot. Fetchers without it remain valid.
type ProxyFetcher interface {
	FetchWithProxy(ctx context.Context, url, proxy string) ([]byte, error)
}

// FetcherFunc adapts a function to Fetcher.
type FetcherFunc func(ctx context.Context, url string) ([]byte, error)

func (f FetcherFunc) Fetch(ctx context.Context, url string) ([]byte, error) { return f(ctx, url) }

// Status is the panel update-check payload.
type Status struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	LatestTag       string `json:"latest_tag,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	Enabled         bool   `json:"enabled"`
	Checking        bool   `json:"checking"`
	LastAttempt     string `json:"last_attempt,omitempty"`
	LastSuccess     string `json:"last_success,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	ReleaseURL      string `json:"release_url,omitempty"`
	PublishedAt     string `json:"published_at,omitempty"`
	Msg             string `json:"msg,omitempty"`
}

// Checker owns release discovery and its small persisted bookkeeping.
type Checker struct {
	currentVersion string
	settings       Settings
	fetcher        Fetcher
	now            func() time.Time
	logger         func(string)

	mu            sync.Mutex
	checking      bool
	latest        *Release
	latestVersion string
	lastAttempt   float64
	lastSuccess   float64
	lastError     string
	startupStamps []float64
	stopCh        chan struct{}
	stopOnce      sync.Once
}

// NewChecker builds a checker. settings may be nil (no persistence); fetcher
// nil uses the GitHub releases fetcher.
func NewChecker(currentVersion string, settings Settings, fetcher Fetcher, now func() time.Time, logger func(string)) *Checker {
	if now == nil {
		now = time.Now
	}
	checker := &Checker{
		currentVersion: strings.TrimSpace(currentVersion),
		settings:       settings,
		fetcher:        fetcher,
		now:            now,
		logger:         logger,
		stopCh:         make(chan struct{}),
	}
	if checker.fetcher == nil {
		checker.fetcher = &GitHubFetcher{Client: &http.Client{Timeout: RequestTimeout}}
	}
	checker.loadState()
	return checker
}

func (c *Checker) loadState() {
	if c == nil || c.settings == nil {
		return
	}
	data, err := c.settings.Load()
	if err != nil {
		return
	}
	raw, _ := data[CheckStateKey].(map[string]any)
	if raw == nil {
		// Earlier work stored the same document under update_check_state.
		raw, _ = data[LegacyCheckStateKey].(map[string]any)
		if raw != nil {
			_, _ = c.settings.Merge(map[string]any{CheckStateKey: raw})
		}
	}
	if raw == nil {
		return
	}
	c.lastAttempt = positiveFloat(raw["last_attempt"])
	c.lastSuccess = positiveFloat(raw["last_success"])
	c.latestVersion = truncateString(strings.TrimSpace(stringValue(raw["latest_version"])), 32)
	if c.latestVersion != "" {
		c.latest = &Release{Version: c.latestVersion, Newer: IsNewer(c.latestVersion, c.currentVersion)}
	}
	if list, ok := raw["startup_checks"].([]any); ok {
		for _, item := range list {
			if value := positiveFloat(item); value > 0 {
				c.startupStamps = append(c.startupStamps, value)
			}
		}
	}
	if len(c.startupStamps) > StartupStampsKept {
		c.startupStamps = c.startupStamps[len(c.startupStamps)-StartupStampsKept:]
	}
}

// Enabled reports the operator's daily-check preference (default false).
func (c *Checker) Enabled() bool {
	if c == nil || c.settings == nil {
		return false
	}
	data, err := c.settings.Load()
	if err != nil {
		return false
	}
	value, _ := data[CheckEnabledKey].(bool)
	return value
}

// Due reports the 24h window.
func (c *Checker) Due(now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastAttempt == 0 {
		return true
	}
	return float64(now.Unix())-c.lastAttempt >= ScheduledInterval.Seconds()
}

// StartupDue reports whether the crash-restart budget allows one startup check.
func (c *Checker) StartupDue(now time.Time) bool {
	if c == nil || !c.Enabled() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastAttempt == 0 {
		return true
	}
	cutoff := float64(now.Add(-StartupWindow).Unix())
	spent := 0
	for _, stamp := range c.startupStamps {
		if stamp > cutoff {
			spent++
		}
	}
	return spent < StartupChecks
}

// Status returns a snapshot without contacting the network.
func (c *Checker) Status() Status {
	if c == nil {
		return Status{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	status := Status{
		CurrentVersion: c.currentVersion,
		Enabled:        c.Enabled(),
		Checking:       c.checking,
		LastAttempt:    formatStamp(c.lastAttempt),
		LastSuccess:    formatStamp(c.lastSuccess),
		LastError:      c.lastError,
	}
	if c.latest != nil {
		status.LatestVersion = c.latest.Version
		status.LatestTag = c.latest.Tag
		status.UpdateAvailable = IsNewer(c.latest.Version, c.currentVersion)
		status.ReleaseURL = c.latest.URL
		status.PublishedAt = c.latest.PublishedAt
	}
	return status
}

// Check runs one discovery round. manual ignores the switch and 24h window.
func (c *Checker) Check(ctx context.Context, manual bool) Status {
	if c == nil {
		return Status{}
	}
	status := c.Status()
	if !manual {
		if !c.Enabled() {
			status.Msg = "skipped: disabled"
			return status
		}
		if !c.Due(c.now()) {
			status.Msg = "skipped: not due yet"
			return status
		}
	}
	c.mu.Lock()
	if c.checking {
		c.mu.Unlock()
		status.Checking = true
		status.Msg = "skipped: already running"
		return status
	}
	c.checking = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.checking = false
		c.mu.Unlock()
	}()
	c.runCheck(ctx)
	status = c.Status()
	return status
}

// CheckStartup runs the startup catch-up when the hourly budget allows.
func (c *Checker) CheckStartup(ctx context.Context) Status {
	if c == nil {
		return Status{}
	}
	now := c.now()
	if !c.StartupDue(now) {
		status := c.Status()
		status.Msg = "skipped: startup budget spent"
		return status
	}
	c.noteStartup(now)
	return c.Check(ctx, true)
}

func (c *Checker) noteStartup(now time.Time) {
	c.mu.Lock()
	c.startupStamps = append(c.startupStamps, float64(now.Unix()))
	if len(c.startupStamps) > StartupStampsKept {
		c.startupStamps = c.startupStamps[len(c.startupStamps)-StartupStampsKept:]
	}
	stamps := append([]float64(nil), c.startupStamps...)
	c.mu.Unlock()
	c.persist(stamps, false, "")
}

func (c *Checker) runCheck(ctx context.Context) {
	now := c.now()
	attempt := float64(now.Unix())
	c.mu.Lock()
	c.lastAttempt = attempt
	c.lastError = ""
	c.mu.Unlock()
	url := GitHubReleasesURL()
	proxy := c.proxyURL()
	var raw []byte
	var err error
	if proxy != "" {
		if proxyFetcher, ok := c.fetcher.(ProxyFetcher); ok {
			raw, err = proxyFetcher.FetchWithProxy(ctx, url, proxy)
		} else {
			raw, err = c.fetcher.Fetch(ctx, url)
		}
	} else {
		raw, err = c.fetcher.Fetch(ctx, url)
	}
	if err != nil {
		c.fail(DescribeFailure(err))
		return
	}
	var document []any
	if err := json.Unmarshal(raw, &document); err != nil {
		c.fail("invalid JSON")
		return
	}
	if document == nil {
		c.fail("unexpected release document")
		return
	}
	latest := PickLatest(document, c.currentVersion)
	c.mu.Lock()
	c.latest = latest
	if latest != nil {
		c.latestVersion = latest.Version
	}
	c.lastSuccess = attempt
	c.lastError = ""
	c.mu.Unlock()
	version := ""
	if latest != nil {
		version = latest.Version
	}
	c.persist(nil, true, version)
	if latest != nil && latest.Newer {
		c.log("发现新版本 " + latest.Version + "（当前 " + c.currentVersion + "）")
	}
}

func (c *Checker) proxyURL() string {
	if c == nil || c.settings == nil {
		return ""
	}
	data, err := c.settings.Load()
	if err != nil {
		return ""
	}
	slotID := strings.TrimSpace(stringValue(data[ProxySlotKey]))
	if slotID == "" {
		return ""
	}
	slots, _ := data["proxy_slots"].([]any)
	for _, raw := range slots {
		slot, _ := raw.(map[string]any)
		if slot == nil || strings.TrimSpace(stringValue(slot["id"])) != slotID {
			continue
		}
		if enabled, ok := slot["enabled"].(bool); ok && !enabled {
			return ""
		}
		return strings.TrimSpace(stringValue(slot["url"]))
	}
	return ""
}

func (c *Checker) fail(message string) {
	c.mu.Lock()
	c.lastError = truncateString(message, 120)
	c.mu.Unlock()
	c.persist(nil, false, "")
	c.log("更新检查失败：" + message)
}

func (c *Checker) persist(startup []float64, success bool, version string) {
	if c == nil || c.settings == nil {
		return
	}
	c.mu.Lock()
	state := map[string]any{}
	if existing, err := c.settings.Load(); err == nil {
		if raw, ok := existing[CheckStateKey].(map[string]any); ok {
			for key, value := range raw {
				state[key] = value
			}
		}
	}
	state["last_attempt"] = c.lastAttempt
	if success {
		state["last_success"] = c.lastSuccess
	}
	if version != "" {
		state["latest_version"] = truncateString(version, 32)
	}
	if startup != nil {
		list := make([]any, 0, len(startup))
		for _, stamp := range startup {
			list = append(list, stamp)
		}
		state["startup_checks"] = list
	} else if existing, ok := state["startup_checks"]; ok {
		state["startup_checks"] = existing
	}
	c.mu.Unlock()
	_, _ = c.settings.Merge(map[string]any{CheckStateKey: state})
}

func (c *Checker) log(message string) {
	if c != nil && c.logger != nil {
		c.logger(message)
	}
}

// Stop makes the optional background loop exit promptly.
func (c *Checker) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() { close(c.stopCh) })
}

// Run is the optional background loop; callers may start it in a goroutine.
func (c *Checker) Run() {
	if c == nil {
		return
	}
	if !sleepOrStop(c.stopCh, FirstDelay) {
		return
	}
	c.CheckStartup(context.Background())
	for {
		if !sleepOrStop(c.stopCh, PollInterval) {
			return
		}
		c.Check(context.Background(), false)
	}
}

func sleepOrStop(stop <-chan struct{}, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

// GitHubReleasesURL resolves the configured owner/repo shape.
func GitHubReleasesURL() string {
	repo := strings.TrimSpace(os.Getenv("WB_UPDATE_REPO"))
	if !repositoryPattern.MatchString(repo) {
		repo = DefaultRepository
	}
	return "https://api.github.com/repos/" + repo + "/releases"
}

// DescribeFailure returns a short safe reason with no URL or response body.
func DescribeFailure(err error) string {
	if err == nil {
		return ""
	}
	var httpErr interface{ HTTPStatus() int }
	if typed, ok := err.(interface{ StatusCode() int }); ok && typed.StatusCode() > 0 {
		return fmt.Sprintf("HTTP %d", typed.StatusCode())
	}
	if typed, ok := err.(*url.Error); ok {
		if typed.Timeout() {
			return "timeout"
		}
		return "network error: " + typeName(typed.Err)
	}
	if _, ok := err.(*json.SyntaxError); ok {
		return "invalid JSON"
	}
	_ = httpErr
	return truncateString(typeName(err), 80)
}

func redirectedClient(client *http.Client) *http.Client {
	copy := *client
	copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return errors.New("redirect must use https")
		}
		host := strings.ToLower(req.URL.Hostname())
		if host != "github.com" && host != "api.github.com" {
			return errors.New("redirect host is not github")
		}
		return nil
	}
	return &copy
}

func typeName(err error) string {
	if err == nil {
		return "unknown"
	}
	text := fmt.Sprintf("%T", err)
	if index := strings.LastIndex(text, "."); index >= 0 {
		text = text[index+1:]
	}
	return text
}

func formatStamp(epoch float64) string {
	if epoch <= 0 {
		return ""
	}
	return time.Unix(int64(epoch), 0).Local().Format("2006-01-02 15:04:05")
}

func positiveFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		if typed > 0 {
			return typed
		}
	case float32:
		if typed > 0 {
			return float64(typed)
		}
	case int:
		if typed > 0 {
			return float64(typed)
		}
	case int64:
		if typed > 0 {
			return float64(typed)
		}
	case json.Number:
		if number, err := typed.Float64(); err == nil && number > 0 {
			return number
		}
	}
	return 0
}

// GitHubFetcher is the real network seam. Tests never use it directly.
type GitHubFetcher struct {
	Client *http.Client
}

// Fetch tries the releases API then the releases/latest redirect endpoint.
func (f *GitHubFetcher) Fetch(ctx context.Context, releasesURL string) ([]byte, error) {
	return f.FetchWithProxy(ctx, releasesURL, "")
}

// FetchWithProxy is Fetch pinned to one proxy URL.
func (f *GitHubFetcher) FetchWithProxy(ctx context.Context, releasesURL, proxy string) ([]byte, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: RequestTimeout}
	}
	if strings.TrimSpace(proxy) != "" {
		proxyURL, err := url.Parse(proxy)
		if err == nil {
			copy := *client
			copy.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
			client = &copy
		}
	}
	client = redirectedClient(client)
	apiErr := error(nil)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err == nil {
		request.Header.Set("User-Agent", "wb2api-hub-update-check")
		request.Header.Set("Accept", "application/vnd.github+json")
		if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			defer response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return io.ReadAll(io.LimitReader(response.Body, MaxReleaseBody))
			}
			apiErr = fmt.Errorf("HTTP %d", response.StatusCode)
		} else {
			apiErr = requestErr
		}
	} else {
		apiErr = err
	}
	repo := strings.TrimSpace(os.Getenv("WB_UPDATE_REPO"))
	if !repositoryPattern.MatchString(repo) {
		repo = DefaultRepository
	}
	latestURL := "https://github.com/" + repo + "/releases/latest"
	latestRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, apiErr
	}
	latestRequest.Header.Set("User-Agent", "wb2api-hub-update-check")
	response, err := client.Do(latestRequest)
	if err != nil {
		return nil, apiErr
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	final := ""
	if response.Request != nil && response.Request.URL != nil {
		final = response.Request.URL.String()
	}
	if !strings.HasPrefix(final, "https://github.com/") {
		return nil, apiErr
	}
	match := releaseTagPattern.FindStringSubmatch(final)
	if match == nil {
		return nil, apiErr
	}
	payload := []any{map[string]any{
		"tag_name": match[1], "draft": false, "prerelease": false,
		"html_url": final, "published_at": "",
	}}
	return json.Marshal(payload)
}
