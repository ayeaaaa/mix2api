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
