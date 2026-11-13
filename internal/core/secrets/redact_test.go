package secrets

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRedactKnownSecrets(t *testing.T) {
	got := Redact("Authorization: Bearer abcdefgh refresh=zzzzzzzz", "abcdefgh", "zzzzzzzz")
	if strings.Contains(got, "abcdefgh") || strings.Contains(got, "zzzzzzzz") {
		t.Fatal(got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatal(got)
	}
}

func TestMask(t *testing.T) {
	if Mask("abcdefgh") == "abcdefgh" {
		t.Fatal("not masked")
	}
}

func TestMaskNeverLeaksValue(t *testing.T) {
	for _, value := range []string{"abcd", "abcdefgh", strings.Repeat("x", 64)} {
		if got := Mask(value); got != "<redacted>" {
			t.Fatalf("Mask(%q) = %q, want fixed marker", value, got)
		}
	}
	if got := Mask(""); got != "" {
		t.Fatalf("Mask(\"\") = %q, want empty", got)
	}
}

func TestRedactIgnoresEmptyAndShortValues(t *testing.T) {
	got := Redact("a=abcd b=abc c=", "", "abc", "abcd")
	if got != "a=<redacted> b=abc c=" {
		t.Fatalf("Redact = %q", got)
	}
	if got := Redact("plain text", "", "abc"); got != "plain text" {
		t.Fatalf("short or empty value changed text: %q", got)
	}
}

func TestRedactLongestValueFirst(t *testing.T) {
	// Replacing the shorter value first would leave a partial secret behind.
	got := Redact("token=abcdefgh", "defgh", "abcdefgh")
	if got != "token=<redacted>" {
		t.Fatalf("Redact = %q", got)
	}
}

func TestRedactURLUserinfoPassword(t *testing.T) {
	got := Redact("connect to https://alice:hunter2@example.com/path")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("password leaked: %q", got)
	}
	want := "connect to https://alice:<redacted>@example.com/path"
	if got != want {
		t.Fatalf("Redact = %q, want %q", got, want)
	}
}

func TestRedactURLUserinfoShortPassword(t *testing.T) {
	got := Redact("https://bob:pw@example.com")
	if strings.Contains(got, "pw") {
		t.Fatalf("password leaked: %q", got)
	}
	if got != "https://bob:<redacted>@example.com" {
		t.Fatalf("Redact = %q", got)
	}
}

func TestRedactLeavesURLWithoutUserinfoAlone(t *testing.T) {
	const text = "https://example.com:8443/path?q=1"
	if got := Redact(text); got != text {
		t.Fatalf("Redact = %q, want unchanged", got)
	}
}

func TestWritePrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")
	data := []byte(`{"token":"s3cr3t"}`)

	if err := WritePrivate(path, data); err != nil {
		t.Fatalf("WritePrivate: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content = %q, want %q", got, data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("mode = %o, want 0600", perm)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret.json" {
		t.Fatalf("leftover files: %v", entryNames(entries))
	}
}

func TestWritePrivateOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")
	if err := os.WriteFile(path, []byte("old contents that are longer"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := WritePrivate(path, []byte("new")); err != nil {
		t.Fatalf("WritePrivate: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("mode = %o, want 0600", perm)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret.json" {
		t.Fatalf("leftover files: %v", entryNames(entries))
	}
}

func TestWritePrivateCleansUpTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target-dir")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if err := WritePrivate(target, []byte("secret")); err == nil {
		t.Fatal("WritePrivate over a directory unexpectedly succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "target-dir" {
		t.Fatalf("leftover files after failure: %v", entryNames(entries))
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}
