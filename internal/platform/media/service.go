package media

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/netguard"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// Error is the platform-visible media failure. Code is one of the v1 stable
// values; Message is safe for the client. API maps Code to HTTP status.
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func mediaError(code, message string) *Error { return &Error{Code: code, Message: message} }

// ImageRequest is one synchronous image generation/edit call.
type ImageRequest struct {
	KeyID           string
	BoundRealm      string
	AllowedAccounts []string
	Model           string
	Prompt          string
	Size            string
	ResponseFormat  string
	N               int
	Images          []string
}

// ImageResponse is the OpenAI Images wire body.
type ImageResponse struct {
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Mode    string           `json:"mode"`
	Data    []map[string]any `json:"data"`
	Credit  *float64         `json:"credit,omitempty"`
	Hint    string           `json:"hint,omitempty"`
}

// VideoRequest is one asynchronous video submission.
type VideoRequest struct {
	KeyID           string
	BoundRealm      string
	AllowedAccounts []string
	Payload         map[string]any
}

// VideoSubmission reports whether the task was submitted or locally queued.
type VideoSubmission struct {
	Task          Task
	Queued        bool
	QueuePosition int
}

// ServiceOptions composes the media platform service.
type ServiceOptions struct {
	ConfigFunc func() Config
	Providers  []providers.Provider
	Pricing    *Pricing
	Tasks      *TaskStore
	Usage      *usage.Recorder
	UsageLog   string
	Now        func() time.Time
	Logger     func(string)
	HTTPClient *http.Client
	LookupIP   func(host string) ([]net.IP, error)
	Slots      int
	CatalogTTL time.Duration
}

// Service owns provider selection, realm policy, limits, pricing evidence and
// usage recording. Account rotation and account budget checks stay in the
// provider's MediaProvider hook.
type Service struct {
	ConfigFunc func() Config
	Providers  []providers.Provider
	Pricing    *Pricing
	Tasks      *TaskStore
	Usage      *usage.Recorder
	UsageLog   string
	Now        func() time.Time
	Logger     func(string)
	HTTPClient *http.Client
	LookupIP   func(host string) ([]net.IP, error)

	slots chan struct{}

	catalogMu  sync.Mutex
	catalog    map[string]catalogEntry
	catalogTTL time.Duration

	rateMu sync.Mutex
	rates  map[string]*rateBucket

	spendMu   sync.Mutex
	spendDay  string
	spend     map[string]float64
	spendRead bool
}

// NewService builds the service and wires the task completion callback.
func NewService(opts ServiceOptions) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 180 * time.Second}
	}
	if opts.LookupIP == nil {
		opts.LookupIP = net.LookupIP
	}
	if opts.Tasks == nil {
		opts.Tasks = NewTaskStore(TaskStoreOptions{Now: opts.Now, Logger: opts.Logger})
	}
	if opts.Pricing == nil {
		opts.Pricing = NewPricing(opts.Tasks.dir)
	}
	if opts.Slots <= 0 {
		opts.Slots = 4
	}
	if opts.CatalogTTL <= 0 {
		opts.CatalogTTL = 5 * time.Minute
	}
	service := &Service{
		ConfigFunc: opts.ConfigFunc,
		Providers:  opts.Providers,
		Pricing:    opts.Pricing,
		Tasks:      opts.Tasks,
		Usage:      opts.Usage,
		UsageLog:   opts.UsageLog,
		Now:        opts.Now,
		Logger:     opts.Logger,
		HTTPClient: opts.HTTPClient,
		LookupIP:   opts.LookupIP,
		slots:      make(chan struct{}, opts.Slots),
		catalog:    map[string]catalogEntry{},
		catalogTTL: opts.CatalogTTL,
		rates:      map[string]*rateBucket{},
	}
	if service.ConfigFunc != nil {
		service.Tasks.SetConcurrency(service.Config().VideoConcurrency)
	}
	service.Tasks.SetOnFinish(service.recordTaskFinish)
	service.Tasks.SetSubmitter(func(ctx context.Context, task Task) (providers.MediaVideoSubmit, error) {
		provider, err := service.MediaProviderFor(task.Realm)
		if err != nil {
			return providers.MediaVideoSubmit{}, err
		}
		body, _ := service.Tasks.PendingBody(task.ID)
		params := map[string]any{
			"seconds": task.Seconds, "resolution": task.Resolution,
			"aspect_ratio": task.AspectRatio, "enable_audio": task.EnableAudio,
			"image_to_video": task.ImageToVideo,
			"cost_hint": service.Pricing.CostHint("video", task.Model,
				intValue(task.Seconds, DefaultVideoSeconds), stringValue(task.Resolution), task.HasReferenceMaterials),
		}
		busy := service.Tasks.BusyAccounts()
		if len(busy) > 0 {
			excluded := make([]string, 0, len(busy))
			for account := range busy {
				excluded = append(excluded, account)
			}
			params["exclude_accounts"] = excluded
		}
		request := providers.MediaRequest{
			Realm: task.Realm, Model: task.Model, KeyID: task.Key,
			Params: params, Body: body,
		}
		if prompt, ok := body["prompt"].(string); ok {
			request.Prompt = prompt
		}
		return provider.SubmitVideo(ctx, request)
	})
	service.Tasks.SetTaskPoller(
		func(ctx context.Context, task Task) (providers.MediaVideoState, error) {
			provider, err := service.MediaProviderFor(task.Realm)
			if err != nil {
				return providers.MediaVideoState{}, err
			}
			return provider.PollVideo(ctx, task.Account, task.UpstreamID)
		},
		func(ctx context.Context, task Task, source, dest string) error {
			provider, err := service.MediaProviderFor(task.Realm)
			if err != nil {
				return err
			}
			return provider.DownloadVideo(ctx, task.Account, source, dest)
		},
	)
	return service
}

