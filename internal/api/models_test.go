package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

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

func TestModelsCatalogProvenance(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models: []providers.Model{{"id": "raccoon-090dd3", "object": "model"}},
		catalog: map[string]any{
			"source": "raccoon", "fetched_at": 1700000000.5,
			"accounts": 1, "warning": "",
		},
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=raccoon", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	catalog, ok := body["catalog"].(map[string]any)
	if !ok {
		t.Fatalf("catalog = %#v", body["catalog"])
	}
	if catalog["source"] != "raccoon" || catalog["warning"] != "" ||
		numberText(catalog["accounts"]) != "1" {
		t.Fatalf("catalog = %#v", catalog)
	}
	if _, ok := catalog["fetched_at"]; !ok {
		t.Fatalf("catalog has no fetched_at: %#v", catalog)
	}
	if data := body["data"].([]any); len(data) != 1 {
		t.Fatalf("data = %#v", body["data"])
	}
}

func TestModelsUnknownRealm404(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models: []providers.Model{{"id": "m1"}},
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=missing", "", nil)
	requireErrorEnvelope(t, rec, http.StatusNotFound, "invalid_request_error")
}

func TestModelsDefaultSingleProvider(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models: []providers.Model{{"id": "m1"}},
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

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
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{first, second}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models", "", nil)
	failure := requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")
	message, _ := failure["message"].(string)
	if !strings.Contains(message, "realm=gateway") {
		t.Fatalf("multi-provider error should point at realm=gateway: %#v", failure)
	}
}

func TestModelsAPIKeyRealmOutranksExplicitRealm(t *testing.T) {
	alpha := &stubProvider{id: "alpha", realm: "alpha", models: []providers.Model{{"id": "alpha-model"}}}
	beta := &stubProvider{id: "beta", realm: "beta", models: []providers.Model{{"id": "beta-model"}}}
	srv := newTestHTTPServer(ServerOptions{
		Providers:   []providers.Provider{alpha, beta},
		APIKeyRealm: func(*http.Request) string { return "beta" },
	})

	for _, request := range []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{name: "no realm", path: "/v1/models"},
		{name: "query realm", path: "/v1/models?realm=alpha"},
		{name: "header realm", path: "/v1/models", headers: map[string]string{"X-Realm": "alpha"}},
		{name: "realm all", path: "/v1/models?realm=all"},
	} {
		t.Run(request.name, func(t *testing.T) {
			rec := serveRequest(srv.Handler, http.MethodGet, request.path, "", request.headers)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode models: %v", err)
			}
			if body["realm"] != "beta" {
				t.Fatalf("bound models realm = %#v, want beta", body["realm"])
			}
			data, _ := body["data"].([]any)
			if len(data) != 1 || data[0].(map[string]any)["id"] != "beta-model" {
				t.Fatalf("bound models data = %#v", body["data"])
			}
		})
	}
}

func TestModelsInvalidAPIKeyRealm400(t *testing.T) {
	alpha := &stubProvider{id: "alpha", realm: "alpha", models: []providers.Model{{"id": "alpha-model"}}}
	srv := newTestHTTPServer(ServerOptions{
		Providers:   []providers.Provider{alpha},
		APIKeyRealm: func(*http.Request) string { return "missing" },
	})
	for _, path := range []string{"/v1/models", "/v1/models?realm=alpha", "/v1/models?realm=all"} {
		rec := serveRequest(srv.Handler, http.MethodGet, path, "", nil)
		failure := requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")
		message, _ := failure["message"].(string)
		if !strings.Contains(message, "invalid api key realm") {
			t.Fatalf("%s failure = %#v", path, failure)
		}
	}
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
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{intl, cn, raccoon}})

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
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=all", "", nil)
	failure := requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")
	if failure["message"] == "" {
		t.Fatalf("missing locked-realm explanation")
	}
}

