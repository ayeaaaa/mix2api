package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corerelease "github.com/ayeaaaa/mix2api-v2/internal/core/release"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
)

type backupFile struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Mode    uint32 `json:"mode"`
	Size    int64  `json:"size"`
	Existed bool   `json:"existed"`
}

func (f backupFile) entry() corerelease.File {
	return corerelease.File{Path: f.Path, SHA256: f.SHA256, Mode: f.Mode, Size: f.Size}
}

type backupRecord struct {
	Format    string       `json:"format"`
	Version   string       `json:"version"`
	CreatedAt string       `json:"created_at"`
	Complete  bool         `json:"complete"`
	Reason    string       `json:"reason,omitempty"`
	Files     []backupFile `json:"files"`
	Dir       string       `json:"-"`

	SourceVersion string `json:"source_version,omitempty"`
}

func (b *backupRecord) FilesDir() string { return filepath.Join(b.Dir, backupFilesDir) }

func (b *backupRecord) sizeMB() int {
	total := int64(0)
	for _, file := range b.Files {
		if file.Existed {
			total += file.Size
		}
	}
	return int((total + (1 << 20) - 1) >> 20)
}

func (a *Applier) createBackup(manifest corerelease.Manifest, targetVersion string) (*backupRecord, error) {
	if err := os.MkdirAll(a.backupDir, 0o700); err != nil {
		return nil, err
	}
	sourceVersion := a.currentVersion()
	label := sourceVersion
	if label == "" {
		label = "unknown"
	}
	stamp := a.now().UTC().Format("20060102-150405")
	dir := filepath.Join(a.backupDir, label+"-"+stamp)
	for index := 1; ; index++ {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			break
		}
		dir = filepath.Join(a.backupDir, fmt.Sprintf("%s-%s-%d", label, stamp, index))
	}
	if err := os.MkdirAll(filepath.Join(dir, backupFilesDir), 0o700); err != nil {
		return nil, err
	}
	record := &backupRecord{
		Format: BackupFormat, Version: sourceVersion, CreatedAt: a.now().Format("2006-01-02 15:04:05"),
		Dir: dir, SourceVersion: targetVersion,
	}
	paths := append([]corerelease.File(nil), manifest.Files...)
	// Back up the installed manifest too, so a rollback restores the version
	// identity that the update is about to replace.
	manifestPath := filepath.Join(a.root, corerelease.ManifestName)
	if info, err := os.Lstat(manifestPath); err == nil && info.Mode().IsRegular() {
		if sum, size, err := corerelease.FileSHA256(manifestPath); err == nil {
			paths = append(paths, corerelease.File{
				Path: corerelease.ManifestName, SHA256: sum, Mode: uint32(info.Mode().Perm()), Size: size,
			})
		}
	}
	for _, file := range paths {
		target, err := secureExistingPath(a.root, file.Path)
		if err != nil {
			return a.backupFailed(record, "unsafe path", err)
		}
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			record.Files = append(record.Files, backupFile{Path: file.Path, Existed: false})
			continue
		}
		if err != nil {
			return a.backupFailed(record, "read source failed", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return a.backupFailed(record, "source is not a regular file", errors.New("source is not a regular file"))
		}
		dest := filepath.Join(record.FilesDir(), filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return a.backupFailed(record, "create backup directory failed", err)
		}
		mode := info.Mode().Perm()
		if err := copyFileMode(target, dest, mode); err != nil {
			return a.backupFailed(record, "copy backup failed", err)
		}
		sum, size, err := corerelease.FileSHA256(dest)
		if err != nil || size != info.Size() {
			return a.backupFailed(record, "verify backup failed", errors.Join(err, errors.New("size mismatch")))
		}
		record.Files = append(record.Files, backupFile{
			Path: file.Path, SHA256: sum, Mode: uint32(mode), Size: size, Existed: true,
		})
	}
	record.Complete = true
	if err := writeBackupMeta(record); err != nil {
		return record, err
	}
	_ = a.publishBackups()
	return record, nil
}

func (a *Applier) backupFailed(record *backupRecord, reason string, cause error) (*backupRecord, error) {
	record.Reason = reason
	record.Complete = false
	_ = writeBackupMeta(record)
	return record, fmt.Errorf("apply: %s: %w", reason, cause)
}

