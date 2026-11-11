package workbuddy

import (
	"strings"
	"testing"
)

func TestSanitizeBillingHeaderLine(t *testing.T) {
	full := "x-anthropic-billing-header: cc_version=2.1.37; cc_entrypoint=cli; cch=abc;\nYou are helpful."
	out := sanitizeText(full, true, true)
	if strings.Contains(strings.ToLower(out), "x-anthropic-billing") || strings.Contains(out, "cch=") {
		t.Fatalf("billing header leaked: %q", out)
	}
	if out != "You are helpful." {
		t.Fatalf("surrounding text = %q", out)
	}
	short := sanitizeText("x-anthropic-billing-hdr: cch=abc;\nKeep me.", true, true)
	if strings.Contains(strings.ToLower(short), "x-anthropic-billing") || !strings.Contains(short, "Keep me.") {
		t.Fatalf("short billing header = %q", short)
	}
}

func TestSanitizeDropsHeaderOnlySystemMessage(t *testing.T) {
	messages := sanitizeChatMessages([]any{
		map[string]any{"role": "system", "content": "x-anthropic-billing-header: cch=abc;"},
		map[string]any{"role": "user", "content": "hi"},
	})
	if len(messages) != 1 {
		t.Fatalf("header-only system message survived: %#v", messages)
	}
	if creditText(messages[0].(map[string]any)["role"]) != "user" {
		t.Fatalf("remaining message = %#v", messages)
	}
}

func TestSanitizeClientIdentityStrings(t *testing.T) {
	for _, input := range []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are a coding agent running in the Codex CLI.",
		"You are an AI agent powered by DeepSeek Harness.",
	} {
		if out := sanitizeText(input, true, true); !strings.Contains(out, "You are a helpful AI assistant.") {
			t.Fatalf("identity %q -> %q", input, out)
		}
	}
	// User content is data, not a client fingerprint.
	user := "You are an AI agent powered by DeepSeek Harness."
	out := sanitizeChatMessages([]any{map[string]any{"role": "user", "content": user}})
	if got := creditText(out[0].(map[string]any)["content"]); got != user {
		t.Fatalf("user content was rewritten: %q", got)
	}
}

func TestSanitizeDSHAndChannelPhrases(t *testing.T) {
	gui := "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	if out := sanitizeText(gui, true, true); !strings.Contains(out, "whose URL is named in the runtime context") ||
		strings.Contains(out, "DeepSeek Harness") {
		t.Fatalf("GUI rewrite = %q", out)
	}
	fileRef := "Tokens prefixed with @ are paths the user explicitly referenced."
	if out := sanitizeText(fileRef, true, true); !strings.Contains(out, "paths that the user") {
		t.Fatalf("file-ref rewrite = %q", out)
	}
	if out := sanitizeText("code 11128", false, true); out != "code 11128" {
		t.Fatalf("11128 body text was rewritten: %q", out)
	}
	if out := sanitizeText("Sisyphus-Junior - Focused executor from OhMyOpenCode", false, true); out != "Sisyphus-Junior - Focused executor" {
		t.Fatalf("OMO rewrite = %q", out)
	}
}

func TestSanitizeToolsAndBillingScope(t *testing.T) {
	header := "x-anthropic-billing-header: cch=abc;"
	assistant := sanitizeChatMessages([]any{
		map[string]any{"role": "assistant", "content": header},
		map[string]any{"role": "user", "content": header},
	})
	if len(assistant) != 2 || !strings.Contains(creditText(assistant[0].(map[string]any)["content"]), "x-anthropic-billing") {
		t.Fatalf("assistant billing text was removed: %#v", assistant)
	}
	system := sanitizeChatMessages([]any{
		map[string]any{"role": "system", "content": header},
		map[string]any{"role": "user", "content": "hi"},
	})
	if len(system) != 1 || creditText(system[0].(map[string]any)["role"]) != "user" {
		t.Fatalf("header-only system message survived: %#v", system)
	}
	tools := sanitizeTools([]any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "run",
			"description": "You are Claude Code, Anthropic's official CLI for Claude.",
		},
	}})
	desc := tools.([]any)[0].(map[string]any)["function"].(map[string]any)["description"]
	if desc != "You are a helpful AI assistant." {
		t.Fatalf("tool description = %q", desc)
	}
	if out := sanitizeText("mention x-anthropic-billing-header here", false, true); strings.Contains(out, "x-anthropic-billing-header") {
		t.Fatalf("bare billing key = %q", out)
	}
}

func TestSanitizeToolArguments(t *testing.T) {
	messages := sanitizeChatMessages([]any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{
					"id": "call-1", "type": "function",
					"function": map[string]any{
						"name":      "run",
						"arguments": `{"query":"x-anthropic-billing-header: cch=abc;"}`,
					},
				},
				map[string]any{
					"id": "call-2", "type": "function",
					"function": map[string]any{"name": "nilArgs", "arguments": nil},
				},
			},
		},
	})
	calls := messages[0].(map[string]any)["tool_calls"].([]any)
	first := calls[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if !strings.Contains(first, "x-anthropic-billing") {
		t.Fatalf("tool argument data was rewritten: %q", first)
	}
	second := calls[1].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if second != "{}" {
		t.Fatalf("nil arguments = %q", second)
	}
}
