package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
)

func newUpdateFixture(t *testing.T) (*Server, *update.Checker, *update.RequestManager, string) {
	t.Helper()
	settings := config.New(filepath.Join(t.TempDir(), "settings.json"))
	if _, err := settings.Merge(map[string]any{update.CheckEnabledKey: true}); err != nil {
		t.Fatal(err)
	}
	fetcher := update.FetcherFunc(func(ctx context.Context, url string) ([]byte, error) {
		return []byte(`[{"tag_name":"v1.2.3","html_url":"https://github.com/x/y/releases/tag/v1.2.3","published_at":"2026-01-01"}]`), nil
	})
	now := time.Unix(1700000000, 0)
	checker := update.NewChecker("1.0.0", settings, fetcher, func() time.Time { return now }, nil)
	dir := t.TempDir()
	unit := filepath.Join(t.TempDir(), "unit")
	link := filepath.Join(t.TempDir(), "link")
	_ = os.WriteFile(unit, []byte("u"), 0o600)
	_ = os.WriteFile(link, []byte("l"), 0o600)
	t.Setenv("WB_UPDATE_INSTALL_ENABLED", "1")
	manager := update.NewRequestManager(update.RequestManagerOptions{
		Dir: dir, Executor: &update.Executor{Dir: dir, UnitPath: unit, EnabledLink: link},
		Now: func() time.Time { return now }, Port: 8788, Root: "/srv/app",
	})
	server := New(ServerOptions{Updates: checker, UpdateRequests: manager, PanelAuth: passThroughAuth, Version: "1.0.0"})
	return server, checker, manager, dir
}

func TestUpdateRoutes(t *testing.T) {
	server, _, _, dir := newUpdateFixture(t)
	rec := doRequest(server, http.MethodGet, "/updates", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["current_version"] != "1.0.0" || body["install_enabled"] != true {
		t.Fatalf("status body = %#v", body)
	}
	// Install before a successful check is 409.
	rec = doRequest(server, http.MethodPost, "/updates/install", "{}", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("early install = %d %s", rec.Code, rec.Body)
	}
	rec = doRequest(server, http.MethodPost, "/updates/check", "{}", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("check = %d %s", rec.Code, rec.Body)
	}
	body = decodeBody(t, rec)
	if body["update_available"] != true || body["latest_tag"] != "v1.2.3" || body["ok"] != true {
		t.Fatalf("check body = %#v", body)
	}
	rec = doRequest(server, http.MethodPost, "/updates/install", "{}", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("install = %d %s", rec.Code, rec.Body)
	}
	body = decodeBody(t, rec)
	if body["ok"] != true || body["state"] != "queued" || body["version"] != "1.2.3" {
		t.Fatalf("install body = %#v", body)
	}
	if _, err := os.Stat(filepath.Join(dir, update.RequestFile)); err != nil {
		t.Fatalf("request file missing: %v", err)
	}
	// A second install while the request exists is 409.
	rec = doRequest(server, http.MethodPost, "/updates/install", "{}", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate install = %d %s", rec.Code, rec.Body)
	}

	// Rollback needs a complete backup; seed one after clearing the request.
	_ = os.Remove(filepath.Join(dir, update.RequestFile))
	backups := map[string]any{"backups": []any{map[string]any{"version": "0.9.0", "complete": true, "files": 4}}}
	raw, _ := json.Marshal(backups)
	_ = os.WriteFile(filepath.Join(dir, update.BackupsFile), raw, 0o600)
	rec = doRequest(server, http.MethodGet, "/updates/backups", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("backups = %d %s", rec.Code, rec.Body)
	}
	body = decodeBody(t, rec)
	if body["source"] != "systemd" || body["keep_backups"] == nil || body["keep_backups_max"] == nil {
		t.Fatalf("backups body = %#v", body)
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "/srv") {
		t.Fatalf("backups leaked paths: %s", rec.Body)
	}
	rec = doRequest(server, http.MethodPost, "/updates/rollback", `{}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "请选择") {
		t.Fatalf("empty rollback = %d %s", rec.Code, rec.Body)
	}
	rec = doRequest(server, http.MethodPost, "/updates/rollback", `{"version":"9.9.9"}`, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unknown rollback = %d %s", rec.Code, rec.Body)
	}
	rec = doRequest(server, http.MethodPost, "/updates/rollback", `{"version":"0.9.0"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("rollback = %d %s", rec.Code, rec.Body)
	}
	if body := decodeBody(t, rec); body["version"] != "0.9.0" {
		t.Fatalf("rollback body = %#v", body)
	}
}
