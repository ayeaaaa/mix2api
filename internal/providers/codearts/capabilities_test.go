package codearts

import "testing"

func TestCapabilities(t *testing.T) {
	provider := &Provider{}
	caps := provider.Capabilities()
	if caps.MediaGeneration || caps.WindowProbe || caps.QuotaScan {
		t.Fatalf("codearts capabilities = %#v", caps)
	}
	if !caps.ImageInputInChat || !caps.UsageLimits {
		t.Fatalf("codearts capabilities lost image/limit support: %#v", caps)
	}
	if _, ok := any(provider).(interface{ MediaModels() }); ok {
		t.Fatal("codearts must not expose a media hook")
	}
}
