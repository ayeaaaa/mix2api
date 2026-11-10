package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestPlatformGoldenRoutes pins the semantic wire shape of the platform
// protocol aliases. NormalizeJSON drops Date/Server, generated ids and
// timestamps, so the goldens compare field presence, types, text, usage and
// termination rather than run-to-run noise.
func TestPlatformGoldenRoutes(t *testing.T) {
	f := newPlatformContractFixture(t)

	health := f.request(t, "GET", "/health", "", nil)
	if health.Code != 200 {
		t.Fatalf("health = %d", health.Code)
	}
	comparePlatformGolden(t, "health", health.Body.Bytes())

	models := f.request(t, "GET", "/v1/models", "", f.apiHeaders())
	if models.Code != 200 {
		t.Fatalf("models = %d body=%s", models.Code, models.Body)
	}
	comparePlatformGolden(t, "models", models.Body.Bytes())

	chat := f.request(t, "POST", "/v1/chat/completions",
		`{"model":"`+platformModel+`","reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`,
		f.apiHeaders())
	if chat.Code != 200 {
		t.Fatalf("chat = %d body=%s", chat.Code, chat.Body)
	}
	comparePlatformGolden(t, "chat", chat.Body.Bytes())

	responses := f.request(t, "POST", "/v1/responses",
		`{"model":"`+platformModel+`","reasoning_effort":"high","input":"hello"}`,
		f.apiHeaders())
	if responses.Code != 200 {
		t.Fatalf("responses = %d body=%s", responses.Code, responses.Body)
	}
	comparePlatformGolden(t, "responses", responses.Body.Bytes())

	messages := f.request(t, "POST", "/v1/messages",
		`{"model":"`+platformModel+`","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`,
		f.apiHeaders())
	if messages.Code != 200 {
		t.Fatalf("messages = %d body=%s", messages.Code, messages.Body)
	}
	comparePlatformGolden(t, "messages", messages.Body.Bytes())
}

func comparePlatformGolden(t *testing.T, name string, body []byte) {
	t.Helper()
	normalized, err := NormalizeJSON(body)
	if err != nil {
		t.Fatalf("%s: normalize: %v", name, err)
	}
	path := filepath.Join("testdata", "platform", "golden", name+".json")
	if os.Getenv("PLATFORM_UPDATE_GOLDENS") == "1" {
		encoded, err := json.MarshalIndent(normalized, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: read golden: %v", name, err)
	}
	expected, err := NormalizeJSON(raw)
	if err != nil {
		t.Fatalf("%s: normalize golden: %v", name, err)
	}
	if !reflect.DeepEqual(normalized, expected) {
		actual, _ := json.MarshalIndent(normalized, "", "  ")
		want, _ := json.MarshalIndent(expected, "", "  ")
		t.Fatalf("%s: contract mismatch\n actual=%s\n expected=%s", name, actual, want)
	}
}
