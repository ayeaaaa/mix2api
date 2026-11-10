// Package oracle records selected v1.6.5 fake-upstream responses and compares
// normalized v2 responses against them. It only contacts loopback addresses;
// tests use httptest servers and never touch the real network.
package oracle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/tests/contract"
)

const Format = "mix2api-oracle/1"

// Auth selects the credential header a case needs.
type Auth string

const (
	AuthNone  Auth = "none"
	AuthPanel Auth = "panel"
	AuthAPI   Auth = "api"
)

// Case is one recorded endpoint request.
type Case struct {
	Name    string            `json:"name"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Body    json.RawMessage   `json:"body,omitempty"`
	Auth    Auth              `json:"auth"`
	Kind    string            `json:"kind"` // json or sse
	Status  int               `json:"status"`
	File    string            `json:"file,omitempty"`
	SHA256  string            `json:"sha256,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Provenance is the honesty record stored beside every oracle fixture.
type Provenance struct {
	TargetVersion string `json:"target_version"`
	TargetCommit  string `json:"target_commit"`
	RecordedAt    string `json:"recorded_at"`
	BaseURL       string `json:"base_url"`
	FakeUpstream  bool   `json:"fake_upstream"`
	Network       string `json:"network"`
	Note          string `json:"note,omitempty"`
}

// Document is the oracle manifest.
type Document struct {
	Format     string     `json:"format"`
	Provenance Provenance `json:"provenance"`
	Cases      []Case     `json:"cases"`
}

// Credentials supplies panel/API headers for a target.
type Credentials struct {
	PanelToken string
	APIKey     string
}

// DefaultCases is the selected v1.6.5 output matrix required by Gate F. Each
// case hits a route the v2 server implements; provider-specific detail is
// selected through the Raccoon realm, matching the documented v1 oracle setup.
func DefaultCases() []Case {
	chat := func(stream bool) json.RawMessage {
		value := map[string]any{
			"model": "raccoon-090dd3",
			"messages": []any{
				map[string]any{"role": "user", "content": "hello"},
			},
			"stream": stream,
		}
		raw, _ := json.Marshal(value)
		return raw
	}
	return []Case{
		{Name: "health", Method: http.MethodGet, Path: "/health?force=1", Auth: AuthNone, Kind: "json"},
		{Name: "models", Method: http.MethodGet, Path: "/v1/models?realm=raccoon", Auth: AuthAPI, Kind: "json"},
		{Name: "chat", Method: http.MethodPost, Path: "/v1/chat/completions?realm=raccoon", Body: chat(false), Auth: AuthAPI, Kind: "json"},
		{Name: "chat_stream", Method: http.MethodPost, Path: "/v1/chat/completions?realm=raccoon", Body: chat(true), Auth: AuthAPI, Kind: "sse"},
		{Name: "responses", Method: http.MethodPost, Path: "/v1/responses?realm=raccoon", Body: json.RawMessage(`{"model":"raccoon-090dd3","input":"hello","stream":false}`), Auth: AuthAPI, Kind: "json"},
		{Name: "responses_stream", Method: http.MethodPost, Path: "/v1/responses?realm=raccoon", Body: json.RawMessage(`{"model":"raccoon-090dd3","input":"hello","stream":true}`), Auth: AuthAPI, Kind: "sse"},
		{Name: "messages", Method: http.MethodPost, Path: "/v1/messages?realm=raccoon", Body: json.RawMessage(`{"model":"raccoon-090dd3","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`), Auth: AuthAPI, Kind: "json"},
		{Name: "admin_accounts", Method: http.MethodGet, Path: "/raccoon/accounts", Auth: AuthPanel, Kind: "json"},
		{Name: "media_models", Method: http.MethodGet, Path: "/v1/media/models", Auth: AuthAPI, Kind: "json"},
		{Name: "agents", Method: http.MethodGet, Path: "/agents", Auth: AuthPanel, Kind: "json"},
		{Name: "updates", Method: http.MethodGet, Path: "/updates", Auth: AuthPanel, Kind: "json"},
		{Name: "updates_backups", Method: http.MethodGet, Path: "/updates/backups", Auth: AuthPanel, Kind: "json"},
		{Name: "usage", Method: http.MethodGet, Path: "/usage", Auth: AuthPanel, Kind: "json"},
		{Name: "logs", Method: http.MethodGet, Path: "/logs", Auth: AuthPanel, Kind: "json"},
	}
}

// Recorder captures one target.
type Recorder struct {
	BaseURL    string
	Client     *http.Client
	Creds      Credentials
	Cases      []Case
	Provenance Provenance
	Now        func() time.Time
}

// Record fetches every case and writes raw responses plus oracle.json.
func (r *Recorder) Record(ctx context.Context, outDir string) error {
	if err := validateLoopback(r.BaseURL); err != nil {
		return err
	}
	if !r.Provenance.FakeUpstream {
		return errors.New("oracle: refusing to record without --fake-upstream")
	}
	if len(r.Cases) == 0 {
		r.Cases = DefaultCases()
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("oracle: output directory is not empty")
	}
	document := Document{Format: Format, Provenance: r.Provenance}
	document.Provenance.RecordedAt = r.Now().UTC().Format(time.RFC3339)
	document.Provenance.Network = "loopback-only"
	if document.Provenance.BaseURL == "" {
		document.Provenance.BaseURL = r.BaseURL
	}
	for _, item := range r.Cases {
		status, body, err := r.fetch(ctx, item)
		if err != nil {
			return fmt.Errorf("oracle: record %s: %w", item.Name, err)
		}
		item.Status = status
		item.File = item.Name + "." + item.Kind
		sum := sha256.Sum256(body)
		item.SHA256 = hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(outDir, item.File), body, 0o644); err != nil {
			return err
		}
		document.Cases = append(document.Cases, item)
	}
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "oracle.json"), append(raw, '\n'), 0o644)
}

