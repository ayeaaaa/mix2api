package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const maxChatBodyBytes = 16 << 20

// chatHTTPRequest is the parsed gateway view of one /v1/chat/completions call.
type chatHTTPRequest struct {
	payload    map[string]json.RawMessage
	model      string
	messages   []json.RawMessage
	stream     bool
	realm      string
	sessionKey string
	headers    http.Header
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeFailure(w, localFailure(http.StatusMethodNotAllowed, "method not allowed"))
		return
	}
	s.HandleChat(w, r, nil, "")
}

// HandleChat serves one chat completion. The normal router passes a nil
// provider and empty model so the request is resolved by realm or model. Tests
// and future embedders may pass an explicit provider/model pair; the model is
// still validated against that provider's Models catalog.
func (s *Server) HandleChat(w http.ResponseWriter, r *http.Request, provider providers.Provider, model string) {
	request, failure := parseChatHTTPRequest(w, r)
	if failure != nil {
		writeFailure(w, failure)
		return
	}

	override := strings.TrimSpace(model)
	if override != "" {
		request.model = override
		encoded, err := json.Marshal(override)
		if err != nil {
			writeFailure(w, localFailure(http.StatusBadRequest, "model is required"))
			return
		}
		request.payload["model"] = encoded
	}
	if strings.TrimSpace(request.model) == "" {
		writeFailure(w, localFailure(http.StatusBadRequest, "model is required"))
		return
	}

	if provider == nil {
		resolved, err := s.resolveChatProvider(r.Context(), request)
		if err != nil {
			writeFailure(w, err)
			return
		}
		provider = resolved
	} else if err := s.validateModel(r.Context(), provider, request.model); err != nil {
		writeFailure(w, err)
		return
	}

	chatRequest := providers.ChatRequest{
		Raw:        request.payload,
		Headers:    request.headers,
		Model:      request.model,
		Messages:   request.messages,
		SessionKey: request.sessionKey,
	}
	stream, err := provider.OpenChat(r.Context(), chatRequest)
	if err != nil {
		if stream != nil {
			_ = stream.Close()
		}
		writeFailure(w, err)
		return
	}
	if stream == nil {
		writeFailure(w, localFailure(http.StatusBadGateway, "upstream returned no chat stream"))
		return
	}
	defer func() { _ = stream.Close() }()

	if request.stream {
		writeChatStream(w, r, stream)
		return
	}
	writeChatCompletion(w, stream, request.model)
}

func parseChatHTTPRequest(w http.ResponseWriter, r *http.Request) (*chatHTTPRequest, *apiFailure) {
	if r == nil || r.Body == nil {
		return nil, localFailure(http.StatusBadRequest, "invalid JSON body")
	}
	limitReader := http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	decoder := json.NewDecoder(limitReader)
	decoder.UseNumber()

	var payload map[string]json.RawMessage
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

	model, ok := decodeJSONString(payload["model"])
	if !ok || model == "" {
		return nil, localFailure(http.StatusBadRequest, "model is required")
	}

	rawMessages, hasMessages := payload["messages"]
	var messages []json.RawMessage
	if !hasMessages || json.Unmarshal(rawMessages, &messages) != nil || len(messages) == 0 {
		return nil, localFailure(http.StatusBadRequest, "messages is empty")
	}

	stream := false
	if raw, ok := payload["stream"]; ok {
		var value bool
		if json.Unmarshal(raw, &value) == nil {
			stream = value
		}
	}

	// Realm precedence mirrors v1: X-Realm first, then ?realm, then the body
	// extension. realm is a gateway extension; never forward it upstream.
	realm := strings.TrimSpace(r.Header.Get("X-Realm"))
	if realm == "" {
		realm = strings.TrimSpace(r.URL.Query().Get("realm"))
	}
	if realm == "" {
		if bodyRealm, ok := decodeJSONString(payload["realm"]); ok {
			realm = bodyRealm
		}
	}
	delete(payload, "realm")

	return &chatHTTPRequest{
		payload:    payload,
		model:      model,
		messages:   messages,
		stream:     stream,
		realm:      realm,
		sessionKey: chatSessionKey(r, payload),
		headers:    r.Header.Clone(),
	}, nil
}

func decodeJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return strings.TrimSpace(value), true
}

