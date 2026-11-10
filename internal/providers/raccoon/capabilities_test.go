package raccoon

import "testing"

func TestCapabilities(t *testing.T) {
	provider := &Provider{}
	caps := provider.Capabilities()
	if caps.MediaGeneration || caps.MediaImages || caps.MediaVideos || caps.ImageInputInChat ||
		caps.AgentModelSpecs || caps.WindowProbe || caps.QuotaScan || caps.UsageLimits {
		t.Fatalf("raccoon capabilities = %#v", caps)
	}
}
