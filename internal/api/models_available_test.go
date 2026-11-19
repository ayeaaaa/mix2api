package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

type paidStubProvider struct {
	*stubProvider
	usable   bool
	paid     bool
	proModel map[string]bool
}

func (p *paidStubProvider) ModelPoolAvailable(context.Context, string) (bool, bool) {
	return p.usable, p.paid
}

func (p *paidStubProvider) RequiresPro(modelID string) bool {
	return p.proModel[modelID]
}

func modelsOnlyServer(t *testing.T, enabled bool, usable, paid bool) *Server {
	t.Helper()
	store := config.New(t.TempDir() + "/settings.json")
	if enabled {
		if _, err := store.Merge(map[string]any{"models_only_available": true}); err != nil {
			t.Fatalf("store merge: %v", err)
		}
	}
	settingsSvc := settings.New(settings.Service{Store: store})
	provider := &paidStubProvider{
		stubProvider: &stubProvider{
			id: "workbuddy", realm: "intl",
			models: []providers.Model{{"id": "free-model"}, {"id": "pro-model"}},
		},
		usable:   usable,
		paid:     paid,
		proModel: map[string]bool{"pro-model": true},
	}
	return newTestServer(ServerOptions{
		Providers: []providers.Provider{provider},
		Settings:  settingsSvc,
	})
}

func modelsOnlyIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make([]string, 0, len(body.Data))
	for _, item := range body.Data {
		out = append(out, item.ID)
	}
	return out
}

func TestModelsOnlyAvailableOffKeepsFullCatalogue(t *testing.T) {
	srv := modelsOnlyServer(t, false, true, false)
	rec := serveRequest(srv.Handler(), http.MethodGet, "/v1/models?realm=intl", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	ids := modelsOnlyIDs(t, rec)
	if len(ids) != 2 || ids[0] != "free-model" || ids[1] != "pro-model" {
		t.Fatalf("ids = %v, want full catalogue", ids)
	}
}

func TestModelsOnlyAvailableHidesProWithoutPaidAccount(t *testing.T) {
	srv := modelsOnlyServer(t, true, true, false)
	rec := serveRequest(srv.Handler(), http.MethodGet, "/v1/models?realm=intl", "", nil)
	ids := modelsOnlyIDs(t, rec)
	if len(ids) != 1 || ids[0] != "free-model" {
		t.Fatalf("ids = %v, want only free-model", ids)
	}
}

func TestModelsOnlyAvailableHidesEverythingWithoutAnyAccount(t *testing.T) {
	srv := modelsOnlyServer(t, true, false, false)
	rec := serveRequest(srv.Handler(), http.MethodGet, "/v1/models?realm=intl", "", nil)
	ids := modelsOnlyIDs(t, rec)
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
}
