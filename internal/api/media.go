package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/media"
)

const mediaMaxBodyBytes = 24 << 20

// MediaHandlers mounts the v1 image/video API. Media calls never consume chat
// concurrency slots and never apply chat model aliases.
type MediaHandlers struct {
	Service *media.Service
	server  *Server
}

// NewMediaHandlers builds the media route group.
func NewMediaHandlers(service *media.Service, server *Server) *MediaHandlers {
	return &MediaHandlers{Service: service, server: server}
}

// Register mounts exact v1 aliases plus the task/content routes.
func (h *MediaHandlers) Register(mux *http.ServeMux) {
	if h == nil || mux == nil || h.Service == nil {
		return
	}
	server := h.server
	gate := func(next http.HandlerFunc) http.Handler {
		if server == nil {
			return next
		}
		return server.apiGate()(next)
	}
	for _, path := range []string{"/v1/images/generations", "/images/generations"} {
		mux.Handle(path, gate(h.handleImage(false)))
	}
	for _, path := range []string{"/v1/images/edits", "/images/edits"} {
		mux.Handle(path, gate(h.handleImage(true)))
	}
	for _, path := range []string{"/v1/videos", "/videos"} {
		mux.Handle(path, gate(h.handleVideoSubmit))
	}
	for _, path := range []string{"/v1/media/models", "/media/models"} {
		mux.Handle(path, gate(h.handleModels))
	}
	mux.Handle("/v1/media/tasks", gate(h.handleTasks))
	// The content route authenticates by signed token OR API key, so it must
	// not sit behind the generic gate.
	mux.HandleFunc("/v1/videos/", h.handleVideoPath)
}

func (h *MediaHandlers) handleImage(edit bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		payload, failure := readMediaPayload(w, r, h.server.mediaPayloadLimit())
		if failure != nil {
			writeFailure(w, failure)
			return
		}
		keyID := h.server.requestKeyID(r)
		bound := h.server.requestBoundRealm(r)
		request := media.ImageRequest{
			KeyID: keyID, BoundRealm: bound,
			Model: stringPayload(payload, "model"), Prompt: stringPayload(payload, "prompt"),
			Size: stringPayload(payload, "size"), ResponseFormat: stringPayload(payload, "response_format"),
		}
		if n, ok := payload["n"]; ok && n != nil {
			if text, isString := n.(string); isString && strings.TrimSpace(text) == "" {
				request.N = 1
			} else {
				number, ok := wholePayloadInt(n)
				if !ok {
					writeV1Error(w, http.StatusBadRequest, "invalid_request_error", "n 必须是整数 1")
					return
				}
				request.N = number
			}
		}
		if edit {
			request.Images = mediaImages(payload["image"])
		}
		if allowed, wait := h.Service.AllowImage(keyID); !allowed {
			writeV1ErrorWithRetry(w, http.StatusTooManyRequests, "rate_limit_error",
				"媒体请求太频繁：每 Key 每分钟的生图上限已用尽，请 "+strconv.Itoa(wait)+" 秒后再试",
				timeSeconds(wait), nowPlusSeconds(wait))
			return
		}
		var response media.ImageResponse
		var err error
		if edit {
			response, err = h.Service.EditImage(r.Context(), request)
		} else {
			response, err = h.Service.GenerateImage(r.Context(), request)
		}
		if err != nil {
			h.writeMediaError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, response)
	}
}

func (h *MediaHandlers) handleVideoSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	payload, failure := readMediaPayload(w, r, h.server.mediaPayloadLimit())
	if failure != nil {
		writeFailure(w, failure)
		return
	}
	submission, err := h.Service.SubmitVideo(r.Context(), media.VideoRequest{
		KeyID: h.server.requestKeyID(r), BoundRealm: h.server.requestBoundRealm(r), Payload: payload,
	})
	if err != nil {
		h.writeMediaError(w, err)
		return
	}
	public := taskPublic(h.Service, submission.Task)
	if submission.Queued {
		public["queue_position"] = submission.QueuePosition
		public["hint"] = "已进入队列，未提交上游、未扣积分；轮询 GET /v1/videos/" + submission.Task.ID + " 查看进度"
	}
	if waitRequested(r, payload) {
		done, _ := h.Service.Tasks.Wait(submission.Task.ID, 600*1e9)
		public = taskPublic(h.Service, done)
	}
	writeJSONResponse(w, http.StatusAccepted, public)
}

