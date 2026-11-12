package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/web"
)

// TestDashboardContract pins the Gate E surface: the embedded asset matches
// the documented v1.6.5 provenance plus the total-quota, daily-active and branding deltas, the
// static aliases are public and non-CORS, the API route inventory is complete,
// and the management gate fails closed.
func TestDashboardContract(t *testing.T) {
	body := web.DashboardHTML()
	if got := web.BodyHash(body); got != web.DashboardSHA256 {
		t.Fatalf("embedded dashboard SHA-256 = %s, want %s", got, web.DashboardSHA256)
	}
	html := string(body)
	for _, marker := range []string{
		"<!doctype html>",
		`<html lang="zh-CN" data-ui-language="__WB_UI_LANGUAGE__">`,
		"mix2api 控制台",
		"const PANEL_STORE = 'wb-proxy-panel-token'",
		"async function getJSON(url, opts)",
		"async function postJSON(url, body)",
		"tabIntlTotal",
		"tabCnTotal",
		"tabIntlExpiringLine",
		"tabCnExpiringLine",
		"analyticsScopeSeg",
		"intlSize",
		"cnSize",
		"raccoonDailyActive",
		"raccoonDailyCell",
		"/raccoon/accounts/daily-active",
		"siteFavicon",
		"setSiteTitle",
		"setSiteIconFile",
		"applyBranding",
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("embedded dashboard is missing marker %q", marker)
		}
	}
	if len(dashboardRoutes()) == 0 {
		t.Fatal("dashboard route inventory is empty")
	}

	// The runner and the selected suites are test-only artifacts. Production
	// builds never import or execute Node.
	root := repositoryRoot(t)
	for _, path := range []string{
		filepath.Join(root, "tests", "dashboard", "node", "run.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_dashboard_source.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_dom_stub.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_dashboard_sandbox.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_test_page_nav.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_test_panel_gate_first_paint.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_test_accounts_realm_switch.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_test_expiring_credits.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_test_analytics_realm_scope.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_test_settings_load.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_test_daily_chat_pending.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_test_buddy_checkin_status.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_test_settings_update_ui.js"),
		filepath.Join(root, "tests", "dashboard", "node", "_test_update_rollback_ui.js"),
		filepath.Join(root, "tests", "oracle", "dashboard.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("contract artifact missing: %v", err)
		}
	}
}

func TestDashboardStaticContract(t *testing.T) {
	fixture := newDashboardFixture(t)
	for _, path := range []string{"/", "/dashboard.html", "/dashboard", "/ui", "/classic", "/classic/"} {
		reply := fixture.get(path, "")
		if reply.Status != http.StatusOK {
			t.Fatalf("%s status = %d", path, reply.Status)
		}
		if got := web.BodyHash(reply.Body); got != web.DashboardSHA256 {
			t.Fatalf("%s body hash = %s", path, got)
		}
		if reply.Header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("%s content-type = %q", path, reply.Header.Get("Content-Type"))
		}
		if reply.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s cache-control = %q", path, reply.Header.Get("Cache-Control"))
		}
		if reply.Header.Get("ETag") == "" {
			t.Fatalf("%s missing ETag", path)
		}
		if reply.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s is CORS-readable: %q", path, reply.Header.Get("Access-Control-Allow-Origin"))
		}
		if reply.Header.Get("Server") != "mix2api/"+dashboardVersion {
			t.Fatalf("%s Server = %q", path, reply.Header.Get("Server"))
		}
	}

	conditional := fixture.do(http.MethodGet, "/dashboard.html", "", "", map[string]string{
		"If-None-Match": web.DashboardETag(),
	})
	if conditional.Status != http.StatusNotModified || len(conditional.Body) != 0 {
		t.Fatalf("conditional GET = %d body=%d", conditional.Status, len(conditional.Body))
	}

	method := fixture.do(http.MethodPost, "/dashboard.html", "{}", "")
	if method.Status != http.StatusMethodNotAllowed {
		t.Fatalf("POST dashboard = %d", method.Status)
	}
	if method.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST dashboard Allow = %q", method.Header.Get("Allow"))
	}

	favicon := fixture.get("/favicon.ico", "")
	if favicon.Status != http.StatusNotFound {
		t.Fatalf("favicon = %d", favicon.Status)
	}
}

func TestDashboardAuthContract(t *testing.T) {
	fixture := newDashboardFixture(t)

	// Management routes fail closed with a structured v1 envelope.
	for _, target := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/settings"},
		{http.MethodGet, "/accounts?realm=all"},
		{http.MethodGet, "/usage"},
		{http.MethodGet, "/raccoon/overview"},
		{http.MethodPost, "/pricing/refresh"},
		{http.MethodPost, "/shutdown"},
		{http.MethodGet, "/v1/models"},
	} {
		reply := fixture.do(target.method, target.path, "", "")
		assertAuthError(t, reply, target.method+" "+target.path)
	}

	// The first-paint status and health checks stay reachable; neither contains
	// a credential or session token.
	status := fixture.get("/panel/status", "")
	if status.Status != http.StatusOK {
		t.Fatalf("status = %d", status.Status)
	}
	if strings.Contains(status.RawText, "MIX2API_API_KEY") || strings.Contains(status.RawText, "Bearer ") {
		t.Fatalf("status contains credential material: %s", status.RawText)
	}
	health := fixture.get("/health", "")
	if health.Status != http.StatusOK || health.JSON["ok"] != true {
		t.Fatalf("health = %d %#v", health.Status, health.JSON)
	}

	token := fixture.login("admin")
	settings := fixture.get("/settings", token)
	if settings.Status != http.StatusOK {
		t.Fatalf("authenticated settings = %d", settings.Status)
	}
	if strings.Contains(settings.RawText, token) {
		t.Fatalf("settings leaked the session token")
	}
}
