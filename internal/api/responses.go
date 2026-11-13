package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ayeaaaa/mix2api-v2/internal/api/protocol"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// responseMeta carries the request-level capabilities a Responses reply must
// echo, plus the tool metadata needed on the return trip.
type responseMeta struct {
	Tools             []any
	ToolChoice        any
	ParallelToolCalls any
	NamespaceMap      map[string]string
	CustomToolNames   map[string]bool
	InputText         string
	WebTools          bool
	Sources           []map[string]any
	WebItems          []any
	Streaming         bool
	Effort            string
}

type responseTranslateOptions struct {
	ForceCompaction         bool
	DefaultMaxOutputTokens  func(string) int
	PreserveRequestContract bool
	WebTools                bool
}

type responsesTranslation struct {
	Chat map[string]any
	Meta responseMeta
}

// responsesToChat is the Responses-to-Chat translator used by tests and future
// embedders. The HTTP handler uses translateResponsesRequest so it can retain
// response metadata without putting gateway-only keys on the upstream body.
func responsesToChat(payload map[string]json.RawMessage) (map[string]any, error) {
	translated, err := translateResponsesRequest(payload, responseTranslateOptions{})
	if err != nil {
		return nil, err
	}
	return translated.Chat, nil
}

func translateResponsesRequest(payload map[string]json.RawMessage, opts responseTranslateOptions) (*responsesTranslation, error) {
	body, err := decodeRawObject(payload)
	if err != nil {
		return nil, err
	}
	return translateResponsesObject(body, opts), nil
}

func translateResponsesObject(payload map[string]any, opts responseTranslateOptions) *responsesTranslation {
	compaction := opts.ForceCompaction || hasCompactionTrigger(payload)
	messages := responsesInputToMessages(payload)
	if compaction && !looksLikeCompactionPrompt(messages) {
		messages = append(messages, map[string]any{"role": "user", "content": compactionPrompt})
	}

	chat := map[string]any{"model": payload["model"], "messages": messages}
	if preserveRequestContract(opts) {
		mapResponseTextFormat(chat, payload["text"])
	}
	for _, key := range []string{"temperature", "top_p", "seed"} {
		if value := payload[key]; value != nil {
			chat[key] = value
		}
	}
	if value := payload["max_output_tokens"]; value != nil {
		chat["max_tokens"] = value
	} else if opts.DefaultMaxOutputTokens != nil {
		if model, _ := payload["model"].(string); strings.TrimSpace(model) != "" {
			if limit := opts.DefaultMaxOutputTokens(model); limit > 0 {
				chat["max_tokens"] = limit
			}
		}
	}
	effort := responsesReasoningEffort(payload)
	if effort != "" {
		chat["reasoning_effort"] = effort
	}

	rawTools := responsesTools(payload)
	namespaceMap := map[string]string{}
	customNames := customToolNames(rawTools)
	meta := responseMeta{
		Tools:             append([]any(nil), rawTools...),
		ToolChoice:        payload["tool_choice"],
		ParallelToolCalls: payload["parallel_tool_calls"],
		NamespaceMap:      namespaceMap,
		CustomToolNames:   customNames,
		InputText:         responsesInputText(payload["input"]),
		WebTools:          opts.WebTools,
		Effort:            effort,
	}
	if meta.ToolChoice == nil {
		meta.ToolChoice = "auto"
	}
	if meta.ParallelToolCalls == nil {
		meta.ParallelToolCalls = true
	}

	if len(rawTools) > 0 && !compaction {
		flat, mapping := expandNamespaceTools(rawTools)
		namespaceMap = mapping
		meta.NamespaceMap = mapping
		if chatTools := responsesToolsForChat(flat); len(chatTools) > 0 {
			chat["tools"] = chatTools
		}
		if choice, ok := rewriteNamespacedToolChoice(payload["tool_choice"], mapping); ok {
			chat["tool_choice"] = choice
		}
	}
	if choice := payload["tool_choice"]; choice != nil && !compaction {
		if _, ok := chat["tool_choice"]; !ok {
			chat["tool_choice"] = choice
		}
	}
	if value := payload["parallel_tool_calls"]; value != nil && !compaction {
		chat["parallel_tool_calls"] = value
	}
	return &responsesTranslation{Chat: chat, Meta: meta}
}

