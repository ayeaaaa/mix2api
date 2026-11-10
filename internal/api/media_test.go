package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/media"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type mediaTestProvider struct {
	*stubProvider
	models []providers.Model

	mu           sync.Mutex
	imageCalls   int
	modelCalls   int
	submitCalls  int
	pollCalls    int
	lastImage    providers.MediaRequest
	lastVideo    providers.MediaRequest
	imageResult  providers.MediaImageResult
	imageErr     error
	submitResult providers.MediaVideoSubmit
	submitErr    error
	pollState    providers.MediaVideoState
	pollErr      error
	downloadErr  error
}

func (p *mediaTestProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{MediaGeneration: true, MediaImages: true, MediaVideos: true}
}
func (p *mediaTestProvider) MediaModels(context.Context, string) ([]providers.Model, error) {
	p.mu.Lock()
	p.modelCalls++
	models := append([]providers.Model(nil), p.models...)
	p.mu.Unlock()
	return models, nil
}
func (p *mediaTestProvider) GenerateImage(_ context.Context, req providers.MediaRequest) (providers.MediaImageResult, error) {
	p.mu.Lock()
	p.imageCalls++
	p.lastImage = req
	result, err := p.imageResult, p.imageErr
	p.mu.Unlock()
	return result, err
}
func (p *mediaTestProvider) EditImage(ctx context.Context, req providers.MediaRequest) (providers.MediaImageResult, error) {
	return p.GenerateImage(ctx, req)
}
func (p *mediaTestProvider) SubmitVideo(_ context.Context, req providers.MediaRequest) (providers.MediaVideoSubmit, error) {
	p.mu.Lock()
	p.submitCalls++
	p.lastVideo = req
	result, err := p.submitResult, p.submitErr
	p.mu.Unlock()
	if result.UpstreamID == "" && err == nil {
		result.UpstreamID = "up-1"
	}
	return result, err
}
func (p *mediaTestProvider) PollVideo(context.Context, string, string) (providers.MediaVideoState, error) {
	p.mu.Lock()
	p.pollCalls++
	state, err := p.pollState, p.pollErr
	p.mu.Unlock()
	return state, err
}
func (p *mediaTestProvider) DownloadVideo(_ context.Context, _, _, dest string) error {
	p.mu.Lock()
	err := p.downloadErr
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(dest, []byte("mp4-bytes"), 0o600)
}
func (p *mediaTestProvider) CancelVideo(context.Context, string, string) error { return nil }

func mediaModels() []providers.Model {
	return []providers.Model{
		{"id": "img-1", "name": "Image One", "tags": []any{"text-to-image"}},
		{"id": "edit-1", "name": "Edit One", "tags": []any{"image-to-image"}},
		{"id": "vid-1", "name": "Video One", "tags": []any{"text-to-video"},
			"videoGeneration": map[string]any{"defaultSeconds": 5, "resolutions": []any{"720P", "1080P"}, "aspectRatios": []any{"16:9", "9:16"}}},
		{"id": "vid-img", "name": "Video Image", "tags": []any{"image-to-video"},
			"videoGeneration": map[string]any{"defaultSeconds": 5, "resolutions": []any{"720P"}}},
	}
}

type mediaFixture struct {
	Server   *Server
	Service  *media.Service
	Provider *mediaTestProvider
	UsageLog string
	Dir      string
}

