package web

import (
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestDashboardAssetHash(t *testing.T) {
	body := DashboardHTML()
	if got := BodyHash(body); got != DashboardSHA256 {
		t.Fatalf("dashboard SHA-256 = %s, want %s", got, DashboardSHA256)
	}
	if len(body) == 0 {
		t.Fatal("embedded dashboard is empty")
	}
	if !strings.Contains(string(body), "data-ui-language=") {
		t.Fatal("embedded dashboard does not look like the v1 page")
	}
	// The v2 asset keeps the v1 repository links byte-for-byte.
	if DashboardV1SHA256 != DashboardSHA256 {
		t.Fatal("v2 dashboard must remain byte-identical to v1.6.5")
	}
	if strings.Contains(string(body), `github.com/ayeaaaa/mix2api-v2"`) {
		t.Fatal("dashboard still links to the retired v2 repository")
	}
	if count := strings.Count(string(body), `github.com/ayeaaaa/mix2api"`); count != 2 {
		t.Fatalf("dashboard repository link count = %d, want 2", count)
	}
}

func TestDashboardServedAtV1Paths(t *testing.T) {
	handler := Handler()
	body := DashboardHTML()
	for _, path := range []string{"/", "/dashboard.html", "/dashboard", "/ui", "/classic", "/classic/"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Fatalf("%s content-type = %q", path, got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s cache-control = %q", path, got)
		}
		if got := rec.Header().Get("ETag"); got != DashboardETag() || got == "" {
			t.Fatalf("%s etag = %q", path, got)
		}
		if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
			t.Fatalf("%s content-length = %q want %d", path, got, len(body))
		}
		if rec.Body.Len() != len(body) || rec.Body.String() != string(body) {
			t.Fatalf("%s body is not byte-identical to the embedded asset", path)
		}
	}
}

func TestDashboardConditionalGetAndMethods(t *testing.T) {
	handler := Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", DashboardETag())
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 body = %q", rec.Body.String())
	}
	if rec.Header().Get("ETag") != DashboardETag() || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("304 headers = %#v", rec.Header())
	}
	if rec.Header().Get("Content-Type") != "" {
		t.Fatalf("304 must not advertise a body type: %q", rec.Header().Get("Content-Type"))
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", "W/"+DashboardETag()+", \"other\"")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("weak/list conditional status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD status=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Length") != strconv.Itoa(len(DashboardHTML())) {
		t.Fatalf("HEAD content-length = %q", rec.Header().Get("Content-Length"))
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST status=%d allow=%q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestDashboardRejectsUnknownAndTraversalPaths(t *testing.T) {
	handler := Handler()
	for _, path := range []string{
		"/dashboard.html/..",
		"/../internal/web/dashboard.html",
		"/assets/../../etc/passwd",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/dashboard.html%00.txt",
		"/robots.txt",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("favicon status = %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("favicon cache-control = %q", rec.Header().Get("Cache-Control"))
	}
}

func TestWebImportsNoManagementPackages(t *testing.T) {
	root := webPackageRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			for _, forbidden := range []string{
				"github.com/ayeaaaa/mix2api-v2/internal/providers",
				"github.com/ayeaaaa/mix2api-v2/internal/platform",
				"github.com/ayeaaaa/mix2api-v2/internal/api",
				"github.com/ayeaaaa/mix2api-v2/internal/app",
			} {
				if importPath == forbidden || strings.HasPrefix(importPath, forbidden+"/") {
					t.Fatalf("%s imports management package %s", filepath.Base(path), importPath)
				}
			}
		}
	}
}

func webPackageRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(file), "dashboard.html")); err != nil {
		t.Fatalf("dashboard.html missing: %v", err)
	}
	return filepath.Dir(file)
}
