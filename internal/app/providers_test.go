package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/codearts"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/workbuddy"
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

func TestBuildRegistersOneProviderPerRealm(t *testing.T) {
	root := t.TempDir()
	setup, err := Build(Paths{
		AccountsDir: filepath.Join(root, "accounts"),
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(setup.Providers) != 4 {
		t.Fatalf("providers = %d, want raccoon, codearts, and workbuddy intl/cn", len(setup.Providers))
	}
	counts := make(map[string]int, len(setup.Providers))
	for _, provider := range setup.Providers {
		counts[provider.ID()]++
	}
	wanted := map[string]string{
		"raccoon":        "raccoon",
		"codearts":       "codearts",
		"workbuddy-intl": "intl",
		"workbuddy-cn":   "cn",
	}
	for id, realm := range wanted {
		if counts[id] != 1 {
			t.Fatalf("provider ID %q count = %d, want exactly one Provider/Pool", id, counts[id])
		}
		provider, ok := setup.Registry.Get(realm)
		if !ok || provider == nil || provider.ID() != id || provider.Realm() != realm {
			t.Fatalf("registry realm %q missing or wrong: %v", realm, provider)
		}
	}
}

func TestBuildCodeArtsInitFailureIsGeneric(t *testing.T) {
	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	codeartsDir := filepath.Join(accountsDir, "codearts")
	if err := os.MkdirAll(codeartsDir, 0o700); err != nil {
		t.Fatalf("create codearts dir: %v", err)
	}

	// A malformed state document fails the pool load. The auth document on
	// disk must not be echoed by the startup error.
	accessToken := "codearts-slice-token-never-log"
	secretKey := "codearts-slice-secret-never-log"
	auth := fmt.Sprintf(
		`{"user_id":"slice-user","cloud_dragon_token":%q,`+
			`"access_key_id":"AK-SLICE","secret_access_key":%q,"proxySlot":""}`,
		accessToken, secretKey,
	)
	if err := os.WriteFile(
		filepath.Join(codeartsDir, "codearts-slice-user.json"), []byte(auth), 0o600,
	); err != nil {
		t.Fatalf("write auth fixture: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(codeartsDir, "state.json"), []byte(`{"accounts": "not-a-list"}`), 0o600,
	); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}

	setup, err := Build(Paths{
		AccountsDir: accountsDir,
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err == nil {
		t.Fatal("Build succeeded with a malformed codearts state document")
	}
	if setup != nil {
		t.Fatal("Build returned a setup alongside an error")
	}
	if !strings.Contains(err.Error(), "app: initialize codearts provider") {
		t.Fatal("startup error is not the generic codearts provider-init failure")
	}
	for _, secret := range []string{accessToken, secretKey} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("codearts provider-init error leaked a credential")
		}
	}
}

func TestCodeArtsProxySlotsSnapshot(t *testing.T) {
	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatalf("create accounts dir: %v", err)
	}
	store := config.New(filepath.Join(accountsDir, "settings.json"))
	provider, err := codearts.New(store)
	if err != nil {
		t.Fatalf("codearts.New: %v", err)
	}
	if _, err := provider.Pool().Add(codearts.Auth{
		UserID:           "slot-user",
		UserName:         "Slot User",
		CloudDragonToken: "slot-token-never-log",
		ProxySlot:        "slot-codearts",
	}); err != nil {
		t.Fatalf("add codearts account: %v", err)
	}
	taken := codeartsProxySlots(provider)
	slots := taken()
	if len(slots) != 1 || slots[0] != "slot-codearts" {
		t.Fatalf("taken slots = %#v, want [slot-codearts]", slots)
	}
	if nilSlots := codeartsProxySlots(nil)(); nilSlots != nil {
		t.Fatalf("nil provider slots = %#v", nilSlots)
	}
}

func TestBuildSharesWorkBuddyActivityPath(t *testing.T) {
	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	setup, err := Build(Paths{AccountsDir: accountsDir, UsageDir: filepath.Join(root, "usage")})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := filepath.Join(accountsDir, workbuddy.ActivityHistoryFile)
	for _, realm := range []string{"intl", "cn"} {
		provider, ok := setup.Registry.Get(realm)
		if !ok {
			t.Fatalf("registry has no %q provider", realm)
		}
		wb, ok := provider.(*workbuddy.Provider)
		if !ok {
			t.Fatalf("registry[%q] = %T, want *workbuddy.Provider", realm, provider)
		}
		if got := wb.ActivityHistoryPath(); got != want {
			t.Fatalf("registry[%q] activity path = %q, want %q", realm, got, want)
		}
	}
}

func TestBuildWiresCodeArtsSlotsIntoWorkBuddyLogin(t *testing.T) {
	root := t.TempDir()
	setup, err := Build(Paths{
		AccountsDir: filepath.Join(root, "accounts"),
		UsageDir:    filepath.Join(root, "usage"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	provider, ok := setup.Registry.Get("codearts")
	if !ok {
		t.Fatal("registry has no codearts provider")
	}
	codeartsProvider, ok := provider.(*codearts.Provider)
	if !ok {
		t.Fatalf("codearts provider = %T", provider)
	}
	if _, err := codeartsProvider.Pool().Add(codearts.Auth{
		UserID:           "login-slot-user",
		UserName:         "Login Slot User",
		CloudDragonToken: "login-slot-token-never-log",
		ProxySlot:        "slot-login",
	}); err != nil {
		t.Fatalf("add codearts account: %v", err)
	}

	for _, realm := range []string{"intl", "cn"} {
		provider, ok := setup.Registry.Get(realm)
		if !ok {
			t.Fatalf("registry has no %q provider", realm)
		}
		wb, ok := provider.(*workbuddy.Provider)
		if !ok {
			t.Fatalf("registry[%q] = %T", realm, provider)
		}
		slots := wb.LoginTakenSlots()
		if len(slots) != 1 || slots[0] != "slot-login" {
			t.Fatalf("registry[%q] login taken slots = %#v, want [slot-login]", realm, slots)
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
