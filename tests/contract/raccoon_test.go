package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/raccoon"
)

const contractVersion = "v2-contract"

// contractCatalogFixture is the two-model catalog the v1.6.5 oracle fake
// serves. Keeping it small makes the golden exact: IDs, capability spellings,
// counts, and the v1 display names all come from the same fixture.
const contractCatalogFixture = `{
  "code": 0,
  "data": {
    "categories": [
      {
        "type": "chat",
        "default_model": "raccoon-090dd3",
        "models": [
          {
            "name": "raccoon-090dd3",
            "display_name": "Raccoon-Work",
            "params": {"context_window": 1000000, "max_tokens": 100000},
            "tags": ["general", "code", "vision", "reasoning"]
          },
          {
            "name": "sn-glm-5-3",
            "display_name": "GLM-5-3",
            "params": {"context_window": 1000000, "max_tokens": 100000},
            "tags": ["code", "reasoning"]
          }
        ]
      }
    ]
  }
}`

func TestNormalizeJSONIgnoresDynamicFields(t *testing.T) {
	first := []byte(`{
	  "id": "chatcmpl-abc123",
	  "object": "chat.completion",
	  "created": 1700000000,
	  "model": "raccoon-090dd3",
	  "server": "nginx/1.25",
	  "Date": "Mon, 01 Jan 2024 00:00:00 GMT",
	  "request_id": "req-123",
	  "session_id": "sess-456",
	  "trace_id": "trace-789",
	  "expires_at": "2026-11-08T12:34:56Z",
	  "fetched_at": 1700000000.5,
	  "uuid": "550e8400-e29b-41d4-a716-446655440000",
	  "account": {
	    "uid": "u-1",
	    "account_id": "550e8400-e29b-41d4-a716-446655440000",
	    "nickname": "Raccoon Fan"
	  },
	  "models": [
	    {"id": "raccoon-090dd3", "name": "Raccoon-Work"},
	    {"id": "sn-deepseek-v4-1-flash-1017", "name": "sn-glm-5-3"}
	  ],
	  "tool": {"id": "call_abc", "name": "lookup", "arguments": "{\"x\":1}"},
	  "count": 2,
	  "ratio": 1.25,
	  "flag": true,
	  "nested": {"keep": "yes", "updated_at": 1700000001}
	}`)
	second := bytes.ReplaceAll(first, []byte("chatcmpl-abc123"), []byte("chatcmpl-def456"))
	second = bytes.ReplaceAll(second, []byte("1700000000.5"), []byte("1700000999.25"))
	second = bytes.ReplaceAll(second, []byte("1700000001"), []byte("1700000888"))
	second = bytes.Replace(second,
		[]byte(`"uuid": "550e8400-e29b-41d4-a716-446655440000"`),
		[]byte(`"uuid": "123e4567-e89b-12d3-a456-426614174000"`), 1)

	got, err := NormalizeJSON(first)
	if err != nil {
		t.Fatalf("NormalizeJSON(first): %v", err)
	}
	again, err := NormalizeJSON(second)
	if err != nil {
		t.Fatalf("NormalizeJSON(second): %v", err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("normalized dynamic differences leaked:\nfirst:  %#v\nsecond: %#v", got, again)
	}

	root, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("normalized root = %T, want map[string]any", got)
	}
	for _, key := range []string{"server", "Date", "request_id", "session_id", "trace_id"} {
		if _, present := root[key]; present {
			t.Errorf("dynamic key %q was not removed: %#v", key, root[key])
		}
	}

	if root["model"] != "raccoon-090dd3" {
		t.Errorf("semantic model = %#v, want raccoon-090dd3", root["model"])
	}
	if root["id"] != DynamicValue {
		t.Errorf("chatcmpl id = %#v, want %q", root["id"], DynamicValue)
	}
	if root["created"] != TimestampValue {
		t.Errorf("created = %#v, want %q", root["created"], TimestampValue)
	}
	if root["expires_at"] != TimestampValue || root["fetched_at"] != TimestampValue {
		t.Errorf("timestamp keys were not normalized: %#v %#v", root["expires_at"], root["fetched_at"])
	}
	if root["uuid"] != UUIDValue {
		t.Errorf("uuid = %#v, want %q", root["uuid"], UUIDValue)
	}

	account, ok := root["account"].(map[string]any)
	if !ok {
		t.Fatalf("account = %T, want map", root["account"])
	}
	if account["nickname"] != "Raccoon Fan" || account["uid"] != "u-1" {
		t.Errorf("account/nickname content was normalized away: %#v", account)
	}
	if account["account_id"] != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("account ID content was replaced: %#v", account["account_id"])
	}

	models, ok := root["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("models = %#v, want two entries", root["models"])
	}
	firstModel := models[0].(map[string]any)
	secondModel := models[1].(map[string]any)
	if firstModel["id"] != "raccoon-090dd3" || firstModel["name"] != "Raccoon-Work" {
		t.Errorf("first model was normalized: %#v", firstModel)
	}
	if secondModel["id"] != "sn-deepseek-v4-1-flash-1017" || secondModel["name"] != "sn-glm-5-3" {
		t.Errorf("semantic sn-* model content was normalized: %#v", secondModel)
	}

	tool := root["tool"].(map[string]any)
	if tool["id"] != DynamicValue || tool["name"] != "lookup" {
		t.Errorf("tool = %#v, want dynamic id and preserved name", tool)
	}

	nested := root["nested"].(map[string]any)
	if nested["keep"] != "yes" || nested["updated_at"] != TimestampValue {
		t.Errorf("nested = %#v", nested)
	}

	count, ok := root["count"].(json.Number)
	if !ok || count.String() != "2" {
		t.Fatalf("count = %#v (%T), want json.Number 2", root["count"], root["count"])
	}
	ratio, ok := root["ratio"].(json.Number)
	if !ok || ratio.String() != "1.25" {
		t.Fatalf("ratio = %#v (%T), want json.Number 1.25", root["ratio"], root["ratio"])
	}
	if root["flag"] != true {
		t.Fatalf("flag = %#v, want bool true", root["flag"])
	}
}