// Load reads and integrity-checks an oracle directory.
func Load(dir string) (*Document, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "oracle.json"))
	if err != nil {
		return nil, err
	}
	var document Document
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if document.Format != Format {
		return nil, fmt.Errorf("oracle: unsupported format %q", document.Format)
	}
	if !document.Provenance.FakeUpstream {
		return nil, errors.New("oracle: fixture does not declare a fake upstream")
	}
	if document.Provenance.Network != "loopback-only" {
		return nil, errors.New("oracle: fixture does not declare loopback-only provenance")
	}
	if len(document.Cases) == 0 {
		return nil, errors.New("oracle: no cases")
	}
	for _, item := range document.Cases {
		body, err := os.ReadFile(filepath.Join(dir, item.File))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		if item.SHA256 != hex.EncodeToString(sum[:]) {
			return nil, fmt.Errorf("oracle: checksum mismatch for %s", item.File)
		}
	}
	return &document, nil
}

// Comparer fetches a target and compares normalized output with a fixture.
type Comparer struct {
	BaseURL string
	Client  *http.Client
	Creds   Credentials
	Dir     string
}

// Compare returns the first mismatch, or nil when every case matches.
func (c *Comparer) Compare(ctx context.Context) error {
	if err := validateLoopback(c.BaseURL); err != nil {
		return err
	}
	document, err := Load(c.Dir)
	if err != nil {
		return err
	}
	for _, item := range document.Cases {
		want, err := os.ReadFile(filepath.Join(c.Dir, item.File))
		if err != nil {
			return err
		}
		status, got, err := c.fetch(ctx, item)
		if err != nil {
			return fmt.Errorf("oracle: compare %s: %w", item.Name, err)
		}
		if status != item.Status {
			return fmt.Errorf("oracle: %s status %d, want %d", item.Name, status, item.Status)
		}
		if err := compareBody(item.Kind, want, got); err != nil {
			return fmt.Errorf("oracle: %s: %w", item.Name, err)
		}
	}
	return nil
}

func (r *Recorder) fetch(ctx context.Context, item Case) (int, []byte, error) {
	request, err := r.build(ctx, item)
	if err != nil {
		return 0, nil, err
	}
	return doRequest(r.Client, request)
}

