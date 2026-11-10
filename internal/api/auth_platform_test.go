package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestAuthDisabled(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	keyStore := keys.New(store)
	if _, err := keyStore.Save([]keys.Entry{{ID: "k1", Name: "one", Key: "key-one"}}, nil); err != nil {
		t.Fatal(err)
	}
	provider := &stubProvider{id: "stub", realm: "intl", models: []providers.Model{{"id": "m1"}}}
	service := settings.New(settings.Service{
		Store: store, Keys: keyStore, Providers: []providers.Provider{provider},
		AccountsDir: dir, Version: "test",
	})
	server := New(ServerOptions{
		Providers: []providers.Provider{provider}, KeyStore: keyStore, Settings: service,
	})
	rec := doRequest(server, http.MethodGet, "/v1/models?realm=intl", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth-required status = %d", rec.Code)
	}
	if err := keyStore.SetAuthDisabled(true); err != nil {
		t.Fatal(err)
	}
	rec = doRequest(server, http.MethodGet, "/v1/models?realm=intl", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth-disabled status = %d body=%s", rec.Code, rec.Body)
	}
	// Panel routes still require a session.
	rec = doRequest(server, http.MethodGet, "/settings", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("panel route with auth disabled = %d", rec.Code)
	}
	if err := keyStore.SetAuthDisabled(false); err != nil {
		t.Fatal(err)
	}
	rec = doRequest(server, http.MethodGet, "/v1/models?realm=intl", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("re-enabled status = %d", rec.Code)
	}
}

func chatSuccessStream() providers.ChatStream {
	return newStubStream(
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	)
}

