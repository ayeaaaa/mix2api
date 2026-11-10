package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/web"
)

func TestPanelFirstPaintLoginAndGate(t *testing.T) {
	fixture := newDashboardFixture(t)

	// The page itself is public and byte-identical to the embedded asset.
	root := fixture.get("/", "")
	if root.Status != http.StatusOK {
		t.Fatalf("dashboard = %d", root.Status)
	}
	if got := web.BodyHash(root.Body); got != web.DashboardSHA256 {
		t.Fatalf("dashboard hash = %s", got)
	}
	if len(root.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("dashboard set a cookie: %v", root.Header.Values("Set-Cookie"))
	}
	if root.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("management dashboard must not be CORS-readable: %q", root.Header.Get("Access-Control-Allow-Origin"))
	}

	// /panel/status is the first-paint decision and must answer without auth.
	status := fixture.get("/panel/status", "")
	if status.Status != http.StatusOK {
		t.Fatalf("panel status = %d", status.Status)
	}
	if status.JSON["panel_password_required"] != true ||
		status.JSON["panel_password_is_default"] != true ||
		status.JSON["authenticated"] != false ||
		status.JSON["api_key_set"] != false {
		t.Fatalf("first paint status = %#v", status.JSON)
	}
	if _, leaked := status.JSON["token"]; leaked {
		t.Fatalf("first paint status leaked a token: %#v", status.JSON)
	}
	if strings.Contains(status.RawText, "sk-") {
		t.Fatalf("panel status leaked a key: %s", status.RawText)
	}

	// The gate covers the settings subtree before any handler runs.
	blocked := fixture.get("/settings", "")
	assertAuthError(t, blocked, "unauthenticated settings")
	if errorMessage(t, blocked) != "panel password required" {
		t.Fatalf("settings gate message = %q", errorMessage(t, blocked))
	}

	token := fixture.login("admin")
	if strings.Contains(token, "admin") {
		t.Fatal("session token must be independent of the password")
	}
	authed := fixture.get("/panel/status", token)
	if authed.JSON["authenticated"] != true || authed.JSON["panel_password_is_default"] != true {
		t.Fatalf("authenticated status = %#v", authed.JSON)
	}
	if strings.Contains(authed.RawText, token) {
		t.Fatalf("panel status echoed the session token: %s", authed.RawText)
	}
	if len(authed.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("panel session must travel in X-Panel-Token, not a cookie: %v", authed.Header.Values("Set-Cookie"))
	}

	settings := fixture.get("/settings", token)
	if settings.Status != http.StatusOK {
		t.Fatalf("authenticated settings = %d %s", settings.Status, settings.RawText)
	}
	for _, key := range []string{"panel_password_is_default", "api_keys", "auth_required", "raccoon", "codearts", "usage_dir"} {
		if _, ok := settings.JSON[key]; !ok {
			t.Fatalf("settings missing %q", key)
		}
	}
}

func TestPanelLogoutAndPasswordRotation(t *testing.T) {
	fixture := newDashboardFixture(t)
	token := fixture.login("admin")

	settings := fixture.get("/settings", token)
	if settings.Status != http.StatusOK {
		t.Fatalf("settings before logout = %d", settings.Status)
	}
	logout := fixture.post("/panel/logout", `{}`, token)
	if logout.Status != http.StatusOK || logout.JSON["ok"] != true {
		t.Fatalf("logout = %d %s", logout.Status, logout.RawText)
	}
	assertAuthError(t, fixture.get("/settings", token), "settings after logout")

	// Log in again while the fresh-install password is still active, then
	// rotate it. The old session and old password must both stop working.
	oldToken := fixture.login("admin")
	rotated := fixture.post("/panel/password", `{"current":"admin","new":"dash-secret-42"}`, oldToken)
	if rotated.Status != http.StatusOK {
		t.Fatalf("password rotation = %d %s", rotated.Status, rotated.RawText)
	}
	if rotated.JSON["using_default_password"] != false {
		t.Fatalf("rotation still reports default password: %#v", rotated.JSON)
	}
	newToken, _ := rotated.JSON["token"].(string)
	if newToken == "" || newToken == oldToken {
		t.Fatalf("rotation token = %q", newToken)
	}
	assertAuthError(t, fixture.get("/settings", oldToken), "old session token")

	wrong := fixture.post("/panel/login", `{"password":"admin"}`, "")
	if wrong.Status != http.StatusUnauthorized {
		t.Fatalf("old password login = %d %s", wrong.Status, wrong.RawText)
	}
	freshToken := fixture.login("dash-secret-42")
	if fresh := fixture.get("/settings", freshToken); fresh.Status != http.StatusOK {
		t.Fatalf("new password session = %d", fresh.Status)
	}
	if rotatedSession := fixture.get("/settings", newToken); rotatedSession.Status != http.StatusOK {
		t.Fatalf("rotation session = %d", rotatedSession.Status)
	}
}