func (s *Service) nowTime() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Config returns the current media settings.
func (s *Service) Config() Config {
	if s == nil || s.ConfigFunc == nil {
		return DefaultConfig()
	}
	cfg := s.ConfigFunc()
	defaults := DefaultConfig()
	if cfg.Realm != "cn" && cfg.Realm != "intl" {
		cfg.Realm = defaults.Realm
	}
	if cfg.Identity != "desktop" && cfg.Identity != "cli" {
		cfg.Identity = defaults.Identity
	}
	if cfg.ImageMaxPerMinute <= 0 {
		cfg.ImageMaxPerMinute = defaults.ImageMaxPerMinute
	}
	if cfg.VideoMaxSeconds <= 0 {
		cfg.VideoMaxSeconds = defaults.VideoMaxSeconds
	}
	if cfg.VideoConcurrency <= 0 {
		cfg.VideoConcurrency = defaults.VideoConcurrency
	}
	return cfg
}

// Realm resolves the request exit. A key-bound realm wins, but a mismatch with
// the configured media realm is the v1 user-visible error.
func (s *Service) Realm(bound string) (string, error) {
	cfg := s.Config()
	bound = strings.ToLower(strings.TrimSpace(bound))
	if bound != "" {
		if bound != cfg.Realm {
			return "", mediaError("invalid_request", fmt.Sprintf(
				"本 Key 绑定在 %s 出口，而媒体能力配置在 %s 出口；请换对应出口的 Key，或在设置页把媒体出口改成 %s",
				bound, cfg.Realm, bound))
		}
		return bound, nil
	}
	return cfg.Realm, nil
}

// MediaProviderFor returns the provider hook for realm. It checks both the
// provider realm and the optional capabilities flag before any call.
func (s *Service) MediaProviderFor(realm string) (providers.MediaProvider, error) {
	for _, provider := range s.Providers {
		if provider == nil || !strings.EqualFold(provider.Realm(), realm) {
			continue
		}
		hook, ok := provider.(providers.MediaProvider)
		if !ok {
			continue
		}
		if caps, ok := provider.(providers.CapabilityProvider); ok && !caps.Capabilities().MediaGeneration {
			continue
		}
		return hook, nil
	}
	return nil, mediaError("no_route", "该区没有可用的媒体能力")
}