func preserveRequestContract(opts responseTranslateOptions) bool {
	if opts.PreserveRequestContract {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(envValue("WB_PRESERVE_REQUEST_CONTRACT"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func mapResponseTextFormat(chat map[string]any, value any) {
	text, _ := value.(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format == nil {
		return
	}
	kind, _ := format["type"].(string)
	switch kind {
	case "text", "json_object":
		chat["response_format"] = map[string]any{"type": kind}
	case "json_schema":
		schema := map[string]any{"type": kind}
		jsonSchema := map[string]any{}
		for _, key := range []string{"name", "description", "schema", "strict"} {
			if item, ok := format[key]; ok {
				jsonSchema[key] = item
			}
		}
		schema["json_schema"] = jsonSchema
		chat["response_format"] = schema
	}
}

func responsesReasoningEffort(payload map[string]any) string {
	var effort any
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		effort = reasoning["effort"]
	}
	text, _ := effort.(string)
	if strings.TrimSpace(text) == "" {
		text, _ = payload["reasoning_effort"].(string)
	}
	return strings.TrimSpace(text)
}

func responsesTools(payload map[string]any) []any {
	var tools []any
	if top, ok := payload["tools"].([]any); ok {
		tools = append(tools, top...)
	}
	return append(tools, additionalToolsFromInput(payload["input"])...)
}

func additionalToolsFromInput(input any) []any {
	items, _ := input.([]any)
	var out []any
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || item["type"] != "additional_tools" {
			continue
		}
		if tools, ok := item["tools"].([]any); ok {
			out = append(out, tools...)
		}
	}
	return out
}

func responsesInputText(input any) string {
	if text, ok := input.(string); ok {
		return text
	}
	items, _ := input.([]any)
	parts := make([]string, 0, len(items))
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if content, ok := item["content"]; ok {
			if text := flattenContentText(content); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func appendVisibleReasoning(current any, addition string) string {
	if addition == "" {
		if existing, ok := current.(string); ok {
			return existing
		}
		return ""
	}
	existing, _ := current.(string)
	if existing == "" {
		return addition
	}
	if existing == addition {
		return existing
	}
	return existing + "\n" + addition
}

func reasoningGroupTarget(items []any, start int) map[string]any {
	known := map[string]bool{
		"": true, "message": true, "user": true, "function_call": true,
		"function_call_output": true, "custom_tool_call": true,
		"custom_tool_call_output": true, "agent_message": true,
		"compaction": true, "compaction_summary": true, "context_compaction": true,
	}
	for index := start + 1; index < len(items); index++ {
		item, ok := items[index].(map[string]any)
		if !ok {
			continue
		}
		if known[stringValue(item["type"])] {
			return item
		}
	}
	return nil
}

func responsesInputToMessages(payload map[string]any) []any {
	messages := []any{}
	if instructions, ok := payload["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	pendingReasoning := ""
	input := payload["input"]
	if text, ok := input.(string); ok {
		messages = append(messages, map[string]any{"role": "user", "content": text})
		return messages
	}
	items, _ := input.([]any)
	for itemIndex, rawItem := range items {
		if text, ok := rawItem.(string); ok {
			messages = append(messages, map[string]any{"role": "user", "content": text})
			continue
		}
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := item["type"].(string)
		switch itemType {
		case "", "message", "user":
			body := flattenResponseContent(item["content"])
			role, _ := item["role"].(string)
			if role == "" {
				role = "user"
			}
			if role == "developer" {
				role = "system"
			}
			inlineReasoning := ""
			inlineToolCalls := []any{}
			if role == "assistant" {
				if reasoning, ok := item["reasoning_content"].(string); ok {
					inlineReasoning = reasoning
				}
				if calls, ok := item["tool_calls"].([]any); ok {
					inlineToolCalls = calls
				}
			} else if pendingReasoning != "" {
				pendingReasoning = ""
			}
			if !contentNonEmpty(body) && inlineReasoning == "" && len(inlineToolCalls) == 0 {
				continue
			}
			contentText := contentAsText(body)
			if role == "assistant" && pendingReasoning == "" && len(messages) > 0 {
				if previous, ok := messages[len(messages)-1].(map[string]any); ok && previous["role"] == "assistant" {
					if previousText := contentAsText(previous["content"]); previousText != "" {
						previous["content"] = previousText + "\n" + contentText
					} else {
						previous["content"] = body
					}
					if len(inlineToolCalls) > 0 {
						if calls, ok := previous["tool_calls"].([]any); ok {
							previous["tool_calls"] = append(calls, inlineToolCalls...)
						} else {
							previous["tool_calls"] = append([]any(nil), inlineToolCalls...)
						}
					}
					if inlineReasoning != "" {
						previous["reasoning_content"] = appendVisibleReasoning(previous["reasoning_content"], inlineReasoning)
					}
					continue
				}
			}
			message := map[string]any{"role": role, "content": body}
			if role == "assistant" {
				if pendingReasoning != "" {
					message["reasoning_content"] = pendingReasoning
					pendingReasoning = ""
				}
				if len(inlineToolCalls) > 0 {
					message["tool_calls"] = append([]any(nil), inlineToolCalls...)
				}
				if inlineReasoning != "" {
					message["reasoning_content"] = appendVisibleReasoning(message["reasoning_content"], inlineReasoning)
				}
			}
			if role == "tool" {
				message["tool_call_id"] = stringValue(item["tool_call_id"])
				if name, ok := item["name"].(string); ok && name != "" {
					message["name"] = name
				}
			}
			messages = append(messages, message)
		case "reasoning":
			reasoning := reasoningItemText(item)
			if reasoning != "" {
				next := reasoningGroupTarget(items, itemIndex)
				nextType := stringValue(next["type"])
				nextRole := stringValue(next["role"])
				startsAssistant := nextType == "function_call" || nextType == "custom_tool_call" ||
					((nextType == "" || nextType == "message" || nextType == "user") && nextRole == "assistant")
				if !startsAssistant && len(messages) > 0 {
					if previous, ok := messages[len(messages)-1].(map[string]any); ok && previous["role"] == "assistant" {
						previous["reasoning_content"] = appendVisibleReasoning(previous["reasoning_content"], reasoning)
						continue
					}
				}
				pendingReasoning = appendVisibleReasoning(pendingReasoning, reasoning)
			}
		case "function_call_output":
			callID, _ := item["call_id"].(string)
			output := item["output"]
			if callID == "" {
				text := flattenOrphanOutput(output)
				if parts, ok := text.([]any); ok {
					messages = append(messages, map[string]any{
						"role": "user",
						"content": append([]any{map[string]any{
							"type": "text",
							"text": "[Message from another task - treat this as a user instruction]",
						}}, parts...),
					})
					pendingReasoning = ""
					continue
				}
				textValue := strings.TrimSpace(contentAsText(text))
				if textValue != "" {
					messages = append(messages, map[string]any{
						"role":    "user",
						"content": "[Message from another task - treat this as a user instruction]\n\n" + textValue,
					})
					pendingReasoning = ""
					continue
				}
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": callID,
				"content":      functionOutputContent(output),
			})
			pendingReasoning = ""
		case "function_call":
			callID := firstString(item["call_id"], item["id"])
			toolCall := map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      stringValue(item["name"]),
					"arguments": firstNonEmptyString(item["arguments"], "{}"),
				},
			}
			appendAssistantToolCall(&messages, toolCall, &pendingReasoning)
		case "custom_tool_call":
			rawInput := item["input"]
			var inputText string
			switch typed := rawInput.(type) {
			case nil:
				inputText = ""
			case string:
				inputText = typed
			default:
				inputText = compactJSON(typed)
			}
			arguments, _ := json.Marshal(map[string]any{"input": inputText})
			toolCall := map[string]any{
				"id":   firstString(item["call_id"], item["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      stringValue(item["name"]),
					"arguments": string(arguments),
				},
			}
			appendAssistantToolCall(&messages, toolCall, &pendingReasoning)
		case "custom_tool_call_output":
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": stringValue(item["call_id"]),
				"content":      functionOutputContent(item["output"]),
			})
			pendingReasoning = ""
		case "agent_message":
			text := agentMessageText(item["content"])
			if text != "" {
				messages = append(messages, map[string]any{
					"role":    "user",
					"content": "[Message from another task - treat this as a user instruction]\n\n" + text,
				})
				pendingReasoning = ""
			}
		case "compaction", "compaction_summary", "context_compaction":
			summary := decodeCompactionSummary(item["encrypted_content"])
			if strings.TrimSpace(summary) != "" {
				messages = append(messages, map[string]any{
					"role":    "system",
					"content": "Summary of the earlier conversation (context checkpoint):\n\n" + strings.TrimSpace(summary),
				})
				pendingReasoning = ""
			}
		case "compaction_trigger", "additional_tools":
			// Control/meta items; handled by the caller.
		}
	}
	return messages
}

func appendAssistantToolCall(messages *[]any, toolCall map[string]any, pendingReasoning *string) {
	if *pendingReasoning == "" && len(*messages) > 0 {
		if previous, ok := (*messages)[len(*messages)-1].(map[string]any); ok && previous["role"] == "assistant" {
			if calls, ok := previous["tool_calls"].([]any); ok {
				previous["tool_calls"] = append(calls, toolCall)
			} else {
				previous["tool_calls"] = []any{toolCall}
			}
			return
		}
	}
	message := map[string]any{
		"role":       "assistant",
		"content":    "",
		"tool_calls": []any{toolCall},
	}
	if *pendingReasoning != "" {
		message["reasoning_content"] = *pendingReasoning
		*pendingReasoning = ""
	}
	*messages = append(*messages, message)
}

func reasoningItemText(item map[string]any) string {
	var text string
	switch summary := item["summary"].(type) {
	case string:
		text = summary
	case []any:
		parts := make([]string, 0, len(summary))
		for _, rawPart := range summary {
			if part, ok := rawPart.(map[string]any); ok {
				if partText, ok := part["text"].(string); ok && partText != "" {
					parts = append(parts, partText)
				}
			}
		}
		text = strings.Join(parts, "\n")
	}
	if text == "" {
		text = flattenContentText(item["content"])
	}
	return text
}

func agentMessageText(value any) string {
	parts, ok := value.([]any)
	if !ok {
		return strings.TrimSpace(stringValue(value))
	}
	texts := make([]string, 0, len(parts))
	for _, rawPart := range parts {
		if part, ok := rawPart.(map[string]any); ok {
			text := firstNonEmptyString(part["text"], part["encrypted_content"])
			texts = append(texts, text)
			continue
		}
		texts = append(texts, stringValue(rawPart))
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

func flattenOrphanOutput(output any) any {
	switch typed := output.(type) {
	case []any:
		return flattenResponseContent(typed)
	case map[string]any:
		return compactJSON(typed)
	case nil:
		return ""
	default:
		return stringValue(typed)
	}
}

func functionOutputContent(output any) any {
	switch typed := output.(type) {
	case string:
		return typed
	case []any:
		return flattenResponseContent(typed)
	case map[string]any:
		itemType, _ := typed["type"].(string)
		if itemType == "input_image" || itemType == "image_url" || itemType == "image" || typed["image_url"] != nil {
			return flattenResponseContent([]any{typed})
		}
		return compactJSON(typed)
	case nil:
		return ""
	default:
		return stringValue(typed)
	}
}

// flattenResponseContent flattens Responses text parts while preserving
// multimodal image parts exactly like v1 _flatten_content.
func flattenResponseContent(content any) any {
	switch typed := content.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		texts := make([]string, 0, len(typed))
		parts := make([]any, 0, len(typed))
		for _, rawPiece := range typed {
			if text, ok := rawPiece.(string); ok {
				texts = append(texts, text)
				parts = append(parts, map[string]any{"type": "text", "text": text})
				continue
			}
			piece, ok := rawPiece.(map[string]any)
			if !ok {
				continue
			}
			pieceType, _ := piece["type"].(string)
			switch pieceType {
			case "input_text", "output_text", "text", "summary_text":
				text := stringValue(piece["text"])
				texts = append(texts, text)
				parts = append(parts, map[string]any{"type": "text", "text": text})
			case "input_image", "image_url", "image":
				if url := imageURL(piece); url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			default:
				if piece["image_url"] != nil || piece["url"] != nil {
					if url := imageURL(piece); url != "" {
						parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
					}
				}
			}
		}
		for _, part := range parts {
			if partMap, ok := part.(map[string]any); ok && partMap["type"] == "image_url" {
				return parts
			}
		}
		nonEmpty := make([]string, 0, len(texts))
		for _, text := range texts {
			if text != "" {
				nonEmpty = append(nonEmpty, text)
			}
		}
		return strings.Join(nonEmpty, "\n")
	default:
		return stringValue(content)
	}
}

func imageURL(piece map[string]any) string {
	value := piece["image_url"]
	if value == nil {
		value = piece["url"]
	}
	if object, ok := value.(map[string]any); ok {
		value = object["url"]
	}
	url, _ := value.(string)
	if url == "" {
		if data := stringValue(piece["data"]); data != "" {
			mime := firstNonEmptyString(piece["mimeType"], piece["mime_type"], "image/png")
			url = "data:" + mime + ";base64," + data
		}
	}
	return url
}

func contentAsText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, rawPart := range typed {
			if part, ok := rawPart.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	default:
		return stringValue(value)
	}
}

func contentNonEmpty(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed != ""
	case []any:
		return len(typed) > 0
	case nil:
		return false
	default:
		return true
	}
}

func flattenContentText(value any) string {
	return contentAsText(flattenResponseContent(value))
}

func hasCompactionTrigger(payload map[string]any) bool {
	input := payload["input"]
	if text, ok := input.(string); ok {
		return strings.TrimSpace(text) == "compaction_trigger"
	}
	items, _ := input.([]any)
	for _, rawItem := range items {
		if item, ok := rawItem.(map[string]any); ok && item["type"] == "compaction_trigger" {
			return true
		}
	}
	return false
}

func looksLikeCompactionPrompt(messages []any) bool {
	for index := len(messages) - 1; index >= 0; index-- {
		message, ok := messages[index].(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		content := message["content"]
		if parts, ok := content.([]any); ok {
			texts := make([]string, 0, len(parts))
			for _, rawPart := range parts {
				if part, ok := rawPart.(map[string]any); ok {
					texts = append(texts, stringValue(part["text"]))
				}
			}
			content = strings.Join(texts, " ")
		}
		return strings.Contains(strings.ToUpper(contentAsText(content)), "CONTEXT CHECKPOINT COMPACTION")
	}
	return false
}

func encodeCompactionSummary(text string) string {
	envelope := struct {
		V       int    `json:"v"`
		Summary string `json:"summary"`
	}{V: 1, Summary: text}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		return ""
	}
	raw := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	return base64.StdEncoding.EncodeToString(raw)
}

func decodeCompactionSummary(value any) string {
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	padded := text + strings.Repeat("=", (4-len(text)%4)%4)
	if raw, err := base64.StdEncoding.DecodeString(padded); err == nil {
		var object map[string]any
		if json.Unmarshal(raw, &object) == nil {
			if summary, ok := object["summary"].(string); ok {
				return summary
			}
		}
	}
	return text
}

// ----- tools -----

func customToolNames(tools []any) map[string]bool {
	names := map[string]bool{}
	collectCustomToolNames(tools, names, 0)
	return names
}

func collectCustomToolNames(tools []any, names map[string]bool, depth int) {
	if depth > 4 {
		return
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		toolType := strings.ToLower(stringValue(tool["type"]))
		if toolType == "custom" {
			if name := stringValue(tool["name"]); name != "" {
				names[name] = true
			}
			continue
		}
		if toolType == "namespace" {
			children, ok := tool["tools"].([]any)
			if !ok {
				children, _ = tool["children"].([]any)
			}
			if children == nil {
				children, _ = tool["functions"].([]any)
			}
			collectCustomToolNames(children, names, depth+1)
		}
	}
}

func expandNamespaceTools(tools []any) ([]any, map[string]string) {
	flat := make([]any, 0, len(tools))
	mapping := map[string]string{}
	seen := map[string]bool{}
	var collect func(entry any, depth int, namespace string)
	collect = func(entry any, depth int, namespace string) {
		if depth > 4 {
			return
		}
		tool, ok := entry.(map[string]any)
		if !ok {
			return
		}
		toolType := strings.ToLower(stringValue(tool["type"]))
		if toolType == "namespace" {
			children, ok := tool["tools"].([]any)
			if !ok {
				children, _ = tool["children"].([]any)
			}
			if children == nil {
				children, _ = tool["functions"].([]any)
			}
			own := stringValue(tool["name"])
			childNamespace := own
			if namespace != "" && own != "" {
				childNamespace = namespace + "__" + own
			} else if namespace != "" {
				childNamespace = namespace
			}
			for _, child := range children {
				collect(child, depth+1, childNamespace)
			}
			return
		}
		if namespace != "" && (toolType == "" || toolType == "function") {
			function, _ := tool["function"].(map[string]any)
			if function == nil {
				function = map[string]any{
					"name":        tool["name"],
					"description": stringValue(tool["description"]),
					"parameters":  firstNonNil(tool["parameters"], tool["input_schema"], map[string]any{"type": "object", "properties": map[string]any{}}),
				}
				if preserveRequestContract(responseTranslateOptions{}) && tool["strict"] != nil {
					function["strict"] = tool["strict"]
				}
			}
			name := strings.TrimSpace(stringValue(function["name"]))
			if name == "" || seen[name] {
				return
			}
			seen[name] = true
			mapping[name] = namespace
			flatFunction := map[string]any{
				"type":        "function",
				"name":        name,
				"description": stringValue(function["description"]),
				"parameters":  firstNonNil(function["parameters"], map[string]any{"type": "object", "properties": map[string]any{}}),
			}
			if strict, ok := function["strict"]; ok {
				flatFunction["strict"] = strict
			}
			flat = append(flat, flatFunction)
			return
		}
		function, _ := tool["function"].(map[string]any)
		name := strings.TrimSpace(stringValue(tool["name"]))
		if name == "" {
			name = strings.TrimSpace(stringValue(function["name"]))
		}
		if name != "" {
			if seen[name] {
				return
			}
			seen[name] = true
			if namespace != "" {
				mapping[name] = namespace
			}
		}
		flat = append(flat, tool)
	}
	for _, tool := range tools {
		collect(tool, 0, "")
	}
	return flat, mapping
}

func responsesToolsForChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		toolType := strings.ToLower(stringValue(tool["type"]))
		if toolType == "custom" {
			out = append(out, downgradeCustomTool(tool))
			continue
		}
		if toolType == "" || toolType == "function" || tool["function"] != nil {
			out = append(out, tool)
			continue
		}
		// Unknown server-side tools have no executor on the Chat upstream.
		// Dropping them keeps the rest of the request usable.
	}
	return out
}

