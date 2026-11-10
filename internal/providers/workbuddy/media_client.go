package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	mediaConfigPath       = "/v3/config"
	mediaImagePath        = "/v2/images/generations"
	mediaEditPath         = "/v2/images/edits"
	mediaVideoSubmitPath  = "/v2/videos/generations"
	mediaVideoTaskPath    = "/v2/videos/tasks"
	mediaDefaultTimeout   = 180 * time.Second
	mediaSubmitTimeout    = 60 * time.Second
	mediaPollTimeout      = 60 * time.Second
	mediaDownloadTimeout  = 120 * time.Second
	mediaMaxBodyBytes     = 8 << 20
	mediaMaxArtifactBytes = 512 << 20
	mediaRotateMax        = 3
)

func (p *Provider) mediaClient() ContextHTTPDoer {
	if p != nil && p.mediaDoer != nil {
		return p.mediaDoer
	}
	return p.taskClient()
}

func (p *Provider) mediaBase() string {
	if p != nil {
		if base := strings.TrimSpace(p.mediaBaseURL); base != "" {
			return strings.TrimRight(base, "/")
		}
		if base := strings.TrimSpace(p.tasksChatBaseURL); base != "" {
			return strings.TrimRight(base, "/")
		}
	}
	if p == nil {
		return ""
	}
	return strings.TrimRight(p.taskChatBase(), "/")
}

func (p *Provider) mediaNowTime() time.Time {
	if p != nil && p.mediaNow != nil {
		return p.mediaNow()
	}
	return time.Now()
}

func (p *Provider) mediaIdentity() string {
	if p == nil || p.store == nil {
		return ""
	}
	data, err := p.store.Load()
	if err != nil {
		return ""
	}
	media, _ := data["media"].(map[string]any)
	identity, _ := media["identity"].(string)
	switch identity {
	case "desktop", "cli":
		return identity
	default:
		return ""
	}
}

func (p *Provider) mediaHeaders(account *Account) http.Header {
	auth := taskAuthSnapshot(account)
	product := normalizeProduct(auth.Product)
	switch p.mediaIdentity() {
	case "desktop":
		product = productDesktop
	case "cli":
		product = productCLI
	}
	deviceToken, deviceTokenError := p.chatDeviceToken(auth, product)
	return p.chatHeaders(auth, product, "", "", "", deviceToken, deviceTokenError, p.Config())
}

func (p *Provider) mediaDoRaw(ctx context.Context, account *Account, method, path string, body []byte, timeout time.Duration) (int, []byte, error) {
	client := p.mediaClient()
	if client == nil {
		return 0, nil, errors.New("workbuddy: media client is nil")
	}
	if timeout <= 0 {
		timeout = mediaDefaultTimeout
	}
	if p != nil && p.mediaTimeout > 0 {
		timeout = p.mediaTimeout
	}
	requestCtx, cancel := context.WithTimeout(nonNilContext(ctx), timeout)
	defer cancel()
	endpoint := p.mediaBase() + path
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	request, err := http.NewRequestWithContext(requestCtx, method, endpoint, reader)
	if err != nil {
		return 0, nil, errors.New("workbuddy: invalid media request")
	}
	request.Header = p.mediaHeaders(account)
	proxyURL := ""
	if account != nil {
		account.mu.RLock()
		proxyURL = strings.TrimSpace(account.Auth.ProxyURL)
		account.mu.RUnlock()
	}
	response, err := client.Do(requestCtx, request, proxyURL)
	if err != nil {
		return 0, nil, errors.New("workbuddy: media request failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, mediaMaxBodyBytes+1))
	if err != nil {
		return response.StatusCode, nil, errors.New("workbuddy: media response read failed")
	}
	if len(raw) > mediaMaxBodyBytes {
		return response.StatusCode, nil, errors.New("workbuddy: media response too large")
	}
	return response.StatusCode, raw, nil
}

func (p *Provider) mediaPost(ctx context.Context, account *Account, path string, body map[string]any, timeout time.Duration, phase string) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, &providers.MediaError{Code: "invalid_request", Message: "无法编码媒体请求"}
	}
	status, payload, err := p.mediaDoRaw(ctx, account, http.MethodPost, path, raw, timeout)
	if err != nil {
		return nil, &providers.MediaError{Code: "upstream", Message: "媒体请求失败：" + err.Error()}
	}
	if status < 200 || status >= 300 {
		return nil, mediaFailure(status, string(payload), phase)
	}
	var decoded map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, &providers.MediaError{Code: "upstream", Message: "上游返回的不是 JSON 对象"}
	}
	return decoded, nil
}

