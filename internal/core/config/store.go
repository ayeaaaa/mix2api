// Package config provides the persistent JSON settings store.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Store is a JSON settings file at Path.
type Store struct {
	Path string
}

// mergeLocks serializes read-modify-write cycles by file path. Multiple Store
// values may point at the same settings file (for example the command-level
// store and a provider-owned store), so a per-Store mutex is not sufficient.
var mergeLocks sync.Map

func mergeLock(path string) *sync.Mutex {
	key := path
	if absolute, err := filepath.Abs(path); err == nil {
		key = absolute
	}
	key = filepath.Clean(key)
	value, _ := mergeLocks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
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
//
// The load/merge/write cycle is serialized across Store values that share the
// same file path, so two concurrent patches cannot lose each other's keys.
func (s *Store) Merge(patch map[string]any) (map[string]any, error) {
	lock := mergeLock(s.Path)
	lock.Lock()
	defer lock.Unlock()

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

// Update applies fn to the current settings and atomically writes the result.
// It is the read-modify-write primitive for callers that must delete legacy
// keys, which Merge cannot express because it only adds or replaces values.
// The whole cycle is serialized with Merge across Store values sharing a path.
func (s *Store) Update(fn func(map[string]any) error) (map[string]any, error) {
	lock := mergeLock(s.Path)
	lock.Lock()
	defer lock.Unlock()

	current, err := s.Load()
	if err != nil {
		return nil, err
	}
	if fn != nil {
		if err := fn(current); err != nil {
			return nil, err
		}
	}
	if err := s.write(current); err != nil {
		return nil, err
	}
	return current, nil
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
