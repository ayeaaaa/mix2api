// Package secrets provides secret redaction and private file helpers.
package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"
)

const redacted = "<redacted>"

// urlUserinfoPassword matches the password in scheme://user:password@host.
var urlUserinfoPassword = regexp.MustCompile(`(://[^/@\s:]*):([^/@\s]+)@`)

// Redact replaces each non-empty value of at least four characters with
// "<redacted>" and masks passwords embedded in URL userinfo. Longer values are
// replaced first so an overlapping shorter value cannot leave a partial
// secret behind.
func Redact(text string, values ...string) string {
	ordered := append([]string(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, value := range ordered {
		if utf8.RuneCountInString(value) < 4 {
			continue
		}
		text = strings.ReplaceAll(text, value, redacted)
	}
	return urlUserinfoPassword.ReplaceAllString(text, "${1}:<redacted>@")
}

// Mask returns a fixed marker for a non-empty value without exposing any part
// of it. Mask("") returns "" because there is nothing to hide.
func Mask(value string) string {
	if value == "" {
		return ""
	}
	return redacted
}

// WritePrivate atomically writes data to path with mode 0600 on Unix systems.
// The temporary file is created in the destination directory so the rename is
// atomic. On Windows the permission change is skipped.
func WritePrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("secrets: create temp file: %w", err)
	}
	tmp := f.Name()
	remove := func() { _ = os.Remove(tmp) }

	if runtime.GOOS != "windows" {
		if err := f.Chmod(0600); err != nil {
			_ = f.Close()
			remove()
			return fmt.Errorf("secrets: chmod temp file: %w", err)
		}
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		remove()
		return fmt.Errorf("secrets: write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		remove()
		return fmt.Errorf("secrets: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return fmt.Errorf("secrets: close temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		remove()
		return fmt.Errorf("secrets: replace %s: %w", path, err)
	}

	// Persist the rename where directory fsync is supported; the file data is
	// already synced, so this is best effort.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
