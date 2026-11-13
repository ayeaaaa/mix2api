package raccoon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

const catalogFixture = `{
  "code": 0,
  "data": {
    "categories": [
      {
        "type": "chat",
        "default_model": "raccoon-090dd3",
        "models": [
          {
            "name": "raccoon-090dd3",
            "display_name": "Raccoon-Work",
            "params": {"context_window": 1000000, "max_tokens": 100000},
            "tags": ["general", "code", "vision", "reasoning"]
          },
          {
            "name": "sn-glm-5-3",
            "display_name": "GLM-5-3",
            "params": {"context_window": 1000000, "max_tokens": 100000},
            "tags": ["code", "reasoning"]
          }
        ]
      }
    ]
  }
}`

var catalogSeedIDs = []string{
	"raccoon-090dd3",
	"raccoon-19b265",
	"raccoon-405a1c",
	"sn-sensenova-6-8-flash",
	"sn-sensenova-6-8-flash-lite",
	"sn-glm-5-3",
	"sn-kimi-k3",
	"sn-glm-5-3-flash",
	"sn-deepseek-v4-1-flash-1017",
}

const catalogUnionAccountA = `{"code":0,"data":{"categories":[{"type":"chat","models":[
  {"name":"raccoon-090dd3","display_name":"Raccoon A","tags":["vision"]},
  {"name":"shared-model","display_name":"Shared A","tags":["code"]}
]}]}}`

const catalogUnionAccountB = `{"code":0,"data":{"categories":[{"type":"chat","models":[
  {"name":"sn-glm-5-3","display_name":"GLM B","tags":["reasoning"]},
  {"name":"Shared-Model","display_name":"Shared B",
   "params":{"context_window":128000,"max_tokens":4096},"tags":["vision"]}
]}]}}`

// fakeCatalogUpstream is the deterministic Go translation of v1's
// _fake_raccoon.py catalog/refresh subset. No test reaches the real gateway.
type fakeCatalogUpstream struct {
	mu sync.Mutex

	catalogCalls int
	refreshCalls int

	catalogStatus    int
	catalogBody      string
	catalogErrorBody string
	allowedToken     string
	authStatus       int

	// Per-token overrides keyed by the bearer token without the "Bearer "
	// prefix let one fake model several accounts with distinct catalogs.
	catalogByToken       map[string]string
	catalogStatusByToken map[string]int

	refreshStatus        int
	refreshedToken       string
	refreshedRT          string
	expectedRefreshToken string
}

func (f *fakeCatalogUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == CatalogPath:
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.catalogCalls++
		status := f.catalogStatus
		body := f.catalogBody
		errorBody := f.catalogErrorBody
		allowed := f.allowedToken
		authStatus := f.authStatus
		if perToken, ok := f.catalogStatusByToken[token]; ok {
			status = perToken
		}
		if perToken, ok := f.catalogByToken[token]; ok {
			body = perToken
		}
		f.mu.Unlock()

		if allowed != "" && r.Header.Get("Authorization") != "Bearer "+allowed {
			if authStatus == 0 {
				authStatus = http.StatusUnauthorized
			}
			writeCatalogJSON(w, authStatus, map[string]any{
				"message": "invalid access token",
			})
			return
		}
		if status == 0 {
			status = http.StatusOK
		}
		if status != http.StatusOK {
			if errorBody == "" {
				errorBody = `{"message":"catalog failed"}`
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(errorBody))
			return
		}
		if body == "" {
			body = catalogFixture
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))

	case r.Method == http.MethodPost && r.URL.Path == RefreshPath:
		var requestBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&requestBody)

		f.mu.Lock()
		f.refreshCalls++
		status := f.refreshStatus
		token := f.refreshedToken
		refreshToken := f.refreshedRT
		expectedRefreshToken := f.expectedRefreshToken
		f.mu.Unlock()

		if expectedRefreshToken != "" {
			supplied, _ := requestBody["refresh_token"].(string)
			if supplied != expectedRefreshToken {
				writeCatalogJSON(w, http.StatusUnauthorized, map[string]any{
					"message": "invalid refresh token",
				})
				return
			}
		}
		if status == 0 {
			status = http.StatusOK
		}
		if status != http.StatusOK {
			writeCatalogJSON(w, status, map[string]any{"message": "refresh rejected"})
			return
		}
		if token == "" {
			writeCatalogJSON(w, http.StatusInternalServerError, map[string]any{
				"message": "test refresh token missing",
			})
			return
		}
		if refreshToken == "" {
			refreshToken = "refresh-u-catalog"
		}
		writeCatalogJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"access_token":  token,
				"refresh_token": refreshToken,
				"token_type":    "Bearer",
				"expires_at":    time.Now().Add(time.Hour).Unix(),
			},
		})

	default:
		writeCatalogJSON(w, http.StatusNotFound, map[string]any{"message": "not found"})
	}
}

