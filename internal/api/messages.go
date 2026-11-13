package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api/protocol"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const anthropicDisabledMessage = "Anthropic Messages is disabled by default: this gateway " +
	"translates requests to Chat Completions and ignores " +
	"cache_control. This can cause excessive credit consumption. " +
	"Enable it in the Settings panel or set WB_ENABLE_ANTHROPIC_MESSAGES=1 " +
	"to opt in at your own risk (see issue #367)."

// messagesToChat translates one Anthropic Messages request body into the Chat
// Completions body the generic provider contract understands.
func messagesToChat(payload map[string]any) (map[string]any, error) {
	return messagesToChatWithDefault(payload, nil)
}

func messagesToChatWithDefault(payload map[string]any, defaultMaxOutput func(string) int) (map[string]any, error) {
	if payload == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	model := strings.TrimSpace(anthropicText(payload["model"]))
	if model == "" {
		return nil, errors.New("model is required")
	}
	messagesIn, ok := payload["messages"].([]any)
	if !ok {
		return nil, errors.New("messages must be a list")
	}
	if len(messagesIn) == 0 {
		return nil, errors.New("messages must not be empty")
	}

	chat := map[string]any{"model": model, "messages": []any{}}
	for _, key := range []string{"temperature", "top_p"} {
		if value := payload[key]; value != nil {
			chat[key] = value
		}
	}
	if value := payload["max_tokens"]; value != nil {
		chat["max_tokens"] = value
	} else if defaultMaxOutput != nil {
		if limit := defaultMaxOutput(model); limit > 0 {
			chat["max_tokens"] = limit
		}
	}
	if value, ok := payload["stream"]; ok {
		chat["stream"] = boolValue(value)
	}
	if stops, ok := payload["stop_sequences"].([]any); ok && len(stops) > 0 {
		values := make([]any, 0, len(stops))
		for _, stop := range stops {
			if text, ok := stop.(string); ok && text != "" {
				values = append(values, text)
			}
		}
		if len(values) > 0 {
			chat["stop"] = values
		}
	}
	if metadata, ok := payload["metadata"].(map[string]any); ok && metadata["user_id"] != nil {
		chat["user"] = metadata["user_id"]
	}

	tools, skipped := anthropicToolsToChat(payload["tools"])
	if len(tools) > 0 {
		chat["tools"] = tools
	}
	if choice := anthropicToolChoice(payload["tool_choice"]); choice != nil {
		chat["tool_choice"] = choice
	}
	if rawChoice, ok := payload["tool_choice"].(map[string]any); ok && rawChoice["disable_parallel_tool_use"] == true {
		chat["parallel_tool_calls"] = false
	}
	if effort := anthropicEffortFromMessages(payload); effort != "" {
		chat["reasoning_effort"] = effort
	}

	messages := []any{}
	system := anthropicSystemText(payload["system"])
	if len(skipped) > 0 {
		note := "These Anthropic server tools are not available here: " +
			strings.Join(skipped, ", ") + ". Do not call them."
		if system != "" {
			system += "\n" + note
		} else {
			system = note
		}
	}
	if system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}

	for index, rawItem := range messagesIn {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(anthropicText(item["role"]))
		if role == "" {
			role = "user"
		}
		content := item["content"]
		if role == "system" || role == "developer" {
			text := anthropicSystemText(content)
			if text == "" {
				text = anthropicContentToText(content, nil)
			}
			if text != "" {
				messages = append(messages, map[string]any{"role": "system", "content": text})
			}
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, errors.New("messages[" + strconv.Itoa(index) + "].role '" +
				role + "' must be user or assistant; " +
				"pass a system prompt in the top-level system field")
		}
		switch typed := content.(type) {
		case string:
			messages = append(messages, map[string]any{"role": role, "content": typed})
		case []any:
			messages = append(messages, anthropicBlocksToMessages(role, typed)...)
		case nil:
			// v1 ignores a nil content block.
		default:
			if text := anthropicText(typed); text != "" {
				messages = append(messages, map[string]any{"role": role, "content": text})
			}
		}
	}
	chat["messages"] = messages
	return chat, nil
}