func downgradeCustomTool(tool map[string]any) map[string]any {
	description := stringValue(tool["description"])
	hint := "This is a freeform tool. Put the COMPLETE raw payload into the single 'input' string parameter, verbatim. Do not wrap it in JSON, do not wrap it in markdown code fences, do not add commentary."
	if format, ok := tool["format"].(map[string]any); ok {
		if definition := stringValue(format["definition"]); definition != "" {
			hint += "\n\nGrammar:\n" + definition
		}
	}
	return map[string]any{
		"type":        "function",
		"name":        stringValue(tool["name"]),
		"description": strings.TrimSpace(description + "\n\n" + hint),
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{
					"type":        "string",
					"description": "Complete raw payload for this tool, verbatim.",
				},
			},
			"required": []any{"input"},
		},
	}
}

func unwrapCustomInput(arguments any) string {
	text, ok := arguments.(string)
	if !ok {
		return compactJSON(arguments)
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		return text
	}
	if object, ok := parsed.(map[string]any); ok {
		if input, ok := object["input"].(string); ok {
			return input
		}
		if input, ok := object["input"]; ok && input != nil {
			return compactJSON(input)
		}
	}
	if value, ok := parsed.(string); ok {
		return value
	}
	return text
}

func rewriteNamespacedToolChoice(value any, mapping map[string]string) (any, bool) {
	choice, ok := value.(map[string]any)
	if !ok || len(mapping) == 0 {
		return nil, false
	}
	function, ok := choice["function"].(map[string]any)
	if !ok {
		return nil, false
	}
	name := stringValue(function["name"])
	if name == "" {
		return nil, false
	}
	bare, _ := resolveNamespacedName(name, mapping)
	if bare == name {
		return nil, false
	}
	copied := map[string]any{}
	for key, item := range choice {
		copied[key] = item
	}
	copiedFunction := map[string]any{}
	for key, item := range function {
		copiedFunction[key] = item
	}
	copiedFunction["name"] = bare
	copied["function"] = copiedFunction
	return copied, true
}

