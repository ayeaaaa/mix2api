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

func TestSemanticEqualTreatsJSONNumbersByValue(t *testing.T) {
	if !semanticEqual(json.Number("0"), json.Number("0.0")) {
		t.Fatal("0 and 0.0 should compare equal")
	}
	if !semanticEqual(json.Number("4500"), json.Number("4500.00")) {
		t.Fatal("4500 and 4500.00 should compare equal")
	}
	if semanticEqual(json.Number("1"), json.Number("2")) {
		t.Fatal("1 and 2 should differ")
	}
	if !semanticEqual(
		map[string]any{"n": json.Number("1.0"), "s": "same"},
		map[string]any{"n": json.Number("1"), "s": "same"},
	) {
		t.Fatal("nested numeric spelling should compare equal")
	}
}

func TestDescribeDifferencesReportsPathsAndAdditiveFields(t *testing.T) {
	want := map[string]any{"stable": "a", "missing": true}
	got := map[string]any{"stable": "b", "extra": true}
	text := describeDifferences(want, got)
	for _, needle := range []string{
		"stable: \"b\", want \"a\"",
		"missing: missing on target",
		"extra: extra on target",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("difference report missing %q:\n%s", needle, text)
		}
	}
}

func TestCompareAllReportsEveryMismatch(t *testing.T) {
	var changed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := "same"
		if changed.Load() {
			value = "different-" + strings.TrimPrefix(r.URL.Path, "/")
		}
		writeTestJSON(w, map[string]any{"value": value})
	}))
	defer server.Close()

	cases := []Case{
		{Name: "one", Method: http.MethodGet, Path: "/one", Auth: AuthNone, Kind: "json"},
		{Name: "two", Method: http.MethodGet, Path: "/two", Auth: AuthNone, Kind: "json"},
	}
	dir := t.TempDir() + "/oracle"
	recorder := &Recorder{
		BaseURL: server.URL, Cases: cases,
		Provenance: Provenance{TargetVersion: "1.6.5", TargetCommit: "abc", FakeUpstream: true},
		Now:        func() time.Time { return time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC) },
	}
	if err := recorder.Record(context.Background(), dir); err != nil {
		t.Fatalf("record: %v", err)
	}
	comparer := &Comparer{BaseURL: server.URL, Dir: dir}
	if diffs := comparer.CompareAll(context.Background()); len(diffs) != 0 {
		t.Fatalf("unexpected diffs: %v", diffs)
	}
	changed.Store(true)
	diffs := comparer.CompareAll(context.Background())
	if len(diffs) != 2 {
		t.Fatalf("CompareAll returned %d diffs, want 2: %v", len(diffs), diffs)
	}
	names := map[string]bool{}
	for _, diff := range diffs {
		names[diff.Name] = true
	}
	if !names["one"] || !names["two"] {
		t.Fatalf("CompareAll names = %v", names)
	}
}

func TestLoadRejectsIncompleteProvenanceAndUnsafeCasePaths(t *testing.T) {
	dir := t.TempDir()
	hash := strings.Repeat("a", 64)
	write := func(document Document) {
		raw, _ := json.Marshal(document)
		_ = os.WriteFile(filepath.Join(dir, "oracle.json"), raw, 0o644)
	}
	base := Document{
		Format: Format,
		Provenance: Provenance{
			TargetVersion: "1.6.5", TargetCommit: "", FakeUpstream: true,
			Network: "loopback-only", RecordedAt: "2026-11-08T00:00:00Z", BaseURL: "http://127.0.0.1:1",
		},
		Cases: []Case{{Name: "one", Method: "GET", Path: "/one", Kind: "json", Status: 200, File: "one.json", SHA256: hash}},
	}
	write(base)
	if _, err := Load(dir); err == nil {
		t.Fatal("missing commit accepted")
	}
	base.Provenance.TargetCommit = "abc"
	base.Cases[0].File = "../one.json"
	write(base)
	if _, err := Load(dir); err == nil {
		t.Fatal("unsafe fixture filename accepted")
	}
}

func TestProvenanceAndCaseValidationMatrix(t *testing.T) {
	valid := Provenance{
		TargetVersion: "1.6.5", TargetCommit: "e40c4e8", FakeUpstream: true,
		Network: "loopback-only", RecordedAt: "2026-11-08T00:00:00Z",
		BaseURL: "http://127.0.0.1:18765",
	}
	if err := validateProvenance(valid); err != nil {
		t.Fatalf("valid provenance rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Provenance){
		"missing version":      func(p *Provenance) { p.TargetVersion = " " },
		"missing commit":       func(p *Provenance) { p.TargetCommit = "" },
		"real upstream":        func(p *Provenance) { p.FakeUpstream = false },
		"network case":         func(p *Provenance) { p.Network = "Loopback-Only" },
		"network whitespace":   func(p *Provenance) { p.Network = "loopback-only " },
		"recorded at not time": func(p *Provenance) { p.RecordedAt = "yesterday" },
		"remote base url":      func(p *Provenance) { p.BaseURL = "https://github.com" },
		"file base url":        func(p *Provenance) { p.BaseURL = "file:///tmp/oracle" },
		"empty base url":       func(p *Provenance) { p.BaseURL = "" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := validateProvenance(candidate); err == nil {
				t.Fatalf("invalid provenance accepted: %#v", candidate)
			}
		})
	}

	hash := strings.Repeat("a", 64)
	for name, item := range map[string]Case{
		"bad method":  {Name: "one", Method: "", Path: "/x", Kind: "json", Status: 200, File: "x.json", SHA256: hash},
		"relative":    {Name: "one", Method: "GET", Path: "x", Kind: "json", Status: 200, File: "x.json", SHA256: hash},
		"url path":    {Name: "one", Method: "GET", Path: "http://evil", Kind: "json", Status: 200, File: "x.json", SHA256: hash},
		"kind case":   {Name: "one", Method: "GET", Path: "/x", Kind: "JSON", Status: 200, File: "x.json", SHA256: hash},
		"status zero": {Name: "one", Method: "GET", Path: "/x", Kind: "json", Status: 0, File: "x.json", SHA256: hash},
		"no name":     {Name: "", Method: "GET", Path: "/x", Kind: "json", Status: 200, File: "x.json", SHA256: hash},
		"upper hash":  {Name: "one", Method: "GET", Path: "/x", Kind: "json", Status: 200, File: "x.json", SHA256: strings.Repeat("A", 64)},
	} {
		t.Run("case "+name, func(t *testing.T) {
			if err := validateCases([]Case{item}); err == nil {
				t.Fatalf("invalid case accepted: %#v", item)
			}
		})
	}
	duplicate := []Case{
		{Name: "dup", Method: "GET", Path: "/x", Kind: "json", Status: 200, File: "x.json", SHA256: hash},
		{Name: "dup", Method: "GET", Path: "/y", Kind: "json", Status: 200, File: "y.json", SHA256: hash},
	}
	if err := validateCases(duplicate); err == nil {
		t.Fatal("duplicate case names accepted")
	}
}
