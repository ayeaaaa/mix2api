package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

const (
	defaultProbeURL      = "https://api.ipify.org"
	defaultGeoURL        = "http://ip-api.com/json"
	defaultDiscoverHost  = "cli-proxy-mihomo"
	defaultDiscoverPorts = "17901-17910"
	probeTimeout         = 12 * time.Second
	geoTimeout           = 8 * time.Second
	discoverTimeout      = 45 * time.Second
	discoverWorkers      = 8
	maxProbeBody         = 256
	maxDiscoverPorts     = 64
	geoFields            = "status,message,country,countryCode,isp,org,as,hosting"
)

var discoverRangeSpace = regexp.MustCompile(`\s*-\s*`)

type intelFields struct {
	Country     string
	CountryCode string
	IPType      string
	ISP         string
	ASN         string
}

// Probe checks one proxy exit through the same transport used by the account
// pools. Reachability failures are returned as ok=false with a safe error;
// only unexpected internal failures use the error return.
func Probe(ctx context.Context, proxyURL string) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized := transport.NormalizeProxyURL(proxyURL)
	if normalized == "" {
		return probeResult(false, "", 0, "代理地址为空", intelFields{}), nil
	}

	target := strings.TrimSpace(os.Getenv("WB_PROXY_PROBE_URL"))
	if target == "" {
		target = defaultProbeURL
	}
	started := time.Now()
	exitIP, err := probeExit(ctx, normalized, target)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return probeResult(false, "", latency, safeProbeError(err, normalized), intelFields{}), nil
	}
	intel := lookupIntel(ctx, exitIP)
	return probeResult(true, exitIP, latency, "", intel), nil
}

// Discover probes the configured mihomo host/ports. The host and port list are
// intentionally environment-driven, matching the v1 bridge defaults:
// WB_PROXY_DISCOVER_HOST (default cli-proxy-mihomo) and
// WB_PROXY_DISCOVER_PORTS (default 17901-17910; ranges or comma lists).
func Discover(ctx context.Context) []map[string]any {
	if ctx == nil {
		ctx = context.Background()
	}
	host := strings.TrimSpace(os.Getenv("WB_PROXY_DISCOVER_HOST"))
	if host == "" {
		host = defaultDiscoverHost
	}
	rawPorts := strings.TrimSpace(os.Getenv("WB_PROXY_DISCOVER_PORTS"))
	if rawPorts == "" {
		rawPorts = defaultDiscoverPorts
	}
	ports := discoverPorts(rawPorts)
	if len(ports) == 0 {
		return []map[string]any{}
	}

	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	results := make([]map[string]any, len(ports))
	var wg sync.WaitGroup
	sem := make(chan struct{}, discoverWorkers)
	for index, port := range ports {
		wg.Add(1)
		go func(index, port int) {
			defer wg.Done()
			targetURL := fmt.Sprintf("http://%s:%d", discoverHost(host), port)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[index] = probeResult(false, "", 0, "发现已取消", intelFields{})
				results[index]["url"] = targetURL
				results[index]["reachable"] = false
				return
			}
			result, _ := Probe(ctx, targetURL)
			result["url"] = targetURL
			result["reachable"], _ = result["ok"].(bool)
			results[index] = result
		}(index, port)
	}
	wg.Wait()
	return results
}

func probeExit(ctx context.Context, proxyURL, target string) (string, error) {
	client := transport.NewClient(transport.ClientOptions{
		Timeout:         probeTimeout,
		RefuseRedirects: true,
	})
	defer client.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "mix2api-proxy-probe")
	resp, err := client.Do(ctx, req, proxyURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxProbeBody {
		return "", errors.New("probe response too large")
	}
	text := strings.TrimSpace(string(body))
	if net.ParseIP(text) == nil {
		return "", errors.New("probe response is not an IP address")
	}
	return text, nil
}