func anthropicBlocksToMessages(role string, blocks []any) []any {
	toolCalls := []any{}
	toolMessages := []any{}
	contentParts := []any{}
	texts := []string{}
	hasNonText := false

	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "thinking", "redacted_thinking":
			continue
		case "tool_use", "server_tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id":   anthropicText(block["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      anthropicText(block["name"]),
					"arguments": protocol.JSONText(block["input"]),
				},
			})
			continue
		case "tool_result":
			toolMessages = append(toolMessages, map[string]any{
				"role":         "tool",
				"tool_call_id": anthropicText(block["tool_use_id"]),
				"content":      anthropicToolResultContent(block["content"], block["is_error"]),
			})
			continue
		case "image":
			part := anthropicImagePart(block)
			if imageURLPart(part) == "" {
				continue
			}
			hasNonText = true
			contentParts = append(contentParts, part)
			continue
		case "document":
			hasNonText = true
			contentParts = append(contentParts, map[string]any{"type": "text", "text": anthropicDocumentText(block)})
			continue
		default:
			text := anthropicBlockText(block)
			texts = append(texts, text)
			contentParts = append(contentParts, map[string]any{"type": "text", "text": text})
		}
	}

	out := append([]any(nil), toolMessages...)
	if len(toolCalls) == 0 && len(texts) == 0 && !hasNonText {
		return out
	}
	message := map[string]any{"role": role}
	if role == "" {
		message["role"] = "user"
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if hasNonText {
		message["content"] = contentParts
	} else if len(texts) > 0 {
		message["content"] = strings.Join(texts, "")
	} else {
		message["content"] = ""
	}
	return append(out, message)
}

func anthropicText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return ""
	}
}

func anthropicContentToText(value any, isError any) string {
	var text string
	switch typed := value.(type) {
	case string:
		text = typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, rawItem := range typed {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			switch item["type"] {
			case "image":
				part := anthropicImagePart(item)
				parts = append(parts, imageURLPart(part))
			case "document":
				parts = append(parts, anthropicDocumentText(item))
			default:
				parts = append(parts, anthropicBlockText(item))
			}
		}
		text = strings.Join(parts, "")
	case map[string]any:
		text = anthropicBlockText(typed)
	default:
		text = anthropicText(value)
	}
	if isError == true {
		return "error: " + text
	}
	return text
}

func anthropicBlockText(block map[string]any) string {
	if block == nil {
		return ""
	}
	if text, ok := block["text"]; ok {
		return anthropicText(text)
	}
	if content, ok := block["content"]; ok {
		return anthropicContentToText(content, nil)
	}
	return ""
}

func anthropicToolResultContent(value any, isError any) any {
	items, ok := value.([]any)
	if !ok {
		return anthropicContentToText(value, isError)
	}
	parts := []any{}
	hasImage := false
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			if text, ok := rawItem.(string); ok {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			continue
		}
		switch item["type"] {
		case "image":
			part := anthropicImagePart(item)
			if url := imageURLPart(part); url != "" {
				hasImage = true
				parts = append(parts, part)
			} else {
				parts = append(parts, map[string]any{"type": "text", "text": "[image omitted: no data]"})
			}
		case "document":
			parts = append(parts, map[string]any{"type": "text", "text": anthropicDocumentText(item)})
		default:
			if text := anthropicBlockText(item); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
		}
	}
	if !hasImage {
		var builder strings.Builder
		for _, rawPart := range parts {
			if part, ok := rawPart.(map[string]any); ok && part["type"] == "text" {
				builder.WriteString(stringValue(part["text"]))
			}
		}
		text := builder.String()
		if isError == true {
			return "error: " + text
		}
		return text
	}
	if isError == true {
		parts = append([]any{map[string]any{"type": "text", "text": "error:"}}, parts...)
	}
	return parts
}

