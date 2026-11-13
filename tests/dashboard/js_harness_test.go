package dashboard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/web"
)

// TestDashboardJSHarness runs the selected v1 dashboard suites under Node
// against the exact bytes embedded in the binary. Node is a development/CI
// prerequisite only: a normal `go test` skips cleanly when it is missing, and
// the production server never launches it.
func TestDashboardJSHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; dashboard JS harness is CI-only")
	}
	root := repositoryRoot(t)
	runner := filepath.Join(root, "tests", "dashboard", "node", "run.js")
	if _, err := os.Stat(runner); err != nil {
		t.Fatalf("JS harness missing: %v", err)
	}

	path := filepath.Join(t.TempDir(), "dashboard.html")
	if err := os.WriteFile(path, web.DashboardHTML(), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := web.BodyHash(mustReadFile(t, path)), web.DashboardSHA256; got != want {
		t.Fatalf("harness input hash = %s, want embedded %s", got, want)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, runner)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"DASHBOARD_HTML="+path,
		"NO_COLOR=1",
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("dashboard JS harness timed out: %s", output)
	}
	if err != nil {
		t.Fatalf("dashboard JS harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "dashboard JS suites passed: 13/13") {
		t.Fatalf("JS harness did not report all suites:\n%s", output)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