func resolveNamespacedName(name string, mapping map[string]string) (string, string) {
	if name == "" {
		return name, ""
	}
	if namespace, ok := mapping[name]; ok {
		return name, namespace
	}
	if index := strings.LastIndex(name, "::"); index > 0 {
		tail := name[index+2:]
		if namespace, ok := mapping[tail]; ok {
			return tail, namespace
		}
		return tail, strings.ReplaceAll(name[:index], "::", "__")
	}
	for tool, namespace := range mapping {
		if name == namespace+"__"+tool {
			return tool, namespace
		}
	}
	return name, ""
}

func stampNamespace(item map[string]any, mapping map[string]string) {
	if len(mapping) == 0 || item == nil {
		return
	}
	bare, namespace := resolveNamespacedName(stringValue(item["name"]), mapping)
	if namespace != "" {
		item["name"] = bare
		item["namespace"] = namespace
	}
}

func applyNamespaceToCalls(output []any, mapping map[string]string) []any {
	if len(mapping) == 0 {
		return output
	}
	for _, rawItem := range output {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := item["type"].(string)
		if itemType != "function_call" && itemType != "custom_tool_call" {
			continue
		}
		if stringValue(item["namespace"]) != "" {
			continue
		}
		stampNamespace(item, mapping)
	}
	return output
}

