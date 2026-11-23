package providers

import (
	"encoding/json"
	"strings"
	"testing"
)

func sessionRaw(t *testing.T, values ...any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		out = append(out, encoded)
	}
	return out
}

func TestDerivePrefixSessionKey(t *testing.T) {
	system := map[string]any{"role": "system", "content": "sys"}
	firstUser := map[string]any{"role": "user", "content": "first"}
	otherUser := map[string]any{"role": "user", "content": "other"}
	assistant := map[string]any{"role": "assistant", "content": "answer"}
	base := sessionRaw(t, system, firstUser)

	key := DerivePrefixSessionKey(base, false)
	if !strings.HasPrefix(key, "pfx-") || len(key) != 20 {
		t.Fatalf("key = %q, want pfx- plus 16 hex chars", key)
	}
	if again := DerivePrefixSessionKey(base, false); again != key {
		t.Fatalf("same head produced different keys: %q vs %q", key, again)
	}
	if other := DerivePrefixSessionKey(sessionRaw(t, system, otherUser), false); other == key {
		t.Fatal("different first user produced the same key")
	}

	// The default (v1-compatible) head is the first two messages; a third
	// assistant turn only joins with stable=true.
	grown := sessionRaw(t, system, firstUser, assistant)
	if got := DerivePrefixSessionKey(grown, false); got != key {
		t.Fatalf("default key changed with a third message: %q vs %q", got, key)
	}
	if got := DerivePrefixSessionKey(grown, true); got == key {
		t.Fatal("stable key did not include the first assistant turn")
	}
	if got := DerivePrefixSessionKey(grown, true); got != DerivePrefixSessionKey(grown, true) {
		t.Fatal("stable key is not deterministic")
	}

	// Reordered JSON object keys must not change the conversation key.
	reordered := sessionRaw(t, map[string]any{"content": "sys", "role": "system"}, firstUser)
	if got := DerivePrefixSessionKey(reordered, false); got != key {
		t.Fatalf("object key order changed the key: %q vs %q", got, key)
	}

	if got := DerivePrefixSessionKey(nil, false); got != "" {
		t.Fatalf("empty messages key = %q, want empty", got)
	}
}

func TestChatRequestMessagesFallsBackToRaw(t *testing.T) {
	values := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "first"},
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	req := ChatRequest{Raw: map[string]json.RawMessage{"messages": encoded}}
	messages := ChatRequestMessages(req)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(messages))
	}
	if got := DerivePrefixSessionKey(messages, false); !strings.HasPrefix(got, "pfx-") {
		t.Fatalf("derived key = %q", got)
	}

	if got := ChatRequestMessages(ChatRequest{Messages: sessionRaw(t, values...)}); len(got) != 2 {
		t.Fatalf("explicit messages = %d, want 2", len(got))
	}
	if got := ChatRequestMessages(ChatRequest{}); got != nil {
		t.Fatalf("empty request messages = %#v, want nil", got)
	}
}
