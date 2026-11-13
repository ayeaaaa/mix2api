// Package apply implements the privileged update executor for mix2api release
// archives. It validates one queued request, verifies the fixed repository
// asset and checksum, takes a complete backup, atomically replaces files, runs
// the service restart hook, and can recover an interrupted application or
// restore a retained backup.
//
// The gateway process never imports this package; it runs as a separate,
// operator-owned command.
package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	corerelease "github.com/ayeaaaa/mix2api-v2/internal/core/release"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
)

const (
	BackupFormat      = "mix2api-update-backup/1"
	JournalName       = ".mix2api-update-journal.json"
	BackupMetaName    = "backup.json"
	backupFilesDir    = "files"
	maxArchiveBytes   = 512 << 20
	maxArchiveFiles   = 1024
	maxSingleFileSize = 256 << 20
	keepBackups       = 3
)

// Downloader fetches one fixed URL into dest. Tests substitute a fake.
type Downloader interface {
	Download(ctx context.Context, url, dest string) error
}

// ServiceController stops and starts the supervised gateway. Empty commands
// are a no-op so Unix deployments that replace a running binary still work.
type ServiceController interface {
	Stop(ctx context.Context, request update.Request) error
	Start(ctx context.Context, request update.Request) error
}

// Options configures one Applier. Zero values are resolved to the command's
// runtime platform and standard paths.
type Options struct {
	RequestDir     string
	Root           string
	BackupDir      string
	Repository     string
	CurrentVersion string
	GOOS           string
	GOARCH         string
	GOARM          string
	Now            func() time.Time
	Downloader     Downloader
	Controller     ServiceController
	Logger         func(string)
	ArchivePath    string
	Checksums      string
}

// Applier performs validated installs and rollbacks.
type Applier struct {
	requestDir      string
	root            string
	backupDir       string
	repository      string
	fallbackVersion string
	goos            string
	goarch          string
	goarm           string
	now             func() time.Time
	downloader      Downloader
	controller      ServiceController
	logger          func(string)
	archive         string
	checksums       string
}

// Result is the outcome of an install or rollback.
type Result struct {
	State         string `json:"state"`
	Version       string `json:"version"`
	Message       string `json:"message"`
	BackupVersion string `json:"backup_version,omitempty"`
	Asset         string `json:"asset,omitempty"`
}

// New validates configuration and resolves defaults.
func New(opts Options) (*Applier, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = func(string) {}
	}
	if opts.Repository == "" {
		opts.Repository = corerelease.Repository
	}
	if opts.Repository != corerelease.Repository {
		return nil, fmt.Errorf("apply: repository %q is not allowed", opts.Repository)
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	if strings.TrimSpace(opts.GOARM) == "" && opts.GOARCH == "arm" {
		opts.GOARM = corerelease.RuntimeGOARM()
	}
	if opts.GOARCH == "arm" {
		family, ok := corerelease.NormalizeGOARM(opts.GOARM)
		if !ok {
			return nil, fmt.Errorf("apply: invalid GOARM %q", opts.GOARM)
		}
		opts.GOARM = family
	} else if strings.TrimSpace(opts.GOARM) != "" {
		return nil, fmt.Errorf("apply: GOARM is only valid for GOARCH arm")
	}
	if opts.Root == "" {
		executable, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("apply: resolve executable: %w", err)
		}
		opts.Root = filepath.Dir(executable)
	}
	root, err := secureDir(opts.Root, "install root")
	if err != nil {
		return nil, err
	}
	opts.Root = root
	if opts.BackupDir == "" {
		opts.BackupDir = filepath.Join(root, ".mix2api-backups")
	}
	backupDir, err := filepath.Abs(opts.BackupDir)
	if err != nil {
		return nil, fmt.Errorf("apply: backup directory: %w", err)
	}
	opts.BackupDir = filepath.Clean(backupDir)
	if opts.Downloader == nil {
		opts.Downloader = NewHTTPDownloader(nil, maxArchiveBytes)
	}
	if opts.Controller == nil {
		opts.Controller = NoopController{}
	}
	return &Applier{
		requestDir: strings.TrimSpace(opts.RequestDir),
		root:       opts.Root,
		backupDir:  opts.BackupDir,
		repository: opts.Repository, fallbackVersion: strings.TrimPrefix(strings.TrimSpace(opts.CurrentVersion), "v"),
		goos: opts.GOOS, goarch: opts.GOARCH, goarm: opts.GOARM,
		now: opts.Now, downloader: opts.Downloader, controller: opts.Controller,
		logger: opts.Logger, archive: opts.ArchivePath, checksums: opts.Checksums,
	}, nil
}

