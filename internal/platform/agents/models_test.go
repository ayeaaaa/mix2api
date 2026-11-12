package agents

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type pickerProvider struct {
	realm    string
	picker   []providers.Model
	models   []providers.Model
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
	if p.models != nil {
		return p.models, nil
	}
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
			{"id": "alpha", "name": "Alpha"}, // same upstream id as intl/alpha, different exit
			{"id": "deepseek-v4.1-flash-1017", "name": "DeepSeek-V4.1-Flash-1017"},
		},
	}
	models := GatewayModels(context.Background(), []providers.Provider{intl, cn, raccoon, codearts})
	// MergeModels sorts and dedupes canonical ids; the picker's own order is not
	// the contract, only its selection rules and metadata.
	if !sort.SliceIsSorted(models, func(i, j int) bool { return models[i].ID < models[j].ID }) {
		t.Fatalf("models are not sorted by canonical id: %#v", models)
	}
	byID := map[string]ModelSpec{}
	for _, model := range models {
		if !strings.Contains(model.ID, "/") {
			t.Fatalf("bare model id leaked: %#v", models)
		}
		if _, duplicate := byID[model.ID]; duplicate {
			t.Fatalf("duplicate model id %q: %#v", model.ID, models)
		}
		byID[model.ID] = model
	}
	want := []string{
		"cn/cn-only",
		"codearts/alpha",
		"codearts/deepseek-v4.1-flash-1017",
		"intl/alpha",
		"intl/zeta",
		"raccoon/raccoon-090dd3",
	}
	if len(models) != len(want) {
		t.Fatalf("models = %#v, want %v", models, want)
	}
	for _, id := range want {
		if _, ok := byID[id]; !ok {
			t.Fatalf("missing %q in %#v", id, models)
		}
	}
	zeta := byID["intl/zeta"]
	if zeta.Name != "Zeta" || zeta.ContextWindow != 1000 || zeta.MaxOutput != 100 ||
		!zeta.SupportsVision || len(zeta.ReasoningEfforts) != 2 {
		t.Fatalf("metadata = %#v", zeta)
	}
	if official := byID["codearts/deepseek-v4.1-flash-1017"]; official.Name != "DeepSeek-V4.1-Flash-1017" {
		t.Fatalf("official name = %#v", official)
	}
	// The CodeArts official alpha must stay namespaced as codearts/alpha; it must
	// not duplicate or replace intl/alpha.
	if byID["codearts/alpha"].Name != "Alpha" || byID["intl/alpha"].Name != "Alpha" {
		t.Fatalf("alpha exits = %#v", models)
	}
}

func TestGatewayModelsPrefersPickerCatalogue(t *testing.T) {
	provider := &pickerProvider{
		realm:  "intl",
		picker: []providers.Model{{"id": "picker-model", "name": "Picker"}},
		models: []providers.Model{{"id": "plain-model", "name": "Plain"}},
	}
	models := GatewayModels(context.Background(), []providers.Provider{provider})
	if len(models) != 1 || models[0].ID != "intl/picker-model" || models[0].Name != "Picker" {
		t.Fatalf("picker preference = %#v", models)
	}
}

func TestGatewayModelsIncludesEveryWorkBuddyRealm(t *testing.T) {
	intl := &pickerProvider{
		realm: "intl", active: "cn", options: []string{"intl", "cn"},
		picker: []providers.Model{{"id": "intl-model"}},
	}
	cn := &pickerProvider{
		realm: "cn", picker: []providers.Model{{"id": "cn-model"}},
	}
	models := GatewayModels(context.Background(), []providers.Provider{intl, cn})
	if len(models) != 2 {
		t.Fatalf("every realm picker = %#v", models)
	}
	if models[0].ID != "cn/cn-model" || models[1].ID != "intl/intl-model" {
		t.Fatalf("every realm picker = %#v", models)
	}
}

func TestGatewayModelsCapsEveryExitList(t *testing.T) {
	picker := make([]providers.Model, 0, 120)
	for i := 0; i < 120; i++ {
		picker = append(picker, providers.Model{"id": fmt.Sprintf("model-%03d", i)})
	}
	models := GatewayModels(context.Background(), []providers.Provider{&pickerProvider{realm: "intl", picker: picker}})
	if len(models) != MaxModels {
		t.Fatalf("model cap = %d", len(models))
	}
	for _, model := range models {
		if !strings.HasPrefix(model.ID, "intl/") {
			t.Fatalf("bare model id leaked: %#v", model)
		}
	}
}
