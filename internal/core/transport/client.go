// Package transport provides the shared HTTP client used by every provider.
// Transports are reused per normalized proxy URL, and a direct request never
// consults the process proxy environment.
package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Slot is one configured proxy slot from the legacy settings file.
type Slot struct {
	ID      string
	Name    string
	URL     string
	Enabled bool
}

// Slots is an ordered list of proxy slots.
type Slots []Slot

// URL resolves a slot id to its proxy URL. An empty id, an unknown slot, and a
// disabled slot all resolve to "" (direct).
func (s Slots) URL(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	for _, slot := range s {
		if slot.ID != id {
			continue
		}
		if !slot.Enabled {
			return ""
		}
		return slot.URL
	}
	return ""
}

// SlotsFromSettings reads the legacy proxy_slots list from a settings map.
// Unusable entries are skipped, duplicate URLs are dropped (first wins), and
// entries stored without an id receive a legacy slot-N id. Matching the old
// Python behavior, a missing enabled field means enabled.
func SlotsFromSettings(raw map[string]any) Slots {
	entries, ok := proxySlotEntries(raw["proxy_slots"])
	if !ok {
		return nil
	}

	slots := make(Slots, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, value := range entries {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		proxyURL := normalizeProxyURL(stringValue(entry["url"]))
		if proxyURL == "" || seen[proxyURL] {
			continue
		}
		seen[proxyURL] = true

		slot := Slot{
			ID:      strings.TrimSpace(stringValue(entry["id"])),
			Name:    strings.TrimSpace(stringValue(entry["name"])),
			URL:     proxyURL,
			Enabled: true,
		}
		if enabled, ok := entry["enabled"].(bool); ok {
			slot.Enabled = enabled
		}
		if slot.ID == "" {
			slot.ID = nextSlotID(slots)
		}
		slots = append(slots, slot)
	}
	return slots
}

// proxySlotEntries accepts the shapes a decoded JSON list and hand-built test
// values can have.
func proxySlotEntries(value any) ([]any, bool) {
	switch entries := value.(type) {
	case []any:
		return entries, true
	case []map[string]any:
		out := make([]any, len(entries))
		for i, entry := range entries {
			out[i] = entry
		}
		return out, true
	default:
		return nil, false
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

// nextSlotID returns the next unused legacy slot-N id.
func nextSlotID(existing []Slot) string {
	highest := 0
	for _, slot := range existing {
		if number, ok := slotNumber(slot.ID); ok && number > highest {
			highest = number
		}
	}
	return fmt.Sprintf("slot-%d", highest+1)
}

func slotNumber(id string) (int, bool) {
	const prefix = "slot-"
	if !strings.HasPrefix(id, prefix) {
		return 0, false
	}
	digits := id[len(prefix):]
	if digits == "" {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	number, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return number, true
}

// normalizeProxyURL trims whitespace, supplies the default http scheme, and
// converts the old vendor host:port:user:pass form. It mirrors the Python
// normalize_proxy_url helper so migrated settings keep working.
func normalizeProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	scheme := "http"
	if index := strings.Index(raw, "://"); index >= 0 {
		scheme = strings.ToLower(strings.TrimSpace(raw[:index]))
		if scheme == "" {
			scheme = "http"
		}
		raw = strings.TrimSpace(raw[index+len("://"):])
	}
	// net/http only understands socks5. Its SOCKS5 client sends host names to
	// the proxy, so the socks5h behavior is already what we get. Normalizing
	// also lets socks5h and socks5 share one cached transport.
	if scheme == "socks5h" {
		scheme = "socks5"
	}

	parts := strings.Split(raw, ":")
	if len(parts) == 4 && allDigits(parts[1]) {
		return scheme + "://" + parts[2] + ":" + parts[3] + "@" + parts[0] + ":" + parts[1]
	}
	return scheme + "://" + raw
}

func allDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ClientOptions configures a Client.
type ClientOptions struct {
	// Timeout bounds the whole request, including body reads. Zero means no
	// timeout.
	Timeout time.Duration
}

// Client is a shared HTTP client whose transport is selected per request.
type Client struct {
	timeout time.Duration

	mu         sync.Mutex
	transports map[string]*http.Transport
}

// NewClient returns a Client with the given options.
func NewClient(opts ClientOptions) *Client {
	return &Client{
		timeout:    opts.Timeout,
		transports: make(map[string]*http.Transport),
	}
}

// Do sends req through the proxy at proxyURL, or directly when proxyURL is
// empty. Transports are reused per normalized proxy URL.
func (c *Client) Do(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("transport: nil request")
	}
	if ctx == nil {
		return nil, errors.New("transport: nil context")
	}

	tr, err := c.transportFor(proxyURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: tr, Timeout: c.timeout}
	return client.Do(req.WithContext(ctx))
}

// Close closes idle connections for every cached transport and clears the
// cache. A later request builds a fresh transport.
func (c *Client) Close() {
	c.mu.Lock()
	transports := c.transports
	c.transports = make(map[string]*http.Transport)
	c.mu.Unlock()

	for _, tr := range transports {
		tr.CloseIdleConnections()
	}
}

// transportFor returns the cached transport for a normalized proxy URL,
// creating it on first use.
func (c *Client) transportFor(proxyURL string) (*http.Transport, error) {
	key := normalizeProxyURL(proxyURL)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.transports == nil {
		c.transports = make(map[string]*http.Transport)
	}
	if tr, ok := c.transports[key]; ok {
		return tr, nil
	}
	tr, err := newTransport(key)
	if err != nil {
		return nil, err
	}
	c.transports[key] = tr
	return tr, nil
}

// newTransport builds a transport with the sane defaults of
// http.DefaultTransport. Direct transports have Proxy nil, which ignores the
// proxy environment.
func newTransport(proxyURL string) (*http.Transport, error) {
	tr := clonedDefaultTransport()
	tr.Proxy = nil
	if proxyURL == "" {
		return tr, nil
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		// Do not echo the parse error: it can contain raw userinfo.
		return nil, errors.New("transport: invalid proxy URL")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5":
	default:
		return nil, fmt.Errorf("transport: unsupported proxy scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("transport: proxy URL has no host")
	}

	tr.Proxy = http.ProxyURL(parsed)
	return tr, nil
}

func clonedDefaultTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}
	return &http.Transport{}
}