func (f *fakeCatalogUpstream) counts() (catalog, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catalogCalls, f.refreshCalls
}

func writeCatalogJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func configureCatalogPool(t *testing.T, p *Pool, baseURL string, now func() time.Time) {
	t.Helper()
	client := transport.NewClient(transport.ClientOptions{})
	p.SetHTTPClient(NewHTTPClient(client, HTTPClientOptions{BaseURL: baseURL}))
	p.SetTokenManager(p.NewTokenManager(client, TokenManagerOptions{
		BaseURL: baseURL,
		Now:     now,
		Timeout: 2 * time.Second,
	}))
}

func catalogIDs(models []map[string]any) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func catalogByID(models []map[string]any, id string) map[string]any {
	for _, model := range models {
		if model["id"] == id {
			return model
		}
	}
	return nil
}

func catalogAccountToken(t *testing.T, p *Pool, uid string) string {
	t.Helper()
	account := p.Get(uid)
	if account == nil {
		t.Fatalf("account %q not found", uid)
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.Auth.AccessToken
}

func TestCatalogBareIDs(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-catalog", "", now.Add(time.Hour), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	fake := &fakeCatalogUpstream{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureCatalogPool(t, p, server.URL, func() time.Time { return now })

	models, err := p.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	ids := catalogIDs(models)
	if len(ids) != 2 || ids[0] != "raccoon-090dd3" || ids[1] != "sn-glm-5-3" {
		t.Fatalf("catalog ids = %v, want bare [raccoon-090dd3 sn-glm-5-3]", ids)
	}
	for _, id := range ids {
		if strings.ContainsAny(id, "/:") {
			t.Fatalf("model id %q was prefixed or namespaced", id)
		}
	}

	raccoon := catalogByID(models, "raccoon-090dd3")
	if raccoon["name"] != "Raccoon-Work" {
		t.Fatalf("display name = %v", raccoon["name"])
	}
	for _, key := range []string{"context_window", "context_length", "max_input_tokens"} {
		if raccoon[key] != int64(1000000) {
			t.Fatalf("%s = %v, want 1000000", key, raccoon[key])
		}
	}
	for _, key := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens", "max_output"} {
		if raccoon[key] != int64(100000) {
			t.Fatalf("%s = %v, want 100000", key, raccoon[key])
		}
	}
	if raccoon["vision"] != true {
		t.Fatalf("raccoon capabilities = %#v", raccoon)
	}
	capabilities, _ := raccoon["capabilities"].(map[string]any)
	if capabilities["vision"] != true || capabilities["reasoning"] != true || capabilities["tool_calls"] != true {
		t.Fatalf("raccoon capability map = %#v", raccoon["capabilities"])
	}
	if raccoon["supports_vision"] != true || raccoon["supports_tool_calls"] != true || raccoon["supports_reasoning"] != true {
		t.Fatalf("raccoon capability spellings = %#v", raccoon)
	}
	if raccoon["object"] != "model" || raccoon["owned_by"] != "sensetime" || raccoon["provider"] != "raccoon" {
		t.Fatalf("raccoon wire fields = %#v", raccoon)
	}

	glm := catalogByID(models, "sn-glm-5-3")
	if glm["vision"] != false || glm["supports_reasoning"] != true {
		t.Fatalf("glm capabilities = %#v", glm)
	}

	deepseek := catalogModelEntry("sn-deepseek-v4-1-flash-1017", "DeepSeek V4.1 Flash 1017",
		1000000, 100000, []string{"reasoning"}, 0)
	efforts, _ := deepseek["reasoning_efforts"].([]string)
	hasMax, hasHigh := false, false
	for _, effort := range efforts {
		hasMax = hasMax || effort == "max"
		hasHigh = hasHigh || effort == "high"
	}
	if !hasMax || !hasHigh {
		t.Fatalf("1017 reasoning_efforts = %#v, want max/high", deepseek["reasoning_efforts"])
	}
}