func anthropicImagePart(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	url := ""
	if source != nil {
		switch anthropicText(source["type"]) {
		case "base64":
			media := anthropicText(source["media_type"])
			if media == "" {
				media = "image/png"
			}
			url = "data:" + media + ";base64," + anthropicText(source["data"])
		case "url":
			url = anthropicText(source["url"])
		case "file":
			url = anthropicText(source["file_id"])
			if url == "" {
				url = anthropicText(source["url"])
			}
		}
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

func imageURLPart(part map[string]any) string {
	image, _ := part["image_url"].(map[string]any)
	return anthropicText(image["url"])
}

func anthropicDocumentText(block map[string]any) string {
	if block == nil {
		return ""
	}
	title := anthropicText(block["title"])
	source, _ := block["source"].(map[string]any)
	body := ""
	if source != nil {
		switch anthropicText(source["type"]) {
		case "text":
			body = anthropicText(source["data"])
		case "content":
			body = anthropicContentToText(source["content"], nil)
		case "base64":
			media := anthropicText(source["media_type"])
			if media == "" {
				media = "application/octet-stream"
			}
			body = "[document " + media + " omitted]"
		case "url":
			body = anthropicText(source["url"])
		case "file":
			body = anthropicText(source["file_id"])
			if body == "" {
				body = anthropicText(source["url"])
			}
		}
	}
	if title != "" {
		return title + "\n" + body
	}
	return body
}

func anthropicSystemText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, rawItem := range typed {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			if text := anthropicText(item["text"]); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func anthropicToolChoice(value any) any {
	switch typed := value.(type) {
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		switch normalized {
		case "auto":
			return "auto"
		case "any":
			return "required"
		case "none":
			return "none"
		case "required":
			return "required"
		default:
			return normalized
		}
	case map[string]any:
		switch strings.ToLower(strings.TrimSpace(anthropicText(typed["type"]))) {
		case "auto":
			return "auto"
		case "any":
			return "required"
		case "none":
			return "none"
		case "tool":
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": anthropicText(typed["name"])},
			}
		}
	}
	return nil
}

func anthropicToolsToChat(value any) ([]any, []string) {
	items, _ := value.([]any)
	out := []any{}
	skipped := []string{}
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := item["function"].(map[string]any); ok {
			out = append(out, item)
			continue
		}
		name := anthropicText(item["name"])
		if name == "" {
			name = anthropicText(item["type"])
		}
		schema := item["input_schema"]
		if schema == nil {
			schema = item["parameters"]
		}
		if schema == nil {
			if name != "" {
				skipped = append(skipped, name)
			}
			continue
		}
		function := map[string]any{"name": name, "parameters": schema}
		if item["description"] != nil {
			function["description"] = item["description"]
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out, skipped
}

func anthropicEffortFromMessages(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if outputConfig, ok := payload["output_config"].(map[string]any); ok {
		if effort := strings.TrimSpace(anthropicText(outputConfig["effort"])); effort != "" {
			return effort
		}
	}
	thinking, ok := payload["thinking"].(map[string]any)
	if !ok {
		return ""
	}
	thinkingType := strings.ToLower(strings.TrimSpace(anthropicText(thinking["type"])))
	if thinkingType == "disabled" {
		return "none"
	}
	if thinkingType != "enabled" {
		return ""
	}
	budget, err := strconv.Atoi(strings.TrimSpace(anthropicText(thinking["budget_tokens"])))
	if err != nil || budget <= 0 {
		return ""
	}
	switch {
	case budget >= 32000:
		return "xhigh"
	case budget >= 16000:
		return "high"
	case budget >= 8000:
		return "medium"
	default:
		return "low"
	}
}

func chatToMessages(chat map[string]any) map[string]any {
	if chat == nil {
		chat = map[string]any{}
	}
	message := map[string]any{}
	finish := ""
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if value, ok := choice["message"].(map[string]any); ok {
				message = value
			}
			finish, _ = choice["finish_reason"].(string)
		}
	}
	content := []any{}
	if reasoning := anthropicText(message["reasoning_content"]); reasoning != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": reasoning})
	}
	if textParts, ok := message["content"].([]any); ok {
		var builder strings.Builder
		for _, rawPart := range textParts {
			if part, ok := rawPart.(map[string]any); ok {
				builder.WriteString(anthropicBlockText(part))
			}
		}
		if text := builder.String(); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
	} else if text := anthropicText(message["content"]); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	if toolCalls, ok := message["tool_calls"].([]any); ok {
		for _, rawCall := range toolCalls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			function, _ := call["function"].(map[string]any)
			id := anthropicText(call["id"])
			if id == "" {
				id = randomChatID("toolu_")
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  anthropicText(function["name"]),
				"input": protocol.ParseToolInput(function["arguments"]),
			})
		}
	}
	return map[string]any{
		"id":            anthropicMessageID(anthropicText(chat["id"])),
		"type":          "message",
		"role":          "assistant",
		"model":         anthropicText(chat["model"]),
		"content":       content,
		"stop_reason":   protocol.AnthropicStopReason(finish),
		"stop_sequence": nil,
		"usage":         protocol.AnthropicUsage(usageMap(chat["usage"])),
	}
}