// Models returns the raw media model catalog, optionally filtered by tag, with
// measured pricing evidence attached.
func (s *Service) Models(ctx context.Context, realm, tag string) (map[string]any, error) {
	cfg := s.Config()
	realm = strings.TrimSpace(realm)
	if realm == "" {
		realm = cfg.Realm
	}
	if realm != "cn" && realm != "intl" {
		return nil, mediaError("invalid_request", "realm must be one of cn, intl")
	}
	models, err := s.catalogModels(ctx, realm)
	if err != nil {
		return nil, err
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if tag != "" && !hasTag(model, tag) {
			continue
		}
		copied := map[string]any{}
		for key, value := range model {
			copied[key] = value
		}
		s.decoratePrice(copied)
		filtered = append(filtered, copied)
	}
	return map[string]any{
		"realm": realm, "enabled": cfg.Enabled, "identity": cfg.Identity,
		"models": filtered,
	}, nil
}

func (s *Service) decoratePrice(model map[string]any) {
	id := stringValue(model["id"])
	tags := stringList(model["tags"])
	isVideo := false
	for _, tag := range tags {
		if tag == TagTextToVideo || tag == TagImageToVideo {
			isVideo = true
			break
		}
	}
	if !isVideo {
		price := s.Pricing.ImagePrice(id)
		model["creditPerImage"] = ImageCostEstimate
		model["creditPerImageMeasured"] = false
		if price != nil {
			model["creditPerImage"] = *price
			model["creditPerImageMeasured"] = true
		}
		return
	}
	generation := VideoCapabilities(model)
	resolution := stringValue(generation["defaultResolution"])
	if resolution == "" {
		if values := stringList(generation["resolutions"]); len(values) > 0 {
			resolution = values[0]
		}
	}
	if price := s.Pricing.PricePerSecond(id, resolution, true, false); price != nil {
		model["creditPerSecond"] = *price
	} else {
		model["creditPerSecond"] = nil
	}
	perResolution := map[string]float64{}
	for _, name := range stringList(generation["resolutions"]) {
		if price := s.Pricing.PricePerSecond(id, name, true, false); price != nil {
			perResolution[name] = *price
		}
	}
	if len(perResolution) == 0 && resolution != "" {
		if price := s.Pricing.PricePerSecond(id, resolution, true, false); price != nil {
			perResolution[resolution] = *price
		}
	}
	if len(perResolution) > 0 {
		model["creditsPerSecond"] = perResolution
	}
	withRef := map[string]float64{}
	for _, name := range stringList(generation["resolutions"]) {
		if price := s.Pricing.PricePerSecond(id, name, true, true); price != nil {
			withRef[name] = *price
		}
	}
	if len(withRef) > 0 {
		model["creditsPerSecondWithReference"] = withRef
	}
	if model["creditPerSecond"] == nil {
		model["creditPerSecondCeiling"] = VideoCostPerSecondCeiling
	}
}

// AllowImage applies the in-memory per-key image rate gate.
func (s *Service) AllowImage(keyID string) (bool, int) {
	if s == nil {
		return true, 0
	}
	limit := s.Config().ImageMaxPerMinute
	if limit <= 0 {
		return true, 0
	}
	now := s.nowTime()
	bucket := now.Unix() / 60
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	prefix := fmt.Sprint(bucket) + "\x00"
	for key := range s.rates {
		if !strings.HasPrefix(key, prefix) {
			delete(s.rates, key)
		}
	}
	slot := prefix + keyID
	count := 0
	if entry := s.rates[slot]; entry != nil {
		count = entry.count
	}
	if count >= limit {
		return false, int(60 - now.Unix()%60)
	}
	s.rates[slot] = &rateBucket{count: count + 1}
	return true, 0
}

type rateBucket struct{ count int }

type catalogEntry struct {
	models []map[string]any
	at     time.Time
}

// CheckDailySpend enforces the per-key daily media credit budget. 0 disables.
func (s *Service) CheckDailySpend(keyID string, cost float64) error {
	limit := s.Config().DailyCreditsPerKey
	if limit <= 0 {
		return nil
	}
	spent := s.DailySpend(keyID)
	if spent+cost > float64(limit) {
		return mediaError("no_credit", fmt.Sprintf("本 Key 今日媒体花费已达 %.2f / %d 积分上限（本次预估 %g）", spent, limit, cost))
	}
	return nil
}

// DailySpend sums today's image/video usage rows for keyID.
func (s *Service) DailySpend(keyID string) float64 {
	if s == nil {
		return 0
	}
	now := s.nowTime()
	day := now.Format("2006-01-02")
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	if !s.spendRead || s.spendDay != day {
		s.spend = s.scanSpend(day)
		s.spendDay = day
		s.spendRead = true
	}
	return s.spend[keyID]
}

