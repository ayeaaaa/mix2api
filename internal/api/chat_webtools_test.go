package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/webtools"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func chatWebToolsForTest(t *testing.T) *webtools.Service {
	t.Helper()
	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<li class="b_algo"><h2><a href="https://go.dev/doc">Go docs</a></h2><p>Official docs</p></li>`)
	}))
	t.Cleanup(searchServer.Close)
	web := webtools.New()
	web.AllowPrivate = true
	web.MinInterval = 0
	web.SearchBudget = 5 * time.Second
	web.Engines = []string{"bing"}
	web.EngineURLs = map[string]string{"bing": searchServer.URL + "?q=%s"}
	return web
}

func TestChatWebRoundsStreaming(t *testing.T) {
	web := chatWebToolsForTest(t)
	first := webToolCallStream()
	second := newStubStream(chatSSE(textChunks("c2", "chat stream final")...)...)
	server, provider := responseTestServer(t, nil, ServerOptions{WebTools: web})
	provider.mu.Lock()
	provider.openStreams = []providers.ChatStream{first, second}
	provider.openStream = nil
	provider.mu.Unlock()
	rec := doRequest(server, http.MethodPost, "/v1/chat/completions", `{
		"model":"m1","stream":true,
		"messages":[{"role":"user","content":"search go"}],
		"tools":[{"type":"web_search"}]
	}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat stream = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "chat stream final") {
		t.Fatalf("chat stream missing final text: %s", body)
	}
	if strings.Contains(body, `"name":"web_search"`) {
		t.Fatalf("internal web call leaked to chat client: %s", body)
	}
	if len(provider.opened()) != 2 {
		t.Fatalf("chat stream rounds = %d", len(provider.opened()))
	}
}

func TestChatWebToolsCanonicalModelEcho(t *testing.T) {
	web := chatWebToolsForTest(t)
	provider := &stubProvider{
		id: "codearts", realm: "codearts",
		models: []providers.Model{{"id": "shared"}},
	}
	provider.mu.Lock()
	provider.openStreams = []providers.ChatStream{webToolCallStream(), chatStreamWithModel("upstream-alias")}
	provider.mu.Unlock()
	var observed []ProtocolObservation
	srv := newTestHTTPServer(ServerOptions{
		WebTools:  web,
		Providers: []providers.Provider{provider},
		ProtocolObserver: func(observation ProtocolObservation) {
			observed = append(observed, observation)
		},
	})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"codearts/shared",
		"messages":[{"role":"user","content":"search go"}],
		"tools":[{"type":"web_search"}]
	}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["model"] != "codearts/shared" {
		t.Fatalf("response model = %#v, want codearts/shared", body["model"])
	}
	if strings.Contains(rec.Body.String(), "upstream-alias") {
		t.Fatalf("web-tool response leaked the upstream model: %s", rec.Body)
	}

	requests := provider.opened()
	if len(requests) != 2 {
		t.Fatalf("provider rounds = %d, want 2", len(requests))
	}
	for index, request := range requests {
		if request.Model != "shared" {
			t.Fatalf("round %d ChatRequest.Model = %q, want shared", index, request.Model)
		}
	}
	if len(observed) != 1 {
		t.Fatalf("observations = %d, want 1", len(observed))
	}
	if observed[0].Model != "shared" {
		t.Fatalf("observation Model = %q, want shared", observed[0].Model)
	}
	if observed[0].RequestedModel != "codearts/shared" {
		t.Fatalf("observation RequestedModel = %q, want codearts/shared", observed[0].RequestedModel)
	}
}

func TestChatWebToolsCanonicalModelStreamEcho(t *testing.T) {
	web := chatWebToolsForTest(t)
	provider := &stubProvider{
		id: "codearts", realm: "codearts",
		models: []providers.Model{{"id": "shared"}},
	}
	provider.mu.Lock()
	provider.openStreams = []providers.ChatStream{webToolCallStream(), chatStreamWithModel("upstream-alias")}
	provider.mu.Unlock()
	srv := newTestHTTPServer(ServerOptions{WebTools: web, Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"codearts/shared","stream":true,
		"messages":[{"role":"user","content":"search go"}],
		"tools":[{"type":"web_search"}]
	}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "upstream-alias") || strings.Contains(body, `"model":"shared"`) {
		t.Fatalf("web-tool stream leaked an upstream model: %s", body)
	}
	requireStreamModel(t, body, "codearts/shared")

	requests := provider.opened()
	if len(requests) != 2 {
		t.Fatalf("provider rounds = %d, want 2", len(requests))
	}
	for index, request := range requests {
		if request.Model != "shared" {
			t.Fatalf("round %d ChatRequest.Model = %q, want shared", index, request.Model)
		}
	}
}
