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
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
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

// ControlRequest is a deterministic fake-upstream setup call made before a
// case. It is test scaffolding, never a request to the recorded target.
type ControlRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

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
	// Group documents which recording run produced the case. It is not used
	// for matching but keeps a fixture's provenance readable.
	Group string `json:"group,omitempty"`
	// Setup runs against the fake control server before the target request.
	Setup *ControlRequest `json:"setup,omitempty"`
	// Observe fetches a deterministic fake-upstream counter snapshot after
	// the target request. ObserveFile/SHA256 commit that snapshot beside the
	// response body so exactly-once/retry behaviour is compared, not inferred.
	Observe       string `json:"observe,omitempty"`
	ObserveFile   string `json:"observe_file,omitempty"`
	ObserveSHA256 string `json:"observe_sha256,omitempty"`
	// Capture names values extracted from a JSON response so later cases can
	// address resources the target generated (a video task id, its signed
	// content URL). Keys are template names; values are dotted paths such as
	// "id" or "data.0.url". Templates use {{name}} in Path or Observe.
	Capture map[string]string `json:"capture,omitempty"`
	// DelayMS waits before the request. It is only used for asynchronous
	// resources whose background worker must reach a stable state first.
	DelayMS int `json:"delay_ms,omitempty"`
}

// Provenance is the honesty record stored beside every oracle fixture.
type Provenance struct {
	TargetVersion string `json:"target_version"`
	TargetCommit  string `json:"target_commit"`
	RecordedAt    string `json:"recorded_at"`
	BaseURL       string `json:"base_url"`
	FakeUpstream  bool   `json:"fake_upstream"`
	Network       string `json:"network"`
	FakeControl   string `json:"fake_control_url,omitempty"`
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
// case hits a route the v2 server implements. The core matrix uses Raccoon;
// CodeArts is included as the provider-specific slice that has a standalone
// v1 fake upstream. WorkBuddy intl has no v1 fake and hardcodes its origin, so
// it remains a documented pending case.
func DefaultCases() []Case {
	chat := func(model string, stream bool) json.RawMessage {
		value := map[string]any{
			"model": model,
			"messages": []any{
				map[string]any{"role": "user", "content": "hello"},
			},
			"stream": stream,
		}
		raw, _ := json.Marshal(value)
		return raw
	}
	// Models is fetched before health on purpose: v1 reports the live
	// catalog source only after a catalog request has populated the pool
	// cache. Recording health first would capture the seed fallback.
	return []Case{
		{Name: "models", Method: http.MethodGet, Path: "/v1/models?realm=raccoon", Auth: AuthAPI, Kind: "json"},
		{Name: "codearts_models", Method: http.MethodGet, Path: "/v1/models?realm=codearts", Auth: AuthAPI, Kind: "json"},
		{Name: "health", Method: http.MethodGet, Path: "/health?force=1", Auth: AuthNone, Kind: "json"},
		{Name: "chat", Method: http.MethodPost, Path: "/v1/chat/completions?realm=raccoon", Body: chat("raccoon-090dd3", false), Auth: AuthAPI, Kind: "json"},
		{Name: "chat_stream", Method: http.MethodPost, Path: "/v1/chat/completions?realm=raccoon", Body: chat("raccoon-090dd3", true), Auth: AuthAPI, Kind: "sse"},
		{Name: "responses", Method: http.MethodPost, Path: "/v1/responses?realm=raccoon", Body: json.RawMessage(`{"model":"raccoon-090dd3","input":"hello","stream":false}`), Auth: AuthAPI, Kind: "json"},
		{Name: "responses_stream", Method: http.MethodPost, Path: "/v1/responses?realm=raccoon", Body: json.RawMessage(`{"model":"raccoon-090dd3","input":"hello","stream":true}`), Auth: AuthAPI, Kind: "sse"},
		{Name: "messages", Method: http.MethodPost, Path: "/v1/messages?realm=raccoon", Body: json.RawMessage(`{"model":"raccoon-090dd3","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`), Auth: AuthAPI, Kind: "json"},
		{Name: "codearts_chat", Method: http.MethodPost, Path: "/v1/chat/completions?realm=codearts", Body: chat("GLM-5.2", false), Auth: AuthAPI, Kind: "json"},
		{Name: "codearts_chat_stream", Method: http.MethodPost, Path: "/v1/chat/completions?realm=codearts", Body: chat("GLM-5.2", true), Auth: AuthAPI, Kind: "sse"},
		{Name: "admin_accounts", Method: http.MethodGet, Path: "/raccoon/accounts", Auth: AuthPanel, Kind: "json"},
		{Name: "codearts_accounts", Method: http.MethodGet, Path: "/codearts/accounts", Auth: AuthPanel, Kind: "json"},
		{Name: "media_models", Method: http.MethodGet, Path: "/v1/media/models", Auth: AuthAPI, Kind: "json"},
		{Name: "agents", Method: http.MethodGet, Path: "/agents", Auth: AuthPanel, Kind: "json"},
		{Name: "updates", Method: http.MethodGet, Path: "/updates", Auth: AuthPanel, Kind: "json"},
		{Name: "updates_backups", Method: http.MethodGet, Path: "/updates/backups", Auth: AuthPanel, Kind: "json"},
		{Name: "codearts_overview", Method: http.MethodGet, Path: "/codearts/overview?force=1", Auth: AuthPanel, Kind: "json"},
		{Name: "codearts_checkin", Method: http.MethodPost, Path: "/codearts/accounts/checkin", Body: json.RawMessage(`{"uid":"u1"}`), Auth: AuthPanel, Kind: "json"},
		{Name: "codearts_credits_refresh", Method: http.MethodPost, Path: "/codearts/credits/refresh", Body: json.RawMessage(`{}`), Auth: AuthPanel, Kind: "json"},
		{Name: "usage", Method: http.MethodGet, Path: "/usage", Auth: AuthPanel, Kind: "json"},
		{Name: "usage_raccoon", Method: http.MethodGet, Path: "/usage?realm=raccoon", Auth: AuthPanel, Kind: "json"},
		{Name: "usage_codearts", Method: http.MethodGet, Path: "/usage?realm=codearts", Auth: AuthPanel, Kind: "json"},
		{Name: "logs", Method: http.MethodGet, Path: "/logs", Auth: AuthPanel, Kind: "json"},
		{Name: "codearts_models_refresh", Method: http.MethodPost, Path: "/codearts/models/refresh", Body: json.RawMessage(`{}`), Auth: AuthPanel, Kind: "json"},
	}
}

// WorkBuddyCases is the v1.6.5 WorkBuddy provider slice. v1 has no standalone
// WorkBuddy fake and hardcodes the intl origin, so recording uses the ignored
// copied v1 tree with the test-only endpoint patch documented in
// tests/oracle/workbuddy.md and a local fake upstream.
func WorkBuddyCases() []Case {
	chat := func(model string, stream bool) json.RawMessage {
		value := map[string]any{
			"model": model,
			"messages": []any{
				map[string]any{"role": "user", "content": "hello"},
			},
			"stream": stream,
		}
		raw, _ := json.Marshal(value)
		return raw
	}
	const model = "hy4-preview"
	return []Case{
		{Name: "wb_models_intl", Method: http.MethodGet, Path: "/v1/models?realm=intl", Auth: AuthAPI, Kind: "json", Group: "workbuddy"},
		{Name: "wb_models_cn", Method: http.MethodGet, Path: "/v1/models?realm=cn", Auth: AuthAPI, Kind: "json", Group: "workbuddy"},
		{Name: "wb_health", Method: http.MethodGet, Path: "/health?force=1", Auth: AuthNone, Kind: "json", Group: "workbuddy"},
		{Name: "wb_chat", Method: http.MethodPost, Path: "/v1/chat/completions?realm=intl", Body: chat(model, false), Auth: AuthAPI, Kind: "json", Group: "workbuddy"},
		{Name: "wb_chat_stream", Method: http.MethodPost, Path: "/v1/chat/completions?realm=intl", Body: chat(model, true), Auth: AuthAPI, Kind: "sse", Group: "workbuddy"},
		{Name: "wb_chat_cn", Method: http.MethodPost, Path: "/v1/chat/completions?realm=cn", Body: chat(model, false), Auth: AuthAPI, Kind: "json", Group: "workbuddy"},
		{Name: "wb_chat_stream_cn", Method: http.MethodPost, Path: "/v1/chat/completions?realm=cn", Body: chat(model, true), Auth: AuthAPI, Kind: "sse", Group: "workbuddy"},
		{Name: "wb_responses", Method: http.MethodPost, Path: "/v1/responses?realm=intl", Body: json.RawMessage(`{"model":"hy4-preview","input":"hello","stream":false}`), Auth: AuthAPI, Kind: "json", Group: "workbuddy"},
		{Name: "wb_responses_stream", Method: http.MethodPost, Path: "/v1/responses?realm=intl", Body: json.RawMessage(`{"model":"hy4-preview","input":"hello","stream":true}`), Auth: AuthAPI, Kind: "sse", Group: "workbuddy"},
		{Name: "wb_messages", Method: http.MethodPost, Path: "/v1/messages?realm=intl", Body: json.RawMessage(`{"model":"hy4-preview","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`), Auth: AuthAPI, Kind: "json", Group: "workbuddy"},
		{Name: "wb_accounts", Method: http.MethodGet, Path: "/accounts?realm=all", Auth: AuthPanel, Kind: "json", Group: "workbuddy"},
		{Name: "wb_usage_intl", Method: http.MethodGet, Path: "/usage?realm=intl", Auth: AuthPanel, Kind: "json", Group: "workbuddy"},
		{Name: "wb_usage_cn", Method: http.MethodGet, Path: "/usage?realm=cn", Auth: AuthPanel, Kind: "json", Group: "workbuddy"},
		{Name: "wb_logs", Method: http.MethodGet, Path: "/logs", Auth: AuthPanel, Kind: "json", Group: "workbuddy"},
		{Name: "wb_credits_intl", Method: http.MethodPost, Path: "/accounts/credits", Body: json.RawMessage(`{"realm":"intl"}`), Auth: AuthPanel, Kind: "json", Group: "workbuddy"},
		{Name: "wb_credits_cn", Method: http.MethodPost, Path: "/accounts/credits", Body: json.RawMessage(`{"realm":"cn"}`), Auth: AuthPanel, Kind: "json", Group: "workbuddy"},
		{Name: "wb_checkin_cn", Method: http.MethodPost, Path: "/accounts/checkin", Body: json.RawMessage(`{"uid":"oracle-cn"}`), Auth: AuthPanel, Kind: "json", Group: "workbuddy"},
		{Name: "wb_catalog_refresh_intl", Method: http.MethodPost, Path: "/catalog/refresh", Body: json.RawMessage(`{"realm":"intl"}`), Auth: AuthPanel, Kind: "json", Group: "workbuddy"},
	}
}

// MediaCases is the v1.6.5 WorkBuddy media success slice: image generation,
// image edit, image b64 passthrough, asynchronous video submit/status/content,
// local queue cancellation, the task list, and usage rows. The fake toggles
// media catalog entries and video completion from the same control hook used
// by the WorkBuddy failure matrix.
func MediaCases() []Case {
	mediaSetup := func(scenario string) *ControlRequest {
		return &ControlRequest{
			Method: http.MethodPost, Path: "/__setup",
			Body: json.RawMessage(`{"provider":"workbuddy","scenario":"` + scenario + `"}`),
		}
	}
	imageBody := func(model string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{
			"model": model, "prompt": "oracle image", "size": "1024x1024",
		})
		return raw
	}
	videoBody := func() json.RawMessage {
		raw, _ := json.Marshal(map[string]any{
			"model": "kling-video-v3", "prompt": "oracle video",
			"seconds": 5, "resolution": "720P",
		})
		return raw
	}
	// 1x1 transparent PNG. The edit path must receive a data URL, never a
	// server path, and the fake never decodes it.
	const tinyPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z0xQAAAAASUVORK5CYII="
	editBody, _ := json.Marshal(map[string]any{
		"model": "hunyuan-image-alpha-edit", "prompt": "oracle edit",
		"size": "1024x1024", "image": tinyPNG,
	})
	return []Case{
		{Name: "media_models", Method: http.MethodGet, Path: "/v1/media/models", Auth: AuthAPI, Kind: "json", Group: "media", Setup: mediaSetup("media")},
		{Name: "media_image_generation", Method: http.MethodPost, Path: "/v1/images/generations", Body: imageBody("hunyuan-image-alpha"), Auth: AuthAPI, Kind: "json", Group: "media", Setup: mediaSetup("media")},
		{Name: "media_image_edit", Method: http.MethodPost, Path: "/v1/images/edits", Body: editBody, Auth: AuthAPI, Kind: "json", Group: "media", Setup: mediaSetup("media")},
		{Name: "media_image_b64", Method: http.MethodPost, Path: "/v1/images/generations", Body: imageBody("gemini-image-oracle"), Auth: AuthAPI, Kind: "json", Group: "media", Setup: mediaSetup("media")},
		{Name: "media_video_wait", Method: http.MethodPost, Path: "/v1/videos?wait=1", Body: videoBody(), Auth: AuthAPI, Kind: "json", Group: "media", Setup: mediaSetup("media"), Capture: map[string]string{"video_task_id": "id", "video_url": "url"}},
		{Name: "media_video_status", Method: http.MethodGet, Path: "/v1/videos/{{video_task_id}}", Auth: AuthAPI, Kind: "json", Group: "media", Setup: mediaSetup("media")},
		{Name: "media_video_content", Method: http.MethodGet, Path: "{{video_url}}", Auth: AuthNone, Kind: "raw", Group: "media", Setup: mediaSetup("media")},
		{Name: "media_video_running", Method: http.MethodPost, Path: "/v1/videos", Body: videoBody(), Auth: AuthAPI, Kind: "json", Group: "media", Setup: mediaSetup("media_running"), Capture: map[string]string{"video_running_id": "id"}},
		{Name: "media_video_queued", Method: http.MethodPost, Path: "/v1/videos", Body: videoBody(), Auth: AuthAPI, Kind: "json", Group: "media", Setup: mediaSetup("media_running"), Capture: map[string]string{"video_queue_id": "id"}},
		{Name: "media_video_cancel", Method: http.MethodPost, Path: "/v1/videos/{{video_queue_id}}/cancel", Auth: AuthAPI, Kind: "json", Group: "media", Setup: mediaSetup("media_running")},
		{Name: "media_tasks", Method: http.MethodGet, Path: "/v1/media/tasks", Auth: AuthAPI, Kind: "json", Group: "media", DelayMS: 1000},
		{Name: "media_usage", Method: http.MethodGet, Path: "/usage?realm=intl", Auth: AuthPanel, Kind: "json", Group: "media"},
		{Name: "media_usage_recent", Method: http.MethodGet, Path: "/usage/recent?realm=intl&limit=10", Auth: AuthPanel, Kind: "json", Group: "media"},
	}
}

