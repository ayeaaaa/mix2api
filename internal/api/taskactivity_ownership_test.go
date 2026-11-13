package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestTaskActivityOwnership(t *testing.T) {
	type routeSpec struct {
		method string
		path   string
	}
	specs := []routeSpec{
		{http.MethodGet, "/tasks"},
		{http.MethodPost, "/tasks/run"},
		{http.MethodPost, "/tasks/claim"},
		{http.MethodPost, "/tasks/travel"},
		{http.MethodPost, "/tasks/streak-bonus"},
		{http.MethodPost, "/tasks/buddy"},
		{http.MethodGet, "/activity/history"},
		{http.MethodGet, "/activity/campaigns"},
		{http.MethodPost, "/activity/campaigns/run"},
		{http.MethodPost, "/activity/campaigns/claim"},
	}
	called := map[string]bool{}
	routes := []providers.Route{}
	for _, spec := range specs {
		current := spec
		routes = append(routes, providers.Route{
			Method: current.method, Path: current.path, Auth: providers.AuthPanel,
			Handler: func(w http.ResponseWriter, r *http.Request) {
				called[current.path] = true
				writeJSONResponse(w, http.StatusOK, map[string]any{"owner": "workbuddy", "path": current.path})
			},
		})
	}
	provider := &stubProvider{id: "workbuddy", realm: "cn", routes: routes}
	server := New(ServerOptions{Providers: []providers.Provider{provider}, PanelAuth: passThroughAuth})
	for _, spec := range specs {
		req := httptest.NewRequest(spec.method, spec.path, nil)
		handler, pattern := server.mux.Handler(req)
		if handler == nil || !strings.HasSuffix(pattern, spec.path) {
			t.Fatalf("%s pattern = %q, want %q", spec.path, pattern, spec.path)
		}
		rec := doRequest(server, spec.method, spec.path, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", spec.path, rec.Code, rec.Body)
		}
		if !called[spec.path] {
			t.Fatalf("%s did not reach the provider handler", spec.path)
		}
	}
}
