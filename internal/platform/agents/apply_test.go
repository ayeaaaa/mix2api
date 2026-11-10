package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type agentTestProvider struct{}

func (p *agentTestProvider) ID() string    { return "workbuddy" }
func (p *agentTestProvider) Realm() string { return "intl" }
func (p *agentTestProvider) Overview(context.Context, bool) (any, error) {
	return nil, nil
}
func (p *agentTestProvider) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *agentTestProvider) Models(context.Context, bool) ([]providers.Model, error) {
	return []providers.Model{
		{"id": "model-a", "name": "Model A", "context_window": 200000, "max_output": 32000, "supports_vision": true, "reasoning_efforts": []any{"low", "high"}},
	}, nil
}
func (p *agentTestProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}
func (p *agentTestProvider) Routes() []providers.Route { return nil }
func (p *agentTestProvider) Jobs() []scheduler.Job     { return nil }
func (p *agentTestProvider) AgentModelSpecs(context.Context) ([]providers.AgentModelSpec, error) {
	return []providers.AgentModelSpec{{ID: "model-a", Name: "Model A", ContextWindow: 200000, MaxOutputTokens: 32000, ReasoningEfforts: []string{"low", "high"}, SupportsVision: true}}, nil
}

func newAgentService(t *testing.T) (*Service, string, string) {
	t.Helper()
	home := t.TempDir()
	accounts := filepath.Join(t.TempDir(), "accounts")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	return New(Service{AccountsDir: accounts, Home: home, Providers: []providers.Provider{&agentTestProvider{}}}), accounts, home
}

func TestApplyClaudeCode(t *testing.T) {
	service, _, home := newAgentService(t)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"env":{"KEEP":"yes"},"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := service.Apply(context.Background(), "claude-code", "http://gateway:8788/v1", "sk-key", "model-a", []ModelSpec{{ID: "model-a"}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := loadJSON(path, "claude")
	env := data["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "http://gateway:8788" || env["ANTHROPIC_AUTH_TOKEN"] != "sk-key" {
		t.Fatalf("env = %#v", env)
	}
	if env["KEEP"] != "yes" || data["theme"] != "dark" {
		t.Fatalf("existing keys lost: %#v", data)
	}
	for _, name := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
		if env[name] != "model-a" {
			t.Fatalf("%s = %#v", name, env[name])
		}
	}
}

