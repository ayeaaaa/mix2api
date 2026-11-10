package dashboard

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/web"
)

// dashboardRoute is one path the shipped inline JavaScript requests. Source is
// the literal used by dashboard.html (queries stripped); dynamic client actions
// share a source prefix such as /raccoon/accounts/.
type dashboardRoute struct {
	Method string
	Path   string
	Source string
	Public bool
}

func (r dashboardRoute) probePath() string {
	return strings.NewReplacer("{id}", "dash-task", "{action}", "enable").Replace(r.Path)
}

func (r dashboardRoute) source() string {
	if strings.TrimSpace(r.Source) != "" {
		return r.Source
	}
	path, _, _ := strings.Cut(r.Path, "?")
	return path
}

func dashboardRoutes() []dashboardRoute {
	get := func(path string, public bool) dashboardRoute {
		return dashboardRoute{Method: http.MethodGet, Path: path, Public: public}
	}
	post := func(path string) dashboardRoute {
		return dashboardRoute{Method: http.MethodPost, Path: path}
	}
	withSource := func(route dashboardRoute, prefix string) dashboardRoute {
		route.Source = prefix
		return route
	}
	action := func(path, prefix string) dashboardRoute {
		return withSource(post(path), prefix)
	}

	routes := []dashboardRoute{
		get("/health", true),
		get("/panel/status", true),
		{Method: http.MethodPost, Path: "/panel/login", Public: true},
		post("/panel/logout"),
		post("/panel/password"),

		get("/accounts", false),
		get("/accounts/export", false),
		post("/accounts/add-api-key"),
		post("/accounts/checkin"),
		post("/accounts/credits"),
		post("/accounts/credits/detail"),
		get("/accounts/credits/grants", false),
		post("/accounts/daily-chat"),
		post("/accounts/daily-chat-web"),
		post("/accounts/delete"),
		post("/accounts/import"),
		post("/accounts/import/desktop"),
		post("/accounts/login/cancel"),
		get("/accounts/login/poll", false),
		post("/accounts/login/start"),
		post("/accounts/product"),
		post("/accounts/refresh"),
		post("/accounts/set"),
		post("/accounts/set-all"),
		post("/accounts/sync-profile"),
		post("/accounts/test"),

		get("/activity/history", false),

		get("/agents", false),
		get("/agents/available", false),
		get("/agents/model-selection", false),
		post("/agents/model-selection"),
		post("/agents/apply"),
		post("/agents/restore"),

		post("/catalog/refresh"),
		get("/realm", false),
		post("/realm"),

		get("/codearts/overview", false),
		action("/codearts/accounts/checkin", "/codearts/accounts/"),
		action("/codearts/accounts/clear-cooldown", "/codearts/accounts/"),
		action("/codearts/accounts/delete", "/codearts/accounts/"),
		action("/codearts/accounts/disable", "/codearts/accounts/"),
		action("/codearts/accounts/enable", "/codearts/accounts/"),
		action("/codearts/accounts/note", "/codearts/accounts/"),
		action("/codearts/accounts/tasks", "/codearts/accounts/"),
		action("/codearts/accounts/test", "/codearts/accounts/"),
		action("/codearts/accounts/proxy", "/codearts/accounts/"),
		action("/codearts/accounts/oauth/start", "/codearts/accounts/"),
		action("/codearts/accounts/oauth/poll", "/codearts/accounts/"),
		action("/codearts/accounts/oauth/callback", "/codearts/accounts/"),
		post("/codearts/credits/refresh"),
		post("/codearts/keepalive"),
		post("/codearts/test"),

		get("/raccoon/overview", false),
		action("/raccoon/accounts/clear-cooldown", "/raccoon/accounts/"),
		action("/raccoon/accounts/delete", "/raccoon/accounts/"),
		action("/raccoon/accounts/disable", "/raccoon/accounts/"),
		action("/raccoon/accounts/enable", "/raccoon/accounts/"),
		action("/raccoon/accounts/grant-desktop", "/raccoon/accounts/"),
		action("/raccoon/accounts/grant-mobile", "/raccoon/accounts/"),
		action("/raccoon/accounts/import", "/raccoon/accounts/"),
		action("/raccoon/accounts/note", "/raccoon/accounts/"),
		action("/raccoon/accounts/oauth/callback", "/raccoon/accounts/"),
		action("/raccoon/accounts/oauth/poll", "/raccoon/accounts/"),
		action("/raccoon/accounts/oauth/start", "/raccoon/accounts/"),
		action("/raccoon/accounts/proxy", "/raccoon/accounts/"),
		action("/raccoon/accounts/tasks", "/raccoon/accounts/"),
		action("/raccoon/accounts/test", "/raccoon/accounts/"),
		post("/raccoon/credits/refresh"),
		post("/raccoon/test"),
		post("/raccoon/models/refresh"),
		post("/codearts/models/refresh"),

		get("/logs", false),
		post("/logs/clear"),
		post("/pricing/mapping"),
		post("/pricing/refresh"),
		get("/pricing", false),
		post("/proxy/discover"),
		get("/proxy/slots", false),
		post("/proxy/slots/save"),
		post("/proxy/slots/test"),
		post("/quota-scan/run"),

		get("/scheduler", false),
		post("/scheduler/toggle"),
		post("/scheduler/trigger"),
		get("/settings", false),
		post("/settings/restriction/reset"),
		get("/settings/reveal", false),
		post("/settings/save"),
		post("/shutdown"),

		get("/tasks", false),
		post("/tasks/claim"),
		post("/tasks/run"),
		post("/tasks/streak-bonus"),
		post("/tasks/travel"),

		get("/updates", false),
		get("/updates/backups", false),
		post("/updates/check"),
		post("/updates/install"),
		post("/updates/rollback"),

		get("/usage", false),
		get("/usage/analytics", false),
		get("/usage/by-account", false),
		get("/usage/perf", false),
		get("/usage/recent", false),
		get("/usage/remaining", false),
		get("/usage/timeseries", false),

		get("/v1/media/models", false),
		get("/v1/media/tasks", false),
		get("/v1/models", false),
		withSource(post("/v1/videos/{id}/cancel"), "/v1/videos/"),
	}
	return routes
}