func anthropicMessageID(value string) string {
	if value == "" {
		return randomChatID("msg_")
	}
	if strings.HasPrefix(value, "msg_") {
		return value
	}
	return "msg_" + value
}

// ----- HTTP -----

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", time.Time{})
		return
	}
	if !s.anthropicMessagesEnabled() {
		failure := &apiFailure{Status: http.StatusBadRequest, Message: anthropicDisabledMessage}
		s.observeMessagesFailureDetail(r, "", false, failure, "")
		writeAnthropicError(w, failure.Status, failure.Message, "invalid_request_error", time.Time{})
		return
	}
	payload, failure := s.parseMessagesPayload(w, r)
	if failure != nil {
		s.observeMessagesFailureDetail(r, "", false, failure, "")
		writeAnthropicError(w, failure.Status, failure.Message, protocol.AnthropicErrorType(failure.Status), time.Time{})
		return
	}
	// Parse the exit-qualified reference before translation so the chat map and
	// the provider payload only carry the upstream model ID. The default
	// max-output lookup therefore sees the same ID the provider does, and the
	// prefix never reaches the provider.
	rawModel := strings.TrimSpace(anthropicText(payload["model"]))
	ref, err := parseModelRef(rawModel, func(exit string) bool {
		return s != nil && s.providerForRealm(exit) != nil
	})
	if err != nil {
		failure := &apiFailure{Status: http.StatusBadRequest, Message: err.Error()}
		s.observeMessagesFailureDetail(r, rawModel, false, failure, "")
		writeAnthropicError(w, failure.Status, failure.Message, "invalid_request_error", time.Time{})
		return
	}
	if ref.Qualified {
		payload["model"] = ref.Upstream
	}
	chat, err := messagesToChatWithDefault(payload, s.defaultMaxOutputTokens)
	if err != nil {
		failure := &apiFailure{Status: http.StatusBadRequest, Message: err.Error()}
		s.observeMessagesFailureDetail(r, ref.Requested(), false, failure, "")
		writeAnthropicError(w, failure.Status, failure.Message, "invalid_request_error", time.Time{})
		return
	}
	s.serveMessagesRequest(w, r, payload, chat, ref)
}

func (s *Server) parseMessagesPayload(w http.ResponseWriter, r *http.Request) (map[string]any, *apiFailure) {
	raw, failure := parseProtocolObject(w, r)
	if failure != nil {
		return nil, failure
	}
	payload, err := decodeRawObject(raw)
	if err != nil {
		return nil, localFailure(http.StatusBadRequest, err.Error())
	}
	return payload, nil
}