func TestCatalogCacheAndForce(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-cache", "", now.Add(time.Hour), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	fake := &fakeCatalogUpstream{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureCatalogPool(t, p, server.URL, func() time.Time { return now })

	if _, err := p.Models(context.Background(), false); err != nil {
		t.Fatalf("first Models: %v", err)
	}
	if catalog, _ := fake.counts(); catalog != 1 {
		t.Fatalf("first Models catalog calls = %d, want 1", catalog)
	}

	if _, err := p.Models(context.Background(), false); err != nil {
		t.Fatalf("second Models: %v", err)
	}
	if catalog, _ := fake.counts(); catalog != 1 {
		t.Fatalf("cached Models catalog calls = %d, want 1", catalog)
	}

	now = now.Add(599 * time.Second)
	if _, err := p.Models(context.Background(), false); err != nil {
		t.Fatalf("Models before TTL: %v", err)
	}
	if catalog, _ := fake.counts(); catalog != 1 {
		t.Fatalf("Models before TTL catalog calls = %d, want 1", catalog)
	}

	now = now.Add(2 * time.Second)
	if _, err := p.Models(context.Background(), false); err != nil {
		t.Fatalf("Models after TTL: %v", err)
	}
	if catalog, _ := fake.counts(); catalog != 2 {
		t.Fatalf("Models after TTL catalog calls = %d, want 2", catalog)
	}

	if _, err := p.Models(context.Background(), true); err != nil {
		t.Fatalf("forced Models: %v", err)
	}
	if catalog, _ := fake.counts(); catalog != 3 {
		t.Fatalf("forced Models catalog calls = %d, want 3", catalog)
	}
}

