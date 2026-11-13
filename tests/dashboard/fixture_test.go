package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/app"
)

const (
	dashboardVersion = "2.0.0-dashboard-test"
	dashboardAPIKey  = "dashboard-test-api-key"
)

// dashboardFixture is the production composition in miniature: app.Build
// constructs every provider, app.BuildPlatform constructs the platform
// services over the same settings store, and api.New mounts the real route
// table. No handler stubs or pass-through auth are used.
type dashboardFixture struct {
	t        *testing.T
	Platform *app.Platform
	Server   *api.Server
	HTTP     *httptest.Server
	Client   *http.Client

	shutdownCalls atomic.Int32
}

func newDashboardFixture(t *testing.T) *dashboardFixture {
	t.Helper()
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
		Version:       dashboardVersion,
		PanelPassword: "admin",
		MediaDir:      filepath.Join(root, "media"),
		UpdateDir:     filepath.Join(root, "updates"),
		Home:          root,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &dashboardFixture{t: t, Platform: platform}
	server := api.New(api.ServerOptions{
		Version:      dashboardVersion,
		Providers:    setup.Providers,
		Platform:     platform.HTTPServices(),
		APIKeyAuth:   api.RequireAPIKeyAuth(dashboardAPIKey),
		StaticAPIKey: dashboardAPIKey,
		Shutdown: func() {
			fixture.shutdownCalls.Add(1)
		},
	})
	fixture.Server = server
	fixture.HTTP = httptest.NewServer(server.Handler())
	fixture.Client = fixture.HTTP.Client()
	t.Cleanup(func() {
		fixture.HTTP.Close()
		platform.Close()
	})
	return fixture
}

type dashboardReply struct {
	Status  int
	Header  http.Header
	Body    []byte
	JSON    map[string]any
	RawText string
}

func (f *dashboardFixture) do(method, path, body, token string, headers ...map[string]string) dashboardReply {
	f.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.HTTP.URL+path, reader)
	if err != nil {
		f.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Panel-Token", token)
	}
	for _, set := range headers {
		for name, value := range set {
			req.Header.Set(name, value)
		}
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		f.t.Fatal(err)
	}
	reply := dashboardReply{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: raw, RawText: string(raw)}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&reply.JSON); err != nil {
		reply.JSON = nil
	}
	return reply
}

func (f *dashboardFixture) get(path, token string) dashboardReply {
	f.t.Helper()
	return f.do(http.MethodGet, path, "", token)
}

func (f *dashboardFixture) post(path, body, token string) dashboardReply {
	f.t.Helper()
	return f.do(http.MethodPost, path, body, token)
}

// login uses the real panel endpoint and returns the session token.
func (f *dashboardFixture) login(password string) string {
	f.t.Helper()
	reply := f.post("/panel/login", `{"password":`+jsonString(password)+`}`, "")
	if reply.Status != http.StatusOK {
		f.t.Fatalf("panel login status=%d body=%s", reply.Status, reply.RawText)
	}
	token, _ := reply.JSON["token"].(string)
	if strings.TrimSpace(token) == "" {
		f.t.Fatalf("panel login returned no token: %s", reply.RawText)
	}
	return token
}

func jsonString(value string) string {
	body, _ := json.Marshal(value)
	return string(body)
}

func errorMessage(t *testing.T, reply dashboardReply) string {
	t.Helper()
	envelope, _ := reply.JSON["error"].(map[string]any)
	message, _ := envelope["message"].(string)
	return message
}

func assertAuthError(t *testing.T, reply dashboardReply, label string) {
	t.Helper()
	if reply.Status != http.StatusUnauthorized && reply.Status != http.StatusForbidden {
		t.Fatalf("%s status=%d body=%s", label, reply.Status, reply.RawText)
	}
	if reply.JSON == nil {
		t.Fatalf("%s did not return a JSON envelope: %s", label, reply.RawText)
	}
	envelope, ok := reply.JSON["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s missing error envelope: %s", label, reply.RawText)
	}
	if envelope["type"] == "" || envelope["code"] == nil {
		t.Fatalf("%s incomplete error envelope: %s", label, reply.RawText)
	}
}
