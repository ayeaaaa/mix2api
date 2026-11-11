// Package release defines the metadata shared by the release builder and the
// update executor. Keeping one schema prevents the updater from accepting an
// archive whose manifest was produced by a different naming convention.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// ManifestName is the per-archive manifest included at the archive root.
	ManifestName = "release-manifest.json"
	// ChecksumsName is the GitHub release checksum file.
	ChecksumsName = "SHA256SUMS"
	// Format is the manifest schema marker. It is intentionally distinct from
	// v1's Python portable manifest.
	Format = "mix2api-release/2"
	// Repository is the only repository the updater accepts.
	Repository = "ayeaaaa/mix2api"

	KindStandard = "standard"
	KindOpenWrt  = "openwrt"
	KindPortable = "portable"
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

// File is one payload entry inside an archive.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
}

// Manifest describes one release archive. It contains no filesystem paths
// outside the archive and no credentials.
type Manifest struct {
	Format     string `json:"format"`
	Version    string `json:"version"`
	Tag        string `json:"tag"`
	Commit     string `json:"commit"`
	Date       string `json:"date"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	GOARM      string `json:"goarm,omitempty"`
	Kind       string `json:"kind"`
	Repository string `json:"repository"`
	Files      []File `json:"files"`
}

// ValidateVersion accepts stable and prerelease semantic versions without a
// leading v. Build metadata is intentionally rejected so archive names stay
// unambiguous.
func ValidateVersion(value string) error {
	if !versionPattern.MatchString(strings.TrimSpace(value)) {
		return fmt.Errorf("release: invalid version %q", value)
	}
	return nil
}

// Tag returns the fixed GitHub tag for a version.
func Tag(version string) string { return "v" + strings.TrimPrefix(strings.TrimSpace(version), "v") }

// NormalizeGOARM accepts the GOARM spellings emitted by Go, release YAML, and
// docs ("7", "v7", "armv7") and returns the canonical digit family.
func NormalizeGOARM(value string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.TrimPrefix(normalized, "arm")
	normalized = strings.TrimPrefix(normalized, "v")
	switch normalized {
	case "5", "6", "7":
		return normalized, true
	default:
		return "", false
	}
}

// ArchLabel normalizes GOARCH/GOARM for asset names.
func ArchLabel(goarch, goarm string) string {
	goarch = strings.TrimSpace(goarch)
	if goarch == "arm" {
		if family, ok := NormalizeGOARM(goarm); ok {
			return "armv" + family
		}
	}
	return goarch
}

// AssetName returns the fixed release asset name. The updater deliberately
// reconstructs this name instead of trusting a URL from a request.
func AssetName(version, kind, goos, goarch, goarm string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	ext := ".tar.gz"
	if strings.EqualFold(goos, "windows") {
		ext = ".zip"
	}
	switch kind {
	case KindOpenWrt:
		return fmt.Sprintf("mix2api-%s-openwrt-%s%s", version, ArchLabel(goarch, goarm), ext)
	case KindPortable:
		return fmt.Sprintf("mix2api-%s-portable-%s-%s%s", version, goos, ArchLabel(goarch, goarm), ext)
	default:
		return fmt.Sprintf("mix2api-%s-%s-%s%s", version, goos, ArchLabel(goarch, goarm), ext)
	}
}

// ArchiveRoot is the single top-level directory inside an asset.
func ArchiveRoot(version, kind, goos, goarch, goarm string) string {
	name := AssetName(version, kind, goos, goarch, goarm)
	for _, suffix := range []string{".tar.gz", ".zip"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

// FileSHA256 hashes one file.
func FileSHA256(path string) (string, int64, error) {
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

// NewManifest builds a sorted manifest from archive payload paths. Paths are
// slash-separated and relative to the archive root.
func NewManifest(files []File) Manifest {
	out := append([]File(nil), files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return Manifest{Format: Format, Files: out}
}

// VerifyFile checks one file against a manifest entry.
func VerifyFile(root string, entry File) error {
	clean := path.Clean("/" + entry.Path)
	if strings.Contains(entry.Path, "\\") || clean != "/"+entry.Path || strings.HasPrefix(entry.Path, "../") {
		return fmt.Errorf("release: unsafe manifest path %q", entry.Path)
	}
	target := filepath.Join(root, filepath.FromSlash(entry.Path))
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("release: %s is not a regular file", entry.Path)
	}
	if entry.Size != 0 && info.Size() != entry.Size {
		return fmt.Errorf("release: %s size %d, want %d", entry.Path, info.Size(), entry.Size)
	}
	sum, _, err := FileSHA256(target)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, entry.SHA256) {
		return fmt.Errorf("release: %s checksum mismatch", entry.Path)
	}
	return nil
}

// ParseChecksums parses `sha256  name` lines, ignoring comments and blank
// lines. Names are reduced to their base name; duplicate conflicting entries
// are rejected.
func ParseChecksums(data []byte) (map[string]string, error) {
	out := map[string]string{}
	for lineNumber, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("release: malformed checksum line %d", lineNumber+1)
		}
		hash := strings.ToLower(fields[0])
		if len(hash) != 64 || !isHex(hash) {
			return nil, fmt.Errorf("release: malformed sha256 on line %d", lineNumber+1)
		}
		name := strings.TrimPrefix(fields[1], "*")
		name = filepath.Base(name)
		if name == "" || name == "." || name == ".." {
			return nil, fmt.Errorf("release: unsafe checksum name on line %d", lineNumber+1)
		}
		if existing, ok := out[name]; ok && existing != hash {
			return nil, fmt.Errorf("release: conflicting checksum for %s", name)
		}
		out[name] = hash
	}
	if len(out) == 0 {
		return nil, errors.New("release: empty checksum file")
	}
	return out, nil
}

// ValidateManifest checks the schema, version/tag agreement, fixed repository,
// and path safety before any file is extracted.
func (m Manifest) Validate() error {
	if m.Format != Format {
		return fmt.Errorf("release: unsupported manifest format %q", m.Format)
	}
	if err := ValidateVersion(m.Version); err != nil {
		return err
	}
	if m.Tag != Tag(m.Version) {
		return fmt.Errorf("release: tag %q does not match version %q", m.Tag, m.Version)
	}
	if m.Repository != Repository {
		return fmt.Errorf("release: repository %q is not allowed", m.Repository)
	}
	if m.GOOS == "" || m.GOARCH == "" {
		return errors.New("release: manifest is missing platform")
	}
	if m.GOARCH == "arm" {
		if _, ok := NormalizeGOARM(m.GOARM); !ok {
			return fmt.Errorf("release: invalid GOARM %q", m.GOARM)
		}
	} else if strings.TrimSpace(m.GOARM) != "" {
		return fmt.Errorf("release: GOARM must be empty for GOARCH %s", m.GOARCH)
	}
	if len(m.Files) == 0 {
		return errors.New("release: manifest has no files")
	}
	seen := map[string]bool{}
	for _, entry := range m.Files {
		if entry.Path == "" || strings.Contains(entry.Path, "\\") || strings.HasPrefix(entry.Path, "/") {
			return fmt.Errorf("release: unsafe manifest path %q", entry.Path)
		}
		clean := path.Clean(entry.Path)
		if clean != entry.Path || clean == "." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("release: unsafe manifest path %q", entry.Path)
		}
		if len(entry.SHA256) != 64 || !isHex(entry.SHA256) {
			return fmt.Errorf("release: invalid sha256 for %q", entry.Path)
		}
		if seen[entry.Path] {
			return fmt.Errorf("release: duplicate manifest path %q", entry.Path)
		}
		seen[entry.Path] = true
	}
	return nil
}

// PlatformMatches reports whether a manifest targets the executor platform.
// ARM families accept 7, v7, and armv7 as the same target.
func PlatformMatches(m Manifest, goos, goarch, goarm string) bool {
	if m.GOOS != goos || m.GOARCH != goarch {
		return false
	}
	if goarch == "arm" {
		left, leftOK := NormalizeGOARM(m.GOARM)
		right, rightOK := NormalizeGOARM(goarm)
		return leftOK && rightOK && left == right
	}
	return strings.TrimSpace(m.GOARM) == ""
}

// Marshal renders a manifest with a trailing newline.
func (m Manifest) Marshal() ([]byte, error) {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func isHex(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