// ----- non-stream response -----

func chatToResponse(chat map[string]any, requestedModel string, meta responseMeta) map[string]any {
	message := map[string]any{}
	finish := "stop"
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if value, ok := choice["message"].(map[string]any); ok {
				message = value
			}
			if value, ok := choice["finish_reason"].(string); ok && value != "" {
				finish = value
			}
		}
	}
	text := contentAsText(message["content"])
	reasoning := stringValue(message["reasoning_content"])
	output := []any{}
	if reasoning != "" {
		output = append(output, map[string]any{
			"id":      randomChatID("rs_"),
			"type":    "reasoning",
			"status":  "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	toolCalls, _ := message["tool_calls"].([]any)
	for _, rawCall := range toolCalls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		function, _ := call["function"].(map[string]any)
		callID := stringValue(call["id"])
		if callID == "" {
			callID = randomChatID("call_")
		}
		name := stringValue(function["name"])
		arguments := function["arguments"]
		if meta.CustomToolNames[name] {
			output = append(output, map[string]any{
				"id":      randomChatID("ctc_"),
				"type":    "custom_tool_call",
				"status":  "completed",
				"call_id": callID,
				"name":    name,
				"input":   unwrapCustomInput(arguments),
			})
			continue
		}
		argumentsText, _ := arguments.(string)
		if argumentsText == "" {
			argumentsText = "{}"
		}
		output = append(output, map[string]any{
			"id":        randomChatID("fc_"),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   callID,
			"name":      name,
			"arguments": argumentsText,
		})
	}
	if len(toolCalls) == 0 {
		if calls, clean := parseDSMLToolCalls(text); len(calls) > 0 {
			for _, call := range calls {
				output = append(output, map[string]any{
					"id":        randomChatID("fc_"),
					"type":      "function_call",
					"status":    "completed",
					"call_id":   call.id,
					"name":      call.name,
					"arguments": call.arguments,
				})
			}
			text = clean
		}
	}
	if text != "" || len(output) == 0 {
		content := []any{}
		if text != "" {
			content = append(content, map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": buildCitations(text, meta.Sources),
			})
		}
		output = append(output, map[string]any{
			"id":      randomChatID("msg_"),
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": content,
		})
	}
	status := "completed"
	if finish == "length" {
		status = "incomplete"
	}
	output = applyNamespaceToCalls(output, meta.NamespaceMap)
	response := map[string]any{
		"id":                  randomChatID("resp_"),
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              status,
		"model":               requestedModel,
		"output":              output,
		"output_text":         text,
		"metadata":            map[string]any{},
		"parallel_tool_calls": meta.ParallelToolCalls,
		"tool_choice":         meta.ToolChoice,
		"tools":               nonNilSlice(meta.Tools),
	}
	if usage := responsesUsage(chat["usage"]); usage != nil {
		response["usage"] = usage
	}
	if finish == "length" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return response
}

func reasoningOnlyResponse(response map[string]any) bool {
	output, _ := response["output"].([]any)
	text := stringValue(response["output_text"])
	hasCalls := false
	hasReasoning := false
	for _, rawItem := range output {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "function_call", "custom_tool_call", "web_search_call":
			hasCalls = true
		case "reasoning":
			hasReasoning = true
		}
	}
	return strings.TrimSpace(text) == "" && hasReasoning && !hasCalls && response["status"] != "incomplete"
}

