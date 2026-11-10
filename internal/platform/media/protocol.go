package media

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Protocol constants mirror v1's wb_media.py.
const (
	TagTextToImage  = "text-to-image"
	TagImageToImage = "image-to-image"
	TagTextToVideo  = "text-to-video"
	TagImageToVideo = "image-to-video"

	ImageMaxPixels      = 4 * 1024 * 1024
	MaxInputImageBytes  = 8 * 1024 * 1024
	DefaultImageSize    = "1024x1024"
	DefaultVideoSeconds = 5
	DefaultAspectRatio  = "16:9"
	PromptLimitDefault  = 2500
)

var sizePattern = regexp.MustCompile(`^(\d{2,5})\s*[xX×*]\s*(\d{2,5})$`)

var resolutionLadder = map[string]int{
	"480P": 480, "720P": 720, "768P": 768, "1080P": 1080,
	"2K": 1440, "1440P": 1440, "4K": 2160,
}

var dataURLPattern = regexp.MustCompile(`^data:([^;,]+);base64,(.+)$`)

// ProtocolError is a locally rejected media argument. It carries the v1 stable
// error code so the API can pick the exact status and message.
type ProtocolError struct {
	Code    string
	Message string
}

func (e *ProtocolError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func invalidMedia(message string) *ProtocolError {
	return &ProtocolError{Code: "invalid_request", Message: message}
}

// ParseDimensions parses widthxheight without applying the image pixel cap.
// Video resolution mapping must accept 4K sizes that image generation rejects.
func ParseDimensions(value string) (int, int, string, error) {
	text := strings.TrimSpace(value)
	if text == "" || strings.EqualFold(text, "auto") {
		text = DefaultImageSize
	}
	match := sizePattern.FindStringSubmatch(text)
	if match == nil {
		return 0, 0, "", invalidMedia(fmt.Sprintf("size 要写成「宽x高」，如 1024x1024、1536x1024、1024x1536（给定 %q）", value))
	}
	width, _ := strconv.Atoi(match[1])
	height, _ := strconv.Atoi(match[2])
	if width < 16 || height < 16 {
		return 0, 0, "", invalidMedia(fmt.Sprintf("size 的宽高都得大于 16 像素（给定 %s）", text))
	}
	return width, height, fmt.Sprintf("%dx%d", width, height), nil
}

// ParseSize parses widthxheight; empty/auto means the default. It rejects
// dimensions below 16 and totals above 4 MP for image generation.
func ParseSize(value string) (int, int, string, error) {
	width, height, normalized, err := ParseDimensions(value)
	if err != nil {
		return 0, 0, "", err
	}
	if width*height > ImageMaxPixels {
		return 0, 0, "", invalidMedia(fmt.Sprintf("size %dx%d = %.2f MP 超过上限 %.2f MP。上限是按总像素算的：2048x2048、2048x1536、2560x1440、2560x1600 都在限内，2560x2560 起会被上游直接拒绝且不会降级。",
			width, height, float64(width*height)/1e6, float64(ImageMaxPixels)/1e6))
	}
	return width, height, normalized, nil
}

// CheckSize validates and normalizes an OpenAI image size.
func CheckSize(value string) (string, error) {
	_, _, normalized, err := ParseSize(value)
	return normalized, err
}

// ClassifyImageInput accepts only data:image/*;base64 or a public http(s) URL.
// Local paths and file:// are always rejected: the gateway is not allowed to
// read arbitrary server files for an upstream image call.
func ClassifyImageInput(value string) (kind, normalized string, err error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return "", "", invalidMedia("image 是空字符串")
	}
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "data:") {
		match := dataURLPattern.FindStringSubmatch(text)
		if match == nil {
			return "", "", invalidMedia("image 看起来是 data URL，但格式不对（需要 data:image/*;base64,...）")
		}
		mimeType := strings.ToLower(match[1])
		if !strings.HasPrefix(mimeType, "image/") {
			return "", "", invalidMedia(fmt.Sprintf("data URL 的媒体类型必须是 image/*，给定 %s", mimeType))
		}
		if strings.TrimSpace(match[2]) == "" {
			return "", "", invalidMedia("data URL 里的 base64 是空的")
		}
		if size := DecodedBase64Length(text); size <= 0 {
			return "", "", invalidMedia("data URL 解不出内容")
		} else if size > MaxInputImageBytes {
			return "", "", invalidMedia(fmt.Sprintf("输入图（内联 data URL） %.1f MB，超过 %.0f MB 上限", float64(size)/1048576, float64(MaxInputImageBytes)/1048576))
		}
		return "data_url", text, nil
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return "url", text, nil
	}
	if strings.HasPrefix(lower, "file://") {
		return "", "", invalidMedia("本网关不接受 file:// 输入（会把服务器本地文件发给上游）。请把图片读成 data:image/*;base64,... 再传。")
	}
	return "", "", invalidMedia("image 只接受 data:image/*;base64,... 或公网 http(s) URL。本地路径与 file:// 一律拒绝——那会让网关读取并把服务器上的文件发给上游。")
}

