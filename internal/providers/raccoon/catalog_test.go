package raccoon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	refreshStatus  int
	refreshedToken string
	refreshedRT    string
}

func (f *fakeCatalogUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == CatalogPath:
		f.mu.Lock()
		f.catalogCalls++
		status := f.catalogStatus
		body := f.catalogBody
		errorBody := f.catalogErrorBody
		allowed := f.allowedToken
		f.mu.Unlock()

		if allowed != "" && r.Header.Get("Authorization") != "Bearer "+allowed {
			writeCatalogJSON(w, http.StatusUnauthorized, map[string]any{
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
		f.mu.Unlock()

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

func TestCatalog401RefreshesOnceAndRetries(t *testing.T) {
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
		allowedToken:   newToken,
		refreshedToken: newToken,
		refreshedRT:    "refresh-u-catalog",
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
		t.Fatalf("Models after 401: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("catalog after 401 has %d models, want 2", len(models))
	}
	catalogCalls, refreshCalls := fake.counts()
	if catalogCalls != 2 {
		t.Fatalf("catalog calls = %d, want 2 (one 401 + one retry)", catalogCalls)
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
