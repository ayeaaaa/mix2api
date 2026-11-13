package raccoon

import (
	"context"
	"fmt"
)

// SettingsView returns the masked Raccoon settings section. It contains only
// configuration values and account counts, never auth material.
func (p *Provider) SettingsView(ctx context.Context) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("raccoon: provider is nil")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	cfg := LoadConfig(p.store)
	view := configValues(cfg)
	view["configured"] = cfg.Enabled
	if p.pool != nil {
		stats := p.pool.Stats()
		view["accounts"] = stats.Total
		view["healthy"] = stats.Healthy
	} else {
		view["accounts"] = 0
		view["healthy"] = 0
	}
	return map[string]any{"raccoon": view}, nil
}

// SaveSettings merges only the raccoon section, preserving unknown nested keys.
func (p *Provider) SaveSettings(ctx context.Context, patch map[string]any) error {
	if p == nil {
		return fmt.Errorf("raccoon: provider is nil")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	raw, present := patch["raccoon"]
	if !present {
		return nil
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("raccoon must be an object")
	}
	_, err := SaveConfig(p.store, section)
	return err
}