func chatSessionKey(r *http.Request, payload map[string]json.RawMessage) string {
	if r != nil {
		for _, name := range []string{
			"X-Conversation-Id", "Conversation-Id", "X-Session-Id", "Session-Id",
		} {
			if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
				return value
			}
		}
	}
	for _, key := range []string{"conversation_id", "session_id"} {
		if value, ok := decodeJSONString(payload[key]); ok && value != "" {
			return value
		}
	}
	if raw, ok := payload["metadata"]; ok {
		var metadata map[string]json.RawMessage
		if json.Unmarshal(raw, &metadata) == nil {
			if value, ok := decodeJSONString(metadata["conversation_id"]); ok && value != "" {
				return value
			}
		}
	}
	return ""
}

// resolveChatProvider applies the documented resolution order: an explicit
// realm (X-Realm, then query, then body), a unique model match across
// registered providers, then the only registered provider. Model validation
// happens in the same pass so a request never reaches a provider with an
// unknown model.
func (s *Server) resolveChatProvider(ctx context.Context, request *chatHTTPRequest) (providers.Provider, error) {
	if realm := strings.TrimSpace(request.realm); realm != "" {
		if strings.EqualFold(realm, "all") {
			candidates := s.workBuddyProviders()
			if len(candidates) == 0 {
				return nil, localFailure(http.StatusBadRequest,
					"realm=all is only available for WorkBuddy (intl/cn); no WorkBuddy realm is registered")
			}
			return s.resolveAmong(ctx, candidates, request.model, "all")
		}
		provider := s.providerForRealm(realm)
		if provider == nil {
			return nil, localFailure(http.StatusNotFound, "unknown realm: "+realm)
		}
		if err := s.validateModel(ctx, provider, request.model); err != nil {
			return nil, err
		}
		return provider, nil
	}

	if len(s.providers) == 0 {
		return nil, localFailure(http.StatusBadRequest, "no provider is registered")
	}
	if len(s.providers) == 1 {
		provider := s.providers[0]
		if err := s.validateModel(ctx, provider, request.model); err != nil {
			return nil, err
		}
		return provider, nil
	}
	return s.resolveAmong(ctx, s.providers, request.model, "")
}

func (s *Server) resolveAmong(ctx context.Context, candidates []providers.Provider, model, realm string) (providers.Provider, error) {
	var firstErr error
	successes := 0
	for _, provider := range candidates {
		models, err := provider.Models(ctx, false)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		successes++
		if modelMatches(models, model) {
			return provider, nil
		}
	}
	if successes == 0 && firstErr != nil {
		return nil, firstErr
	}
	if realm == "all" {
		return nil, localFailure(http.StatusNotFound,
			"model not found in WorkBuddy realms intl/cn: "+model)
	}
	return nil, localFailure(http.StatusNotFound, "model not found: "+model)
}

func (s *Server) validateModel(ctx context.Context, provider providers.Provider, model string) error {
	models, err := provider.Models(ctx, false)
	if err != nil {
		return err
	}
	if !modelMatches(models, model) {
		return localFailure(http.StatusNotFound, "model not found: "+model)
	}
	return nil
}

func modelMatches(models []providers.Model, model string) bool {
	want := strings.ToLower(strings.TrimSpace(model))
	if want == "" {
		return false
	}
	for _, item := range models {
		id, _ := item["id"].(string)
		if strings.ToLower(strings.TrimSpace(id)) == want {
			return true
		}
	}
	return false
}

func (s *Server) providerForRealm(realm string) providers.Provider {
	realm = strings.TrimSpace(realm)
	for _, provider := range s.providers {
		if strings.EqualFold(providerRealm(provider), realm) {
			return provider
		}
	}
	return nil
}

func (s *Server) workBuddyProviders() []providers.Provider {
	candidates := make([]providers.Provider, 0, 2)
	for _, realm := range []string{"intl", "cn"} {
		if provider := s.providerForRealm(realm); provider != nil {
			candidates = append(candidates, provider)
		}
	}
	return candidates
}

