// Command release builds the reproducible mix2api release asset set.
//
// It is intentionally standard-library only and works offline once dependencies
// are in the module cache. Outputs are deterministic for a fixed version,
// commit, date, and toolchain: builds use -trimpath and -buildvcs=false, the
// linker build id is cleared, and archives use fixed timestamps, owners, and
// sorted entries.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	corerelease "github.com/ayeaaaa/mix2api-v2/internal/core/release"
	"github.com/ayeaaaa/mix2api-v2/internal/core/version"
)

const binaryName = "mix2api"

type target struct {
	Name   string
	GOOS   string
	GOARCH string
	GOARM  string
	Kind   string
}

func (t target) archiveFormat() string {
	if t.GOOS == "windows" {
		return "zip"
	}
	return "tar.gz"
}

func (t target) assetName(version string) string {
	return corerelease.AssetName(version, t.Kind, t.GOOS, t.GOARCH, t.GOARM)
}

func (t target) root(version string) string {
	return corerelease.ArchiveRoot(version, t.Kind, t.GOOS, t.GOARCH, t.GOARM)
}

// defaultTargets is the v2 release matrix. Linux amd64/arm64/armv7 also ship
// as OpenWrt archives; portable archives add launcher scripts around the same
// verified binary.
func defaultTargets() []target {
	targets := []target{
		{Name: "linux-amd64", GOOS: "linux", GOARCH: "amd64", Kind: corerelease.KindStandard},
		{Name: "linux-arm64", GOOS: "linux", GOARCH: "arm64", Kind: corerelease.KindStandard},
		{Name: "linux-armv7", GOOS: "linux", GOARCH: "arm", GOARM: "7", Kind: corerelease.KindStandard},
		{Name: "windows-amd64", GOOS: "windows", GOARCH: "amd64", Kind: corerelease.KindStandard},
		{Name: "darwin-amd64", GOOS: "darwin", GOARCH: "amd64", Kind: corerelease.KindStandard},
		{Name: "darwin-arm64", GOOS: "darwin", GOARCH: "arm64", Kind: corerelease.KindStandard},
		{Name: "openwrt-amd64", GOOS: "linux", GOARCH: "amd64", Kind: corerelease.KindOpenWrt},
		{Name: "openwrt-arm64", GOOS: "linux", GOARCH: "arm64", Kind: corerelease.KindOpenWrt},
		{Name: "openwrt-armv7", GOOS: "linux", GOARCH: "arm", GOARM: "7", Kind: corerelease.KindOpenWrt},
		{Name: "portable-linux-amd64", GOOS: "linux", GOARCH: "amd64", Kind: corerelease.KindPortable},
		{Name: "portable-windows-amd64", GOOS: "windows", GOARCH: "amd64", Kind: corerelease.KindPortable},
	}
	return targets
}

type options struct {
	Out        string
	RepoRoot   string
	Version    string
	Commit     string
	Date       string
	Targets    stringList
	List       bool
	DryRun     bool
	SkipBuild  bool
	SourceDate string
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			*s = append(*s, trimmed)
		}
	}
	return nil
}

