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
	t.Setenv("WB_CODEARTS_SNAP_HOST", upstream.URL)
	t.Setenv("WB_CODEARTS_SNAP_MANAGER", upstream.URL+"/snap-manager")
	t.Setenv("WB_CODEARTS_BENEFIT_HOST", upstream.URL)
	t.Setenv("WB_CODEARTS_STS_HOST", upstream.URL)
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
	if len(setup.Providers) != 2 {
		t.Fatalf("providers = %d, want raccoon and codearts", len(setup.Providers))
	}
	wantedRealms := map[string]string{
		"raccoon":  "raccoon",
		"codearts": "codearts",
	}
	counts := make(map[string]int, len(setup.Providers))
	for _, provider := range setup.Providers {
		counts[provider.ID()]++
		if want := wantedRealms[provider.ID()]; provider.Realm() != want {
			t.Fatalf("provider %q realm = %q, want %q", provider.ID(), provider.Realm(), want)
		}
	}
	for id := range wantedRealms {
		if counts[id] != 1 {
			t.Fatalf("provider ID %q count = %d, want 1", id, counts[id])
		}
		provider, ok := setup.Registry.Get(id)
		if !ok || provider == nil {
			t.Fatalf("registry has no %q realm", id)
		}
		if provider.ID() != id {
			t.Fatalf("registry[%q].ID() = %q", id, provider.ID())
		}
	}
}