// serveMessagesRequest opens the upstream for one translated Messages request.
// ref carries the client spelling, canonical requested ID and parsed routing
// prefix; the chat map's model is the upstream ID. The key allow-list is
// enforced here once, after the reference is parsed, so a qualified request can
// match either its canonical or its upstream spelling while error text keeps
// the client's original spelling.
func (s *Server) serveMessagesRequest(w http.ResponseWriter, r *http.Request, payload, chat map[string]any, ref modelRef) {
	started := time.Now()
	requestedModel := ref.Requested()
	exit := ref.Exit
	upstreamModel := strings.TrimSpace(anthropicText(chat["model"]))
	stream := boolValue(chat["stream"])
	if failure := s.keyModelFailureForRef(r, ref); failure != nil {
		s.observeMessagesFailureDetail(r, requestedModel, stream, failure, "")
		writeAnthropicError(w, failure.Status, failure.Message, "invalid_request_error", time.Time{})
		return
	}
	realm, explicitRealm := realmFromRequest(r)
	if realm == "" {
		if bodyRealm := strings.TrimSpace(anthropicText(payload["realm"])); bodyRealm != "" {
			realm = bodyRealm
			explicitRealm = true
		}
	}
	boundRealm, boundFailure := s.apiKeyBoundRealm(r)
	if boundFailure != nil {
		s.observeMessagesFailureDetail(r, requestedModel, stream, boundFailure, "")
		writeAnthropicError(w, boundFailure.Status, boundFailure.Message, protocol.AnthropicErrorType(boundFailure.Status), time.Time{})
		return
	}

	rawChat, err := chatMapToRaw(chat)
	if err != nil {
		failure := localFailure(http.StatusBadRequest, "invalid request")
		s.observeMessagesFailureDetail(r, requestedModel, stream, failure, realm)
		writeAnthropicError(w, failure.Status, failure.Message, "invalid_request_error", time.Time{})
		return
	}
	request := &chatHTTPRequest{
		payload:        rawChat,
		model:          upstreamModel,
		requestedModel: requestedModel,
		exit:           exit,
		stream:         stream,
		realm:          realm,
		explicitRealm:  explicitRealm,
		sessionKey:     chatSessionKey(r, rawChat),
		headers:        r.Header.Clone(),
	}
	provider, resolveErr := s.resolveChatProvider(r.Context(), request, boundRealm)
	if resolveErr != nil {
		failure := failureFromError(resolveErr)
		failureRealm := boundRealm
		if exit != "" {
			failureRealm = exit
		}
		s.observeMessagesFailureDetail(r, requestedModel, stream, failure, failureRealm)
		writeAnthropicError(w, failure.Status, failure.Message, protocol.AnthropicErrorType(failure.Status), time.Time{})
		return
	}
	var allowRealmFallback *bool
	if exit != "" || explicitRealm || boundRealm != "" {
		disabled := false
		allowRealmFallback = &disabled
	}
	chatRequest := providers.ChatRequest{
		Raw:                rawChat,
		Headers:            r.Header.Clone(),
		Model:              upstreamModel,
		Messages:           chatMessagesRaw(chat),
		SessionKey:         request.sessionKey,
		AllowRealmFallback: allowRealmFallback,
		AllowedAccounts:    keyAllowedAccounts(r),
	}
	// The provider payload and ChatRequest above carry the upstream ID. Output
	// helpers must echo the canonical requested ID instead of an upstream alias.
	chat["model"] = requestedModel
	upstream, openErr := provider.OpenChat(r.Context(), chatRequest)
	if openErr != nil {
		if upstream != nil {
			_ = upstream.Close()
		}
		failure := failureFromError(openErr)
		s.observeMessagesFailureDetail(r, requestedModel, stream, failure, provider.Realm())
		writeAnthropicError(w, failure.Status, failure.Message, protocol.AnthropicErrorType(failure.Status), retryReset(openErr))
		return
	}
	if upstream == nil {
		failure := localFailure(http.StatusBadGateway, "upstream returned no chat stream")
		s.observeMessagesFailureDetail(r, requestedModel, stream, failure, provider.Realm())
		writeAnthropicError(w, failure.Status, failure.Message, "api_error", time.Time{})
		return
	}
	tracked := newUsageTracker(withAttribution(upstream, provider.Realm()), started)
	defer func() { _ = tracked.Close() }()

	if stream {
		s.serveMessagesStream(w, r, tracked, requestedModel, upstreamModel, exit != "", anthropicText(chat["reasoning_effort"]))
		return
	}
	chatObject, aggregateErr := aggregateChatStream(tracked, requestedModel)
	if aggregateErr != nil {
		failure := failureFromError(aggregateErr)
		observation := ProtocolObservation{
			Protocol: "messages", Model: upstreamModel, RequestedModel: requestedModel,
			Realm: provider.Realm(), Effort: anthropicText(chat["reasoning_effort"]),
			Stream: false, Outcome: "failed", Status: failure.Status,
			Error: true, Message: failure.Message,
		}
		applyTracker(&observation, tracked)
		s.observeProtocol(r, observation)
		writeAnthropicError(w, failure.Status, failure.Message, protocol.AnthropicErrorType(failure.Status), retryReset(aggregateErr))
		return
	}
	if exit != "" {
		chatObject["model"] = requestedModel
	}
	result := chatToMessages(chatObject)
	observation := ProtocolObservation{
		Protocol: "messages", Model: upstreamModel, RequestedModel: requestedModel,
		Realm: provider.Realm(), Effort: anthropicText(chat["reasoning_effort"]),
		Stream: false, Outcome: "completed", Status: http.StatusOK,
		Usage: usageMap(chatObject["usage"]),
	}
	applyTracker(&observation, tracked)
	s.observeProtocol(r, observation)
	writeJSONResponse(w, http.StatusOK, result)
}