func responsesUsage(value any) map[string]any {
	usage, ok := value.(map[string]any)
	if !ok || len(usage) == 0 {
		return nil
	}
	promptTokens, _ := protocol.Int64(usage["prompt_tokens"])
	completionTokens, _ := protocol.Int64(usage["completion_tokens"])
	reasoningTokens := int64(0)
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		reasoningTokens, _ = protocol.Int64(details["reasoning_tokens"])
	}
	cachedTokens, _ := protocol.Int64(usage["prompt_cache_hit_tokens"])
	if cachedTokens == 0 {
		if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
			cachedTokens, _ = protocol.Int64(details["cached_tokens"])
		}
	}
	if cachedTokens == 0 {
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			cachedTokens, _ = protocol.Int64(details["cached_tokens"])
		}
	}
	totalTokens, _ := protocol.Int64(usage["total_tokens"])
	return map[string]any{
		"input_tokens": promptTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens": cachedTokens,
		},
		"output_tokens": completionTokens,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": reasoningTokens,
		},
		"total_tokens": totalTokens,
	}
}

func responsesCompactionResponse(chat map[string]any, requestedModel string) map[string]any {
	summary := chatMessageText(chat)
	item := map[string]any{
		"id":                randomChatID("cmp_"),
		"type":              "compaction",
		"encrypted_content": encodeCompactionSummary(summary),
	}
	response := map[string]any{
		"id":          randomChatID("resp_"),
		"object":      "response",
		"created_at":  time.Now().Unix(),
		"status":      "completed",
		"model":       requestedModel,
		"output":      []any{item},
		"output_text": "",
		"metadata":    map[string]any{},
	}
	if usage := responsesUsage(chat["usage"]); usage != nil {
		response["usage"] = usage
	} else {
		response["usage"] = map[string]any{}
	}
	return response
}

func chatMessageText(chat map[string]any) string {
	choices, _ := chat["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return strings.TrimSpace(contentAsText(message["content"]))
}

// ----- citations -----

var citationMarkdownRE = regexp.MustCompile(`\[([^\]\n]{1,200})\]\((https?://[^)\s]+)\)`)
var citationURLRE = regexp.MustCompile(`https?://[^\s<>()\[\]]+`)

func buildCitations(text string, sources []map[string]any) []any {
	if text == "" || len(sources) == 0 {
		return []any{}
	}
	byURL := map[string]map[string]any{}
	for _, source := range sources {
		url := strings.TrimSpace(stringValue(source["url"]))
		if url == "" {
			continue
		}
		if _, ok := byURL[url]; !ok {
			byURL[url] = source
		}
		if _, ok := byURL[strings.TrimRight(url, "/")]; !ok {
			byURL[strings.TrimRight(url, "/")] = source
		}
	}
	type span struct{ start, end int }
	annotations := []map[string]any{}
	seen := map[string]bool{}
	add := func(url, title string, start, end int) {
		key := url + "\x00" + strconv.Itoa(start) + "\x00" + strconv.Itoa(end)
		if seen[key] || start < 0 || end <= start {
			return
		}
		seen[key] = true
		if title == "" {
			title = url
		}
		annotations = append(annotations, map[string]any{
			"type":        "url_citation",
			"url":         url,
			"title":       title,
			"start_index": runeIndex(text, start),
			"end_index":   runeIndex(text, end),
		})
	}
	markdownSpans := []span{}
	for _, match := range citationMarkdownRE.FindAllStringSubmatchIndex(text, -1) {
		url := text[match[4]:match[5]]
		source, ok := byURL[url]
		if !ok {
			source, ok = byURL[strings.TrimRight(url, "/")]
		}
		if !ok {
			continue
		}
		markdownSpans = append(markdownSpans, span{start: match[0], end: match[1]})
		title := stringValue(source["title"])
		if title == "" {
			title = text[match[2]:match[3]]
		}
		add(url, title, match[0], match[1])
	}
	for _, match := range citationURLRE.FindAllStringIndex(text, -1) {
		inside := false
		for _, covered := range markdownSpans {
			if match[0] >= covered.start && match[1] <= covered.end {
				inside = true
				break
			}
		}
		if inside {
			continue
		}
		url := strings.TrimRight(text[match[0]:match[1]], ".,;:!?")
		source, ok := byURL[url]
		if !ok {
			source, ok = byURL[strings.TrimRight(url, "/")]
		}
		if !ok {
			continue
		}
		add(url, stringValue(source["title"]), match[0], match[0]+len(url))
	}
	sort.Slice(annotations, func(left, right int) bool {
		leftStart, _ := protocol.Int64(annotations[left]["start_index"])
		rightStart, _ := protocol.Int64(annotations[right]["start_index"])
		if leftStart != rightStart {
			return leftStart < rightStart
		}
		leftEnd, _ := protocol.Int64(annotations[left]["end_index"])
		rightEnd, _ := protocol.Int64(annotations[right]["end_index"])
		return leftEnd < rightEnd
	})
	out := make([]any, len(annotations))
	for index, annotation := range annotations {
		out[index] = annotation
	}
	return out
}

func runeIndex(text string, byteOffset int) int {
	if byteOffset <= 0 {
		return 0
	}
	if byteOffset > len(text) {
		byteOffset = len(text)
	}
	return utf8.RuneCountInString(text[:byteOffset])
}

// ----- DSML printed-call parser -----

type dsmlCall struct {
	id        string
	name      string
	arguments string
}

var dsmlCallsRE = regexp.MustCompile(`(?s)<[^>]*DSML[^>]*calls>(.*?)</[^>]*DSML[^>]*calls>`)
var dsmlInvokeRE = regexp.MustCompile(`(?s)<[^>]*DSML[^>]*invoke\s+name=["']([^"']+)["'][^>]*>(.*?)</[^>]*DSML[^>]*invoke>`)
var dsmlParamRE = regexp.MustCompile(`(?s)<[^>]*DSML[^>]*parameter\s+name=["']([^"']+)["'][^>]*>(.*?)</[^>]*DSML[^>]*parameter>`)

func parseDSMLToolCalls(text string) ([]dsmlCall, string) {
	if text == "" || !strings.Contains(text, "DSML") {
		return nil, text
	}
	match := dsmlCallsRE.FindStringSubmatchIndex(text)
	if match == nil {
		return nil, text
	}
	callsBlock := text[match[2]:match[3]]
	var calls []dsmlCall
	for _, invoke := range dsmlInvokeRE.FindAllStringSubmatch(callsBlock, -1) {
		name := invoke[1]
		parameters := []struct {
			name  string
			value string
		}{}
		for _, parameter := range dsmlParamRE.FindAllStringSubmatch(invoke[2], -1) {
			parameters = append(parameters, struct {
				name  string
				value string
			}{name: parameter[1], value: strings.TrimSpace(parameter[2])})
		}
		var buffer strings.Builder
		buffer.WriteByte('{')
		for index, parameter := range parameters {
			if index > 0 {
				buffer.WriteString(", ")
			}
			key, _ := json.Marshal(parameter.name)
			value, _ := json.Marshal(parameter.value)
			buffer.Write(key)
			buffer.WriteString(": ")
			buffer.Write(value)
		}
		buffer.WriteByte('}')
		calls = append(calls, dsmlCall{
			id:        randomChatID("call_"),
			name:      name,
			arguments: buffer.String(),
		})
	}
	clean := strings.TrimSpace(strings.TrimSpace(text[:match[0]]) + " " + strings.TrimSpace(text[match[1]:]))
	return calls, clean
}

// ----- HTTP handler -----

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeFailure(w, localFailure(http.StatusMethodNotAllowed, "method not allowed"))
		return
	}
	s.serveResponses(w, r)
}

