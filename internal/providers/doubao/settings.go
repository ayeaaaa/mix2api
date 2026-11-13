package doubao

import (
	"context"
	"fmt"
)

// SettingsView exposes only non-secret configuration and account counts.
func (p *Provider) SettingsView(ctx context.Context) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("doubao: provider is nil")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	cfg := p.currentConfig()
	view := configValues(cfg)
	view["configured"] = cfg.Enabled
	stats := p.pool.Stats()
	view["accounts"] = stats.Total
	view["healthy"] = stats.Healthy
	return map[string]any{"doubao": view}, nil
}

// SaveSettings merges the doubao section and reloads the live config.
func (p *Provider) SaveSettings(ctx context.Context, patch map[string]any) error {
	if p == nil {
		return fmt.Errorf("doubao: provider is nil")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	raw, present := patch["doubao"]
	if !present {
		return nil
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("doubao must be an object")
	}
	if _, err := SaveConfig(p.store, section); err != nil {
		return err
	}
	_, err := p.applySettings()
	return err
}