// secureExistingPath resolves a relative path without requiring its parent to
// exist yet. It verifies the closest existing ancestor is still inside root,
// so a symlinked component cannot redirect a backup read.
func secureExistingPath(root, relative string) (string, error) {
	if relative == "" || strings.Contains(relative, "\\") || strings.HasPrefix(relative, "/") {
		return "", fmt.Errorf("apply: unsafe path %q", relative)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("apply: unsafe path %q", relative)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, clean)
	parent := filepath.Dir(target)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
				return "", fmt.Errorf("apply: path %q escapes root", relative)
			}
			return target, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", fmt.Errorf("apply: cannot resolve parent for %q", relative)
		}
		parent = next
	}
}

func (a *Applier) currentVersion() string {
	manifest, err := readInstalledManifest(a.root)
	if err == nil && manifest != nil && releaseVersion(manifest.Version) {
		return manifest.Version
	}
	if releaseVersion(a.fallbackVersion) {
		return a.fallbackVersion
	}
	return ""
}

func readInstalledManifest(root string) (*corerelease.Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(root, corerelease.ManifestName))
	if err != nil {
		return nil, err
	}
	var manifest corerelease.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func writeBackupMeta(record *backupRecord) error {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(record.Dir, BackupMetaName), append(raw, '\n'), 0o600)
}

func (a *Applier) verifyBackup(record *backupRecord) error {
	if record == nil || !record.Complete || !releaseVersion(record.Version) {
		return errors.New("apply: backup is incomplete or has an invalid version")
	}
	for _, file := range record.Files {
		if !file.Existed {
			continue
		}
		path := filepath.Join(record.FilesDir(), filepath.FromSlash(file.Path))
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("apply: backup contains a non-regular file")
		}
		sum, size, err := corerelease.FileSHA256(path)
		if err != nil {
			return err
		}
		if size != file.Size || !strings.EqualFold(sum, file.SHA256) {
			return errors.New("apply: backup checksum mismatch")
		}
	}
	return nil
}

func (a *Applier) findBackup(version string) (*backupRecord, error) {
	records, err := a.listBackups()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Complete && record.Version == version {
			return record, nil
		}
	}
	return nil, errors.New("apply: backup not found")
}

func (a *Applier) listBackups() ([]*backupRecord, error) {
	entries, err := os.ReadDir(a.backupDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]*backupRecord, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(a.backupDir, entry.Name())
		raw, err := os.ReadFile(filepath.Join(dir, BackupMetaName))
		if err != nil {
			continue
		}
		var record backupRecord
		if json.Unmarshal(raw, &record) != nil || record.Format != BackupFormat {
			continue
		}
		record.Dir = dir
		records = append(records, &record)
	}
	return sortedBackups(records), nil
}

func (a *Applier) publishBackups() error {
	if strings.TrimSpace(a.requestDir) == "" {
		return nil
	}
	records, err := a.listBackups()
	if err != nil {
		return err
	}
	rows := make([]map[string]any, 0, 8)
	for _, record := range records {
		if len(rows) >= 8 {
			break
		}
		if !releaseVersion(record.Version) {
			continue
		}
		rows = append(rows, map[string]any{
			"version": record.Version, "created_at": record.CreatedAt,
			"files": countExisting(record.Files), "complete": record.Complete,
			"size_mb": record.sizeMB(),
		})
	}
	raw, err := json.Marshal(map[string]any{"backups": rows})
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.requestDir, update.BackupsFile), raw, 0o600)
}

func countExisting(files []backupFile) int {
	count := 0
	for _, file := range files {
		if file.Existed {
			count++
		}
	}
	return count
}

func (a *Applier) pruneBackups() {
	records, err := a.listBackups()
	if err != nil {
		return
	}
	complete := make([]*backupRecord, 0, len(records))
	for _, record := range records {
		// Unknown/0.0.0 backups are diagnostic artifacts, not rollback
		// targets, so they must not count against the retained release set.
		if record.Complete && releaseVersion(record.Version) {
			complete = append(complete, record)
		}
	}
	if len(complete) <= keepBackups {
		return
	}
	for _, record := range complete[keepBackups:] {
		_ = os.RemoveAll(record.Dir)
	}
}

type journalEntry struct {
	Path           string `json:"path"`
	Target         string `json:"target"`
	OldPath        string `json:"old_path,omitempty"`
	Existed        bool   `json:"existed"`
	OriginalSHA256 string `json:"original_sha256,omitempty"`
	OriginalSize   int64  `json:"original_size,omitempty"`
	Mode           uint32 `json:"mode"`
	NewSHA256      string `json:"new_sha256,omitempty"`
	NewSize        int64  `json:"new_size,omitempty"`
	Complete       bool   `json:"complete"`
	// Restored marks one entry whose original state has been re-established
	// and verified. Recovery persists the journal after every entry so a crash
	// during recovery resumes instead of replaying already-restored files.
	Restored bool `json:"restored,omitempty"`
}

