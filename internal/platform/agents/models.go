// Package agents owns the v1-compatible local AI client configurator.
package agents

import (
	"context"
	"sort"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// ModelSpec is the gateway model metadata written into client configs.
type ModelSpec struct {
	ID               string   `json:"id"`
	Name             string   `json:"name,omitempty"`
	ContextWindow    int64    `json:"context_window,omitempty"`
	MaxOutput        int64    `json:"max_output,omitempty"`
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
	SupportsVision   bool     `json:"supports_vision,omitempty"`
}

// MaxModels is the v1 apply cap.
const MaxModels = 80

// MergeModels merges provider specs and gateway catalog entries, preserving
// explicit payload fields. Catalog wins only when no richer spec exists.
func MergeModels(sources ...[]ModelSpec) []ModelSpec {
	out := []ModelSpec{}
	index := map[string]int{}
	for _, source := range sources {
		for _, spec := range source {
			id := strings.TrimSpace(spec.ID)
			if id == "" {
				continue
			}
			spec.ID = id
			if position, ok := index[id]; ok {
				out[position] = mergeSpec(out[position], spec)
				continue
			}
			index[id] = len(out)
			out = append(out, spec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > MaxModels {
		out = out[:MaxModels]
	}
	return out
}

func mergeSpec(base, incoming ModelSpec) ModelSpec {
	if base.Name == "" {
		base.Name = incoming.Name
	}
	if base.ContextWindow == 0 {
		base.ContextWindow = incoming.ContextWindow
	}
	if base.MaxOutput == 0 {
		base.MaxOutput = incoming.MaxOutput
	}
	if len(base.ReasoningEfforts) == 0 {
		base.ReasoningEfforts = incoming.ReasoningEfforts
	}
	if !base.SupportsVision {
		base.SupportsVision = incoming.SupportsVision
	}
	return base
}

// ModelSpecsFromCatalog projects provider Catalog entries.
func ModelSpecsFromCatalog(models []providers.Model) []ModelSpec {
	out := make([]ModelSpec, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(stringValue(model["id"]))
		if id == "" {
			continue
		}
		name := strings.TrimSpace(stringValue(model["name"]))
		if name == "" {
			name = id
		}
		out = append(out, ModelSpec{
			ID: id, Name: name,
			ContextWindow:    int64Value(model["context_window"]),
			MaxOutput:        int64Value(model["max_output"]),
			ReasoningEfforts: stringList(model["reasoning_efforts"]),
			SupportsVision:   boolValue(model["supports_vision"]),
		})
	}
	return out
}

// GatewayModels collects agent specs from providers without importing any
// concrete implementation. The v1 picker uses the current WorkBuddy catalogue
// only (never Raccoon), then appends configured CodeArts official ids. When no
// provider exposes the picker hook, the neutral provider scan below remains the
// fallback for embedders and tests.
func GatewayModels(ctx context.Context, list []providers.Provider) []ModelSpec {
	if specs, ok := pickerModels(ctx, list); ok {
		return specs
	}
	rich := []ModelSpec{}
	catalogs := []ModelSpec{}
	for _, provider := range list {
		if provider == nil {
			continue
		}
		if hook, ok := provider.(providers.AgentModelSpecProvider); ok {
			specs, err := hook.AgentModelSpecs(ctx)
			if err == nil {
				for _, spec := range specs {
					rich = append(rich, ModelSpec{
						ID: spec.ID, Name: spec.Name, ContextWindow: spec.ContextWindow,
						MaxOutput: spec.MaxOutputTokens, ReasoningEfforts: spec.ReasoningEfforts,
						SupportsVision: spec.SupportsVision,
					})
				}
			}
		}
		if models, err := provider.Models(ctx, false); err == nil {
			catalogs = append(catalogs, ModelSpecsFromCatalog(models)...)
		}
	}
	return MergeModels(rich, catalogs)
}

// pickerModels implements v1's _agents_models order: one WorkBuddy realm's
// bundled/live catalogue, followed by the official CodeArts ids that are not
// already present. It returns false when the composition has no WorkBuddy
// picker, so callers keep the neutral fallback.
func pickerModels(ctx context.Context, list []providers.Provider) ([]ModelSpec, bool) {
	active := ""
	for _, provider := range list {
		if selector, ok := provider.(providers.RealmSelector); ok {
			active = strings.ToLower(strings.TrimSpace(selector.ActiveRealm(ctx)))
			break
		}
	}
	var selected providers.AgentPickerModelProvider
	selectedRealm := ""
	for _, provider := range list {
		picker, ok := provider.(providers.AgentPickerModelProvider)
		if !ok {
			continue
		}
		realm := strings.ToLower(strings.TrimSpace(provider.Realm()))
		if realm != "intl" && realm != "cn" {
			continue
		}
		if realm == active {
			selected = picker
			selectedRealm = realm
			break
		}
		if selected == nil || (selectedRealm != "intl" && realm == "intl") {
			selected = picker
			selectedRealm = realm
		}
	}
	if selected == nil {
		return nil, false
	}

	out := []ModelSpec{}
	seen := map[string]bool{}
	if models, err := selected.AgentPickerModels(ctx, false); err == nil {
		for _, spec := range ModelSpecsFromCatalog(models) {
			key := strings.ToLower(strings.TrimSpace(spec.ID))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, spec)
		}
	}
	for _, provider := range list {
		official, ok := provider.(providers.AgentOfficialModelProvider)
		if !ok {
			continue
		}
		for _, model := range official.AgentOfficialModels() {
			id := strings.ToLower(strings.TrimSpace(stringValue(model["id"])))
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			name := strings.TrimSpace(stringValue(model["name"]))
			if name == "" {
				name = id
			}
			out = append(out, ModelSpec{ID: id, Name: name})
		}
	}
	if len(out) > MaxModels {
		out = out[:MaxModels]
	}
	return out, true
}

// Enrich fills id-only payload entries from specs. Explicit fields win.
func Enrich(models []ModelSpec, specs []ModelSpec) []ModelSpec {
	byID := map[string]ModelSpec{}
	for _, spec := range specs {
		byID[spec.ID] = spec
	}
	out := make([]ModelSpec, 0, len(models))
	for _, model := range models {
		if spec, ok := byID[model.ID]; ok {
			merged := spec
			if model.Name != "" {
				merged.Name = model.Name
			}
			if model.ContextWindow > 0 {
				merged.ContextWindow = model.ContextWindow
			}
			if model.MaxOutput > 0 {
				merged.MaxOutput = model.MaxOutput
			}
			if len(model.ReasoningEfforts) > 0 {
				merged.ReasoningEfforts = model.ReasoningEfforts
			}
			if model.SupportsVision {
				merged.SupportsVision = true
			}
			model = merged
		}
		if model.Name == "" {
			model.Name = model.ID
		}
		out = append(out, model)
	}
	if len(out) > MaxModels {
		out = out[:MaxModels]
	}
	return out
}

// embeddingClients are the clients whose configs embed the model list.
var embeddingClients = map[string]bool{
	"opencode": true, "dsh": true, "crush": true, "zcode": true,
}

// ModelEmbeddingClients returns the sorted v1 list for the panel.
func ModelEmbeddingClients() []string {
	out := make([]string, 0, len(embeddingClients))
	for id := range embeddingClients {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func stringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return out
	default:
		return nil
	}
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func boolValue(value any) bool {
	flag, _ := value.(bool)
	return flag
}
