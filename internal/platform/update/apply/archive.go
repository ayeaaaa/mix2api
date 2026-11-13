package apply

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corerelease "github.com/ayeaaaa/mix2api-v2/internal/core/release"
)

type stagedRelease struct {
	Dir      string
	Root     string
	Manifest corerelease.Manifest
}

func (a *Applier) extractVerified(archivePath, checksumPath, version string) (*stagedRelease, error) {
	asset := filepath.Base(archivePath)
	checksumRaw, err := os.ReadFile(checksumPath)
	if err != nil {
		return nil, fmt.Errorf("read checksums: %w", err)
	}
	if len(checksumRaw) > 1<<20 {
		return nil, errors.New("checksum file is too large")
	}
	sums, err := corerelease.ParseChecksums(checksumRaw)
	if err != nil {
		return nil, err
	}
	want, ok := sums[asset]
	if !ok {
		return nil, fmt.Errorf("checksum file has no entry for %s", asset)
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxArchiveBytes {
		return nil, errors.New("archive is not a regular file within the size limit")
	}
	got, _, err := corerelease.FileSHA256(archivePath)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(got, want) {
		return nil, errors.New("archive checksum mismatch")
	}
	dir, err := os.MkdirTemp(a.backupDir, ".stage-*")
	if err != nil {
		return nil, err
	}
	extractDir := filepath.Join(dir, "extract")
	rootName, err := extractArchive(archivePath, extractDir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	root := filepath.Join(extractDir, rootName)
	rawManifest, err := os.ReadFile(filepath.Join(root, corerelease.ManifestName))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var manifest corerelease.Manifest
	if err := jsonUnmarshalStrict(rawManifest, &manifest); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := manifest.Validate(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if manifest.Version != version || manifest.Tag != corerelease.Tag(version) {
		_ = os.RemoveAll(dir)
		return nil, errors.New("manifest version does not match request")
	}
	if manifest.Kind != corerelease.KindStandard {
		_ = os.RemoveAll(dir)
		return nil, errors.New("manifest kind is not a standard update")
	}
	if !corerelease.PlatformMatches(manifest, a.goos, a.goarch, a.goarm) {
		_ = os.RemoveAll(dir)
		return nil, errors.New("manifest platform does not match this executor")
	}
	if err := verifyExtracted(root, manifest); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &stagedRelease{Dir: dir, Root: root, Manifest: manifest}, nil
}

// jsonUnmarshalStrict rejects unknown fields in the security-relevant
// manifest while leaving payload file contents untouched.
func jsonUnmarshalStrict(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("parse manifest: trailing data")
	}
	return nil
}

func verifyExtracted(root string, manifest corerelease.Manifest) error {
	listed := map[string]bool{}
	for _, file := range manifest.Files {
		if file.Path == corerelease.ManifestName {
			return errors.New("manifest must not list itself")
		}
		listed[file.Path] = true
		if err := corerelease.VerifyFile(root, file); err != nil {
			return err
		}
	}
	extras := make([]string, 0)
	err := filepath.Walk(root, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == corerelease.ManifestName {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive contains non-regular file %s", relative)
		}
		if !listed[relative] {
			extras = append(extras, relative)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(extras) != 0 {
		sort.Strings(extras)
		return fmt.Errorf("archive contains unlisted files: %s", strings.Join(extras, ", "))
	}
	return nil
}

func extractArchive(archivePath, dest string) (string, error) {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	if strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return extractZip(archivePath, dest)
	}
	if strings.HasSuffix(strings.ToLower(archivePath), ".tar.gz") || strings.HasSuffix(strings.ToLower(archivePath), ".tgz") {
		return extractTarGz(archivePath, dest)
	}
	return "", errors.New("unsupported archive format")
}

func extractTarGz(archivePath, dest string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return "", err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	root := ""
	count := 0
	total := int64(0)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		name, entryRoot, err := archiveRelativeName(header.Name)
		if err != nil {
			return "", err
		}
		if root == "" {
			root = entryRoot
		}
		if entryRoot != root {
			return "", errors.New("archive has multiple top-level roots")
		}
		if name == "" {
			if header.Typeflag != tar.TypeDir {
				return "", fmt.Errorf("archive top-level entry %s is not a directory", header.Name)
			}
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filepath.Join(dest, root, filepath.FromSlash(name)), 0o755); err != nil {
				return "", err
			}
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return "", fmt.Errorf("archive entry %s has unsupported type", header.Name)
		}
		if header.Mode&0o7000 != 0 {
			return "", fmt.Errorf("archive entry %s has setuid/setgid/sticky bits", header.Name)
		}
		count++
		if count > maxArchiveFiles {
			return "", errors.New("archive contains too many files")
		}
		if header.Size < 0 || header.Size > maxSingleFileSize {
			return "", errors.New("archive entry is too large")
		}
		total += header.Size
		if total > maxArchiveBytes {
			return "", errors.New("archive expands beyond the size limit")
		}
		info := header.FileInfo()
		if err := writeExtracted(filepath.Join(dest, root), name, reader, info.Mode().Perm(), header.Size); err != nil {
			return "", err
		}
	}
	if root == "" {
		return "", errors.New("archive is empty")
	}
	return root, nil
}

func extractZip(archivePath, dest string) (string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	root := ""
	count := 0
	total := int64(0)
	for _, entry := range reader.File {
		name, entryRoot, err := archiveRelativeName(entry.Name)
		if err != nil {
			return "", err
		}
		if root == "" {
			root = entryRoot
		}
		if entryRoot != root {
			return "", errors.New("archive has multiple top-level roots")
		}
		if name == "" {
			if !entry.FileInfo().IsDir() {
				return "", fmt.Errorf("archive top-level entry %s is not a directory", entry.Name)
			}
			continue
		}
		mode := entry.Mode()
		if mode&os.ModeSymlink != 0 {
			return "", fmt.Errorf("archive entry %s is a symlink", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(dest, root, filepath.FromSlash(name)), 0o755); err != nil {
				return "", err
			}
			continue
		}
		if !mode.IsRegular() {
			return "", fmt.Errorf("archive entry %s is not a regular file", entry.Name)
		}
		if mode.Perm()&0o7000 != 0 {
			return "", fmt.Errorf("archive entry %s has setuid/setgid/sticky bits", entry.Name)
		}
		count++
		if count > maxArchiveFiles {
			return "", errors.New("archive contains too many files")
		}
		if entry.UncompressedSize64 > uint64(maxSingleFileSize) {
			return "", errors.New("archive entry is too large")
		}
		total += int64(entry.UncompressedSize64)
		if total > maxArchiveBytes {
			return "", errors.New("archive expands beyond the size limit")
		}
		part, err := entry.Open()
		if err != nil {
			return "", err
		}
		writeErr := writeExtracted(filepath.Join(dest, root), name, part, mode.Perm(), int64(entry.UncompressedSize64))
		closeErr := part.Close()
		if writeErr != nil {
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	if root == "" {
		return "", errors.New("archive is empty")
	}
	return root, nil
}

func archiveRelativeName(raw string) (relative, root string, err error) {
	name := strings.TrimSpace(raw)
	name = strings.TrimPrefix(name, "./")
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", "", fmt.Errorf("unsafe archive path %q", raw)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("unsafe archive path %q", raw)
	}
	parts := strings.SplitN(clean, "/", 2)
	if parts[0] == "" {
		return "", "", fmt.Errorf("unsafe archive path %q", raw)
	}
	if len(parts) == 1 {
		return "", parts[0], nil
	}
	return parts[1], parts[0], nil
}

func writeExtracted(dest, relative string, source io.Reader, mode os.FileMode, size int64) error {
	target := filepath.Join(dest, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(source, size+1))
	if copyErr == nil && written != size {
		copyErr = fmt.Errorf("archive entry %s size %d, want %d", relative, written, size)
	}
	if copyErr != nil {
		_ = file.Close()
		return copyErr
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// HTTPDownloader fetches only fixed GitHub release URLs.
type HTTPDownloader struct {
	Client   *http.Client
	MaxBytes int64
}

// NewHTTPDownloader builds a downloader. A nil client uses a 2-minute timeout
// and a redirect policy that stays on HTTPS GitHub hosts.
func NewHTTPDownloader(client *http.Client, maxBytes int64) *HTTPDownloader {
	if client == nil {
		client = &http.Client{
			Timeout: 2 * time.Minute,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				if request.URL.Scheme != "https" {
					return errors.New("update download must stay on https")
				}
				host := strings.ToLower(request.URL.Hostname())
				if host != "github.com" && host != "objects.githubusercontent.com" &&
					host != "release-assets.githubusercontent.com" {
					return errors.New("update download redirected to an unexpected host")
				}
				return nil
			},
		}
	}
	if maxBytes <= 0 {
		maxBytes = maxArchiveBytes
	}
	return &HTTPDownloader{Client: client, MaxBytes: maxBytes}
}

func releaseURLAllowed(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return parsed.Scheme == "https" && parsed.Hostname() == "github.com" &&
		strings.HasPrefix(parsed.Path, "/"+corerelease.Repository+"/releases/download/")
}

// Download writes one validated GitHub release URL to dest.
func (d *HTTPDownloader) Download(ctx context.Context, rawURL, dest string) error {
	if !releaseURLAllowed(rawURL) {
		return fmt.Errorf("apply: refusing non-release URL %q", rawURL)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "mix2api-updater")
	response, err := d.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("apply: download status %d", response.StatusCode)
	}
	if response.ContentLength > d.MaxBytes {
		return errors.New("apply: download is too large")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, d.MaxBytes+1))
	if copyErr == nil && written > d.MaxBytes {
		copyErr = errors.New("apply: download exceeds size limit")
	}
	if copyErr != nil {
		_ = file.Close()
		_ = os.Remove(dest)
		return copyErr
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