func (s *Service) scanSpend(day string) map[string]float64 {
	out := map[string]float64{}
	path := strings.TrimSpace(s.UsageLog)
	if path == "" {
		return out
	}
	file, err := os.Open(path)
	if err != nil {
		return out
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var row map[string]any
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.UseNumber()
		if decoder.Decode(&row) != nil {
			continue
		}
		kind := usage.RowKind(row)
		if kind != "image" && kind != "video" {
			continue
		}
		at, ok := usage.RowAt(row)
		if !ok || time.Unix(int64(at), 0).Format("2006-01-02") != day {
			continue
		}
		credit, ok := number(row["credit"])
		if !ok || credit <= 0 {
			continue
		}
		out[usage.RowKey(row)] += credit
	}
	return out
}

func (s *Service) addSpend(keyID string, credit *float64) {
	if credit == nil || *credit <= 0 {
		return
	}
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	now := s.nowTime().Format("2006-01-02")
	if !s.spendRead || s.spendDay != now {
		s.spend = s.scanSpend(now)
		s.spendDay = now
		s.spendRead = true
	}
	s.spend[keyID] += *credit
}

// AcquireSlot takes one media concurrency slot without blocking.
func (s *Service) AcquireSlot() bool {
	if s == nil {
		return true
	}
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// ReleaseSlot returns a media slot.
func (s *Service) ReleaseSlot() {
	if s == nil {
		return
	}
	select {
	case <-s.slots:
	default:
	}
}

// GenerateImage performs one synchronous text-to-image request.
func (s *Service) GenerateImage(ctx context.Context, request ImageRequest) (ImageResponse, error) {
	return s.generate(ctx, request, false)
}

// EditImage performs one synchronous image edit request.
func (s *Service) EditImage(ctx context.Context, request ImageRequest) (ImageResponse, error) {
	return s.generate(ctx, request, true)
}

func (s *Service) generate(ctx context.Context, request ImageRequest, edit bool) (ImageResponse, error) {
	cfg := s.Config()
	if !cfg.Enabled {
		return ImageResponse{}, mediaError("no_route", "not found")
	}
	realm, err := s.Realm(request.BoundRealm)
	if err != nil {
		return ImageResponse{}, err
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return ImageResponse{}, mediaError("invalid_request", "prompt 必填，且必须是非空字符串")
	}
	n := request.N
	if n == 0 {
		n = 1
	}
	if n != 1 {
		return ImageResponse{}, mediaError("invalid_request", "本网关一次生成一张图（n 只能为 1）。要更多请多次调用——每张都真实扣积分，不做不可控的批量支出。")
	}
	models, err := s.mediaModels(ctx, realm)
	if err != nil {
		return ImageResponse{}, err
	}
	tag := TagTextToImage
	if edit {
		tag = TagImageToImage
	}
	model, err := PickModel(models, request.Model, tag)
	if err != nil {
		return ImageResponse{}, fromProtocol(err)
	}
	modelID := stringValue(model["id"])
	size, err := CheckSize(request.Size)
	if err != nil {
		return ImageResponse{}, fromProtocol(err)
	}
	var body map[string]any
	if edit {
		images := make([]string, 0, len(request.Images))
		for _, raw := range request.Images {
			resolved, resolveErr := s.resolveInputImage(ctx, raw)
			if resolveErr != nil {
				return ImageResponse{}, resolveErr
			}
			images = append(images, resolved)
		}
		body, err = BuildEditBody(modelID, request.Prompt, size, images)
	} else {
		body, err = BuildImageBody(modelID, request.Prompt, size)
	}
	if err != nil {
		return ImageResponse{}, fromProtocol(err)
	}
	if code := strings.ToLower(strings.TrimSpace(request.ResponseFormat)); code != "" && code != "url" && code != "b64_json" {
		return ImageResponse{}, mediaError("invalid_request", fmt.Sprintf("response_format 只能是 url 或 b64_json（给定 %q）", request.ResponseFormat))
	} else if code != "" {
		request.ResponseFormat = code
	} else {
		request.ResponseFormat = "url"
	}
	cost := s.Pricing.CostHint("image", modelID, 0, "", false)
	if err := s.CheckDailySpend(request.KeyID, cost); err != nil {
		return ImageResponse{}, err
	}
	if !s.AcquireSlot() {
		return ImageResponse{}, mediaError("busy", "media slots busy; retry shortly")
	}
	defer s.ReleaseSlot()
	provider, err := s.MediaProviderFor(realm)
	if err != nil {
		return ImageResponse{}, err
	}
	mediaRequest := providers.MediaRequest{
		Realm: realm, Model: modelID, Prompt: request.Prompt, KeyID: request.KeyID,
		Params: map[string]any{"size": size, "mode": modeName(edit), "cost_hint": cost, "allowed_accounts": request.AllowedAccounts},
		Body:   body,
	}
	var result providers.MediaImageResult
	if edit {
		result, err = provider.EditImage(ctx, mediaRequest)
	} else {
		result, err = provider.GenerateImage(ctx, mediaRequest)
	}
	if err != nil {
		return ImageResponse{}, normalizeProviderError(err)
	}
	out := ImageResponse{Created: result.Created, Model: modelID, Mode: modeName(edit)}
	item := map[string]any{}
	if result.URL != "" {
		item["url"] = result.URL
	}
	if result.B64JSON != "" {
		item["b64_json"] = result.B64JSON
	}
	if result.RevisedPrompt != "" {
		item["revised_prompt"] = result.RevisedPrompt
	}
	degraded := ""
	if request.ResponseFormat == "b64_json" && item["url"] != nil {
		raw, fetchErr := s.fetchImageBytes(ctx, stringValue(item["url"]))
		if fetchErr != nil {
			degraded = "（要 b64_json 但转码失败，已退回 url：" + fetchErr.Error() + "）"
		} else {
			item["b64_json"] = base64.StdEncoding.EncodeToString(raw)
			delete(item, "url")
		}
	}
	if item["url"] != nil {
		out.Hint = "COS 签名 URL 约 24 小时失效；要长期保留请及时下载。" + degraded
	} else if degraded != "" {
		out.Hint = degraded
	}
	if out.Created <= 0 {
		out.Created = s.nowTime().Unix()
	}
	out.Data = []map[string]any{item}
	if result.Credit != nil {
		out.Credit = result.Credit
		s.Pricing.LearnImage(modelID, *result.Credit)
		s.addSpend(request.KeyID, result.Credit)
	}
	s.recordUsage(ctx, "image", realm, modelID, result.Account, request.KeyID, result.Credit, map[string]any{"resolution": size}, time.Time{})
	return out, nil
}

func modeName(edit bool) string {
	if edit {
		return "edit"
	}
	return "generate"
}

func (s *Service) mediaModels(ctx context.Context, realm string) ([]map[string]any, error) {
	models, err := s.catalogModels(ctx, realm)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(models))
	for _, model := range models {
		out = append(out, cloneModel(model))
	}
	return out, nil
}

