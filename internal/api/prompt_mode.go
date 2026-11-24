package api

import (
	"context"
	"encoding/json"
	"strings"
)

const defaultGatewayPrompt = `你是一名工程助手，幫助用戶完成軟件工程任務。

- 先理解代碼與上下文再動手，遵循既有模式與約定。
- 最小改動：只改必要部分，不做無關重構。
- 改動後要驗證：跑測試或構建確認結果，不假設「應該沒問題」。
- 對不確定的事保持誠實：說明不確定並給出驗證路徑，不編造。
- 跟隨用戶語言；簡潔直接，技術術語精確。
- 如實報告失敗與邊界，不掩蓋、不粉飾。
`

// gatewayPromptConfig reads the panel's prompt settings fail-open. Any read or
// shape error leaves passthrough (current behavior), so a broken settings file
// never rewrites client messages.
func (s *Server) gatewayPromptConfig(ctx context.Context) (mode, text string) {
	mode, text = "passthrough", ""
	if s == nil || s.settings == nil || s.settings.Store == nil {
		return
	}
	data, err := s.settings.Store.Load()
	if err != nil {
		return
	}
	raw, _ := data["prompt"].(map[string]any)
	if raw == nil {
		return
	}
	if value, ok := raw["mode"].(string); ok {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "custom" || normalized == "append" {
			mode = normalized
		}
	}
	if value, ok := raw["text"].(string); ok {
		text = value
	}
	return
}

func gatewayPromptText(text string) string {
	if strings.TrimSpace(text) != "" {
		return text
	}
	return defaultGatewayPrompt
}

// applyGatewayPromptMode rewrites chat messages for the configured prompt
// mode. user/assistant/tool messages are never touched.
func applyGatewayPromptMode(messages []any, mode, text string) []any {
	if len(messages) == 0 {
		return messages
	}
	switch mode {
	case "custom":
		out := make([]any, 0, len(messages)+1)
		for _, item := range messages {
			if !isGatewaySystemMessage(item) {
				out = append(out, item)
			}
		}
		return append([]any{map[string]any{"role": "system", "content": gatewayPromptText(text)}}, out...)
	case "append":
		insert := 0
		for _, item := range messages {
			if !isGatewaySystemMessage(item) {
				break
			}
			insert++
		}
		out := make([]any, 0, len(messages)+1)
		out = append(out, messages[:insert]...)
		out = append(out, map[string]any{"role": "system", "content": gatewayPromptText(text)})
		return append(out, messages[insert:]...)
	default:
		return messages
	}
}

func isGatewaySystemMessage(item any) bool {
	object, ok := item.(map[string]any)
	if !ok {
		return false
	}
	role, _ := object["role"].(string)
	role = strings.ToLower(strings.TrimSpace(role))
	return role == "system" || role == "developer"
}

// applyGatewayPromptModeRaw is the Chat Completions bridge: messages arrive as
// []json.RawMessage and are reflected back into the raw payload so providers
// see the rewritten head.
func applyGatewayPromptModeRaw(messages []json.RawMessage, mode, text string) ([]json.RawMessage, error) {
	items := make([]any, 0, len(messages))
	for _, raw := range messages {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			return messages, err
		}
		items = append(items, item)
	}
	rewritten := applyGatewayPromptMode(items, mode, text)
	out := make([]json.RawMessage, 0, len(rewritten))
	for _, item := range rewritten {
		raw, err := json.Marshal(item)
		if err != nil {
			return messages, err
		}
		out = append(out, raw)
	}
	return out, nil
}