func failureCase(name, path string, body json.RawMessage, kind, provider, scenario string) Case {
	return Case{
		Name: name, Method: http.MethodPost, Path: path, Body: body,
		Auth: AuthAPI, Kind: kind, Group: provider + "_failure",
		Setup: &ControlRequest{
			Method: http.MethodPost, Path: "/__setup",
			Body: json.RawMessage(`{"provider":"` + provider + `","scenario":"` + scenario + `"}`),
		},
		Observe: "/__observe?provider=" + provider,
	}
}

// RaccoonFailureCases is the deterministic Raccoon failure matrix. Each case
// resets the fake to a scenario that fails the first upstream attempt; the
// observation records how many upstream calls the target actually made.
func RaccoonFailureCases() []Case {
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
		failureCase("raccoon_429_failover", "/v1/chat/completions?realm=raccoon", chat(false), "json", "raccoon", "429"),
		failureCase("raccoon_429_failover_stream", "/v1/chat/completions?realm=raccoon", chat(true), "sse", "raccoon", "429"),
		failureCase("raccoon_credit_400", "/v1/chat/completions?realm=raccoon", chat(false), "json", "raccoon", "credit_400"),
		failureCase("raccoon_concurrency_400", "/v1/chat/completions?realm=raccoon", chat(false), "json", "raccoon", "concurrency_400"),
		// 401 refreshes and rotates the credential, so keep it last: later
		// cases would otherwise see an account whose token the fake no longer
		// accepts after the scenario reset.
		failureCase("raccoon_401_refresh", "/v1/chat/completions?realm=raccoon", chat(false), "json", "raccoon", "401"),
	}
}