func (s *Service) catalogModels(ctx context.Context, realm string) ([]map[string]any, error) {
	now := s.nowTime()
	s.catalogMu.Lock()
	if entry, ok := s.catalog[realm]; ok && now.Sub(entry.at) < s.catalogTTL {
		models := make([]map[string]any, 0, len(entry.models))
		for _, model := range entry.models {
			models = append(models, cloneModel(model))
		}
		s.catalogMu.Unlock()
		return models, nil
	}
	s.catalogMu.Unlock()
	provider, err := s.MediaProviderFor(realm)
	if err != nil {
		return nil, err
	}
	models, err := provider.MediaModels(ctx, realm)
	if err != nil {
		return nil, normalizeProviderError(err)
	}
	if len(models) == 0 {
		return nil, mediaError("no_media_model", "该区目录里没有任何媒体模型（该账号可能没有生图权限）")
	}
	copied := make([]map[string]any, 0, len(models))
	for _, model := range models {
		copied = append(copied, cloneModel(model))
	}
	s.catalogMu.Lock()
	s.catalog[realm] = catalogEntry{models: copied, at: now}
	s.catalogMu.Unlock()
	out := make([]map[string]any, 0, len(copied))
	for _, model := range copied {
		out = append(out, cloneModel(model))
	}
	return out, nil
}

