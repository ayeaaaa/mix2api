package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type modelsResponse struct {
	Object   string            `json:"object"`
	Data     []providers.Model `json:"data"`
	Realm    string            `json:"realm"`
	Catalog  map[string]any    `json:"catalog,omitempty"`
	Catalogs map[string]any    `json:"catalogs,omitempty"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeFailure(w, localFailure(http.StatusMethodNotAllowed, "method not allowed"))
		return
	}

	scope, _ := realmFromRequest(r)
	boundRealm, failure := s.apiKeyBoundRealm(r)
	if failure != nil {
		writeFailure(w, failure)
		return
	}
	realm := scope
	if boundRealm == "gateway" {
		// An all-provider key defaults to the gateway catalog when the caller
		// did not ask for a concrete scope. Explicit concrete/all/gateway
		// scopes are all valid for this key.
		if realm == "" {
			realm = "gateway"
		}
	} else if boundRealm != "" {
		// A concrete key binding is policy: it outranks X-Realm/query/body,
		// including realm=all. A gateway request keeps its scope and is
		// narrowed to the bound realm below, so a bound key never discovers
		// another exit.
		realm = boundRealm
		if scope == "gateway" {
			realm = "gateway"
		}
	}
	if realm == "" {
		if len(s.providers) == 0 {
			writeFailure(w, localFailure(http.StatusNotFound, "no provider is registered"))
			return
		}
		if len(s.providers) != 1 {
			writeFailure(w, localFailure(http.StatusBadRequest,
				"realm is required when multiple providers are registered; use realm=gateway for the all-exit catalog"))
			return
		}
		realm = providerRealm(s.providers[0])
	}

	force := queryBool(r, "force")

	if realm == "gateway" {
		candidates := s.providers
		if boundRealm != "" && boundRealm != "gateway" {
			provider := s.providerForRealm(boundRealm)
			if provider == nil {
				writeFailure(w, localFailure(http.StatusBadRequest, "invalid api key realm: "+boundRealm))
				return
			}
			candidates = []providers.Provider{provider}
		}
		if len(candidates) == 0 {
			writeFailure(w, localFailure(http.StatusNotFound, "no provider is registered"))
			return
		}
		data, catalogs, err := s.gatewayModels(r.Context(), candidates, force)
		if err != nil {
			writeFailure(w, err)
			return
		}
		writeGatewayModelsResponse(w, data, catalogs)
		return
	}

	if realm == "all" {
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
		var catalog map[string]any
		for _, candidate := range candidates {
			if provenance, ok := candidate.(providers.CatalogProvenanceProvider); ok {
				if value := provenance.CatalogProvenance(); len(value) > 0 {
					catalog = value
					break
				}
			}
		}
		writeModelsResponse(w, "all", data, catalog)
		return
	}

	provider := s.providerForRealm(realm)
	if provider == nil {
		writeFailure(w, localFailure(http.StatusNotFound, "unknown realm: "+realm))
		return
	}
	models, err := provider.Models(r.Context(), force)
	if err != nil {
		writeFailure(w, err)
		return
	}
	data := make([]providers.Model, 0, len(models))
	for _, model := range models {
		// Concrete realms keep the bare upstream id for older clients and add
		// the qualified identity beside it. Entries without a usable string id
		// are skipped so the response never invents "<realm>/".
		entry := decorateModel(model, provider, false)
		if entry == nil {
			continue
		}
		data = append(data, entry)
	}
	writeModelsResponse(w, providerRealm(provider), data, catalogProvenance(provider))
}

func catalogProvenance(provider providers.Provider) map[string]any {
	if provider == nil {
		return nil
	}
	provenance, ok := provider.(providers.CatalogProvenanceProvider)
	if !ok {
		return nil
	}
	value := provenance.CatalogProvenance()
	if len(value) == 0 {
		return nil
	}
	return value
}

func writeModelsResponse(w http.ResponseWriter, realm string, data []providers.Model, catalog map[string]any) {
	if data == nil {
		data = []providers.Model{}
	}
	writeJSONResponse(w, http.StatusOK, modelsResponse{
		Object:  "list",
		Data:    data,
		Realm:   realm,
		Catalog: catalog,
	})
}

// writeGatewayModelsResponse writes the cross-exit catalog. Provenance is
// grouped by realm because the gateway scope has no single provider catalog.
func writeGatewayModelsResponse(w http.ResponseWriter, data []providers.Model, catalogs map[string]any) {
	if data == nil {
		data = []providers.Model{}
	}
	writeJSONResponse(w, http.StatusOK, modelsResponse{
		Object:   "list",
		Data:     data,
		Realm:    "gateway",
		Catalogs: catalogs,
	})
}

// decorateModel copies one provider-owned model entry and adds its exit
// identity. The provider map is cloned rather than mutated: catalogs may be
// shared with provider caches. Concrete realms keep the bare id, while gateway
// entries replace it with the canonical "<realm>/<upstream-id>".
func decorateModel(model providers.Model, provider providers.Provider, canonicalID bool) providers.Model {
	if provider == nil {
		return nil
	}
	upstream, ok := model["id"].(string)
	if !ok || strings.TrimSpace(upstream) == "" {
		return nil
	}
	out := make(providers.Model, len(model)+4)
	for key, value := range model {
		out[key] = value
	}
	realm := providerRealm(provider)
	qualified := canonicalModelID(realm, upstream)
	out["upstream_id"] = upstream
	out["qualified_id"] = qualified
	out["provider"] = provider.ID()
	out["realm"] = realm
	if canonicalID {
		out["id"] = qualified
	}
	return out
}

// gatewayModels unions every candidate catalog under canonical IDs, preserving
// provider order. Entries are deduplicated by lowercased qualified ID. Each
// catalog read is bounded by catalogResolutionTimeout; a timeout counts as that
// provider's failure. A failure from one exit does not hide the others; the
// first error is returned only when no candidate produced a catalog.
func (s *Server) gatewayModels(ctx context.Context, candidates []providers.Provider, force bool) ([]providers.Model, map[string]any, error) {
	out := make([]providers.Model, 0)
	seen := make(map[string]bool)
	catalogs := make(map[string]any)
	successes := 0
	var firstErr error

	for _, provider := range candidates {
		lookupCtx, cancel := context.WithTimeout(ctx, catalogResolutionTimeout)
		models, err := provider.Models(lookupCtx, force)
		cancel()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		successes++
		realm := providerRealm(provider)
		if catalog := catalogProvenance(provider); len(catalog) > 0 {
			catalogs[realm] = catalog
		}
		for _, model := range models {
			entry := decorateModel(model, provider, true)
			if entry == nil {
				continue
			}
			qualified, _ := entry["qualified_id"].(string)
			key := strings.ToLower(strings.TrimSpace(qualified))
			if key != "" {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			out = append(out, entry)
		}
	}
	if successes == 0 && firstErr != nil {
		return nil, nil, firstErr
	}
	if len(catalogs) == 0 {
		catalogs = nil
	}
	return out, catalogs, nil
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