type transactionJournal struct {
	Format  string         `json:"format"`
	Version string         `json:"version"`
	Root    string         `json:"root"`
	Phase   string         `json:"phase"`
	Entries []journalEntry `json:"entries"`
}

const transactionFormat = "mix2api-update-transaction/1"

func (a *Applier) applyFiles(stageRoot string, files []corerelease.File, backup *backupRecord) error {
	journal := &transactionJournal{Format: transactionFormat, Root: a.root, Phase: "applying"}
	if backup != nil {
		journal.Version = backup.Version
	}
	for _, file := range files {
		target, err := a.prepareTarget(file.Path)
		if err != nil {
			return err
		}
		entry := journalEntry{
			Path: file.Path, Target: target, Mode: file.Mode,
			NewSHA256: file.SHA256, NewSize: file.Size,
		}
		info, err := os.Lstat(target)
		if err == nil {
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("apply: target %s is not a regular file", file.Path)
			}
			entry.Existed = true
			entry.OldPath = target + ".mix2api-old"
			entry.OriginalSHA256, entry.OriginalSize, err = corerelease.FileSHA256(target)
			if err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		journal.Entries = append(journal.Entries, entry)
	}
	if err := a.writeJournal(journal); err != nil {
		return err
	}
	for index := range journal.Entries {
		entry := &journal.Entries[index]
		source := filepath.Join(stageRoot, filepath.FromSlash(entry.Path))
		if info, err := os.Stat(source); err != nil || !info.Mode().IsRegular() {
			return a.abortApply(journal, fmt.Errorf("apply: staged file %s is missing", entry.Path))
		}
		if entry.Existed {
			_ = os.Remove(entry.OldPath)
			if err := os.Rename(entry.Target, entry.OldPath); err != nil {
				return a.abortApply(journal, err)
			}
			_ = syncDir(filepath.Dir(entry.Target))
			// Persist the rename before writing the replacement so a crash
			// between these steps can always reconstruct the original.
			if err := a.writeJournal(journal); err != nil {
				_ = os.Rename(entry.OldPath, entry.Target)
				return a.abortApply(journal, err)
			}
		}
		temp := entry.Target + ".mix2api-new"
		_ = os.Remove(temp)
		mode := os.FileMode(entry.Mode).Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := copyFileMode(source, temp, mode); err != nil {
			return a.abortApply(journal, err)
		}
		if err := os.Rename(temp, entry.Target); err != nil {
			_ = os.Remove(temp)
			return a.abortApply(journal, err)
		}
		_ = syncDir(filepath.Dir(entry.Target))
		entry.Complete = true
		if err := a.writeJournal(journal); err != nil {
			return a.abortApply(journal, err)
		}
	}
	if err := a.verifyApplied(journal); err != nil {
		return a.abortApply(journal, err)
	}
	// Delete the journal only after every applied file is verified. The
	// original siblings stay until the journal is gone, so a crash during
	// cleanup is either recoverable (journal present) or already committed.
	if err := a.removeJournal(); err != nil {
		return a.abortApply(journal, err)
	}
	for _, entry := range journal.Entries {
		if entry.OldPath != "" {
			_ = os.Remove(entry.OldPath)
		}
	}
	_ = syncDir(a.root)
	return nil
}

func (a *Applier) abortApply(journal *transactionJournal, cause error) error {
	if err := a.restoreJournal(journal); err != nil {
		return errors.Join(cause, fmt.Errorf("apply: restore failed: %w", err))
	}
	if err := a.removeJournal(); err != nil {
		a.logger("update: remove journal after restore: " + err.Error())
	}
	return cause
}

func (a *Applier) verifyApplied(journal *transactionJournal) error {
	for _, entry := range journal.Entries {
		info, err := os.Lstat(entry.Target)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("apply: applied file %s is missing", entry.Path)
		}
		sum, size, err := corerelease.FileSHA256(entry.Target)
		if err != nil {
			return err
		}
		if !strings.EqualFold(sum, entry.NewSHA256) || (entry.NewSize != 0 && size != entry.NewSize) {
			return fmt.Errorf("apply: applied file %s checksum mismatch", entry.Path)
		}
	}
	return nil
}
func (a *Applier) prepareTarget(relative string) (string, error) {
	target, err := safeJoinExistingParent(a.root, relative)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	return target, nil
}

