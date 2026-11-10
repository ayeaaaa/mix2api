package workbuddy

import (
	"context"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// Capabilities reports WorkBuddy's optional platform surfaces.
func (p *Provider) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		MediaGeneration:  true,
		MediaImages:      true,
		MediaVideos:      true,
		ImageInputInChat: true,
		AgentModelSpecs:  true,
		WindowProbe:      true,
		QuotaScan:        true,
		UsageLimits:      true,
	}
}

// AgentModelSpecs projects the provider catalog into the agent configurator's
// neutral model metadata. A catalog failure returns an empty list so the panel
// can still render the client cards.
func (p *Provider) AgentModelSpecs(ctx context.Context) ([]providers.AgentModelSpec, error) {
	models, err := p.Models(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]providers.AgentModelSpec, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		id := strings.TrimSpace(metaText(model["id"]))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		spec := providers.AgentModelSpec{
			ID:               id,
			Name:             strings.TrimSpace(metaText(model["name"])),
			ContextWindow:    int64(metaNumber(model["context_window"])),
			MaxOutputTokens:  int64(metaNumber(model["max_output"])),
			SupportsVision:   metaBool(model["supports_vision"]),
			ReasoningEfforts: metaStringList(model["reasoning_efforts"]),
		}
		if spec.Name == "" {
			spec.Name = id
		}
		out = append(out, spec)
	}
	return out, nil
}

func metaNumber(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case float32:
		return int(typed)
	default:
		return 0
	}
}
