package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateCommandDryRunAndApply(t *testing.T) {
	root := t.TempDir()
	accounts := filepath.Join(root, "accounts")
	if err := os.MkdirAll(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accounts, "settings.json"), []byte(`{"unknown":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "accounts-v2")
	var out, errOut strings.Builder
	if code := runMigrateCommand([]string{"--from", accounts, "--to", dest, "--dry-run", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("dry-run exit=%d stderr=%s", code, errOut.String())
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &summary); err != nil {
		t.Fatalf("json = %q: %v", out.String(), err)
	}
	if summary["dry_run"] != true {
		t.Fatalf("summary = %#v", summary)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dry run created destination: %v", err)
	}

	out.Reset()
	if code := runMigrateCommand([]string{"--from", accounts, "--to", dest}, &out, &errOut); code != 0 {
		t.Fatalf("migrate exit=%d stderr=%s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dest, "settings.json")); err != nil {
		t.Fatalf("destination missing settings: %v", err)
	}
	if _, err := os.Stat(dest + ".migration.json"); err != nil {
		t.Fatalf("receipt missing: %v", err)
	}
}

func TestCopyCommandRequiresPaths(t *testing.T) {
	var out, errOut strings.Builder
	if code := runRollbackCommand([]string{"--from", "x"}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "--from and --to are required") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}
