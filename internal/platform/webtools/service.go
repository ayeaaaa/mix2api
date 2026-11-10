package webtools

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// Service owns the local web tools, cache, engine cooling and HTTP client.
type Service struct {
	Client   *http.Client
	LookupIP func(host string) ([]net.IP, error)
	Now      func() time.Time
	Logger   func(string)

	CacheTTL        time.Duration
	CooldownBlocked time.Duration
	CooldownError   time.Duration
	MinInterval     time.Duration
	SearchBudget    time.Duration
	Engines         []string
	EngineURLs      map[string]string
	AllowPrivate    bool

	cache *ttlCache

	readyOnce sync.Once

	flightMu sync.Mutex
	flights  map[string]*flight

	coolMu sync.Mutex
	cool   map[string]coolEntry

	throttleMu   sync.Mutex
	throttleLast time.Time
}

type coolEntry struct {
	until  time.Time
	reason string
}

// New returns a service with v1 defaults.
func New() *Service {
	return &Service{
		Client:          &http.Client{Timeout: HTTPTimeout * time.Second},
		Now:             time.Now,
		CacheTTL:        10 * time.Minute,
		CooldownBlocked: 10 * time.Minute,
		CooldownError:   time.Minute,
		MinInterval:     500 * time.Millisecond,
		SearchBudget:    25 * time.Second,
		Engines:         []string{"duckduckgo", "bing", "wikipedia", "stackoverflow", "news"},
		cache:           newTTLCache(128),
		flights:         map[string]*flight{},
		cool:            map[string]coolEntry{},
	}
}

// NewService is an alias for New for composition code.
func NewService() *Service { return New() }

// Default is the package-level service used by the convenience functions.
var Default = New()

func (s *Service) ensureReady() {
	if s == nil {
		return
	}
	s.readyOnce.Do(func() {
		if s.Client == nil {
			s.Client = &http.Client{Timeout: HTTPTimeout * time.Second}
		}
		if s.Now == nil {
			s.Now = time.Now
		}
		if s.cache == nil {
			s.cache = newTTLCache(128)
		}
		if s.flights == nil {
			s.flights = map[string]*flight{}
		}
		if s.cool == nil {
			s.cool = map[string]coolEntry{}
		}
		if s.CacheTTL <= 0 {
			s.CacheTTL = 10 * time.Minute
		}
		if s.MinInterval <= 0 {
			s.MinInterval = 500 * time.Millisecond
		}
		if s.SearchBudget <= 0 {
			s.SearchBudget = 25 * time.Second
		}
		if s.CooldownBlocked <= 0 {
			s.CooldownBlocked = 10 * time.Minute
		}
		if s.CooldownError <= 0 {
			s.CooldownError = time.Minute
		}
		if len(s.Engines) == 0 {
			s.Engines = []string{"duckduckgo", "bing", "wikipedia", "stackoverflow", "news"}
		}
	})
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log(message string) {
	if s != nil && s.Logger != nil {
		s.Logger(message)
	}
}

func (s *Service) throttle(ctx context.Context) {
	if s == nil || s.MinInterval <= 0 {
		return
	}
	s.throttleMu.Lock()
	now := s.now()
	wait := s.throttleLast.Add(s.MinInterval).Sub(now)
	s.throttleLast = now
	if wait < 0 {
		wait = 0
	}
	s.throttleMu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	}
}

func (s *Service) cooling(name string) bool {
	s.coolMu.Lock()
	defer s.coolMu.Unlock()
	entry, ok := s.cool[name]
	if !ok {
		return false
	}
	if !entry.until.After(s.now()) {
		delete(s.cool, name)
		return false
	}
	return true
}

func (s *Service) markCooling(name string, duration time.Duration, reason string) {
	if duration <= 0 {
		return
	}
	s.coolMu.Lock()
	s.cool[name] = coolEntry{until: s.now().Add(duration), reason: reason}
	s.coolMu.Unlock()
	s.log("webtools: " + name + " cooling down (" + reason + ")")
}

func (s *Service) clearCooling(name string) {
	s.coolMu.Lock()
	delete(s.cool, name)
	s.coolMu.Unlock()
}

// ResetState clears caches and cooling, for tests and panel reset.
func (s *Service) ResetState() {
	if s == nil || s.cache == nil {
		return
	}
	s.cache.clear()
	s.coolMu.Lock()
	s.cool = map[string]coolEntry{}
	s.coolMu.Unlock()
	s.flightMu.Lock()
	s.flights = map[string]*flight{}
	s.flightMu.Unlock()
}

