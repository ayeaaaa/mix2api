// Package web serves the embedded v1.6.5 dashboard (plus the top-bar
// total-quota columns, daily-active controls and branding controls) and its static aliases. No build step rewrites it.
// This package deliberately imports no provider or platform code so the
// static surface cannot grow a management dependency.
package web

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// The shipped dashboard. It is the v1.6.5 asset plus the top-bar total-quota
// columns, daily-active controls and branding controls, including the two repository links documented in
// tests/oracle/dashboard.md; the contract test pins its SHA-256.
//
//go:embed dashboard.html
var dashboardHTML []byte

// DashboardSHA256 is the lowercase hex digest of the embedded dashboard. It is
// recorded in tests/oracle/dashboard.md and checked by the contract test.
const DashboardSHA256 = "85b80abdac4b7c70e1bdc57efcd249f80bb6854dd31588a064f7cda2efe649af"

// DashboardV1SHA256 is the original v1.6.5 digest. It differs from
// DashboardSHA256 because the v2 asset adds the top-bar total-quota columns
// daily-active and branding controls.
const DashboardV1SHA256 = "70aa3ddf8c9a1164f140d64e785941d99a7754c3c44887b044a50127e0a96793"

var (
	etagOnce sync.Once
	etag     string
)

// DashboardHTML returns a copy of the embedded dashboard bytes.
func DashboardHTML() []byte {
	return append([]byte(nil), dashboardHTML...)
}

// DashboardETag returns the strong validator for the static response. The body
// is immutable for the lifetime of the process, so a content hash is complete.
func DashboardETag() string {
	etagOnce.Do(func() {
		sum := sha256.Sum256(dashboardHTML)
		// A short prefix is plenty for cache validation and keeps the header
		// readable; the full hash is exposed through DashboardSHA256.
		etag = `"` + hex.EncodeToString(sum[:16]) + `"`
	})
	return etag
}

// Handler serves the dashboard at "/" and its documented aliases. It is an
// exact-path handler: any other path is a 404, so mounting it under a catch-all
// mux entry cannot turn into a directory server. GET and HEAD are accepted;
// other methods get 405 with an Allow header.
func Handler() http.Handler {
	return http.HandlerFunc(serve)
}

func serve(w http.ResponseWriter, r *http.Request) {
	if r == nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	switch r.URL.Path {
	case "/", "/dashboard", "/dashboard.html", "/ui", "/classic", "/classic/":
		serveDashboard(w, r)
	case "/favicon.ico":
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func serveDashboard(w http.ResponseWriter, r *http.Request) {
	tag := DashboardETag()
	if ifNoneMatch(r.Header.Get("If-None-Match"), tag) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		w.Header().Set("ETag", tag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("ETag", tag)
	w.Header().Set("Content-Length", strconv.Itoa(len(dashboardHTML)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(dashboardHTML)
}

// ifNoneMatch implements the strong/weak comparison RFC 7232 §3.2 requires for
// If-None-Match. "*" matches any current representation.
func ifNoneMatch(header, current string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if candidate == "*" {
			return true
		}
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == current {
			return true
		}
	}
	return false
}

// BodyHash returns the SHA-256 of arbitrary dashboard bytes. Tests use it to
// prove an alternate copy is byte-identical without duplicating hash logic.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
