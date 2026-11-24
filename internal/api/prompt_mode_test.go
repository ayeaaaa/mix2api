package api

import (
	"encoding/json"
	"testing"
)

func TestApplyGatewayPromptMode(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "client sys"},
		map[string]any{"role": "developer", "content": "client dev"},
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
	}

	pass := applyGatewayPromptMode(messages, "passthrough", "ignored")
	if len(pass) != 4 || pass[0].(map[string]any)["content"] != "client sys" {
		t.Fatalf("passthrough changed messages: %#v", pass)
	}

	custom := applyGatewayPromptMode(messages, "custom", "gateway rule")
	if len(custom) != 3 {
		t.Fatalf("custom len = %d, want 3: %#v", len(custom), custom)
	}
	if head := custom[0].(map[string]any); head["role"] != "system" || head["content"] != "gateway rule" {
		t.Fatalf("custom head = %#v", head)
	}
	if custom[1].(map[string]any)["content"] != "hi" || custom[2].(map[string]any)["content"] != "hello" {
		t.Fatalf("custom removed user/assistant: %#v", custom)
	}

	appended := applyGatewayPromptMode(messages, "append", "gateway rule")
	if len(appended) != 5 {
		t.Fatalf("append len = %d, want 5: %#v", len(appended), appended)
	}
	if appended[0].(map[string]any)["content"] != "client sys" || appended[1].(map[string]any)["content"] != "client dev" {
		t.Fatalf("append moved leading system block: %#v", appended)
	}
	if head := appended[2].(map[string]any); head["role"] != "system" || head["content"] != "gateway rule" {
		t.Fatalf("append inserted wrong head: %#v", head)
	}
	if appended[3].(map[string]any)["content"] != "hi" {
		t.Fatalf("append lost user message: %#v", appended)
	}
}

func TestApplyGatewayPromptModeDefaultsAndUserOnly(t *testing.T) {
	out := applyGatewayPromptMode([]any{map[string]any{"role": "user", "content": "hi"}}, "custom", "")
	if len(out) != 2 || out[0].(map[string]any)["content"] != defaultGatewayPrompt {
		t.Fatalf("default prompt missing: %#v", out)
	}

	raw := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"hi"}`),
		json.RawMessage(`{"role":"assistant","content":"ok"}`),
	}
	got, err := applyGatewayPromptModeRaw(raw, "append", "gateway rule")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("raw append len = %d", len(got))
	}
	var head map[string]any
	if err := json.Unmarshal(got[0], &head); err != nil {
		t.Fatal(err)
	}
	if head["role"] != "system" || head["content"] != "gateway rule" {
		t.Fatalf("raw head = %#v", head)
	}
}

func TestGatewayPromptConfigFailOpen(t *testing.T) {
	s := &Server{}
	if mode, text := s.gatewayPromptConfig(t.Context()); mode != "passthrough" || text != "" {
		t.Fatalf("nil settings changed config: %q %q", mode, text)
	}
}