// DecodedBase64Length is the decoded byte count of a data URL.
func DecodedBase64Length(text string) int {
	match := dataURLPattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return 0
	}
	body := strings.TrimSpace(match[2])
	padding := strings.Count(body, "=")
	return len(body)*3/4 - padding
}

// SplitDataURL decodes a data URL into bytes plus a suggested filename.
func SplitDataURL(text string) ([]byte, string, error) {
	match := dataURLPattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return nil, "", invalidMedia("不是 data URL")
	}
	mimeType := strings.ToLower(strings.TrimSpace(match[1]))
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(match[2]))
	if err != nil || len(data) == 0 {
		return nil, "", invalidMedia("data URL 解不出内容")
	}
	ext := ""
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		ext = exts[0]
	}
	switch mimeType {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	case "video/mp4":
		ext = ".mp4"
	case "video/quicktime":
		ext = ".mov"
	case "audio/mpeg":
		ext = ".mp3"
	case "audio/wav", "audio/x-wav":
		ext = ".wav"
	}
	if ext == "" {
		ext = ".bin"
	}
	return data, "upload" + ext, nil
}

// BuildImageBody mirrors v1's prefix-dependent body.
func BuildImageBody(model, prompt, size string) (map[string]any, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, invalidMedia("prompt 必填，且必须是非空字符串")
	}
	if strings.TrimSpace(model) == "" {
		return nil, invalidMedia("model 必填")
	}
	checked, err := CheckSize(size)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"model": model, "prompt": prompt, "size": checked}
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

// BuildEditBody adds the data-URL array expected by the edits endpoint.
func BuildEditBody(model, prompt, size string, images []string) (map[string]any, error) {
	if len(images) == 0 {
		return nil, invalidMedia("改图要带 image（data URL）。没有输入图的改图请求是调用方的拼接错误，不是可以重试的上游问题。")
	}
	for _, image := range images {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(image)), "data:image/") {
			return nil, invalidMedia("改图的 image 必须已经归一成 data:image/*;base64,... 形式")
		}
	}
	body, err := BuildImageBody(model, prompt, size)
	if err != nil {
		return nil, err
	}
	body["image"] = images
	return body, nil
}

// CoerceSeconds accepts the OpenAI string/number seconds spelling.
func CoerceSeconds(value any) (int, bool) {
	switch typed := value.(type) {
	case bool:
		return 0, false
	case nil:
		return 0, false
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil || number != float64(int(number)) {
			return 0, false
		}
		return int(number), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		number, err := strconv.ParseFloat(typed.String(), 64)
		if err != nil || number != float64(int(number)) {
			return 0, false
		}
		return int(number), true
	case uint:
		return int(typed), true
	default:
		return 0, false
	}
}

// SizeToResolution maps a size to the model's ladder by short edge.
func SizeToResolution(size string, resolutions []string, fallback string) (string, string) {
	width, height, normalized, err := ParseDimensions(size)
	if err != nil {
		return fallback, ""
	}
	short := width
	if height < short {
		short = height
	}
	best := ""
	bestPixels := -1
	for _, name := range resolutions {
		pixels, ok := resolutionLadder[strings.ToUpper(name)]
		if ok && pixels <= short && pixels > bestPixels {
			best, bestPixels = name, pixels
		}
	}
	if best == "" {
		return fallback, fmt.Sprintf("size %s 低于该模型的最低分辨率档（%s）；改用 %s", normalized, strings.Join(resolutions, "、"), fallback)
	}
	return best, ""
}

// AspectRatioFromSize is used for text-to-video only.
func AspectRatioFromSize(size string) string {
	width, height, _, err := ParseDimensions(size)
	if err != nil {
		return DefaultAspectRatio
	}
	if width > height {
		return "16:9"
	}
	if width < height {
		return "9:16"
	}
	return "1:1"
}