// Search runs the v1 engine fallback chain. It never panics.
func (s *Service) Search(ctx context.Context, query string, numResults int) string {
	s.ensureReady()
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return "Error: web_search needs a query of at least 2 characters; got " + quoteRunes(query) + ". Pass it as {\"query\": \"...\"}."
	}
	if numResults <= 0 {
		numResults = 5
	}
	if numResults > MaxResults {
		numResults = MaxResults
	}
	key := "search\x00" + strings.ToLower(query) + "\x00" + itoa(numResults)
	if cached, ok := s.cache.get(key, s.now()); ok {
		s.log("webtools: search cache hit for " + query)
		return cached
	}
	release, owner := s.waitFlight(ctx, key)
	if !owner {
		if cached, ok := s.cache.get(key, s.now()); ok {
			return cached
		}
	}
	if owner {
		defer release()
	}
	text := s.searchFanout(ctx, query, numResults)
	if !strings.HasPrefix(text, unavailablePrefix) {
		ttl := s.CacheTTL
		if !strings.HasPrefix(text, "Search results") && ttl > 2*time.Minute {
			ttl = 2 * time.Minute
		}
		s.cache.put(key, text, ttl, s.now())
	}
	return text
}

// Fetch returns readable text with v1's pagination header/truncation message.
func (s *Service) Fetch(ctx context.Context, target string, startIndex int) string {
	s.ensureReady()
	guarded, err := s.GuardURL(target, true)
	if err != nil {
		return "Error: " + err.Error()
	}
	if startIndex < 0 {
		startIndex = 0
	}
	text := s.fetchText(ctx, guarded)
	if strings.HasPrefix(text, "Error:") {
		return text
	}
	if startIndex >= len(text) {
		return "Error: startIndex " + itoa(startIndex) + " is past the end of the page (" + itoa(len(text)) + " characters total)."
	}
	end := startIndex + MaxFetchChars
	if end > len(text) {
		end = len(text)
	}
	head := "URL: " + guarded + "\nCharacters: " + itoa(startIndex) + "-" + itoa(end) + " of " + itoa(len(text))
	tail := ""
	if end < len(text) {
		tail = "\n\n[Truncated. Call web_fetch again with startIndex=" + itoa(end) + " to continue.]"
	}
	return head + "\n\n" + text[startIndex:end] + tail
}

func (s *Service) fetchText(ctx context.Context, target string) string {
	key := "fetch\x00" + target
	if cached, ok := s.cache.get(key, s.now()); ok {
		return cached
	}
	raw, _, contentType, charset, err := s.openURL(ctx, target, MaxBodyBytes, "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if err != nil {
		return "Error: could not fetch " + target + " (" + errType(err) + ")."
	}
	if !isTextualContentType(contentType) {
		main := strings.TrimSpace(strings.Split(contentType, ";")[0])
		if main == "" {
			main = "a binary resource"
		}
		return "Error: " + target + " is " + main + ", which is not a text page the gateway can read."
	}
	text := stripTags(decodeText(raw, charset))
	if s.CacheTTL > 0 && len(text) <= 200000 {
		s.cache.put(key, text, s.CacheTTL, s.now())
	}
	return text
}

// Execute runs one internal tool and always returns a model-readable string.
func (s *Service) Execute(ctx context.Context, name string, args any) (result string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = "Error: gateway web tool failed."
		}
	}()
	parsed := ArgsFromAny(args)
	switch strings.TrimSpace(name) {
	case WebSearchName:
		return s.Search(ctx, QueryArgs(parsed), intArg(parsed["numResults"], 5))
	case WebFetchName:
		return s.Fetch(ctx, URLArg(parsed), intArg(parsed["startIndex"], 0))
	default:
		return "Error: " + strings.TrimSpace(name) + " is not a tool this gateway runs."
	}
}

// Package-level wrappers use Default.
func Search(ctx context.Context, query string, n int) string { return Default.Search(ctx, query, n) }
func Fetch(ctx context.Context, url string, startIndex int) string {
	return Default.Fetch(ctx, url, startIndex)
}
func Execute(ctx context.Context, name string, args any) string {
	return Default.Execute(ctx, name, args)
}

func intArg(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		number, err := typed.Int64()
		if err == nil {
			return int(number)
		}
	}
	return fallback
}

func errType(err error) string {
	if err == nil {
		return "unknown"
	}
	var urlErr *net.OpError
	if errors.As(err, &urlErr) {
		return "network error"
	}
	text := err.Error()
	if index := strings.Index(text, ":"); index > 0 {
		return text[:index]
	}
	return "network error"
}

func quoteRunes(text string) string {
	raw, _ := json.Marshal(text)
	return string(raw)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
