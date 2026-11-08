// Package contract contains provider-neutral normalization helpers and the
// golden contract tests used to compare the Go v2 API with the v1.6.5 oracle.
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Stable placeholders for values that legitimately change between two runs of
// the same contract. Goldens use the literal strings so normalized API output
// and hand-recorded v1.6.5 output can be compared directly.
const (
	DynamicValue   = "<dynamic>"
	TimestampValue = "<timestamp>"
	UUIDValue      = "<uuid>"
)

var uuidPattern = regexp.MustCompile(
	`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
)

// NormalizeJSON decodes one JSON document with json.Number precision and
// recursively replaces fields that are expected to differ between runs:
//
//   - transport metadata such as Date and Server is removed;
//   - timestamp keys keep their shape but their value becomes
//     TimestampValue;
//   - UUID values become UUIDValue, except in account/nickname content;
//   - generated chatcmpl/resp/session/request/trace/call IDs become
//     DynamicValue, detected by prefix or UUID rather than by the bare key
//     name, so semantic model IDs (raccoon-090dd3, sn-*) survive.
//
// Numbers remain json.Number and nested objects/arrays keep their structure.
func NormalizeJSON(data []byte) (any, error) {
	value, err := decodeJSONValue(data)
	if err != nil {
		return nil, fmt.Errorf("contract: normalize JSON: %w", err)
	}
	return normalizeJSONValue(value, "", normalizeContext{}), nil
}

// normalizeContext carries just enough parent information to distinguish a
// semantic identity (a model or account ID) from an opaque generated ID. It is
// deliberately not a generic "normalize every UUID" switch.
type normalizeContext struct {
	semanticID bool
}

func decodeJSONValue(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("contract: unexpected trailing JSON")
	}
	return value, nil
}

func normalizeJSONValue(value any, key string, parent normalizeContext) any {
	switch typed := value.(type) {
	case map[string]any:
		context := parent
		if isSemanticIdentityMap(typed) {
			context.semanticID = true
		}
		out := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			if isDroppedDynamicKey(childKey) {
				continue
			}
			if isTimestampKey(childKey) {
				out[childKey] = normalizeTimestampValue(childValue)
				continue
			}
			childContext := context
			if isSemanticCollectionKey(childKey) {
				childContext.semanticID = true
			}
			out[childKey] = normalizeJSONValue(childValue, childKey, childContext)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = normalizeJSONValue(item, "", parent)
		}
		return out
	case string:
		return normalizeStringValue(typed, key, parent)
	default:
		return value
	}
}

// isSemanticIdentityMap marks model/account-shaped objects. Identity values in
// these contexts are semantic even when they coincide with a UUID shape.
func isSemanticIdentityMap(value map[string]any) bool {
	if object, _ := value["object"].(string); strings.EqualFold(strings.TrimSpace(object), "model") {
		return true
	}
	for _, key := range []string{"uid", "account_id", "nickname"} {
		if _, ok := value[key]; ok {
			return true
		}
	}
	return false
}

func isSemanticCollectionKey(key string) bool {
	switch canonicalKey(key) {
	case "models", "accounts", "account":
		return true
	default:
		return false
	}
}

// droppedDynamicKeys are metadata-only fields whose values carry no contract
// meaning. They are removed rather than replaced so a golden cannot mistake a
// normalized placeholder for a real field.
var droppedDynamicKeys = func() map[string]bool {
	keys := []string{
		"date", "server",
		"request_id", "requestid", "req_id", "request-id",
		"x_request_id", "x-request-id", "xrequestid",
		"session_id", "sessionid", "sess_id", "session-id",
		"x_session_id", "x-session-id", "xsessionid",
		"conversation_id", "conversation-id", "chat_id",
		"trace_id", "traceid", "traceparent", "span_id", "spanid",
		"x_trace_id", "x-trace-id", "xtraceid",
		"response_id", "responseid", "completion_id", "completionid",
		"message_id", "messageid", "cf_ray", "cf-ray", "cfray",
	}
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		out[canonicalKey(key)] = true
	}
	return out
}()

// contentKeys identify account, nickname, and model content that must survive
// normalization even when a value looks like a UUID.
var contentKeys = func() map[string]bool {
	keys := []string{
		"uid", "account_id", "user_id", "owner_id", "org_id",
		"office_identity", "office_org_name", "nickname", "name",
		"display_name", "model", "model_id", "email", "note", "user",
	}
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		out[canonicalKey(key)] = true
	}
	return out
}()

func canonicalKey(key string) string {
	lower := strings.ToLower(strings.TrimSpace(key))
	replacer := strings.NewReplacer("_", "", "-", "")
	return replacer.Replace(lower)
}

func isDroppedDynamicKey(key string) bool {
	return droppedDynamicKeys[canonicalKey(key)]
}

func preservesContentKey(key string) bool {
	return contentKeys[canonicalKey(key)]
}

// isDynamicIDKey limits prefix-based replacement to identifier fields. The
// value must still look generated; a bare key named "id" alone is not enough.
func isDynamicIDKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "id" {
		return true
	}
	switch canonicalKey(key) {
	case "requestid", "sessionid", "conversationid", "chatid", "traceid",
		"spanid", "responseid", "completionid", "messageid", "eventid",
		"runid", "callid", "toolcallid", "toolid":
		return true
	}
	return strings.HasSuffix(lower, "_id") && !preservesContentKey(key)
}

var dynamicIDPrefixes = []string{
	"chatcmpl-", "chatcmpl_",
	"resp_", "resp-",
	"request_", "request-", "req_", "req-",
	"trace_", "trace-",
	"session_", "session-", "sess_", "sess-",
	"conversation_", "conversation-",
	"call_", "call-",
	"run_", "run-",
	"msg_", "msg-",
	"item_", "item-",
	"evt_", "evt-",
	"toolu_", "toolu-",
	"cmpl_", "cmpl-",
}

func normalizeStringValue(value, key string, context normalizeContext) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value
	}
	if context.semanticID && isIdentityFieldKey(key) {
		return value
	}
	if uuidPattern.MatchString(trimmed) {
		if !isUUIDDynamicKey(key, context) || preservesContentKey(key) {
			return value
		}
		return UUIDValue
	}
	if !context.semanticID && isDynamicIDKey(key) {
		lower := strings.ToLower(trimmed)
		for _, prefix := range dynamicIDPrefixes {
			if strings.HasPrefix(lower, prefix) {
				return DynamicValue
			}
		}
	}
	return value
}

// isUUIDDynamicKey limits UUID replacement to identifiers whose key/context is
// known to be generated. A UUID-shaped model or account ID is semantic and must
// survive, while request/session/trace/chat UUIDs are normalized.
func isUUIDDynamicKey(key string, context normalizeContext) bool {
	if context.semanticID && isIdentityFieldKey(key) {
		return false
	}
	if preservesContentKey(key) {
		return false
	}
	canonical := canonicalKey(key)
	if canonical == "uuid" || strings.HasSuffix(canonical, "uuid") ||
		strings.HasSuffix(canonical, "guid") {
		return true
	}
	return isDynamicIDKey(key)
}

func isIdentityFieldKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "id" || canonicalKey(key) == "modelid" || canonicalKey(key) == "uid" {
		return true
	}
	return strings.HasSuffix(lower, "_id")
}

// isTimestampKey recognizes wire timestamp spellings. It deliberately does not
// treat every *_id or arbitrary string as time so semantic fields keep their
// values.
func isTimestampKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	switch canonicalKey(key) {
	case "created", "createdat", "timestamp", "time", "datetime", "servertime",
		"requesttime", "updated", "updatedat", "modified", "started",
		"startedat", "finished", "finishedat", "deletedat", "requestedat",
		"completedat", "expires", "expiresat", "fetchedat", "retryafter",
		"reset", "notbefore", "lastseen", "lastseenat", "cooluntil", "until":
		return true
	}
	for _, suffix := range []string{
		"_at", "_time", "_timestamp", "_date", "_datetime", "_until",
	} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func normalizeTimestampValue(value any) any {
	switch typed := value.(type) {
	case json.Number, float32, float64, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return TimestampValue
	case string:
		if looksLikeTimestamp(typed) {
			return TimestampValue
		}
		return value
	default:
		return value
	}
}

func looksLikeTimestamp(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return true
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05-0700",
		"2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05.999999999",
		time.RFC1123Z,
		time.RFC1123,
		time.RFC850,
		time.ANSIC,
		time.UnixDate,
		time.RubyDate,
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if _, err := time.Parse(layout, trimmed); err == nil {
			return true
		}
	}
	return false
}

// ToolCall is one merged OpenAI streaming tool call. Arguments are the
// concatenation of every fragment for the same index.
type ToolCall struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Type      string `json:"type,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Event is the provider-neutral semantic form of an SSE stream. A chat
// completion stream is folded into one Event, so frame/chunk boundaries,
// comments, keepalives, and generated ids have no effect on a golden.
//
// Type is the first explicit event: name, or the first JSON object value when
// the stream has no event: field (OpenAI chat chunks therefore default to
// "chat.completion.chunk"). Types and Objects retain first-seen order.
type Event struct {
	Type             string         `json:"type,omitempty"`
	Types            []string       `json:"types,omitempty"`
	Objects          []string       `json:"objects,omitempty"`
	Role             string         `json:"role,omitempty"`
	FinishReason     string         `json:"finish_reason,omitempty"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
	Usage            map[string]any `json:"usage,omitempty"`
	Done             bool           `json:"done,omitempty"`
	// Data carries a non-chat payload (for example a Responses API event or a
	// non-JSON SSE payload) normalized with the same dynamic-field rules.
	Data any `json:"data,omitempty"`
}

// NormalizeSSE parses SSE frames, ignores comments/keepalives and chunk
// boundaries, merges OpenAI chat deltas, and records the [DONE] marker. It does
// not depend on any Raccoon-specific fields.
func NormalizeSSE(data []byte) ([]Event, error) {
	aggregate := newStreamAggregate()
	generic := make([]Event, 0)

	for _, frame := range splitSSEFrames(data) {
		payload := strings.Join(frame.data, "\n")
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" {
			continue
		}
		if frame.event != "" {
			aggregate.noteType(frame.event)
		}
		if trimmed == "[DONE]" {
			aggregate.sawStream = true
			aggregate.done = true
			continue
		}

		value, err := decodeJSONValue([]byte(payload))
		if err != nil {
			generic = append(generic, Event{Type: frame.event, Data: payload})
			continue
		}
		object, ok := value.(map[string]any)
		if !ok {
			generic = append(generic, Event{
				Type: frame.event,
				Data: normalizeJSONValue(value, "", normalizeContext{}),
			})
			continue
		}

		objectName, _ := object["object"].(string)
		choices, _ := object["choices"].([]any)
		usage, _ := object["usage"].(map[string]any)
		isChat := (objectName != "" && strings.HasPrefix(objectName, "chat.completion")) ||
			len(choices) > 0 || len(usage) > 0
		if isChat {
			if objectName != "" {
				aggregate.noteObject(objectName)
			}
			aggregate.absorbChat(object)
			continue
		}

		normalized := normalizeJSONValue(object, "", normalizeContext{})
		if _, hasError := object["error"]; hasError && frame.event == "" {
			generic = append(generic, Event{Type: "error", Data: normalized})
			continue
		}
		generic = append(generic, Event{Type: frame.event, Data: normalized})
	}

	events := make([]Event, 0, 1+len(generic))
	if aggregate.sawStream {
		events = append(events, aggregate.event())
	}
	events = append(events, generic...)
	return events, nil
}

type sseFrame struct {
	event string
	data  []string
}

// splitSSEFrames implements the SSE field/blank-line grammar. CRLF and bare CR
// are accepted so transport chunking cannot affect the semantic result.
func splitSSEFrames(data []byte) []sseFrame {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	frames := make([]sseFrame, 0)
	current := sseFrame{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			if len(current.data) > 0 {
				frames = append(frames, current)
			}
			current = sseFrame{}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field = line
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "data":
			current.data = append(current.data, value)
		case "event":
			current.event = value
		}
	}
	if len(current.data) > 0 {
		frames = append(frames, current)
	}
	return frames
}

type streamAggregate struct {
	sawStream bool
	done      bool

	types     []string
	typeSet   map[string]bool
	objects   []string
	objectSet map[string]bool

	role             string
	finishReason     string
	content          strings.Builder
	reasoningContent strings.Builder
	toolCalls        map[int]*ToolCall
	usage            map[string]any
}

func newStreamAggregate() *streamAggregate {
	return &streamAggregate{
		typeSet:   make(map[string]bool),
		objectSet: make(map[string]bool),
		toolCalls: make(map[int]*ToolCall),
	}
}

func (a *streamAggregate) noteType(eventName string) {
	eventName = strings.TrimSpace(eventName)
	if eventName == "" || a.typeSet[eventName] {
		return
	}
	a.typeSet[eventName] = true
	a.types = append(a.types, eventName)
}

func (a *streamAggregate) noteObject(objectName string) {
	objectName = strings.TrimSpace(objectName)
	if objectName == "" || a.objectSet[objectName] {
		return
	}
	a.objectSet[objectName] = true
	a.objects = append(a.objects, objectName)
}

func (a *streamAggregate) absorbChat(object map[string]any) {
	a.sawStream = true

	if choices, ok := object["choices"].([]any); ok {
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
				a.absorbDelta(delta)
			}
			if finish, ok := choice["finish_reason"].(string); ok &&
				strings.TrimSpace(finish) != "" {
				a.finishReason = finish
			}
		}
	}

	if usage, ok := object["usage"].(map[string]any); ok && len(usage) > 0 {
		if normalized, ok := normalizeJSONValue(usage, "", normalizeContext{}).(map[string]any); ok {
			a.usage = normalized
		}
	}
}

func (a *streamAggregate) absorbDelta(delta map[string]any) {
	if role, ok := delta["role"].(string); ok && strings.TrimSpace(role) != "" {
		a.role = role
	}
	if piece, ok := delta["content"].(string); ok && piece != "" {
		a.content.WriteString(piece)
	}
	if piece, ok := delta["reasoning_content"].(string); ok && piece != "" {
		a.reasoningContent.WriteString(piece)
	}
	a.absorbToolCalls(delta["tool_calls"])
	if functionCall, ok := delta["function_call"].(map[string]any); ok {
		a.absorbLegacyFunctionCall(functionCall)
	}
}

func (a *streamAggregate) absorbToolCalls(value any) {
	calls, ok := value.([]any)
	if !ok {
		return
	}
	for position, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		index, ok := sseInt(call["index"])
		if !ok || index < 0 {
			index = position
		}
		entry := a.toolCalls[index]
		if entry == nil {
			entry = &ToolCall{Index: index}
			a.toolCalls[index] = entry
		}
		if id, ok := call["id"].(string); ok && strings.TrimSpace(id) != "" {
			entry.ID = normalizeStringValue(id, "id", normalizeContext{})
		}
		if kind, ok := call["type"].(string); ok && strings.TrimSpace(kind) != "" {
			entry.Type = kind
		}
		if function, ok := call["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok && name != "" {
				entry.Name += name
			}
			if arguments, ok := function["arguments"].(string); ok && arguments != "" {
				entry.Arguments += arguments
			}
		}
	}
}

func (a *streamAggregate) absorbLegacyFunctionCall(function map[string]any) {
	name, hasName := function["name"].(string)
	arguments, hasArguments := function["arguments"].(string)
	if (!hasName || strings.TrimSpace(name) == "") && !hasArguments {
		return
	}
	entry := a.toolCalls[0]
	if entry == nil {
		entry = &ToolCall{Index: 0}
		a.toolCalls[0] = entry
	}
	if strings.TrimSpace(name) != "" && entry.Name == "" {
		entry.Name = name
	}
	if hasArguments {
		entry.Arguments += arguments
	}
}

func (a *streamAggregate) event() Event {
	event := Event{
		Role:             a.role,
		FinishReason:     a.finishReason,
		Content:          a.content.String(),
		ReasoningContent: a.reasoningContent.String(),
		Usage:            a.usage,
		Done:             a.done,
	}
	event.Types = append([]string(nil), a.types...)
	event.Objects = append([]string(nil), a.objects...)
	switch {
	case len(event.Types) > 0:
		event.Type = event.Types[0]
	case len(event.Objects) > 0:
		event.Type = event.Objects[0]
	}
	if len(a.toolCalls) > 0 {
		indexes := make([]int, 0, len(a.toolCalls))
		for index := range a.toolCalls {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		event.ToolCalls = make([]ToolCall, 0, len(indexes))
		for _, index := range indexes {
			if call := a.toolCalls[index]; call != nil {
				event.ToolCalls = append(event.ToolCalls, *call)
			}
		}
	}
	return event
}

func sseInt(value any) (int, bool) {
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