// Root reports the resolved install root.
func (a *Applier) Root() string { return a.root }

// Run recovers an interrupted transaction, then executes the queued request.
func (a *Applier) Run(ctx context.Context, request update.Request) (Result, error) {
	if err := a.Recover(ctx); err != nil {
		return Result{}, err
	}
	if err := a.validateRequest(request); err != nil {
		return Result{}, err
	}
	a.consumeRequest()
	switch request.Action {
	case "install":
		return a.install(ctx, request)
	case "rollback":
		return a.rollback(ctx, request)
	default:
		return Result{}, errors.New("apply: unsupported action")
	}
}

func (a *Applier) validateRequest(request update.Request) error {
	if request.Action != "install" && request.Action != "rollback" {
		return errors.New("apply: request action must be install or rollback")
	}
	version := strings.TrimPrefix(strings.TrimSpace(request.Version), "v")
	if !releaseVersion(version) {
		return fmt.Errorf("apply: invalid target version %q", request.Version)
	}
	if request.Action == "install" && !stableVersion(version) {
		return fmt.Errorf("apply: install target %q must be a stable release", request.Version)
	}
	if request.Tag != "v"+version {
		return fmt.Errorf("apply: tag %q does not match version %q", request.Tag, version)
	}
	if strings.TrimSpace(request.Root) != "" {
		root, err := secureDir(request.Root, "request root")
		if err != nil {
			return err
		}
		if !samePath(root, a.root) {
			return errors.New("apply: request root does not match executor root")
		}
	}
	return nil
}

func stableVersion(value string) bool {
	return corerelease.ValidateVersion(value) == nil && !strings.Contains(value, "-")
}

func releaseVersion(value string) bool {
	return corerelease.ValidateVersion(value) == nil && value != "0.0.0"
}

func (a *Applier) install(ctx context.Context, request update.Request) (Result, error) {
	version := strings.TrimPrefix(request.Version, "v")
	if err := os.MkdirAll(a.backupDir, 0o700); err != nil {
		return a.fail(version, "创建备份目录失败", err)
	}
	a.setStatus("downloading", version, "正在下载更新包")
	archivePath, checksumPath, cleanup, err := a.fetchRelease(ctx, version)
	if err != nil {
		return a.fail(version, "下载或校验更新包失败", err)
	}
	defer cleanup()

	a.setStatus("verifying", version, "正在校验更新包")
	staging, err := a.extractVerified(archivePath, checksumPath, version)
	if err != nil {
		return a.fail(version, "更新包校验失败", err)
	}
	defer os.RemoveAll(staging.Dir)

	a.setStatus("backing_up", version, "正在备份当前版本")
	backup, err := a.createBackup(staging.Manifest, version)
	if err != nil {
		return a.fail(version, "备份当前版本失败", err)
	}

	a.setStatus("stopping", version, "正在停止服务")
	if err := a.controller.Stop(ctx, request); err != nil {
		return a.fail(version, "停止服务失败", err)
	}

	a.setStatus("applying", version, "正在应用更新")
	if err := a.applyFiles(staging.Root, staging.Manifest.Files, backup); err != nil {
		_ = a.controller.Start(ctx, request)
		return a.fail(version, "应用更新失败，已回滚", err)
	}
	if err := a.writeInstalledManifest(staging); err != nil {
		_ = a.rollbackTo(ctx, request, backup)
		_ = a.controller.Start(ctx, request)
		return a.fail(version, "写入版本信息失败，已回滚", err)
	}

	a.setStatus("restarting", version, "正在启动服务")
	if err := a.controller.Start(ctx, request); err != nil {
		rollbackErr := a.rollbackTo(ctx, request, backup)
		if rollbackErr != nil {
			return a.fail(version, "启动失败且回滚失败", errors.Join(err, rollbackErr))
		}
		_ = a.controller.Start(ctx, request)
		return a.fail(version, "启动失败，已恢复到上一个版本", err)
	}

	a.setStatus("succeeded", version, "更新完成")
	a.pruneBackups()
	_ = a.publishBackups()
	return Result{State: "succeeded", Version: version, Message: "更新完成", BackupVersion: backup.Version, Asset: filepath.Base(archivePath)}, nil
}