func TestCatalogSeedFallback(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-seed", "", now.Add(time.Hour), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	fake := &fakeCatalogUpstream{catalogStatus: http.StatusInternalServerError}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureCatalogPool(t, p, server.URL, func() time.Time { return now })

	account := p.Get("u-seed")
	account.mu.Lock()
	accessToken := account.Auth.AccessToken
	refreshToken := account.Auth.RefreshToken
	account.mu.Unlock()
	fake.mu.Lock()
	fake.catalogStatus = http.StatusInternalServerError
	fake.catalogErrorBody = `{"message":"` + accessToken + `","refresh_token":"` + refreshToken + `"}`
	fake.mu.Unlock()

	_, err := p.FetchCatalog(context.Background(), true)
	if err == nil {
		t.Fatal("FetchCatalog unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), accessToken) || strings.Contains(err.Error(), refreshToken) {
		t.Fatalf("catalog error leaked a token: %v", err)
	}

	models, err := p.Models(context.Background(), true)
	if err != nil {
		t.Fatalf("Models fallback: %v", err)
	}
	ids := catalogIDs(models)
	if len(ids) != len(catalogSeedIDs) {
		t.Fatalf("seed model count = %d, want %d (%v)", len(ids), len(catalogSeedIDs), ids)
	}
	for index, want := range catalogSeedIDs {
		if ids[index] != want {
			t.Fatalf("seed ids[%d] = %q, want %q (all %v)", index, ids[index], want, ids)
		}
	}
	for _, id := range ids {
		if strings.ContainsAny(id, "/:") {
			t.Fatalf("seed id %q was prefixed", id)
		}
	}

	// An empty live catalog is a failure, not a successful empty model list.
	fake.mu.Lock()
	fake.catalogStatus = http.StatusOK
	fake.catalogBody = `{"data":{"categories":[{"type":"chat","models":[]}]}}`
	fake.mu.Unlock()
	models, err = p.Models(context.Background(), true)
	if err != nil {
		t.Fatalf("Models empty-catalog fallback: %v", err)
	}
	if len(catalogIDs(models)) != len(catalogSeedIDs) {
		t.Fatalf("empty catalog did not fall back to seeds: %v", catalogIDs(models))
	}
}

func TestCatalogFailureKeepsLastGood(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-last", "", now.Add(time.Hour), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	fake := &fakeCatalogUpstream{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureCatalogPool(t, p, server.URL, func() time.Time { return now })

	first, err := p.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("first Models: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first catalog size = %d, want 2", len(first))
	}

	fake.mu.Lock()
	fake.catalogStatus = http.StatusBadGateway
	fake.mu.Unlock()

	// Drop the shared cache so the account's own last good catalog is the
	// fallback that must survive a forced refresh failure.
	p.catalogMu.Lock()
	p.catalog = catalogCache{}
	p.catalogMu.Unlock()

	after, err := p.Models(context.Background(), true)
	if err != nil {
		t.Fatalf("failed forced Models returned an error: %v", err)
	}
	if got, want := catalogIDs(after), []string{"raccoon-090dd3", "sn-glm-5-3"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("last good catalog = %v, want %v", got, want)
	}
}

