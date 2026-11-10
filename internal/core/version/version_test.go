package version

import (
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	if Version == "" || !strings.HasPrefix(Version, "2.") {
		t.Fatal(Version)
	}
	if NormalizedVersion() != strings.TrimPrefix(Version, "v") {
		t.Fatalf("NormalizedVersion() = %q for %q", NormalizedVersion(), Version)
	}
}

func TestCurrentAndJSON(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldVersion, oldCommit, oldDate })
	Version, Commit, Date = "v2.0.0-rc.1", "abc123", "2026-11-08T00:00:00Z"

	info := Current()
	if info.Version != "2.0.0-rc.1" || info.Commit != "abc123" || info.Date != Date {
		t.Fatalf("Current() = %#v", info)
	}
	if info.Go == "" || info.OS == "" || info.Arch == "" {
		t.Fatalf("runtime identity missing: %#v", info)
	}
	raw, err := info.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version":"2.0.0-rc.1"`, `"commit":"abc123"`, `"os":`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("JSON %s missing %s", raw, want)
		}
	}
	text := info.String()
	if !strings.Contains(text, "mix2api 2.0.0-rc.1") || !strings.Contains(text, "commit: abc123") {
		t.Fatalf("String() = %q", text)
	}
}

func TestParseDate(t *testing.T) {
	if _, ok := ParseDate("2026-11-08T00:00:00Z"); !ok {
		t.Fatal("RFC3339 rejected")
	}
	if _, ok := ParseDate("not-a-date"); ok {
		t.Fatal("invalid date accepted")
	}
	if _, ok := ParseDate(""); ok {
		t.Fatal("empty date accepted")
	}
}

func TestNormalizePlaceholders(t *testing.T) {
	if normalize(" ") != "unknown" || normalize("v1.2.3") != "1.2.3" {
		t.Fatalf("normalize() = %q %q", normalize(" "), normalize("v1.2.3"))
	}
}
