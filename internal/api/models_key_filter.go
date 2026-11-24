package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// filterModelsForKeyListing applies the panel's filter_models_by_key setting
// to /v1/models. Key enforcement on chat/messages/responses is unconditional
// and does not consult this setting.
func filterModelsForKeyListing(ctx context.Context, s *Server, r *http.Request, data []providers.Model) []providers.Model {
	if s == nil || s.settings == nil || r == nil || len(data) == 0 {
		return data
	}
	entry := KeyEntryFromContext(r)
	if entry == nil {
		return data
	}
	view, err := s.settings.View(ctx)
	if err != nil {
		return data
	}
	enabled, _ := view["filter_models_by_key"].(bool)
	if !enabled {
		return data
	}
	out := make([]providers.Model, 0, len(data))
	for _, model := range data {
		id := strings.TrimSpace(modelString(model, "id"))
		upstream := strings.TrimSpace(modelString(model, "upstream_id"))
		if id == "" {
			id = upstream
		}
		qualified := strings.TrimSpace(modelString(model, "qualified_id"))
		if keys.AllowsModelAny(*entry, id, upstream, qualified) {
			out = append(out, model)
		}
	}
	return out
}

func modelString(model providers.Model, key string) string {
	if model == nil {
		return ""
	}
	value, _ := model[key].(string)
	return value
}