func main() {
	if code := run(os.Args[1:], os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "release: "+err.Error())
		return 2
	}
	if err := opts.resolve(stderr); err != nil {
		fmt.Fprintln(stderr, "release: "+err.Error())
		return 2
	}
	targets, err := selectTargets(opts.Targets)
	if err != nil {
		fmt.Fprintln(stderr, "release: "+err.Error())
		return 2
	}
	if opts.List {
		for _, item := range targets {
			fmt.Fprintf(stdout, "%s\t%s/%s%s\t%s\t%s\n", item.Name, item.GOOS, item.GOARCH,
				armSuffix(item.GOARM), item.Kind, item.assetName(opts.Version))
		}
		return 0
	}
	if opts.DryRun {
		fmt.Fprintf(stdout, "version=%s commit=%s date=%s archive_date=%s out=%s\n", opts.Version, opts.Commit, opts.Date, opts.SourceDate, opts.Out)
		for _, item := range targets {
			fmt.Fprintf(stdout, "%s -> %s\n", item.Name, item.assetName(opts.Version))
		}
		return 0
	}
	assets, err := buildRelease(opts, targets, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "release: "+err.Error())
		return 1
	}
	for _, asset := range assets {
		fmt.Fprintln(stdout, asset)
	}
	return 0
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	opts := options{Out: "dist"}
	flags := flag.NewFlagSet("release", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&opts.Out, "out", opts.Out, "output directory")
	flags.StringVar(&opts.RepoRoot, "repo-root", "", "repository root (default: current directory)")
	flags.StringVar(&opts.Version, "version", "", "semantic version without v (default: build default)")
	flags.StringVar(&opts.Commit, "commit", "", "source commit (default: git HEAD)")
	flags.StringVar(&opts.Date, "date", "", "build date RFC3339 (default: SOURCE_DATE_EPOCH or now)")
	flags.Var(&opts.Targets, "target", "target name or comma list (repeatable)")
	flags.BoolVar(&opts.List, "list", false, "list selected targets and exit")
	flags.BoolVar(&opts.DryRun, "dry-run", false, "print the plan and exit")
	flags.BoolVar(&opts.SkipBuild, "skip-build", false, "reuse binaries under <out>/.build (development only)")
	flags.StringVar(&opts.SourceDate, "archive-date", "", "archive entry timestamp RFC3339 (default: SOURCE_DATE_EPOCH or 1980-01-01)")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return opts, nil
}

func (o *options) resolve(stderr io.Writer) error {
	if strings.TrimSpace(o.RepoRoot) == "" {
		root, err := findRepoRoot()
		if err != nil {
			return err
		}
		o.RepoRoot = root
	}
	absoluteRoot, err := filepath.Abs(o.RepoRoot)
	if err != nil {
		return err
	}
	o.RepoRoot = absoluteRoot
	if strings.TrimSpace(o.Version) == "" {
		o.Version = strings.TrimPrefix(version.DefaultVersion, "v")
	}
	o.Version = strings.TrimPrefix(strings.TrimSpace(o.Version), "v")
	if err := corerelease.ValidateVersion(o.Version); err != nil {
		return err
	}
	if strings.TrimSpace(o.Commit) == "" {
		o.Commit = gitCommit(o.RepoRoot)
	}
	if strings.TrimSpace(o.Date) == "" {
		o.Date = defaultBuildDate()
	}
	parsed, ok := version.ParseDate(o.Date)
	if !ok {
		return fmt.Errorf("invalid --date %q (want RFC3339)", o.Date)
	}
	o.Date = parsed.Format(time.RFC3339)
	if strings.TrimSpace(o.SourceDate) == "" {
		o.SourceDate, err = defaultArchiveDate()
		if err != nil {
			return err
		}
	}
	if _, ok := version.ParseDate(o.SourceDate); !ok {
		return fmt.Errorf("invalid --archive-date %q", o.SourceDate)
	}
	if strings.TrimSpace(o.Out) == "" {
		return errors.New("empty --out")
	}
	absoluteOut, err := filepath.Abs(o.Out)
	if err != nil {
		return err
	}
	o.Out = absoluteOut
	if o.Commit == "" {
		o.Commit = "unknown"
		fmt.Fprintln(stderr, "release: warning: commit is unknown (not a git checkout)")
	}
	return nil
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found; pass --repo-root")
		}
		dir = parent
	}
}

