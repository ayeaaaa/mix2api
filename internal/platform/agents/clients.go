package agents

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	ProviderID    = "wb-proxy"
	ProviderName  = "WB Proxy"
	CredentialRef = "WB_PROXY_API_KEY"
)

// Context is the information one client apply needs.
type Context struct {
	BaseURL   string
	BaseURLV1 string
	APIKey    string
	Model     string
	Models    []ModelSpec
	Home      string
}

// Client is one configurable local AI client.
type Client struct {
	ID       string
	Label    string
	Desc     string
	Protocol string
	Files    func(home string) map[string]string
	Probe    func(home string) bool
	Apply    func(paths map[string]string, ctx Context) (map[string]string, error)
}

var dshRetryPolicy = map[string]any{
	"mode": "normal", "maxRetries": 8,
	"backoff": map[string]any{"initialDelayMs": 2000, "maxDelayMs": 30000, "jitterRatio": 0.1},
}

func claudeSettingsPath(home string) string {
	if override := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); override != "" {
		return filepath.Join(override, "settings.json")
	}
	return filepath.Join(home, ".claude", "settings.json")
}

func codexHome(home string) string {
	if override := strings.TrimSpace(os.Getenv("CODEX_HOME")); override != "" {
		return override
	}
	return filepath.Join(home, ".codex")
}

func dshProfilePatches(home string) []string {
	root := filepath.Join(home, ".dsh", "profiles")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "cordis.patch.yml")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func dshFiles(home string) map[string]string {
	files := map[string]string{"credentials": filepath.Join(home, ".dsh", ".credentials.yaml")}
	settings := filepath.Join(home, ".dsh", "settings.yaml")
	patches := dshProfilePatches(home)
	if len(patches) > 0 {
		for index, path := range patches {
			files[fmt.Sprintf("patch-%d", index)] = path
		}
		if info, err := os.Stat(settings); err == nil && !info.IsDir() {
			files["settings"] = settings
		}
	} else {
		files["settings"] = settings
	}
	return files
}

// Clients returns the fixed v1 client registry.
func Clients() []Client {
	return []Client{
		{
			ID: "claude-code", Label: "Claude Code",
			Desc: "Anthropic CLI; points ANTHROPIC_BASE_URL at this gateway.", Protocol: "anthropic",
			Files: func(home string) map[string]string { return map[string]string{"settings": claudeSettingsPath(home)} },
			Probe: func(home string) bool {
				if info, err := os.Stat(filepath.Dir(claudeSettingsPath(home))); err == nil && info.IsDir() {
					return true
				}
				_, err := os.Stat(claudeSettingsPath(home))
				if err == nil {
					return true
				}
				_, err = exec.LookPath("claude")
				return err == nil
			},
			Apply: applyClaudeCode,
		},
		{
			ID: "codex", Label: "Codex CLI",
			Desc: "OpenAI Codex CLI; adds a wb-proxy model provider.", Protocol: "openai",
			Files: func(home string) map[string]string {
				dir := codexHome(home)
				return map[string]string{"config": filepath.Join(dir, "config.toml"), "auth": filepath.Join(dir, "auth.json")}
			},
			Probe: func(home string) bool {
				dir := codexHome(home)
				if info, err := os.Stat(dir); err == nil && info.IsDir() {
					return true
				}
				_, err := exec.LookPath("codex")
				return err == nil
			},
			Apply: applyCodex,
		},
		{
			ID: "opencode", Label: "OpenCode",
			Desc: "OpenCode; registers an openai-compatible provider.", Protocol: "openai",
			Files: func(home string) map[string]string {
				return map[string]string{"config": filepath.Join(home, ".config", "opencode", "opencode.json")}
			},
			Probe: func(home string) bool {
				dir := filepath.Join(home, ".config", "opencode")
				if info, err := os.Stat(dir); err == nil && info.IsDir() {
					return true
				}
				_, err := exec.LookPath("opencode")
				return err == nil
			},
			Apply: applyOpenCode,
		},
		{
			ID: "dsh", Label: "DeepSeek Harness",
			Desc: "DSH; adds a wb-proxy provider to the profile patch layer (legacy settings.yaml kept).", Protocol: "openai",
			Files: dshFiles,
			Probe: func(home string) bool {
				dir := filepath.Join(home, ".dsh")
				if info, err := os.Stat(dir); err == nil && info.IsDir() {
					return true
				}
				_, err := exec.LookPath("dsh")
				return err == nil
			},
			Apply: applyDSH,
		},
		{
			ID: "crush", Label: "Crush",
			Desc: "Charm Crush; adds a wb-proxy provider entry.", Protocol: "openai",
			Files: func(home string) map[string]string {
				return map[string]string{"config": filepath.Join(home, ".config", "crush", "crush.json")}
			},
			Probe: func(home string) bool {
				dir := filepath.Join(home, ".config", "crush")
				if info, err := os.Stat(dir); err == nil && info.IsDir() {
					return true
				}
				_, err := exec.LookPath("crush")
				return err == nil
			},
			Apply: applyCrush,
		},
		{
			ID: "zcode", Label: "ZCode",
			Desc: "ZCode; adds a wb-proxy provider to its provider config. Quit ZCode first - it rewrites that file while running.", Protocol: "openai",
			Files: func(home string) map[string]string {
				return map[string]string{"config": filepath.Join(home, ".zcode", "v2", "provider_config.json")}
			},
			Probe: func(home string) bool {
				dir := filepath.Join(home, ".zcode")
				if info, err := os.Stat(dir); err == nil && info.IsDir() {
					return true
				}
				_, err := os.Stat(filepath.Join(home, ".zcode", "v2", "provider_config.json"))
				return err == nil
			},
			Apply: applyZCode,
		},
	}
}