// CodeArtsFailureCases is the deterministic CodeArts failure matrix.
func CodeArtsFailureCases() []Case {
	chat := func(stream bool) json.RawMessage {
		value := map[string]any{
			"model": "GLM-5.2",
			"messages": []any{
				map[string]any{"role": "user", "content": "hello"},
			},
			"stream": stream,
		}
		raw, _ := json.Marshal(value)
		return raw
	}
	return []Case{
		failureCase("codearts_429_failover", "/v1/chat/completions?realm=codearts", chat(false), "json", "codearts", "429"),
		failureCase("codearts_429_failover_stream", "/v1/chat/completions?realm=codearts", chat(true), "sse", "codearts", "429"),
		failureCase("codearts_credit_400", "/v1/chat/completions?realm=codearts", chat(false), "json", "codearts", "credit_400"),
		failureCase("codearts_concurrency_retry", "/v1/chat/completions?realm=codearts", chat(false), "json", "codearts", "concurrency_400"),
		// v1 treats a chat 401 as terminal for that credential and rotates.
		// Keep it last for the same reset reason as Raccoon.
		failureCase("codearts_401_failover", "/v1/chat/completions?realm=codearts", chat(false), "json", "codearts", "401"),
	}
}

// WorkBuddyFailureCases is the deterministic WorkBuddy failure matrix. The
// fake fails the first chat attempt and records whether the target retried,
// refreshed, or rotated to the next account.
func WorkBuddyFailureCases() []Case {
	chat := func(stream bool) json.RawMessage {
		value := map[string]any{
			"model": "hy4-preview",
			"messages": []any{
				map[string]any{"role": "user", "content": "hello"},
			},
			"stream": stream,
		}
		raw, _ := json.Marshal(value)
		return raw
	}
	return []Case{
		failureCase("wb_429_failover", "/v1/chat/completions?realm=intl", chat(false), "json", "workbuddy", "429"),
		failureCase("wb_429_failover_stream", "/v1/chat/completions?realm=intl", chat(true), "sse", "workbuddy", "429"),
		failureCase("wb_401_refresh", "/v1/chat/completions?realm=intl", chat(false), "json", "workbuddy", "401"),
		failureCase("wb_5xx_failover", "/v1/chat/completions?realm=intl", chat(false), "json", "workbuddy", "5xx"),
		failureCase("wb_5xx_failover_stream", "/v1/chat/completions?realm=intl", chat(true), "sse", "workbuddy", "5xx"),
	}
}

