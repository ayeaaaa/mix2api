package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/logs"
)

func logsTestStore(t *testing.T, enabled bool) *logs.LogStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage", "logs.sqlite3")
	store := logs.Open(path, enabled, logs.Caps{}, nil)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func logsTestHandler(t *testing.T, store *logs.LogStore, gate func(http.Handler) http.Handler) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	NewLogsHandlers(store, gate).Register(mux)
	return mux
}

func TestServerRecordsRequestLogs(t *testing.T) {
	store := logsTestStore(t, false)
	server := New(ServerOptions{LogStore: store, PanelAuth: passThroughAuth})
	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d body=%s", rec.Code, rec.Body)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["mode"] != "memory" {
		t.Fatalf("mode = %#v", payload["mode"])
	}
	logsList, _ := payload["logs"].([]any)
	if len(logsList) == 0 || payload["total"].(float64) == 0 {
		t.Fatalf("request log store is empty: %#v", payload)
	}
	entry, _ := logsList[0].(map[string]any)
	for _, key := range []string{"id", "ts", "time", "level", "tag", "msg"} {
		if _, ok := entry[key]; !ok {
			t.Fatalf("log entry missing %s: %#v", key, entry)
		}
	}
	if msg, _ := entry["msg"].(string); !strings.Contains(msg, "GET /health") {
		t.Fatalf("first log = %#v, want the /health request line", entry)
	}
}