// safeJoinExistingParent walks each path component without following a
// symlink, creating missing directories only after the path is proven safe.
func safeJoinExistingParent(root, relative string) (string, error) {
	if relative == "" || strings.Contains(relative, "\\") || strings.HasPrefix(relative, "/") {
		return "", fmt.Errorf("apply: unsafe path %q", relative)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("apply: unsafe path %q", relative)
	}
	current := root
	parts := strings.Split(filepath.Dir(clean), string(filepath.Separator))
	for _, part := range parts {
		if part == "." || part == "" {
			continue
		}
		next := filepath.Join(current, part)
		info, err := os.Lstat(next)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(next, 0o755); err != nil {
				return "", err
			}
			current = next
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("apply: unsafe path component %q", part)
		}
		current = next
	}
	return filepath.Join(root, clean), nil
}

func (a *Applier) writeJournal(journal *transactionJournal) error {
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.root, JournalName), append(raw, '\n'), 0o600)
}

func (a *Applier) restoreJournal(journal *transactionJournal) error {
	var firstErr error
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		entry := &journal.Entries[index]
		if entry.Restored {
			if err := verifyRestoredJournalEntry(*entry); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := a.restoreJournalEntry(entry); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Persist progress per entry. A crash after this write skips the
		// entry above, so recovery never mistakes an already-restored file
		// for a file the abandoned transaction introduced.
		entry.Restored = true
		if err := a.writeJournal(journal); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		_ = syncDir(filepath.Dir(entry.Target))
	}
	if firstErr == nil {
		_ = syncDir(a.root)
	}
	return firstErr
}

// journalOldPath returns the sibling used by this entry. Journals written
// before OldPath was persisted (the original schema) omitted both OldPath and
// Existed, so Target+".mix2api-old" is the only evidence that the previous
// file was moved before the crash.
func journalOldPath(entry journalEntry) string {
	if strings.TrimSpace(entry.OldPath) != "" {
		return entry.OldPath
	}
	if strings.TrimSpace(entry.Target) != "" {
		return entry.Target + ".mix2api-old"
	}
	return ""
}

func (a *Applier) restoreJournalEntry(entry *journalEntry) error {
	oldPath := journalOldPath(*entry)
	oldExists := false
	if oldPath != "" {
		if info, err := os.Lstat(oldPath); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("apply: recovery sibling %s is not a regular file", oldPath)
			}
			oldExists = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	existed := entry.Existed || oldExists
	if existed {
		if oldExists {
			entry.Existed = true
			if info, err := os.Lstat(entry.Target); err == nil {
				if !info.Mode().IsRegular() {
					return fmt.Errorf("apply: recovery target %s is not a regular file", entry.Path)
				}
				if err := os.Remove(entry.Target); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.Rename(oldPath, entry.Target); err != nil {
				return err
			}
			_ = syncDir(filepath.Dir(entry.Target))
		}
		_ = os.Remove(entry.Target + ".mix2api-new")
		return verifyRestoredJournalEntry(*entry)
	}
	// A file that did not exist before is removed only when the current
	// target is the exact file this transaction wrote. An untouched
	// directory (or unrelated file) at the same path must survive.
	info, err := os.Lstat(entry.Target)
	if errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(entry.Target + ".mix2api-new")
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if entry.NewSHA256 != "" {
		sum, _, hashErr := corerelease.FileSHA256(entry.Target)
		if hashErr != nil {
			return hashErr
		}
		if !strings.EqualFold(sum, entry.NewSHA256) {
			return nil
		}
	}
	if err := os.Remove(entry.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(entry.Target + ".mix2api-new")
	return nil
}

// verifyRestoredJournalEntry checks the state a finished recovery must leave:
// the original content when the journal recorded a hash, or at least a regular
// file when it predates hashes.
func verifyRestoredJournalEntry(entry journalEntry) error {
	if !entry.Existed {
		return nil
	}
	info, err := os.Lstat(entry.Target)
	if err != nil {
		return fmt.Errorf("apply: recovered file %s is missing: %w", entry.Path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("apply: recovered file %s is not a regular file", entry.Path)
	}
	if entry.OriginalSHA256 == "" {
		return nil
	}
	sum, size, err := corerelease.FileSHA256(entry.Target)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, entry.OriginalSHA256) || (entry.OriginalSize != 0 && size != entry.OriginalSize) {
		return fmt.Errorf("apply: recovered file %s does not match its original checksum", entry.Path)
	}
	return nil
}

func (a *Applier) removeJournal() error {
	path := filepath.Join(a.root, JournalName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(a.root)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer dir.Close()
	// Directory fsync is best effort; Windows and some filesystems reject it.
	_ = dir.Sync()
	return nil
}

// NoopController is used when the operator supervises restarts externally.
type NoopController struct{}

func (NoopController) Stop(context.Context, update.Request) error  { return nil }
func (NoopController) Start(context.Context, update.Request) error { return nil }
