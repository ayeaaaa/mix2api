package load

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	v1BaselineVersion = "1.6.5"
	v1BaselineCommit  = "e40c4e8994f9cc6ace3457e350466cdd5d422a37"
)

// TestV1Baseline is the manual v1.6.5 half of the Gate F performance
// comparison. It starts the ignored v1.6.5 copy and the shared oracle fake
// upstream, seeds synthetic WorkBuddy accounts, and runs exactly the same
// 200-concurrent streaming chat workload as TestSSELoad.
//
// It is skipped by default and must run inside a private network namespace so
// the v1 gateway cannot reach a real provider:
//
//	MIX2API_LOAD_V1=1 MIX2API_LOAD_NETNS=1 \
//	  go test ./tests/load -run TestV1Baseline -v -count=1 -timeout 5m
//
// tests/load/run-v1-baseline.sh wraps that command with `unshare -n`.
func TestV1Baseline(t *testing.T) {
	if os.Getenv("MIX2API_LOAD_V1") != "1" {
		t.Skip("set MIX2API_LOAD_V1=1 to run the v1.6.5 baseline harness")
	}
	if os.Getenv("MIX2API_LOAD_NETNS") != "1" {
		t.Skip("run tests/load/run-v1-baseline.sh so the gateway is isolated from the real network")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the v1 baseline reads process RSS/CPU from /proc")
	}
	root := loadRepoRoot(t)
	v1Tree := filepath.Join(root, "tests", "oracle", "v1.6.5")
	proxy := filepath.Join(v1Tree, "app", "wb_proxy.py")
	if _, err := os.Stat(proxy); err != nil {
		t.Skipf("ignored v1.6.5 copy is missing: %v", err)
	}
	fakeScript := filepath.Join(root, "tests", "oracle", "fakes", "fake_upstream.py")
	if _, err := os.Stat(fakeScript); err != nil {
		t.Fatalf("oracle fake upstream is missing: %v", err)
	}
	concurrency := envInt("MIX2API_LOAD_CONCURRENCY", 200)
	work := t.TempDir()
	accountsDir := filepath.Join(work, "accounts")
	usageDir := filepath.Join(work, "usage")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(usageDir, 0o700); err != nil {
		t.Fatal(err)
	}

	infoPath := filepath.Join(work, "fake.json")
	fake := exec.Command("python3", fakeScript, "--info", infoPath)
	fake.Dir = root
	startBaselineProcess(t, "fake upstream", fake, filepath.Join(work, "fake.log"))
	waitForBaselineFile(t, infoPath, 20*time.Second)
	rawInfo, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Workbuddy string `json:"workbuddy"`
	}
	if err := json.Unmarshal(rawInfo, &info); err != nil {
		t.Fatal(err)
	}
	if err := requireLoopbackURL(info.Workbuddy); err != nil {
		t.Fatalf("fake WorkBuddy upstream: %v", err)
	}

	seedV1Accounts(t, accountsDir)
	port := freeLoopbackPort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	gateway := exec.Command("python3", proxy,
		"--port", strconv.Itoa(port),
		"--accounts-dir", accountsDir,
		"--usage-dir", usageDir,
		"--api-key", loadAPIKey,
		"--panel-password", loadPanel,
	)
	gateway.Dir = root
	gateway.Env = append(os.Environ(),
		"WB_INTL_CHAT_UPSTREAM="+info.Workbuddy,
		"WB_CN_CHAT_UPSTREAM="+info.Workbuddy,
		"WB_BILLING_UPSTREAM="+info.Workbuddy,
		"WB_TASK_WEB_UPSTREAM="+info.Workbuddy,
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
		// Match the v2 load profile: one chat slot per configured concurrent
		// request instead of v1's default 32.
		"WB_MAX_CONCURRENT_CHAT="+strconv.Itoa(concurrency+32),
		"WB_CHAT_SLOT_WAIT=30",
	)
	startBaselineProcess(t, "v1 gateway", gateway, filepath.Join(work, "gateway.log"))
	waitForV1Health(t, base, 8, 30*time.Second)

	payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"load"}],"stream":true}`, loadModel)
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns: concurrency * 2, MaxIdleConnsPerHost: concurrency * 2,
		IdleConnTimeout: 90 * time.Second, DisableCompression: true,
	}}

	pid := gateway.Process.Pid
	rssBefore := rssKB(pid)
	cpuBefore := procCPU(pid)
	sampler := startRSSSampler(pid)
	start := make(chan struct{})
	results := make([]requestResult, concurrency)
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for index := 0; index < concurrency; index++ {
		go func(index int) {
			defer wg.Done()
			<-start
			results[index] = runSSERequest(client, base+"/v1/chat/completions", payload)
		}(index)
	}
	started := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(started)
	rssPeak := sampler.Stop()
	rssAfter := rssKB(pid)
	cpuAfter := procCPU(pid)

	latencies := make([]time.Duration, 0, concurrency)
	failures := 0
	done := 0
	for _, result := range results {
		if result.err != nil {
			failures++
			t.Logf("request error: %v", result.err)
			continue
		}
		latencies = append(latencies, result.ttfb)
		if result.done {
			done++
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := percentile(latencies, 0.50)
	p95 := percentile(latencies, 0.95)
	p99 := percentile(latencies, 0.99)
	rows := waitForUsageRows(filepath.Join(usageDir, "usage.jsonl"), concurrency, 5*time.Second)

	evidence := map[string]any{
		"target": "v1", "target_version": v1BaselineVersion, "target_commit": v1BaselineCommit,
		"runtime": "python", "python_version": pythonVersion(t),
		"network": "loopback-only", "fake_upstream": true,
		"concurrency": concurrency, "requests": concurrency, "failures": failures,
		"done": done, "done_failures": concurrency - done,
		"elapsed_ms": elapsed.Milliseconds(), "p50_first_byte_ms": p50.Milliseconds(),
		"p95_first_byte_ms": p95.Milliseconds(), "p99_first_byte_ms": p99.Milliseconds(),
		"rss_before_kb": rssBefore, "rss_peak_kb": rssPeak, "rss_after_kb": rssAfter,
		"cpu_seconds": cpuAfter - cpuBefore,
		"usage_rows":  rows, "go_version": runtime.Version(),
		"recorded_at": time.Now().UTC().Format(time.RFC3339),
	}
	writeLoadEvidence(t, evidence)
	if failures != 0 {
		t.Fatalf("%d/%d requests failed", failures, concurrency)
	}
	if rows != concurrency {
		t.Fatalf("usage rows = %d, want %d", rows, concurrency)
	}
}

func loadRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate load test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s: %v", root, err)
	}
	return root
}

func startBaselineProcess(t *testing.T, name string, cmd *exec.Cmd, logPath string) {
	t.Helper()
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = logFile.Close()
	})
}

func waitForBaselineFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func waitForV1Health(t *testing.T, base string, wantAccounts int, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		response, err := client.Get(base + "/health")
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK {
				var payload map[string]any
				if json.Unmarshal(body, &payload) == nil {
					if ready, ok := payload["accounts_ready"].(float64); ok && int(ready) >= wantAccounts {
						return
					}
					last = strings.TrimSpace(string(body))
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("v1 health not ready: %s", last)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func seedV1Accounts(t *testing.T, dir string) {
	t.Helper()
	settings := map[string]any{"api_key": loadAPIKey, "api_key_set": true}
	writeBaselineJSON(t, filepath.Join(dir, "settings.json"), settings, 0o600)
	for index := 0; index < 8; index++ {
		uid := fmt.Sprintf("load-account-%02d", index)
		document := map[string]any{
			"uid": uid, "nickname": uid, "realm": "intl", "domain": "www.workbuddy.ai",
			"accessToken":  "wb-" + uid + "-access-0123456789",
			"refreshToken": "refresh-" + uid,
			"expiresAt":    time.Now().Add(24 * time.Hour).Unix(),
			"platform":     "CLI", "product": "workbuddy", "source": "oauth",
			"enabled": true, "tasksEnabled": true,
		}
		writeBaselineJSON(t, filepath.Join(dir, uid+".json"), document, 0o600)
	}
}

func writeBaselineJSON(t *testing.T, path string, value any, mode os.FileMode) {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), mode); err != nil {
		t.Fatal(err)
	}
}

func waitForUsageRows(path string, want int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	last := 0
	for {
		count, err := countJSONLines(path)
		if err == nil {
			last = count
			if count >= want {
				return count
			}
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pythonVersion(t *testing.T) string {
	t.Helper()
	raw, err := exec.Command("python3", "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(raw))
}
