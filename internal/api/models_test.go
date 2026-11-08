package api

import (
	"encoding/json"
	"errors"
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

func TestModelsRealmHeaderPrecedence(t *testing.T) {
	alpha := &stubProvider{id: "alpha", realm: "alpha", models: []providers.Model{{"id": "alpha-model"}}}
	beta := &stubProvider{id: "beta", realm: "beta", models: []providers.Model{{"id": "beta-model"}}}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{alpha, beta}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=alpha", "",
		map[string]string{"X-Realm": "beta"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if body["realm"] != "beta" {
		t.Fatalf("realm = %#v, want header realm beta", body["realm"])
	}
	data := body["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "beta-model" {
		t.Fatalf("data = %#v, want beta-model", data)
	}

	rec = serveRequest(srv.Handler, http.MethodGet, "/v1/models", "",
		map[string]string{"X-Realm": "alpha"})
	if rec.Code != http.StatusOK {
		t.Fatalf("header-only status = %d, body = %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("header-only body %s: %v", rec.Body, err)
	}
	if body["realm"] != "alpha" {
		t.Fatalf("header-only realm = %#v", body["realm"])
	}
}

func TestModelsUnknownProviderErrorDoesNotLeakLocalDetail(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		modelsErr: errors.New("open /root/private/raccoon/u-abc.json: uid u-abc"),
	}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=raccoon", "", nil)

	failure := requireErrorEnvelope(t, rec, http.StatusBadGateway, "invalid_request_error")
	if failure["message"] != "upstream request failed" {
		t.Fatalf("message = %#v", failure["message"])
	}
	assertNoSensitiveText(t, rec.Body.String(), "/root/private", "u-abc", ".json")
}

func TestModelsRealmAllWorkBuddyIDPrefix(t *testing.T) {
	intl := &stubProvider{
		id: "workbuddy", realm: "intl",
		models: []providers.Model{{"id": "intl-model"}},
	}
	cn := &stubProvider{
		id: "workbuddy-cn", realm: "cn",
		models: []providers.Model{{"id": "cn-model"}},
	}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{intl, cn}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=all", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	data := body["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data = %#v, want intl and cn models", data)
	}
}

func TestModelsRealmAllFiltersNonWorkBuddyIdentity(t *testing.T) {
	intl := &stubProvider{
		id: "workbuddy", realm: "intl",
		models: []providers.Model{{"id": "intl-model"}},
	}
	cn := &stubProvider{
		id: "raccoon", realm: "cn",
		models: []providers.Model{{"id": "cn-model"}},
	}
	srv := NewServer(ServerOptions{Providers: []providers.Provider{intl, cn}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=all", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	data := body["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "intl-model" {
		t.Fatalf("data = %#v, want only intl-model", data)
	}
}