func TestLogsRoutes(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		store := logsTestStore(t, false)
		for index := 1; index <= 2; index++ {
			if err := store.Append(logs.Entry{Level: "INFO", Tag: "tasks", Msg: "entry " + strconv.Itoa(index)}); err != nil {
				t.Fatal(err)
			}
		}
		handler := logsTestHandler(t, store, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/logs?limit=1", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Fatalf("content type = %q", got)
		}
		var payload map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if payload["mode"] != "memory" {
			t.Fatalf("mode = %#v", payload["mode"])
		}
		if payload["total"].(float64) != 2 || payload["max_id"].(float64) != 2 {
			t.Fatalf("payload = %#v", payload)
		}
		if rows := payload["logs"].([]any); len(rows) != 1 {
			t.Fatalf("rows = %#v", rows)
		}
		if _, ok := payload["stored"]; ok {
			t.Fatalf("memory mode returned stored: %#v", payload)
		}
		if _, ok := payload["db_bytes"]; ok {
			t.Fatalf("memory mode returned db_bytes: %#v", payload)
		}
	})

	t.Run("db", func(t *testing.T) {
		store := logsTestStore(t, true)
		if err := store.Append(logs.Entry{Level: "ERROR", Tag: "chat", Msg: "boom"}); err != nil {
			t.Fatal(err)
		}
		handler := logsTestHandler(t, store, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/logs?level=error&tag=CHAT&search=BOOM", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", response.Code, response.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if payload["mode"] != "db" {
			t.Fatalf("mode = %#v", payload["mode"])
		}
		if payload["total"].(float64) != 1 {
			t.Fatalf("total = %#v", payload["total"])
		}
		if payload["stored"].(float64) != 1 {
			t.Fatalf("stored = %#v", payload["stored"])
		}
		if payload["db_bytes"].(float64) <= 0 {
			t.Fatalf("db_bytes = %#v", payload["db_bytes"])
		}
	})

	t.Run("legacy database rows are redacted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "logs.sqlite3")
		seed, err := logs.NewSQLite(path)
		if err != nil {
			t.Fatalf("NewSQLite: %v", err)
		}
		now := time.Now()
		if err := seed.AppendBatch([]logs.Entry{{
			ID: 1, TS: now.Format("2006-01-02 15:04:05"), Time: now.Format("15:04:05"),
			Level: "INFO", Tag: "auth", Msg: "legacy 13812345678 callback?code=secret&state=state",
		}}); err != nil {
			_ = seed.Close()
			t.Fatalf("seed: %v", err)
		}
		if err := seed.Close(); err != nil {
			t.Fatalf("close seed: %v", err)
		}
		store := logs.Open(path, true, logs.Caps{}, nil)
		t.Cleanup(func() { _ = store.Close() })
		handler := logsTestHandler(t, store, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/logs", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", response.Code, response.Body.String())
		}
		body := response.Body.String()
		if strings.Contains(body, "13812345678") || strings.Contains(body, "code=secret") {
			t.Fatalf("legacy row leaked plaintext: %s", body)
		}
		if !strings.Contains(body, "138****5678") || !strings.Contains(body, "code=<redacted>") {
			t.Fatalf("legacy row was not redacted: %s", body)
		}
	})

	t.Run("export", func(t *testing.T) {
		store := logsTestStore(t, false)
		if err := store.Append(logs.Entry{
			TS:    "2026-11-10 12:00:00",
			Time:  "12:00:00",
			Level: "INFO",
			Tag:   "tasks",
			Msg:   "row 13812345678",
		}); err != nil {
			t.Fatal(err)
		}
		fixed := time.Date(2026, 11, 10, 12, 34, 56, 0, time.UTC)
		mux := http.NewServeMux()
		group := NewLogsHandlers(store, nil)
		group.Now = func() time.Time { return fixed }
		group.Register(mux)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/logs/export", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Fatalf("content type = %q", got)
		}
		if got := response.Header().Get("Content-Disposition"); got != `attachment; filename="wb-proxy-20261110-123456.log"` {
			t.Fatalf("content disposition = %q", got)
		}
		body := response.Body.String()
		want := "[2026-11-10 12:00:00] [INFO] [tasks] row 138****5678"
		if body != want {
			t.Fatalf("export body = %q, want %q", body, want)
		}
		if got := response.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
			t.Fatalf("content length = %q, want %d", got, len(body))
		}
	})

	t.Run("clear", func(t *testing.T) {
		store := logsTestStore(t, true)
		if err := store.Append(logs.Entry{Level: "INFO", Tag: "tasks", Msg: "one"}); err != nil {
			t.Fatal(err)
		}
		handler := logsTestHandler(t, store, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/logs/clear", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", response.Code, response.Body.String())
		}
		if strings.TrimSpace(response.Body.String()) != `{"ok":true}` {
			t.Fatalf("clear body = %q", response.Body.String())
		}
		result, err := store.Query(logs.Query{})
		if err != nil || result.Total != 0 {
			t.Fatalf("rows after clear = %#v, %v", result, err)
		}
		stats, err := store.Stats()
		if err != nil || stats.Rows != 0 {
			t.Fatalf("stats after clear = %#v, %v", stats, err)
		}
	})

	t.Run("panel only", func(t *testing.T) {
		store := logsTestStore(t, false)
		if err := store.Append(logs.Entry{Level: "INFO", Tag: "tasks", Msg: "one"}); err != nil {
			t.Fatal(err)
		}
		gate := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Panel") != "yes" {
					writeV1Error(w, http.StatusUnauthorized, "invalid_request_error", "panel password required")
					return
				}
				next.ServeHTTP(w, r)
			})
		}
		handler := logsTestHandler(t, store, gate)
		for _, request := range []*http.Request{
			httptest.NewRequest(http.MethodGet, "/logs", nil),
			httptest.NewRequest(http.MethodGet, "/logs/export", nil),
			httptest.NewRequest(http.MethodPost, "/logs/clear", nil),
		} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s status = %d, want 401", request.Method, request.URL.Path, response.Code)
			}
		}
		for _, request := range []*http.Request{
			httptest.NewRequest(http.MethodGet, "/logs", nil),
			httptest.NewRequest(http.MethodGet, "/logs/export", nil),
			httptest.NewRequest(http.MethodPost, "/logs/clear", nil),
		} {
			request.Header.Set("X-Panel", "yes")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("%s %s status = %d, want 200", request.Method, request.URL.Path, response.Code)
			}
		}
	})
}