// ResolveVideoParams validates a request against the model's declared
// videoGeneration block. Unknown constraints are passed through.
func ResolveVideoParams(request map[string]any, generation map[string]any) (map[string]any, error) {
	prompt, _ := request["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, invalidMedia("prompt 必填，且必须是非空字符串")
	}
	limit := intValue(generation["maxPromptLength"], PromptLimitDefault)
	if limit <= 0 {
		limit = PromptLimitDefault
	}
	if len([]rune(prompt)) > limit {
		return nil, invalidMedia(fmt.Sprintf("prompt 不能超过 %d 字符（该模型的 maxPromptLength）", limit))
	}
	if negative, ok := request["negative_prompt"].(string); ok && len([]rune(negative)) > limit {
		return nil, invalidMedia(fmt.Sprintf("negative_prompt 不能超过 %d 字符（该模型的 maxPromptLength）", limit))
	}

	rawSeconds, hasSeconds := request["seconds"]
	seconds := 0
	if hasSeconds && rawSeconds != nil {
		parsed, ok := CoerceSeconds(rawSeconds)
		if !ok {
			return nil, invalidMedia(fmt.Sprintf("seconds 要写成秒数（如 5 或 \"5\"），给定 %v", rawSeconds))
		}
		seconds = parsed
	}
	if seconds <= 0 {
		seconds = intValue(generation["defaultSeconds"], DefaultVideoSeconds)
		if seconds <= 0 {
			seconds = DefaultVideoSeconds
		}
	}
	low := intValue(generation["minSeconds"], 0)
	high := intValue(generation["maxSeconds"], 0)
	if low > 0 && high > 0 && (seconds < low || seconds > high) {
		return nil, invalidMedia(fmt.Sprintf("时长必须在 %d 到 %d 秒之间（给定 %d）", low, high, seconds))
	}

	hasImage := nonEmpty(request["image"]) != "" || nonEmpty(request["input_reference"]) != ""
	refImages := referenceList(request, "reference_images")
	if len(refImages) > 0 {
		hasImage = true
	}
	imageToVideo := hasImage

	resolutions := stringList(generation["resolutions"])
	defaultResolution := stringValue(generation["defaultResolution"])
	resolution := defaultResolution
	askedResolution := nonEmpty(request["resolution"])
	askedSize := nonEmpty(request["size"])
	warnings := []string{}
	if askedResolution != "" {
		if len(resolutions) > 0 {
			hit := ""
			for _, candidate := range resolutions {
				if candidate == askedResolution || strings.EqualFold(candidate, askedResolution) {
					hit = candidate
					break
				}
			}
			if hit == "" {
				warnings = append(warnings, fmt.Sprintf("resolution %s 不被该模型支持（可选 %s）；改用 %s", askedResolution, strings.Join(resolutions, "、"), defaultResolution))
				resolution = defaultResolution
			} else {
				resolution = hit
			}
		} else {
			resolution = askedResolution
		}
	} else if askedSize != "" {
		var warning string
		resolution, warning = SizeToResolution(askedSize, resolutions, defaultResolution)
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}

	aspect := ""
	if !imageToVideo {
		allowed := stringList(generation["aspectRatios"])
		if len(allowed) == 0 {
			allowed = []string{"16:9", "9:16", "1:1"}
		}
		asked := nonEmpty(request["aspect_ratio"])
		if asked == "" {
			if askedSize != "" {
				asked = AspectRatioFromSize(askedSize)
			} else {
				asked = stringValue(generation["defaultAspectRatio"])
			}
		}
		if asked == "" {
			asked = DefaultAspectRatio
		}
		hit := ""
		for _, candidate := range allowed {
			if candidate == asked || strings.EqualFold(candidate, asked) {
				hit = candidate
				break
			}
		}
		if hit == "" {
			fallback := stringValue(generation["defaultAspectRatio"])
			if fallback == "" {
				fallback = DefaultAspectRatio
			}
			warnings = append(warnings, fmt.Sprintf("aspect_ratio %s 不在该模型的可选集合（%s）；改用 %s", asked, strings.Join(allowed, "、"), fallback))
			hit = fallback
		}
		aspect = hit
	}

	enableAudio := true
	if value, ok := request["enable_audio"].(bool); ok {
		enableAudio = value
	} else if value, ok := generation["defaultEnableAudio"].(bool); ok {
		enableAudio = value
	}
	if !enableAudio && stringValue(generation["audioMode"]) == "always-on" {
		warnings = append(warnings, "该模型音频常开（audioMode=always-on），enable_audio=false 可能被忽略")
		enableAudio = true
	}

	result := map[string]any{
		"seconds": seconds, "enable_audio": enableAudio,
		"image_to_video": imageToVideo, "warnings": warnings,
	}
	if strings.TrimSpace(resolution) != "" {
		result["resolution"] = resolution
	}
	if strings.TrimSpace(aspect) != "" {
		result["aspect_ratio"] = aspect
	}
	return result, nil
}

