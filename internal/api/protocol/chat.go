// Package protocol contains the provider-neutral wire parsers shared by the
// Chat Completions, OpenAI Responses, and Anthropic Messages adapters.
package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// Chunk is one chat-completions SSE frame. Valid reports whether Data decoded
// as a JSON object; invalid frames are retained so callers that forward
// provider bytes verbatim can do so without reparsing them.
type Chunk struct {
	Data   string
	Done   bool
	Valid  bool
	Fields map[string]any

	ID          string
	Object      string
	Model       string
	Created     int64
	Choices     []Choice
	ChoiceCount int
	Usage       map[string]any
}

// Choice is one element of a chat chunk's choices array.
type Choice struct {
	Index        int
	Delta        Delta
	Message      Delta
	FinishReason string
	Raw          map[string]any
}

// Delta is a chat delta or message body. Content is only populated for string
// content; structured multimodal content stays in Raw.
type Delta struct {
	Role             string
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	FunctionCall     *FunctionCall
	Raw              map[string]any
}

// ToolCall is one OpenAI tool call. Name and Arguments are already joined by
// the decoder; the wire may split them across frames.
type ToolCall struct {
	Index     int
	HasIndex  bool
	ID        string
	Type      string
	Name      string
	Arguments string
}

// FunctionCall is the legacy single-call shape.
type FunctionCall struct {
	Name      string
	Arguments string
}

// Payload joins the data lines of one complete SSE frame, mirroring the v2
// Chat parser: comments and non-data fields are ignored.
func Payload(frame []byte) string {
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

// ParseFrame decodes one SSE frame. Parse never fails: malformed or non-data
// frames return a zero Chunk with Valid=false.
func ParseFrame(frame []byte) Chunk {
	chunk := Chunk{}
	data := Payload(frame)
	chunk.Data = data
	if strings.TrimSpace(data) == "" {
		return chunk
	}
	if strings.TrimSpace(data) == "[DONE]" {
		chunk.Done = true
		chunk.Valid = true
		return chunk
	}

	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return chunk
	}
	chunk.Valid = true
	chunk.Fields = fields
	chunk.ID, _ = fields["id"].(string)
	chunk.Object, _ = fields["object"].(string)
	chunk.Model, _ = fields["model"].(string)
	chunk.Created, _ = Int64(fields["created"])
	chunk.Usage, _ = fields["usage"].(map[string]any)
	if rawChoices, ok := fields["choices"].([]any); ok {
		chunk.ChoiceCount = len(rawChoices)
	}
	chunk.Choices = ParseChoices(fields["choices"])
	return chunk
}

// ParseChoices decodes the choices array shared by streaming and aggregated
// chat objects.
func ParseChoices(value any) []Choice {
	rawChoices, ok := value.([]any)
	if !ok {
		return nil
	}
	choices := make([]Choice, 0, len(rawChoices))
	for _, rawChoice := range rawChoices {
		choiceMap, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		choice := Choice{Raw: choiceMap}
		choice.Index, _ = Int(choiceMap["index"])
		choice.FinishReason, _ = choiceMap["finish_reason"].(string)
		if delta, ok := choiceMap["delta"].(map[string]any); ok {
			choice.Delta = ParseDelta(delta)
		}
		if message, ok := choiceMap["message"].(map[string]any); ok {
			choice.Message = ParseDelta(message)
		}
		choices = append(choices, choice)
	}
	return choices
}

// ParseDelta decodes one chat delta/message object.
func ParseDelta(value map[string]any) Delta {
	delta := Delta{Raw: value}
	if value == nil {
		return delta
	}
	delta.Role, _ = value["role"].(string)
	delta.Content, _ = value["content"].(string)
	delta.ReasoningContent, _ = value["reasoning_content"].(string)
	if rawCalls, ok := value["tool_calls"].([]any); ok {
		delta.ToolCalls = make([]ToolCall, 0, len(rawCalls))
		for _, rawCall := range rawCalls {
			callMap, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			call := ToolCall{}
			call.Index, call.HasIndex = Int(callMap["index"])
			call.ID, _ = callMap["id"].(string)
			call.Type, _ = callMap["type"].(string)
			if function, ok := callMap["function"].(map[string]any); ok {
				call.Name, _ = function["name"].(string)
				call.Arguments, _ = function["arguments"].(string)
			}
			delta.ToolCalls = append(delta.ToolCalls, call)
		}
	}
	if function, ok := value["function_call"].(map[string]any); ok {
		delta.FunctionCall = &FunctionCall{}
		delta.FunctionCall.Name, _ = function["name"].(string)
		delta.FunctionCall.Arguments, _ = function["arguments"].(string)
	}
	return delta
}

// Reader streams decoded chunks from a providers.ChatStream. It skips empty
// and malformed frames (the v1 translation pipeline did the same) but returns
// [DONE] as Chunk.Done so callers can finish and still read post-DONE usage
// frames when they need them.
type Reader struct {
	stream providers.ChatStream
}

// NewReader wraps a provider chat stream.
func NewReader(stream providers.ChatStream) *Reader {
	return &Reader{stream: stream}
}

// Next returns the next decoded chunk. Blank/comment/malformed frames are
// skipped. io.EOF is returned only when the upstream stream itself ends.
func (r *Reader) Next() (Chunk, error) {
	if r == nil || r.stream == nil {
		return Chunk{}, io.EOF
	}
	for {
		frame, err := r.stream.Next()
		if err != nil {
			return Chunk{}, err
		}
		if len(frame) == 0 {
			continue
		}
		chunk := ParseFrame(frame)
		if chunk.Data == "" {
			continue
		}
		if chunk.Done {
			return chunk, nil
		}
		if !chunk.Valid {
			continue
		}
		return chunk, nil
	}
}

// Close closes the underlying stream once.
func (r *Reader) Close() error {
	if r == nil || r.stream == nil {
		return nil
	}
	return r.stream.Close()
}

// Int converts a JSON number/string value to int.
func Int(value any) (int, bool) {
	number, ok := Int64(value)
	return int(number), ok
}

// Int64 converts a JSON number/string value to int64.
func Int64(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		return number, err == nil
	case float64:
		return int64(typed), true
	case float32:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case string:
		var number json.Number = json.Number(strings.TrimSpace(typed))
		parsed, err := number.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

// Float converts a JSON number/string value to float64.
func Float(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case string:
		var number json.Number = json.Number(strings.TrimSpace(typed))
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}
