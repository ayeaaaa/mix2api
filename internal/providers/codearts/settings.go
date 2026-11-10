package codearts

import (
	"context"
	"fmt"
)

// SettingsView returns the masked CodeArts settings section. It contains only
// configuration values and account counts, never auth material.
func (p *Provider) SettingsView(ctx context.Context) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("codearts: provider is nil")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	cfg := LoadConfig(p.store)
	view := configValues(cfg)
	view["configured"] = cfg.Enabled
	pool := p.Pool()
	if pool != nil {
		stats := pool.Stats()
		view["accounts"] = stats.Total
		view["healthy"] = stats.Healthy
	} else {
		view["accounts"] = 0
		view["healthy"] = 0
	}
	return map[string]any{"codearts": view}, nil
}

// SaveSettings merges only the codearts section. Unknown nested keys survive
// because SaveConfig merges into the stored section.
func (p *Provider) SaveSettings(ctx context.Context, patch map[string]any) error {
	if p == nil {
		return fmt.Errorf("codearts: provider is nil")
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	raw, present := patch["codearts"]
	if !present {
		return nil
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("codearts must be an object")
	}
	_, err := SaveConfig(p.store, section)
	return err
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
