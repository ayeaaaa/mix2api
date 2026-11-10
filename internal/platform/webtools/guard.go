package webtools

import (
	"compress/gzip"
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/core/netguard"
	"time"
)

const (
	MaxBodyBytes    = 4 * 1024 * 1024
	SearchBodyBytes = 3 * 1024 * 1024
	MaxFetchChars   = 100000
	MaxResults      = 10
	HTTPTimeout     = 20
)

var charsetPattern = regexp.MustCompile(`(?i)charset\s*=\s*["']?([\w-]+)`)
var metaCharsetPattern = regexp.MustCompile(`(?is)<meta[^>]+charset\s*=\s*["']?([A-Za-z0-9_-]+)`)

// GuardURL validates scheme, credentials, host shape and every resolved IP.
func (s *Service) GuardURL(raw string, resolve bool) (string, error) {
	text := strings.TrimSpace(raw)
	parsed, err := url.Parse(text)
	if err != nil {
		return "", fmt.Errorf("Invalid URL.")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("Only http:// or https:// URLs are supported.")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("Credentials in the URL are not allowed.")
	}
	if s != nil && s.AllowPrivate {
		return parsed.String(), nil
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || strings.HasSuffix(host, ".localhost") {
		return "", fmt.Errorf("Private, loopback or single-label hosts are not allowed.")
	}
	literal := net.ParseIP(host) != nil
	if literal {
		if !s.publicIP(net.ParseIP(host)) {
			return "", fmt.Errorf("Private network addresses are not allowed.")
		}
	} else {
		if !strings.Contains(host, ".") {
			return "", fmt.Errorf("Private, loopback or single-label hosts are not allowed.")
		}
		if strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "10.") ||
			strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "169.254.") ||
			strings.HasPrefix(host, "0.") || regexp.MustCompile(`^172\.(1[6-9]|2\d|3[01])\.`).MatchString(host) {
			return "", fmt.Errorf("Private network addresses are not allowed.")
		}
	}
	if resolve && !literal {
		ips, err := s.lookupHost(host)
		if err != nil || len(ips) == 0 {
			return "", fmt.Errorf("could not resolve %s", host)
		}
		for _, ip := range ips {
			if !s.publicIP(ip) {
				return "", fmt.Errorf("%s resolves to a non-public address (%s)", host, ip)
			}
		}
	}
	return parsed.String(), nil
}

func (s *Service) publicIP(ip net.IP) bool { return netguard.IsPublicIP(ip) }

func (s *Service) lookupHost(host string) ([]net.IP, error) {
	if s != nil && s.LookupIP != nil {
		return s.LookupIP(host)
	}
	return net.LookupIP(host)
}

func (s *Service) openURL(ctx context.Context, target string, maxBytes int, accept string) ([]byte, int, string, string, error) {
	if maxBytes <= 0 {
		maxBytes = MaxBodyBytes
	}
	if _, err := s.GuardURL(target, false); err != nil {
		return nil, 0, "", "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, "", "", err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", accept)
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Accept-Encoding", "gzip")
	client := http.Client{Timeout: HTTPTimeout * time.Second}
	if s != nil && s.Client != nil {
		client = *s.Client
	}
	base := client
	previous := base.CheckRedirect
	base.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if _, err := s.GuardURL(req.URL.String(), true); err != nil {
			return err
		}
		if len(via) > 5 {
			return fmt.Errorf("too many redirects")
		}
		if previous != nil {
			return previous(req, via)
		}
		return nil
	}
	s.throttle(ctx)
	response, err := base.Do(request)
	if err != nil {
		return nil, 0, "", "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, response.StatusCode, "", "", err
	}
	if len(raw) > maxBytes {
		raw = raw[:maxBytes]
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Encoding")), "gzip") {
		if reader, gzipErr := gzip.NewReader(strings.NewReader(string(raw))); gzipErr == nil {
			if inflated, readErr := io.ReadAll(io.LimitReader(reader, int64(maxBytes)+1)); readErr == nil {
				raw = inflated
				if len(raw) > maxBytes {
					raw = raw[:maxBytes]
				}
			}
		}
	}
	contentType := response.Header.Get("Content-Type")
	charset := ""
	if match := charsetPattern.FindStringSubmatch(contentType); match != nil {
		charset = match[1]
	}
	return raw, response.StatusCode, contentType, charset, nil
}

func decodeText(raw []byte, charset string) string {
	candidates := []string{charset}
	if match := metaCharsetPattern.FindSubmatch(raw); match != nil {
		candidates = append(candidates, string(match[1]))
	}
	candidates = append(candidates, "utf-8")
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if decoded, err := decodeCharset(raw, candidate); err == nil {
			return decoded
		}
	}
	return string(raw)
}

func isTextualContentType(contentType string) bool {
	if strings.TrimSpace(contentType) == "" {
		return true
	}
	main := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if strings.HasPrefix(main, "text/") {
		return true
	}
	switch main {
	case "application/json", "application/xml", "application/xhtml+xml", "application/rss+xml",
		"application/atom+xml", "application/ld+json", "application/javascript", "application/x-javascript":
		return true
	}
	return strings.HasSuffix(main, "+json") || strings.HasSuffix(main, "+xml")
}

var scriptStylePattern = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(script|style|noscript)>`)
var breakPattern = regexp.MustCompile(`(?is)<br\s*/?>`)
var blockEndPattern = regexp.MustCompile(`(?is)</(p|div|li|tr|h[1-6])>`)
var tagPattern = regexp.MustCompile(`(?s)<[^>]+>`)
var whitespacePattern = regexp.MustCompile(`[ \t\f\v]+`)
var newlinePattern = regexp.MustCompile(`\s*\n\s*`)
var blankLinesPattern = regexp.MustCompile(`\n{3,}`)

func stripTags(text string) string {
	text = scriptStylePattern.ReplaceAllString(text, " ")
	text = breakPattern.ReplaceAllString(text, "\n")
	text = blockEndPattern.ReplaceAllString(text, "\n")
	text = tagPattern.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = strings.ReplaceAll(text, "\u00a0", " ")
	text = whitespacePattern.ReplaceAllString(text, " ")
	text = newlinePattern.ReplaceAllString(text, "\n")
	text = blankLinesPattern.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

func decodeCharset(raw []byte, charset string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(charset))
	switch name {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return string(raw), nil
	case "iso-8859-1", "latin1", "latin-1", "windows-1252", "cp1252":
		var builder strings.Builder
		for _, value := range raw {
			if name == "windows-1252" || name == "cp1252" {
				if mapped, ok := cp1252[value]; ok {
					builder.WriteRune(mapped)
					continue
				}
			}
			builder.WriteRune(rune(value))
		}
		return builder.String(), nil
	default:
		return "", fmt.Errorf("unsupported charset %s", charset)
	}
}

var cp1252 = map[byte]rune{
	0x80: '\u20ac', 0x82: '\u201a', 0x83: '\u0192', 0x84: '\u201e', 0x85: '\u2026',
	0x86: '\u2020', 0x87: '\u2021', 0x88: '\u02c6', 0x89: '\u2030', 0x8a: '\u0160',
	0x8b: '\u2039', 0x8c: '\u0152', 0x8e: '\u017d', 0x91: '\u2018', 0x92: '\u2019',
	0x93: '\u201c', 0x94: '\u201d', 0x95: '\u2022', 0x96: '\u2013', 0x97: '\u2014',
	0x98: '\u02dc', 0x99: '\u2122', 0x9a: '\u0161', 0x9b: '\u203a', 0x9c: '\u0153',
	0x9e: '\u017e', 0x9f: '\u0178',
}