// ClientByID returns one registry entry.
func ClientByID(id string) (Client, bool) {
	for _, client := range Clients() {
		if client.ID == id {
			return client, true
		}
	}
	return Client{}, false
}

func applyClaudeCode(paths map[string]string, ctx Context) (map[string]string, error) {
	path := paths["settings"]
	data, err := loadJSON(path, "claude settings")
	if err != nil {
		return nil, err
	}
	env, _ := data["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	base := regexp.MustCompile(`/v1/?$`).ReplaceAllString(ctx.BaseURL, "")
	env["ANTHROPIC_BASE_URL"] = base
	env["ANTHROPIC_AUTH_TOKEN"] = ctx.APIKey
	if ctx.Model != "" {
		for _, name := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
			env[name] = ctx.Model
		}
	}
	data["env"] = env
	raw, err := dumpJSON(data)
	if err != nil {
		return nil, err
	}
	return map[string]string{path: string(raw)}, nil
}

func applyCodex(paths map[string]string, ctx Context) (map[string]string, error) {
	configPath := paths["config"]
	text, err := readText(configPath)
	if err != nil {
		return nil, err
	}
	text, err = tomlUpsertTopKey(text, "model_provider", ProviderID)
	if err != nil {
		return nil, err
	}
	if ctx.Model != "" {
		text, err = tomlUpsertTopKey(text, "model", ctx.Model)
		if err != nil {
			return nil, err
		}
	}
	text, err = tomlUpsertTable(text, "model_providers."+ProviderID, map[string]any{
		"name": ProviderName, "base_url": ctx.BaseURLV1, "wire_api": "responses",
		"experimental_bearer_token": ctx.APIKey,
	})
	if err != nil {
		return nil, err
	}
	text = tomlDropTableKeys(text, "model_providers."+ProviderID, []string{"env_key", "env_key_instructions"})
	authPath := paths["auth"]
	auth, err := loadJSON(authPath, "codex auth")
	if err != nil {
		return nil, err
	}
	auth = deepMerge(auth, map[string]any{"OPENAI_API_KEY": ctx.APIKey})
	rawAuth, err := dumpJSON(auth)
	if err != nil {
		return nil, err
	}
	return map[string]string{configPath: text, authPath: string(rawAuth)}, nil
}

