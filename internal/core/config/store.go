// Package config provides the persistent JSON settings store.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Store is a JSON settings file at Path.
type Store struct {
	Path string
}

// New returns a Store backed by path.
func New(path string) *Store {
	return &Store{Path: path}
}

// Load reads the settings file as a JSON object. A missing or malformed file
// yields an empty map and no error. Numbers are decoded as json.Number so they
// round-trip without precision loss.
func (s *Store) Load() (map[string]any, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	defer f.Close()

	settings, err := decodeSettings(f)
	if err != nil {
		return map[string]any{}, nil
	}
	return settings, nil
}

func decodeSettings(r io.Reader) (map[string]any, error) {
	settings := map[string]any{}
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&settings); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("config: trailing data after JSON object")
	}
	if settings == nil {
		return map[string]any{}, nil
	}
	return settings, nil
}

// Merge recursively merges patch into the current settings, writes the result
// atomically, and returns the merged settings. Map values are merged; all
// other values, including arrays, replace the previous value. Keys absent from
// patch are preserved, including unknown nested keys.
func (s *Store) Merge(patch map[string]any) (map[string]any, error) {
	current, err := s.Load()
	if err != nil {
		return nil, err
	}
	merged := mergeMaps(current, patch)
	if err := s.write(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func mergeMaps(dst, patch map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for key, value := range patch {
		patchMap, patchIsMap := value.(map[string]any)
		dstMap, dstIsMap := dst[key].(map[string]any)
		if patchIsMap && dstIsMap {
			dst[key] = mergeMaps(dstMap, patchMap)
			continue
		}
		dst[key] = value
	}
	return dst
}

// write replaces Path with settings atomically: the data is written to a
// temporary file in the same directory, synced, closed, and renamed over the
// target. The resulting file is mode 0600 on POSIX systems.
func (s *Store) write(settings map[string]any) error {
	dir := filepath.Dir(s.Path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(s.Path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: create temp file: %w", err)
	}
	tmp := f.Name()
	remove := func() { _ = os.Remove(tmp) }

	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		remove()
		return fmt.Errorf("config: chmod temp file: %w", err)
	}
	if err := json.NewEncoder(f).Encode(settings); err != nil {
		_ = f.Close()
		remove()
		return fmt.Errorf("config: encode settings: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		remove()
		return fmt.Errorf("config: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return fmt.Errorf("config: close temp file: %w", err)
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		remove()
		return fmt.Errorf("config: replace settings: %w", err)
	}

	// Persist the rename itself. Directory fsync is not supported everywhere,
	// so failure here is best effort; the file data is already synced.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
