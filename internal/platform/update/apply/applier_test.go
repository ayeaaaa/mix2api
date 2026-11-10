package apply

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	corerelease "github.com/ayeaaaa/mix2api-v2/internal/core/release"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
)

type fakeController struct {
	stops, starts int
	failStop      bool
	failStart     bool
}

func (f *fakeController) Stop(context.Context, update.Request) error {
	f.stops++
	if f.failStop {
		return fmt.Errorf("stop failed")
	}
	return nil
}

func (f *fakeController) Start(context.Context, update.Request) error {
	f.starts++
	if f.failStart {
		return fmt.Errorf("start failed")
	}
	return nil
}

func testVersion() string { return "2.0.0" }

func TestInstallBackupRollbackAndPublish(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "mix2api"), "old-binary", 0o755)
	writeFile(t, filepath.Join(root, "LICENSE"), "old-license", 0o644)
	writeCurrentManifest(t, root, "1.0.0", map[string][]byte{
		"mix2api": []byte("old-binary"),
		"LICENSE": []byte("old-license"),
	})

	archive, checksums, _ := buildTestRelease(t, root, "2.0.0", map[string][]byte{
		"mix2api":   []byte("new-binary"),
		"LICENSE":   []byte("new-license"),
		"README.md": []byte("new readme"),
	})
	requestDir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")
	controller := &fakeController{}
	applier, err := New(Options{
		RequestDir: requestDir, Root: root, BackupDir: backupDir,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Controller: controller, ArchivePath: archive, Checksums: checksums,
		Now: singleTime(),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := update.Request{Action: "install", Version: "2.0.0", Tag: "v2.0.0", Root: root, Port: 8788}
	result, err := applier.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.State != "succeeded" || controller.stops != 1 || controller.starts != 1 {
		t.Fatalf("result=%#v stops=%d starts=%d", result, controller.stops, controller.starts)
	}
	if got := readString(t, filepath.Join(root, "mix2api")); got != "new-binary" {
		t.Fatalf("binary = %q", got)
	}
	if got := readString(t, filepath.Join(root, "README.md")); got != "new readme" {
		t.Fatalf("README = %q", got)
	}
	statusRaw, err := os.ReadFile(filepath.Join(requestDir, update.StatusFile))
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(statusRaw, &status); err != nil {
		t.Fatal(err)
	}
	if status["state"] != "succeeded" || status["version"] != "2.0.0" {
		t.Fatalf("status = %#v", status)
	}
	rawBackups, err := os.ReadFile(filepath.Join(requestDir, update.BackupsFile))
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Backups []map[string]any `json:"backups"`
	}
	if err := json.Unmarshal(rawBackups, &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Backups) != 1 || inventory.Backups[0]["version"] != "1.0.0" || inventory.Backups[0]["complete"] != true {
		t.Fatalf("inventory = %#v", inventory)
	}
	if _, err := os.Stat(filepath.Join(root, JournalName)); !os.IsNotExist(err) {
		t.Fatalf("journal was not removed: %v", err)
	}

	// Roll back to 1.0.0. README.md was not in the backup and must disappear.
	request = update.Request{Action: "rollback", Version: "1.0.0", Tag: "v1.0.0", Root: root, Port: 8788}
	result, err = applier.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if result.State != "rolled_back" {
		t.Fatalf("rollback result = %#v", result)
	}
	if got := readString(t, filepath.Join(root, "mix2api")); got != "old-binary" {
		t.Fatalf("rolled back binary = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("README introduced by the update survived rollback")
	}
	if controller.stops != 2 || controller.starts != 2 {
		t.Fatalf("controller stops=%d starts=%d", controller.stops, controller.starts)
	}
}

func TestChecksumMismatchLeavesInstallUntouched(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "mix2api"), "old", 0o755)
	writeCurrentManifest(t, root, "1.0.0", map[string][]byte{"mix2api": []byte("old")})
	archive, _, _ := buildTestRelease(t, root, "2.0.0", map[string][]byte{"mix2api": []byte("new")})
	checksums := filepath.Join(t.TempDir(), corerelease.ChecksumsName)
	if err := os.WriteFile(checksums, []byte(strings.Repeat("0", 64)+"  "+filepath.Base(archive)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	applier, err := New(Options{
		Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		ArchivePath: archive, Checksums: checksums, RequestDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = applier.Run(context.Background(), update.Request{Action: "install", Version: "2.0.0", Tag: "v2.0.0", Root: root})
	if err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if got := readString(t, filepath.Join(root, "mix2api")); got != "old" {
		t.Fatalf("binary changed to %q", got)
	}
}

func TestArchiveTraversalAndSymlinkRejected(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "mix2api"), "old", 0o755)
	archive, checksums, _ := buildRawArchive(t, "2.0.0", map[string]tarEntry{
		"mix2api":   {data: []byte("new"), mode: 0o755},
		"../escape": {data: []byte("bad"), mode: 0o644},
	}, "")
	applier, err := New(Options{
		Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		ArchivePath: archive, Checksums: checksums,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.extractVerified(archive, checksums, "2.0.0"); err == nil {
		t.Fatal("traversal archive accepted")
	}

	archive, checksums, _ = buildRawArchive(t, "2.0.0", map[string]tarEntry{
		"mix2api": {data: []byte("new"), mode: 0o755, symlink: true},
	}, "")
	if _, err := applier.extractVerified(archive, checksums, "2.0.0"); err == nil {
		t.Fatal("symlink archive accepted")
	}
}

func TestApplyFailureRestoresPreviousFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "mix2api"), "old-binary", 0o755)
	if err := os.Mkdir(filepath.Join(root, "README.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCurrentManifest(t, root, "1.0.0", map[string][]byte{"mix2api": []byte("old-binary")})
	archive, checksums, _ := buildTestRelease(t, root, "2.0.0", map[string][]byte{
		"mix2api":   []byte("new-binary"),
		"README.md": []byte("new readme"),
	})
	applier, err := New(Options{
		Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		ArchivePath: archive, Checksums: checksums, RequestDir: t.TempDir(),
		Controller: &fakeController{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Run(context.Background(), update.Request{Action: "install", Version: "2.0.0", Tag: "v2.0.0", Root: root}); err == nil {
		t.Fatal("expected apply failure")
	}
	if got := readString(t, filepath.Join(root, "mix2api")); got != "old-binary" {
		t.Fatalf("binary after failed apply = %q", got)
	}
	if info, err := os.Stat(filepath.Join(root, "README.md")); err != nil || !info.IsDir() {
		t.Fatalf("README.md directory changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, JournalName)); !os.IsNotExist(err) {
		t.Fatalf("journal remained after restore: %v", err)
	}
}

func TestRecoverInterruptedJournal(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "mix2api")
	writeFile(t, target+".mix2api-old", "old-binary", 0o755)
	writeFile(t, target, "new-binary", 0o755)
	journal := transactionJournal{
		Format: transactionFormat, Version: "2.0.0", Root: root, Phase: "applying",
		Entries: []journalEntry{{Path: "mix2api", Target: target, OldPath: target + ".mix2api-old", Existed: true, Complete: true}},
	}
	raw, _ := json.Marshal(journal)
	if err := os.WriteFile(filepath.Join(root, JournalName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	applier, err := New(Options{Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), RequestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := applier.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, target); got != "old-binary" {
		t.Fatalf("recovered binary = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, JournalName)); !os.IsNotExist(err) {
		t.Fatalf("journal remained: %v", err)
	}
}

func TestRequestValidation(t *testing.T) {
	root := t.TempDir()
	if _, err := New(Options{Root: root, Repository: "evil/repo"}); err == nil {
		t.Fatal("untrusted repository accepted")
	}
	applier, err := New(Options{Root: root, BackupDir: filepath.Join(t.TempDir(), "b"), RequestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []update.Request{
		{Action: "install", Version: "2.0.0", Tag: "v2.0.1"},
		{Action: "install", Version: "2.0.0-rc.1", Tag: "v2.0.0-rc.1"},
		{Action: "upgrade", Version: "2.0.0", Tag: "v2.0.0"},
		{Action: "install", Version: "2.0.0", Tag: "v2.0.0", Root: t.TempDir()},
	} {
		if err := applier.validateRequest(request); err == nil {
			t.Fatalf("invalid request accepted: %#v", request)
		}
	}
}

func TestHTTPDownloaderRejectsNonReleaseURL(t *testing.T) {
	downloader := NewHTTPDownloader(nil, 1024)
	for _, rawURL := range []string{
		"http://github.com/ayeaaaa/mix2api/releases/download/v2.0.0/x.zip",
		"https://evil.example/ayeaaaa/mix2api/releases/download/v2.0.0/x.zip",
		"https://github.com/other/repo/releases/download/v2.0.0/x.zip",
	} {
		if err := downloader.Download(context.Background(), rawURL, filepath.Join(t.TempDir(), "x")); err == nil {
			t.Fatalf("unsafe URL accepted: %s", rawURL)
		}
	}
}

type tarEntry struct {
	data    []byte
	mode    int64
	symlink bool
}

func buildTestRelease(t *testing.T, root, version string, files map[string][]byte) (archive, checksums string, manifest corerelease.Manifest) {
	t.Helper()
	entries := make(map[string]tarEntry, len(files))
	for name, data := range files {
		entries[name] = tarEntry{data: data, mode: 0o644}
	}
	if entry, ok := entries["mix2api"]; ok {
		entry.mode = 0o755
		entries["mix2api"] = entry
	}
	return buildRawArchive(t, version, entries, "")
}

func buildRawArchive(t *testing.T, version string, files map[string]tarEntry, extraManifest string) (archive, checksums string, manifest corerelease.Manifest) {
	t.Helper()
	dir := t.TempDir()
	rootName := "mix2api-" + version + "-" + runtime.GOOS + "-" + runtime.GOARCH
	manifest = corerelease.Manifest{
		Format: corerelease.Format, Version: version, Tag: corerelease.Tag(version),
		Commit: "test", Date: "2026-11-08T00:00:00Z",
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GOARM: os.Getenv("GOARM"),
		Kind: corerelease.KindStandard, Repository: corerelease.Repository,
	}
	for name, entry := range files {
		sum := sha256.Sum256(entry.data)
		manifest.Files = append(manifest.Files, corerelease.File{
			Path: name, SHA256: hex.EncodeToString(sum[:]), Mode: uint32(entry.mode), Size: int64(len(entry.data)),
		})
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	rawManifest, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if extraManifest != "" {
		rawManifest = []byte(extraManifest)
	}
	archive = filepath.Join(dir, corerelease.AssetName(version, corerelease.KindStandard, runtime.GOOS, runtime.GOARCH, os.Getenv("GOARM")))
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	// Root directory entry.
	if err := tarWriter.WriteHeader(&tar.Header{Name: rootName + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		entry := files[name]
		header := &tar.Header{Name: rootName + "/" + name, Mode: entry.mode, Size: int64(len(entry.data)), Typeflag: tar.TypeReg}
		if entry.symlink {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = "target"
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if !entry.symlink {
			if _, err := tarWriter.Write(entry.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	manifestHeader := &tar.Header{Name: rootName + "/" + corerelease.ManifestName, Mode: 0o644, Size: int64(len(rawManifest)), Typeflag: tar.TypeReg}
	if err := tarWriter.WriteHeader(manifestHeader); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(rawManifest); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	sum, _, err := corerelease.FileSHA256(archive)
	if err != nil {
		t.Fatal(err)
	}
	checksums = filepath.Join(dir, corerelease.ChecksumsName)
	if err := os.WriteFile(checksums, []byte(sum+"  "+filepath.Base(archive)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return archive, checksums, manifest
}

func writeCurrentManifest(t *testing.T, root, version string, files map[string][]byte) {
	t.Helper()
	manifest := corerelease.Manifest{
		Format: corerelease.Format, Version: version, Tag: corerelease.Tag(version),
		Commit: "old", Date: "2026-01-01T00:00:00Z",
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GOARM: os.Getenv("GOARM"),
		Kind: corerelease.KindStandard, Repository: corerelease.Repository,
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := files[name]
		sum := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, corerelease.File{
			Path: name, SHA256: hex.EncodeToString(sum[:]), Mode: 0o644, Size: int64(len(data)),
		})
	}
	raw, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, corerelease.ManifestName), string(raw), 0o644)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func singleTime() func() time.Time {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}
