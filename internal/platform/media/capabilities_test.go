package media

import (
	"context"
	"errors"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type noMediaProvider struct{}

func (p *noMediaProvider) ID() string                                  { return "plain" }
func (p *noMediaProvider) Realm() string                               { return "cn" }
func (p *noMediaProvider) Overview(context.Context, bool) (any, error) { return nil, nil }
func (p *noMediaProvider) Health(context.Context, bool) (providers.Health, error) {
	return providers.Health{"ok": true}, nil
}
func (p *noMediaProvider) Models(context.Context, bool) ([]providers.Model, error) { return nil, nil }
func (p *noMediaProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, errors.New("not used")
}
func (p *noMediaProvider) Routes() []providers.Route { return nil }
func (p *noMediaProvider) Jobs() []scheduler.Job     { return nil }

func TestMediaCapabilities(t *testing.T) {
	service := NewService(ServiceOptions{
		Providers: []providers.Provider{&noMediaProvider{}},
		ConfigFunc: func() Config {
			cfg := DefaultConfig()
			cfg.Enabled = true
			return cfg
		},
	})
	if _, err := service.Models(context.Background(), "cn", ""); err == nil {
		t.Fatal("provider without MediaProvider was called")
	}
	if _, err := service.MediaProviderFor("cn"); err == nil {
		t.Fatal("provider without a media hook resolved")
	}
}