// CasesForSuite returns the named recording matrix. The default core suite is
// the original Raccoon/CodeArts matrix.
func CasesForSuite(name string) ([]Case, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "core":
		return DefaultCases(), nil
	case "workbuddy":
		return WorkBuddyCases(), nil
	case "media":
		return MediaCases(), nil
	case "raccoon-failures", "raccoon_failures":
		return RaccoonFailureCases(), nil
	case "codearts-failures", "codearts_failures":
		return CodeArtsFailureCases(), nil
	case "workbuddy-failures", "workbuddy_failures":
		return WorkBuddyFailureCases(), nil
	default:
		return nil, fmt.Errorf("oracle: unknown suite %q", name)
	}
}

// Recorder captures one target.
type Recorder struct {
	BaseURL     string
	FakeControl string
	Client      *http.Client
	Creds       Credentials
	Cases       []Case
	Provenance  Provenance
	Now         func() time.Time
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
	if r.FakeControl != "" {
		if err := validateLoopback(r.FakeControl); err != nil {
			return fmt.Errorf("oracle: fake control URL: %w", err)
		}
		// Provenance records the control origin so a reader can tell the
		// scenario hooks never point at a remote host.
		r.Provenance.FakeControl = r.FakeControl
	}
	for _, item := range r.Cases {
		if (item.Setup != nil || item.Observe != "") && r.FakeControl == "" {
			return fmt.Errorf("oracle: case %s needs --fake-control", item.Name)
		}
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
	if err := validateProvenance(document.Provenance); err != nil {
		return err
	}
	vars := map[string]string{}
	for _, item := range r.Cases {
		if item.DelayMS > 0 {
			time.Sleep(time.Duration(item.DelayMS) * time.Millisecond)
		}
		if item.Setup != nil {
			if err := r.runControl(ctx, *item.Setup, vars); err != nil {
				return fmt.Errorf("oracle: setup %s: %w", item.Name, err)
			}
		}
		status, body, err := r.fetch(ctx, item, vars)
		if err != nil {
			return fmt.Errorf("oracle: record %s: %w", item.Name, err)
		}
		if err := captureValues(body, item.Capture, vars); err != nil {
			return fmt.Errorf("oracle: capture %s: %w", item.Name, err)
		}
		item.Status = status
		item.File = item.Name + "." + item.Kind
		sum := sha256.Sum256(body)
		item.SHA256 = hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(outDir, item.File), body, 0o644); err != nil {
			return err
		}
		if item.Observe != "" {
			observed, err := r.fetchControl(ctx, item.Observe, vars)
			if err != nil {
				return fmt.Errorf("oracle: observe %s: %w", item.Name, err)
			}
			item.ObserveFile = item.Name + ".observe.json"
			observeSum := sha256.Sum256(observed)
			item.ObserveSHA256 = hex.EncodeToString(observeSum[:])
			if err := os.WriteFile(filepath.Join(outDir, item.ObserveFile), observed, 0o644); err != nil {
				return err
			}
		}
		document.Cases = append(document.Cases, item)
	}
	if err := validateDocument(&document); err != nil {
		return err
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
	if err := validateProvenance(document.Provenance); err != nil {
		return nil, err
	}
	if len(document.Cases) == 0 {
		return nil, errors.New("oracle: no cases")
	}
	if err := validateCases(document.Cases); err != nil {
		return nil, err
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
		if item.ObserveFile != "" {
			observed, err := os.ReadFile(filepath.Join(dir, item.ObserveFile))
			if err != nil {
				return nil, err
			}
			observeSum := sha256.Sum256(observed)
			if item.ObserveSHA256 != hex.EncodeToString(observeSum[:]) {
				return nil, fmt.Errorf("oracle: observation checksum mismatch for %s", item.ObserveFile)
			}
		}
	}
	return &document, nil
}

// Comparer fetches a target and compares normalized output with a fixture.
type Comparer struct {
	BaseURL     string
	FakeControl string
	Client      *http.Client
	Creds       Credentials
	Dir         string
}

// CaseError is one mismatched case from CompareAll.
type CaseError struct {
	Name string
	Err  error
}

func (e CaseError) Error() string {
	return fmt.Sprintf("%s: %v", e.Name, e.Err)
}

// Compare returns the first mismatch, or nil when every case matches.
func (c *Comparer) Compare(ctx context.Context) error {
	diffs := c.CompareAll(ctx)
	if len(diffs) > 0 {
		return diffs[0]
	}
	return nil
}

// CompareAll fetches every case and returns all mismatches. It is used to
// report the full Gate F delta instead of stopping at the first v1-only field.
func (c *Comparer) CompareAll(ctx context.Context) []CaseError {
	if err := validateLoopback(c.BaseURL); err != nil {
		return []CaseError{{Name: "target", Err: err}}
	}
	document, err := Load(c.Dir)
	if err != nil {
		return []CaseError{{Name: "fixture", Err: err}}
	}
	var failures []CaseError
	vars := map[string]string{}
	for _, item := range document.Cases {
		want, err := os.ReadFile(filepath.Join(c.Dir, item.File))
		if err != nil {
			failures = append(failures, CaseError{Name: item.Name, Err: err})
			continue
		}
		if item.DelayMS > 0 {
			time.Sleep(time.Duration(item.DelayMS) * time.Millisecond)
		}
		if item.Setup != nil {
			if c.FakeControl == "" {
				failures = append(failures, CaseError{Name: item.Name, Err: errors.New("fake control URL is required")})
				continue
			}
			if err := c.runControl(ctx, *item.Setup, vars); err != nil {
				failures = append(failures, CaseError{Name: item.Name, Err: fmt.Errorf("setup: %w", err)})
				continue
			}
		}
		status, got, err := c.fetch(ctx, item, vars)
		if err != nil {
			failures = append(failures, CaseError{Name: item.Name, Err: err})
			continue
		}
		if err := captureValues(got, item.Capture, vars); err != nil {
			failures = append(failures, CaseError{Name: item.Name, Err: fmt.Errorf("capture: %w", err)})
			continue
		}
		if status != item.Status {
			failures = append(failures, CaseError{Name: item.Name,
				Err: fmt.Errorf("status %d, want %d", status, item.Status)})
			continue
		}
		if err := compareBody(item.Kind, want, got); err != nil {
			failures = append(failures, CaseError{Name: item.Name, Err: err})
			continue
		}
		if item.ObserveFile != "" {
			wantObserved, err := os.ReadFile(filepath.Join(c.Dir, item.ObserveFile))
			if err != nil {
				failures = append(failures, CaseError{Name: item.Name, Err: err})
				continue
			}
			gotObserved, err := c.fetchControl(ctx, item.Observe, vars)
			if err != nil {
				failures = append(failures, CaseError{Name: item.Name, Err: fmt.Errorf("observe: %w", err)})
				continue
			}
			if err := compareBody("json", wantObserved, gotObserved); err != nil {
				failures = append(failures, CaseError{Name: item.Name, Err: fmt.Errorf("observation: %w", err)})
			}
		}
	}
	return failures
}

func (r *Recorder) runControl(ctx context.Context, control ControlRequest, vars map[string]string) error {
	_, _, err := doControl(ctx, r.FakeControl, control, vars)
	return err
}

func (r *Recorder) fetchControl(ctx context.Context, path string, vars map[string]string) ([]byte, error) {
	return getControl(ctx, r.FakeControl, path, vars)
}

func (c *Comparer) runControl(ctx context.Context, control ControlRequest, vars map[string]string) error {
	_, _, err := doControl(ctx, c.FakeControl, control, vars)
	return err
}

func (c *Comparer) fetchControl(ctx context.Context, path string, vars map[string]string) ([]byte, error) {
	return getControl(ctx, c.FakeControl, path, vars)
}

func doControl(ctx context.Context, baseURL string, control ControlRequest, vars map[string]string) (int, []byte, error) {
	if baseURL == "" {
		return 0, nil, errors.New("oracle: fake control URL is required")
	}
	target, err := joinURL(baseURL, expandVariables(control.Path, vars))
	if err != nil {
		return 0, nil, err
	}
	var body io.Reader
	if len(control.Body) > 0 {
		body = bytes.NewReader(control.Body)
	}
	method := control.Method
	if method == "" {
		method = http.MethodPost
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, err
	}
	if len(control.Body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	status, raw, err := doRequest(nil, request)
	if err != nil {
		return status, raw, err
	}
	if status < 200 || status >= 300 {
		return status, raw, fmt.Errorf("fake control returned HTTP %d", status)
	}
	return status, raw, nil
}

func getControl(ctx context.Context, baseURL, path string, vars map[string]string) ([]byte, error) {
	if baseURL == "" {
		return nil, errors.New("oracle: fake control URL is required")
	}
	target, err := joinURL(baseURL, expandVariables(path, vars))
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	status, raw, err := doRequest(nil, request)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("fake control returned HTTP %d", status)
	}
	return raw, nil
}

func (r *Recorder) fetch(ctx context.Context, item Case, vars map[string]string) (int, []byte, error) {
	request, err := r.build(ctx, item, vars)
	if err != nil {
		return 0, nil, err
	}
	return doRequest(r.Client, request)
}

func (c *Comparer) fetch(ctx context.Context, item Case, vars map[string]string) (int, []byte, error) {
	request, err := buildRequest(ctx, c.BaseURL, c.Creds, item, vars)
	if err != nil {
		return 0, nil, err
	}
	return doRequest(c.Client, request)
}

func (r *Recorder) build(ctx context.Context, item Case, vars map[string]string) (*http.Request, error) {
	return buildRequest(ctx, r.BaseURL, r.Creds, item, vars)
}

func buildRequest(ctx context.Context, baseURL string, creds Credentials, item Case, vars map[string]string) (*http.Request, error) {
	target, err := joinURL(baseURL, expandVariables(item.Path, vars))
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

// tokenRemainingPattern matches the short credential countdown strings v1
// emits (for example "59m" or "3h59m"). The contract normalizer deliberately
// preserves this value for its own CodeArts fixtures, so the oracle harness
// replaces it here instead of changing the shared rule.
var tokenRemainingPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]`)

// mediaContentURLPattern matches a video download URL. The task id and signed
// token are generated per run; the route, index, and token query key are
// stable contract fields and survive.
var mediaContentURLPattern = regexp.MustCompile(`^/v1/videos/[^/]+/content\?i=([0-9]+)&t=[^&]+$`)

// mediaHintTaskPattern matches the task id embedded in the queued-submit hint.
var mediaHintTaskPattern = regexp.MustCompile(`(/v1/videos/)[A-Za-z0-9_-]+`)

func normalizeMediaContentURL(value string) (string, bool) {
	match := mediaContentURLPattern.FindStringSubmatch(value)
	if match == nil {
		return "", false
	}
	return "/v1/videos/" + contract.DynamicValue + "/content?i=" + match[1] + "&t=" + contract.DynamicValue, true
}

func normalizeOracleJSON(data []byte) (any, error) {
	value, err := contract.NormalizeJSON(data)
	if err != nil {
		return nil, err
	}
	return normalizeOracleValues(value), nil
}

func normalizeOracleValues(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.EqualFold(strings.TrimSpace(key), "token_remaining") {
				if text, ok := child.(string); ok &&
					tokenRemainingPattern.MatchString(strings.TrimSpace(text)) {
					typed[key] = contract.DurationValue
					continue
				}
			}
			if strings.EqualFold(strings.TrimSpace(key), "url") {
				if text, ok := child.(string); ok {
					if normalized, matched := normalizeMediaContentURL(text); matched {
						typed[key] = normalized
						continue
					}
				}
			}
			if strings.EqualFold(strings.TrimSpace(key), "hint") {
				if text, ok := child.(string); ok {
					typed[key] = mediaHintTaskPattern.ReplaceAllString(text, "${1}"+contract.DynamicValue)
					continue
				}
			}
			typed[key] = normalizeOracleValues(child)
		}
		return typed
	case []any:
		for index, child := range typed {
			typed[index] = normalizeOracleValues(child)
		}
		return typed
	default:
		return value
	}
}

func expandVariables(text string, vars map[string]string) string {
	if len(vars) == 0 || !strings.Contains(text, "{{") {
		return text
	}
	for key, value := range vars {
		text = strings.ReplaceAll(text, "{{"+key+"}}", value)
	}
	return text
}

// captureValues extracts named values from a JSON response. It is used for
// resources the target generates during a recording (video task ids and their
// signed content URLs) so later cases can address the same resource.
func captureValues(body []byte, capture map[string]string, vars map[string]string) error {
	if len(capture) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return fmt.Errorf("response is not JSON: %w", err)
	}
	for name, path := range capture {
		value, ok := lookupPath(root, path)
		if !ok {
			return fmt.Errorf("capture %q: path %q not found", name, path)
		}
		switch typed := value.(type) {
		case string:
			vars[name] = typed
		case json.Number:
			vars[name] = typed.String()
		case bool:
			vars[name] = strconv.FormatBool(typed)
		default:
			return fmt.Errorf("capture %q: path %q is not a scalar", name, path)
		}
	}
	return nil
}

func lookupPath(root any, path string) (any, bool) {
	current := root
	for _, segment := range strings.Split(strings.TrimSpace(path), ".") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			return nil, false
		}
		switch typed := current.(type) {
		case map[string]any:
			value, ok := typed[segment]
			if !ok {
				return nil, false
			}
			current = value
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, false
			}
			current = typed[index]
		default:
			return nil, false
		}
	}
	return current, true
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
		if !semanticEqual(wantEvents, gotEvents) {
			return fmt.Errorf("SSE mismatch:\n%s", describeDifferences(wantEvents, gotEvents))
		}
	case "json":
		wantValue, err := normalizeOracleJSON(want)
		if err != nil {
			return fmt.Errorf("normalize oracle: %w", err)
		}
		gotValue, err := normalizeOracleJSON(got)
		if err != nil {
			return fmt.Errorf("normalize target: %w", err)
		}
		if !semanticEqual(wantValue, gotValue) {
			return fmt.Errorf("JSON mismatch:\n%s", describeDifferences(wantValue, gotValue))
		}
	case "raw":
		if !bytes.Equal(want, got) {
			return fmt.Errorf("raw body mismatch: %d bytes, want %d", len(got), len(want))
		}
	default:
		if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
			return errors.New("raw body mismatch")
		}
	}
	return nil
}

// describeDifferences walks two normalized values and returns stable,
// path-qualified differences. It is deliberately bounded so a large panel
// payload cannot flood the report.
func describeDifferences(want, got any) string {
	diffs := make([]string, 0, 16)
	diffValues("", want, got, &diffs, 40)
	if len(diffs) == 0 {
		return "values differ but no structural difference was found"
	}
	return strings.Join(diffs, "\n")
}

// semanticEqual compares normalized JSON values. JSON numbers keep their
// lexical form in json.Number, so 0 and 0.0 are the same wire value even
// though their strings differ.
func semanticEqual(want, got any) bool {
	switch left := want.(type) {
	case json.Number:
		right, ok := got.(json.Number)
		if !ok {
			return false
		}
		leftRat, leftOK := new(big.Rat).SetString(left.String())
		rightRat, rightOK := new(big.Rat).SetString(right.String())
		if leftOK && rightOK {
			return leftRat.Cmp(rightRat) == 0
		}
		return left.String() == right.String()
	case map[string]any:
		right, ok := got.(map[string]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			other, ok := right[key]
			if !ok || !semanticEqual(value, other) {
				return false
			}
		}
		return true
	case []any:
		right, ok := got.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for index := range left {
			if !semanticEqual(left[index], right[index]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}

func diffValues(path string, want, got any, out *[]string, limit int) {
	if len(*out) >= limit {
		return
	}
	if semanticEqual(want, got) {
		return
	}
	label := path
	if label == "" {
		label = "<root>"
	}
	switch left := want.(type) {
	case map[string]any:
		right, ok := got.(map[string]any)
		if !ok {
			appendDiff(out, fmt.Sprintf("%s: type %T, want %T", label, got, want))
			return
		}
		keys := make([]string, 0, len(left)+len(right))
		seen := map[string]bool{}
		for key := range left {
			keys = append(keys, key)
			seen[key] = true
		}
		for key := range right {
			if !seen[key] {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := key
			if path != "" {
				child = path + "." + key
			}
			wantValue, wantOK := left[key]
			gotValue, gotOK := right[key]
			switch {
			case !gotOK:
				appendDiff(out, fmt.Sprintf("%s: missing on target (oracle=%s)", child, compact(wantValue)))
			case !wantOK:
				appendDiff(out, fmt.Sprintf("%s: extra on target (target=%s)", child, compact(gotValue)))
			default:
				diffValues(child, wantValue, gotValue, out, limit)
			}
			if len(*out) >= limit {
				return
			}
		}
	case []any:
		right, ok := got.([]any)
		if !ok {
			appendDiff(out, fmt.Sprintf("%s: type %T, want %T", label, got, want))
			return
		}
		if len(left) != len(right) {
			appendDiff(out, fmt.Sprintf("%s: length %d, want %d", label, len(right), len(left)))
		}
		for index := 0; index < len(left) && index < len(right); index++ {
			diffValues(fmt.Sprintf("%s[%d]", path, index), left[index], right[index], out, limit)
			if len(*out) >= limit {
				return
			}
		}
	default:
		appendDiff(out, fmt.Sprintf("%s: %s, want %s", label, compact(got), compact(want)))
	}
}

func appendDiff(out *[]string, line string) {
	if len(*out) < 40 {
		*out = append(*out, line)
	}
}

func compact(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	text := string(raw)
	if len(text) > 240 {
		text = text[:240] + "…"
	}
	return text
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

func validateDocument(document *Document) error {
	if document == nil {
		return errors.New("oracle: nil document")
	}
	if document.Format != Format {
		return fmt.Errorf("oracle: unsupported format %q", document.Format)
	}
	if err := validateProvenance(document.Provenance); err != nil {
		return err
	}
	if len(document.Cases) == 0 {
		return errors.New("oracle: no cases")
	}
	return validateCases(document.Cases)
}

func validateProvenance(value Provenance) error {
	if strings.TrimSpace(value.TargetVersion) == "" || strings.TrimSpace(value.TargetCommit) == "" {
		return errors.New("oracle: provenance requires target version and commit")
	}
	if !value.FakeUpstream {
		return errors.New("oracle: fixture does not declare a fake upstream")
	}
	if value.Network != "loopback-only" {
		return errors.New("oracle: fixture does not declare loopback-only provenance")
	}
	if _, err := time.Parse(time.RFC3339, value.RecordedAt); err != nil {
		return errors.New("oracle: provenance recorded_at must be RFC3339")
	}
	if err := validateLoopback(value.BaseURL); err != nil {
		return fmt.Errorf("oracle: provenance base_url: %w", err)
	}
	if value.FakeControl != "" {
		if err := validateLoopback(value.FakeControl); err != nil {
			return fmt.Errorf("oracle: provenance fake_control_url: %w", err)
		}
	}
	return nil
}

func validateCases(cases []Case) error {
	names := map[string]bool{}
	for _, item := range cases {
		name := strings.TrimSpace(item.Name)
		if name == "" || names[name] {
			return fmt.Errorf("oracle: empty or duplicate case name %q", item.Name)
		}
		names[name] = true
		if item.Method == "" || (!strings.HasPrefix(item.Path, "/") && !strings.HasPrefix(item.Path, "{{")) || strings.Contains(item.Path, "://") {
			return fmt.Errorf("oracle: invalid request for %s", name)
		}
		if item.Kind != "json" && item.Kind != "sse" && item.Kind != "raw" {
			return fmt.Errorf("oracle: invalid kind for %s", name)
		}
		if item.DelayMS < 0 {
			return fmt.Errorf("oracle: negative delay for %s", name)
		}
		for captureName, capturePath := range item.Capture {
			if strings.TrimSpace(captureName) == "" || strings.TrimSpace(capturePath) == "" {
				return fmt.Errorf("oracle: empty capture for %s", name)
			}
		}
		if item.Status < 200 || item.Status > 599 {
			return fmt.Errorf("oracle: invalid status for %s", name)
		}
		if item.File == "" || filepath.Base(item.File) != item.File || strings.ContainsAny(item.File, "/\\") {
			return fmt.Errorf("oracle: unsafe fixture filename for %s", name)
		}
		if !isSHA256(item.SHA256) {
			return fmt.Errorf("oracle: invalid fixture checksum for %s", name)
		}
		if item.Setup != nil {
			if item.Setup.Method == "" || !strings.HasPrefix(item.Setup.Path, "/") ||
				strings.Contains(item.Setup.Path, "://") {
				return fmt.Errorf("oracle: invalid setup request for %s", name)
			}
		}
		if item.Observe != "" && (!strings.HasPrefix(item.Observe, "/") ||
			strings.Contains(item.Observe, "://")) {
			return fmt.Errorf("oracle: invalid observe path for %s", name)
		}
		if (item.ObserveFile == "") != (item.ObserveSHA256 == "") {
			return fmt.Errorf("oracle: incomplete observation metadata for %s", name)
		}
		if item.ObserveFile != "" {
			if filepath.Base(item.ObserveFile) != item.ObserveFile ||
				strings.ContainsAny(item.ObserveFile, "/\\") {
				return fmt.Errorf("oracle: unsafe observation filename for %s", name)
			}
			if !isSHA256(item.ObserveSHA256) {
				return fmt.Errorf("oracle: invalid observation checksum for %s", name)
			}
		}
	}
	return nil
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
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