func TestSettingsSaveRevealAndNoSecretLeak(t *testing.T) {
	fixture := newDashboardFixture(t)
	token := fixture.login("admin")

	const clearKey = "sk-dashboard-test-secret-1234"
	saved := fixture.post("/settings/save", `{"api_keys":[{"id":"k1","name":"panel key","key":"`+clearKey+`","realm":"intl","enabled":true}]}`, token)
	if saved.Status != http.StatusOK {
		t.Fatalf("save key = %d %s", saved.Status, saved.RawText)
	}
	if strings.Contains(saved.RawText, clearKey) {
		t.Fatalf("save response leaked the clear key: %s", saved.RawText)
	}

	settings := fixture.get("/settings", token)
	if settings.Status != http.StatusOK {
		t.Fatalf("settings = %d", settings.Status)
	}
	body := settings.RawText
	if strings.Contains(body, clearKey) {
		t.Fatalf("settings leaked the clear key: %s", body)
	}
	if strings.Contains(body, token) {
		t.Fatalf("settings leaked the session token: %s", body)
	}
	rows, _ := settings.JSON["api_keys"].([]any)
	if len(rows) != 1 {
		t.Fatalf("api_keys = %#v", settings.JSON["api_keys"])
	}
	row, _ := rows[0].(map[string]any)
	if row["id"] != "k1" || row["name"] != "panel key" {
		t.Fatalf("key summary = %#v", row)
	}
	if _, present := row["key"]; present {
		t.Fatalf("key summary contains a clear key: %#v", row)
	}
	masked, _ := row["masked"].(string)
	if masked == "" || masked == clearKey || !strings.Contains(masked, "*") {
		t.Fatalf("masked key = %q", masked)
	}

	revealed := fixture.get("/settings/reveal?id=k1", token)
	if revealed.Status != http.StatusOK || revealed.JSON["key"] != clearKey {
		t.Fatalf("reveal = %d %#v", revealed.Status, revealed.JSON)
	}
	assertAuthError(t, fixture.get("/settings/reveal?id=k1", ""), "unauthenticated reveal")

	saveLanguage := fixture.post("/settings/save", `{"ui_language":"en","section_max_height":700}`, token)
	if saveLanguage.Status != http.StatusOK || saveLanguage.JSON["ok"] != true {
		t.Fatalf("save settings = %d %s", saveLanguage.Status, saveLanguage.RawText)
	}
	updated := fixture.get("/settings", token)
	if updated.JSON["ui_language"] != "en" {
		t.Fatalf("ui_language = %#v", updated.JSON["ui_language"])
	}
	if number, ok := updated.JSON["section_max_height"].(json.Number); !ok || number.String() != "700" {
		t.Fatalf("section_max_height = %#v", updated.JSON["section_max_height"])
	}
}

