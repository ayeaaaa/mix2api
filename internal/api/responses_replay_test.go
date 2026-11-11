package api

import (
	"strings"
	"testing"
)

func TestResponsesReplayJoinsAdjacentReasoning(t *testing.T) {
	messages := responsesInputToMessages(map[string]any{
		"input": []any{
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "first"},
				map[string]any{"type": "summary_text", "text": "second"},
			}},
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "third"},
			}},
			map[string]any{"type": "message", "role": "assistant", "content": "answer"},
		},
	})
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	message := messages[0].(map[string]any)
	if got := stringValue(message["reasoning_content"]); got != "first\nsecond\nthird" {
		t.Fatalf("reasoning = %q", got)
	}
}

func TestResponsesReplayPreservesInlineAssistantReasoningAndCalls(t *testing.T) {
	messages := responsesInputToMessages(map[string]any{
		"input": []any{
			map[string]any{
				"type": "message", "role": "assistant", "content": "hello",
				"reasoning_content": "think",
				"tool_calls": []any{map[string]any{
					"id": "call-1", "type": "function",
					"function": map[string]any{"name": "run", "arguments": "{}"},
				}},
			},
		},
	})
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	message := messages[0].(map[string]any)
	if message["reasoning_content"] != "think" {
		t.Fatalf("inline reasoning lost: %#v", message)
	}
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("inline tool calls lost: %#v", message)
	}
}

func TestResponsesReplayReasoningBeforeToolCallStartsNewAssistant(t *testing.T) {
	messages := responsesInputToMessages(map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "assistant", "content": "earlier"},
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "tool thinking"},
			}},
			map[string]any{
				"type": "function_call", "call_id": "call-1", "name": "run", "arguments": "{}",
			},
		},
	})
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	first := messages[0].(map[string]any)
	if first["content"] != "earlier" || first["reasoning_content"] != nil {
		t.Fatalf("first assistant changed: %#v", first)
	}
	second := messages[1].(map[string]any)
	if second["reasoning_content"] != "tool thinking" {
		t.Fatalf("tool reasoning lost: %#v", second)
	}
	calls, _ := second["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool call missing: %#v", second)
	}
}

func TestResponsesReplayToolOutputClearsPendingReasoning(t *testing.T) {
	messages := responsesInputToMessages(map[string]any{
		"input": []any{
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "orphan thinking"},
			}},
			map[string]any{
				"type": "function_call_output", "call_id": "", "output": "next instruction",
			},
			map[string]any{"type": "message", "role": "assistant", "content": "done"},
		},
	})
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	assistant := messages[1].(map[string]any)
	if assistant["reasoning_content"] != nil {
		t.Fatalf("orphan reasoning leaked into assistant: %#v", assistant)
	}
	if !strings.Contains(stringValue(assistant["content"]), "done") {
		t.Fatalf("assistant content = %#v", assistant)
	}
}