func TestModelsRejectsNonCanonicalRealm(t *testing.T) {
	provider := &stubProvider{id: "raccoon", realm: "raccoon", models: []providers.Model{{"id": "m1"}}}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=Raccoon", "", nil)
	requireErrorEnvelope(t, rec, http.StatusNotFound, "invalid_request_error")

	rec = serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=%20raccoon%20", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("trimmed canonical realm status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestModelsProviderErrorMapped(t *testing.T) {
	provider := &stubProvider{
		id: "raccoon", realm: "raccoon",
		modelsErr: &mappedTestError{
			status: http.StatusTooManyRequests, errType: "rate_limit_error", message: "slow down",
		},
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=raccoon", "", nil)
	requireErrorEnvelope(t, rec, http.StatusTooManyRequests, "rate_limit_error")
}

func TestModelsMethodNotAllowedEnvelope(t *testing.T) {
	provider := &stubProvider{id: "raccoon", realm: "raccoon", models: []providers.Model{{"id": "m1"}}}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/models?realm=raccoon", "{}", nil)
	requireErrorEnvelope(t, rec, http.StatusMethodNotAllowed, "invalid_request_error")
}

func TestModelsRealmHeaderPrecedence(t *testing.T) {
	alpha := &stubProvider{id: "alpha", realm: "alpha", models: []providers.Model{{"id": "alpha-model"}}}
	beta := &stubProvider{id: "beta", realm: "beta", models: []providers.Model{{"id": "beta-model"}}}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{alpha, beta}})

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
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
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
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{intl, cn}})

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
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{intl, cn}})

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

func TestModelsGatewayUnionsAllProviders(t *testing.T) {
	intl := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "shared", "name": "Intl"}}}
	cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "shared", "name": "CN"}}}
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared", "name": "CodeArts"}}}
	raccoon := &stubProvider{id: "raccoon", realm: "raccoon", models: []providers.Model{{"id": "r1"}}}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{intl, cn, codearts, raccoon}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["object"] != "list" || body["realm"] != "gateway" {
		t.Fatalf("envelope = %#v", body)
	}
	data, ok := body["data"].([]any)
	if !ok || len(data) != 4 {
		t.Fatalf("data = %#v, want four gateway entries", body["data"])
	}
	wantIDs := []string{"intl/shared", "cn/shared", "codearts/shared", "raccoon/r1"}
	wantUpstream := []string{"shared", "shared", "shared", "r1"}
	wantRealm := []string{"intl", "cn", "codearts", "raccoon"}
	wantProvider := []string{"workbuddy", "workbuddy", "codearts", "raccoon"}
	for index, raw := range data {
		entry := raw.(map[string]any)
		if entry["id"] != wantIDs[index] || entry["qualified_id"] != wantIDs[index] {
			t.Fatalf("entry %d = %#v, want canonical id %q", index, entry, wantIDs[index])
		}
		if entry["upstream_id"] != wantUpstream[index] {
			t.Fatalf("entry %d upstream_id = %#v, want %q", index, entry["upstream_id"], wantUpstream[index])
		}
		if entry["realm"] != wantRealm[index] || entry["provider"] != wantProvider[index] {
			t.Fatalf("entry %d attribution = %#v, want realm %q provider %q",
				index, entry, wantRealm[index], wantProvider[index])
		}
	}
	if data[0].(map[string]any)["name"] != "Intl" ||
		data[2].(map[string]any)["name"] != "CodeArts" {
		t.Fatalf("gateway entries lost provider metadata: %#v", data)
	}
	// Decoration must clone the provider-owned map instead of mutating it.
	if _, mutated := intl.models[0]["qualified_id"]; mutated {
		t.Fatalf("gateway decoration mutated the provider-owned model: %#v", intl.models[0])
	}
	if _, mutated := codearts.models[0]["upstream_id"]; mutated {
		t.Fatalf("gateway decoration mutated the provider-owned model: %#v", codearts.models[0])
	}
	if catalog, ok := body["catalog"]; ok {
		t.Fatalf("gateway response kept the singular catalog: %#v", catalog)
	}
}

