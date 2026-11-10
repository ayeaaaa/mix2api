package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// MediaModels implements providers.MediaProvider.
func (p *Provider) MediaModels(ctx context.Context, realm string) ([]providers.Model, error) {
	if p == nil {
		return nil, &providers.MediaError{Code: "no_route", Message: "媒体能力不可用"}
	}
	return p.fetchMediaModels(ctx, realm)
}

// GenerateImage implements providers.MediaProvider.
func (p *Provider) GenerateImage(ctx context.Context, req providers.MediaRequest) (providers.MediaImageResult, error) {
	return p.runImage(ctx, req, false)
}

// EditImage implements providers.MediaProvider.
func (p *Provider) EditImage(ctx context.Context, req providers.MediaRequest) (providers.MediaImageResult, error) {
	return p.runImage(ctx, req, true)
}

func (p *Provider) runImage(ctx context.Context, req providers.MediaRequest, edit bool) (providers.MediaImageResult, error) {
	body := req.Body
	if body == nil {
		size, _ := req.Params["size"].(string)
		var err error
		if edit {
			body, err = wbBuildImageBody(req.Model, req.Prompt, size)
			if err == nil {
				images, _ := req.Params["images"].([]string)
				if len(images) > 0 {
					body["image"] = images
				}
			}
		} else {
			body, err = wbBuildImageBody(req.Model, req.Prompt, size)
		}
		if err != nil {
			return providers.MediaImageResult{}, err
		}
	}
	cost, _ := mediaNumber(req.Params["cost_hint"])
	excluded := excludedAccounts(req.Params)
	var lastErr error
	for attempt := 0; attempt < mediaRotateMax; attempt++ {
		account, err := p.pickMediaAccount(req.Model, excluded, cost)
		if err != nil {
			if lastErr != nil {
				return providers.MediaImageResult{}, lastErr
			}
			return providers.MediaImageResult{}, err
		}
		path := mediaImagePath
		if edit {
			path = mediaEditPath
		}
		payload, err := p.mediaPost(ctx, account, path, body, mediaDefaultTimeout, "image")
		if err != nil {
			lastErr = err
			p.noteMediaFailure(account, err, req.Model)
			if mediaRetryable(err) {
				excluded[account.UID()] = true
				continue
			}
			return providers.MediaImageResult{}, err
		}
		result, err := parseMediaImage(payload)
		if err != nil {
			lastErr = err
			p.noteMediaFailure(account, err, req.Model)
			if mediaRetryable(err) {
				excluded[account.UID()] = true
				continue
			}
			return providers.MediaImageResult{}, err
		}
		result.Account = account.UID()
		result.Kind = "image"
		p.noteMediaSuccess(account, req.Model, result.Credit)
		return result, nil
	}
	if lastErr == nil {
		lastErr = &providers.MediaError{Code: "upstream", Message: "media request failed on every account"}
	}
	return providers.MediaImageResult{}, lastErr
}