func applyOpenCode(paths map[string]string, ctx Context) (map[string]string, error) {
	path := paths["config"]
	data, err := loadJSON(path, "opencode config")
	if err != nil {
		return nil, err
	}
	if _, ok := data["$schema"]; !ok {
		data["$schema"] = "https://opencode.ai/config.json"
	}
	models := map[string]any{}
	for _, entry := range ctx.Models {
		if strings.TrimSpace(entry.ID) == "" {
			continue
		}
		limits := map[string]any{}
		if entry.ContextWindow > 0 {
			limits["context"] = entry.ContextWindow
		}
		if entry.MaxOutput > 0 {
			limits["output"] = entry.MaxOutput
		}
		if len(limits) > 0 {
			models[entry.ID] = map[string]any{"limit": limits}
		} else {
			models[entry.ID] = map[string]any{}
		}
	}
	provider := map[string]any{
		"npm": "@ai-sdk/openai-compatible", "name": ProviderName,
		"options": map[string]any{"baseURL": ctx.BaseURLV1, "apiKey": ctx.APIKey},
	}
	if len(models) > 0 {
		provider["models"] = models
	}
	mergedProviders, _ := data["provider"].(map[string]any)
	if mergedProviders == nil {
		mergedProviders = map[string]any{}
	}
	existing, _ := mergedProviders[ProviderID].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	mergedProviders[ProviderID] = deepMerge(existing, provider)
	if modelsPresent, ok := mergedProviders[ProviderID].(map[string]any); ok {
		if len(models) > 0 {
			modelsPresent["models"] = models
		} else {
			delete(modelsPresent, "models")
		}
	}
	data["provider"] = mergedProviders
	if ctx.Model != "" {
		data["model"] = ProviderID + "/" + ctx.Model
	}
	raw, err := dumpJSON(data)
	if err != nil {
		return nil, err
	}
	return map[string]string{path: string(raw)}, nil
}

func dshReasoningLevels(values []string) map[string]any {
	if len(values) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, value := range values {
		name := strings.TrimSpace(value)
		if name != "" {
			out[name] = name
		}
	}
	return out
}

func applyDSH(paths map[string]string, ctx Context) (map[string]string, error) {
	models := []map[string]any{}
	for _, entry := range ctx.Models {
		if strings.TrimSpace(entry.ID) == "" {
			continue
		}
		item := map[string]any{"id": entry.ID}
		if entry.ContextWindow > 0 {
			item["contextWindow"] = entry.ContextWindow
		}
		if entry.MaxOutput > 0 {
			item["maxTokens"] = entry.MaxOutput
		}
		if levels := dshReasoningLevels(entry.ReasoningEfforts); levels != nil {
			item["reasoningEfforts"] = levels
		}
		if entry.SupportsVision {
			item["input"] = []string{"text", "image"}
		}
		models = append(models, item)
	}
	provider := map[string]any{
		"displayName": ProviderName, "apiKeyEnv": CredentialRef,
		"api": "openai-completions", "baseURL": ctx.BaseURLV1,
		"compat":      map[string]any{"thinkingFormat": "openai", "supportsReasoningEffort": true},
		"retryPolicy": dshRetryPolicy,
	}
	if len(models) > 0 {
		provider["models"] = models
	}
	out := map[string]string{}
	patches := dshProfilePatches(ctx.Home)
	for _, path := range patches {
		text, err := readText(path)
		if err != nil {
			return nil, err
		}
		out[path] = dshPatchUpsert(text, provider, ctx.Model)
	}
	if settingsPath := paths["settings"]; settingsPath != "" {
		text, err := readText(settingsPath)
		if err != nil {
			return nil, err
		}
		text, err = yamlUpsertInline(text, "llm-pi-ai.providers."+ProviderID, provider)
		if err != nil {
			return nil, err
		}
		if ctx.Model != "" {
			text, err = yamlUpsertInline(text, "agent-default-model", dshDefaultModelMerge(text, ProviderID, ctx.Model))
			if err != nil {
				return nil, err
			}
		}
		out[settingsPath] = text
	}
	credentialsPath := paths["credentials"]
	credText, err := readText(credentialsPath)
	if err != nil {
		return nil, err
	}
	credText, err = yamlRefsUpsert(credText, CredentialRef, ctx.APIKey)
	if err != nil {
		return nil, err
	}
	out[credentialsPath] = credText
	return out, nil
}