func (a *Applier) rollback(ctx context.Context, request update.Request) (Result, error) {
	version := strings.TrimPrefix(request.Version, "v")
	a.setStatus("rolling_back", version, "正在准备回退")
	backup, err := a.findBackup(version)
	if err != nil {
		return a.fail(version, "没有可用的完整备份", err)
	}
	if err := a.verifyBackup(backup); err != nil {
		return a.fail(version, "备份校验失败", err)
	}
	a.setStatus("restoring", version, "正在恢复备份")
	if err := a.controller.Stop(ctx, request); err != nil {
		return a.fail(version, "停止服务失败", err)
	}
	current, _ := readInstalledManifest(a.root)
	if err := a.rollbackTo(ctx, request, backup); err != nil {
		_ = a.controller.Start(ctx, request)
		return a.fail(version, "回退失败", err)
	}
	if current != nil {
		// Remove files introduced by the newer release but absent from the
		// backup. The transaction already restored every backed-up file.
		_ = a.removeExtras(current.Files, backup)
	}
	if !backupHasManifest(backup) {
		_ = os.Remove(filepath.Join(a.root, corerelease.ManifestName))
		if releaseVersion(backup.Version) {
			if err := a.synthesizeManifest(backup); err != nil {
				_ = a.controller.Start(ctx, request)
				return a.fail(version, "回退后写入版本信息失败", err)
			}
		}
	}
	a.setStatus("restarting", version, "正在启动服务")
	if err := a.controller.Start(ctx, request); err != nil {
		return a.fail(version, "回退后启动失败", err)
	}
	a.setStatus("rolled_back", version, "回退完成")
	_ = a.publishBackups()
	return Result{State: "rolled_back", Version: version, Message: "回退完成", BackupVersion: backup.Version}, nil
}

func (a *Applier) rollbackTo(ctx context.Context, request update.Request, backup *backupRecord) error {
	a.setStatus("applying", backup.Version, "正在应用备份")
	entries := make([]corerelease.File, 0, len(backup.Files))
	for _, file := range backup.Files {
		if !file.Existed {
			continue
		}
		entries = append(entries, file.entry())
	}
	return a.applyFiles(backup.FilesDir(), entries, backup)
}

func (a *Applier) fail(version, message string, err error) (Result, error) {
	a.logger("update: " + message + ": " + err.Error())
	a.setStatus("failed", version, a.safeMessage(message))
	return Result{State: "failed", Version: version, Message: message}, err
}

func (a *Applier) safeMessage(message string) string {
	message = strings.ReplaceAll(message, a.root, "<root>")
	message = strings.ReplaceAll(message, a.backupDir, "<backups>")
	return truncate(message, 240)
}

// Recovery reports whether an interrupted transaction was found.
type Recovery struct {
	Found    bool   `json:"found"`
	Restored bool   `json:"restored"`
	Version  string `json:"version,omitempty"`
}

// Recover finishes or reverses an interrupted apply transaction.
func (a *Applier) Recover(ctx context.Context) error {
	_, err := a.RecoverOnce(ctx)
	return err
}

// RecoverOnce is Recover with an explicit no-op result for the CLI.
func (a *Applier) RecoverOnce(ctx context.Context) (Recovery, error) {
	journalPath := filepath.Join(a.root, JournalName)
	raw, err := os.ReadFile(journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return Recovery{Found: false}, nil
	}
	if err != nil {
		return Recovery{}, fmt.Errorf("apply: read journal: %w", err)
	}
	var journal transactionJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return Recovery{}, fmt.Errorf("apply: parse journal: %w", err)
	}
	if err := a.restoreJournal(&journal); err != nil {
		return Recovery{Found: true, Version: journal.Version}, fmt.Errorf("apply: recover interrupted update: %w", err)
	}
	if err := a.removeJournal(); err != nil {
		return Recovery{Found: true, Version: journal.Version}, err
	}
	a.setStatus("failed", journal.Version, "上次更新中断，已恢复原版本")
	a.logger("update: recovered interrupted transaction")
	return Recovery{Found: true, Restored: true, Version: journal.Version}, nil
}

