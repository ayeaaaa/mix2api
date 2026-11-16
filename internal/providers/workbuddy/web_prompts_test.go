package workbuddy

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestBuiltinDailyChatWebPromptsAreCompleteAndIsolated(t *testing.T) {
	prompts := builtinDailyChatWebPrompts()
	if len(prompts) != 300 {
		t.Fatalf("builtin prompt count = %d, want 300", len(prompts))
	}
	seen := make(map[string]bool, len(prompts))
	for _, prompt := range prompts {
		if strings.TrimSpace(prompt) != prompt || prompt == "" {
			t.Fatalf("builtin prompt is not a clean non-empty line: %q", prompt)
		}
		if seen[prompt] {
			t.Fatalf("duplicate builtin prompt: %q", prompt)
		}
		seen[prompt] = true
	}
	prompts[0] = "mutated"
	if builtinDailyChatWebPrompts()[0] == "mutated" {
		t.Fatal("builtin prompt slice was not copied")
	}
}

func TestDailyChatWebPromptDefaultsToHi(t *testing.T) {
	dir := t.TempDir()
	provider, _ := newPoolProvider(t, realmIntl, dir)
	writePoolSettings(t, dir, map[string]any{})

	if got := provider.dailyChatWebPrompt(); got != dailyChatPrompt {
		t.Fatalf("default prompt = %q, want %q", got, dailyChatPrompt)
	}
}

func TestDailyChatWebPromptUsesConfiguredPool(t *testing.T) {
	dir := t.TempDir()
	provider, _ := newPoolProvider(t, realmIntl, dir)
	writePoolSettings(t, dir, map[string]any{
		"daily_chat_web_random_prompt": true,
		"daily_chat_web_prompts":       []any{"自定义一号"},
	})

	if got := provider.dailyChatWebPrompt(); got != "自定义一号" {
		t.Fatalf("configured prompt = %q, want custom prompt", got)
	}
}

func TestDailyChatWebPromptEmptyPoolUsesBuiltin(t *testing.T) {
	dir := t.TempDir()
	provider, _ := newPoolProvider(t, realmIntl, dir)
	writePoolSettings(t, dir, map[string]any{
		"daily_chat_web_random_prompt": true,
		"daily_chat_web_prompts":       []any{},
	})

	builtin := make(map[string]bool)
	for _, prompt := range builtinDailyChatWebPrompts() {
		builtin[prompt] = true
	}
	for i := 0; i < 20; i++ {
		if got := provider.dailyChatWebPrompt(); !builtin[got] {
			t.Fatalf("prompt %q is not in builtin pool", got)
		}
	}
}

func TestSettingsViewIncludesWebPromptSettings(t *testing.T) {
	dir := t.TempDir()
	provider, _ := newPoolProvider(t, realmIntl, dir)
	writePoolSettings(t, dir, map[string]any{
		"daily_chat_web_random_prompt": true,
		"daily_chat_web_prompts":       []any{"自定义一号"},
	})

	view, err := provider.SettingsView(context.Background())
	if err != nil {
		t.Fatalf("SettingsView: %v", err)
	}
	if view["daily_chat_web_random_prompt"] != true {
		t.Fatalf("random prompt view = %#v", view["daily_chat_web_random_prompt"])
	}
	builtin, _ := view["daily_chat_web_builtin_prompts"].([]string)
	if len(builtin) != 300 {
		t.Fatalf("builtin prompts view len = %d, want 300", len(builtin))
	}
	stored, _ := view["daily_chat_web_prompts"].([]string)
	if len(stored) != 1 || stored[0] != "自定义一号" {
		t.Fatalf("stored prompts view = %#v", view["daily_chat_web_prompts"])
	}
	if view["daily_chat_web_prompts_max"] != dailyChatWebPromptsMax {
		t.Fatalf("prompts max view = %#v", view["daily_chat_web_prompts_max"])
	}
}

func TestSaveSettingsPersistsWebPromptSettings(t *testing.T) {
	dir := t.TempDir()
	provider, _ := newPoolProvider(t, realmIntl, dir)
	writePoolSettings(t, dir, map[string]any{})

	err := provider.SaveSettings(context.Background(), map[string]any{
		"daily_chat_web_random_prompt": true,
		"daily_chat_web_prompts":       "a\n b \r\n\n" + strings.Repeat("x", dailyChatWebPromptMaxLen+1),
	})
	if err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if got := provider.dailyChatWebPrompt(); got != "a" && got != "b" {
		t.Fatalf("saved prompt = %q, want a or b", got)
	}
	view, err := provider.SettingsView(context.Background())
	if err != nil {
		t.Fatalf("SettingsView: %v", err)
	}
	if view["daily_chat_web_random_prompt"] != true {
		t.Fatalf("random prompt not persisted: %#v", view["daily_chat_web_random_prompt"])
	}
	stored, _ := view["daily_chat_web_prompts"].([]string)
	if len(stored) != 2 || stored[0] != "a" || stored[1] != "b" {
		t.Fatalf("cleaned prompts = %#v", view["daily_chat_web_prompts"])
	}
}

func TestTasksDailyChatWebUsesConfiguredRandomPrompt(t *testing.T) {
	fake, provider, _, _ := setupDailyWebFake(t, realmIntl, nil)
	if _, err := provider.store.Merge(map[string]any{
		"daily_chat_web_random_prompt": true,
		"daily_chat_web_prompts":       []any{"自定义一号"},
	}); err != nil {
		t.Fatalf("merge settings: %v", err)
	}
	if _, err := provider.DailyChatWeb(context.Background(), "web-daily"); err != nil {
		t.Fatalf("DailyChatWeb: %v", err)
	}
	for _, call := range fake.snapshot() {
		if call.Method == http.MethodPost && call.Path == "/console/as/conversations/" {
			if got := call.Body["prompt"]; got != "自定义一号" {
				t.Fatalf("conversation prompt = %#v, want 自定义一号", got)
			}
			return
		}
	}
	t.Fatal("no create-conversation request observed")
}

func TestSaveSettingsRejectsInvalidWebPromptToggle(t *testing.T) {
	dir := t.TempDir()
	provider, _ := newPoolProvider(t, realmIntl, dir)
	writePoolSettings(t, dir, map[string]any{})

	if err := provider.SaveSettings(context.Background(), map[string]any{
		"daily_chat_web_random_prompt": "true",
	}); err == nil {
		t.Fatal("SaveSettings accepted a non-boolean random prompt toggle")
	}
}