func applyCrush(paths map[string]string, ctx Context) (map[string]string, error) {
	path := paths["config"]
	data, err := loadJSON(path, "crush config")
	if err != nil {
		return nil, err
	}
	models := []map[string]any{}
	for _, entry := range ctx.Models {
		if strings.TrimSpace(entry.ID) == "" {
			continue
		}
		item := map[string]any{"id": entry.ID, "name": entry.ID}
		if entry.ContextWindow > 0 {
			item["context_window"] = entry.ContextWindow
		}
		if entry.MaxOutput > 0 {
			item["default_max_tokens"] = entry.MaxOutput
		}
		models = append(models, item)
	}
	provider := map[string]any{
		"name": ProviderName, "type": "openai", "base_url": ctx.BaseURLV1,
		"api_key": ctx.APIKey, "models": models,
	}
	providers, _ := data["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	providers[ProviderID] = provider
	data["providers"] = providers
	raw, err := dumpJSON(data)
	if err != nil {
		return nil, err
	}
	return map[string]string{path: string(raw)}, nil
}

func applyZCode(paths map[string]string, ctx Context) (map[string]string, error) {
	path := paths["config"]
	data, err := loadJSON(path, "zcode provider config")
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		data = map[string]any{
			"schemaVersion": 1,
			"config": map[string]any{
				"providerOrder":       []any{},
				"providerConfigRules": map[string]any{"providerRules": []any{}},
			},
		}
	}
	config, _ := data["config"].(map[string]any)
	if config == nil {
		return nil, fmt.Errorf("zcode provider config has no config object: %s", path)
	}
	holder, _ := config["providerConfigRules"].(map[string]any)
	if holder == nil {
		return nil, fmt.Errorf("zcode provider config has no providerConfigRules: %s", path)
	}
	rules, _ := holder["providerRules"].([]any)
	if rules == nil {
		return nil, fmt.Errorf("zcode providerConfigRules has no providerRules list: %s", path)
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, entry := range ctx.Models {
		id := strings.TrimSpace(entry.ID)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	rule := map[string]any{
		"providerId": ProviderID, "providerName": ProviderName,
		"config": map[string]any{
			"group":  "standard-personal",
			"access": map[string]any{"type": "api-key", "apiKey": ctx.APIKey},
			"api":    map[string]any{"type": "openai-chat-completions", "baseUrl": ctx.BaseURLV1},
		},
	}
	if len(ids) > 0 {
		rule["config"].(map[string]any)["personalModelIds"] = ids
		rule["config"].(map[string]any)["modelOrder"] = ids
	}
	replaced := false
	for index, raw := range rules {
		existing, _ := raw.(map[string]any)
		if existing != nil && existing["providerId"] == ProviderID {
			rules[index] = deepMerge(existing, rule)
			replaced = true
			break
		}
	}
	if !replaced {
		rules = append(rules, rule)
	}
	holder["providerRules"] = rules
	order, _ := config["providerOrder"].([]any)
	if order == nil {
		order = []any{}
	}
	found := false
	for _, item := range order {
		if item == ProviderID {
			found = true
			break
		}
	}
	if !found {
		order = append(order, ProviderID)
	}
	config["providerOrder"] = order
	data["config"] = config
	raw, err := dumpJSON(data)
	if err != nil {
		return nil, err
	}
	return map[string]string{path: string(raw)}, nil
}