func (h *MediaHandlers) handleVideoPath(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
	if path == "" {
		writeV1Error(w, http.StatusNotFound, "invalid_request_error", "not found")
		return
	}
	if strings.HasSuffix(path, "/cancel") {
		if r.Method != http.MethodPost {
			writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		// Cancel is an API-key route; use the standard gate semantics by
		// resolving the key directly (the mux has no nested middleware).
		if !h.server.contentAuthorized(r) {
			writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "missing or invalid API key")
			return
		}
		taskID := strings.TrimSuffix(path, "/cancel")
		h.handleCancel(w, r, taskID)
		return
	}
	if strings.HasSuffix(path, "/content") {
		taskID := strings.TrimSuffix(path, "/content")
		h.handleContent(w, r, taskID)
		return
	}
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !h.server.contentAuthorized(r) {
		writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "missing or invalid API key")
		return
	}
	taskID := strings.Split(strings.Trim(path, "/"), "/")[0]
	task, ok := h.Service.Task(taskID)
	if !ok || !h.visible(r, task) {
		writeV1Error(w, http.StatusNotFound, "invalid_request_error", "no such task: "+taskID)
		return
	}
	writeJSONResponse(w, http.StatusOK, taskPublic(h.Service, task))
}

func (h *MediaHandlers) handleCancel(w http.ResponseWriter, r *http.Request, taskID string) {
	task, ok := h.Service.Task(taskID)
	if !ok || !h.visible(r, task) {
		writeV1Error(w, http.StatusNotFound, "invalid_request_error", "no such task: "+taskID)
		return
	}
	if _, err := h.Service.Cancel(taskID); err != nil || !cancelable(task.Status) {
		writeV1Error(w, http.StatusConflict, "invalid_request_error",
			"只能取消还在排队的任务（当前 "+task.Status+"）。上游没有取消接口，已提交的任务会照常跑完并计费。")
		return
	}
	updated, _ := h.Service.Task(taskID)
	writeJSONResponse(w, http.StatusOK, taskPublic(h.Service, updated))
}

func (h *MediaHandlers) handleContent(w http.ResponseWriter, r *http.Request, taskID string) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	index := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("i")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			index = parsed
		}
	}
	token := strings.TrimSpace(r.URL.Query().Get("t"))
	if !h.Service.Verify(taskID, index, token) {
		if !h.server.contentAuthorized(r) {
			writeV1Error(w, http.StatusUnauthorized, "authentication_error", "missing or invalid download token")
			return
		}
		task, ok := h.Service.Task(taskID)
		if !ok || !h.visible(r, task) {
			writeV1Error(w, http.StatusNotFound, "invalid_request_error", "no content for this task")
			return
		}
	}
	path, ok := h.Service.ContentPath(taskID, index)
	if !ok {
		writeV1Error(w, http.StatusNotFound, "invalid_request_error", "no content for this task (yet)")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "cannot read artifact")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "cannot read artifact")
		return
	}
	filename := safeDownloadName(taskID) + "-" + strconv.Itoa(index) + ".mp4"
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

func (h *MediaHandlers) handleTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	tasks, stats := h.Service.TasksList(limit)
	rows := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		if !h.visible(r, task) {
			continue
		}
		rows = append(rows, taskPublic(h.Service, task))
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"tasks": rows, "stats": stats})
}

func (h *MediaHandlers) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeV1Error(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	realm := strings.TrimSpace(r.URL.Query().Get("realm"))
	if realm == "" {
		realm = h.Service.Config().Realm
	}
	body, err := h.Service.Models(r.Context(), realm, strings.TrimSpace(r.URL.Query().Get("tag")))
	if err != nil {
		h.writeMediaError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, body)
}

func (h *MediaHandlers) visible(r *http.Request, task media.Task) bool {
	if h == nil || h.server == nil || h.server.isPanelRequest(r) {
		return true
	}
	keyID := h.server.requestKeyID(r)
	return keyID != "" && task.Key == keyID
}

func (h *MediaHandlers) writeMediaError(w http.ResponseWriter, err error) {
	var mediaErr *media.Error
	if !errors.As(err, &mediaErr) || mediaErr == nil {
		writeV1Error(w, http.StatusBadGateway, "server_error", err.Error())
		return
	}
	status, errorType := http.StatusBadGateway, "server_error"
	message := mediaErr.Message
	switch mediaErr.Code {
	case "invalid_request", "no_media_model", "rejected":
		status, errorType = http.StatusBadRequest, "invalid_request_error"
		message += "（本次未扣积分，改完参数可立即重试）"
	case "no_route":
		status, errorType = http.StatusNotFound, "invalid_request_error"
	case "no_credit":
		status, errorType = http.StatusPaymentRequired, "insufficient_quota"
	case "rate_limit", "busy", "queue_full":
		status, errorType = http.StatusTooManyRequests, "rate_limit_error"
	case "auth", "upstream", "empty_result", "task_lost":
		status, errorType = http.StatusBadGateway, "server_error"
	}
	writeV1Error(w, status, errorType, message)
}

func readMediaPayload(w http.ResponseWriter, r *http.Request, limit int64) (map[string]any, *apiFailure) {
	defer func() { _ = r.Body.Close() }()
	body := http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(body)
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, localFailure(http.StatusRequestEntityTooLarge, "request body too large")
		}
		return nil, localFailure(http.StatusBadRequest, "invalid JSON body")
	}
	if payload == nil {
		return nil, localFailure(http.StatusBadRequest, "expected a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, localFailure(http.StatusBadRequest, "invalid JSON body")
	}
	return payload, nil
}