func newMediaFixture(t *testing.T, cfg media.Config, mutate func(*media.ServiceOptions)) *mediaFixture {
	t.Helper()
	dir := t.TempDir()
	provider := &mediaTestProvider{
		stubProvider: &stubProvider{id: "workbuddy", realm: "cn"},
		models:       mediaModels(),
		imageResult:  providers.MediaImageResult{URL: "https://cdn.example/image.png", Credit: floatPtr(5.71), Created: 100},
		pollState:    providers.MediaVideoState{Status: "running"},
	}
	opts := media.ServiceOptions{
		ConfigFunc: func() media.Config { return cfg },
		Providers:  []providers.Provider{provider},
		Pricing:    media.NewPricing(dir),
		UsageLog:   filepath.Join(dir, "usage.jsonl"),
		HTTPClient: &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}},
				Body: io.NopCloser(strings.NewReader("png-bytes"))}, nil
		})},
		LookupIP: func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil },
	}
	if mutate != nil {
		mutate(&opts)
	}
	if opts.Tasks == nil {
		opts.Tasks = media.NewTaskStore(media.TaskStoreOptions{Dir: dir, Concurrency: cfg.VideoConcurrency, PollInterval: time.Millisecond, PollTimeout: 500 * time.Millisecond})
	}
	usageStore := usage.New(usage.Paths{Dir: dir, Log: opts.UsageLog})
	opts.Usage = &usage.Recorder{Store: usageStore}
	service := media.NewService(opts)
	t.Cleanup(service.Tasks.Stop)
	server := New(ServerOptions{Media: service, APIKeyAuth: passThroughAuth, PanelAuth: passThroughAuth})
	return &mediaFixture{Server: server, Service: service, Provider: provider, UsageLog: opts.UsageLog, Dir: dir}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func floatPtr(value float64) *float64 { return &value }

func TestMediaDisabled404(t *testing.T) {
	fixture := newMediaFixture(t, media.Config{Enabled: false, Realm: "cn", Identity: "desktop", ImageMaxPerMinute: 6, VideoMaxSeconds: 15, VideoConcurrency: 3}, nil)
	rec := doRequest(fixture.Server, http.MethodPost, "/v1/images/generations", `{"prompt":"x"}`, nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("disabled image = %d %s", rec.Code, rec.Body)
	}
	rec = doRequest(fixture.Server, http.MethodPost, "/v1/videos", `{"prompt":"x"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled video = %d %s", rec.Code, rec.Body)
	}
	rec = doRequest(fixture.Server, http.MethodGet, "/v1/media/models", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("models while disabled = %d %s", rec.Code, rec.Body)
	}
	if body := decodeBody(t, rec); body["enabled"] != false {
		t.Fatalf("models body = %#v", body)
	}
}

func TestImageGeneration(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	fixture := newMediaFixture(t, cfg, nil)
	rec := doRequest(fixture.Server, http.MethodPost, "/v1/images/generations", `{"prompt":"a cat","model":"img-1","size":"1024x1024"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("image = %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["model"] != "img-1" || body["mode"] != "generate" || numberText(body["credit"]) != "5.71" {
		t.Fatalf("image body = %#v", body)
	}
	data, _ := body["data"].([]any)
	if len(data) != 1 || !strings.Contains(rec.Body.String(), "cdn.example/image.png") {
		t.Fatalf("image data = %#v", body["data"])
	}
	rec = doRequest(fixture.Server, http.MethodPost, "/v1/images/generations", `{"prompt":"a cat","model":"img-1","n":2}`, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "n 只能为 1") {
		t.Fatalf("n=2 = %d %s", rec.Code, rec.Body)
	}
	rec = doRequest(fixture.Server, http.MethodPost, "/v1/images/generations", `{"prompt":"a cat","model":"img-1","size":"2560x2560"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversize = %d %s", rec.Code, rec.Body)
	}

	rec = doRequest(fixture.Server, http.MethodPost, "/v1/images/generations", `{"prompt":"a cat","model":"img-1","response_format":"b64_json"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("b64 = %d %s", rec.Code, rec.Body)
	}
	body = decodeBody(t, rec)
	data, _ = body["data"].([]any)
	item, _ := data[0].(map[string]any)
	if item["b64_json"] == nil || item["url"] != nil {
		t.Fatalf("b64 item = %#v", item)
	}
	decoded, err := base64.StdEncoding.DecodeString(item["b64_json"].(string))
	if err != nil || string(decoded) != "png-bytes" {
		t.Fatalf("b64 decode = %q err=%v", decoded, err)
	}
}

func TestImageGenerationB64Fallback(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	fixture := newMediaFixture(t, cfg, func(opts *media.ServiceOptions) {
		opts.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("cdn down")
		})}
	})
	rec := doRequest(fixture.Server, http.MethodPost, "/v1/images/generations", `{"prompt":"a cat","model":"img-1","response_format":"b64_json"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	data, _ := body["data"].([]any)
	item, _ := data[0].(map[string]any)
	if item["url"] == nil || item["b64_json"] != nil || !strings.Contains(body["hint"].(string), "转码失败") {
		t.Fatalf("fallback item/hint = %#v / %#v", item, body["hint"])
	}
}

func TestImageEditValidation(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	fixture := newMediaFixture(t, cfg, nil)
	for _, payload := range []string{
		`{"prompt":"x","model":"edit-1","image":"/etc/passwd"}`,
		`{"prompt":"x","model":"edit-1","image":"file:///etc/passwd"}`,
		`{"prompt":"x","model":"edit-1","image":"data:text/plain;base64,aGk="}`,
		`{"prompt":"x","model":"img-1","image":"data:image/png;base64,aGk="}`,
	} {
		rec := doRequest(fixture.Server, http.MethodPost, "/v1/images/edits", payload, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("payload %s = %d %s", payload, rec.Code, rec.Body)
		}
	}
	rec := doRequest(fixture.Server, http.MethodPost, "/v1/images/edits", `{"prompt":"x","model":"edit-1","image":"data:image/png;base64,aGk="}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid edit = %d %s", rec.Code, rec.Body)
	}
	if fixture.Provider.lastImage.Body["image"] == nil {
		t.Fatalf("edit body = %#v", fixture.Provider.lastImage.Body)
	}
}