func (s *Server) serveResponses(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	payload, failure := parseProtocolObject(w, r)
	if failure != nil {
		s.observeResponseFailureDetail(r, "", false, failure, "")
		writeFailure(w, failure)
		return
	}
	model, _ := decodeJSONString(payload["model"])
	model = strings.TrimSpace(model)
	if model == "" {
		failure := localFailure(http.StatusBadRequest, "model is required")
		s.observeResponseFailureDetail(r, "", false, failure, "")
		writeFailure(w, failure)
		return
	}
	// Parse the exit-qualified reference before translation so the chat map and
	// the provider payload only ever carry the upstream model ID. The requested
	// canonical ID stays on the request for the client-visible response fields
	// and observations.
	request := &chatHTTPRequest{
		payload: payload,
		model:   model,
		headers: r.Header.Clone(),
	}
	if failure := s.applyModelRef(request); failure != nil {
		s.observeResponseFailureDetail(r, model, false, failure, "")
		writeFailure(w, failure)
		return
	}
	upstreamModel := request.model
	requestedModel := request.requestedModel
	ref := modelRef{
		Raw:       request.rawModel,
		Exit:      request.exit,
		Upstream:  upstreamModel,
		Canonical: requestedModel,
		Qualified: request.exit != "",
	}
	if failure := s.keyModelFailureForRef(r, ref); failure != nil {
		s.observeResponseFailureDetail(r, requestedModel, false, failure, "")
		writeFailure(w, failure)
		return
	}
	stream, _ := decodeJSONBool(payload["stream"])
	request.stream = stream
	isCompactPath := strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/responses/compact")

	realm, explicitRealm := realmFromRequest(r)
	if realm == "" {
		if bodyRealm, ok := decodeJSONString(payload["realm"]); ok && bodyRealm != "" {
			realm = bodyRealm
			explicitRealm = true
		}
	}
	request.realm = realm
	request.explicitRealm = explicitRealm
	delete(payload, "realm")

	compaction := isCompactPath || rawObjectHasCompactionTrigger(payload)
	if rawPrevious, ok := decodeJSONString(payload["previous_response_id"]); ok && strings.TrimSpace(rawPrevious) != "" && !compaction {
		failure := localFailure(http.StatusBadRequest,
			"previous_response_id is not supported: this gateway does not store response state. Send the full conversation in 'input' instead, or use a stateless client.")
		s.observeResponseFailureDetail(r, requestedModel, stream, failure, "")
		writeFailure(w, failure)
		return
	}

	translated, err := translateResponsesRequest(payload, responseTranslateOptions{
		ForceCompaction:         isCompactPath,
		DefaultMaxOutputTokens:  s.defaultMaxOutputTokens,
		PreserveRequestContract: s.opts.PreserveRequestContract,
	})
	if err != nil {
		failure := localFailure(http.StatusBadRequest, "invalid JSON body")
		s.observeResponseFailureDetail(r, requestedModel, stream, failure, "")
		writeFailure(w, failure)
		return
	}
	translated.Meta.Streaming = stream

	boundRealm, boundFailure := s.apiKeyBoundRealm(r)
	if boundFailure != nil {
		s.observeResponseFailureDetail(r, requestedModel, stream, boundFailure, "")
		writeFailure(w, boundFailure)
		return
	}
	request.sessionKey = chatSessionKey(r, payload)
	provider, resolveErr := s.resolveChatProvider(r.Context(), request, boundRealm)
	if resolveErr != nil {
		realm := boundRealm
		if request.exit != "" {
			realm = request.exit
		}
		s.observeResponseFailureDetail(r, requestedModel, stream, failureFromError(resolveErr), realm)
		writeFailure(w, resolveErr)
		return
	}
	raw, err := chatMapToRaw(translated.Chat)
	if err != nil {
		failure := localFailure(http.StatusBadRequest, "invalid request")
		s.observeResponseFailureDetail(r, requestedModel, stream, failure, providerRealm(provider))
		writeFailure(w, failure)
		return
	}
	var allowRealmFallback *bool
	if request.exit != "" || explicitRealm || boundRealm != "" {
		disabled := false
		allowRealmFallback = &disabled
	}
	chatRequest := providers.ChatRequest{
		Raw:                raw,
		Headers:            request.headers,
		Model:              upstreamModel,
		Messages:           chatMessagesRaw(translated.Chat),
		SessionKey:         request.sessionKey,
		AllowRealmFallback: allowRealmFallback,
		AllowedAccounts:    keyAllowedAccounts(r),
	}
	if !compaction && s.opts.WebTools != nil && s.opts.WebTools.Active() {
		if stream {
			if handled := s.serveResponsesWithWebToolsStream(w, r, provider, chatRequest, translated, requestedModel, upstreamModel, started); handled {
				return
			}
		} else if handled := s.serveResponsesWithWebTools(w, r, provider, chatRequest, translated, requestedModel, upstreamModel, started); handled {
			return
		}
	}
	upstream, openErr := provider.OpenChat(r.Context(), chatRequest)
	if openErr != nil {
		if upstream != nil {
			_ = upstream.Close()
		}
		s.observeResponseFailureDetail(r, requestedModel, stream, failureFromError(openErr), provider.Realm())
		writeFailure(w, openErr)
		return
	}
	if upstream == nil {
		failure := localFailure(http.StatusBadGateway, "upstream returned no chat stream")
		s.observeResponseFailureDetail(r, requestedModel, stream, failure, provider.Realm())
		writeFailure(w, failure)
		return
	}
	tracked := newUsageTracker(withAttribution(upstream, provider.Realm()), started)
	defer func() { _ = tracked.Close() }()

	if compaction {
		s.writeResponsesCompaction(w, r, tracked, requestedModel, upstreamModel, translated.Meta)
		return
	}
	if stream {
		s.serveResponsesStream(w, r, tracked, requestedModel, upstreamModel, translated.Meta)
		return
	}
	chat, err := aggregateChatStream(tracked, upstreamModel)
	if err != nil {
		failure := failureFromError(err)
		observation := ProtocolObservation{
			Protocol: "responses", Model: upstreamModel, RequestedModel: requestedModel,
			Realm: provider.Realm(), Effort: translated.Meta.Effort,
			Stream: false, Outcome: "failed", Status: failure.Status,
			Error: true, Message: failure.Message,
		}
		applyTracker(&observation, tracked)
		s.observeProtocol(r, observation)
		writeFailure(w, err)
		return
	}
	response := chatToResponse(chat, requestedModel, translated.Meta)
	if reasoningOnlyResponse(response) {
		observation := ProtocolObservation{
			Protocol: "responses", Model: upstreamModel, RequestedModel: requestedModel,
			Realm: provider.Realm(), Effort: translated.Meta.Effort,
			Stream: false, Outcome: "failed", Status: http.StatusBadGateway,
			Error: true, Message: "upstream returned reasoning without output text; retry",
			Usage: usageMap(chat["usage"]),
		}
		applyTracker(&observation, tracked)
		s.observeProtocol(r, observation)
		writeFailure(w, &apiFailure{Status: http.StatusBadGateway, Type: "server_error",
			Message: "upstream returned reasoning without output text; retry"})
		return
	}
	observation := ProtocolObservation{
		Protocol: "responses", Model: upstreamModel, RequestedModel: requestedModel,
		Realm: provider.Realm(), Effort: translated.Meta.Effort,
		Stream: false, Outcome: "completed", Status: http.StatusOK,
		Usage: usageMap(chat["usage"]),
	}
	applyTracker(&observation, tracked)
	s.observeProtocol(r, observation)
	writeJSONResponse(w, http.StatusOK, response)
}

