package workbuddy

import (
	"encoding/json"
	"regexp"
	"testing"
)

// officialConversationRequestID is the exact assertion the CodeBuddy CLI
// 2.185.0 applies to X-Conversation-Request-ID: a lowercase UUIDv7 encoded as
// 32 hexadecimal characters.
var officialConversationRequestID = regexp.MustCompile(`^[0-9a-f]{12}7[0-9a-f]{3}[89ab][0-9a-f]{15}$`)

func TestCLIMessageIDMatchesOfficialUUIDv7Shape(t *testing.T) {
	for i := 0; i < 100; i++ {
		if id := newMessageID(); !officialConversationRequestID.MatchString(id) {
			t.Fatalf("message id %q does not match the official UUIDv7 shape", id)
		}
	}
}

func TestCLIRequestBodyExtraVars(t *testing.T) {
	body := map[string]any{
		"model":                "deepseek-v4.1-flash",
		"messages":             []any{map[string]any{"role": "user", "content": "hi"}},
		"metadata":             map[string]any{"trace": "drop"},
		"conversation_id":      "drop-me",
		"previous_response_id": "drop-me-too",
	}
	out, err := chatBodyForProduct(body, productCLI, Versions{CLI: "9.9.9"})
	if err != nil {
		t.Fatalf("chatBodyForProduct: %v", err)
	}
	if out["cache_retention"] != cliCacheRetention {
		t.Fatalf("cache_retention = %#v", out["cache_retention"])
	}
	for _, key := range cliBodyDropFields {
		if _, exists := out[key]; exists {
			t.Fatalf("%s was not dropped", key)
		}
	}
	extra, ok := out["extra_vars"].(map[string]any)
	if !ok {
		t.Fatalf("extra_vars = %#v", out["extra_vars"])
	}
	if extra["initiator"] != "user" || extra["interactionType"] != "prompt" {
		t.Fatalf("extra_vars interaction = %#v", extra)
	}
	harness, _ := extra["harnessMetadata"].(string)
	var metadata map[string]any
	if err := json.Unmarshal([]byte(harness), &metadata); err != nil {
		t.Fatalf("harnessMetadata is not JSON: %q: %v", harness, err)
	}
	if metadata["schemaVersion"] != float64(1) || metadata["harnessVersion"] != "9.9.9" ||
		metadata["stepIndex"] != float64(0) {
		t.Fatalf("harnessMetadata = %#v", metadata)
	}
	agent, _ := metadata["agent"].(map[string]any)
	if agent["depth"] != float64(0) {
		t.Fatalf("harnessMetadata agent = %#v", metadata["agent"])
	}
}

func TestCLIRequestBodyExtraVarsPreservesClientValues(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"extra_vars": map[string]any{
			"initiator":       "agent",
			"interactionType": "schedule",
			"harnessMetadata": `{"schemaVersion":1,"harnessVersion":"client","agent":{"depth":2}}`,
			"clientExtension": "keep",
		},
	}
	out, err := chatBodyForProduct(body, productCLI, Versions{})
	if err != nil {
		t.Fatalf("chatBodyForProduct: %v", err)
	}
	extra, _ := out["extra_vars"].(map[string]any)
	if extra["initiator"] != "agent" || extra["interactionType"] != "schedule" {
		t.Fatalf("client interaction was overwritten: %#v", extra)
	}
	if extra["harnessMetadata"] != `{"schemaVersion":1,"harnessVersion":"client","agent":{"depth":2}}` {
		t.Fatalf("client harnessMetadata was overwritten: %#v", extra["harnessMetadata"])
	}
	if extra["clientExtension"] != "keep" {
		t.Fatalf("client extension was lost: %#v", extra)
	}
}

func TestCLIRequestBodyScheduledInteraction(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{
			"role": "user", "content": "scheduled", "scheduledTaskTrigger": "timer",
		}},
	}
	out, err := chatBodyForProduct(body, productCLI, Versions{})
	if err != nil {
		t.Fatalf("chatBodyForProduct: %v", err)
	}
	extra, _ := out["extra_vars"].(map[string]any)
	if extra["initiator"] != "agent" || extra["interactionType"] != "schedule" {
		t.Fatalf("scheduled interaction = %#v", extra)
	}
}

func TestNonCLIRequestBodyLeavesExtraVarsAlone(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	for _, product := range []string{productDesktop, productVSCode} {
		out, err := chatBodyForProduct(body, product, Versions{})
		if err != nil {
			t.Fatalf("chatBodyForProduct(%s): %v", product, err)
		}
		if _, exists := out["extra_vars"]; exists {
			t.Fatalf("%s unexpectedly gained extra_vars: %#v", product, out["extra_vars"])
		}
		if _, exists := out["cache_retention"]; exists {
			t.Fatalf("%s unexpectedly gained cache_retention", product)
		}
	}
}
