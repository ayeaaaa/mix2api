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
	if status["updated_at"] == "" {
		t.Fatalf("status missing updated_at: %#v", status)
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
	return buildRawArchiveFor(t, version, files, extraManifest, runtime.GOOS, runtime.GOARCH, os.Getenv("GOARM"))
}

func buildRawArchiveFor(t *testing.T, version string, files map[string]tarEntry, extraManifest, goos, goarch, goarm string) (archive, checksums string, manifest corerelease.Manifest) {
	t.Helper()
	dir := t.TempDir()
	rootName := corerelease.ArchiveRoot(version, corerelease.KindStandard, goos, goarch, goarm)
	manifest = corerelease.Manifest{
		Format: corerelease.Format, Version: version, Tag: corerelease.Tag(version),
		Commit: "test", Date: "2026-11-08T00:00:00Z",
		GOOS: goos, GOARCH: goarch, GOARM: goarm,
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
	archive = filepath.Join(dir, corerelease.AssetName(version, corerelease.KindStandard, goos, goarch, goarm))
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

func TestBackupUsesRunningVersionWhenManifestIsMissing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "mix2api"), "old-binary", 0o755)
	archive, checksums, _ := buildTestRelease(t, root, "2.0.0", map[string][]byte{"mix2api": []byte("new-binary")})
	requestDir := t.TempDir()
	applier, err := New(Options{
		RequestDir: requestDir, Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"),
		CurrentVersion: "1.5.0", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		ArchivePath: archive, Checksums: checksums, Controller: &fakeController{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Run(context.Background(), update.Request{Action: "install", Version: "2.0.0", Tag: "v2.0.0", Root: root}); err != nil {
		t.Fatal(err)
	}
	records, err := applier.listBackups()
	if err != nil || len(records) != 1 || records[0].Version != "1.5.0" {
		for _, record := range records {
			t.Logf("backup version=%q complete=%v reason=%q", record.Version, record.Complete, record.Reason)
		}
		t.Fatalf("backups = %d err=%v", len(records), err)
	}
}

func writeTestJournal(t *testing.T, root string, entry journalEntry) {
	t.Helper()
	journal := transactionJournal{Format: transactionFormat, Version: "2.0.0", Root: root, Phase: "applying", Entries: []journalEntry{entry}}
	raw, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, JournalName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverInitialJournalDoesNotTouchUntouchedFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "mix2api")
	writeFile(t, target, "old-binary", 0o755)
	newHash := sha256.Sum256([]byte("new-binary"))
	writeTestJournal(t, root, journalEntry{
		Path: "mix2api", Target: target, NewSHA256: hex.EncodeToString(newHash[:]), NewSize: int64(len("new-binary")),
	})
	applier, err := New(Options{Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), RequestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := applier.RecoverOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Found || !recovery.Restored {
		t.Fatalf("recovery = %#v", recovery)
	}
	if got := readString(t, target); got != "old-binary" {
		t.Fatalf("untouched file was changed to %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, JournalName)); !os.IsNotExist(err) {
		t.Fatalf("journal remained: %v", err)
	}
}

func TestRecoverReconstructsLegacyJournalFromOldSibling(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "mix2api")
	oldPath := target + ".mix2api-old"
	writeFile(t, oldPath, "old-binary", 0o755)
	// Simulate the crash window before the journal gained Existed=true.
	writeTestJournal(t, root, journalEntry{
		Path: "mix2api", Target: target, OldPath: oldPath,
		NewSHA256: strings.Repeat("0", 64), NewSize: 10,
	})
	applier, err := New(Options{Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), RequestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.RecoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, target); got != "old-binary" {
		t.Fatalf("recovered file = %q", got)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old sibling remained: %v", err)
	}
}

func TestRecoverExactLegacyJournalWithoutOldPathOrExisted(t *testing.T) {
	for _, beforeReplacement := range []bool{false, true} {
		name := "after-replacement"
		if beforeReplacement {
			name = "before-replacement"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "mix2api")
			oldPath := target + ".mix2api-old"
			writeFile(t, oldPath, "old-binary", 0o755)
			if !beforeReplacement {
				writeFile(t, target, "new-binary", 0o755)
			}
			// The original journal schema omitted old_path and existed
			// entirely. Marshal a map so this test cannot silently start
			// writing the newer fields.
			raw, err := json.Marshal(map[string]any{
				"format": transactionFormat, "version": "2.0.0",
				"root": root, "phase": "applying",
				"entries": []any{map[string]any{
					"path": "mix2api", "target": target, "mode": 0o755,
					"new_sha256": strings.Repeat("0", 64), "new_size": 10,
					"complete": true,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, JournalName), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			applier, err := New(Options{Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), RequestDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := applier.RecoverOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !recovery.Found || !recovery.Restored {
				t.Fatalf("recovery = %#v", recovery)
			}
			if got := readString(t, target); got != "old-binary" {
				t.Fatalf("recovered file = %q", got)
			}
			if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
				t.Fatalf("old sibling remained: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, JournalName)); !os.IsNotExist(err) {
				t.Fatalf("journal remained: %v", err)
			}
		})
	}
}

func TestRecoveryPersistsVerifiedEntriesAndKeepsJournalOnFailure(t *testing.T) {
	root := t.TempDir()
	goodTarget := filepath.Join(root, "mix2api")
	goodOld := goodTarget + ".mix2api-old"
	writeFile(t, goodTarget, "new-binary", 0o755)
	writeFile(t, goodOld, "old-binary", 0o755)

	badTarget := filepath.Join(root, "README.md")
	badOld := badTarget + ".mix2api-old"
	writeFile(t, badOld, "old-readme", 0o644)
	if err := os.Mkdir(badTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	journal := transactionJournal{
		Format: transactionFormat, Version: "2.0.0", Root: root, Phase: "applying",
		Entries: []journalEntry{
			{Path: "README.md", Target: badTarget, OldPath: badOld, Existed: true},
			{Path: "mix2api", Target: goodTarget, OldPath: goodOld, Existed: true},
		},
	}
	raw, _ := json.Marshal(journal)
	journalPath := filepath.Join(root, JournalName)
	if err := os.WriteFile(journalPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	applier, err := New(Options{Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), RequestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.RecoverOnce(context.Background()); err == nil {
		t.Fatal("recovery with a non-regular target succeeded")
	}
	if got := readString(t, goodTarget); got != "old-binary" {
		t.Fatalf("first entry was not restored: %q", got)
	}
	if info, err := os.Stat(badTarget); err != nil || !info.IsDir() {
		t.Fatalf("failing target changed: %v", err)
	}
	raw, err = os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("journal must survive failed recovery: %v", err)
	}
	var persisted transactionJournal
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Entries) != 2 || !persisted.Entries[1].Restored || persisted.Entries[0].Restored {
		t.Fatalf("per-entry recovery progress not persisted: %s", raw)
	}
	// Remove the obstruction and resume. The already-restored entry is
	// skipped; the journal is removed only after both entries verify.
	if err := os.Remove(badTarget); err != nil {
		t.Fatal(err)
	}
	recovery, err := applier.RecoverOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Restored {
		t.Fatalf("recovery = %#v", recovery)
	}
	if got := readString(t, badTarget); got != "old-readme" {
		t.Fatalf("second entry was not restored: %q", got)
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("journal remained after verified restore: %v", err)
	}
}

func TestRecoverAfterNewFileAppliedRestoresOriginal(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "mix2api")
	oldPath := target + ".mix2api-old"
	writeFile(t, oldPath, "old-binary", 0o755)
	writeFile(t, target, "new-binary", 0o755)
	oldHash := sha256.Sum256([]byte("old-binary"))
	newHash := sha256.Sum256([]byte("new-binary"))
	writeTestJournal(t, root, journalEntry{
		Path: "mix2api", Target: target, OldPath: oldPath, Existed: true,
		OriginalSHA256: hex.EncodeToString(oldHash[:]), OriginalSize: int64(len("old-binary")),
		NewSHA256: hex.EncodeToString(newHash[:]), NewSize: int64(len("new-binary")), Complete: true,
	})
	applier, err := New(Options{Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), RequestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.RecoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, target); got != "old-binary" {
		t.Fatalf("recovered file = %q", got)
	}
}

func TestRecoverWithoutJournalIsDistinct(t *testing.T) {
	applier, err := New(Options{Root: t.TempDir(), BackupDir: filepath.Join(t.TempDir(), "backups"), RequestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := applier.RecoverOnce(context.Background())
	if err != nil || recovery.Found || recovery.Restored {
		t.Fatalf("recovery = %#v err=%v", recovery, err)
	}
}

func TestARMExecutorNormalizesGOARM(t *testing.T) {
	root := t.TempDir()
	applier, err := New(Options{Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), GOOS: "linux", GOARCH: "arm", GOARM: "armv7"})
	if err != nil {
		t.Fatal(err)
	}
	if applier.goarm != "7" || applier.goarch != "arm" {
		t.Fatalf("platform = %s/%s goarm=%q", applier.goos, applier.goarch, applier.goarm)
	}
	if _, err := New(Options{Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), GOOS: "linux", GOARCH: "arm", GOARM: "8"}); err == nil {
		t.Fatal("invalid GOARM accepted")
	}
}

func TestExtractVerifiedUsesARMFamilyMatching(t *testing.T) {
	for _, spelling := range []string{"7", "v7", "armv7"} {
		t.Run(spelling, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "mix2api"), "old-binary", 0o755)
			archive, checksums, _ := buildRawArchiveFor(t, "2.0.0", map[string]tarEntry{
				"mix2api": {data: []byte("new-binary"), mode: 0o755},
			}, "", "linux", "arm", spelling)
			applier, err := New(Options{
				Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), RequestDir: t.TempDir(),
				CurrentVersion: "1.0.0", GOOS: "linux", GOARCH: "arm", GOARM: "7",
				ArchivePath: archive, Checksums: checksums, Controller: &fakeController{},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := applier.Run(context.Background(), update.Request{
				Action: "install", Version: "2.0.0", Tag: "v2.0.0", Root: root,
			}); err != nil {
				t.Fatalf("install with manifest GOARM=%q: %v", spelling, err)
			}
			if got := readString(t, filepath.Join(root, "mix2api")); got != "new-binary" {
				t.Fatalf("binary = %q", got)
			}
			manifest, err := readInstalledManifest(root)
			if err != nil || manifest.GOARM != spelling {
				t.Fatalf("installed manifest = %#v err=%v", manifest, err)
			}
		})
	}

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "mix2api"), "old-binary", 0o755)
	archive, checksums, _ := buildRawArchiveFor(t, "2.0.0", map[string]tarEntry{
		"mix2api": {data: []byte("new-binary"), mode: 0o755},
	}, "", "linux", "arm", "7")
	applier, err := New(Options{
		Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), RequestDir: t.TempDir(),
		CurrentVersion: "1.0.0", GOOS: "linux", GOARCH: "arm", GOARM: "6",
		ArchivePath: archive, Checksums: checksums, Controller: &fakeController{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Run(context.Background(), update.Request{
		Action: "install", Version: "2.0.0", Tag: "v2.0.0", Root: root,
	}); err == nil {
		t.Fatal("GOARM=6 executor accepted an armv7 archive")
	}
	if got := readString(t, filepath.Join(root, "mix2api")); got != "old-binary" {
		t.Fatalf("mismatched install changed the binary: %q", got)
	}
}

func TestWindowsBatchHooksUseCmdExe(t *testing.T) {
	spec := CommandSpec{Path: `C:\Program Files\mix2api\restart.cmd`, Args: []string{"--fast", `C:\data dir`}}
	program, args := commandProgram(spec, "windows")
	wantLine := `cmd.exe /d /s /c ""C:\Program Files\mix2api\restart.cmd" "--fast" "C:\data dir""`
	if program != "cmd.exe" || len(args) != 4 || args[0] != "/d" || args[1] != "/s" || args[2] != "/c" ||
		args[3] != wantLine {
		t.Fatalf("batch program = %q args=%v", program, args)
	}
	if got := batchCommandLine(spec); got != wantLine {
		t.Fatalf("batch command line = %q, want %q", got, wantLine)
	}
	if got := batchCommandLine(CommandSpec{Path: `C:\hooks\restart.cmd`}); got != `cmd.exe /d /s /c ""C:\hooks\restart.cmd""` {
		t.Fatalf("no-arg command line = %q", got)
	}
	program, args = commandProgram(CommandSpec{Path: `C:\hooks\restart.exe`, Args: []string{"--fast"}}, "windows")
	if program != `C:\hooks\restart.exe` || len(args) != 1 || args[0] != "--fast" {
		t.Fatalf("exe program = %q args=%v", program, args)
	}
	program, args = commandProgram(CommandSpec{Path: "/usr/bin/systemctl", Args: []string{"stop", "mix2api"}}, "linux")
	if program != "/usr/bin/systemctl" || len(args) != 2 {
		t.Fatalf("unix program = %q args=%v", program, args)
	}
}

func TestRollbackToManifestlessInstallRemovesNewerManifestAndSynthesizes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "mix2api"), "old-binary", 0o755)
	archive, checksums, _ := buildTestRelease(t, root, "2.0.0", map[string][]byte{
		"mix2api":   []byte("new-binary"),
		"README.md": []byte("new readme"),
	})
	applier, err := New(Options{
		Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), CurrentVersion: "1.5.0",
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ArchivePath: archive, Checksums: checksums,
		RequestDir: t.TempDir(), Controller: &fakeController{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Run(context.Background(), update.Request{Action: "install", Version: "2.0.0", Tag: "v2.0.0", Root: root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "README.md")); err != nil {
		t.Fatalf("new file missing before rollback: %v", err)
	}
	if _, err := applier.Run(context.Background(), update.Request{Action: "rollback", Version: "1.5.0", Tag: "v1.5.0", Root: root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("newer-only file survived manifest-less rollback: %v", err)
	}
	manifest, err := readInstalledManifest(root)
	if err != nil {
		t.Fatalf("synthesized manifest: %v", err)
	}
	if manifest.Version != "1.5.0" || manifest.Tag != "v1.5.0" {
		t.Fatalf("synthesized manifest = %#v", manifest)
	}
}

func TestUnknownBackupVersionIsNotPublished(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "mix2api"), "old-binary", 0o755)
	archive, checksums, _ := buildTestRelease(t, root, "2.0.0", map[string][]byte{"mix2api": []byte("new-binary")})
	requestDir := t.TempDir()
	applier, err := New(Options{
		Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ArchivePath: archive, Checksums: checksums,
		RequestDir: requestDir, Controller: &fakeController{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Run(context.Background(), update.Request{Action: "install", Version: "2.0.0", Tag: "v2.0.0", Root: root}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(requestDir, update.BackupsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "0.0.0") {
		t.Fatalf("unknown backup leaked into inventory: %s", raw)
	}
}

func TestReleaseURLIdentity(t *testing.T) {
	safe := "https://github.com/ayeaaaa/mix2api/releases/download/v2.0.0/mix2api-2.0.0-linux-amd64.tar.gz"
	if !releaseURLAllowed(safe) {
		t.Fatal("canonical release URL rejected")
	}
	old := "https://github.com/ayeaaaa/mix2api-v2/releases/download/v2.0.0/mix2api-2.0.0-linux-amd64.tar.gz"
	if releaseURLAllowed(old) {
		t.Fatal("v1 repository URL accepted for v2 assets")
	}
}

func TestInstallFinalFromRCAndRollbackToRC(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "mix2api"), "rc-binary", 0o755)
	writeCurrentManifest(t, root, "2.0.0-rc.1", map[string][]byte{"mix2api": []byte("rc-binary")})
	archive, checksums, _ := buildTestRelease(t, root, "2.0.0", map[string][]byte{"mix2api": []byte("final-binary")})
	applier, err := New(Options{
		Root: root, BackupDir: filepath.Join(t.TempDir(), "backups"), RequestDir: t.TempDir(),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ArchivePath: archive, Checksums: checksums,
		Controller: &fakeController{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Run(context.Background(), update.Request{Action: "install", Version: "2.0.0", Tag: "v2.0.0", Root: root}); err != nil {
		t.Fatalf("RC->final install: %v", err)
	}
	if got := readString(t, filepath.Join(root, "mix2api")); got != "final-binary" {
		t.Fatalf("final binary = %q", got)
	}
	if _, err := applier.Run(context.Background(), update.Request{Action: "rollback", Version: "2.0.0-rc.1", Tag: "v2.0.0-rc.1", Root: root}); err != nil {
		t.Fatalf("final->RC rollback: %v", err)
	}
	if got := readString(t, filepath.Join(root, "mix2api")); got != "rc-binary" {
		t.Fatalf("RC binary = %q", got)
	}
	manifest, err := readInstalledManifest(root)
	if err != nil || manifest.Version != "2.0.0-rc.1" {
		t.Fatalf("manifest = %#v err=%v", manifest, err)
	}
}

func TestPruneKeepsIncompleteBackupsForDiagnosis(t *testing.T) {
	backupDir := t.TempDir()
	applier, err := New(Options{Root: t.TempDir(), BackupDir: backupDir, RequestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	makeRecord := func(version, created string, complete bool) *backupRecord {
		dir := filepath.Join(backupDir, version+"-"+strings.ReplaceAll(created, ":", ""))
		if err := os.MkdirAll(filepath.Join(dir, backupFilesDir), 0o700); err != nil {
			t.Fatal(err)
		}
		record := &backupRecord{Format: BackupFormat, Version: version, CreatedAt: created, Complete: complete, Dir: dir}
		if err := writeBackupMeta(record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	makeRecord("1.0.0", "2026-11-01 00:00:00", true)
	makeRecord("1.1.0", "2026-11-02 00:00:00", true)
	makeRecord("1.2.0", "2026-11-03 00:00:00", false)
	makeRecord("1.3.0", "2026-11-04 00:00:00", true)
	makeRecord("1.4.0", "2026-11-05 00:00:00", true)
	applier.pruneBackups()
	records, err := applier.listBackups()
	if err != nil {
		t.Fatal(err)
	}
	incomplete := 0
	complete := 0
	for _, record := range records {
		if record.Complete {
			complete++
		} else {
			incomplete++
		}
	}
	if complete != keepBackups || incomplete != 1 {
		t.Fatalf("after prune complete=%d incomplete=%d records=%d", complete, incomplete, len(records))
	}
}

func TestPruneDoesNotCountUnknownCompleteBackups(t *testing.T) {
	backupDir := t.TempDir()
	applier, err := New(Options{Root: t.TempDir(), BackupDir: backupDir, RequestDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	makeRecord := func(version, created string, complete bool) *backupRecord {
		dir := filepath.Join(backupDir, version+"-"+strings.ReplaceAll(created, ":", ""))
		if err := os.MkdirAll(filepath.Join(dir, backupFilesDir), 0o700); err != nil {
			t.Fatal(err)
		}
		record := &backupRecord{Format: BackupFormat, Version: version, CreatedAt: created, Complete: complete, Dir: dir}
		if err := writeBackupMeta(record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	makeRecord("1.0.0", "2026-11-01 00:00:00", true)
	makeRecord("1.1.0", "2026-11-02 00:00:00", true)
	makeRecord("1.2.0", "2026-11-03 00:00:00", true)
	makeRecord("0.0.0", "2026-11-04 00:00:00", true)
	makeRecord("unknown", "2026-11-05 00:00:00", false)
	applier.pruneBackups()
	records, err := applier.listBackups()
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]bool{}
	for _, record := range records {
		versions[record.Version] = true
	}
	for _, want := range []string{"1.0.0", "1.1.0", "1.2.0", "0.0.0", "unknown"} {
		if !versions[want] {
			t.Fatalf("prune removed %s; records=%#v", want, versions)
		}
	}
	// Adding one more valid complete backup must retire only the oldest valid
	// release, leaving 0.0.0/unknown diagnostics on disk.
	makeRecord("1.3.0", "2026-11-06 00:00:00", true)
	applier.pruneBackups()
	records, err = applier.listBackups()
	if err != nil {
		t.Fatal(err)
	}
	versions = map[string]bool{}
	for _, record := range records {
		versions[record.Version] = true
	}
	if versions["1.0.0"] || !versions["1.1.0"] || !versions["1.2.0"] || !versions["1.3.0"] ||
		!versions["0.0.0"] || !versions["unknown"] {
		t.Fatalf("unexpected prune result: %#v", versions)
	}
}