func TestVideoTaskLifecycleAndUsage(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	var polls int32
	fixture := newMediaFixture(t, cfg, nil)
	fixture.Provider.pollState = providers.MediaVideoState{Status: "completed", Videos: []string{"https://cdn.example/v.mp4"}, Credit: floatPtr(71.43)}
	fixture.Provider.pollErr = nil
	// First poll reports running, then completed, proving both transitions.
	fixture.Provider.pollState = providers.MediaVideoState{Status: "running"}
	rec := doRequest(fixture.Server, http.MethodPost, "/v1/videos", `{"prompt":"a cat","model":"vid-1","seconds":"5","resolution":"720P"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	taskID, _ := body["id"].(string)
	if taskID == "" || body["status"] != "queued" || body["seconds"] != "5" {
		t.Fatalf("submit body = %#v", body)
	}
	fixture.Provider.mu.Lock()
	fixture.Provider.pollState = providers.MediaVideoState{Status: "completed", Videos: []string{"https://cdn.example/v.mp4"}, Credit: floatPtr(71.43)}
	fixture.Provider.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec = doRequest(fixture.Server, http.MethodGet, "/v1/videos/"+taskID, "", nil)
		body = decodeBody(t, rec)
		if body["status"] == "completed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if body["status"] != "completed" || body["url"] == nil || numberText(body["credit"]) != "71.43" {
		t.Fatalf("completed body = %#v", body)
	}
	rec = doRequest(fixture.Server, http.MethodPost, "/v1/videos/"+taskID+"/cancel", "", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel completed = %d %s", rec.Code, rec.Body)
	}
	var raw []byte
	var err error
	for i := 0; i < 100; i++ {
		raw, err = os.ReadFile(fixture.UsageLog)
		if err == nil && strings.Contains(string(raw), `"kind":"video"`) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("usage log: %v", err)
	}
	if !strings.Contains(string(raw), `"kind":"video"`) || !strings.Contains(string(raw), `"cost_policy":"media"`) || strings.Contains(string(raw), "a cat") {
		t.Fatalf("usage row = %s", raw)
	}
	_ = polls
}

func TestVideoWaitAndQueue(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	cfg.VideoConcurrency = 1
	fixture := newMediaFixture(t, cfg, nil)
	fixture.Provider.pollState = providers.MediaVideoState{Status: "running"}
	rec := doRequest(fixture.Server, http.MethodPost, "/v1/videos", `{"prompt":"a cat","model":"vid-1","resolution":"720P"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first = %d %s", rec.Code, rec.Body)
	}
	first := decodeBody(t, rec)
	if first["status"] != "queued" {
		t.Fatalf("first body = %#v", first)
	}
	rec = doRequest(fixture.Server, http.MethodPost, "/v1/videos", `{"prompt":"a dog","model":"vid-1"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second = %d %s", rec.Code, rec.Body)
	}
	second := decodeBody(t, rec)
	if numberText(second["queue_position"]) != "1" {
		t.Fatalf("queue body = %#v", second)
	}
	fixture.Provider.mu.Lock()
	submits := fixture.Provider.submitCalls
	fixture.Provider.mu.Unlock()
	if submits != 1 {
		t.Fatalf("submit calls = %d, want 1 while the first task is active", submits)
	}
}

func TestVideoOwnershipAndSignedURL(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	keyStore := keys.New(store)
	if _, err := keyStore.Save([]keys.Entry{
		{ID: "k1", Name: "one", Key: "key-one", Realm: "cn", Enabled: true},
		{ID: "k2", Name: "two", Key: "key-two", Realm: "cn", Enabled: true},
	}, nil); err != nil {
		t.Fatal(err)
	}
	fixture := newMediaFixture(t, cfg, func(opts *media.ServiceOptions) {
		opts.Tasks = media.NewTaskStore(media.TaskStoreOptions{Dir: dir, Concurrency: cfg.VideoConcurrency, PollInterval: time.Millisecond, PollTimeout: 500 * time.Millisecond})
	})
	fixture.Provider.pollState = providers.MediaVideoState{Status: "completed", Videos: []string{"https://cdn.example/v.mp4"}, Credit: floatPtr(10)}
	server := New(ServerOptions{Media: fixture.Service, KeyStore: keyStore, APIKeyAuth: passThroughAuth, PanelAuth: passThroughAuth})
	auth := map[string]string{"Authorization": "Bearer key-one"}
	rec := doRequest(server, http.MethodPost, "/v1/videos", `{"prompt":"x","model":"vid-1"}`, auth)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body)
	}
	taskID := decodeBody(t, rec)["id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec = doRequest(server, http.MethodGet, "/v1/videos/"+taskID, "", auth)
		if decodeBody(t, rec)["status"] == "completed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	body := decodeBody(t, rec)
	contentURL, _ := body["url"].(string)
	if contentURL == "" {
		t.Fatalf("content url missing: %#v", body)
	}
	rec = doRequest(server, http.MethodGet, "/v1/videos/"+taskID, "", map[string]string{"Authorization": "Bearer key-two"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other key status = %d %s", rec.Code, rec.Body)
	}
	rec = doRequest(server, http.MethodGet, contentURL, "", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "video/mp4" || !strings.Contains(rec.Header().Get("Content-Disposition"), taskID) {
		t.Fatalf("signed content = %d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
	}
	if rec.Body.String() != "mp4-bytes" {
		t.Fatalf("content = %q", rec.Body.String())
	}
	rec = doRequest(server, http.MethodGet, "/v1/videos/"+taskID+"/content?i=0&t=bad.token", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d %s", rec.Code, rec.Body)
	}
}

func TestMediaUsageRowsImage(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	fixture := newMediaFixture(t, cfg, nil)
	rec := doRequest(fixture.Server, http.MethodPost, "/v1/images/generations", `{"prompt":"private prompt","model":"img-1","size":"1024x1024"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("image = %d %s", rec.Code, rec.Body)
	}
	raw, err := os.ReadFile(fixture.UsageLog)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"kind":"image"`) || !strings.Contains(text, `"cost_policy":"media"`) ||
		!strings.Contains(text, `"credit":5.71`) || !strings.Contains(text, `"resolution":"1024x1024"`) {
		t.Fatalf("image usage row = %s", text)
	}
	if strings.Contains(text, "private prompt") {
		t.Fatalf("prompt leaked into usage: %s", text)
	}
}

func numberText(value any) string {
	switch typed := value.(type) {
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	default:
		return ""
	}
}

func TestMediaQueueFullBeforeSpending(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	cfg.VideoConcurrency = 1
	fixture := newMediaFixture(t, cfg, nil)
	fixture.Service.Tasks.SetMaxPending(1)
	fixture.Provider.pollState = providers.MediaVideoState{Status: "running"}
	rec := doRequest(fixture.Server, http.MethodPost, "/v1/videos", `{"prompt":"one","model":"vid-1"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first = %d %s", rec.Code, rec.Body)
	}
	fixture.Provider.mu.Lock()
	before := fixture.Provider.submitCalls
	fixture.Provider.mu.Unlock()
	rec = doRequest(fixture.Server, http.MethodPost, "/v1/videos", `{"prompt":"two","model":"vid-1"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("queue full = %d %s", rec.Code, rec.Body)
	}
	fixture.Provider.mu.Lock()
	after := fixture.Provider.submitCalls
	fixture.Provider.mu.Unlock()
	if after != before {
		t.Fatalf("queue-full request spent credits: submits %d -> %d", before, after)
	}
}

func TestVideoQueueDrainsAfterCompletion(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	cfg.VideoConcurrency = 1
	fixture := newMediaFixture(t, cfg, nil)
	fixture.Provider.pollState = providers.MediaVideoState{Status: "running"}
	first := doRequest(fixture.Server, http.MethodPost, "/v1/videos", `{"prompt":"one","model":"vid-1"}`, nil)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first = %d %s", first.Code, first.Body)
	}
	second := doRequest(fixture.Server, http.MethodPost, "/v1/videos", `{"prompt":"two","model":"vid-1"}`, nil)
	if second.Code != http.StatusAccepted {
		t.Fatalf("second = %d %s", second.Code, second.Body)
	}
	secondBody := decodeBody(t, second)
	if numberText(secondBody["queue_position"]) != "1" {
		t.Fatalf("second was not queued: %#v", secondBody)
	}
	fixture.Provider.mu.Lock()
	fixture.Provider.pollState = providers.MediaVideoState{Status: "completed", Videos: []string{"https://cdn.example/v.mp4"}, Credit: floatPtr(1)}
	fixture.Provider.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fixture.Provider.mu.Lock()
		submits := fixture.Provider.submitCalls
		fixture.Provider.mu.Unlock()
		if submits >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("waiting video task never drained into the submitter")
}

func TestVideoTaskLostMapping(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	fixture := newMediaFixture(t, cfg, nil)
	fixture.Provider.pollErr = &providers.MediaError{Code: "task_lost", Message: "account gone"}
	rec := doRequest(fixture.Server, http.MethodPost, "/v1/videos", `{"prompt":"one","model":"vid-1"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body)
	}
	taskID := decodeBody(t, rec)["id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec = doRequest(fixture.Server, http.MethodGet, "/v1/videos/"+taskID, "", nil)
		body := decodeBody(t, rec)
		if body["status"] == "failed" {
			errorObj, _ := body["error"].(map[string]any)
			if errorObj["code"] != "task_lost" {
				t.Fatalf("error = %#v", errorObj)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("task_lost task did not reach failed")
}

func TestMediaBusyReturns503(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	fixture := newMediaFixture(t, cfg, nil)
	for i := 0; i < 4; i++ {
		if !fixture.Service.AcquireSlot() {
			t.Fatalf("could not acquire slot %d", i)
		}
	}
	defer func() {
		for i := 0; i < 4; i++ {
			fixture.Service.ReleaseSlot()
		}
	}()
	rec := doRequest(fixture.Server, http.MethodPost, "/v1/images/generations", `{"prompt":"x","model":"img-1"}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy = %d %s", rec.Code, rec.Body)
	}
}

func TestMediaCatalogCacheAndPricingShape(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	fixture := newMediaFixture(t, cfg, nil)
	fixture.Service.Pricing.LearnVideo("vid-1", "720P", 57.14, 4, false)
	for i := 0; i < 2; i++ {
		rec := doRequest(fixture.Server, http.MethodGet, "/v1/media/models", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("models = %d %s", rec.Code, rec.Body)
		}
	}
	fixture.Provider.mu.Lock()
	calls := fixture.Provider.modelCalls
	fixture.Provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("catalog calls = %d, want cached once", calls)
	}
	rec := doRequest(fixture.Server, http.MethodGet, "/v1/media/models", "", nil)
	body := decodeBody(t, rec)
	models, _ := body["models"].([]any)
	for _, raw := range models {
		model, _ := raw.(map[string]any)
		if model["id"] == "vid-1" {
			if numberText(model["creditPerSecond"]) != "14.285" {
				t.Fatalf("creditPerSecond = %#v", model["creditPerSecond"])
			}
			perSecond, _ := model["creditsPerSecond"].(map[string]any)
			if numberText(perSecond["720P"]) != "14.285" {
				t.Fatalf("creditsPerSecond = %#v", model["creditsPerSecond"])
			}
		}
	}
}

func TestMediaTasksStatsShape(t *testing.T) {
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	fixture := newMediaFixture(t, cfg, nil)
	rec := doRequest(fixture.Server, http.MethodGet, "/v1/media/tasks", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("tasks = %d %s", rec.Code, rec.Body)
	}
	stats, _ := decodeBody(t, rec)["stats"].(map[string]any)
	for _, key := range []string{"total", "pending", "by_status", "queue_depth", "workers", "concurrency", "active", "waiting", "busy_accounts"} {
		if _, ok := stats[key]; !ok {
			t.Fatalf("stats missing %q: %#v", key, stats)
		}
	}
}

func TestMediaAuthDisabledTaskVisibility(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	keyStore := keys.New(store)
	if _, err := keyStore.Save([]keys.Entry{{ID: "k1", Name: "one", Key: "key-one", Realm: "cn", Enabled: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := keyStore.SetAuthDisabled(true); err != nil {
		t.Fatal(err)
	}
	if keyStore.AuthRequired() {
		t.Fatal("auth_disabled did not clear AuthRequired")
	}
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	fixture := newMediaFixture(t, cfg, nil)
	fixture.Provider.pollState = providers.MediaVideoState{Status: "running"}
	server := New(ServerOptions{Media: fixture.Service, KeyStore: keyStore, PanelAuth: passThroughAuth})
	// No key request when auth is disabled owns key="" and must be visible.
	rec := doRequest(server, http.MethodPost, "/v1/videos", `{"prompt":"x","model":"vid-1"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body)
	}
	taskID := decodeBody(t, rec)["id"].(string)
	rec = doRequest(server, http.MethodGet, "/v1/videos/"+taskID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth-disabled task = %d %s", rec.Code, rec.Body)
	}
	// A request with a different key still cannot see it.
	rec = doRequest(server, http.MethodGet, "/v1/videos/"+taskID, "", map[string]string{"Authorization": "Bearer key-one"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("key-scoped task = %d %s", rec.Code, rec.Body)
	}
}

func TestMediaPassesAllowedAccounts(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	keyStore := keys.New(store)
	if _, err := keyStore.Save([]keys.Entry{{
		ID: "k1", Name: "one", Key: "key-one", Realm: "cn", Enabled: true,
		Accounts: []string{"acct-allowed"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	cfg := media.DefaultConfig()
	cfg.Enabled = true
	fixture := newMediaFixture(t, cfg, nil)
	server := New(ServerOptions{Media: fixture.Service, KeyStore: keyStore, PanelAuth: passThroughAuth})
	rec := doRequest(server, http.MethodPost, "/v1/images/generations", `{"prompt":"x","model":"img-1"}`, map[string]string{"Authorization": "Bearer key-one"})
	if rec.Code != http.StatusOK {
		t.Fatalf("image = %d %s", rec.Code, rec.Body)
	}
	fixture.Provider.mu.Lock()
	allowed, _ := fixture.Provider.lastImage.Params["allowed_accounts"].([]string)
	fixture.Provider.mu.Unlock()
	if len(allowed) != 1 || allowed[0] != "acct-allowed" {
		t.Fatalf("allowed accounts = %#v", allowed)
	}
}