func TestModelsGatewayCatalogsByRealm(t *testing.T) {
	intl := &stubProvider{
		id: "workbuddy", realm: "intl",
		models:  []providers.Model{{"id": "m1"}},
		catalog: map[string]any{"source": "remote", "fetched_at": 1700000000},
	}
	raccoon := &stubProvider{
		id: "raccoon", realm: "raccoon",
		models:  []providers.Model{{"id": "r1"}},
		catalog: map[string]any{"source": "raccoon"},
	}
	broken := &stubProvider{
		id: "codearts", realm: "codearts",
		modelsErr: errors.New("offline"),
		catalog:   map[string]any{"source": "codearts"},
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{intl, raccoon, broken}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	catalogs, ok := body["catalogs"].(map[string]any)
	if !ok {
		t.Fatalf("catalogs = %#v", body["catalogs"])
	}
	if entry, _ := catalogs["intl"].(map[string]any); entry["source"] != "remote" {
		t.Fatalf("intl catalog = %#v", catalogs["intl"])
	}
	if entry, _ := catalogs["raccoon"].(map[string]any); entry["source"] != "raccoon" {
		t.Fatalf("raccoon catalog = %#v", catalogs["raccoon"])
	}
	if _, ok := catalogs["codearts"]; ok {
		t.Fatalf("failed provider catalog leaked: %#v", catalogs)
	}
	if _, ok := body["catalog"]; ok {
		t.Fatalf("gateway response kept the singular catalog: %#v", body["catalog"])
	}
}

func TestModelsGatewayPartialCatalogFailure(t *testing.T) {
	good := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "m1"}}}
	broken := &stubProvider{id: "codearts", realm: "codearts", modelsErr: errors.New("offline")}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{good, broken}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	data := body["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "cn/m1" {
		t.Fatalf("data = %#v, want only cn/m1", body["data"])
	}
}

// TestModelsGatewayCatalogReadsAreBounded pins the per-provider deadline: a
// hung catalog must count as that provider's failure instead of hanging the
// whole gateway listing.
func TestModelsGatewayCatalogReadsAreBounded(t *testing.T) {
	bounded := &stubProvider{
		id: "workbuddy", realm: "cn",
		models: []providers.Model{{"id": "m1"}},
		modelsCtx: func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("catalog read has no deadline")
			}
			return nil
		},
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{bounded}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	data := body["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "cn/m1" {
		t.Fatalf("data = %#v, want cn/m1", body["data"])
	}
}

// TestModelsReservedScopesAreNotProviders proves a provider registered under a
// discovery scope cannot become the target of providerForRealm.
func TestModelsReservedScopesAreNotProviders(t *testing.T) {
	gateway := &stubProvider{id: "fake-gateway", realm: "gateway", models: []providers.Model{{"id": "gw"}}}
	all := &stubProvider{id: "fake-all", realm: "all", models: []providers.Model{{"id": "everything"}}}
	cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "m1"}}}
	srv := newTestServer(ServerOptions{Providers: []providers.Provider{gateway, all, cn}})

	if got := srv.providerForRealm("gateway"); got != nil {
		t.Fatalf("providerForRealm(gateway) = %#v, want nil", got)
	}
	if got := srv.providerForRealm("all"); got != nil {
		t.Fatalf("providerForRealm(all) = %#v, want nil", got)
	}
	if len(srv.providers) != 1 || providerRealm(srv.providers[0]) != "cn" {
		t.Fatalf("providers = %#v, want only cn", srv.providers)
	}
}

func TestModelsGatewayAPIKeyRealmRejected(t *testing.T) {
	alpha := &stubProvider{id: "alpha", realm: "alpha", models: []providers.Model{{"id": "alpha-model"}}}
	srv := newTestHTTPServer(ServerOptions{
		Providers:   []providers.Provider{alpha},
		APIKeyRealm: func(*http.Request) string { return "gateway" },
	})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
	failure := requireErrorEnvelope(t, rec, http.StatusBadRequest, "invalid_request_error")
	if message, _ := failure["message"].(string); !strings.Contains(message, "invalid api key realm: gateway") {
		t.Fatalf("failure = %#v", failure)
	}
}

