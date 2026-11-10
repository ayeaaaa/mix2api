package codearts

import "github.com/ayeaaaa/mix2api-v2/internal/providers"

// Capabilities reports CodeArts' optional platform surfaces. Text chat with
// image input is supported; media generation, agents, probes and WorkBuddy
// credit limits are not.
func (p *Provider) Capabilities() providers.Capabilities {
	return providers.Capabilities{ImageInputInChat: true}
}
