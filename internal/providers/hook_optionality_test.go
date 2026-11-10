package providers_test

import (
	"context"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/codearts"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/raccoon"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/workbuddy"
)

func TestProviderHookOptionality(t *testing.T) {
	var dummy providers.Provider = dummyProvider{}
	if _, ok := dummy.(providers.LimitApplier); ok {
		t.Fatal("dummy provider unexpectedly implements LimitApplier")
	}
	if _, ok := dummy.(providers.SettingsContributor); ok {
		t.Fatal("dummy provider unexpectedly implements SettingsContributor")
	}
	if _, ok := dummy.(providers.MediaProvider); ok {
		t.Fatal("dummy provider unexpectedly implements MediaProvider")
	}
	if _, ok := dummy.(providers.ProbeProvider); ok {
		t.Fatal("dummy provider unexpectedly implements ProbeProvider")
	}
	if _, ok := dummy.(providers.UsageSnapshotReporter); ok {
		t.Fatal("dummy provider unexpectedly implements UsageSnapshotReporter")
	}

	var workbuddyProvider providers.Provider = &workbuddy.Provider{}
	if _, ok := workbuddyProvider.(providers.LimitApplier); !ok {
		t.Fatal("WorkBuddy does not implement LimitApplier")
	}
	if _, ok := workbuddyProvider.(providers.SettingsContributor); !ok {
		t.Fatal("WorkBuddy does not implement SettingsContributor")
	}
	if _, ok := workbuddyProvider.(providers.RealmSelector); !ok {
		t.Fatal("WorkBuddy does not implement RealmSelector")
	}
	if _, ok := workbuddyProvider.(providers.MediaProvider); !ok {
		t.Fatal("WorkBuddy does not implement MediaProvider")
	}

	var codeartsProvider interface{} = &codearts.Provider{}
	if capabilities := (&codearts.Provider{}).Capabilities(); capabilities.UsageLimits || capabilities.MediaGeneration || capabilities.WindowProbe || capabilities.QuotaScan {
		t.Fatalf("CodeArts capabilities = %+v", capabilities)
	}
	if _, ok := codeartsProvider.(providers.LimitApplier); ok {
		t.Fatal("CodeArts implements LimitApplier")
	}
	var raccoonProvider interface{} = &raccoon.Provider{}
	if capabilities := (&raccoon.Provider{}).Capabilities(); capabilities.UsageLimits || capabilities.MediaGeneration || capabilities.WindowProbe || capabilities.QuotaScan {
		t.Fatalf("Raccoon capabilities = %+v", capabilities)
	}
	if _, ok := raccoonProvider.(providers.LimitApplier); ok {
		t.Fatal("Raccoon implements LimitApplier")
	}
}

type dummyProvider struct{}

func (dummyProvider) ID() string                                  { return "dummy" }
func (dummyProvider) Realm() string                               { return "dummy" }
func (dummyProvider) Overview(context.Context, bool) (any, error) { return nil, nil }
func (dummyProvider) Health(context.Context, bool) (providers.Health, error) {
	return nil, nil
}
func (dummyProvider) Models(context.Context, bool) ([]providers.Model, error) { return nil, nil }
func (dummyProvider) OpenChat(context.Context, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, nil
}
func (dummyProvider) Routes() []providers.Route { return nil }
func (dummyProvider) Jobs() []scheduler.Job     { return nil }
