package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corerelease "github.com/ayeaaaa/mix2api-v2/internal/core/release"
)

func TestDefaultTargetsCoverRequiredMatrix(t *testing.T) {
	required := map[string]bool{
		"linux-amd64": false, "linux-arm64": false, "linux-armv7": false,
		"windows-amd64": false, "darwin-amd64": false, "darwin-arm64": false,
		"openwrt-amd64": false, "openwrt-arm64": false, "openwrt-armv7": false,
		"portable-linux-amd64": false, "portable-windows-amd64": false,
	}
	names := map[string]bool{}
	for _, item := range defaultTargets() {
		if _, ok := names[item.Name]; ok {
			t.Fatalf("duplicate target %s", item.Name)
		}
		names[item.Name] = true
		if _, ok := required[item.Name]; ok {
			required[item.Name] = true
		}
		if item.GOOS == "windows" && item.archiveFormat() != "zip" {
			t.Fatalf("%s format = %s", item.Name, item.archiveFormat())
		}
		if item.GOOS != "windows" && item.archiveFormat() != "tar.gz" {
			t.Fatalf("%s format = %s", item.Name, item.archiveFormat())
		}
	}
	for name, seen := range required {
		if !seen {
			t.Fatalf("missing required target %s", name)
		}
	}
}

func TestSelectTargetsRejectsUnknown(t *testing.T) {
	selected, err := selectTargets([]string{"linux-amd64", "windows-amd64"})
	if err != nil || len(selected) != 2 {
		t.Fatalf("selectTargets = %#v %v", selected, err)
	}
	if _, err := selectTargets([]string{"plan9-amd64"}); err == nil {
		t.Fatal("unknown target accepted")
	}
}

func TestLDFlagsCarryInjectedIdentity(t *testing.T) {
	value := ldflags("2.0.0-rc.1", "abc123", "2026-11-08T00:00:00Z")
	for _, want := range []string{
		"-buildid=",
		"version.Version=2.0.0-rc.1",
		"version.Commit=abc123",
		"version.Date=2026-11-08T00:00:00Z",
	} {
		if !strings.Contains(value, want) {
			t.Fatalf("ldflags %q missing %q", value, want)
		}
	}
	if strings.Contains(value, "-trimpath") {
		t.Fatal("trimpath belongs on the go build command, not ldflags")
	}
}

func TestWriteArchiveIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "mix2api")
	if err := os.WriteFile(binary, []byte("fake-binary-contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	license := filepath.Join(dir, "LICENSE")
	if err := os.WriteFile(license, []byte("MIT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []archiveEntry{
		{Name: "mix2api", Source: binary, Mode: 0o755},
		{Name: "LICENSE", Source: license, Mode: 0o644},
	}
	date := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, format := range []string{"zip", "tar.gz"} {
		first := filepath.Join(dir, "first."+format)
		second := filepath.Join(dir, "second."+format)
		if err := writeArchive(first, format, "mix2api-2.0.0-linux-amd64", entries, date); err != nil {
			t.Fatalf("%s first: %v", format, err)
		}
		if err := writeArchive(second, format, "mix2api-2.0.0-linux-amd64", entries, date); err != nil {
			t.Fatalf("%s second: %v", format, err)
		}
		firstRaw, _ := os.ReadFile(first)
		secondRaw, _ := os.ReadFile(second)
		if !bytes.Equal(firstRaw, secondRaw) {
			t.Fatalf("%s archives differ for identical inputs", format)
		}
	}
}

func TestWriteChecksumsSorted(t *testing.T) {
	path := filepath.Join(t.TempDir(), corerelease.ChecksumsName)
	sums := map[string]string{
		"z.zip":    strings.Repeat("b", 64),
		"a.tar.gz": strings.Repeat("a", 64),
	}
	if err := writeChecksums(path, sums); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "a.tar.gz") || !strings.HasSuffix(lines[1], "z.zip") {
		t.Fatalf("checksums = %q", raw)
	}
	parsed, err := corerelease.ParseChecksums(raw)
	if err != nil || parsed["a.tar.gz"] != sums["a.tar.gz"] {
		t.Fatalf("round trip = %#v %v", parsed, err)
	}
}

func TestBuildDateFromSourceEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1762560000")
	if got := defaultBuildDate(); got != "2025-11-08T00:00:00Z" {
		t.Fatalf("defaultBuildDate = %q", got)
	}
}

func TestArchiveDateFromSourceEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1762560000")
	got, err := defaultArchiveDate()
	if err != nil || got != "2025-11-08T00:00:00Z" {
		t.Fatalf("defaultArchiveDate = %q err=%v", got, err)
	}
	t.Setenv("SOURCE_DATE_EPOCH", "")
	got, err = defaultArchiveDate()
	if err != nil || got != "1980-01-01T00:00:00Z" {
		t.Fatalf("defaultArchiveDate without epoch = %q err=%v", got, err)
	}
	t.Setenv("SOURCE_DATE_EPOCH", "not-a-number")
	if _, err := defaultArchiveDate(); err == nil {
		t.Fatal("invalid SOURCE_DATE_EPOCH accepted")
	}

	// --archive-date still wins over the environment for reproducibility
	// checks that keep BUILD_DATE independent.
	t.Setenv("SOURCE_DATE_EPOCH", "1762560000")
	opts, err := parseOptions([]string{"--repo-root", t.TempDir(), "--version", "2.0.0", "--archive-date", "1980-01-01T00:00:00Z"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := opts.resolve(io.Discard); err != nil {
		t.Fatal(err)
	}
	if opts.SourceDate != "1980-01-01T00:00:00Z" {
		t.Fatalf("explicit archive date overridden: %q", opts.SourceDate)
	}
}

func TestSummaryShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release.json")
	sums := map[string]string{"asset.tar.gz": strings.Repeat("a", 64)}
	opts := options{Version: "2.0.0-rc.1", Commit: "abc", Date: "2026-11-08T00:00:00Z"}
	if err := writeSummary(path, opts, sums); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{`"tag": "v2.0.0-rc.1"`, `"asset.tar.gz"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("summary missing %q: %s", want, raw)
		}
	}
}

func TestChecksumsCoverSBOMAndReleaseJSON(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	buildDir := filepath.Join(out, ".build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "mix2api-linux-amd64"), []byte("fake-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := options{
		Out: out, RepoRoot: repoRoot, Version: "2.0.0-rc.1", Commit: "abc123",
		Date: "2026-11-08T00:00:00Z", SourceDate: "1980-01-01T00:00:00Z", SkipBuild: true,
	}
	targets := []target{{Name: "linux-amd64", GOOS: "linux", GOARCH: "amd64", Kind: corerelease.KindStandard}}
	if _, err := buildRelease(opts, targets, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, corerelease.ChecksumsName))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := corerelease.ParseChecksums(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sbom.spdx.json", "release.json", "mix2api-2.0.0-rc.1-linux-amd64.tar.gz"} {
		if sums[name] == "" {
			t.Fatalf("checksum missing %s: %s", name, raw)
		}
	}
	if _, ok := sums[corerelease.ChecksumsName]; ok {
		t.Fatal("checksum file must not checksum itself")
	}
	sbomHash, _, err := corerelease.FileSHA256(filepath.Join(out, "sbom.spdx.json"))
	if err != nil || sums["sbom.spdx.json"] != sbomHash {
		t.Fatalf("sbom checksum = %q want %q err=%v", sums["sbom.spdx.json"], sbomHash, err)
	}
}