func msgstatShards() int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("WB_MSGSTAT_SHARDS")))
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func msgstatDailySalt() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WB_MSGSTAT_DAILY_SALT"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// msgstatAffinityKey gives one-shot /v1/messages calls a stable account bucket.
// A real conversation, a streamed call, or a client-supplied session key keeps
// the normal prefix affinity.
func msgstatAffinityKey(r *http.Request, chat map[string]any, model string) string {
	shards := msgstatShards()
	if shards <= 0 || chat == nil || boolValue(chat["stream"]) {
		return ""
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) < 2 || len(msgs) > 3 {
		return ""
	}
	keyID := ""
	if entry := KeyEntryFromContext(r); entry != nil {
		keyID = entry.ID
	}
	system := ""
	if first, ok := msgs[0].(map[string]any); ok {
		system = anthropicContentToText(first["content"], false)
	}
	toolNames := make([]string, 0)
	if tools, ok := chat["tools"].([]any); ok {
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			fn, _ := tool["function"].(map[string]any)
			if fn != nil {
				toolNames = append(toolNames, strings.TrimSpace(anthropicText(fn["name"])))
			}
		}
	}
	sort.Strings(toolNames)
	stableRaw, _ := json.Marshal(map[string]any{
		"k": keyID, "m": model, "s": system, "t": toolNames,
	})
	stable := sha256.Sum256(stableRaw)
	volatileRaw, _ := json.Marshal(msgs[1:])
	volatile := sha256.Sum256(volatileRaw)
	bucket := int(volatile[0])<<8 | int(volatile[1])
	key := "msgstat-" + hex.EncodeToString(stable[:8]) + "-" + strconv.Itoa(bucket%shards)
	if msgstatDailySalt() {
		key += "-" + time.Now().Format("2006-01-02")
	}
	return key
}

func (s *Server) anthropicMessagesEnabled() bool {
	if s != nil && s.opts.AnthropicMessagesEnabled != nil && s.opts.AnthropicMessagesEnabled() {
		return true
	}
	switch strings.ToLower(envValue("WB_ENABLE_ANTHROPIC_MESSAGES")) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		}
	case json.Number:
		number, err := typed.Int64()
		return err == nil && number != 0
	case float64:
		return typed != 0
	}
	return false
}

func (s *Server) observeMessagesFailure(r *http.Request, model string, stream bool, status int, realms ...string) {
	s.observeMessagesFailureDetail(r, model, stream, &apiFailure{Status: status, Message: "request failed"}, firstRealm(realms))
}

func (s *Server) observeMessagesFailureDetail(r *http.Request, model string, stream bool, failure *apiFailure, realm string) {
	status := http.StatusBadGateway
	message := "request failed"
	if failure != nil {
		if failure.Status > 0 {
			status = failure.Status
		}
		if strings.TrimSpace(failure.Message) != "" {
			message = failure.Message
		}
	}
	s.observeProtocol(r, ProtocolObservation{
		Protocol: "messages", Model: model, RequestedModel: model, Realm: realm,
		Stream: stream, Outcome: "failed", Status: status, Error: true, Message: message,
	})
}

func retryReset(err error) time.Time {
	var retry providers.RetryMetadata
	if errors.As(err, &retry) {
		return retry.RetryReset()
	}
	return time.Time{}
}

// writeAnthropicError writes the native Messages error envelope, including the
// absolute reset metadata v1 clients consume on 429.
func writeAnthropicError(w http.ResponseWriter, status int, message, errType string, reset time.Time) {
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	if strings.TrimSpace(errType) == "" {
		errType = protocol.AnthropicErrorType(status)
	}
	validReset := time.Time{}
	if !reset.IsZero() && reset.After(time.Now()) {
		validReset = reset.UTC()
	}
	if status == http.StatusTooManyRequests && !validReset.IsZero() {
		message += " (resets at " + validReset.Format(time.RFC3339) + ")"
	}
	body, err := marshalJSON(protocol.AnthropicError(status, message, errType))
	if err != nil {
		body = []byte(`{"type":"error","error":{"type":"api_error","message":"response encoding failed"}}`)
		status = http.StatusInternalServerError
	}
	if status == http.StatusTooManyRequests && !validReset.IsZero() {
		seconds := int64(time.Until(validReset).Seconds() + 0.5)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(validReset.Unix(), 10))
		w.Header().Set("X-RateLimit-Reset-At", validReset.Format(time.RFC3339))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// protectMessages accepts either the panel session or an API key, matching
// v1's _key_ok gate. Each middleware is probed against a capturing writer; a
// successful probe runs the real handler with SSE flushing. A non-401 rejection
// from the gate that actually rejected the credential is preserved instead of
// being collapsed into 401 (for example per-key 429 concurrency limits).
func (s *Server) protectMessages(next http.Handler) http.Handler {
	if s == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeAnthropicError(w, http.StatusUnauthorized, "invalid api key", "authentication_error", time.Time{})
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			writer := &anthropicErrorWriter{ResponseWriter: w}
			defer writer.finish()
			next.ServeHTTP(writer, request)
		})

		var panelRejection *authProbe
		if gate := s.panelGate(); gate != nil {
			probe := &authProbe{header: make(http.Header)}
			if s.probeMessagesAuth(w, r, handler, gate, probe) {
				return
			}
			panelRejection = probe
		}
		apiProbe := &authProbe{header: make(http.Header)}
		if s.probeMessagesAuth(w, r, handler, s.apiGate(), apiProbe) {
			return
		}
		if apiProbe.status != 0 && apiProbe.status != http.StatusUnauthorized {
			apiProbe.writeAnthropic(w)
			return
		}
		if panelRejection != nil && panelRejection.status != 0 && panelRejection.status != http.StatusUnauthorized {
			panelRejection.writeAnthropic(w)
			return
		}
		writeAnthropicError(w, http.StatusUnauthorized, "invalid api key", "authentication_error", time.Time{})
	})
}