func TestCatalogUnionAcrossAccounts(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-a", "", now.Add(time.Hour), nil)
	writePoolAuth(t, dir, "u-b", "", now.Add(time.Hour), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	fake := &fakeCatalogUpstream{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureCatalogPool(t, p, server.URL, func() time.Time { return now })

	tokenA := catalogAccountToken(t, p, "u-a")
	tokenB := catalogAccountToken(t, p, "u-b")
	fake.mu.Lock()
	fake.catalogByToken = map[string]string{
		tokenA: catalogUnionAccountA,
		tokenB: catalogUnionAccountB,
	}
	fake.mu.Unlock()

	models, err := p.FetchCatalog(context.Background(), true)
	if err != nil {
		t.Fatalf("FetchCatalog union: %v", err)
	}
	ids := catalogIDs(models)
	if len(ids) != 3 || ids[0] != "raccoon-090dd3" || ids[1] != "shared-model" || ids[2] != "sn-glm-5-3" {
		t.Fatalf("union ids = %v, want [raccoon-090dd3 shared-model sn-glm-5-3]", ids)
	}
	if catalog, _ := fake.counts(); catalog != 2 {
		t.Fatalf("catalog calls = %d, want one per account (2)", catalog)
	}

	shared := catalogByID(models, "shared-model")
	if shared["name"] != "Shared A" {
		t.Fatalf("merged display name = %v, want first account name", shared["name"])
	}
	if shared["context_window"] != int64(128000) || shared["max_tokens"] != int64(4096) {
		t.Fatalf("merged limits = %#v", shared)
	}
	if shared["vision"] != true || shared["reasoning"] != false {
		t.Fatalf("merged capabilities = %#v", shared)
	}
	tags, _ := shared["tags"].([]string)
	if len(tags) != 2 || tags[0] != "code" || tags[1] != "vision" {
		t.Fatalf("merged tags = %#v", shared["tags"])
	}

	// The shared cache is now the union too, so a normal Models read uses it.
	wire, err := p.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models after union fetch: %v", err)
	}
	if got := catalogIDs(wire); len(got) != 3 || got[1] != "shared-model" {
		t.Fatalf("cached union ids = %v", got)
	}
}

func TestCatalogPartialFailureKeepsHealthyAccount(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-a", "", now.Add(time.Hour), nil)
	writePoolAuth(t, dir, "u-b", "", now.Add(time.Hour), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	fake := &fakeCatalogUpstream{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureCatalogPool(t, p, server.URL, func() time.Time { return now })

	tokenA := catalogAccountToken(t, p, "u-a")
	tokenB := catalogAccountToken(t, p, "u-b")
	fake.mu.Lock()
	fake.catalogByToken = map[string]string{tokenA: catalogUnionAccountA}
	fake.catalogStatusByToken = map[string]int{tokenB: http.StatusBadGateway}
	fake.mu.Unlock()

	models, err := p.FetchCatalog(context.Background(), true)
	if err != nil {
		t.Fatalf("one failed account blocked the healthy one: %v", err)
	}
	ids := catalogIDs(models)
	if len(ids) != 2 || ids[0] != "raccoon-090dd3" || ids[1] != "shared-model" {
		t.Fatalf("partial-failure union ids = %v", ids)
	}
	if catalog, _ := fake.counts(); catalog != 2 {
		t.Fatalf("catalog calls = %d, want one per usable account (2)", catalog)
	}
}

func TestCatalogFailurePreservesSharedCache(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-cache", "", now.Add(time.Hour), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	fake := &fakeCatalogUpstream{}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureCatalogPool(t, p, server.URL, func() time.Time { return now })

	if _, err := p.Models(context.Background(), false); err != nil {
		t.Fatalf("first Models: %v", err)
	}
	p.catalogMu.Lock()
	beforeFetchedAt := p.catalog.fetchedAt
	beforeModels := cloneModels(p.catalog.models)
	p.catalogMu.Unlock()

	// Empty the account snapshot so failure must adopt the shared cache rather
	// than its own last-good catalog.
	account := p.Get("u-cache")
	account.mu.Lock()
	account.Models = nil
	account.mu.Unlock()

	fake.mu.Lock()
	fake.catalogStatus = http.StatusBadGateway
	fake.mu.Unlock()

	models, err := p.Models(context.Background(), true)
	if err != nil {
		t.Fatalf("failure fallback returned an error: %v", err)
	}
	if got, want := catalogIDs(models), []string{"raccoon-090dd3", "sn-glm-5-3"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("failure fallback ids = %v, want %v", got, want)
	}

	p.catalogMu.Lock()
	afterFetchedAt := p.catalog.fetchedAt
	afterModels := cloneModels(p.catalog.models)
	p.catalogMu.Unlock()
	if !afterFetchedAt.Equal(beforeFetchedAt) || len(afterModels) != len(beforeModels) {
		t.Fatalf("failed refresh mutated the shared cache: before=%v after=%v", beforeModels, afterModels)
	}
	if got, want := catalogIDs(afterModels), catalogIDs(beforeModels); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("shared cache ids changed: %v -> %v", want, got)
	}

	account.mu.Lock()
	adopted := cloneModels(account.Models)
	account.mu.Unlock()
	if got := catalogIDs(adopted); len(got) != 2 || got[0] != "raccoon-090dd3" {
		t.Fatalf("failed account did not adopt the shared cache: %v", got)
	}
}

func TestCatalogNoAccountsReturnsSeeds(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	p := mustPool(t, t.TempDir(), Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	p.catalogMu.Lock()
	p.catalog = catalogCache{
		models: []map[string]any{{
			"id":             "cached-only",
			"name":           "cached-only",
			"context_window": int64(1),
			"max_tokens":     int64(1),
			"tags":           []string{},
			"vision":         false,
			"reasoning":      false,
		}},
		fetchedAt: now.Add(-24 * time.Hour),
		source:    "raccoon",
	}
	p.catalogMu.Unlock()

	models, err := p.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models with no accounts: %v", err)
	}
	ids := catalogIDs(models)
	if len(ids) != len(catalogSeedIDs) || ids[0] != catalogSeedIDs[0] || ids[8] != catalogSeedIDs[8] {
		t.Fatalf("no-account ids = %v, want nine seeds", ids)
	}
	if catalogByID(models, "cached-only") != nil {
		t.Fatal("no-account Models returned the stale shared cache")
	}
}

func TestCatalogNilContext(t *testing.T) {
	p := mustPool(t, t.TempDir(), Config{MaxConcurrent: 1})
	if _, err := p.Models(nil, false); err == nil {
		t.Fatal("Models with a nil context unexpectedly succeeded")
	}
	if _, err := p.FetchCatalog(nil, true); err == nil {
		t.Fatal("FetchCatalog with a nil context unexpectedly succeeded")
	}
}

func TestCatalogUsesAccountProxy(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-proxy", "", now.Add(time.Hour), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	target := &fakeCatalogUpstream{}
	targetServer := httptest.NewServer(target)
	defer targetServer.Close()
	configureCatalogPool(t, p, targetServer.URL, func() time.Time { return now })

	var proxyHits int32
	var proxyMu sync.Mutex
	proxyPath := ""
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		proxyMu.Lock()
		proxyPath = r.URL.Path
		proxyMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(catalogFixture))
	}))
	defer proxy.Close()

	p.SetProxySlots([]transport.Slot{{ID: "slot-1", URL: proxy.URL, Enabled: true}})
	if err := p.SetProxySlot("u-proxy", "slot-1"); err != nil {
		t.Fatalf("SetProxySlot: %v", err)
	}

	models, err := p.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models through proxy: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("proxied catalog size = %d, want 2", len(models))
	}
	if hits := atomic.LoadInt32(&proxyHits); hits != 1 {
		t.Fatalf("proxy hits = %d, want 1", hits)
	}
	proxyMu.Lock()
	gotProxyPath := proxyPath
	proxyMu.Unlock()
	if gotProxyPath != CatalogPath {
		t.Fatalf("proxy path = %q, want %q", gotProxyPath, CatalogPath)
	}
	if catalog, _ := target.counts(); catalog != 0 {
		t.Fatalf("direct target was hit %d times despite an account proxy", catalog)
	}
}