func cloneModel(model map[string]any) map[string]any {
	out := make(map[string]any, len(model))
	for key, value := range model {
		switch typed := value.(type) {
		case map[string]any:
			out[key] = cloneModel(typed)
		case []any:
			out[key] = append([]any(nil), typed...)
		case []string:
			out[key] = append([]string(nil), typed...)
		default:
			out[key] = value
		}
	}
	return out
}

// SubmitVideo validates and either submits a video task or queues it locally.
func (s *Service) SubmitVideo(ctx context.Context, request VideoRequest) (VideoSubmission, error) {
	cfg := s.Config()
	if !cfg.Enabled {
		return VideoSubmission{}, mediaError("no_route", "not found")
	}
	realm, err := s.Realm(request.BoundRealm)
	if err != nil {
		return VideoSubmission{}, err
	}
	s.Tasks.SetConcurrency(cfg.VideoConcurrency)
	if s.Tasks.PendingCount() >= s.Tasks.MaxPending() {
		return VideoSubmission{}, mediaError("queue_full", fmt.Sprintf("生视频队列已满（%d 条在排队）；等一会儿再试，或先减少并发", s.Tasks.PendingCount()))
	}
	models, err := s.mediaModels(ctx, realm)
	if err != nil {
		return VideoSubmission{}, err
	}
	payload := request.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	refImages := referenceList(payload, "reference_images")
	hasImage := nonEmpty(payload["image"]) != "" || nonEmpty(payload["input_reference"]) != "" || len(refImages) > 0
	tag := TagTextToVideo
	if hasImage {
		tag = TagImageToVideo
	}
	model, err := PickModel(models, nonEmpty(payload["model"]), tag)
	if err != nil {
		return VideoSubmission{}, fromProtocol(err)
	}
	modelID := stringValue(model["id"])
	generation := VideoCapabilities(model)
	if err := ValidateReferenceCounts(payload, generation); err != nil {
		return VideoSubmission{}, fromProtocol(err)
	}
	requestCopy := map[string]any{}
	for key, value := range payload {
		requestCopy[key] = value
	}
	if image := nonEmpty(payload["image"]); image != "" {
		resolved, resolveErr := s.resolveInputImage(ctx, image)
		if resolveErr != nil {
			return VideoSubmission{}, resolveErr
		}
		requestCopy["image"] = resolved
	} else if image := nonEmpty(payload["input_reference"]); image != "" {
		resolved, resolveErr := s.resolveInputImage(ctx, image)
		if resolveErr != nil {
			return VideoSubmission{}, resolveErr
		}
		requestCopy["image"] = resolved
	}
	if len(refImages) > 0 {
		normalized := make([]string, 0, len(refImages))
		for _, item := range refImages {
			if isDataURL(item) {
				normalized = append(normalized, item)
				continue
			}
			resolved, resolveErr := s.resolveInputImage(ctx, item)
			if resolveErr != nil {
				return VideoSubmission{}, resolveErr
			}
			normalized = append(normalized, resolved)
		}
		requestCopy["reference_images"] = normalized
	}
	params, err := ResolveVideoParams(requestCopy, generation)
	if err != nil {
		return VideoSubmission{}, fromProtocol(err)
	}
	if seconds := intValue(params["seconds"], 0); seconds > cfg.VideoMaxSeconds {
		return VideoSubmission{}, mediaError("invalid_request", fmt.Sprintf("本网关的媒体设置把视频时长限制在 %d 秒以内（给定 %d 秒）", cfg.VideoMaxSeconds, seconds))
	}
	body, err := BuildVideoBody(modelID, requestCopy, params)
	if err != nil {
		return VideoSubmission{}, fromProtocol(err)
	}
	withReference := body["reference_images"] != nil || body["reference_videos"] != nil || body["reference_audios"] != nil
	cost := s.Pricing.CostHint("video", modelID, intValue(params["seconds"], DefaultVideoSeconds), stringValue(params["resolution"]), withReference)
	if err := s.CheckDailySpend(request.KeyID, cost); err != nil {
		return VideoSubmission{}, err
	}
	if s.Tasks.ActiveCount() >= s.Tasks.Concurrency() {
		task, createErr := s.Tasks.CreatePending(modelID, params, body, realm, request.KeyID)
		if createErr != nil {
			if errors.Is(createErr, ErrQueueFull) {
				return VideoSubmission{}, mediaError("queue_full", "生视频队列已满；等一会儿再试，或先减少并发")
			}
			return VideoSubmission{}, mediaError("upstream", createErr.Error())
		}
		return VideoSubmission{Task: task, Queued: true, QueuePosition: s.Tasks.QueuePosition(task.ID)}, nil
	}
	if !s.AcquireSlot() {
		return VideoSubmission{}, mediaError("busy", "media slots busy; retry shortly")
	}
	defer s.ReleaseSlot()
	provider, err := s.MediaProviderFor(realm)
	if err != nil {
		return VideoSubmission{}, err
	}
	busy := s.Tasks.BusyAccounts()
	excluded := make([]string, 0, len(busy))
	for account := range busy {
		excluded = append(excluded, account)
	}
	mediaRequest := providers.MediaRequest{
		Realm: realm, Model: modelID, Prompt: nonEmpty(payload["prompt"]), KeyID: request.KeyID,
		Params: params, Body: body,
	}
	mediaRequest.Params["allowed_accounts"] = request.AllowedAccounts
	mediaRequest.Params["cost_hint"] = cost
	if len(excluded) > 0 {
		mediaRequest.Params["exclude_accounts"] = excluded
	}
	submission, err := provider.SubmitVideo(ctx, mediaRequest)
	if err != nil {
		return VideoSubmission{}, normalizeProviderError(err)
	}
	task, err := s.Tasks.Create(modelID, params, submission.Account, body, submission.UpstreamID, realm, request.KeyID, submission.Warnings...)
	if err != nil {
		return VideoSubmission{}, mediaError("upstream", err.Error())
	}
	return VideoSubmission{Task: task}, nil
}