func TestRealmSwitchAndProviderViews(t *testing.T) {
	fixture := newDashboardFixture(t)
	token := fixture.login("admin")

	realm := fixture.get("/realm", token)
	if realm.Status != http.StatusOK {
		t.Fatalf("realm = %d %s", realm.Status, realm.RawText)
	}
	if realm.JSON["current"] != "intl" {
		t.Fatalf("initial realm = %#v", realm.JSON)
	}
	options, _ := realm.JSON["options"].([]any)
	if len(options) < 2 {
		t.Fatalf("realm options = %#v", realm.JSON["options"])
	}

	switchReply := fixture.post("/realm", `{"realm":"cn"}`, token)
	if switchReply.Status != http.StatusOK || switchReply.JSON["current"] != "cn" {
		t.Fatalf("realm switch = %d %s", switchReply.Status, switchReply.RawText)
	}
	if current := fixture.get("/realm", token); current.JSON["current"] != "cn" {
		t.Fatalf("persisted realm = %#v", current.JSON)
	}
	invalid := fixture.post("/realm", `{"realm":"bogus"}`, token)
	if invalid.Status != http.StatusBadRequest {
		t.Fatalf("invalid realm = %d %s", invalid.Status, invalid.RawText)
	}

	// Provider account views and the panel card endpoints all compose from the
	// real services; an unconfigured provider may answer its v1 error, but it
	// must not look like an unknown route.
	for _, path := range []string{
		"/accounts?realm=all",
		"/accounts/credits/grants",
		"/raccoon/overview",
		"/raccoon/accounts",
		"/agents/available",
		"/proxy/slots",
		"/pricing",
		"/updates",
		"/updates/backups",
		"/usage",
		"/logs",
		"/scheduler",
		"/tasks",
	} {
		reply := fixture.get(path, token)
		if reply.Status == http.StatusNotFound {
			t.Errorf("%s unexpectedly 404: %s", path, reply.RawText)
		}
		if reply.JSON == nil {
			t.Errorf("%s did not return JSON: %s", path, reply.RawText)
		}
	}

	raccoon := fixture.get("/raccoon/overview", token)
	if raccoon.Status != http.StatusOK {
		t.Fatalf("raccoon overview = %d %s", raccoon.Status, raccoon.RawText)
	}
	if _, ok := raccoon.JSON["accounts"]; !ok {
		t.Fatalf("raccoon overview missing accounts: %#v", raccoon.JSON)
	}
	if strings.Contains(raccoon.RawText, token) {
		t.Fatalf("raccoon overview leaked the session token: %s", raccoon.RawText)
	}

	codearts := fixture.get("/codearts/overview", token)
	if codearts.Status == http.StatusNotFound {
		t.Fatalf("codearts overview unexpectedly 404: %s", codearts.RawText)
	}
	if codearts.JSON == nil {
		t.Fatalf("codearts overview did not return JSON: %s", codearts.RawText)
	}
}

func TestPanelConsoleAndShutdownSeam(t *testing.T) {
	fixture := newDashboardFixture(t)
	assertAuthError(t, fixture.post("/shutdown", `{}`, ""), "unauthenticated shutdown")

	token := fixture.login("admin")
	state := fixture.get("/console/state", token)
	if state.Status != http.StatusOK || state.JSON["mode"] != "web" || state.JSON["supported"] != true {
		t.Fatalf("console state = %d %#v", state.Status, state.JSON)
	}
	toggle := fixture.post("/console/toggle", `{}`, token)
	if toggle.Status != http.StatusOK || toggle.JSON["url"] != "/dashboard?tab=logs" {
		t.Fatalf("console toggle = %d %#v", toggle.Status, toggle.JSON)
	}

	shutdown := fixture.post("/shutdown", `{}`, token)
	if shutdown.Status != http.StatusOK || shutdown.JSON["shutting_down"] != true {
		t.Fatalf("shutdown = %d %s", shutdown.Status, shutdown.RawText)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fixture.shutdownCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fixture.shutdownCalls.Load() != 1 {
		t.Fatalf("shutdown callback calls = %d", fixture.shutdownCalls.Load())
	}
	// Repeated clicks must not schedule a second process shutdown.
	if second := fixture.post("/shutdown", `{}`, token); second.Status != http.StatusOK {
		t.Fatalf("second shutdown = %d", second.Status)
	}
	time.Sleep(50 * time.Millisecond)
	if fixture.shutdownCalls.Load() != 1 {
		t.Fatalf("shutdown callback repeated: %d", fixture.shutdownCalls.Load())
	}
	// The injected seam keeps the test server alive; production wires the same
	// callback to http.Server.Shutdown in cmd.
	if again := fixture.get("/health", ""); again.Status != http.StatusOK {
		t.Fatalf("health after injected shutdown = %d", again.Status)
	}
}
