package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestModelsRealmRaccoon(t *testing.T) {
	provider := &stubProvider{
		id:    "raccoon",
		realm: "raccoon",
		models: []providers.Model{
			{"id": "raccoon-090dd3", "object": "model", "owned_by": "sensetime", "provider": "raccoon"},
			{"id": "sn-kimi-k3", "object": "model", "owned_by": "sensetime", "provider": "raccoon"},
		},
	}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=raccoon", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body["object"] != "list" || body["realm"] != "raccoon" {
		t.Fatalf("envelope = %#v", body)
	}
	data, ok := body["data"].([]any)
	if !ok || len(data) != 2 {
		t.Fatalf("data = %#v", body["data"])
	}
	first := data[0].(map[string]any)
	if first["id"] != "raccoon-090dd3" || first["provider"] != "raccoon" {
		t.Fatalf("model entry = %#v", first)
	}
}

func TestModelsUnknownRealm404(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models: []providers.Model{{"id": "m1"}},
	}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=missing", "", nil)
	requireErrorEnvelope(t, rec, http.StatusNotFound, "invalid_request_error")
}

func TestModelsDefaultSingleProvider(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models: []providers.Model{{"id": "m1"}},
	}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body["realm"] != "raccoon" {
		t.Fatalf("realm = %#v", body["realm"])
	}
}

func TestModelsMultipleProvidersRequireRealm(t *testing.T) {
	first := &stubProvider{id: "a", realm: "alpha", models: []providers.Model{{"id": "a1"}}}
	second := &stubProvider{id: "b", realm: "beta", models: []providers.Model{{"id": "b1"}}}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{first, second}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models", "", nil)
	requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")
}

func TestModelsRealmAllUnion(t *testing.T) {
	intl := &stubProvider{
		id: "workbuddy", realm: "intl",
		models: []providers.Model{
			{"id": "shared-model", "provider": "intl"},
			{"id": "intl-only", "provider": "intl"},
		},
	}
	cn := &stubProvider{
		id: "workbuddy", realm: "cn",
		models: []providers.Model{
			{"id": "shared-model", "provider": "cn"},
			{"id": "cn-only", "provider": "cn"},
		},
	}
	raccoon := &stubProvider{id: "raccoon", realm: "raccoon", models: []providers.Model{{"id": "r1"}}}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{intl, cn, raccoon}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=all", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body["realm"] != "all" || body["object"] != "list" {
		t.Fatalf("envelope = %#v", body)
	}
	data := body["data"].([]any)
	ids := make([]string, 0, len(data))
	for _, raw := range data {
		entry := raw.(map[string]any)
		ids = append(ids, entry["id"].(string))
		if entry["id"] == "r1" {
			t.Fatalf("realm=all leaked a non-WorkBuddy model: %#v", data)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("ids = %#v, want 3 deduplicated entries", ids)
	}
	first := data[0].(map[string]any)
	if first["provider"] != "intl" {
		t.Fatalf("duplicate resolution = %#v, want intl first", first)
	}
}

func TestModelsRealmAllLockedWithoutWorkBuddy(t *testing.T) {
	provider := &stubProvider{id: "raccoon", realm: "raccoon", models: []providers.Model{{"id": "m1"}}}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=all", "", nil)
	failure := requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")
	if failure["message"] == "" {
		t.Fatalf("missing locked-realm explanation")
	}
}

func TestModelsProviderErrorMapped(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		modelsErr: &mappedTestError{
			status: http.StatusTooManyRequests, errType: "rate_limit_error", message: "slow down",
		},
	}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=raccoon", "", nil)
	requireErrorEnvelope(t, rec, http.StatusTooManyRequests, "rate_limit_error")
}

func TestModelsMethodNotAllowedEnvelope(t *testing.T) {
	provider := &stubProvider{id: "raccoon", realm: "raccoon", models: []providers.Model{{"id": "m1"}}}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/models?realm=raccoon", "{}", nil)
	requireErrorEnvelope(t, rec, http.StatusMethodNotAllowed, "invalid_request_error")
}