func TestCatalog401RefreshesOnceAndRetries(t *testing.T) {
	testCatalogAuthRefresh(t, http.StatusUnauthorized)
}

func TestCatalog403RefreshesOnceAndRetries(t *testing.T) {
	testCatalogAuthRefresh(t, http.StatusForbidden)
}

func testCatalogAuthRefresh(t *testing.T, rejectedStatus int) {
	t.Helper()
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-catalog", "", now.Add(time.Hour), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	newToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-catalog",
		"exp": now.Add(2 * time.Hour).Unix(),
		"jti": "catalog-refreshed-token",
	})
	fake := &fakeCatalogUpstream{
		allowedToken:         newToken,
		authStatus:           rejectedStatus,
		refreshedToken:       newToken,
		refreshedRT:          "refresh-u-catalog",
		expectedRefreshToken: "refresh-u-catalog",
	}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := transport.NewClient(transport.ClientOptions{})
	p.SetHTTPClient(NewHTTPClient(client, HTTPClientOptions{BaseURL: server.URL}))
	p.SetTokenManager(p.NewTokenManager(client, TokenManagerOptions{
		BaseURL: server.URL,
		Now:     func() time.Time { return now },
		Timeout: 2 * time.Second,
	}))

	models, err := p.Models(context.Background(), true)
	if err != nil {
		t.Fatalf("Models after %d: %v", rejectedStatus, err)
	}
	if len(models) != 2 {
		t.Fatalf("catalog after %d has %d models, want 2", rejectedStatus, len(models))
	}
	catalogCalls, refreshCalls := fake.counts()
	if catalogCalls != 2 {
		t.Fatalf("catalog calls = %d, want 2 (one %d + one retry)", catalogCalls, rejectedStatus)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshCalls)
	}
	account := p.Get("u-catalog")
	account.mu.Lock()
	gotToken := account.Auth.AccessToken
	account.mu.Unlock()
	if gotToken != newToken {
		t.Fatal("account did not publish the refreshed access token")
	}
}

