package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type modelsResponse struct {
	Object string            `json:"object"`
	Data   []providers.Model `json:"data"`
	Realm  string            `json:"realm"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeFailure(w, localFailure(http.StatusMethodNotAllowed, "method not allowed"))
		return
	}

	realm := realmFromRequest(r)
	if realm == "" {
		if len(s.providers) == 0 {
			writeFailure(w, localFailure(http.StatusNotFound, "no provider is registered"))
			return
		}
		if len(s.providers) != 1 {
			writeFailure(w, localFailure(http.StatusBadRequest,
				"realm is required when multiple providers are registered"))
			return
		}
		realm = providerRealm(s.providers[0])
	}

	force := queryBool(r, "force")

	if strings.EqualFold(realm, "all") {
		candidates := s.workBuddyProviders()
		if len(candidates) == 0 {
			writeFailure(w, localFailure(http.StatusBadRequest,
				"realm=all is only available for WorkBuddy providers (intl/cn); none is registered"))
			return
		}
		data, err := unionModels(r.Context(), candidates, force)
		if err != nil {
			writeFailure(w, err)
			return
		}
		writeModelsResponse(w, "all", data)
		return
	}

	provider := s.providerForRealm(realm)
	if provider == nil {
		writeFailure(w, localFailure(http.StatusNotFound, "unknown realm: "+realm))
		return
	}
	data, err := provider.Models(r.Context(), force)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeModelsResponse(w, providerRealm(provider), data)
}

func writeModelsResponse(w http.ResponseWriter, realm string, data []providers.Model) {
	if data == nil {
		data = []providers.Model{}
	}
	writeJSONResponse(w, http.StatusOK, modelsResponse{
		Object: "list",
		Data:   data,
		Realm:  realm,
	})
}

// unionModels merges intl/cn catalogs by bare, case-insensitive model ID. A
// failure from one WorkBuddy realm does not hide the other realm's models; the
// error is only surfaced when no candidate produced a catalog.
func unionModels(ctx context.Context, candidates []providers.Provider, force bool) ([]providers.Model, error) {
	out := make([]providers.Model, 0)
	seen := make(map[string]bool)
	successes := 0
	var firstErr error

	for _, provider := range candidates {
		models, err := provider.Models(ctx, force)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		successes++
		for _, model := range models {
			id, _ := model["id"].(string)
			key := strings.ToLower(strings.TrimSpace(id))
			if key != "" {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			out = append(out, model)
		}
	}
	if successes == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