func (c *Comparer) fetch(ctx context.Context, item Case) (int, []byte, error) {
	request, err := buildRequest(ctx, c.BaseURL, c.Creds, item)
	if err != nil {
		return 0, nil, err
	}
	return doRequest(c.Client, request)
}

func (r *Recorder) build(ctx context.Context, item Case) (*http.Request, error) {
	return buildRequest(ctx, r.BaseURL, r.Creds, item)
}

func buildRequest(ctx context.Context, baseURL string, creds Credentials, item Case) (*http.Request, error) {
	target, err := joinURL(baseURL, item.Path)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if len(item.Body) > 0 {
		body = bytes.NewReader(item.Body)
	}
	request, err := http.NewRequestWithContext(ctx, item.Method, target, body)
	if err != nil {
		return nil, err
	}
	if len(item.Body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range item.Headers {
		request.Header.Set(key, value)
	}
	switch item.Auth {
	case AuthPanel:
		if creds.PanelToken == "" {
			return nil, errors.New("oracle: panel token is required")
		}
		request.Header.Set("X-Panel-Token", creds.PanelToken)
	case AuthAPI:
		if creds.APIKey == "" {
			return nil, errors.New("oracle: API key is required")
		}
		request.Header.Set("Authorization", "Bearer "+creds.APIKey)
	}
	return request, nil
}

func doRequest(client *http.Client, request *http.Request) (int, []byte, error) {
	if client == nil {
		client = loopbackClient()
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return 0, nil, err
	}
	if response.ContentLength > 32<<20 {
		return 0, nil, errors.New("oracle: response is too large")
	}
	return response.StatusCode, body, nil
}

func joinURL(baseURL, path string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("oracle: base URL must be http or https")
	}
	reference, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	return parsed.ResolveReference(reference).String(), nil
}

func validateLoopback(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return errors.New("oracle: invalid base URL")
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("oracle: refusing non-loopback target %q", host)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("oracle: base URL must be http or https")
	}
	return nil
}

// loopbackClient never follows a redirect off loopback.
func loopbackClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("oracle: too many redirects")
			}
			return validateLoopback(request.URL.String())
		},
	}
}

func compareBody(kind string, want, got []byte) error {
	switch kind {
	case "sse":
		wantEvents, err := contract.NormalizeSSE(want)
		if err != nil {
			return fmt.Errorf("normalize oracle: %w", err)
		}
		gotEvents, err := contract.NormalizeSSE(got)
		if err != nil {
			return fmt.Errorf("normalize target: %w", err)
		}
		if !reflect.DeepEqual(wantEvents, gotEvents) {
			return fmt.Errorf("SSE mismatch\nwant: %s\ngot:  %s", summarize(wantEvents), summarize(gotEvents))
		}
	case "json":
		wantValue, err := contract.NormalizeJSON(want)
		if err != nil {
			return fmt.Errorf("normalize oracle: %w", err)
		}
		gotValue, err := contract.NormalizeJSON(got)
		if err != nil {
			return fmt.Errorf("normalize target: %w", err)
		}
		if !reflect.DeepEqual(wantValue, gotValue) {
			return fmt.Errorf("JSON mismatch\nwant: %s\ngot:  %s", summarize(wantValue), summarize(gotValue))
		}
	default:
		if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
			return errors.New("raw body mismatch")
		}
	}
	return nil
}

func summarize(value any) string {
	switch typed := value.(type) {
	case []contract.Event:
		names := make([]string, 0, len(typed))
		for _, event := range typed {
			names = append(names, event.Type)
		}
		return "events=" + strings.Join(names, ",")
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		if len(raw) > 1200 {
			raw = raw[:1200]
		}
		return string(raw)
	}
}

// SortedCaseNames returns fixture names in stable order for reports.
func SortedCaseNames(document *Document) []string {
	names := make([]string, 0, len(document.Cases))
	for _, item := range document.Cases {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	return names
}
