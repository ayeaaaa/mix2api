package agents

import (
	"context"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type pickerProvider struct {
	realm    string
	picker   []providers.Model
	official []providers.Model
	active   string
	options  []string
}

func (p *pickerProvider) ID() string    { return "workbuddy" }
func (p *pickerProvider) Realm() string { return p.realm }
func (p *pickerProvider) Overview(context.Context, bool) (any, error) {
	return nil, nil
}
func (p *pickerProvider) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *pickerProvider) Models(context.Context, bool) ([]providers.Model, error) {
	return p.picker, nil
}
func (p *pickerProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}
func (p *pickerProvider) Routes() []providers.Route { return nil }
func (p *pickerProvider) Jobs() []scheduler.Job     { return nil }
func (p *pickerProvider) AgentPickerModels(context.Context, bool) ([]providers.Model, error) {
	return p.picker, nil
}
func (p *pickerProvider) AgentOfficialModels() []providers.Model { return p.official }
func (p *pickerProvider) ActiveRealm(context.Context) string     { return p.active }
func (p *pickerProvider) SetActiveRealm(_ context.Context, realm string) error {
	p.active = realm
	return nil
}
func (p *pickerProvider) RealmOptions() []string { return p.options }

func TestGatewayModelsUsesPickerOrderAndOfficialIds(t *testing.T) {
	intl := &pickerProvider{
		realm: "intl", active: "intl", options: []string{"intl", "cn"},
		picker: []providers.Model{
			{"id": "zeta", "name": "Zeta", "context_window": 1000, "max_output": 100, "supports_vision": true, "reasoning_efforts": []string{"low", "high"}},
			{"id": "alpha", "name": "Alpha", "context_window": 2000, "max_output": 200},
		},
	}
	cn := &pickerProvider{
		realm: "cn", picker: []providers.Model{{"id": "cn-only", "name": "CN Only"}},
	}
	raccoon := &pickerProvider{
		realm:  "raccoon",
		picker: []providers.Model{{"id": "raccoon-090dd3", "name": "Raccoon-Work"}},
	}
	codearts := &pickerProvider{
		realm: "codearts",
		official: []providers.Model{
			{"id": "alpha", "name": "Alpha"}, // already present, must not duplicate
			{"id": "deepseek-v4.1-flash-1017", "name": "DeepSeek-V4.1-Flash-1017"},
		},
	}
	models := GatewayModels(context.Background(), []providers.Provider{intl, cn, raccoon, codearts})
	if len(models) != 3 {
		t.Fatalf("models = %#v", models)
	}
	if models[0].ID != "zeta" || models[1].ID != "alpha" ||
		models[2].ID != "deepseek-v4.1-flash-1017" {
		t.Fatalf("picker order = %#v", models)
	}
	if models[0].Name != "Zeta" || models[0].ContextWindow != 1000 ||
		models[0].MaxOutput != 100 || !models[0].SupportsVision ||
		len(models[0].ReasoningEfforts) != 2 {
		t.Fatalf("metadata = %#v", models[0])
	}
	if models[2].Name != "DeepSeek-V4.1-Flash-1017" {
		t.Fatalf("official name = %#v", models[2])
	}
	for _, model := range models {
		if model.ID == "raccoon-090dd3" || model.ID == "cn-only" {
			t.Fatalf("picker leaked another provider catalogue: %#v", models)
		}
	}
}

func TestGatewayModelsFollowsActiveWorkBuddyRealm(t *testing.T) {
	intl := &pickerProvider{
		realm: "intl", active: "cn", options: []string{"intl", "cn"},
		picker: []providers.Model{{"id": "intl-model"}},
	}
	cn := &pickerProvider{
		realm: "cn", picker: []providers.Model{{"id": "cn-model"}},
	}
	models := GatewayModels(context.Background(), []providers.Provider{intl, cn})
	if len(models) != 1 || models[0].ID != "cn-model" {
		t.Fatalf("active realm picker = %#v", models)
	}
}