func gitCommit(root string) string {
	command := exec.Command("git", "-C", root, "rev-parse", "HEAD")
	output, err := command.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

func defaultBuildDate() string {
	if value := strings.TrimSpace(os.Getenv("SOURCE_DATE_EPOCH")); value != "" {
		if seconds, err := parseEpoch(value); err == nil {
			return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
		}
	}
	return time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
}

// defaultArchiveDate maps SOURCE_DATE_EPOCH to archive entry timestamps. A
// fixed 1980 epoch keeps output deterministic when the variable is absent.
// An explicitly invalid epoch is an error rather than silently choosing 1980,
// because the operator asked for reproducible timestamps.
func defaultArchiveDate() (string, error) {
	value := strings.TrimSpace(os.Getenv("SOURCE_DATE_EPOCH"))
	if value == "" {
		return "1980-01-01T00:00:00Z", nil
	}
	seconds, err := parseEpoch(value)
	if err != nil {
		return "", fmt.Errorf("invalid SOURCE_DATE_EPOCH %q", value)
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339), nil
}

func parseEpoch(value string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
}

func selectTargets(names []string) ([]target, error) {
	all := defaultTargets()
	if len(names) == 0 {
		return all, nil
	}
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	out := make([]target, 0, len(names))
	for _, item := range all {
		if wanted[item.Name] {
			out = append(out, item)
			delete(wanted, item.Name)
		}
	}
	if len(wanted) != 0 {
		missing := make([]string, 0, len(wanted))
		for name := range wanted {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("unknown target(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func ldflags(versionValue, commit, date string) string {
	return strings.Join([]string{
		"-s", "-w", "-buildid=",
		"-X", "github.com/ayeaaaa/mix2api-v2/internal/core/version.Version=" + versionValue,
		"-X", "github.com/ayeaaaa/mix2api-v2/internal/core/version.Commit=" + commit,
		"-X", "github.com/ayeaaaa/mix2api-v2/internal/core/version.Date=" + date,
	}, " ")
}

type platformKey struct{ GOOS, GOARCH, GOARM string }

func buildRelease(opts options, targets []target, stderr io.Writer) ([]string, error) {
	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return nil, err
	}
	buildDir := filepath.Join(opts.Out, ".build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return nil, err
	}
	binaries := map[platformKey]string{}
	for _, item := range targets {
		key := platformKey{item.GOOS, item.GOARCH, item.GOARM}
		if _, ok := binaries[key]; ok {
			continue
		}
		dest := filepath.Join(buildDir, binaryName+"-"+item.GOOS+"-"+item.GOARCH+armSuffix(item.GOARM))
		if !opts.SkipBuild {
			if err := buildBinary(opts, key, dest); err != nil {
				return nil, err
			}
		} else if _, err := os.Stat(dest); err != nil {
			return nil, fmt.Errorf("--skip-build: %w", err)
		}
		binaries[key] = dest
	}

	assets := make([]string, 0, len(targets))
	checksums := map[string]string{}
	for _, item := range targets {
		binary := binaries[platformKey{item.GOOS, item.GOARCH, item.GOARM}]
		archivePath, err := packageTarget(opts, item, binary)
		if err != nil {
			return nil, err
		}
		sum, _, err := corerelease.FileSHA256(archivePath)
		if err != nil {
			return nil, err
		}
		checksums[filepath.Base(archivePath)] = sum
		assets = append(assets, archivePath)
	}
	sbomPath := filepath.Join(opts.Out, "sbom.spdx.json")
	if err := writeSBOM(sbomPath, opts, firstBinary(binaries)); err != nil {
		return nil, err
	}
	summaryPath := filepath.Join(opts.Out, "release.json")
	if err := writeSummary(summaryPath, opts, checksums); err != nil {
		return nil, err
	}
	// SHA256SUMS covers every generated deliverable except itself.
	for _, extra := range []string{sbomPath, summaryPath} {
		sum, _, err := corerelease.FileSHA256(extra)
		if err != nil {
			return nil, err
		}
		checksums[filepath.Base(extra)] = sum
	}
	checksumPath := filepath.Join(opts.Out, corerelease.ChecksumsName)
	if err := writeChecksums(checksumPath, checksums); err != nil {
		return nil, err
	}
	assets = append(assets, sbomPath, summaryPath, checksumPath)
	sort.Strings(assets)
	fmt.Fprintf(stderr, "release: wrote %d assets to %s\n", len(assets), opts.Out)
	return assets, nil
}

func firstBinary(binaries map[platformKey]string) string {
	keys := make([]string, 0, len(binaries))
	byName := map[string]string{}
	for key, path := range binaries {
		name := key.GOOS + "-" + key.GOARCH + armSuffix(key.GOARM)
		keys = append(keys, name)
		byName[name] = path
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return byName[keys[0]]
}

func buildBinary(opts options, key platformKey, dest string) error {
	args := []string{
		"build",
		"-trimpath",
		"-buildvcs=false",
		"-buildmode=exe",
		"-ldflags", ldflags(opts.Version, opts.Commit, opts.Date),
		"-o", dest,
		"./cmd/mix2api",
	}
	command := exec.Command("go", args...)
	command.Dir = opts.RepoRoot
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+key.GOOS, "GOARCH="+key.GOARCH)
	if key.GOARM != "" {
		command.Env = append(command.Env, "GOARM="+key.GOARM)
	} else {
		command.Env = append(command.Env, "GOARM=")
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build %s/%s: %w\n%s", key.GOOS, key.GOARCH, err, strings.TrimSpace(string(output)))
	}
	return nil
}

type archiveEntry struct {
	Name   string
	Source string
	Mode   uint32
}

func packageTarget(opts options, item target, binary string) (string, error) {
	entries, err := payloadEntries(opts, item, binary)
	if err != nil {
		return "", err
	}
	manifest := corerelease.NewManifest(nil)
	manifest.Version = opts.Version
	manifest.Tag = corerelease.Tag(opts.Version)
	manifest.Commit = opts.Commit
	manifest.Date = opts.Date
	manifest.GOOS = item.GOOS
	manifest.GOARCH = item.GOARCH
	manifest.GOARM = item.GOARM
	manifest.Kind = item.Kind
	manifest.Repository = corerelease.Repository
	for _, entry := range entries {
		sum, size, err := corerelease.FileSHA256(entry.Source)
		if err != nil {
			return "", err
		}
		manifest.Files = append(manifest.Files, corerelease.File{
			Path: entry.Name, SHA256: sum, Mode: entry.Mode, Size: size,
		})
	}
	if err := manifest.Validate(); err != nil {
		return "", err
	}
	raw, err := manifest.Marshal()
	if err != nil {
		return "", err
	}
	manifestPath := filepath.Join(filepath.Dir(binary), "manifest-"+item.Name+".json")
	if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
		return "", err
	}
	entries = append(entries, archiveEntry{Name: corerelease.ManifestName, Source: manifestPath, Mode: 0o644})

	archivePath := filepath.Join(opts.Out, item.assetName(opts.Version))
	archiveDate, _ := version.ParseDate(opts.SourceDate)
	if err := writeArchive(archivePath, item.archiveFormat(), item.root(opts.Version), entries, archiveDate); err != nil {
		return "", err
	}
	return archivePath, nil
}

func payloadEntries(opts options, item target, binary string) ([]archiveEntry, error) {
	binaryEntry := binaryName
	if item.GOOS == "windows" {
		binaryEntry += ".exe"
	}
	entries := []archiveEntry{
		{Name: binaryEntry, Source: binary, Mode: 0o755},
		{Name: "LICENSE", Source: filepath.Join(opts.RepoRoot, "LICENSE"), Mode: 0o644},
		{Name: "README.md", Source: filepath.Join(opts.RepoRoot, "README.md"), Mode: 0o644},
	}
	if item.Kind == corerelease.KindStandard && item.GOOS == "linux" {
		entries = append(entries, archiveEntry{
			Name: "mix2api.service", Source: filepath.Join(opts.RepoRoot, "packaging", "systemd", "mix2api.service"), Mode: 0o644,
		})
	}
	switch item.Kind {
	case corerelease.KindOpenWrt:
		entries = append(entries,
			archiveEntry{Name: "etc/init.d/mix2api", Source: filepath.Join(opts.RepoRoot, "packaging", "openwrt", "files", "mix2api.init"), Mode: 0o755},
			archiveEntry{Name: "etc/config/mix2api", Source: filepath.Join(opts.RepoRoot, "packaging", "openwrt", "files", "mix2api.config"), Mode: 0o644},
			archiveEntry{Name: "OPENWRT.md", Source: filepath.Join(opts.RepoRoot, "packaging", "openwrt", "README.md"), Mode: 0o644},
		)
	case corerelease.KindPortable:
		entries = append(entries,
			archiveEntry{Name: "start-mix2api.sh", Source: filepath.Join(opts.RepoRoot, "packaging", "portable", "start-mix2api.sh"), Mode: 0o755},
			archiveEntry{Name: "stop-mix2api.sh", Source: filepath.Join(opts.RepoRoot, "packaging", "portable", "stop-mix2api.sh"), Mode: 0o755},
			archiveEntry{Name: "start-mix2api.cmd", Source: filepath.Join(opts.RepoRoot, "packaging", "portable", "start-mix2api.cmd"), Mode: 0o644},
			archiveEntry{Name: "stop-mix2api.cmd", Source: filepath.Join(opts.RepoRoot, "packaging", "portable", "stop-mix2api.cmd"), Mode: 0o644},
			archiveEntry{Name: "PORTABLE.md", Source: filepath.Join(opts.RepoRoot, "packaging", "portable", "README.md"), Mode: 0o644},
		)
	}
	for _, entry := range entries {
		info, err := os.Stat(entry.Source)
		if err != nil {
			return nil, fmt.Errorf("package %s: %w", item.Name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("package %s: %s is not a regular file", item.Name, entry.Source)
		}
	}
	return entries, nil
}

func writeArchive(dest, format, root string, entries []archiveEntry, entryDate time.Time) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	temp := dest + ".tmp-" + fmt.Sprint(os.Getpid())
	file, err := os.Create(temp)
	if err != nil {
		return err
	}
	remove := func() { _ = os.Remove(temp) }
	sorted := append([]archiveEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	switch format {
	case "zip":
		writer := zip.NewWriter(file)
		for _, entry := range sorted {
			header := &zip.FileHeader{
				Name:     path.Join(root, entry.Name),
				Method:   zip.Deflate,
				Modified: entryDate,
			}
			header.SetMode(os.FileMode(entry.Mode))
			part, err := writer.CreateHeader(header)
			if err != nil {
				_ = writer.Close()
				_ = file.Close()
				remove()
				return err
			}
			if err := copyFile(part, entry.Source); err != nil {
				_ = writer.Close()
				_ = file.Close()
				remove()
				return err
			}
		}
		if err := writer.Close(); err != nil {
			_ = file.Close()
			remove()
			return err
		}
	case "tar.gz":
		gzipWriter, err := gzip.NewWriterLevel(file, gzip.BestCompression)
		if err != nil {
			_ = file.Close()
			remove()
			return err
		}
		gzipWriter.Header.ModTime = entryDate
		gzipWriter.Header.OS = 255
		tarWriter := tar.NewWriter(gzipWriter)
		for _, entry := range sorted {
			info, err := os.Stat(entry.Source)
			if err != nil {
				_ = tarWriter.Close()
				_ = gzipWriter.Close()
				_ = file.Close()
				remove()
				return err
			}
			header := &tar.Header{
				Name:     path.Join(root, entry.Name),
				Mode:     int64(entry.Mode),
				Size:     info.Size(),
				ModTime:  entryDate,
				Uid:      0,
				Gid:      0,
				Uname:    "",
				Gname:    "",
				Format:   tar.FormatUSTAR,
				Typeflag: tar.TypeReg,
			}
			if err := tarWriter.WriteHeader(header); err != nil {
				_ = tarWriter.Close()
				_ = gzipWriter.Close()
				_ = file.Close()
				remove()
				return err
			}
			if err := copyFile(tarWriter, entry.Source); err != nil {
				_ = tarWriter.Close()
				_ = gzipWriter.Close()
				_ = file.Close()
				remove()
				return err
			}
		}
		if err := tarWriter.Close(); err != nil {
			_ = gzipWriter.Close()
			_ = file.Close()
			remove()
			return err
		}
		if err := gzipWriter.Close(); err != nil {
			_ = file.Close()
			remove()
			return err
		}
	default:
		_ = file.Close()
		remove()
		return fmt.Errorf("unsupported archive format %q", format)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		remove()
		return err
	}
	if err := file.Close(); err != nil {
		remove()
		return err
	}
	if err := os.Rename(temp, dest); err != nil {
		remove()
		return err
	}
	return nil
}

func copyFile(dst io.Writer, source string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(dst, file)
	return err
}

func writeChecksums(dest string, sums map[string]string) error {
	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	for _, name := range names {
		fmt.Fprintf(&builder, "%s  %s\n", sums[name], name)
	}
	return os.WriteFile(dest, []byte(builder.String()), 0o644)
}

type spdxPackage struct {
	SPDXID           string `json:"SPDXID"`
	Name             string `json:"name"`
	VersionInfo      string `json:"versionInfo,omitempty"`
	DownloadLocation string `json:"downloadLocation"`
	FilesAnalyzed    bool   `json:"filesAnalyzed"`
	LicenseConcluded string `json:"licenseConcluded"`
	LicenseDeclared  string `json:"licenseDeclared"`
	CopyrightText    string `json:"copyrightText"`
}

type spdxDocument struct {
	SPDXID            string        `json:"SPDXID"`
	SPDXVersion       string        `json:"spdxVersion"`
	CreationInfo      spdxCreation  `json:"creationInfo"`
	Name              string        `json:"name"`
	DataLicense       string        `json:"dataLicense"`
	DocumentNamespace string        `json:"documentNamespace"`
	Packages          []spdxPackage `json:"packages"`
}

type spdxCreation struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

func writeSBOM(dest string, opts options, binary string) error {
	document := spdxDocument{
		SPDXID:      "SPDXRef-DOCUMENT",
		SPDXVersion: "SPDX-2.3",
		CreationInfo: spdxCreation{
			Created:  opts.Date,
			Creators: []string{"Tool: mix2api-release-" + opts.Version},
		},
		Name:              "mix2api-" + opts.Version,
		DataLicense:       "CC0-1.0",
		DocumentNamespace: "https://github.com/" + corerelease.Repository + "/spdx/" + corerelease.Tag(opts.Version) + "/" + opts.Commit,
	}
	document.Packages = append(document.Packages, spdxPackage{
		SPDXID: "SPDXRef-Package-mix2api", Name: "github.com/ayeaaaa/mix2api-v2",
		VersionInfo: opts.Version, DownloadLocation: "NOASSERTION", FilesAnalyzed: false,
		LicenseConcluded: "NOASSERTION", LicenseDeclared: "MIT", CopyrightText: "NOASSERTION",
	})
	if binary != "" {
		if info, err := buildinfo.ReadFile(binary); err == nil {
			deps := append([]*debug.Module(nil), info.Deps...)
			sort.Slice(deps, func(i, j int) bool { return deps[i].Path < deps[j].Path })
			seen := map[string]bool{}
			for _, dep := range deps {
				if dep == nil || seen[dep.Path] {
					continue
				}
				seen[dep.Path] = true
				name := dep.Path
				versionValue := dep.Version
				if dep.Replace != nil {
					name = dep.Replace.Path
					versionValue = dep.Replace.Version
				}
				document.Packages = append(document.Packages, spdxPackage{
					SPDXID: "SPDXRef-Package-" + sanitizeSPDX(name), Name: name,
					VersionInfo: versionValue, DownloadLocation: "NOASSERTION", FilesAnalyzed: false,
					LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION", CopyrightText: "NOASSERTION",
				})
			}
		}
	}
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dest, append(raw, '\n'), 0o644)
}

func sanitizeSPDX(value string) string {
	replacer := strings.NewReplacer("/", "-", "@", "-", ".", "-", "_", "-", "+", "-", " ", "-")
	return replacer.Replace(value)
}

type releaseSummary struct {
	Format  string            `json:"format"`
	Version string            `json:"version"`
	Tag     string            `json:"tag"`
	Commit  string            `json:"commit"`
	Date    string            `json:"date"`
	Assets  map[string]string `json:"assets"`
}

func writeSummary(dest string, opts options, sums map[string]string) error {
	raw, err := json.MarshalIndent(releaseSummary{
		Format: corerelease.Format, Version: opts.Version, Tag: corerelease.Tag(opts.Version),
		Commit: opts.Commit, Date: opts.Date, Assets: sums,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dest, append(raw, '\n'), 0o644)
}

func armSuffix(goarm string) string {
	if strings.TrimSpace(goarm) == "" {
		return ""
	}
	return "v" + goarm
}