func TestApplyCodexTOML(t *testing.T) {
	service, _, home := newAgentService(t)
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "approval_policy = \"never\"\n[model_providers.wb-proxy]\nenv_key = \"OLD\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"keep":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(context.Background(), "codex", "http://gateway:8788", "sk-key", "model-a", []ModelSpec{{ID: "model-a"}}); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	for _, want := range []string{`model_provider = "wb-proxy"`, `model = "model-a"`, `base_url = "http://gateway:8788/v1"`, `wire_api = "responses"`, `experimental_bearer_token = "sk-key"`} {
		if !strings.Contains(string(text), want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(string(text), "env_key") {
		t.Fatalf("env_key survived:\n%s", text)
	}
	auth, _ := loadJSON(filepath.Join(dir, "auth.json"), "auth")
	if auth["OPENAI_API_KEY"] != "sk-key" || auth["keep"] != true {
		t.Fatalf("auth = %#v", auth)
	}
}

func TestApplyOpenCode(t *testing.T) {
	service, _, home := newAgentService(t)
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"theme":"x","provider":{"wb-proxy":{"models":{"old":{}}},"other":{"name":"keep"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	models := []ModelSpec{{ID: "model-a", ContextWindow: 200000, MaxOutput: 32000}, {ID: "model-b"}}
	if _, err := service.Apply(context.Background(), "opencode", "http://gateway:8788", "sk-key", "model-a", models); err != nil {
		t.Fatal(err)
	}
	data, _ := loadJSON(path, "opencode")
	if data["$schema"] != "https://opencode.ai/config.json" || data["model"] != "wb-proxy/model-a" || data["theme"] != "x" {
		t.Fatalf("top-level = %#v", data)
	}
	providersMap := data["provider"].(map[string]any)
	entry := providersMap["wb-proxy"].(map[string]any)
	if entry["npm"] != "@ai-sdk/openai-compatible" || entry["name"] != "WB Proxy" {
		t.Fatalf("provider = %#v", entry)
	}
	modelsMap := entry["models"].(map[string]any)
	if _, ok := modelsMap["old"]; ok {
		t.Fatalf("selection snapshot was merged instead of replaced: %#v", modelsMap)
	}
	if modelsMap["model-a"].(map[string]any)["limit"].(map[string]any)["context"] != float64(200000) {
		t.Fatalf("limits = %#v", modelsMap["model-a"])
	}
	if _, ok := providersMap["other"]; !ok {
		t.Fatal("unrelated provider lost")
	}
}

func TestApplyDSH(t *testing.T) {
	service, _, home := newAgentService(t)
	patch := filepath.Join(home, ".dsh", "profiles", "desktop", "cordis.patch.yml")
	if err := os.MkdirAll(filepath.Dir(patch), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "- id: other-plugin\n  name: keep-me\n  config:\n    enabled: true\n- id: llm-pi-ai\n  config:\n    keep-inside: yes\n    providers:\n      other-provider:\n        api: keep\n"
	if err := os.WriteFile(patch, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, ".dsh", "settings.yaml")
	if err := os.WriteFile(settings, []byte("agent-default-model:\n  provider: old\n  model: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	models := []ModelSpec{{ID: "model-a", ContextWindow: 200000, MaxOutput: 32000, ReasoningEfforts: []string{"low", "high"}, SupportsVision: true}}
	if _, err := service.Apply(context.Background(), "dsh", "http://gateway:8788", "sk-key", "model-a", models); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(patch)
	for _, want := range []string{"other-plugin", "keep-me", "llm-pi-ai", "keep-inside", "other-provider", "wb-proxy", "reasoningEfforts", "contextWindow", "input:", "- text", "- image"} {
		if !strings.Contains(string(text), want) {
			t.Fatalf("patch missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(string(text), "agent-default-model") || !strings.Contains(string(text), "model: model-a") {
		t.Fatalf("default model missing:\n%s", text)
	}
	cred, err := readText(filepath.Join(home, ".dsh", ".credentials.yaml"))
	if err != nil || !strings.Contains(cred, "WB_PROXY_API_KEY: sk-key") {
		t.Fatalf("credentials = %q err=%v", cred, err)
	}
	legacy, _ := readText(settings)
	if !strings.Contains(legacy, "wb-proxy") || !strings.Contains(legacy, "model-a") {
		t.Fatalf("legacy settings = %s", legacy)
	}
}

func TestApplyCrushAndZCode(t *testing.T) {
	service, _, home := newAgentService(t)
	if _, err := service.Apply(context.Background(), "crush", "http://gateway:8788", "sk-key", "", []ModelSpec{{ID: "model-a", ContextWindow: 1000, MaxOutput: 100}}); err != nil {
		t.Fatal(err)
	}
	crush, _ := loadJSON(filepath.Join(home, ".config", "crush", "crush.json"), "crush")
	provider := crush["providers"].(map[string]any)["wb-proxy"].(map[string]any)
	if provider["base_url"] != "http://gateway:8788/v1" || provider["api_key"] != "sk-key" {
		t.Fatalf("crush provider = %#v", provider)
	}
	if _, err := service.Apply(context.Background(), "zcode", "http://gateway:8788", "sk-key", "", []ModelSpec{{ID: "model-a"}}); err != nil {
		t.Fatal(err)
	}
	zcode, _ := loadJSON(filepath.Join(home, ".zcode", "v2", "provider_config.json"), "zcode")
	cfg := zcode["config"].(map[string]any)
	if cfg["providerOrder"].([]any)[0] != "wb-proxy" {
		t.Fatalf("zcode order = %#v", cfg["providerOrder"])
	}
	rules := cfg["providerConfigRules"].(map[string]any)["providerRules"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["providerId"] != "wb-proxy" {
		t.Fatalf("zcode rules = %#v", rules)
	}
}

func TestRestoreAndExternalChange(t *testing.T) {
	service, accounts, home := newAgentService(t)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\"env\":{\"KEEP\":\"yes\"}}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(home, ".config", "crush", "crush.json")
	if _, err := service.Apply(context.Background(), "claude-code", "http://gateway:8788", "sk-key", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Apply(context.Background(), "crush", "http://gateway:8788", "sk-key", "", []ModelSpec{{ID: "model-a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(created); err != nil {
		t.Fatal("crush config was not created")
	}
	overview := service.Overview(context.Background())
	claude := overview["claude-code"].(map[string]any)
	if claude["configured"] != true || claude["applied"].(map[string]any)["external_change"] != false {
		t.Fatalf("overview = %#v", claude)
	}
	// Hand edit after apply is an external change.
	if err := os.WriteFile(path, []byte("{\"env\":{\"KEEP\":\"changed\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claude = service.Overview(context.Background())["claude-code"].(map[string]any)
	if claude["applied"].(map[string]any)["external_change"] != true {
		t.Fatalf("external change not detected: %#v", claude)
	}
	if _, err := service.Restore(context.Background(), "claude-code"); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(path)
	if string(restored) != string(original) {
		t.Fatalf("restore = %q, want %q", restored, original)
	}
	if _, err := service.Restore(context.Background(), "crush"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("created file survived restore: %v", err)
	}
	if _, err := service.Restore(context.Background(), "crush"); err == nil {
		t.Fatal("missing state record restore succeeded")
	}
	_ = accounts
}

func TestModelSelection(t *testing.T) {
	service, _, _ := newAgentService(t)
	if selection, configured := service.Selection(); configured || selection != nil {
		t.Fatalf("initial selection = %#v configured=%v", selection, configured)
	}
	if err := service.SetSelection([]string{"m2", "m1", "m2"}); err != nil {
		t.Fatal(err)
	}
	selection, configured := service.Selection()
	if !configured || len(selection) != 2 || selection[0] != "m2" {
		t.Fatalf("selection = %#v configured=%v", selection, configured)
	}
	if err := service.SetSelection(nil); err != nil {
		t.Fatal(err)
	}
	selection, configured = service.Selection()
	if !configured || len(selection) != 0 {
		t.Fatalf("empty selection = %#v configured=%v", selection, configured)
	}
	// Empty explicit models for an embedding client is a 400-level error.
	if _, err := service.Apply(context.Background(), "opencode", "http://gateway", "k", "", []ModelSpec{}); err == nil {
		t.Fatal("empty embedding model list accepted")
	}
	// Claude Code does not embed models, so empty is harmless.
	if _, err := service.Apply(context.Background(), "claude-code", "http://gateway", "k", "", []ModelSpec{}); err != nil {
		t.Fatalf("claude empty models: %v", err)
	}
}

func TestMaxAgentModels(t *testing.T) {
	models := []ModelSpec{}
	for i := 0; i < 120; i++ {
		models = append(models, ModelSpec{ID: "m" + string(rune('A'+i%26)) + string(rune('a'+i/26))})
	}
	enriched := Enrich(models, nil)
	if len(enriched) != MaxModels {
		t.Fatalf("model cap = %d", len(enriched))
	}
}

func TestApplySerialized(t *testing.T) {
	service, _, home := newAgentService(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.Apply(context.Background(), "opencode", "http://gateway", "k", "", []ModelSpec{{ID: "m1"}})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := loadJSON(filepath.Join(home, ".config", "opencode", "opencode.json"), "opencode")
	if err != nil || data["provider"] == nil {
		t.Fatalf("concurrent apply corrupted config: %#v err=%v", data, err)
	}
}
