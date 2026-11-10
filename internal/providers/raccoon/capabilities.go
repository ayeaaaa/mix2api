package raccoon

import "github.com/ayeaaaa/mix2api-v2/internal/providers"

// Capabilities reports Raccoon's optional platform surfaces. Every optional
// capability is false; Raccoon remains a text-chat provider.
func (p *Provider) Capabilities() providers.Capabilities {
	return providers.Capabilities{}
}