func (s *Server) probeMessagesAuth(w http.ResponseWriter, r *http.Request, gate http.Handler, auth func(http.Handler) http.Handler, probe *authProbe) bool {
	if auth == nil {
		return false
	}
	passed := false
	authorized := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		passed = true
		gate.ServeHTTP(w, request)
	})
	auth(authorized).ServeHTTP(probe, r)
	return passed
}

type authProbe struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (p *authProbe) Header() http.Header {
	if p.header == nil {
		p.header = make(http.Header)
	}
	return p.header
}

func (p *authProbe) WriteHeader(status int) { p.status = status }

func (p *authProbe) Write(data []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return p.body.Write(data)
}

func (p *authProbe) Flush() {}

func (p *authProbe) writeAnthropic(w http.ResponseWriter) {
	status := p.status
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	message := ""
	decoder := json.NewDecoder(bytes.NewReader(p.body.Bytes()))
	decoder.UseNumber()
	var envelope map[string]any
	if decoder.Decode(&envelope) == nil {
		if rawError, ok := envelope["error"].(map[string]any); ok {
			message = anthropicText(rawError["message"])
		} else if envelope["type"] == "error" {
			if rawError, ok := envelope["error"].(map[string]any); ok {
				message = anthropicText(rawError["message"])
			}
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	// Preserve the rate-limit recovery instant the gate advertised.
	for _, name := range []string{"Retry-After", "X-RateLimit-Reset-At", "X-RateLimit-Reset"} {
		if value := strings.TrimSpace(p.header.Get(name)); value != "" {
			w.Header().Set(name, value)
		}
	}
	reset := time.Time{}
	if raw := strings.TrimSpace(p.header.Get("X-RateLimit-Reset")); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
			reset = time.Unix(seconds, 0)
		}
	}
	writeAnthropicError(w, status, message, protocol.AnthropicErrorType(status), reset)
}

type anthropicErrorWriter struct {
	http.ResponseWriter
	status    int
	intercept bool
	body      bytes.Buffer
}

func (w *anthropicErrorWriter) WriteHeader(status int) {
	if status >= 400 {
		w.status = status
		w.intercept = true
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *anthropicErrorWriter) Write(data []byte) (int, error) {
	if w.intercept {
		return w.body.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *anthropicErrorWriter) Flush() {
	if w.intercept {
		return
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *anthropicErrorWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *anthropicErrorWriter) finish() {
	if !w.intercept || w.status == 0 {
		return
	}
	body := w.body.Bytes()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var envelope map[string]any
	if decoder.Decode(&envelope) == nil {
		if envelope["type"] == "error" {
			w.ResponseWriter.WriteHeader(w.status)
			_, _ = w.ResponseWriter.Write(body)
			return
		}
		if rawError, ok := envelope["error"].(map[string]any); ok {
			message := anthropicText(rawError["message"])
			if message == "" {
				message = "upstream error"
			}
			writeAnthropicError(w.ResponseWriter, w.status, message, protocol.AnthropicErrorType(w.status), time.Time{})
			return
		}
	}
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(body)
}
