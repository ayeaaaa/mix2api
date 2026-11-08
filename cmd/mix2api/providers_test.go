package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestSetupProvidersFromEnv exercises the command's own wiring hook with the
// real env names. The fake upstream is never contacted during construction.
func TestSetupProvidersFromEnv(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)

	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	usageDir := filepath.Join(root, "usage")
	t.Setenv("WB_RACCOON_BASE_URL", upstream.URL)
	t.Setenv("ACCOUNTS_DIR", accountsDir)
	t.Setenv("USAGE_DIR", usageDir)

	setup, err := setupProviders()
	if err != nil {
		t.Fatalf("setupProviders: %v", err)
	}
	if got, want := setup.Store.Path, filepath.Join(accountsDir, "settings.json"); got != want {
		t.Fatalf("store path = %q, want %q", got, want)
	}
	if setup.Paths.AccountsDir != accountsDir || setup.Paths.UsageDir != usageDir {
		t.Fatalf("paths = %#v", setup.Paths)
	}
	if len(setup.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(setup.Providers))
	}
	if got := setup.Providers[0].ID(); got != "raccoon" {
		t.Fatalf("provider ID = %q, want raccoon", got)
	}
	if _, ok := setup.Registry.Get("raccoon"); !ok {
		t.Fatal("registry has no raccoon realm")
	}
}