// Task returns one task row.
func (s *Service) Task(id string) (Task, bool) {
	return s.Tasks.Get(id)
}

// Cancel cancels a waiting/queued task.
func (s *Service) Cancel(id string) (bool, error) {
	return s.Tasks.Cancel(id)
}

// Tasks lists recent tasks.
func (s *Service) TasksList(limit int) ([]Task, map[string]any) {
	return s.Tasks.List(limit), s.Tasks.Stats()
}

// ContentPath resolves a task artifact.
func (s *Service) ContentPath(id string, index int) (string, bool) {
	return s.Tasks.ContentPath(id, index)
}

// Sign returns a download token.
func (s *Service) Sign(id string, index int) string {
	return s.Tasks.Sign(id, index, DefaultSignTTL)
}

// Verify checks a download token.
func (s *Service) Verify(id string, index int, token string) bool {
	return s.Tasks.Verify(id, index, token)
}

func (s *Service) recordTaskFinish(task Task) {
	if task.Credit == nil || *task.Credit <= 0 {
		return
	}
	seconds := intValue(task.Seconds, 0)
	s.Pricing.LearnVideo(task.Model, stringValue(task.Resolution), *task.Credit, seconds, task.HasReferenceMaterials)
	s.addSpend(task.Key, task.Credit)
	// v1 records a video usage row only once the settled credit is known.
	s.recordUsage(context.Background(), "video", task.Realm, task.Model, task.Account, task.Key, task.Credit,
		map[string]any{"seconds": task.Seconds, "resolution": task.Resolution,
			"aspect_ratio": task.AspectRatio, "image_to_video": task.ImageToVideo}, time.Time{})
}

func (s *Service) recordUsage(ctx context.Context, kind, realm, model, account, keyID string, credit *float64, params map[string]any, started time.Time) {
	if s.Usage == nil {
		return
	}
	usageParams := map[string]any{}
	for _, field := range []string{"seconds", "resolution", "aspect_ratio", "image_to_video"} {
		if value, ok := params[field]; ok && value != nil {
			usageParams[field] = value
		}
	}
	usageBlock := map[string]any{}
	if credit != nil {
		usageBlock["credit"] = *credit
	}
	_ = s.Usage.Record(ctx, usage.RecordInput{
		At: s.nowTime(), Model: model, Realm: realm, Account: account, KeyID: keyID,
		Kind: kind, Outcome: "completed", Usage: usageBlock, Media: usageParams,
		ElapsedMS: elapsedMillis(started, s.nowTime()),
	})
}