func mediaFailure(status int, text, phase string) *providers.MediaError {
	message := strings.TrimSpace(text)
	if len(message) > 400 {
		message = message[:400]
	}
	if strings.HasPrefix(message, "{") {
		var parsed map[string]any
		if json.Unmarshal([]byte(message), &parsed) == nil {
			if msg, _ := parsed["msg"].(string); strings.TrimSpace(msg) != "" {
				message = msg
			}
		}
	}
	code := "upstream"
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		code = "auth"
	case status == http.StatusPaymentRequired:
		code = "no_credit"
	case status == http.StatusTooManyRequests:
		code = "rate_limit"
	case status == http.StatusNotFound:
		code = "no_route"
	case phase == "submit" && status >= 400 && status < 500:
		code = "rejected"
	}
	return &providers.MediaError{Code: code, Message: fmt.Sprintf("上游失败（HTTP %d）：%s", status, message), Status: status}
}

func mediaEnvelope(payload map[string]any) (map[string]any, error) {
	if payload == nil {
		return nil, &providers.MediaError{Code: "upstream", Message: "上游返回的不是 JSON 对象"}
	}
	if raw, ok := payload["code"]; ok {
		code := intValueAny(raw)
		if code != 0 {
			message, _ := payload["msg"].(string)
			if strings.TrimSpace(message) == "" {
				message = "unknown error"
			}
			return nil, &providers.MediaError{Code: "upstream", Message: fmt.Sprintf("上游失败（code %d）: %s", code, message)}
		}
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

func (p *Provider) fetchMediaModels(ctx context.Context, realm string) ([]providers.Model, error) {
	pool := p.Pool()
	if pool == nil {
		return nil, &providers.MediaError{Code: "no_route", Message: "该区没有可用账号，读不到媒体目录"}
	}
	var account *Account
	for _, candidate := range pool.Accounts() {
		if candidate == nil || candidate.Auth.Realm != realm {
			continue
		}
		if candidate.Auth.Enabled || !candidate.Auth.EnabledSet {
			account = candidate
			break
		}
	}
	if account == nil {
		return nil, &providers.MediaError{Code: "no_route", Message: "该区没有可用账号，读不到媒体目录"}
	}
	status, raw, err := p.mediaDoRaw(ctx, account, http.MethodGet, mediaConfigPath, nil, 15*time.Second)
	if err != nil {
		return nil, &providers.MediaError{Code: "upstream", Message: "读取产品配置失败：" + err.Error()}
	}
	if status < 200 || status >= 300 {
		return nil, mediaFailure(status, string(raw), "config")
	}
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, &providers.MediaError{Code: "upstream", Message: "产品配置不是 JSON"}
	}
	return mediaModelsFromPayload(payload), nil
}

func mediaModelsFromPayload(payload map[string]any) []providers.Model {
	var entries []any
	if data, _ := payload["data"].(map[string]any); data != nil {
		entries, _ = data["models"].([]any)
	}
	if entries == nil {
		entries, _ = payload["models"].([]any)
	}
	out := make([]providers.Model, 0, len(entries))
	seen := map[string]bool{}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		id, _ := entry["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		tags := stringSliceAny(entry["tags"])
		if !hasAnyMediaTag(tags) {
			continue
		}
		model := providers.Model{
			"id": id, "name": entry["name"], "tags": tags,
		}
		if name, _ := model["name"].(string); strings.TrimSpace(name) == "" {
			model["name"] = id
		}
		if generation, ok := entry["videoGeneration"].(map[string]any); ok && generation != nil {
			model["videoGeneration"] = generation
		}
		seen[id] = true
		out = append(out, model)
	}
	return out
}

func hasAnyMediaTag(tags []string) bool {
	for _, tag := range tags {
		switch tag {
		case "text-to-image", "image-to-image", "text-to-video", "image-to-video":
			return true
		}
	}
	return false
}

func stringSliceAny(value any) []string {
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	default:
		return nil
	}
}

func intValueAny(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		number, _ := typed.Int64()
		return int(number)
	default:
		return 0
	}
}