// SubmitVideo implements providers.MediaProvider.
func (p *Provider) SubmitVideo(ctx context.Context, req providers.MediaRequest) (providers.MediaVideoSubmit, error) {
	body := req.Body
	if body == nil {
		built, err := wbBuildVideoBody(req.Model, req.Params)
		if err != nil {
			return providers.MediaVideoSubmit{}, err
		}
		body = built
	}
	cost, _ := mediaNumber(req.Params["cost_hint"])
	excluded := excludedAccounts(req.Params)
	var lastErr error
	for attempt := 0; attempt < mediaRotateMax; attempt++ {
		account, err := p.pickMediaAccount(req.Model, excluded, cost)
		if err != nil {
			if lastErr != nil {
				return providers.MediaVideoSubmit{}, lastErr
			}
			return providers.MediaVideoSubmit{}, err
		}
		payload, err := p.mediaPost(ctx, account, mediaVideoSubmitPath, body, mediaSubmitTimeout, "submit")
		if err != nil {
			lastErr = err
			p.noteMediaFailure(account, err, req.Model)
			if mediaRetryable(err) {
				excluded[account.UID()] = true
				continue
			}
			return providers.MediaVideoSubmit{}, err
		}
		data, err := mediaEnvelope(payload)
		if err != nil {
			lastErr = err
			p.noteMediaFailure(account, err, req.Model)
			continue
		}
		taskID, _ := data["id"].(string)
		taskID = strings.TrimSpace(taskID)
		if taskID == "" {
			lastErr = &providers.MediaError{Code: "empty_result", Message: "上游没返回 task id，无法进入轮询"}
			p.noteMediaFailure(account, lastErr, req.Model)
			continue
		}
		p.noteMediaSuccess(account, req.Model, nil)
		return providers.MediaVideoSubmit{UpstreamID: taskID, Account: account.UID()}, nil
	}
	if lastErr == nil {
		lastErr = &providers.MediaError{Code: "upstream", Message: "提交生视频任务失败"}
	}
	return providers.MediaVideoSubmit{}, lastErr
}

// PollVideo implements providers.MediaProvider.
func (p *Provider) PollVideo(ctx context.Context, accountUID, upstreamID string) (providers.MediaVideoState, error) {
	account := p.accountByUID(accountUID)
	if account == nil {
		return providers.MediaVideoState{}, &providers.MediaError{Code: "task_lost", Message: "提交该任务的账号当前不可用，上游任务可能仍在进行且已扣费"}
	}
	payload, err := p.mediaPost(ctx, account, mediaVideoTaskPath, map[string]any{"task_id": upstreamID}, mediaPollTimeout, "poll")
	if err != nil {
		return providers.MediaVideoState{}, err
	}
	data, err := mediaEnvelope(payload)
	if err != nil {
		return providers.MediaVideoState{}, err
	}
	status, _ := data["status"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		status = "unknown"
	}
	state := providers.MediaVideoState{Status: status}
	if items, ok := data["data"].([]any); ok && status == "completed" {
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if item == nil {
				continue
			}
			if url, _ := item["url"].(string); strings.TrimSpace(url) != "" {
				state.Videos = append(state.Videos, strings.TrimSpace(url))
			}
		}
	}
	if usageBlock, ok := data["usage"].(map[string]any); ok {
		if credit, ok := mediaNumber(usageBlock["credit"]); ok {
			state.Credit = &credit
		}
	}
	state.Error, _ = mediaTaskError(data)
	if status == "failed" && state.Error == "" {
		state.Error = "上游没有给出原因"
	}
	return state, nil
}

// DownloadVideo implements providers.MediaProvider.
func (p *Provider) DownloadVideo(ctx context.Context, accountUID, source, dest string) error {
	return p.writeMediaArtifact(ctx, p.accountByUID(accountUID), dest, source)
}

// CancelVideo implements providers.MediaProvider. Upstream has no confirmed
// cancel API, so this is a no-op; local polling cancellation is store-owned.
func (p *Provider) CancelVideo(ctx context.Context, account, upstreamID string) error {
	return nil
}

func parseMediaImage(payload map[string]any) (providers.MediaImageResult, error) {
	data, err := mediaEnvelope(payload)
	if err != nil {
		return providers.MediaImageResult{}, err
	}
	items, _ := data["data"].([]any)
	var first map[string]any
	if len(items) > 0 {
		first, _ = items[0].(map[string]any)
	}
	if first == nil {
		return providers.MediaImageResult{}, &providers.MediaError{Code: "empty_result", Message: "上游返回成功，但 data.data 里既没有 url 也没有 b64_json"}
	}
	result := providers.MediaImageResult{}
	result.URL, _ = first["url"].(string)
	result.B64JSON, _ = first["b64_json"].(string)
	result.RevisedPrompt, _ = first["revised_prompt"].(string)
	if strings.TrimSpace(result.URL) == "" && strings.TrimSpace(result.B64JSON) == "" {
		return providers.MediaImageResult{}, &providers.MediaError{Code: "empty_result", Message: "上游返回成功，但 data.data 里既没有 url 也没有 b64_json"}
	}
	if usageBlock, ok := data["usage"].(map[string]any); ok {
		if credit, ok := mediaNumber(usageBlock["credit"]); ok {
			result.Credit = &credit
		}
	}
	if created, ok := mediaNumber(data["created"]); ok {
		result.Created = int64(created)
	}
	return result, nil
}

