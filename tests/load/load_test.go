package load

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/app"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/logs"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/workbuddy"
)

const (
	loadModel  = "hy4-preview"
	loadAPIKey = "load-api-key"
	loadPanel  = "load-panel-password"
)

// TestSSELoad is the manual performance gate. It is skipped by default so the
// normal test suite stays offline and fast:
//
//	MIX2API_LOAD=1 go test ./tests/load -run TestSSELoad -v
func TestSSELoad(t *testing.T) {
	if os.Getenv("MIX2API_LOAD") != "1" {
		t.Skip("set MIX2API_LOAD=1 to run the 200-concurrent SSE load harness")
	}
	concurrency := 200
	if value := os.Getenv("MIX2API_LOAD_CONCURRENCY"); value != "" {
		if _, err := fmt.Sscan(value, &concurrency); err != nil || concurrency < 1 {
			t.Fatalf("invalid MIX2API_LOAD_CONCURRENCY=%q", value)
		}
	}
	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	usageDir := filepath.Join(root, "usage")
	fake := newLoadUpstream()
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	t.Setenv("WB_INTL_CHAT_UPSTREAM", upstream.URL)
	t.Setenv("WB_CN_CHAT_UPSTREAM", upstream.URL)
	t.Setenv("WB_BILLING_UPSTREAM", upstream.URL)
	t.Setenv("WB_TASK_WEB_UPSTREAM", upstream.URL)

	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := config.New(filepath.Join(accountsDir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"workbuddy": map[string]any{
			"intl": map[string]any{
				"enabled": true, "max_concurrent": concurrency + 32,
				"timeout_seconds": 10, "stream_timeout_seconds": 60,
			},
			"cn": map[string]any{"enabled": false},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 8; index++ {
		uid := fmt.Sprintf("load-account-%02d", index)
		auth := workbuddy.Auth{
			UID: uid, Nickname: uid, Realm: "intl", Domain: "www.workbuddy.ai",
			AccessToken:  "wb-" + uid + "-access-0123456789",
			RefreshToken: "refresh-" + uid,
			ExpiresAt:    time.Now().Add(24 * time.Hour),
			Platform:     "CLI", Product: "workbuddy", Source: "oauth",
			Enabled: true, EnabledSet: true, TasksEnabled: true, TasksEnabledSet: true,
		}
		if _, err := workbuddy.SaveAccount(accountsDir, auth); err != nil {
			t.Fatal(err)
		}
	}
	setup, err := app.Build(app.Paths{AccountsDir: accountsDir, UsageDir: usageDir})
	if err != nil {
		t.Fatal(err)
	}
	platform, err := app.BuildPlatform(setup, app.PlatformOptions{
		Paths: setup.Paths, Version: "2.0.0-load", PanelPassword: loadPanel,
		MediaDir: filepath.Join(root, "media"), UpdateDir: filepath.Join(root, "updates"),
		Home: root, Logger: func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	platform.Start(context.Background())
	if err := platform.LogStore.Configure(true, 0, 7, 0); err != nil {
		t.Fatal(err)
	}
	server := api.NewServer(api.ServerOptions{
		Version: "2.0.0-load", Providers: setup.Providers, Platform: platform.HTTPServices(),
		APIKeyAuth: api.RequireAPIKeyAuth(loadAPIKey), StaticAPIKey: loadAPIKey,
	})
	live := httptest.NewServer(server.Handler)
	defer live.Close()

	payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"load"}],"stream":true}`, loadModel)
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns: concurrency * 2, MaxIdleConnsPerHost: concurrency * 2,
		IdleConnTimeout: 90 * time.Second, DisableCompression: true,
	}}

	beforeAllocs, beforeSys, beforeG := memSnapshot()
	start := make(chan struct{})
	results := make([]requestResult, concurrency)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for index := 0; index < concurrency; index++ {
		go func(index int) {
			defer wg.Done()
			<-start
			results[index] = runSSERequest(client, live.URL+"/v1/chat/completions", payload)
		}(index)
	}
	started := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(started)
	afterAllocs, afterSys, afterG := memSnapshot()

	latencies := make([]time.Duration, 0, concurrency)
	failures := 0
	for _, result := range results {
		if result.err != nil {
			failures++
			t.Logf("request error: %v", result.err)
			continue
		}
		latencies = append(latencies, result.ttfb)
	}
	if failures != 0 {
		t.Fatalf("%d/%d requests failed", failures, concurrency)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := percentile(latencies, 0.50)
	p95 := percentile(latencies, 0.95)
	p99 := percentile(latencies, 0.99)
	cpuSeconds := readCPU()
	rows := countUsageRows(t, filepath.Join(usageDir, "usage.jsonl"))
	sqliteStarted := time.Now()
	for index := 0; index < concurrency; index++ {
		if err := platform.LogStore.Append(logs.Entry{
			TS: time.Now().Format("2006-01-02 15:04:05"), Level: "info", Tag: "LOAD",
			Msg: "load log row",
		}); err != nil {
			t.Fatalf("append log: %v", err)
		}
	}
	logResult, err := platform.LogStore.Query(logs.Query{Limit: concurrency + 10})
	if err != nil {
		t.Fatalf("query logs: %v", err)
	}
	sqliteAppend := time.Since(sqliteStarted)
	evidence := map[string]any{
		"concurrency": concurrency, "requests": concurrency, "failures": failures,
		"elapsed_ms": elapsed.Milliseconds(), "p50_first_byte_ms": p50.Milliseconds(),
		"p95_first_byte_ms": p95.Milliseconds(), "p99_first_byte_ms": p99.Milliseconds(),
		"mem_alloc_before": beforeAllocs, "mem_alloc_after": afterAllocs,
		"mem_sys_before": beforeSys, "mem_sys_after": afterSys,
		"goroutines_before": beforeG, "goroutines_after": afterG,
		"cpu_seconds": cpuSeconds, "usage_rows": rows,
		"sqlite_rows": logResult.Total, "sqlite_append_ms": sqliteAppend.Milliseconds(),
		"go_version": runtime.Version(),
	}
	raw, _ := json.Marshal(evidence)
	t.Logf("LOAD_EVIDENCE %s", raw)
	if value := os.Getenv("MIX2API_LOAD_EVIDENCE"); value != "" {
		if err := os.WriteFile(value, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if p95 > time.Duration(envInt("MIX2API_LOAD_P95_MS", 2000))*time.Millisecond {
		t.Fatalf("p95 first byte %s exceeds threshold", p95)
	}
	if rows != concurrency {
		t.Fatalf("usage rows = %d, want %d", rows, concurrency)
	}
	if logResult.Total < concurrency {
		t.Fatalf("sqlite log rows = %d, want at least %d", logResult.Total, concurrency)
	}
}

type requestResult struct {
	ttfb time.Duration
	err  error
}

func runSSERequest(client *http.Client, url, payload string) requestResult {
	started := time.Now()
	var ttfb time.Duration
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return requestResult{err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+loadAPIKey)
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		ttfb = time.Since(started)
	}}
	request = request.WithContext(httptrace.WithClientTrace(context.Background(), trace))
	response, err := client.Do(request)
	if err != nil {
		return requestResult{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return requestResult{err: err}
	}
	if response.StatusCode != http.StatusOK {
		return requestResult{err: fmt.Errorf("status %d: %s", response.StatusCode, body)}
	}
	if !containsDone(body) {
		return requestResult{err: fmt.Errorf("stream did not terminate with [DONE]")}
	}
	if ttfb == 0 {
		ttfb = time.Since(started)
	}
	return requestResult{ttfb: ttfb}
}

func containsDone(body []byte) bool {
	return bytes.Contains(body, []byte("[DONE]"))
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	index := int(float64(len(values)-1) * fraction)
	return values[index]
}

type loadUpstream struct{}

func newLoadUpstream() *loadUpstream { return &loadUpstream{} }

func (f *loadUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v2/plugin/auth/token/refresh":
		writeJSON(w, map[string]any{"data": map[string]any{
			"accessToken": "refreshed", "refreshToken": "refreshed", "expiresAt": 4102444800,
		}})
	case "/v3/config":
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
			"agents": map[string]any{"cli": map[string]any{"models": []any{loadModel}}},
			"models": []any{map[string]any{"id": loadModel, "name": loadModel}},
		}})
	case "/v2/chat/completions":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frames := []string{
			fmt.Sprintf("data: {\"id\":\"chatcmpl-load\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n", loadModel),
			"data: {\"id\":\"chatcmpl-load\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"" + loadModel + "\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n",
			"data: [DONE]\n\n",
		}
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func memSnapshot() (alloc, sys uint64, goroutines int) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.Alloc, stats.Sys, runtime.NumGoroutine()
}

func readCPU() float64 {
	samples := []metrics.Sample{{Name: "/cpu/classes/total:cpu-seconds"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindFloat64 {
		return samples[0].Value.Float64()
	}
	return 0
}

func countUsageRows(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) > 0 {
			count++
		}
	}
	return count
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	var parsed int
	if _, err := fmt.Sscan(value, &parsed); err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