func (p *Provider) mediaBudgetOK(account *Account, model string, cost float64) (bool, string) {
	if account == nil {
		return false, "账号不可用"
	}
	account.mu.RLock()
	credits := account.Credits
	limit := account.DailyCreditLimit
	used := account.DailyCreditsToday
	account.mu.RUnlock()
	if credits != nil && credits.Remain < cost {
		return false, fmt.Sprintf("缓存余额 %g 积分 < 本次预估 %g", credits.Remain, cost)
	}
	if limit > 0 && used != nil {
		headroom := float64(limit) - *used
		if headroom < cost {
			return false, fmt.Sprintf("今日额度剩 %g 积分 < 本次预估 %g", headroom, cost)
		}
	}
	return true, ""
}

func (p *Provider) pickMediaAccount(model string, excluded map[string]bool, cost float64) (*Account, error) {
	pool := p.Pool()
	if pool == nil {
		return nil, &providers.MediaError{Code: "upstream", Message: "账号池不可用"}
	}
	skip := map[string]bool{}
	for key, value := range excluded {
		skip[key] = value
	}
	reasons := []string{}
	for attempt := 0; attempt < 4; attempt++ {
		account, err := pool.Pick(model, "", skip)
		if err != nil || account == nil {
			break
		}
		ok, reason := p.mediaBudgetOK(account, model, cost)
		if ok {
			return account, nil
		}
		reasons = append(reasons, account.UID()[:mediaMinInt(8, len(account.UID()))]+" "+reason)
		skip[account.UID()] = true
	}
	if len(reasons) > 0 {
		return nil, &providers.MediaError{Code: "no_credit", Message: "没有账号付得起这次调用（预估 " + fmt.Sprintf("%g", cost) + " 积分）：" + strings.Join(reasons, "；")}
	}
	return nil, &providers.MediaError{Code: "upstream", Message: "没有可用账号跑这次媒体请求"}
}

func mediaMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (p *Provider) noteMediaFailure(account *Account, err error, model string) {
	if account == nil || err == nil {
		return
	}
	var mediaErr *providers.MediaError
	if !errors.As(err, &mediaErr) || mediaErr == nil {
		return
	}
	switch mediaErr.Code {
	case "rejected", "invalid_request", "no_media_model":
		return
	}
	_ = p.Pool().NoteModelError(account, model, mediaErr.Message, time.Minute)
}

func (p *Provider) noteMediaSuccess(account *Account, model string, credit *float64) {
	if account == nil {
		return
	}
	if credit != nil && *credit > 0 {
		account.ConsumeCredit(*credit)
	}
	_ = p.Pool().NoteSuccess(account, model)
}

func (p *Provider) accountByUID(uid string) *Account {
	if p == nil || p.Pool() == nil {
		return nil
	}
	return p.Pool().Get(uid)
}

func (p *Provider) writeMediaArtifact(ctx context.Context, account *Account, target, source string) error {
	parsed, err := url.Parse(source)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return &providers.MediaError{Code: "upstream", Message: "产物 URL 无效"}
	}
	client := p.mediaClient()
	if client == nil {
		return &providers.MediaError{Code: "upstream", Message: "媒体客户端不可用"}
	}
	requestCtx, cancel := context.WithTimeout(nonNilContext(ctx), mediaDownloadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, source, nil)
	if err != nil {
		return &providers.MediaError{Code: "upstream", Message: "产物请求无效"}
	}
	proxyURL := ""
	if account != nil {
		account.mu.RLock()
		proxyURL = strings.TrimSpace(account.Auth.ProxyURL)
		account.mu.RUnlock()
	}
	response, err := client.Do(requestCtx, request, proxyURL)
	if err != nil {
		return &providers.MediaError{Code: "upstream", Message: "下载产物失败"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &providers.MediaError{Code: "upstream", Message: fmt.Sprintf("下载产物失败（HTTP %d）", response.StatusCode)}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return &providers.MediaError{Code: "upstream", Message: "无法创建产物目录"}
	}
	tmp := target + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return &providers.MediaError{Code: "upstream", Message: "无法写入产物"}
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, mediaMaxArtifactBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written == 0 {
		_ = os.Remove(tmp)
		return &providers.MediaError{Code: "empty_result", Message: "下载到的产物是空的"}
	}
	if written > mediaMaxArtifactBytes {
		_ = os.Remove(tmp)
		return &providers.MediaError{Code: "upstream", Message: "产物超过大小上限"}
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return &providers.MediaError{Code: "upstream", Message: "无法保存产物"}
	}
	return nil
}
