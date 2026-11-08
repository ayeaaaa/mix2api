package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestPanelAuthWrapsProviderSubtree(t *testing.T) {
	provider := &stubProvider{
		id: "panel", realm: "panel",
		models: []providers.Model{{"id": "m1"}},
		routes: []providers.Route{
			{
				Method: http.MethodGet, Path: "/panel/overview", Auth: providers.AuthPanel,
				Handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			},
			{
				Method: http.MethodGet, Path: "/public/callback", Auth: providers.AuthNone,
				Handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			},
		},
	}
	middleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Panel") != "ok" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"panel password required"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	srv := NewServer(ServerOptions{
		Providers: []providers.Provider{provider},
		PanelAuth: middleware,
	})

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("panel route without auth status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/not-a-route", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown panel subtree status = %d, want middleware 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public/callback", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("public route status = %d, want 200", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/panel/overview", nil)
	req.Header.Set("X-Panel", "ok")
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated panel route status = %d", rec.Code)
	}
}

func TestAPIKeyAuthNilFailsClosed(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models:     []providers.Model{{"id": "m1"}},
		openStream: newStubStream("data: [DONE]\n\n"),
	}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})
	for _, path := range []string{"/v1/models?realm=raccoon", "/v1/chat/completions"} {
		method := http.MethodGet
		body := ""
		if path == "/v1/chat/completions" {
			method = http.MethodPost
			body = `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
		}
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s with nil API auth status = %d, want 401", path, rec.Code)
		}
	}
	if len(provider.opened()) != 0 {
		t.Fatal("unauthenticated chat reached the provider")
	}
}

func TestRequireAPIKeyAuthSpellingsAndConstantTimeReject(t *testing.T) {
	protected := RequireAPIKeyAuth("secret-key")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	cases := []struct {
		name    string
		headers map[string]string
		query   string
		want    int
	}{
		{name: "no key", want: http.StatusUnauthorized},
		{name: "wrong key", headers: map[string]string{"X-API-Key": "wrong"}, want: http.StatusUnauthorized},
		{name: "bearer", headers: map[string]string{"Authorization": "Bearer secret-key"}, want: http.StatusOK},
		{name: "raw authorization", headers: map[string]string{"Authorization": "secret-key"}, want: http.StatusOK},
		{name: "x api key", headers: map[string]string{"X-API-Key": "secret-key"}, want: http.StatusOK},
		{name: "api key header", headers: map[string]string{"Api-Key": "secret-key"}, want: http.StatusOK},
		{name: "query key", query: "?key=secret-key", want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions"+tc.query, nil)
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}
			protected.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestRequirePanelAuthHeaderAndOrigin(t *testing.T) {
	protected := RequirePanelAuth("panel-secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing panel token status = %d", rec.Code)
	}

	for _, header := range []struct{ name, value string }{
		{"X-Panel-Token", "panel-secret"},
		{"Authorization", "Bearer panel-secret"},
	} {
		rec = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/panel/overview", nil)
		req.Header.Set(header.name, header.value)
		protected.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s panel token status = %d", header.name, rec.Code)
		}
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:58675/panel/action", nil)
	req.Header.Set("X-Panel-Token", "panel-secret")
	req.Header.Set("Origin", "https://evil.example")
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin panel mutation status = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:58675/panel/action", nil)
	req.Header.Set("X-Panel-Token", "panel-secret")
	req.Header.Set("Origin", "http://127.0.0.1:58675")
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin panel mutation status = %d", rec.Code)
	}
}

func TestPanelAuthNilFailsClosed(t *testing.T) {
	provider := &stubProvider{
		id: "panel", realm: "panel",
		models: []providers.Model{{"id": "m1"}},
		routes: []providers.Route{
			{
				Method: http.MethodGet, Path: "/panel/overview", Auth: providers.AuthPanel,
				Handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			},
		},
	}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("nil middleware status = %d, want fail-closed 401", rec.Code)
	}
}
