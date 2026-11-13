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

type adapterNamedProvider struct {
	adapterTestProvider
}

func (p adapterNamedProvider) RouteNotFoundMessage() string {
	return "unknown test endpoint"
}

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

func TestRegisterProviderRoutesUsesProviderNotFoundMessage(t *testing.T) {
	mux := http.NewServeMux()
	RegisterProviderRoutes(mux, adapterNamedProvider{adapterTestProvider{routes: []providers.Route{
		{
			Method: http.MethodGet, Path: "/x/one", Auth: providers.AuthPanel,
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			Method: http.MethodGet, Path: "/x/two", Auth: providers.AuthPanel,
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
		},
	}}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"]["message"] != "unknown test endpoint" {
		t.Fatalf("message = %#v", body["error"]["message"])
	}
}
