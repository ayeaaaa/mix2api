package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestTaskActivityOwnership(t *testing.T) {
	paths := []string{
		"/tasks", "/tasks/run", "/tasks/claim", "/tasks/travel",
		"/tasks/streak-bonus", "/tasks/buddy", "/activity/history",
	}
	called := map[string]bool{}
	routes := []providers.Route{}
	for _, path := range paths {
		current := path
		method := http.MethodGet
		if current != "/tasks" && current != "/activity/history" {
			method = http.MethodPost
		}
		routes = append(routes, providers.Route{
			Method: method, Path: current, Auth: providers.AuthPanel,
			Handler: func(w http.ResponseWriter, r *http.Request) {
				called[current] = true
				writeJSONResponse(w, http.StatusOK, map[string]any{"owner": "workbuddy", "path": current})
			},
		})
	}
	provider := &stubProvider{id: "workbuddy", realm: "cn", routes: routes}
	server := New(ServerOptions{Providers: []providers.Provider{provider}, PanelAuth: passThroughAuth})
	for _, path := range paths {
		method := http.MethodGet
		if path != "/tasks" && path != "/activity/history" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, path, nil)
		handler, pattern := server.mux.Handler(req)
		if handler == nil || !strings.HasSuffix(pattern, path) {
			t.Fatalf("%s pattern = %q, want %q", path, pattern, path)
		}
		rec := doRequest(server, method, path, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
		if !called[path] {
			t.Fatalf("%s did not reach the provider handler", path)
		}
	}
}