// writeChatStream passes provider frames through unchanged. SSE headers are
// written only after the first successful frame, and a midstream failure turns
// into a terminal readable SSE error frame instead of a silent truncation.
func writeChatStream(w http.ResponseWriter, r *http.Request, stream providers.ChatStream) {
	flusher, _ := w.(http.Flusher)
	wroteHeaders := false
	wroteDone := false

	for {
		frame, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !wroteHeaders {
					writeFailure(w, localFailure(http.StatusBadGateway, "upstream returned an empty stream"))
					return
				}
				if !wroteDone {
					writeSSEFrame(w, flusher, []byte("data: [DONE]\n\n"))
				}
				return
			}
			if r != nil && r.Context().Err() != nil {
				return
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			if !wroteHeaders {
				writeFailure(w, err)
				return
			}
			writeSSEError(w, flusher, err)
			return
		}
		if len(frame) == 0 {
			continue
		}
		if !wroteHeaders {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			wroteHeaders = true
		}
		if _, err := w.Write(frame); err != nil {
			return
		}
		flush(w, flusher)
		if sseFrameDone(frame) {
			wroteDone = true
			return
		}
	}
}

func writeChatCompletion(w http.ResponseWriter, stream providers.ChatStream, model string) {
	completion, err := aggregateChatStream(stream, model)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, completion)
}

func aggregateChatStream(stream providers.ChatStream, model string) (map[string]any, error) {
	if stream == nil {
		return nil, localFailure(http.StatusBadGateway, "upstream returned no chat stream")
	}
	aggregate := &chatAggregate{tools: make(map[int]*chatToolCall)}
	for {
		frame, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		data := ssePayload(frame)
		if data == "" {
			continue
		}
		if strings.TrimSpace(data) == "[DONE]" {
			break
		}
		aggregate.addJSON([]byte(data))
	}
	completion, ok := aggregate.completion(model)
	if !ok {
		return nil, localFailure(http.StatusBadGateway, "upstream returned no chat completion")
	}
	return completion, nil
}

func writeSSEFrame(w http.ResponseWriter, flusher http.Flusher, frame []byte) {
	if len(frame) > 0 {
		if _, err := w.Write(frame); err != nil {
			return
		}
		flush(w, flusher)
	}
}

func writeSSEError(w http.ResponseWriter, flusher http.Flusher, err error) {
	failure := providerFailure(err)
	message := failure.Message
	if message == "" {
		message = "upstream stream error"
	}
	payload, encodeErr := marshalJSON(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"param":   nil,
			"code":    "upstream_stream_error",
		},
	})
	if encodeErr != nil {
		payload = []byte(`{"error":{"message":"upstream stream error","type":"upstream_error","code":"upstream_stream_error"}}`)
	}
	frame := make([]byte, 0, len(payload)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	writeSSEFrame(w, flusher, frame)
}

func flush(w http.ResponseWriter, flusher http.Flusher) {
	if flusher != nil {
		flusher.Flush()
	}
}

func sseFrameDone(frame []byte) bool {
	return strings.TrimSpace(ssePayload(frame)) == "[DONE]"
}

// ssePayload joins the data lines of one complete SSE frame. Non-data fields
// and comments are ignored, matching the v1 data-line pass-through.
func ssePayload(frame []byte) string {
	var lines []string
	for _, rawLine := range bytes.Split(frame, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		if strings.HasPrefix(data, " ") {
			data = data[1:]
		}
		lines = append(lines, data)
	}
	if len(lines) == 0 {
		trimmed := strings.TrimSpace(string(frame))
		if trimmed == "[DONE]" {
			return trimmed
		}
		return ""
	}
	return strings.Join(lines, "\n")
}

// chatAggregate folds OpenAI-style SSE chunks into one chat.completion. It
// intentionally understands only the generic wire shape; no Raccoon fields.
type chatAggregate struct {
	id          string
	model       string
	content     []string
	reasoning   []string
	finish      string
	tools       map[int]*chatToolCall
	sawChoice   bool
	usage       map[string]any
	lastCreated int64
}

type chatToolCall struct {
	id        string
	kind      string
	name      string
	arguments string
}

