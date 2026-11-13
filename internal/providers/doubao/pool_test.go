package doubao

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPoolAddPickEnableDisableRemove(t *testing.T) {
	dir := t.TempDir()
	pool, err := NewPool(dir, Config{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	account, err := pool.Add(
		map[string]string{"sessionid": "session-1", "ttwid": "ttwid-1"},
		map[string]string{"web_id": "wid"},
		"uid-1", "tester", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if account.UID != "uid-1" {
		t.Fatalf("uid = %q", account.UID)
	}
	if _, err := os.Stat(filepath.Join(dir, accountFolder, "uid-1.json")); err != nil {
		t.Fatalf("account file: %v", err)
	}
	if err := pool.SetNote("uid-1", "keep me"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Add(map[string]string{"sessionid": "session-2"}, map[string]string{"web_id": "wid2"}, "uid-1", "renamed", true); err != nil {
		t.Fatal(err)
	}
	if got := len(pool.Accounts()); got != 1 {
		t.Fatalf("re-import created %d accounts, want 1", got)
	}
	if got := pool.account("uid-1"); got == nil || got.Note != "keep me" || got.Nickname != "renamed" {
		t.Fatalf("re-imported account = %#v", got)
	}
	loaded, err := LoadAccounts(dir)
	if err != nil || len(loaded) != 1 || loaded[0].UID != "uid-1" {
		t.Fatalf("loaded = %#v, %v", loaded, err)
	}
	picked, err := pool.Pick("doubao-fast", "s", nil)
	if err != nil || picked.UID != "uid-1" {
		t.Fatalf("pick = %#v, %v", picked, err)
	}
	if !pool.Acquire(picked) {
		t.Fatal("acquire failed")
	}
	if pool.Acquire(picked) {
		t.Fatal("second acquire exceeded max concurrency")
	}
	pool.Release(picked)
	if err := pool.Disable("uid-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Pick("doubao-fast", "s", nil); err == nil {
		t.Fatal("disabled account was picked")
	}
	if err := pool.Enable("uid-1"); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetNote("uid-1", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := pool.Remove("uid-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, accountFolder, "uid-1.json")); !os.IsNotExist(err) {
		t.Fatalf("account file still exists: %v", err)
	}
}

func TestLoadAccountsSkipsInvalid(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, accountFolder)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	accounts, err := LoadAccounts(dir)
	if err != nil || len(accounts) != 0 {
		t.Fatalf("accounts = %#v, %v", accounts, err)
	}
}
