package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionsAndAssetNames(t *testing.T) {
	for _, version := range []string{"2.0.0", "2.0.0-rc.1", "10.20.30"} {
		if err := ValidateVersion(version); err != nil {
			t.Fatalf("%s rejected: %v", version, err)
		}
	}
	for _, version := range []string{"v2.0.0", "2.0", "2.0.0+build", "latest", ""} {
		if err := ValidateVersion(version); err == nil {
			t.Fatalf("%q accepted", version)
		}
	}
	if Tag("2.0.0-rc.1") != "v2.0.0-rc.1" {
		t.Fatal(Tag("2.0.0-rc.1"))
	}
	tests := []struct {
		kind, goos, goarch, goarm, want string
	}{
		{KindStandard, "linux", "amd64", "", "mix2api-2.0.0-linux-amd64.tar.gz"},
		{KindStandard, "linux", "arm", "7", "mix2api-2.0.0-linux-armv7.tar.gz"},
		{KindStandard, "windows", "amd64", "", "mix2api-2.0.0-windows-amd64.zip"},
		{KindOpenWrt, "linux", "arm64", "", "mix2api-2.0.0-openwrt-arm64.tar.gz"},
		{KindPortable, "windows", "amd64", "", "mix2api-2.0.0-portable-windows-amd64.zip"},
	}
	for _, test := range tests {
		if got := AssetName("2.0.0", test.kind, test.goos, test.goarch, test.goarm); got != test.want {
			t.Fatalf("AssetName(%s) = %q, want %q", test.kind, got, test.want)
		}
		if root := ArchiveRoot("2.0.0", test.kind, test.goos, test.goarch, test.goarm); strings.HasSuffix(root, ".zip") || strings.HasSuffix(root, ".tar.gz") {
			t.Fatalf("ArchiveRoot(%s) = %q", test.kind, root)
		}
	}
}

func TestChecksumsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asset.zip")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, size, err := FileSHA256(path)
	if err != nil || size != 7 {
		t.Fatalf("FileSHA256 = %q %d %v", hash, size, err)
	}
	parsed, err := ParseChecksums([]byte(hash + "  asset.zip\n"))
	if err != nil || parsed["asset.zip"] != hash {
		t.Fatalf("ParseChecksums = %#v %v", parsed, err)
	}
	if _, err := ParseChecksums([]byte("nope  asset.zip\n")); err == nil {
		t.Fatal("malformed checksum accepted")
	}
	if _, err := ParseChecksums(nil); err == nil {
		t.Fatal("empty checksums accepted")
	}
}

func TestManifestValidationAndFileVerification(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mix2api"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	hash, size, err := FileSHA256(filepath.Join(dir, "mix2api"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{
		Format: Format, Version: "2.0.0-rc.1", Tag: "v2.0.0-rc.1",
		Commit: "abc", Date: "2026-11-08T00:00:00Z",
		GOOS: "linux", GOARCH: "amd64", Kind: KindStandard, Repository: Repository,
		Files: []File{{Path: "mix2api", SHA256: hash, Mode: 0o755, Size: size}},
	}
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(dir, manifest.Files[0]); err != nil {
		t.Fatal(err)
	}
	manifest.Files[0].SHA256 = strings.Repeat("0", 64)
	if err := VerifyFile(dir, manifest.Files[0]); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	for _, bad := range []Manifest{
		{Format: "other", Version: "2.0.0", Tag: "v2.0.0", Repository: Repository, GOOS: "linux", GOARCH: "amd64", Files: []File{{Path: "x", SHA256: hash}}},
		{Format: Format, Version: "2.0.0", Tag: "v2.0.1", Repository: Repository, GOOS: "linux", GOARCH: "amd64", Files: []File{{Path: "x", SHA256: hash}}},
		{Format: Format, Version: "2.0.0", Tag: "v2.0.0", Repository: "evil/repo", GOOS: "linux", GOARCH: "amd64", Files: []File{{Path: "x", SHA256: hash}}},
		{Format: Format, Version: "2.0.0", Tag: "v2.0.0", Repository: Repository, GOOS: "linux", GOARCH: "amd64", Files: []File{{Path: "../x", SHA256: hash}}},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid manifest accepted: %#v", bad)
		}
	}
}
