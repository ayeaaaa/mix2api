package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/app"
)

// TestProductionServerOptionsBootstrapPanelAuth verifies the exact command
// composition: no static PanelAuth path exists, the env password only opens a
// fresh install, and a rotation rejects the old password.
func TestProductionServerOptionsBootstrapPanelAuth(t *testing.T) {
	root := t.TempDir()
	setup, err := app.Build(app.Paths{
		AccountsDir: filepath.Join(root, "accounts"),
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err != nil {
		t.Fatal(err)
	}
	platform, err := app.BuildPlatform(setup, app.PlatformOptions{
		Paths:         setup.Paths,
		PanelPassword: "old-panel",
		MediaDir:      filepath.Join(root, "media"),
		UpdateDir:     filepath.Join(root, "updates"),
		Home:          root,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	cfg := commandConfig{Host: "127.0.0.1", Port: "0", PanelPassword: "old-panel", APIKey: "launcher-key"}
	options := buildServerOptions(cfg, setup, platform, 0)
	if options.PanelAuth != nil {
		t.Fatal("production options installed a static PanelAuth path")
	}
	server := api.New(options)

	request := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, reader)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := request(http.MethodGet, "/settings", "", map[string]string{"X-Panel-Token": "old-panel"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bootstrap password acted as a static panel token: %d body=%s", rec.Code, rec.Body)
	}
	rec = request(http.MethodPost, "/panel/login", `{"password":"old-panel"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap login = %d body=%s", rec.Code, rec.Body)
	}
	var login map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	token, _ := login["token"].(string)
	if token == "" {
		t.Fatalf("login token = %v", login)
	}
	if rec := request(http.MethodGet, "/settings", "", map[string]string{"X-Panel-Token": token}); rec.Code != http.StatusOK {
		t.Fatalf("session settings = %d body=%s", rec.Code, rec.Body)
	}

	if err := platform.PanelPassword.Set("new-panel"); err != nil {
		t.Fatal(err)
	}
	if rec := request(http.MethodPost, "/panel/login", `{"password":"old-panel"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password after rotation = %d body=%s", rec.Code, rec.Body)
	}
	if rec := request(http.MethodPost, "/panel/login", `{"password":"new-panel"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("new password after rotation = %d body=%s", rec.Code, rec.Body)
	}
}
