package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildProviderInitFailureIsGeneric(t *testing.T) {
	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	raccoonDir := filepath.Join(accountsDir, "raccoon")
	if err := os.MkdirAll(raccoonDir, 0o700); err != nil {
		t.Fatalf("create raccoon dir: %v", err)
	}

	// Both credentials are valid-shaped and on disk; the provider must fail on
	// the malformed state document without echoing either token.
	accessToken := "slice-access-token-never-log"
	refreshToken := "slice-refresh-token-never-log"
	auth := fmt.Sprintf(
		`{"uid":"pu-fail","access_token":%q,"refresh_token":%q,`+
			`"office_identity":"personal","office_org_name":"Fail Org","expires_at":4102444800}`,
		accessToken, refreshToken,
	)
	if err := os.WriteFile(
		filepath.Join(raccoonDir, "raccoon-pu-fail.json"), []byte(auth), 0o600,
	); err != nil {
		t.Fatalf("write auth fixture: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(raccoonDir, "state.json"), []byte(`{"pu-fail":[]}`), 0o600,
	); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	setup, err := Build(Paths{
		AccountsDir: accountsDir,
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err == nil {
		t.Fatal("Build succeeded with a malformed state document")
	}
	if setup != nil {
		t.Fatal("Build returned a setup alongside an error")
	}
	if !strings.Contains(err.Error(), "app: initialize raccoon provider") {
		t.Fatal("startup error is not the generic provider-init failure")
	}
	for _, secret := range []string{accessToken, refreshToken} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("provider-init error leaked a credential")
		}
	}
}

func TestBuildDoesNotCreateUnusedUsageDir(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	accountsDir := filepath.Join(root, "accounts")
	usageDir := filepath.Join(blocker, "usage")
	setup, err := Build(Paths{AccountsDir: accountsDir, UsageDir: usageDir})
	if err != nil {
		t.Fatalf("Build rejected an unused invalid usage path: %v", err)
	}
	if setup.Paths.UsageDir != usageDir {
		t.Fatalf("UsageDir = %q, want retained %q", setup.Paths.UsageDir, usageDir)
	}
	if info, err := os.Stat(usageDir); err == nil {
		t.Fatalf("unused usage directory was created: isDir=%v", info.IsDir())
	}
}
