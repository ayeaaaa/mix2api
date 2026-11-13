package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/agents"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
)

func doAgentRequest(server *Server, method, path, body, remote string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAgentsRoutes(t *testing.T) {
	home := t.TempDir()
	service := agents.New(agents.Service{AccountsDir: filepath.Join(home, "accounts"), Home: home})
	server := New(ServerOptions{Agents: service, PanelAuth: passThroughAuth})
	remote := doAgentRequest(server, http.MethodGet, "/agents", "", "203.0.113.9:5555", nil)
	if remote.Code != http.StatusOK {
		t.Fatalf("remote overview = %d %s", remote.Code, remote.Body)
	}
	if body := decodeBody(t, remote); body["enabled"] != false {
		t.Fatalf("remote body = %#v", body)
	}
	local := doAgentRequest(server, http.MethodGet, "/agents", "", "127.0.0.1:5555", nil)
	if local.Code != http.StatusOK {
		t.Fatalf("local overview = %d %s", local.Code, local.Body)
	}
	body := decodeBody(t, local)
	clients, _ := body["clients"].([]any)
	if len(clients) != 6 || body["enabled"] != true {
		t.Fatalf("local body = %#v", body)
	}
	available := doAgentRequest(server, http.MethodGet, "/agents/available", "", "203.0.113.9:5555", nil)
	if decodeBody(t, available)["enabled"] != false {
		t.Fatalf("available = %s", available.Body)
	}
	selection := doAgentRequest(server, http.MethodPost, "/agents/model-selection", `{"models":["m2","m1"]}`, "127.0.0.1:5555", nil)
	if selection.Code != http.StatusOK || decodeBody(t, selection)["configured"] != true {
		t.Fatalf("selection = %d %s", selection.Code, selection.Body)
	}
	applyBody := `{"client":"claude-code","base_url":"http://gateway:8788","models":[{"id":"m1"}],"key_id":"__global"}`
	applied := doAgentRequest(server, http.MethodPost, "/agents/apply", applyBody, "127.0.0.1:5555", nil)
	if applied.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", applied.Code, applied.Body)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatalf("apply did not write config: %v", err)
	}
	forbidden := doAgentRequest(server, http.MethodPost, "/agents/apply", applyBody, "203.0.113.9:5555", nil)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("remote apply = %d %s", forbidden.Code, forbidden.Body)
	}
	restored := doAgentRequest(server, http.MethodPost, "/agents/restore", `{"client":"claude-code"}`, "127.0.0.1:5555", nil)
	if restored.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", restored.Code, restored.Body)
	}
	// CLI is public even from a remote address.
	cli := doAgentRequest(server, http.MethodGet, "/agents/cli", "", "203.0.113.9:5555", nil)
	if cli.Code != http.StatusOK || !strings.Contains(cli.Body.String(), "wb_agents.py") && !strings.Contains(cli.Body.String(), "argparse") {
		t.Fatalf("cli = %d %s", cli.Code, cli.Body.String()[:minIntAPI(80, cli.Body.Len())])
	}
	if !strings.Contains(cli.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("cli content type = %s", cli.Header().Get("Content-Type"))
	}
}

func minIntAPI(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestAgentsAuthRequiredHonorsAuthDisabled(t *testing.T) {
	home := t.TempDir()
	accounts := filepath.Join(home, "accounts")
	if err := os.MkdirAll(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	store := config.New(filepath.Join(accounts, "settings.json"))
	keyStore := keys.New(store)
	if err := keyStore.SetAuthDisabled(true); err != nil {
		t.Fatal(err)
	}
	service := agents.New(agents.Service{AccountsDir: accounts, Home: home})
	server := New(ServerOptions{
		Agents: service, KeyStore: keyStore, StaticAPIKey: "static-key-value",
		PanelAuth: passThroughAuth,
	})
	rec := doAgentRequest(server, http.MethodGet, "/agents", "", "127.0.0.1:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["global_key_set"] != true {
		t.Fatalf("global_key_set = %#v", body["global_key_set"])
	}
	if body["auth_required"] != false {
		t.Fatalf("auth_required = %#v, want false with auth_disabled", body["auth_required"])
	}
}

func TestAgentsStaticKeyUsed(t *testing.T) {
	home := t.TempDir()
	service := agents.New(agents.Service{AccountsDir: filepath.Join(home, "accounts"), Home: home})
	server := New(ServerOptions{Agents: service, PanelAuth: passThroughAuth, StaticAPIKey: "static-key-value"})
	body := `{"client":"claude-code","base_url":"http://gateway","models":[{"id":"m1"}],"key_id":"__global"}`
	rec := doAgentRequest(server, http.MethodPost, "/agents/apply", body, "127.0.0.1:5555", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "static-key-value") {
		t.Fatalf("static key not written: %s", data)
	}
}