func TestKeyRealmBinding(t *testing.T) {
	_, keyStore, _, _, _ := platformFixture(t)
	if _, err := keyStore.Save([]keys.Entry{{ID: "bound", Name: "cn key", Key: "key-bound", Realm: "cn"}}, nil); err != nil {
		t.Fatal(err)
	}
	intl := &stubProvider{id: "wb-intl", realm: "intl", models: []providers.Model{{"id": "m1"}}, openStream: chatSuccessStream()}
	cn := &stubProvider{id: "wb-cn", realm: "cn", models: []providers.Model{{"id": "m1"}}, openStream: chatSuccessStream()}
	server := New(ServerOptions{Providers: []providers.Provider{intl, cn}, KeyStore: keyStore})
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"realm":"intl"}`
	rec := doRequest(server, http.MethodPost, "/v1/chat/completions?realm=all", body, map[string]string{
		"Authorization": "Bearer key-bound", "X-Realm": "intl",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bound chat status=%d body=%s", rec.Code, rec.Body)
	}
	if len(cn.opened()) != 1 || len(intl.opened()) != 0 {
		t.Fatalf("providers intl=%d cn=%d", len(intl.opened()), len(cn.opened()))
	}
	if request := cn.opened()[0]; request.AllowRealmFallback == nil || *request.AllowRealmFallback {
		t.Fatalf("bound key did not suppress fallback: %#v", request.AllowRealmFallback)
	}

	// An unknown/all binding is rejected with 400 instead of falling through.
	unknownServer := New(ServerOptions{
		Providers: []providers.Provider{intl, cn}, KeyStore: keyStore,
		APIKeyRealm: func(*http.Request) string { return "all" },
	})
	if _, err := unknownServer.keyStore.Save([]keys.Entry{{ID: "unbound", Name: "unbound", Key: "key-unbound"}}, []string{}); err != nil {
		t.Fatal(err)
	}
	rec = doRequest(unknownServer, http.MethodPost, "/v1/chat/completions", body, map[string]string{
		"Authorization": "Bearer key-unbound",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid binding status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestKeyModelAllowlistAPI(t *testing.T) {
	_, keyStore, _, _, _ := platformFixture(t)
	if _, err := keyStore.Save([]keys.Entry{{
		ID: "restricted", Name: "restricted", Key: "key-restricted", Models: []string{"deepseek/*"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	provider := &stubProvider{id: "stub", realm: "intl", models: []providers.Model{{"id": "m1"}}, openStream: chatSuccessStream()}
	server := New(ServerOptions{Providers: []providers.Provider{provider}, KeyStore: keyStore})
	rec := doRequest(server, http.MethodPost, "/v1/chat/completions", `{"model":"gpt-6-pro","messages":[{"role":"user","content":"hi"}]}`, map[string]string{
		"Authorization": "Bearer key-restricted",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("restricted model status=%d body=%s", rec.Code, rec.Body)
	}
	if len(provider.opened()) != 0 {
		t.Fatal("restricted request reached provider")
	}
	rec = doRequest(server, http.MethodPost, "/v1/chat/completions", `{"model":"deepseek/chat","messages":[{"role":"user","content":"hi"}]}`, map[string]string{
		"Authorization": "Bearer key-restricted",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed model status=%d body=%s", rec.Code, rec.Body)
	}
}

func TestMessagesAuthPreservesNon401(t *testing.T) {
	_, keyStore, _, _, _ := platformFixture(t)
	if _, err := keyStore.Save([]keys.Entry{{
		ID: "limited", Name: "limited", Key: "key-limited", ConcurrencyLimit: 1,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	stream := &blockingStream{entered: make(chan struct{}), release: make(chan struct{})}
	provider := &stubProvider{id: "stub", realm: "intl", models: []providers.Model{{"id": "m1"}}, openStream: stream}
	server := New(ServerOptions{
		Providers: []providers.Provider{provider}, KeyStore: keyStore,
		AnthropicMessagesEnabled: func() bool { return true },
	})
	handler := server.Handler()
	firstDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("x-api-key", "key-limited")
		req.Header.Set("anthropic-version", "2023-06-01")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		firstDone <- rec.Code
	}()
	select {
	case <-stream.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first Messages request never reached upstream")
	}
	rec := doRequest(server, http.MethodPost, "/v1/messages", `{"model":"m1","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{
		"x-api-key": "key-limited", "anthropic-version": "2023-06-01",
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("Messages auth status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	errBody, _ := body["error"].(map[string]any)
	if errBody["type"] != "rate_limit_error" {
		t.Fatalf("Messages 429 body = %#v", body)
	}
	close(stream.release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first Messages request did not finish")
	}
}

type blockingStream struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingStream) Next() ([]byte, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil, io.EOF
}
func (s *blockingStream) Close() error { return nil }

func TestKeyConcurrency(t *testing.T) {
	_, keyStore, _, _, _ := platformFixture(t)
	if _, err := keyStore.Save([]keys.Entry{{
		ID: "limited", Name: "limited", Key: "key-limited", ConcurrencyLimit: 1,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	stream := &blockingStream{entered: make(chan struct{}), release: make(chan struct{})}
	provider := &stubProvider{id: "stub", realm: "intl", models: []providers.Model{{"id": "m1"}}, openStream: stream}
	server := New(ServerOptions{Providers: []providers.Provider{provider}, KeyStore: keyStore})
	handler := server.Handler()
	firstDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer key-limited")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		firstDone <- rec.Code
	}()
	select {
	case <-stream.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never reached upstream")
	}
	rec := doRequest(server, http.MethodPost, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, map[string]string{
		"Authorization": "Bearer key-limited",
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d body=%s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	errBody, _ := body["error"].(map[string]any)
	message, _ := errBody["message"].(string)
	if !strings.Contains(message, "concurrency limit (1 in flight)") || errBody["type"] != "rate_limit_error" {
		t.Fatalf("429 body = %#v", body)
	}
	if errBody["reset_at"] == nil || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("missing reset metadata: %#v headers=%v", body, rec.Header())
	}
	close(stream.release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not finish")
	}
	// The lease is released, so a later request can run.
	provider.openStream = chatSuccessStream()
	rec = doRequest(server, http.MethodPost, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, map[string]string{
		"Authorization": "Bearer key-limited",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("post-release status=%d body=%s", rec.Code, rec.Body)
	}
}
