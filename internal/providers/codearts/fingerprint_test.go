package codearts

import (
	"strings"
	"testing"
	"time"
)

func TestIDEFingerprintIdentityShapeAndStability(t *testing.T) {
	t.Setenv("WB_CODEARTS_CLIENT_IP", "10.0.1.145")
	t.Setenv("WB_CODEARTS_IDE_USER_AGENT", "opencode/126.11.3-20261124194420")

	messages := []any{
		map[string]any{"role": "system", "content": "system"},
		map[string]any{"role": "user", "content": "hello"},
	}
	first, err := newIDEFingerprint("uid-1", "glm-5.3", "chat-1", messages)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newIDEFingerprint("uid-1", "glm-5.3", "chat-1", messages)
	if err != nil {
		t.Fatal(err)
	}

	if len(first.TraceID) != 32 || len(first.SpanID) != 16 {
		t.Fatalf("trace/span lengths = %d/%d", len(first.TraceID), len(first.SpanID))
	}
	if first.TraceID == second.TraceID || first.SpanID == second.SpanID {
		t.Fatal("trace/span should be fresh per request")
	}
	if !strings.HasPrefix(first.SessionID, "ses_") || len(first.SessionID) != 30 {
		t.Fatalf("session id = %q", first.SessionID)
	}
	if first.SessionID != second.SessionID {
		t.Fatalf("session id not stable: %q vs %q", first.SessionID, second.SessionID)
	}
	if !strings.HasPrefix(first.MessageID, "msg_") || len(first.MessageID) != 30 {
		t.Fatalf("message id = %q", first.MessageID)
	}
	if first.MessageID != second.MessageID {
		t.Fatalf("message id not stable: %q vs %q", first.MessageID, second.MessageID)
	}
	if first.CreatedAt == "" {
		t.Fatal("created time is empty")
	}
	if _, err := time.Parse(time.RFC3339, first.CreatedAt); err != nil {
		t.Fatalf("created time %q: %v", first.CreatedAt, err)
	}
	if first.ModelID != "glm-5.3" || first.ModelName != "GLM-5.3" {
		t.Fatalf("model id/name = %q/%q", first.ModelID, first.ModelName)
	}
	if first.ClientIP != "10.0.1.145" || first.UserAgent != "opencode/126.11.3-20261124194420" {
		t.Fatalf("client identity = %#v", first)
	}

	changed, err := newIDEFingerprint("uid-1", "glm-5.3", "chat-1", []any{
		map[string]any{"role": "user", "content": "different"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed.SessionID != first.SessionID {
		t.Fatalf("session changed with message: %q vs %q", changed.SessionID, first.SessionID)
	}
	if changed.MessageID == first.MessageID {
		t.Fatal("message id did not change with user text")
	}
}

func TestIDEModelNames(t *testing.T) {
	cases := map[string]string{
		"glm-5.3":          "GLM-5.3",
		"GLM-5.2":          "GLM-5.2",
		"GLM-5.1":          "GLM-5.1",
		"Qwen3-VL-235B":    "Qwen3-VL-235B",
		"some-other-model": "some-other-model",
	}
	for input, want := range cases {
		if got := ideModelName(input); got != want {
			t.Fatalf("ideModelName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIDEUserPrompt(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": "reply"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "second "},
			map[string]any{"type": "text", "text": "prompt"},
		}},
	}
	if got := ideUserPrompt(messages); got != "second prompt" {
		t.Fatalf("user prompt = %q", got)
	}
	if got := ideUserPrompt(nil); got != "" {
		t.Fatalf("empty prompt = %q", got)
	}
}