func mediaTaskError(data map[string]any) (string, string) {
	raw, ok := data["error"]
	if !ok || raw == nil {
		if text, _ := data["error_message"].(string); strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text), ""
		}
		return "", ""
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed), ""
	case map[string]any:
		message := firstText(typed["message"], typed["msg"], typed["error_message"], typed["detail"])
		code := firstText(typed["code"], typed["error_code"], typed["type"])
		if message == "" && code != "" {
			message = "上游错误码 " + code + "（未给说明）"
		}
		if message == "" {
			rawJSON, _ := json.Marshal(typed)
			message = string(rawJSON)
			if len(message) > 400 {
				message = message[:400]
			}
		}
		return message, code
	default:
		return "", ""
	}
}

func firstText(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func mediaRetryable(err error) bool {
	var mediaErr *providers.MediaError
	if !errors.As(err, &mediaErr) || mediaErr == nil {
		return false
	}
	switch mediaErr.Code {
	case "auth", "rate_limit", "no_credit", "upstream":
		return true
	default:
		return false
	}
}

func excludedAccounts(params map[string]any) map[string]bool {
	out := map[string]bool{}
	switch typed := params["exclude_accounts"].(type) {
	case []string:
		for _, value := range typed {
			out[value] = true
		}
	case []any:
		for _, value := range typed {
			if text, ok := value.(string); ok {
				out[text] = true
			}
		}
	}
	return out
}

func mediaNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

// wbBuildImageBody / wbBuildVideoBody are minimal provider-side builders used
// when the platform did not supply a body. They mirror the wire fields the
// upstream expects and deliberately never read local paths.
func wbBuildImageBody(model, prompt, size string) (map[string]any, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, &providers.MediaError{Code: "invalid_request", Message: "prompt 必填，且必须是非空字符串"}
	}
	if strings.TrimSpace(size) == "" {
		size = "1024x1024"
	}
	body := map[string]any{"model": model, "prompt": prompt, "size": size}
	if strings.HasPrefix(model, "hunyuan-") {
		body["n"] = 1
		return body, nil
	}
	if strings.HasPrefix(model, "gemini-") {
		return body, nil
	}
	body["response_format"] = "b64_json"
	return body, nil
}

func wbBuildVideoBody(model string, params map[string]any) (map[string]any, error) {
	body := map[string]any{
		"prompt":          stringParam(params, "prompt"),
		"model":           model,
		"seconds":         intParam(params, "seconds", 5),
		"negative_prompt": stringParam(params, "negative_prompt"),
		"watermark":       false,
	}
	if image := stringParam(params, "image"); image != "" {
		body["image_url"] = image
	}
	if last := stringParam(params, "last_image"); last != "" {
		body["last_image_url"] = last
	}
	extra := map[string]any{
		"resolution":   stringParam(params, "resolution"),
		"enable_audio": params["enable_audio"],
	}
	if !boolParam(params, "image_to_video") {
		extra["aspect_ratio"] = stringParam(params, "aspect_ratio")
	}
	body["extra_parameters"] = extra
	return body, nil
}

func stringParam(params map[string]any, key string) string {
	text, _ := params[key].(string)
	return text
}

func intParam(params map[string]any, key string, fallback int) int {
	if value, ok := mediaNumber(params[key]); ok {
		return int(value)
	}
	return fallback
}

func boolParam(params map[string]any, key string) bool {
	value, _ := params[key].(bool)
	return value
}
