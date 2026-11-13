package raccoon

import (
	"encoding/json"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestChat1017ToolChoiceNoneKeepsDeclarations(t *testing.T) {
	raw := map[string]json.RawMessage{
		"model":       json.RawMessage(`"sn-deepseek-v4-1-flash-1017"`),
		"messages":    json.RawMessage(`[{"role":"user","content":"do the thing"}]`),
		"tools":       json.RawMessage(`[{"type":"function","function":{"name":"run_shell","parameters":{"type":"object"}}}]`),
		"tool_choice": json.RawMessage(`"none"`),
	}
	body, model, err := buildChatBody(providers.ChatRequest{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if string(body["tools"]) != string(raw["tools"]) || string(body["tool_choice"]) != `"none"` || model != "sn-deepseek-v4-1-flash-1017" {
		t.Fatalf("Raccoon 1017 tool declarations/choice/model lost: %v", body)
	}
}

func TestChat1017LegacyFunctionsSurviveNone(t *testing.T) {
	raw := map[string]json.RawMessage{"model": json.RawMessage(`"sn-deepseek-v4-1-flash-1017"`), "messages": json.RawMessage(`[{"role":"user","content":"do the thing"}]`), "functions": json.RawMessage(`[{"name":"run_shell","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}]`), "tool_choice": json.RawMessage(`{"type":"none"}`)}
	body, _, err := buildChatBody(providers.ChatRequest{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	var tools []map[string]any
	if err := json.Unmarshal(body["tools"], &tools); err != nil || len(tools) != 1 || string(body["tool_choice"]) != `"none"` {
		t.Fatalf("legacy tools/none lost: %v %v", body, err)
	}
	if tools[0]["function"].(map[string]any)["name"] != "run_shell" {
		t.Fatalf("function name lost: %v", tools)
	}
	if _, exists := body["functions"]; exists {
		t.Fatal("legacy alias leaked upstream")
	}
}

func TestChat1017LegacyFunctionSelection(t *testing.T) {
	for _, tc := range []struct{ legacy, modern, want string }{
		{`"none"`, "", `"none"`}, {`"auto"`, "", `"auto"`}, {`"none"`, `"auto"`, `"auto"`}, {`{"name":"run_shell"}`, "", `{"function":{"name":"run_shell"},"type":"function"}`},
	} {
		raw := map[string]json.RawMessage{"model": json.RawMessage(`"sn-deepseek-v4-1-flash-1017"`), "messages": json.RawMessage(`[{"role":"user","content":"hi"}]`), "functions": json.RawMessage(`[{"name":"run_shell","parameters":{"type":"object"}}]`), "function_call": json.RawMessage(tc.legacy)}
		if tc.modern != "" {
			raw["tool_choice"] = json.RawMessage(tc.modern)
		}
		body, _, err := buildChatBody(providers.ChatRequest{Raw: raw})
		if err != nil {
			t.Fatal(err)
		}
		if string(body["tool_choice"]) != tc.want || len(body["tools"]) == 0 {
			t.Fatalf("legacy=%s modern=%s: selection/schema lost: %v", tc.legacy, tc.modern, body)
		}
	}
}
