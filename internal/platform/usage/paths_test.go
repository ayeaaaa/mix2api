package usage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePathsPrecedence(t *testing.T) {
	root := t.TempDir()
	explicit := filepath.Join(root, "explicit")
	usageEnv := filepath.Join(root, "usage-env")
	wbEnv := filepath.Join(root, "wb-env")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	cases := []struct {
		name      string
		usageDir  string
		usageEnv  string
		wbEnv     string
		wantDir   string
		wantUsage string
	}{
		{
			name:      "explicit argument wins",
			usageDir:  explicit,
			usageEnv:  usageEnv,
			wbEnv:     wbEnv,
			wantDir:   explicit,
			wantUsage: filepath.Join(explicit, "usage.jsonl"),
		},
		{
			name:      "USAGE_DIR beats WB_PROXY_USAGE_DIR",
			usageEnv:  usageEnv,
			wbEnv:     wbEnv,
			wantDir:   usageEnv,
			wantUsage: filepath.Join(usageEnv, "usage.jsonl"),
		},
		{
			name:      "WB_PROXY_USAGE_DIR is the legacy fallback",
			wbEnv:     wbEnv,
			wantDir:   wbEnv,
			wantUsage: filepath.Join(wbEnv, "usage.jsonl"),
		},
		{
			name:      "defaults to working directory usage folder",
			wantDir:   filepath.Join(cwd, "usage"),
			wantUsage: filepath.Join(cwd, "usage", "usage.jsonl"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("USAGE_DIR", tc.usageEnv)
			t.Setenv("WB_PROXY_USAGE_DIR", tc.wbEnv)

			paths, err := ResolvePaths(filepath.Join(root, "accounts"), tc.usageDir)
			if err != nil {
				t.Fatalf("ResolvePaths: %v", err)
			}
			if paths.Dir != tc.wantDir {
				t.Fatalf("Dir = %q, want %q", paths.Dir, tc.wantDir)
			}
			if paths.Log != tc.wantUsage {
				t.Fatalf("Log = %q, want %q", paths.Log, tc.wantUsage)
			}
			if paths.LimitEvents != filepath.Join(tc.wantDir, "limit-events.jsonl") {
				t.Fatalf("LimitEvents = %q", paths.LimitEvents)
			}
			if paths.EstimateDaily != filepath.Join(tc.wantDir, "estimate-daily.json") {
				t.Fatalf("EstimateDaily = %q", paths.EstimateDaily)
			}
			if paths.RecentIndex != filepath.Join(tc.wantDir, "usage-recent-index.json") {
				t.Fatalf("RecentIndex = %q", paths.RecentIndex)
			}
		})
	}
}

func TestResolvePathsCleansDirectoryAndLeavesAccountsAlone(t *testing.T) {
	t.Setenv("USAGE_DIR", "")
	t.Setenv("WB_PROXY_USAGE_DIR", "")

	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	usageDir := filepath.Join(root, "nested", "..", "usage")

	paths, err := ResolvePaths(accountsDir, usageDir)
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if want := filepath.Join(root, "usage"); paths.Dir != want {
		t.Fatalf("Dir = %q, want cleaned %q", paths.Dir, want)
	}
	if _, err := os.Stat(accountsDir); !os.IsNotExist(err) {
		t.Fatalf("accounts dir was touched: %v", err)
	}
}

func TestEnsureDirCreatesUsageDirectoryWithV1Permissions(t *testing.T) {
	t.Setenv("USAGE_DIR", "")
	t.Setenv("WB_PROXY_USAGE_DIR", "")

	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, "accounts"), filepath.Join(root, "usage"))
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if err := paths.EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	info, err := os.Stat(paths.Dir)
	if err != nil {
		t.Fatalf("Stat usage dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("usage dir mode = %04o, want 0755", got)
	}
	if _, err := os.Stat(filepath.Join(root, "accounts")); !os.IsNotExist(err) {
		t.Fatalf("accounts dir was created: %v", err)
	}
}