func (a *chatAggregate) addJSON(data []byte) {
	if a == nil {
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var chunk map[string]any
	if err := decoder.Decode(&chunk); err != nil || chunk == nil {
		return
	}

	if id, ok := chunk["id"].(string); ok && strings.TrimSpace(id) != "" {
		a.id = id
	}
	if model, ok := chunk["model"].(string); ok && strings.TrimSpace(model) != "" {
		a.model = model
	}
	if created, ok := int64Value(chunk["created"]); ok && created > 0 && a.lastCreated == 0 {
		a.lastCreated = created
	}
	if usage, ok := chunk["usage"].(map[string]any); ok && len(usage) > 0 {
		a.usage = usage
	}

	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	a.sawChoice = true
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta, _ = choice["message"].(map[string]any)
		}
		if delta != nil {
			if piece, ok := delta["content"].(string); ok && piece != "" {
				a.content = append(a.content, piece)
			}
			if piece, ok := delta["reasoning_content"].(string); ok && piece != "" {
				a.reasoning = append(a.reasoning, piece)
			}
			a.addToolCalls(delta["tool_calls"])
			if functionCall, ok := delta["function_call"].(map[string]any); ok {
				a.addLegacyFunctionCall(functionCall)
			}
		}
		if finish, ok := choice["finish_reason"].(string); ok && strings.TrimSpace(finish) != "" {
			a.finish = finish
		}
	}
}

func (a *chatAggregate) addToolCalls(value any) {
	calls, ok := value.([]any)
	if !ok {
		return
	}
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		index, ok := intValue(call["index"])
		if !ok {
			index = len(a.tools)
		}
		entry := a.tools[index]
		if entry == nil {
			entry = &chatToolCall{}
			a.tools[index] = entry
		}
		if id, ok := call["id"].(string); ok && id != "" {
			entry.id = id
		}
		if kind, ok := call["type"].(string); ok && kind != "" {
			entry.kind = kind
		}
		if function, ok := call["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok && name != "" {
				entry.name += name
			}
			if arguments, ok := function["arguments"].(string); ok && arguments != "" {
				entry.arguments += arguments
			}
		}
	}
}

func (a *chatAggregate) addLegacyFunctionCall(function map[string]any) {
	name, _ := function["name"].(string)
	if strings.TrimSpace(name) == "" {
		return
	}
	entry := a.tools[0]
	if entry == nil {
		entry = &chatToolCall{}
		a.tools[0] = entry
	}
	if entry.name == "" {
		entry.name = name
	}
	if arguments, ok := function["arguments"].(string); ok {
		entry.arguments += arguments
	}
}

func (a *chatAggregate) completion(requestedModel string) (map[string]any, bool) {
	if a == nil {
		return nil, false
	}
	if !a.sawChoice && len(a.tools) == 0 && len(a.content) == 0 && len(a.reasoning) == 0 {
		return nil, false
	}

	message := map[string]any{
		"role":    "assistant",
		"content": strings.Join(a.content, ""),
	}
	if reasoning := strings.Join(a.reasoning, ""); reasoning != "" {
		message["reasoning_content"] = reasoning
	}

	finish := a.finish
	if finish == "" {
		finish = "stop"
	}
	if len(a.tools) > 0 {
		indexes := make([]int, 0, len(a.tools))
		for index := range a.tools {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		calls := make([]any, 0, len(indexes))
		for _, index := range indexes {
			call := a.tools[index]
			if call == nil || strings.TrimSpace(call.name) == "" {
				continue
			}
			id := call.id
			if id == "" {
				id = randomChatID("call_")
			}
			kind := call.kind
			if kind == "" {
				kind = "function"
			}
			calls = append(calls, map[string]any{
				"id":   id,
				"type": kind,
				"function": map[string]any{
					"name":      call.name,
					"arguments": call.arguments,
				},
			})
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
			if finish == "stop" {
				finish = "tool_calls"
			}
		} else if finish == "tool_calls" {
			finish = "stop"
		}
	} else if finish == "tool_calls" {
		finish = "stop"
	}

	created := a.lastCreated
	if created == 0 {
		created = time.Now().Unix()
	}
	id := a.id
	if id == "" {
		id = randomChatID("chatcmpl-")
	}
	model := a.model
	if model == "" {
		model = requestedModel
	}

	completion := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
	}
	if a.usage != nil {
		completion["usage"] = a.usage
	}
	return completion, true
}

func intValue(value any) (int, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(number), true
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, false
		}
		return number, true
	default:
		return 0, false
	}
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return number, true
	case float64:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}

func randomChatID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return prefix + hex.EncodeToString(raw[:])
}
