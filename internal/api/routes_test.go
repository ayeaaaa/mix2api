package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type adapterTestProvider struct {
	providers.Provider
	routes []providers.Route
}

func (p adapterTestProvider) Routes() []providers.Route { return p.routes }

func TestRegisterProviderRoutesDispatchesAndKeepsEnvelopes(t *testing.T) {
	routes := []providers.Route{
		{
			Method: http.MethodGet, Path: "/raccoon/overview", Auth: providers.AuthPanel,
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			},
		},
		{
			Method: http.MethodPost, Path: "/raccoon/accounts/import", Auth: providers.AuthPanel,
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			Method: http.MethodGet, Path: "/auth/callback", Auth: providers.AuthNone,
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
		},
	}
	mux := http.NewServeMux()
	RegisterProviderRoutes(mux, adapterTestProvider{routes: routes})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/raccoon/overview", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("known route status=%d body=%s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/raccoon/not-a-route", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown status=%d body=%s", rec.Code, rec.Body)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unknown body = %s: %v", rec.Body, err)
	}
	if body["error"]["code"] != float64(http.StatusNotFound) {
		t.Fatalf("unknown envelope = %#v", body)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/raccoon/accounts/import", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method mismatch status=%d body=%s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("method mismatch body = %s: %v", rec.Body, err)
	}
	if body["error"]["code"] != float64(http.StatusMethodNotAllowed) {
		t.Fatalf("method mismatch envelope = %#v", body)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status=%d body=%s", rec.Code, rec.Body)
	}
}
