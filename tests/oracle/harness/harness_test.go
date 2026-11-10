package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testCases() []Case {
	return []Case{
		{Name: "json", Method: http.MethodGet, Path: "/json", Auth: AuthNone, Kind: "json"},
		{Name: "stream", Method: http.MethodGet, Path: "/stream", Auth: AuthNone, Kind: "sse"},
		{Name: "panel", Method: http.MethodGet, Path: "/panel", Auth: AuthPanel, Kind: "json"},
		{Name: "api", Method: http.MethodGet, Path: "/api", Auth: AuthAPI, Kind: "json"},
	}
}

func TestRecordCompareAndDynamicNormalization(t *testing.T) {
	var run atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			writeTestJSON(w, map[string]any{
				"ok": true, "version": "2.0.0", "id": fmt.Sprintf("chatcmpl-%d", run.Load()),
				"created": 1700000000 + int(run.Load()), "stable": "same",
			})
		case "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-%d\",\"object\":\"chat.completion.chunk\",\"created\":%d,\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n", run.Load(), 1700000000+int(run.Load()))
			fmt.Fprint(w, "data: [DONE]\n\n")
		case "/panel":
			if r.Header.Get("X-Panel-Token") != "panel-secret" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			writeTestJSON(w, map[string]any{"enabled": true})
		case "/api":
			if r.Header.Get("Authorization") != "Bearer api-secret" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			writeTestJSON(w, map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir() + "/oracle"
	creds := Credentials{PanelToken: "panel-secret", APIKey: "api-secret"}
	recorder := &Recorder{
		BaseURL: server.URL, Creds: creds, Cases: testCases(),
		Provenance: Provenance{TargetVersion: "1.6.5", TargetCommit: "abc", FakeUpstream: true},
		Now:        func() time.Time { return time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC) },
	}
	if err := recorder.Record(context.Background(), dir); err != nil {
		t.Fatalf("record: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"fake_upstream": true`) || !strings.Contains(string(raw), `"network": "loopback-only"`) {
		t.Fatalf("provenance = %s", raw)
	}
	run.Store(7)
	comparer := &Comparer{BaseURL: server.URL, Dir: dir, Creds: creds}
	if err := comparer.Compare(context.Background()); err != nil {
		t.Fatalf("compare: %v", err)
	}

	// A semantic change must fail.
	run.Store(8)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			writeTestJSON(w, map[string]any{"ok": true, "version": "2.0.0", "id": "chatcmpl-8", "created": 1, "stable": "different"})
		case "/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-8\",\"object\":\"chat.completion.chunk\",\"created\":1,\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"bye\"}}]}\n\ndata: [DONE]\n\n")
		case "/panel":
			writeTestJSON(w, map[string]any{"enabled": true})
		case "/api":
			writeTestJSON(w, map[string]any{"ok": true})
		}
	})
	if err := comparer.Compare(context.Background()); err == nil {
		t.Fatal("semantic mismatch accepted")
	}
}

func TestRecordRequiresFakeUpstreamAndLoopback(t *testing.T) {
	dir := t.TempDir()
	recorder := &Recorder{
		BaseURL:    "http://example.com",
		Provenance: Provenance{TargetVersion: "1.6.5", TargetCommit: "abc", FakeUpstream: true},
	}
	if err := recorder.Record(context.Background(), filepath.Join(dir, "a")); err == nil {
		t.Fatal("non-loopback recording accepted")
	}
	recorder = &Recorder{
		BaseURL:    "http://127.0.0.1:1",
		Provenance: Provenance{TargetVersion: "1.6.5", TargetCommit: "abc"},
	}
	if err := recorder.Record(context.Background(), filepath.Join(dir, "b")); err == nil {
		t.Fatal("recording without fake upstream accepted")
	}
}

func TestLoadRejectsTamperedFixture(t *testing.T) {
	dir := t.TempDir()
	manifest := Document{
		Format: Format,
		Provenance: Provenance{
			TargetVersion: "1.6.5", TargetCommit: "abc", FakeUpstream: true,
			Network: "loopback-only", RecordedAt: "2026-11-08T00:00:00Z",
		},
		Cases: []Case{{Name: "one", Method: "GET", Path: "/one", Kind: "json", Status: 200, File: "one.json", SHA256: strings.Repeat("0", 64)}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "oracle.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "one.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("tampered fixture accepted")
	}
}

func TestValidateLoopbackAndJoinURL(t *testing.T) {
	for _, value := range []string{"http://127.0.0.1:8080", "http://localhost:1", "http://[::1]:9"} {
		if err := validateLoopback(value); err != nil {
			t.Fatalf("%s rejected: %v", value, err)
		}
	}
	for _, value := range []string{"http://example.com", "http://192.168.1.2", "file:///tmp/x", ""} {
		if err := validateLoopback(value); err == nil {
			t.Fatalf("%q accepted", value)
		}
	}
	joined, err := joinURL("http://127.0.0.1:9/base", "/health?force=1")
	if err != nil || joined != "http://127.0.0.1:9/health?force=1" {
		t.Fatalf("joinURL = %q %v", joined, err)
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
