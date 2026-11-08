package api

import (
	"net/http"
	"net/http/httptest"
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

func TestPanelAuthNilLeavesPanelRoutesDeclaredOnly(t *testing.T) {
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
	if rec.Code != http.StatusOK {
		t.Fatalf("nil middleware status = %d, want declared-only pass-through", rec.Code)
	}
}