func (s *Server) observeResponseFailure(r *http.Request, model string, stream bool, status int, realms ...string) {
	s.observeResponseFailureDetail(r, model, stream, &apiFailure{Status: status, Message: "request failed"}, firstRealm(realms))
}

func (s *Server) observeResponseFailureDetail(r *http.Request, model string, stream bool, failure *apiFailure, realm string) {
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
		Protocol: "responses", Model: model, RequestedModel: model, Realm: realm,
		Stream: stream, Outcome: "failed", Status: status, Error: true, Message: message,
	})
}

func firstRealm(realms []string) string {
	if len(realms) > 0 {
		return realms[0]
	}
	return ""
}

func rawObjectHasCompactionTrigger(payload map[string]json.RawMessage) bool {
	if raw, ok := payload["input"]; ok {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&value) == nil {
			return hasCompactionTrigger(map[string]any{"input": value})
		}
	}
	return false
}

// ----- shared HTTP/JSON helpers -----

func parseProtocolObject(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, *apiFailure) {
	if r == nil || r.Body == nil {
		return nil, localFailure(http.StatusBadRequest, "invalid JSON body")
	}
	body := http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	decoder := json.NewDecoder(body)
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
	if err := decoder.Decode(&trailing); err == nil || !errors.Is(err, io.EOF) {
		return nil, localFailure(http.StatusBadRequest, "invalid JSON body")
	}
	return payload, nil
}

func decodeRawObject(payload map[string]json.RawMessage) (map[string]any, error) {
	if payload == nil {
		return nil, errors.New("expected a JSON object")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("expected a JSON object")
	}
	return object, nil
}

func decodeJSONBool(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 {
		return false, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false, false
	}
	return boolValue(value), true
}

func chatMapToRaw(chat map[string]any) (map[string]json.RawMessage, error) {
	raw := make(map[string]json.RawMessage, len(chat))
	for key, value := range chat {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		raw[key] = encoded
	}
	return raw, nil
}

func chatMessagesRaw(chat map[string]any) []json.RawMessage {
	messages, _ := chat["messages"].([]any)
	out := make([]json.RawMessage, 0, len(messages))
	for _, message := range messages {
		encoded, err := json.Marshal(message)
		if err == nil {
			out = append(out, encoded)
		}
	}
	return out
}

func usageMap(value any) map[string]any {
	usage, _ := value.(map[string]any)
	return usage
}

func nonNilSlice(value []any) []any {
	if value == nil {
		return []any{}
	}
	return value
}

func stringValue(value any) string {
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
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return ""
	}
}

func compactJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func envValue(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}