func (a *Applier) fetchRelease(ctx context.Context, version string) (archivePath, checksumPath string, cleanup func(), err error) {
	asset := corerelease.AssetName(version, corerelease.KindStandard, a.goos, a.goarch, a.goarm)
	if a.archive != "" {
		if a.checksums == "" {
			return "", "", func() {}, errors.New("apply: --archive requires --checksums")
		}
		return a.archive, a.checksums, func() {}, nil
	}
	base := "https://github.com/" + a.repository + "/releases/download/v" + version + "/"
	dir, err := os.MkdirTemp(a.backupDir, ".download-*")
	if err != nil {
		return "", "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	archivePath = filepath.Join(dir, asset)
	checksumPath = filepath.Join(dir, corerelease.ChecksumsName)
	if err := a.downloader.Download(ctx, base+corerelease.ChecksumsName, checksumPath); err != nil {
		cleanup()
		return "", "", func() {}, fmt.Errorf("download checksums: %w", err)
	}
	if err := a.downloader.Download(ctx, base+asset, archivePath); err != nil {
		cleanup()
		return "", "", func() {}, fmt.Errorf("download asset: %w", err)
	}
	return archivePath, checksumPath, cleanup, nil
}

func (a *Applier) setStatus(state, version, message string) {
	if strings.TrimSpace(a.requestDir) == "" {
		return
	}
	payload := map[string]any{
		"state": state, "version": truncate(version, 32),
		"message":    truncate(message, 240),
		"updated_at": a.now().Format("2006-01-02 15:04:05"),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	path := filepath.Join(a.requestDir, update.StatusFile)
	if err := writeFileAtomic(path, raw, 0o600); err != nil {
		a.logger("update: write status: " + err.Error())
	}
}

func (a *Applier) consumeRequest() {
	if strings.TrimSpace(a.requestDir) == "" {
		return
	}
	_ = os.Remove(filepath.Join(a.requestDir, update.RequestFile))
}

func (a *Applier) writeInstalledManifest(staging *stagedRelease) error {
	raw, err := staging.Manifest.Marshal()
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.root, corerelease.ManifestName), raw, 0o644)
}

func backupHasManifest(backup *backupRecord) bool {
	for _, file := range backup.Files {
		if file.Existed && file.Path == corerelease.ManifestName {
			return true
		}
	}
	return false
}

func (a *Applier) synthesizeManifest(backup *backupRecord) error {
	manifest := corerelease.Manifest{
		Format: corerelease.Format, Version: backup.Version, Tag: corerelease.Tag(backup.Version),
		Commit: "rollback", Date: a.now().UTC().Format(time.RFC3339),
		GOOS: a.goos, GOARCH: a.goarch, GOARM: a.goarm,
		Kind: corerelease.KindStandard, Repository: corerelease.Repository,
	}
	for _, file := range backup.Files {
		if !file.Existed {
			continue
		}
		manifest.Files = append(manifest.Files, file.entry())
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	raw, err := manifest.Marshal()
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.root, corerelease.ManifestName), raw, 0o644)
}

func (a *Applier) removeExtras(current []corerelease.File, backup *backupRecord) error {
	kept := map[string]bool{}
	for _, file := range backup.Files {
		if file.Existed {
			kept[file.Path] = true
		}
	}
	for _, file := range current {
		if kept[file.Path] {
			continue
		}
		target, err := safeJoin(a.root, file.Path)
		if err != nil {
			continue
		}
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func secureDir(value, label string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("apply: empty %s", label)
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("apply: resolve %s: %w", label, err)
	}
	absolute = filepath.Clean(absolute)
	if absolute == string(filepath.Separator) || absolute == "." {
		return "", fmt.Errorf("apply: refusing %s %q", label, absolute)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("apply: %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("apply: %s %q is not a real directory", label, absolute)
	}
	return absolute, nil
}

func samePath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo)
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func safeJoin(root, relative string) (string, error) {
	if relative == "" || strings.Contains(relative, "\\") || strings.HasPrefix(relative, "/") {
		return "", fmt.Errorf("apply: unsafe path %q", relative)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("apply: unsafe path %q", relative)
	}
	target := filepath.Join(root, clean)
	parent := filepath.Dir(target)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if resolvedParent != resolvedRoot && !strings.HasPrefix(resolvedParent, resolvedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("apply: path %q escapes root", relative)
	}
	return target, nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	remove := func() { _ = os.Remove(tempName) }
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		remove()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		remove()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		remove()
		return err
	}
	if err := temp.Close(); err != nil {
		remove()
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		remove()
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func copyFileMode(source, dest string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func sortedBackups(records []*backupRecord) []*backupRecord {
	sort.Slice(records, func(i, j int) bool { return records[i].CreatedAt > records[j].CreatedAt })
	return records
}
