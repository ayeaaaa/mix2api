package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
)

func usageTestService(t *testing.T) *usage.Service {
	t.Helper()
	dir := t.TempDir()
	usageDir := filepath.Join(dir, "usage")
	if err := os.MkdirAll(usageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := usage.Paths{
		Dir:           usageDir,
		Log:           filepath.Join(usageDir, "usage.jsonl"),
		LimitEvents:   filepath.Join(usageDir, "limit-events.jsonl"),
		EstimateDaily: filepath.Join(usageDir, "estimate-daily.json"),
		RecentIndex:   filepath.Join(usageDir, "usage-recent-index.json"),
	}
	store := usage.New(paths)
	now := time.Now()
	for index := 0; index < 3; index++ {
		row := map[string]any{
			"at":                float64(now.Unix() - int64(index)*60),
			"model":             "m",
			"realm":             "intl",
			"account":           "a",
			"key":               "k1",
			"outcome":           "completed",
			"kind":              "chat",
			"prompt_tokens":     100,
			"completion_tokens": 10,
			"total_tokens":      110,
			"credit":            1.0,
		}
		if err := store.Append(row); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("WB_PRICING_FILE", filepath.Join(dir, "missing-pricing.json"))
	t.Setenv("WB_STATS_TTL", "15")
	service := usage.NewService(store, nil, usage.NewCache(usageDir))
	service.Now = func() time.Time { return now }
	service.StartedAt = now.Add(-time.Hour)
	service.Keys = []usage.KeyInfo{{ID: "k1", Name: "One", Enabled: true}}
	return service
}

func perform(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestUsageRoutes(t *testing.T) {
	service := usageTestService(t)
	mux := http.NewServeMux()
	NewUsageHandlers(service, nil).Register(mux)

	cases := []struct {
		path        string
		status      int
		contentType string
		contains    []string
	}{
		{"/usage?realm=intl&range=today", 200, "application/json; charset=utf-8", []string{`"requests"`, `"by_model"`, `"log_file"`}},
		{"/v1/usage?realm=intl", 200, "application/json; charset=utf-8", []string{`"usd_cny"`}},
		{"/usage/recent?realm=intl&limit=2&page=2", 200, "application/json; charset=utf-8", []string{`"total"`, `"rows"`, `"key_name"`}},
		{"/usage/analytics?realm=intl&range=today&since=1&until=2", 200, "application/json; charset=utf-8", []string{`"summary"`, `"accounts"`, `"keys"`}},
		{"/usage/perf?realm=intl&sample=50", 200, "application/json; charset=utf-8", []string{`"sampled"`, `"by_model_realm"`}},
		{"/usage/timeseries?realm=intl&range=today&bucket=60", 200, "application/json; charset=utf-8", []string{`"bucket_seconds"`, `"series"`, `"credits"`}},
		{"/usage/by-account", 200, "application/json; charset=utf-8", []string{`"accounts"`}},
		{"/usage/remaining", 200, "application/json; charset=utf-8", []string{`"window_seconds"`, `"rows"`}},
	}
	for _, testCase := range cases {
		recorder := perform(t, mux, testCase.path)
		if recorder.Code != testCase.status {
			t.Fatalf("%s status = %d: %s", testCase.path, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); got != testCase.contentType {
			t.Fatalf("%s content type = %q", testCase.path, got)
		}
		bodyText := recorder.Body.String()
		var body map[string]any
		decoder := json.NewDecoder(strings.NewReader(bodyText))
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil {
			t.Fatalf("%s body: %v", testCase.path, err)
		}
		for _, needle := range testCase.contains {
			if !containsText(bodyText, needle) {
				t.Fatalf("%s body missing %s: %s", testCase.path, needle, bodyText)
			}
		}
	}
}

func TestUsageRoutesGate(t *testing.T) {
	service := usageTestService(t)
	mux := http.NewServeMux()
	gate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "panel password required")
		})
	}
	NewUsageHandlers(service, gate).Register(mux)
	recorder := perform(t, mux, "/usage")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("gate not applied: %d", recorder.Code)
	}
}

func TestUsageRoutesETag(t *testing.T) {
	service := usageTestService(t)
	mux := http.NewServeMux()
	NewUsageHandlers(service, nil).Register(mux)
	first := perform(t, mux, "/usage?realm=intl")
	etag := first.Header().Get("ETag")
	if etag == "" || first.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("missing validators: %+v", first.Header())
	}
	request := httptest.NewRequest(http.MethodGet, "/usage?realm=intl", nil)
	request.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	mux.ServeHTTP(second, request)
	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 carried a body: %q", second.Body.String())
	}
	if second.Header().Get("Content-Length") != "" {
		t.Fatalf("304 carried Content-Length: %q", second.Header().Get("Content-Length"))
	}
	if second.Header().Get("ETag") != etag || second.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("304 dropped validators: %+v", second.Header())
	}
}

func containsText(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
