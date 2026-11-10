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

func TestChatWebRoundsStreaming(t *testing.T) {
	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<li class="b_algo"><h2><a href="https://go.dev/doc">Go docs</a></h2><p>Official docs</p></li>`)
	}))
	defer searchServer.Close()
	web := webtools.New()
	web.AllowPrivate = true
	web.MinInterval = 0
	web.SearchBudget = 5 * time.Second
	web.Engines = []string{"bing"}
	web.EngineURLs = map[string]string{"bing": searchServer.URL + "?q=%s"}
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
