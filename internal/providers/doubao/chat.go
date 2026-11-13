package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type conversationEntry struct {
	conv  conversation
	model string
	at    time.Time
}

func (p *Provider) conversationFor(sessionKey, model string) *conversation {
	p.convMu.Lock()
	defer p.convMu.Unlock()
	key := conversationKey(sessionKey, model)
	entry, ok := p.conversations[key]
	if !ok {
		return nil
	}
	if time.Since(entry.at) > time.Hour {
		delete(p.conversations, key)
		return nil
	}
	conv := entry.conv
	return &conv
}

func (p *Provider) remember(sessionKey, model string, conv *conversation) {
	if p == nil || conv == nil || strings.TrimSpace(conv.ConversationID) == "" {
		return
	}
	p.convMu.Lock()
	defer p.convMu.Unlock()
	if p.conversations == nil {
		p.conversations = map[string]conversationEntry{}
	}
	p.conversations[conversationKey(sessionKey, model)] = conversationEntry{
		conv: *conv, model: model, at: time.Now(),
	}
	if len(p.conversations) > 2048 {
		for key, entry := range p.conversations {
			if time.Since(entry.at) > time.Hour {
				delete(p.conversations, key)
			}
		}
	}
}

func conversationKey(sessionKey, model string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		sessionKey = "default"
	}
	return sessionKey + "\x00" + model
}

// OpenChat picks an account, builds the upstream Doubao body, and returns a
// normalized OpenAI SSE stream. The provider sees only the bare upstream model.
func (p *Provider) OpenChat(ctx context.Context, req providers.ChatRequest) (providers.ChatStream, error) {
	if p == nil {
		return nil, errors.New("doubao: provider is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.applySettings()
	cfg := p.currentConfig()
	if !cfg.Enabled {
		return nil, &APIError{Kind: KindBadRequest, Message: "Doubao 出口未启用：请在设置中打开 doubao.enabled。"}
	}
	model, ok := FindModel(req.Model)
	if !ok {
		return nil, &APIError{Kind: KindBadRequest, Message: "unknown doubao model: " + strings.TrimSpace(req.Model)}
	}
	account, err := p.pool.Pick(model.ID, req.SessionKey, nil)
	if err != nil {
		return nil, &APIError{Kind: KindTransient, Message: "没有可用的 Doubao 账号：请导入并启用至少一个 cookie 账号。"}
	}
	if !p.pool.Acquire(account) {
		return nil, &APIError{Kind: KindTransient, Message: "Doubao 账号正忙，请稍后重试。"}
	}
	conv := p.conversationFor(req.SessionKey, model.ID)
	if conv == nil {
		conv = &conversation{}
	}
	prompt := promptFor(req.Messages, conv)
	if strings.TrimSpace(prompt) == "" {
		p.pool.Release(account)
		return nil, &APIError{Kind: KindBadRequest, Message: "messages is empty"}
	}
	body, err := p.openUpstream(ctx, account, model, prompt, conv)
	if err != nil {
		p.pool.Release(account)
		account.noteError(errorMessage(err), 30*time.Second)
		return nil, err
	}
	p.pool.BindSession(req.SessionKey, account.UID)
	return newChatStream(ctx, body, p, account, model, conv, req.SessionKey), nil
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		return apiErr.Message
	}
	return "upstream request failed"
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func promptFor(raw []json.RawMessage, conv *conversation) string {
	text := lastUserText(raw)
	if conv != nil && strings.TrimSpace(conv.ConversationID) != "" {
		return text
	}
	return historyPrompt(raw)
}

func parseMessages(raw []json.RawMessage) []chatMessage {
	out := make([]chatMessage, 0, len(raw))
	for _, item := range raw {
		var message chatMessage
		if json.Unmarshal(item, &message) == nil {
			out = append(out, message)
		}
	}
	return out
}

func messageText(message chatMessage) string {
	var text string
	if json.Unmarshal(message.Content, &text) == nil {
		return strings.TrimSpace(text)
	}
	var parts []map[string]any
	if json.Unmarshal(message.Content, &parts) == nil {
		var b strings.Builder
		for _, part := range parts {
			if value, ok := part["text"].(string); ok {
				b.WriteString(value)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

func lastUserText(raw []json.RawMessage) string {
	messages := parseMessages(raw)
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(messages[i].Role, "user") {
			return messageText(messages[i])
		}
	}
	return ""
}

func historyPrompt(raw []json.RawMessage) string {
	parts := make([]string, 0, len(raw))
	for _, message := range parseMessages(raw) {
		text := messageText(message)
		if text == "" {
			continue
		}
		switch strings.ToLower(message.Role) {
		case "system":
			parts = append(parts, "[system]\n"+text)
		case "assistant":
			parts = append(parts, "[assistant]\n"+text)
		case "user":
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}
