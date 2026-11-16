package workbuddy

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

const (
	// dailyChatWebPromptsMax is the v1 limit: 300 built-in plus 200 custom.
	dailyChatWebPromptsMax = 500
	// dailyChatWebPromptMaxLen is the UTF-8 byte limit for one prompt.
	dailyChatWebPromptMaxLen = 200
)

func builtinDailyChatWebPrompts() []string {
	return append([]string(nil), builtinDailyChatWebPromptLines...)
}

// dailyChatWebPrompt returns the prompt for one web-activity attempt. The
// random-prompt switch is off by default, preserving the original single
// "Hi" request; when enabled, an empty stored list means the full built-in
// pool and a non-empty list is the operator's exact selection.
func (p *Provider) dailyChatWebPrompt() string {
	if p == nil {
		return dailyChatPrompt
	}
	settings := p.sharedSettings()
	if enabled, _ := settings["daily_chat_web_random_prompt"].(bool); !enabled {
		return dailyChatPrompt
	}
	pool := normalizeDailyChatWebPrompts(settings["daily_chat_web_prompts"])
	if len(pool) == 0 {
		pool = builtinDailyChatWebPrompts()
	}
	if len(pool) == 0 {
		return dailyChatPrompt
	}
	return pool[rand.IntN(len(pool))]
}

func dailyChatWebRandomPrompt(settings map[string]any) bool {
	enabled, _ := settings["daily_chat_web_random_prompt"].(bool)
	return enabled
}

func (p *Provider) sharedSettings() map[string]any {
	if p == nil || p.store == nil {
		return nil
	}
	settings, err := p.store.Load()
	if err != nil {
		return nil
	}
	return settings
}

// normalizeDailyChatWebPrompts accepts the v1 list/newline-string shapes,
// trims and de-duplicates lines, drops over-long entries, and caps the pool.
func normalizeDailyChatWebPrompts(value any) []string {
	var raw []any
	switch typed := value.(type) {
	case nil:
		return []string{}
	case string:
		text := strings.ReplaceAll(typed, "\r\n", "\n")
		text = strings.ReplaceAll(text, "\r", "\n")
		for _, line := range strings.Split(text, "\n") {
			raw = append(raw, line)
		}
	case []string:
		for _, item := range typed {
			raw = append(raw, item)
		}
	case []any:
		raw = typed
	default:
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		if item == nil {
			continue
		}
		prompt := strings.TrimSpace(fmt.Sprint(item))
		if prompt == "" || len(prompt) > dailyChatWebPromptMaxLen || seen[prompt] {
			continue
		}
		seen[prompt] = true
		out = append(out, prompt)
		if len(out) >= dailyChatWebPromptsMax {
			break
		}
	}
	return out
}

func validDailyChatWebPromptsValue(value any) bool {
	switch value.(type) {
	case nil, string, []string, []any:
		return true
	default:
		return false
	}
}