// TestModelsSkipsEntriesWithoutStringID keeps malformed provider metadata out
// of both gateway and concrete-realm responses instead of emitting "<realm>/".
func TestModelsSkipsEntriesWithoutStringID(t *testing.T) {
	cn := &stubProvider{
		id: "workbuddy", realm: "cn",
		models: []providers.Model{
			{"name": "missing id"},
			{"id": 42},
			{"id": "  "},
			{"id": "m1"},
		},
	}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{cn}})

	for _, request := range []struct {
		path  string
		realm string
		want  string
	}{
		{path: "/v1/models?realm=gateway", realm: "gateway", want: "cn/m1"},
		{path: "/v1/models?realm=cn", realm: "cn", want: "m1"},
	} {
		rec := serveRequest(srv.Handler, http.MethodGet, request.path, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", request.path, rec.Code, rec.Body)
		}
		body := decodeBody(t, rec)
		data := body["data"].([]any)
		if len(data) != 1 || data[0].(map[string]any)["id"] != request.want {
			t.Fatalf("%s data = %#v, want %q", request.path, body["data"], request.want)
		}
	}
}

func TestModelsGatewayAllCatalogsFailed(t *testing.T) {
	first := &stubProvider{
		id: "workbuddy", realm: "cn",
		modelsErr: &mappedTestError{
			status: http.StatusServiceUnavailable, errType: "upstream_error", message: "catalog offline",
		},
	}
	second := &stubProvider{id: "codearts", realm: "codearts", modelsErr: errors.New("offline")}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{first, second}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
	requireErrorEnvelope(t, rec, http.StatusServiceUnavailable, "upstream_error")
}

func TestModelsGatewayNoProviders(t *testing.T) {
	srv := newTestHTTPServer(ServerOptions{})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
	requireErrorEnvelope(t, rec, http.StatusNotFound, "invalid_request_error")
}

func TestModelsGatewayBoundKeyFiltersRealm(t *testing.T) {
	cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "m1"}}}
	codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "m1"}}}
	srv := newTestHTTPServer(ServerOptions{
		Providers:   []providers.Provider{cn, codearts},
		APIKeyRealm: func(*http.Request) string { return "cn" },
	})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["realm"] != "gateway" {
		t.Fatalf("realm = %#v, want gateway scope", body["realm"])
	}
	data := body["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "cn/m1" {
		t.Fatalf("data = %#v, want only cn/m1", body["data"])
	}
	for _, raw := range data {
		if raw.(map[string]any)["realm"] == "codearts" {
			t.Fatalf("bound key leaked another exit: %#v", data)
		}
	}
	if codearts.modelCalls != 0 {
		t.Fatalf("bound gateway read an unbound provider catalog: %d calls", codearts.modelCalls)
	}
}

func TestModelsConcreteRealmAddsQualifiedID(t *testing.T) {
	cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "m1"}}}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{cn}})

	rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=cn", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	data := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data = %#v", body["data"])
	}
	entry := data[0].(map[string]any)
	if entry["id"] != "m1" {
		t.Fatalf("concrete realm id = %#v, want bare upstream id", entry["id"])
	}
	if entry["qualified_id"] != "cn/m1" || entry["upstream_id"] != "m1" {
		t.Fatalf("concrete realm identity = %#v", entry)
	}
	if entry["provider"] != "workbuddy" || entry["realm"] != "cn" {
		t.Fatalf("concrete realm attribution = %#v", entry)
	}
	if cn.models[0]["qualified_id"] != nil {
		t.Fatalf("concrete realm decoration mutated the provider-owned model: %#v", cn.models[0])
	}
}