func elapsedMillis(start, end time.Time) int64 {
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func normalizeProviderError(err error) error {
	if err == nil {
		return nil
	}
	var mediaErr *providers.MediaError
	if errors.As(err, &mediaErr) && mediaErr != nil {
		code := mediaErr.Code
		if code == "" {
			code = "upstream"
		}
		return &Error{Code: code, Message: mediaErr.Message, Status: mediaErr.Status}
	}
	var protocolErr *ProtocolError
	if errors.As(err, &protocolErr) {
		return fromProtocol(protocolErr)
	}
	return mediaError("upstream", err.Error())
}

func fromProtocol(err error) error {
	var protocolErr *ProtocolError
	if errors.As(err, &protocolErr) && protocolErr != nil {
		return &Error{Code: protocolErr.Code, Message: protocolErr.Message}
	}
	return err
}

// resolveInputImage normalizes data URLs and public http(s) URLs. Local paths
// and private addresses are rejected before any network read.
func (s *Service) resolveInputImage(ctx context.Context, value string) (string, error) {
	kind, normalized, err := ClassifyImageInput(value)
	if err != nil {
		return "", fromProtocol(err)
	}
	if kind == "data_url" {
		return normalized, nil
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Hostname() == "" {
		return "", mediaError("invalid_request", "输入图 URL 无效")
	}
	if err := s.assertPublicHost(parsed.Hostname()); err != nil {
		return "", err
	}
	data, contentType, err := s.fetchURLBytes(ctx, normalized)
	if err != nil {
		return "", mediaError("invalid_request", "下载输入图失败："+err.Error())
	}
	if len(data) == 0 {
		return "", mediaError("invalid_request", "输入图是空文件")
	}
	if len(data) > MaxInputImageBytes {
		return "", mediaError("invalid_request", fmt.Sprintf("输入图 %.1f MB，超过 %.0f MB 上限", float64(len(data))/1048576, float64(MaxInputImageBytes)/1048576))
	}
	mimeType := strings.TrimSpace(strings.Split(contentType, ";")[0])
	if !strings.HasPrefix(mimeType, "image/") {
		mimeType = sniffImageMIME(data)
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (s *Service) assertPublicHost(host string) error {
	lookup := s.LookupIP
	if lookup == nil {
		lookup = net.LookupIP
	}
	ips, err := lookup(host)
	if err != nil || len(ips) == 0 {
		return mediaError("invalid_request", "输入图 URL 解析失败")
	}
	for _, ip := range ips {
		if !IsPublicIP(ip) {
			return mediaError("invalid_request", "输入图 URL 指向非公网地址，已拒绝（防 SSRF）")
		}
	}
	return nil
}

func (s *Service) fetchURLBytes(ctx context.Context, target string) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	httpClient := s.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 180 * time.Second}
	}
	client := *httpClient
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 1 {
			return errors.New("redirect not allowed")
		}
		if err := s.assertPublicHost(req.URL.Hostname()); err != nil {
			return err
		}
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxInputImageBytes+1025))
	if err != nil {
		return nil, "", err
	}
	if len(data) > MaxInputImageBytes {
		return nil, "", fmt.Errorf("图片超过 %d 字节上限", MaxInputImageBytes)
	}
	return data, response.Header.Get("Content-Type"), nil
}

func (s *Service) fetchImageBytes(ctx context.Context, target string) ([]byte, error) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("产物 URL 无效")
	}
	if err := s.assertPublicHost(parsed.Hostname()); err != nil {
		return nil, fmt.Errorf("产物 URL 不可访问")
	}
	data, _, err := s.fetchURLBytes(ctx, target)
	return data, err
}

// IsPublicIP rejects loopback, private, link-local, multicast, documentation,
// CGNAT, benchmarking and reserved addresses.
func IsPublicIP(ip net.IP) bool { return netguard.IsPublicIP(ip) }

func sniffImageMIME(data []byte) string {
	if len(data) >= 4 && data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' {
		return "image/png"
	}
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg"
	}
	if len(data) >= 4 && string(data[:4]) == "GIF8" {
		return "image/gif"
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	return "image/png"
}

func isDataURL(text string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "data:")
}

func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