// BuildVideoBody mirrors v1's submit body. watermark is fixed false.
func BuildVideoBody(model string, request, params map[string]any) (map[string]any, error) {
	if strings.TrimSpace(model) == "" {
		return nil, invalidMedia("model 必填")
	}
	body := map[string]any{
		"prompt":          stringValue(request["prompt"]),
		"model":           model,
		"seconds":         intValue(params["seconds"], DefaultVideoSeconds),
		"negative_prompt": stringValue(request["negative_prompt"]),
		"watermark":       false,
	}
	if image := nonEmpty(request["image"]); image != "" {
		body["image_url"] = image
	} else if image := nonEmpty(request["input_reference"]); image != "" {
		body["image_url"] = image
	}
	if last := nonEmpty(request["last_image"]); last != "" {
		body["last_image_url"] = last
	}
	for _, kind := range []struct {
		requestKey  string
		upstreamKey string
		innerKey    string
	}{
		{"reference_images", "reference_images", "image_url"},
		{"reference_videos", "reference_videos", "video_url"},
		{"reference_audios", "reference_audios", "audio_url"},
	} {
		items := referenceList(request, kind.requestKey)
		if len(items) == 0 {
			continue
		}
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			out = append(out, map[string]any{kind.innerKey: item})
		}
		body[kind.upstreamKey] = out
	}
	extra := map[string]any{
		"resolution":   stringValue(params["resolution"]),
		"enable_audio": params["enable_audio"],
	}
	if !boolValue(params["image_to_video"]) {
		extra["aspect_ratio"] = stringValue(params["aspect_ratio"])
	}
	body["extra_parameters"] = extra
	return body, nil
}

// ValidateReferenceCounts applies the model's declared per-kind and total caps.
func ValidateReferenceCounts(request, generation map[string]any) error {
	counts := []struct {
		requestKey string
		capKey     string
		label      string
	}{
		{"reference_images", "maxReferenceImages", "参考图"},
		{"reference_videos", "maxReferenceVideos", "参考视频"},
		{"reference_audios", "maxReferenceAudios", "参考音频"},
	}
	total := 0
	kinds := 0
	for _, entry := range counts {
		items := referenceList(request, entry.requestKey)
		if len(items) == 0 {
			continue
		}
		kinds++
		if cap, ok := optionalInt(generation[entry.capKey]); ok && len(items) > cap {
			if cap == 0 {
				return invalidMedia(fmt.Sprintf("该模型不支持%s（%s = 0）；去掉 %s 再提交", entry.label, entry.capKey, entry.requestKey))
			}
			return invalidMedia(fmt.Sprintf("%s最多 %d 个（给定 %d 个）", entry.label, cap, len(items)))
		}
		total += len(items)
	}
	if total > 0 {
		if cap, ok := optionalInt(generation["maxReferenceMaterials"]); ok && total > cap {
			return invalidMedia(fmt.Sprintf("参考素材合计最多 %d 个（给定 %d 个）", cap, total))
		}
		hasFrame := nonEmpty(request["image"]) != "" || nonEmpty(request["input_reference"]) != "" ||
			nonEmpty(request["last_image"]) != ""
		if hasFrame && boolValue(generation["framesExclusiveWithReferenceMaterials"]) {
			return invalidMedia("该模型的首/尾帧与参考素材互斥：要么给首帧（image / input_reference），要么给参考图，不要同时给")
		}
		if !boolValue(generation["allowMixedReferenceMaterials"]) && kinds > 1 {
			return invalidMedia("该模型不允许混用多类参考素材")
		}
	}
	return nil
}