func TestNormalizeSSEMergesChunks(t *testing.T) {
	stream := strings.Join([]string{
		": keepalive",
		"",
		"event: chat.chunk",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"think "}}],`,
		`data: "model":"raccoon-090dd3"}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"more"}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"lo","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"look","arguments":"{\"a\":"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`,
		"",
		"data: [DONE]",
		"",
	}, "\r\n")

	events, err := NormalizeSSE([]byte(stream))
	if err != nil {
		t.Fatalf("NormalizeSSE: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want one merged event: %#v", len(events), events)
	}
	got := events[0]
	want := Event{
		Type:             "chat.chunk",
		Types:            []string{"chat.chunk"},
		Objects:          []string{"chat.completion.chunk"},
		Role:             "assistant",
		FinishReason:     "tool_calls",
		Content:          "Hello",
		ReasoningContent: "think more",
		ToolCalls: []ToolCall{{
			Index:     0,
			ID:        DynamicValue,
			Type:      "function",
			Name:      "look",
			Arguments: `{"a":1}`,
		}},
		Usage: map[string]any{
			"prompt_tokens":     json.Number("10"),
			"completion_tokens": json.Number("4"),
			"total_tokens":      json.Number("14"),
		},
		Done: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized event mismatch:\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestRaccoonGoldenHealthModelsChat(t *testing.T) {
	fixture := newContractFixture(t)

	var catalogCalls int

	t.Run("models", func(t *testing.T) {
		rec := fixture.do(t, http.MethodGet, "/v1/models?realm=raccoon", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		root := decodeJSONBody(t, rec.Body.Bytes())
		if root["object"] != "list" || root["realm"] != "raccoon" {
			t.Fatalf("models envelope = %#v", root)
		}
		data, ok := root["data"].([]any)
		if !ok || len(data) != 2 {
			t.Fatalf("model count = %#v, want 2", root["data"])
		}
		ids := make([]string, 0, len(data))
		for _, entry := range data {
			ids = append(ids, entry.(map[string]any)["id"].(string))
		}
		wantIDs := []string{"raccoon-090dd3", "sn-glm-5-3"}
		if !reflect.DeepEqual(ids, wantIDs) {
			t.Fatalf("model ids = %#v, want %#v", ids, wantIDs)
		}
		assertGoldenJSON(t, root, "models.golden.json")

		fixture.fake.mu.Lock()
		catalogCalls = fixture.fake.catalogCalls
		fixture.fake.mu.Unlock()
		if catalogCalls != 1 {
			t.Fatalf("fake catalog calls = %d, want 1", catalogCalls)
		}
	})

	t.Run("health", func(t *testing.T) {
		// Prime the live catalog so this subtest is also meaningful in
		// isolation; with the parent test already warmed this is a cache hit.
		if rec := fixture.do(t, http.MethodGet, "/v1/models?realm=raccoon", ""); rec.Code != http.StatusOK {
			t.Fatalf("prime models status = %d, body = %s", rec.Code, rec.Body)
		}
		rec := fixture.do(t, http.MethodGet, "/health?force=1", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		root := decodeJSONBody(t, rec.Body.Bytes())
		providerHealth, ok := root["providers"].(map[string]any)["raccoon"].(map[string]any)
		if !ok {
			t.Fatalf("providers.raccoon missing: %#v", root)
		}
		// base_url is the only health field that changes per fake-upstream run;
		// it is asserted directly, while the stable semantic subset is golden.
		if providerHealth["base_url"] != fixture.upstream.URL {
			t.Fatalf("base_url = %#v, want %q", providerHealth["base_url"], fixture.upstream.URL)
		}
		assertGoldenJSON(t, root, "health.golden.json")
	})

	t.Run("chat_stream", func(t *testing.T) {
		before := fixture.chatCalls()
		rec := fixture.do(t, http.MethodPost, "/v1/chat/completions?realm=raccoon",
			`{"model":"raccoon-090dd3","messages":[{"role":"user","content":"hello"}],"stream":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		if got := fixture.chatCalls(); got != before+1 {
			t.Fatalf("fake chat calls = %d, want %d", got, before+1)
		}
		if got := rec.Header().Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
			t.Fatalf("content type = %q", got)
		}
		events, err := NormalizeSSE(rec.Body.Bytes())
		if err != nil {
			t.Fatalf("NormalizeSSE: %v", err)
		}
		var want []Event
		decodeGoldenFile(t, "chat_stream.golden.json", &want)
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("stream golden mismatch:\ngot:  %#v\nwant: %#v", events, want)
		}
	})

	t.Run("chat_nonstream", func(t *testing.T) {
		before := fixture.chatCalls()
		rec := fixture.do(t, http.MethodPost, "/v1/chat/completions?realm=raccoon",
			`{"model":"raccoon-090dd3","messages":[{"role":"user","content":"hello"}],"stream":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		if got := fixture.chatCalls(); got != before+1 {
			t.Fatalf("fake chat calls = %d, want %d", got, before+1)
		}
		root := decodeJSONBody(t, rec.Body.Bytes())
		choices, ok := root["choices"].([]any)
		if !ok || len(choices) != 1 {
			t.Fatalf("choices = %#v", root["choices"])
		}
		choice := choices[0].(map[string]any)
		message := choice["message"].(map[string]any)
		if message["role"] != "assistant" || message["content"] != "你好" {
			t.Fatalf("message = %#v", message)
		}
		if choice["finish_reason"] != "stop" {
			t.Fatalf("finish_reason = %#v", choice["finish_reason"])
		}
		usage := root["usage"].(map[string]any)
		if usage["prompt_tokens"].(json.Number).String() != "3" ||
			usage["completion_tokens"].(json.Number).String() != "2" ||
			usage["total_tokens"].(json.Number).String() != "5" {
			t.Fatalf("usage = %#v", usage)
		}
		assertGoldenJSON(t, root, "chat_nonstream.golden.json")
	})
}

// newContractFixture builds the real Raccoon provider pointed at a fake
// upstream, seeds one valid account, and mounts it on the real generic API.
type contractFixture struct {
	server   *http.Server
	provider *raccoon.Provider
	upstream *httptest.Server
	fake     *contractUpstream
}

func newContractFixture(t *testing.T) *contractFixture {
	t.Helper()

	const uid = "pu-contract"
	now := time.Now().UTC().Truncate(time.Second)
	fake := &contractUpstream{}
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	t.Setenv("WB_RACCOON_BASE_URL", upstream.URL)

	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"raccoon": map[string]any{"enabled": true, "max_concurrent": 1},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	authPath, err := raccoon.AuthPath(dir, uid)
	if err != nil {
		t.Fatalf("AuthPath: %v", err)
	}
	if err := raccoon.SaveAuth(authPath, raccoon.Auth{
		UID:            uid,
		AccessToken:    unsignedJWT(uid, now.Add(2*time.Hour)),
		RefreshToken:   "refresh-" + uid,
		OfficeIdentity: "personal",
		OfficeOrgName:  "Contract Org",
		ExpiresAt:      now.Add(2 * time.Hour),
		TokenType:      "Bearer",
		UpdatedAt:      now.Unix(),
	}); err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}

	provider, err := raccoon.New(store)
	if err != nil {
		t.Fatalf("raccoon.New: %v", err)
	}
	provider.SetNow(func() time.Time { return now })

	return &contractFixture{
		server:   api.NewServer(api.ServerOptions{Version: contractVersion, Providers: []providers.Provider{provider}}),
		provider: provider,
		upstream: upstream,
		fake:     fake,
	}
}

func (f *contractFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var request *http.Request
	if reader == nil {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, reader)
		request.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.server.Handler.ServeHTTP(rec, request)
	return rec
}

func (f *contractFixture) chatCalls() int {
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	return f.fake.chatCalls
}

func unsignedJWT(uid string, expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		fmt.Sprintf(`{"sub":%q,"exp":%d}`, uid, expiry.Unix()),
	))
	return header + "." + payload + ".signature"
}

// contractUpstream is the deterministic fake Raccoon API used by the golden
// test. It serves the v1.6.5 catalog fixture and an OpenAI-compatible SSE
// stream; the provider and generic API do the real work.
type contractUpstream struct {
	mu sync.Mutex

	catalogCalls int
	chatCalls    int
	chatBodies   []map[string]any
}

func (u *contractUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == raccoon.CatalogPath:
		u.mu.Lock()
		u.catalogCalls++
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(contractCatalogFixture))

	case r.Method == http.MethodPost && r.URL.Path == raccoon.ChatPath:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.chatCalls++
		u.chatBodies = append(u.chatBodies, body)
		u.mu.Unlock()

		model, _ := body["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		for _, frame := range contractChatFrames(model) {
			if _, err := w.Write(frame); err != nil {
				return
			}
		}

	default:
		http.NotFound(w, r)
	}
}

// contractChatFrames mirrors the semantic v1.6.5 stream: role, two text
// deltas, a terminator, one trailing usage-only frame, and [DONE]. IDs and
// timestamps are deliberately dynamic and must not affect the SSE golden.
func contractChatFrames(model string) [][]byte {
	chunks := []map[string]any{
		{
			"id": "chatcmpl-upstream", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": ""},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-upstream", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "你"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-upstream", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "好"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-upstream", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
			}},
		},
		{
			"id": "chatcmpl-upstream", "object": "chat.completion.chunk", "created": 1,
			"model":   model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5,
			},
		},
	}
	frames := make([][]byte, 0, len(chunks)+1)
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			panic(err)
		}
		frames = append(frames, []byte("data: "+string(encoded)+"\n\n"))
	}
	return append(frames, []byte("data: [DONE]\n\n"))
}

func decodeJSONBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON %s: %v", body, err)
	}
	root, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("JSON root = %T, want object", value)
	}
	return root
}

func decodeGoldenFile(t *testing.T, name string, target any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode golden %s: %v", name, err)
	}
}

// assertGoldenJSON compares the actual object against the golden shape. The
// golden deliberately carries a stable subset: fields absent from the golden
// (base_url, dynamic ids, panel/overview data v2 does not implement) are not
// asserted and must not be fabricated as parity.
func assertGoldenJSON(t *testing.T, actual map[string]any, name string) {
	t.Helper()
	var want any
	decodeGoldenFile(t, name, &want)
	projected := projectToShape(t, actual, want)
	if !reflect.DeepEqual(projected, want) {
		t.Fatalf("golden %s mismatch:\ngot:  %#v\nwant: %#v", name, projected, want)
	}
}

func projectToShape(t *testing.T, actual any, shape any) any {
	t.Helper()
	switch want := shape.(type) {
	case map[string]any:
		got, ok := actual.(map[string]any)
		if !ok {
			t.Fatalf("shape wants object, actual is %T", actual)
		}
		out := make(map[string]any, len(want))
		for key, value := range want {
			actualValue, present := got[key]
			if !present {
				t.Fatalf("missing golden field %q", key)
			}
			out[key] = projectToShape(t, actualValue, value)
		}
		return out
	case []any:
		got, ok := actual.([]any)
		if !ok {
			t.Fatalf("shape wants array, actual is %T", actual)
		}
		if len(got) != len(want) {
			t.Fatalf("array length = %d, want %d", len(got), len(want))
		}
		out := make([]any, len(want))
		for index := range want {
			out[index] = projectToShape(t, got[index], want[index])
		}
		return out
	default:
		return actual
	}
}
