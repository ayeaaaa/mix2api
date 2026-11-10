// Package migrate copies a v1.6.5 data tree into a new v2 directory without
// modifying the source. It validates every JSON/JSONL/SQLite file it copies,
// takes a source snapshot before and after the copy, writes a checksum receipt,
// and publishes the destination with one atomic rename. The same engine powers
// rollback, which produces a v1-compatible directory from v2 data.
package migrate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	ReceiptFormat = "mix2api-migration/1"
	maxJSONBytes  = 64 << 20
	maxLineBytes  = 16 << 20
	lockSuffix    = ".migration.lock"
)

// Options configures one copy.
type Options struct {
	From    string
	To      string
	Receipt string
	DryRun  bool
	Now     func() time.Time
	Logger  func(string)

	// interrupt is a test seam: returning an error after N copied files must
	// leave no destination and no staging directory.
	interrupt func(path string, index int) error
}

// Summary is the machine-readable migration result.
type Summary struct {
	Format    string   `json:"format"`
	Direction string   `json:"direction"`
	Layout    string   `json:"layout"`
	Source    string   `json:"source"`
	Dest      string   `json:"dest"`
	DryRun    bool     `json:"dry_run"`
	Files     int      `json:"files"`
	Dirs      int      `json:"dirs"`
	Bytes     int64    `json:"bytes"`
	Accounts  int      `json:"accounts"`
	UsageRows int      `json:"usage_rows"`
	SQLite    int      `json:"sqlite_files"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Migrator holds one operation. interrupt is a test seam for proving that a
// failed copy never publishes a partial destination.
type Migrator struct {
	opts      Options
	layout    string
	dirs      []copyRoot
	interrupt func(path string, index int) error
}

type copyRoot struct {
	source string
	dest   string
}

type fileRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
}

type receipt struct {
	Format    string       `json:"format"`
	Direction string       `json:"direction"`
	Layout    string       `json:"layout"`
	Source    string       `json:"source"`
	Dest      string       `json:"dest"`
	CreatedAt string       `json:"created_at"`
	Files     []fileRecord `json:"files"`
}

type snapshotEntry struct {
	Hash string
	Mode os.FileMode
	Size int64
}

// Migrate copies a v1 data tree to a new destination.
func Migrate(opts Options) (Summary, error) { return run("migrate", opts) }

// Rollback copies current v2 data into a fresh v1-compatible directory.
func Rollback(opts Options) (Summary, error) { return run("rollback", opts) }

func run(direction string, opts Options) (Summary, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = func(string) {}
	}
	from, err := secureSource(opts.From)
	if err != nil {
		return Summary{}, err
	}
	to, err := cleanDestination(opts.To)
	if err != nil {
		return Summary{}, err
	}
	realFrom, err := canonicalPath(from)
	if err != nil {
		return Summary{}, err
	}
	realTo, err := canonicalPath(to)
	if err != nil {
		return Summary{}, err
	}
	if overlaps(realFrom, realTo) {
		return Summary{}, errors.New("migrate: source and destination overlap")
	}
	if opts.Receipt == "" {
		opts.Receipt = to + ".migration.json"
	}
	receiptPath, err := filepath.Abs(opts.Receipt)
	if err != nil {
		return Summary{}, err
	}
	realReceipt, err := canonicalPath(receiptPath)
	if err != nil {
		return Summary{}, err
	}
	if overlaps(realFrom, realReceipt) || overlaps(realTo, realReceipt) {
		return Summary{}, errors.New("migrate: receipt path overlaps data")
	}
	if info, err := os.Lstat(receiptPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return Summary{}, errors.New("migrate: receipt path must not be a symlink")
		}
		return Summary{}, errors.New("migrate: receipt already exists; choose a new path")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Summary{}, err
	}
	migrator := &Migrator{opts: opts, interrupt: opts.interrupt}
	if err := migrator.detectLayout(from); err != nil {
		return Summary{}, err
	}
	if err := ensureDestination(to); err != nil {
		return Summary{}, err
	}
	summary := Summary{
		Format: ReceiptFormat, Direction: direction, Layout: migrator.layout,
		Source: from, Dest: to, DryRun: opts.DryRun,
	}
	if opts.DryRun {
		before, err := snapshot(migrator.dirs)
		if err != nil {
			return Summary{}, err
		}
		if err := migrator.validateSource(before, &summary); err != nil {
			return Summary{}, err
		}
		summary.Files = len(before)
		for _, entry := range before {
			summary.Bytes += entry.Size
		}
		return summary, nil
	}

	lockPath := to + lockSuffix
	lock, err := acquireLock(lockPath, opts.Now())
	if err != nil {
		return Summary{}, err
	}
	defer releaseLock(lockPath, lock)

	before, err := snapshot(migrator.dirs)
	if err != nil {
		return Summary{}, err
	}
	summary.Files = len(before)
	for _, entry := range before {
		summary.Bytes += entry.Size
	}
	if err := migrator.validateSource(before, &summary); err != nil {
		return Summary{}, err
	}

	parent := filepath.Dir(to)
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(to)+".partial-*")
	if err != nil {
		return Summary{}, err
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	// Copy into staging, never directly into the final destination.
	migrator.opts.To = staging
	records := make([]fileRecord, 0, len(before))
	dirs := 0
	for index, relative := range sortedKeys(before) {
		if migrator.interrupt != nil {
			if err := migrator.interrupt(relative, index); err != nil {
				return Summary{}, fmt.Errorf("migrate: interrupted copy: %w", err)
			}
		}
		source, dest, err := migrator.mapPath(relative)
		if err != nil {
			return Summary{}, err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return Summary{}, err
		}
		if err := copyFile(source, dest, before[relative].Mode); err != nil {
			return Summary{}, err
		}
		sum, size, err := fileSHA256(dest)
		if err != nil {
			return Summary{}, err
		}
		if sum != before[relative].Hash || size != before[relative].Size {
			return Summary{}, fmt.Errorf("migrate: destination checksum mismatch for %s", relative)
		}
		records = append(records, fileRecord{
			Path: relative, SHA256: sum, Mode: uint32(before[relative].Mode.Perm()), Size: size,
		})
	}
	for _, root := range migrator.dirs {
		dirs += countDirs(root.source)
	}

	// Re-hash the source: a running v1 gateway must not produce a torn copy.
	after, err := snapshot(migrator.dirs)
	if err != nil {
		return Summary{}, err
	}
	if err := compareSnapshots(before, after); err != nil {
		return Summary{}, err
	}
	// Counts were collected from the source snapshot; the destination pass
	// re-validates content without double counting.
	summary.Accounts, summary.UsageRows, summary.SQLite = 0, 0, 0
	if err := validateTree(staging, migrator.layout, &summary); err != nil {
		return Summary{}, err
	}
	// Stage the receipt before publishing. If the receipt cannot be written,
	// the destination is not created at all.
	receiptTemp, err := stageReceipt(receiptPath, receipt{
		Format: ReceiptFormat, Direction: direction, Layout: migrator.layout,
		Source: from, Dest: to, CreatedAt: opts.Now().Format(time.RFC3339),
		Files: records,
	})
	if err != nil {
		return Summary{}, err
	}
	if err := publish(staging, to); err != nil {
		_ = os.Remove(receiptTemp)
		return Summary{}, err
	}
	removeStaging = false
	summary.Dirs = dirs
	if err := os.Rename(receiptTemp, receiptPath); err != nil {
		_ = os.Remove(receiptTemp)
		return summary, fmt.Errorf("migrate: destination published but receipt rename failed: %w", err)
	}
	if err := syncParent(receiptPath); err != nil {
		return summary, err
	}
	return summary, nil
}

func (m *Migrator) detectLayout(from string) error {
	info, err := os.Stat(filepath.Join(from, "accounts"))
	if err == nil && info.IsDir() {
		m.layout = "data"
		for _, name := range []string{"accounts", "usage", "media", "updates", "catalog"} {
			source := filepath.Join(from, name)
			if stat, err := os.Stat(source); err == nil && stat.IsDir() {
				m.dirs = append(m.dirs, copyRoot{source: source, dest: name})
			}
		}
		if len(m.dirs) == 0 {
			return errors.New("migrate: no data directories found")
		}
		return nil
	}
	if isAccountsTree(from) {
		m.layout = "accounts"
		m.dirs = []copyRoot{{source: from, dest: "."}}
		return nil
	}
	return errors.New("migrate: --from must be a data root containing accounts/ or a v1 accounts directory")
}

func isAccountsTree(path string) bool {
	for _, name := range []string{"settings.json", "raccoon", "codearts", "active_realm.json"} {
		if _, err := os.Stat(filepath.Join(path, name)); err == nil {
			return true
		}
	}
	return false
}

func secureSource(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("migrate: empty source")
	}
	absolute, err := filepath.Abs(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("migrate: source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("migrate: source must be a real directory")
	}
	return filepath.Clean(absolute), nil
}

func cleanDestination(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("migrate: empty destination")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if absolute == string(filepath.Separator) || absolute == "." {
		return "", errors.New("migrate: refusing filesystem root as destination")
	}
	if info, err := os.Lstat(absolute); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("migrate: destination must not be a symlink")
	}
	return absolute, nil
}

// canonicalPath resolves symlinks in the existing prefix of path. The final
// component may not exist (the destination is usually new).
func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	missing := []string{}
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("migrate: resolve path %s: %w", path, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func syncParent(path string) error {
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil
	}
	defer parent.Close()
	_ = parent.Sync()
	return nil
}

func ensureDestination(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("migrate: destination must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New("migrate: destination exists and is not a directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("migrate: destination is not empty; choose a new directory")
	}
	return nil
}

func publish(staging, dest string) error {
	if info, err := os.Lstat(dest); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("migrate: destination became a symlink")
		}
		if !info.IsDir() {
			return errors.New("migrate: destination is not a directory")
		}
		entries, err := os.ReadDir(dest)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			return fmt.Errorf("migrate: destination became non-empty: %s", strings.Join(names, ","))
		}
		if err := os.Remove(dest); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, dest); err != nil {
		return fmt.Errorf("migrate: publish destination: %w", err)
	}
	if parent, err := os.Open(filepath.Dir(dest)); err == nil {
		_ = parent.Sync()
		_ = parent.Close()
	}
	return nil
}

func (m *Migrator) mapPath(relative string) (source, dest string, err error) {
	slash := filepath.ToSlash(relative)
	for _, root := range m.dirs {
		prefix := ""
		sourceBase := root.source
		if root.dest != "." {
			prefix = root.dest + "/"
			if !strings.HasPrefix(slash, prefix) {
				continue
			}
			sourceBase = root.source
			slash = strings.TrimPrefix(slash, prefix)
		}
		source = filepath.Join(sourceBase, filepath.FromSlash(slash))
		dest = filepath.Join(m.opts.To, filepath.FromSlash(slash))
		if root.dest != "." {
			dest = filepath.Join(m.opts.To, filepath.FromSlash(root.dest), filepath.FromSlash(slash))
		}
		return source, dest, nil
	}
	return "", "", fmt.Errorf("migrate: path %q is outside the data roots", relative)
}

// stagingPath is used during validation; it maps a logical relative path to
// the staging directory instead of the final destination.
func (m *Migrator) stagingPath(staging, relative string) string {
	return filepath.Join(staging, filepath.FromSlash(relative))
}

func overlaps(left, right string) bool {
	return pathWithin(left, right) || pathWithin(right, left)
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func snapshot(roots []copyRoot) (map[string]snapshotEntry, error) {
	out := map[string]snapshotEntry{}
	for _, root := range roots {
		info, err := os.Lstat(root.source)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("migrate: %s is not a directory", root.source)
		}
		err = filepath.Walk(root.source, func(current string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if current == root.source {
				return nil
			}
			if info.IsDir() {
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("migrate: source contains a symlink: %s", current)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("migrate: source contains a non-regular file: %s", current)
			}
			relative, err := filepath.Rel(root.source, current)
			if err != nil {
				return err
			}
			logical := filepath.ToSlash(relative)
			if root.dest != "." {
				logical = root.dest + "/" + logical
			}
			hash, size, err := fileSHA256(current)
			if err != nil {
				return err
			}
			out[logical] = snapshotEntry{Hash: hash, Mode: info.Mode(), Size: size}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, errors.New("migrate: source contains no files")
	}
	return out, nil
}

func compareSnapshots(before, after map[string]snapshotEntry) error {
	if len(before) != len(after) {
		return errors.New("migrate: source changed while copying (file count differs)")
	}
	for path, first := range before {
		second, ok := after[path]
		if !ok || first != second {
			return fmt.Errorf("migrate: source changed while copying: %s", path)
		}
	}
	return nil
}

func (m *Migrator) validateSource(before map[string]snapshotEntry, summary *Summary) error {
	for relative := range before {
		slash := filepath.ToSlash(relative)
		var source string
		for _, root := range m.dirs {
			if root.dest == "." {
				source = filepath.Join(root.source, filepath.FromSlash(slash))
				break
			}
			prefix := root.dest + "/"
			if strings.HasPrefix(slash, prefix) {
				source = filepath.Join(root.source, filepath.FromSlash(strings.TrimPrefix(slash, prefix)))
				break
			}
		}
		if source == "" {
			return fmt.Errorf("migrate: cannot map %s", relative)
		}
		if err := validateFile(source, slash, m.layout, summary); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(source, dest string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	perm := mode.Perm()
	if perm&0o400 == 0 {
		perm |= 0o600
	}
	output, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		_ = os.Remove(dest)
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		_ = os.Remove(dest)
		return err
	}
	return output.Close()
}

func fileSHA256(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func countDirs(path string) int {
	count := 0
	_ = filepath.Walk(path, func(current string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && current != path {
			count++
		}
		return nil
	})
	return count
}

const lockStaleAfter = 24 * time.Hour

func acquireLock(path string, now time.Time) (*os.File, error) {
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(file, "pid=%d\nstarted=%s\n", os.Getpid(), now.Format(time.RFC3339))
			_ = file.Sync()
			return file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if !staleLock(path, now) {
			return nil, fmt.Errorf("migrate: destination is locked by %s", path)
		}
		_ = os.Remove(path)
	}
	return nil, fmt.Errorf("migrate: could not acquire destination lock %s", path)
}

func staleLock(path string, now time.Time) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	started := time.Time{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "started=") {
			started, _ = time.Parse(time.RFC3339, strings.TrimPrefix(line, "started="))
		}
	}
	if started.IsZero() {
		return true
	}
	return now.Sub(started) > lockStaleAfter
}

func releaseLock(path string, file *os.File) {
	if file != nil {
		_ = file.Close()
	}
	_ = os.Remove(path)
}

func stageReceipt(path string, value receipt) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	name := temp.Name()
	remove := func() { _ = os.Remove(name) }
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		remove()
		return "", err
	}
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		_ = temp.Close()
		remove()
		return "", err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		remove()
		return "", err
	}
	if err := temp.Close(); err != nil {
		remove()
		return "", err
	}
	return name, nil
}

func validateTree(root, layout string, summary *Summary) error {
	err := filepath.Walk(root, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("migrate: destination contains a non-regular file: %s", current)
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		return validateFile(current, filepath.ToSlash(relative), layout, summary)
	})
	return err
}

func validateFile(path, relative, layout string, summary *Summary) error {
	base := filepath.Base(relative)
	switch {
	case strings.HasSuffix(base, ".json"):
		if err := validateJSON(path); err != nil {
			return err
		}
	case strings.HasSuffix(base, ".jsonl"):
		rows, err := validateJSONL(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(base, "usage.jsonl") {
			summary.UsageRows += rows
		}
	case strings.HasSuffix(base, ".sqlite3") || strings.HasSuffix(base, ".sqlite") || strings.HasSuffix(base, ".db"):
		if err := validateSQLite(path); err != nil {
			return err
		}
		summary.SQLite++
	}
	if isAccountFile(layout, relative, base) {
		summary.Accounts++
	}
	return nil
}

func isAccountFile(layout, relative, base string) bool {
	if !strings.HasSuffix(base, ".json") || isKnownNonAccount(base) {
		return false
	}
	if layout == "accounts" {
		return true
	}
	return strings.HasPrefix(filepath.ToSlash(relative), "accounts/")
}

func isKnownNonAccount(name string) bool {
	switch name {
	case "settings.json", "active_realm.json", "model.json", "model_windows.json",
		"quota_scan.json", "integration-state.json", "state.json":
		return true
	}
	return strings.HasPrefix(name, "pricing-") || strings.HasPrefix(name, "usage-")
}

func validateJSON(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > maxJSONBytes {
		return fmt.Errorf("migrate: %s is too large to validate", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxJSONBytes+1))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("migrate: invalid JSON in %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("migrate: trailing data in %s", path)
	}
	return nil
}

func validateJSONL(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 256*1024)
	rows := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > maxLineBytes {
			return rows, fmt.Errorf("migrate: JSONL line too long in %s", path)
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var value any
				decoder := json.NewDecoder(bytes.NewReader(trimmed))
				decoder.UseNumber()
				if err := decoder.Decode(&value); err != nil {
					return rows, fmt.Errorf("migrate: invalid JSONL in %s at row %d: %w", path, rows+1, err)
				}
				var trailing any
				if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
					return rows, fmt.Errorf("migrate: trailing JSONL data in %s at row %d", path, rows+1)
				}
				rows++
			}
		} else if len(line) > 0 {
			// v1 ignores a trailing fragment without a newline. Accept it.
			summary := bytes.TrimSpace(line)
			if len(summary) > 0 {
				rows++
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return rows, nil
			}
			return rows, readErr
		}
	}
}

func validateSQLite(path string) error {
	urlPath := filepath.ToSlash(path)
	if volume := filepath.VolumeName(path); volume != "" && !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	dsn := (&url.URL{Scheme: "file", Path: urlPath, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("migrate: open sqlite %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("migrate: sqlite integrity check %s: %w", path, err)
	}
	if !strings.EqualFold(strings.TrimSpace(result), "ok") {
		return fmt.Errorf("migrate: sqlite integrity check failed for %s: %s", path, result)
	}
	return nil
}