// PickModel chooses a model by capability tag, matching v1's errors.
func PickModel(models []map[string]any, requested, tag string) (map[string]any, error) {
	group := make([]map[string]any, 0)
	for _, model := range models {
		if hasTag(model, tag) {
			group = append(group, model)
		}
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		for _, model := range group {
			if stringValue(model["id"]) == requested {
				return model, nil
			}
		}
		names := make([]string, 0, len(group))
		for _, model := range group {
			names = append(names, stringValue(model["id"]))
		}
		known := strings.Join(names, "、")
		if known == "" {
			known = "（空）"
		}
		return nil, &ProtocolError{Code: "no_media_model", Message: fmt.Sprintf("目录里没有%s模型「%s」。可用：%s", tagLabel(tag), requested, known)}
	}
	if len(group) == 0 {
		return nil, &ProtocolError{Code: "no_media_model", Message: fmt.Sprintf("该区账号的产品配置里没有任何带 %s 标签的模型（%s）。国内区文生图应是 hunyuan-image-alpha、改图 hunyuan-image-alpha-edit、视频是 kling-video-v3 / minimax-video-h3；一个都没有说明该区目录暂时没有这类模型。", tag, tagLabel(tag))}
	}
	return group[0], nil
}

func hasTag(model map[string]any, tag string) bool {
	for _, candidate := range stringList(model["tags"]) {
		if candidate == tag {
			return true
		}
	}
	return false
}

func tagLabel(tag string) string {
	switch tag {
	case TagTextToImage:
		return "文生图"
	case TagImageToImage:
		return "图生图"
	case TagTextToVideo:
		return "文生视频"
	case TagImageToVideo:
		return "图生视频"
	default:
		return tag
	}
}

func referenceList(request map[string]any, key string) []string {
	raw, ok := request[key]
	if !ok && key == "reference_images" {
		raw = request["images"]
	}
	if raw == nil {
		return nil
	}
	var items []any
	switch typed := raw.(type) {
	case string:
		items = []any{typed}
	case []string:
		for _, item := range typed {
			items = append(items, item)
		}
	case []any:
		items = typed
	default:
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		switch typed := item.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				out = append(out, typed)
			}
		case map[string]any:
			value := nonEmpty(typed["url"])
			if value == "" {
				value = nonEmpty(typed["image_url"])
			}
			if value == "" {
				value = nonEmpty(typed["video_url"])
			}
			if value == "" {
				value = nonEmpty(typed["audio_url"])
			}
			if value != "" {
				out = append(out, value)
			}
		}
	}
	return out
}

func nonEmpty(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func boolValue(value any) bool {
	flag, _ := value.(bool)
	return flag
}

func intValue(value any, fallback int) int {
	if number, ok := optionalInt(value); ok {
		return number
	}
	return fallback
}

func optionalInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		number, err := strconv.Atoi(typed.String())
		return number, err == nil
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(typed))
		return number, err == nil
	default:
		return 0, false
	}
}

func stringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

// VideoCapabilities extracts known fields from a catalog entry's
// videoGeneration block. Missing keys are not invented.
func VideoCapabilities(model map[string]any) map[string]any {
	raw, _ := model["videoGeneration"].(map[string]any)
	if raw == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for _, key := range []string{"defaultResolution", "defaultAspectRatio", "audioMode"} {
		if value := nonEmpty(raw[key]); value != "" {
			out[key] = value
		}
	}
	for _, key := range []string{"defaultSeconds", "minSeconds", "maxSeconds", "maxPromptLength",
		"maxReferenceImages", "maxReferenceVideos", "maxReferenceAudios", "maxReferenceMaterials"} {
		if value, ok := optionalInt(raw[key]); ok {
			out[key] = value
		}
	}
	for _, key := range []string{"allowMixedReferenceMaterials", "framesExclusiveWithReferenceMaterials",
		"supportsFirstFrame", "supportsFirstLastFrame", "lastFrameRequiresFirstFrame",
		"referenceAudioRequiresVisualMaterial", "defaultEnableAudio"} {
		if value, ok := raw[key].(bool); ok {
			out[key] = value
		}
	}
	if values := stringList(raw["resolutions"]); len(values) > 0 {
		out["resolutions"] = values
	}
	if values := stringList(raw["aspectRatios"]); len(values) > 0 {
		out["aspectRatios"] = values
	}
	return out
}

// SortMediaModels keeps upstream order but is provided for tests that want a
// deterministic panel listing.
func SortMediaModels(models []map[string]any) {
	sort.SliceStable(models, func(i, j int) bool {
		return stringValue(models[i]["id"]) < stringValue(models[j]["id"])
	})
}