func lookupIntel(ctx context.Context, ip string) intelFields {
	endpoint := geoEndpoint(ip)
	if endpoint == "" {
		return intelFields{}
	}
	client := transport.NewClient(transport.ClientOptions{
		Timeout:         geoTimeout,
		RefuseRedirects: true,
	})
	defer client.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return intelFields{}
	}
	req.Header.Set("User-Agent", "mix2api-proxy-probe")
	resp, err := client.Do(ctx, req, "")
	if err != nil {
		return intelFields{}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return intelFields{}
	}
	var payload struct {
		Status      string `json:"status"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		ISP         string `json:"isp"`
		Org         string `json:"org"`
		AS          string `json:"as"`
		Hosting     *bool  `json:"hosting"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil || payload.Status != "success" {
		return intelFields{}
	}
	ipType := ""
	if payload.Hosting != nil {
		if *payload.Hosting {
			ipType = "datacenter"
		} else {
			ipType = "residential"
		}
	}
	isp := strings.TrimSpace(payload.ISP)
	if isp == "" {
		isp = strings.TrimSpace(payload.Org)
	}
	return intelFields{
		Country:     strings.TrimSpace(payload.Country),
		CountryCode: strings.ToUpper(strings.TrimSpace(payload.CountryCode)),
		IPType:      ipType,
		ISP:         isp,
		ASN:         strings.TrimSpace(payload.AS),
	}
}

func probeResult(ok bool, exitIP string, latency int64, errText string, intel intelFields) map[string]any {
	intelOK := intel.Country != "" || intel.CountryCode != "" || intel.IPType != "" || intel.ISP != "" || intel.ASN != ""
	return map[string]any{
		"ok":           ok,
		"exit_ip":      exitIP,
		"latency_ms":   latency,
		"error":        errText,
		"intel_ok":     intelOK,
		"country":      intel.Country,
		"country_code": intel.CountryCode,
		"ip_type":      intel.IPType,
		"isp":          intel.ISP,
		"asn":          intel.ASN,
	}
}

func safeProbeError(err error, proxyURL string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, raw := range []string{proxyURL, transport.NormalizeProxyURL(proxyURL)} {
		if raw == "" {
			continue
		}
		if parsed, parseErr := url.Parse(raw); parseErr == nil && parsed.User != nil {
			message = strings.ReplaceAll(message, parsed.User.String(), "***")
		}
		message = strings.ReplaceAll(message, raw, "[proxy]")
	}
	if len(message) > 240 {
		message = message[:240]
	}
	return message
}

func geoEndpoint(ip string) string {
	base := strings.TrimSpace(os.Getenv("WB_PROXY_GEO_URL"))
	if base == "" {
		base = defaultGeoURL
	}
	escapedIP := url.PathEscape(ip)
	var endpoint string
	if strings.Contains(base, "%s") {
		endpoint = fmt.Sprintf(base, escapedIP)
	} else {
		parsed, err := url.Parse(base)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return ""
		}
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + escapedIP
		query := parsed.Query()
		query.Set("lang", "zh-CN")
		query.Set("fields", geoFields)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	query := parsed.Query()
	query.Set("lang", "zh-CN")
	query.Set("fields", geoFields)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func discoverHost(host string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]"
	}
	return host
}

func discoverPorts(raw string) []int {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	raw = discoverRangeSpace.ReplaceAllString(raw, "-")
	out := make([]int, 0, 16)
	seen := map[int]bool{}
	add := func(port int) {
		if port < 1 || port > 65535 || len(out) >= maxDiscoverPorts || seen[port] {
			return
		}
		seen[port] = true
		out = append(out, port)
	}
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	}) {
		if low, high, hasRange := strings.Cut(field, "-"); hasRange {
			start, startErr := strconv.Atoi(strings.TrimSpace(low))
			end, endErr := strconv.Atoi(strings.TrimSpace(high))
			if startErr != nil || endErr != nil || start > end {
				continue
			}
			for port := start; port <= end && len(out) < maxDiscoverPorts; port++ {
				add(port)
			}
			continue
		}
		port, err := strconv.Atoi(strings.TrimSpace(field))
		if err == nil {
			add(port)
		}
	}
	return out
}