func stringPayload(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func wholePayloadInt(value any) (int, bool) {
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

func mediaImages(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
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

func waitRequested(r *http.Request, payload map[string]any) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("wait")))
	if value == "" {
		if text, ok := payload["wait"].(string); ok {
			value = strings.ToLower(strings.TrimSpace(text))
		} else if flag, ok := payload["wait"].(bool); ok && flag {
			value = "true"
		}
	}
	return value == "1" || value == "true" || value == "yes"
}

func taskPublic(service *media.Service, task media.Task) map[string]any {
	out := map[string]any{
		"id": task.ID, "object": "video", "status": media.StatusOut(task.Status),
		"model": task.Model, "created_at": task.CreatedAt,
	}
	if task.CompletedAt != nil {
		out["completed_at"] = *task.CompletedAt
	}
	if task.Seconds != nil {
		out["seconds"] = "5"
		switch typed := task.Seconds.(type) {
		case int:
			out["seconds"] = strconv.Itoa(typed)
		case int64:
			out["seconds"] = strconv.FormatInt(typed, 10)
		case float64:
			out["seconds"] = strconv.Itoa(int(typed))
		case string:
			out["seconds"] = typed
		}
	}
	if task.Resolution != nil {
		out["resolution"] = task.Resolution
	}
	if task.Credit != nil {
		out["credit"] = *task.Credit
	}
	if code := media.ErrorCodeFor(task.Status); code != "" {
		out["error"] = map[string]any{"code": code, "message": task.Error}
	}
	if len(task.Videos) > 0 {
		out["url"] = "/v1/videos/" + task.ID + "/content?i=0&t=" + service.Sign(task.ID, 0)
	}
	return out
}

func cancelable(status string) bool {
	return status == media.TaskWaiting || status == media.TaskQueued
}

func safeDownloadName(value string) string {
	var builder strings.Builder
	for _, char := range strings.TrimSpace(value) {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '_', char == '-':
			builder.WriteRune(char)
		default:
			builder.WriteByte('-')
		}
		if builder.Len() >= 48 {
			break
		}
	}
	if builder.Len() == 0 {
		return "video"
	}
	return builder.String()
}

func (s *Server) mediaPayloadLimit() int64 {
	limit := int64(mediaMaxBodyBytes)
	if s == nil || s.settings == nil || s.settings.Store == nil {
		return limit
	}
	data, err := s.settings.Store.Load()
	if err != nil {
		return limit
	}
	configured := 0
	switch typed := data["max_payload_mb"].(type) {
	case float64:
		configured = int(typed)
	case int:
		configured = typed
	case json.Number:
		value, _ := typed.Int64()
		configured = int(value)
	}
	if configured > 0 {
		if generic := int64(configured) << 20; generic > limit {
			limit = generic
		}
	}
	return limit
}

func (s *Server) requestKeyID(r *http.Request) string {
	if entry := s.requestKeyEntry(r); entry != nil {
		if strings.TrimSpace(entry.ID) != "" {
			return entry.ID
		}
		return entry.Key
	}
	return ""
}

func (s *Server) requestKeyEntry(r *http.Request) *keys.Entry {
	if entry := KeyEntryFromContext(r); entry != nil {
		return entry
	}
	if s == nil || s.keyStore == nil || r == nil {
		return nil
	}
	entries, err := s.keyStore.List(false)
	if err != nil {
		return nil
	}
	if entry, ok := keys.Match(entries, apiKeyFromRequest(r)); ok {
		return entry
	}
	return nil
}

func (s *Server) requestBoundRealm(r *http.Request) string {
	if entry := s.requestKeyEntry(r); entry != nil {
		return keys.BoundRealm(entry)
	}
	if s != nil && s.opts.APIKeyRealm != nil {
		return strings.ToLower(strings.TrimSpace(s.opts.APIKeyRealm(r)))
	}
	return ""
}

func (s *Server) isPanelRequest(r *http.Request) bool {
	if s == nil || r == nil {
		return false
	}
	if s.sessions != nil && s.sessions.Valid(panelSessionToken(r)) {
		return true
	}
	// A statically configured panel middleware cannot be inspected; only a
	// session proves panel identity here. API-key callers stay key-scoped.
	return s.sessions == nil && s.keyStore == nil && s.requestKeyEntry(r) == nil
}

func (s *Server) contentAuthorized(r *http.Request) bool {
	if s == nil || r == nil {
		return false
	}
	if s.isPanelRequest(r) {
		return true
	}
	if supplied := strings.TrimSpace(apiKeyFromRequest(r)); supplied == "" {
		return false
	}
	if s.keyStore != nil {
		return s.requestKeyEntry(r) != nil || !s.keyStore.AuthRequired()
	}
	if s.opts.APIKeyAuth != nil {
		return s.staticAPIKeyPasses(r)
	}
	return false
}

func nowPlusSeconds(seconds int) time.Time {
	return time.Now().Add(time.Duration(seconds) * time.Second)
}

func timeSeconds(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}