func TestDashboardAPIInventoryIsCovered(t *testing.T) {
	html := string(web.DashboardHTML())
	routes := dashboardRoutes()

	// Every curated route must still be present in the shipped page. This is
	// the "dashboard changed under us" alarm: adding a fetch literal without a
	// route breaks here before it breaks a browser.
	sources := make(map[string]bool)
	for _, route := range routes {
		sources[route.source()] = true
	}
	for source := range sources {
		if !strings.Contains(html, jsonQuote(source)) && !strings.Contains(html, source) {
			t.Errorf("dashboard route source %q is no longer referenced by dashboard.html", source)
		}
	}

	// Discovery side: every getJSON/postJSON/fetch path literal in the page
	// must map to at least one curated route or be a documented static/alias
	// path. Queries are stripped because the route table is method+path only.
	discovered := discoverFetchPaths(html)
	for _, path := range discovered {
		matched := false
		for _, route := range routes {
			source := route.source()
			if source == path || (strings.HasSuffix(source, "/") && strings.HasPrefix(path, source)) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("dashboard fetch path %q has no entry in dashboardRoutes()", path)
		}
	}
	if len(discovered) < 80 {
		t.Fatalf("only %d dashboard fetch paths discovered; the extractor may be broken", len(discovered))
	}
	if len(routes) < 90 {
		t.Fatalf("route table shrank to %d entries", len(routes))
	}
}

func TestDashboardAPIRoutesExistAndAreGated(t *testing.T) {
	fixture := newDashboardFixture(t)
	token := fixture.login("admin")
	routes := dashboardRoutes()

	// Authenticated method-mismatch probes separate "route exists" (405/200)
	// from the panel subtree's unknown-path 404 without executing handlers.
	for _, route := range routes {
		probeMethod := http.MethodPatch
		// Three provider roots have a subtree registered under the trailing
		// slash, so ServeMux redirects a method-mismatch to "/accounts/" before
		// the route table can answer 405. A GET with the panel token is a local
		// cached read for an empty test fixture; use it to keep those roots
		// distinguishable from genuinely unknown paths.
		switch route.Path {
		case "/accounts", "/tasks", "/scheduler":
			probeMethod = http.MethodGet
		}
		reply := fixture.do(probeMethod, route.probePath(), "", token)
		if reply.Status == http.StatusNotFound {
			t.Errorf("%s %s is not registered (%s probe = 404, body=%s)", route.Method, route.Path, probeMethod, reply.RawText)
		}
		if probeMethod == http.MethodPatch && reply.Status != http.StatusMethodNotAllowed && reply.Status != http.StatusOK {
			t.Errorf("%s %s PATCH probe = %d, want 405 (or 200 for accepted methods)", route.Method, route.Path, reply.Status)
		}
	}

	// Unauthenticated calls must fail closed with the v1 JSON envelope, not a
	// bare 404. Public page/status/login routes are covered separately.
	for _, route := range routes {
		if route.Public {
			continue
		}
		reply := fixture.do(route.Method, route.probePath(), "", "")
		assertAuthError(t, reply, route.Method+" "+route.Path)
	}

	// Public first-paint endpoints stay reachable without a session.
	status := fixture.get("/panel/status", "")
	if status.Status != http.StatusOK {
		t.Fatalf("/panel/status = %d %s", status.Status, status.RawText)
	}
	if status.JSON["panel_password_required"] != true || status.JSON["authenticated"] != false {
		t.Fatalf("/panel/status = %#v", status.JSON)
	}
	health := fixture.get("/health", "")
	if health.Status != http.StatusOK || health.JSON["ok"] != true {
		t.Fatalf("/health = %d %#v", health.Status, health.JSON)
	}
}

func TestDashboardStaticAliases(t *testing.T) {
	fixture := newDashboardFixture(t)
	for _, path := range []string{"/", "/dashboard.html", "/dashboard", "/ui", "/classic", "/classic/"} {
		reply := fixture.get(path, "")
		if reply.Status != http.StatusOK {
			t.Fatalf("%s = %d", path, reply.Status)
		}
		if got := web.BodyHash(reply.Body); got != web.DashboardSHA256 {
			t.Fatalf("%s hash = %s", path, got)
		}
		if contentType := reply.Header.Get("Content-Type"); contentType != "text/html; charset=utf-8" {
			t.Fatalf("%s content-type = %q", path, contentType)
		}
	}
	if favicon := fixture.get("/favicon.ico", ""); favicon.Status != http.StatusNotFound {
		t.Fatalf("/favicon.ico = %d", favicon.Status)
	}
}

// discoverFetchPaths extracts the direct fetch/getJSON/postJSON path literals.
// It intentionally covers the simple string/template forms used by the shipped
// page; concatenated account-action paths are represented by their prefix.
func discoverFetchPaths(html string) []string {
	pattern := regexp.MustCompile("(?:getJSON|postJSON|fetch)\\(\\s*(?:\"([^\"]*)\"|'([^']*)'|`([^`]*)`)")
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, match := range pattern.FindAllStringSubmatch(html, -1) {
		literal := match[1] + match[2] + match[3]
		path, _, _ := strings.Cut(literal, "?")
		path = strings.TrimSpace(path)
		if path == "" || !strings.HasPrefix(path, "/") {
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func jsonQuote(value string) string {
	return `"` + value + `"`
}