func TestCatalogNormalizesFirstChatCategory(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	longName := strings.Repeat("x", 140)
	fixture := `{
	  "code": 0,
	  "data": {
	    "categories": [
	      {"type": "image", "models": [{"name": "image-1"}]},
	      {"type": " Chat ", "default_model": " raccoon-090dd3 ", "models": [
	        {
	          "name": " raccoon-090dd3 ",
	          "display_name": " ` + longName + ` ",
	          "params": {"context_length": "1000000", "max_output_tokens": "100000"},
	          "tags": ["General", " VISION ", "reasoning", "vision"]
	        },
	        {
	          "name": "sn-glm-5-3",
	          "display_name": "GLM-5-3",
	          "params": {"max_input_tokens": 1000000, "max_completion_tokens": 100000},
	          "tags": "code, reasoning"
	        },
	        {
	          "name": "SN-GLM-5-3",
	          "display_name": "duplicate",
	          "params": {"context_window": 1}
	        },
	        {"id": "sn-kimi-k3", "description": "Kimi K3"},
	        {"name": ""},
	        {"name": false},
	        {"name": 0}
	      ]},
	      {"type": "chat", "models": [{"name": "second-chat"}]}
	    ]
	  }
	}`

	dir := t.TempDir()
	writePoolAuth(t, dir, "u-normalize", "", now.Add(time.Hour), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, ModelsTTLSeconds: 600})
	p.now = func() time.Time { return now }

	fake := &fakeCatalogUpstream{catalogBody: fixture}
	server := httptest.NewServer(fake)
	defer server.Close()
	configureCatalogPool(t, p, server.URL, func() time.Time { return now })

	models, err := p.FetchCatalog(context.Background(), true)
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	ids := catalogIDs(models)
	if len(ids) != 3 {
		t.Fatalf("normalized ids = %v, want three (image/second chat ignored)", ids)
	}
	if ids[0] != "raccoon-090dd3" || ids[1] != "sn-glm-5-3" || ids[2] != "sn-kimi-k3" {
		t.Fatalf("normalized ids = %v", ids)
	}

	raccoon := catalogByID(models, "raccoon-090dd3")
	name, _ := raccoon["name"].(string)
	if len([]rune(name)) != 120 {
		t.Fatalf("display name rune length = %d, want 120", len([]rune(name)))
	}
	tags, _ := raccoon["tags"].([]string)
	if len(tags) != 3 || tags[0] != "general" || tags[1] != "vision" || tags[2] != "reasoning" {
		t.Fatalf("normalized tags = %#v", raccoon["tags"])
	}
	if raccoon["vision"] != true || raccoon["reasoning"] != true {
		t.Fatalf("normalized capabilities = %#v", raccoon)
	}
	if raccoon["context_window"] != int64(1000000) || raccoon["max_tokens"] != int64(100000) {
		t.Fatalf("alias normalization = %#v", raccoon)
	}

	glm := catalogByID(models, "sn-glm-5-3")
	if glm["context_window"] != int64(1000000) || glm["max_tokens"] != int64(100000) {
		t.Fatalf("glm alias normalization = %#v", glm)
	}

	kimi := catalogByID(models, "sn-kimi-k3")
	if kimi["context_window"] != int64(1000000) || kimi["max_tokens"] != int64(100000) {
		t.Fatalf("seed fill for an unseeded context = %#v", kimi)
	}
}
