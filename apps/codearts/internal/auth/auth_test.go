package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteRemovesCredentialFile(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{UserID: "u1", CloudDragonTok: "token"}
	if err := SaveNew(dir, a); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, a.FileName())
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("credential file missing before delete: %v", err)
	}
	if err := Delete(dir, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credential file still exists: %v", err)
	}
}

func TestDeleteMissingFileIsNotAnError(t *testing.T) {
	if err := Delete(t.TempDir(), "nobody"); err != nil {
		t.Fatalf("deleting a missing credential should be a no-op: %v", err)
	}
}

func TestDeleteRejectsEmptyDir(t *testing.T) {
	if err := Delete("", "u1"); err == nil {
		t.Fatal("empty dir should be rejected")
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	if Exists(dir, "u1") {
		t.Fatal("exists before save")
	}
	if err := SaveNew(dir, &Auth{UserID: "u1", CloudDragonTok: "token"}); err != nil {
		t.Fatal(err)
	}
	if !Exists(dir, "u1") {
		t.Fatal("missing after save")
	}
}
