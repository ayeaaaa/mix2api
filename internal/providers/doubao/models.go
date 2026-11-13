package doubao

import (
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// Model is one Doubao Work model. The fields mirror the upstream request
// builder in the reference Node client; the API layer never sees these fields.
type Model struct {
	ID               string
	Name             string
	Key              string
	ItemID           int
	AgentMode        int
	ModeID           string
	ConversationMode int
	ExtraParams      map[string]any
	DefaultReasoning int
	Kind             string
}

var models = []Model{
	{ID: "doubao-fast", Name: "快速", Key: "0", ItemID: 0, AgentMode: 2, ModeID: "1", ConversationMode: 1, DefaultReasoning: 3, Kind: "chat"},
	{ID: "doubao-expert", Name: "专家", Key: "3", ItemID: 3, AgentMode: 2, ModeID: "1", ConversationMode: 1, DefaultReasoning: 4, Kind: "chat"},
	{ID: "work-auto", Name: "工作任务 Auto", Key: "9", ItemID: 9, AgentMode: 1, ModeID: "3", ConversationMode: 2, ExtraParams: map[string]any{"total_window_size": "256000"}, DefaultReasoning: 3, Kind: "work"},
	{ID: "doubao-2.1-lite", Name: "豆包 2.1 Lite", Key: "seed-lite-7b", ItemID: 10001, AgentMode: 1, ModeID: "3", ConversationMode: 2, ExtraParams: map[string]any{"memory_profile": "256k", "total_window_size": "256000"}, DefaultReasoning: 5, Kind: "work"},
	{ID: "doubao-2.1-pro", Name: "豆包 2.1 Pro", Key: "5", ItemID: 5, AgentMode: 1, ModeID: "3", ConversationMode: 2, ExtraParams: map[string]any{"total_window_size": "256000"}, DefaultReasoning: 5, Kind: "work"},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Key: "person-deepseek-v4-1-flash-261017", ItemID: 10001, AgentMode: 1, ModeID: "3", ConversationMode: 2, ExtraParams: map[string]any{"memory_profile": "300k_230", "total_window_size": "300000"}, DefaultReasoning: 5, Kind: "work"},
	{ID: "kimi-k3.1-pro", Name: "Kimi K3.1 Pro", Key: "person-kimi-k3-1-pro-261111", ItemID: 10001, AgentMode: 1, ModeID: "3", ConversationMode: 2, ExtraParams: map[string]any{"memory_profile": "300k_230", "total_window_size": "300000"}, DefaultReasoning: 5, Kind: "work"},
	{ID: "kimi-k3.1-pro-highspeed", Name: "Kimi K3.1 Pro 高速", Key: "person-kimi-k3-1-pro-highspeed-261111", ItemID: 10001, AgentMode: 1, ModeID: "3", ConversationMode: 2, ExtraParams: map[string]any{"memory_profile": "300k_230", "total_window_size": "300000"}, DefaultReasoning: 5, Kind: "work"},
	{ID: "kimi-k3", Name: "Kimi K3", Key: "person-kimi-k3-261111", ItemID: 10001, AgentMode: 1, ModeID: "3", ConversationMode: 2, ExtraParams: map[string]any{"memory_profile": "300k_230", "total_window_size": "300000"}, DefaultReasoning: 5, Kind: "work"},
	{ID: "glm-5.3", Name: "GLM 5.3", Key: "person-glm-5-3-261111", ItemID: 10001, AgentMode: 1, ModeID: "3", ConversationMode: 2, ExtraParams: map[string]any{"memory_profile": "300k_230", "total_window_size": "300000"}, DefaultReasoning: 5, Kind: "work"},
	{ID: "glm-5.3-flash", Name: "GLM 5.3 Flash", Key: "person-glm-5-3-flash-261111", ItemID: 10001, AgentMode: 1, ModeID: "3", ConversationMode: 2, ExtraParams: map[string]any{"memory_profile": "300k_230", "total_window_size": "300000"}, DefaultReasoning: 5, Kind: "work"},
	{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", Key: "person-deepseek-v4-pro-ga-260813", ItemID: 10001, AgentMode: 1, ModeID: "3", ConversationMode: 2, ExtraParams: map[string]any{"memory_profile": "300k_230", "total_window_size": "300000"}, DefaultReasoning: 5, Kind: "work"},
}

var modelByAlias = buildModelAliases()

func buildModelAliases() map[string]*Model {
	out := make(map[string]*Model, len(models)*3)
	for i := range models {
		m := &models[i]
		for _, alias := range []string{m.ID, m.Name, m.Key} {
			if alias != "" {
				out[strings.ToLower(alias)] = m
			}
		}
	}
	for alias, id := range map[string]string{
		"doubao": "doubao-fast", "fast": "doubao-fast", "expert": "doubao-expert",
		"deepseek": "deepseek-v4.1-flash", "deepseek-v4.1": "deepseek-v4.1-flash",
		"kimi": "kimi-k3.1-pro", "glm": "glm-5.3",
	} {
		if m := findExact(id); m != nil {
			out[alias] = m
		}
	}
	return out
}

func findExact(id string) *Model {
	for i := range models {
		if strings.EqualFold(models[i].ID, id) {
			return &models[i]
		}
	}
	return nil
}

// FindModel resolves an id, display name, upstream key, or common alias.
func FindModel(name string) (*Model, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return &models[0], true
	}
	if m := modelByAlias[key]; m != nil {
		return m, true
	}
	return nil, false
}

// ListModels returns the OpenAI-compatible catalog. IDs are bare upstream IDs;
// the API gateway adds the doubao/ prefix.
func ListModels() []providers.Model {
	out := make([]providers.Model, 0, len(models))
	for _, m := range models {
		out = append(out, providers.Model{
			"id":       m.ID,
			"object":   "model",
			"owned_by": "doubao-work",
			"name":     m.Name,
			"kind":     m.Kind,
		})
	}
	return out
}
