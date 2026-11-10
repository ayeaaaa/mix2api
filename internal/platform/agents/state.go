package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	StateFile  = "integration-state.json"
	BackupDir  = "agent-backups"
	BackupKeep = 10
	maxConfig  = 8 * 1024 * 1024
	writeMode  = 0o600
)

var stampPattern = regexp.MustCompile(`^\d{8}-\d{6}-`)

// State is the persisted integration record per client.
type State struct {
	Clients map[string]Record `json:"clients"`
}

// Record describes the files one successful apply wrote.
type Record struct {
	AppliedAt string       `json:"applied_at"`
	BaseURL   string       `json:"base_url"`
	Model     string       `json:"model"`
	Files     []FileRecord `json:"files"`
}

// FileRecord is the backup/hash bookkeeping for one target path.
type FileRecord struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
	Backup  string `json:"backup,omitempty"`
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
}

func statePath(accountsDir string) string {
	return filepath.Join(accountsDir, StateFile)
}

func loadState(accountsDir string) State {
	state := State{Clients: map[string]Record{}}
	raw, err := os.ReadFile(statePath(accountsDir))
	if err != nil {
		return state
	}
	var decoded State
	if json.Unmarshal(raw, &decoded) != nil {
		return state
	}
	if decoded.Clients != nil {
		state.Clients = decoded.Clients
	}
	return state
}

func saveState(accountsDir string, state State) error {
	if state.Clients == nil {
		state.Clients = map[string]Record{}
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(statePath(accountsDir), append(raw, '\n'))
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readConfig(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfig {
		return nil, fmt.Errorf("refusing to edit %s: file is %d bytes (limit %d)", path, len(data), maxConfig)
	}
	return data, nil
}

func readText(path string) (string, error) {
	data, err := readConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func loadJSON(path, what string) (map[string]any, error) {
	text, err := readText(path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return map[string]any{}, nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%s): %v", what, path, err)
	}
	if data == nil {
		return nil, fmt.Errorf("%s must be a JSON object: %s", what, path)
	}
	return data, nil
}

func dumpJSON(data map[string]any) ([]byte, error) {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func deepMerge(base, patch map[string]any) map[string]any {
	if base == nil {
		return patch
	}
	if patch == nil {
		return base
	}
	out := make(map[string]any, len(base)+len(patch))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range patch {
		if patchMap, ok := value.(map[string]any); ok {
			if baseMap, ok := out[key].(map[string]any); ok {
				out[key] = deepMerge(baseMap, patchMap)
				continue
			}
		}
		out[key] = value
	}
	return out
}

func targetMode(path string) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return writeMode
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 || mode&0o400 == 0 {
		return writeMode
	}
	return mode
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	remove := func() { _ = os.Remove(name) }
	if err := tmp.Chmod(targetMode(path)); err != nil {
		_ = tmp.Close()
		remove()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		remove()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		remove()
		return err
	}
	if err := tmp.Close(); err != nil {
		remove()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		remove()
		return err
	}
	return nil
}

func backupFile(accountsDir, clientID, path string, original []byte, now time.Time) (string, error) {
	directory := filepath.Join(accountsDir, BackupDir, clientID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	stamp := now.Format("20060102-150405")
	base := filepath.Base(path)
	if strings.TrimSpace(base) == "" {
		base = "config"
	}
	name := stamp + "-" + base
	target := filepath.Join(directory, name)
	for sequence := 1; ; sequence++ {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			break
		}
		target = filepath.Join(directory, name+"-"+strconv.Itoa(sequence))
	}
	if err := writeFileAtomic(target, original); err != nil {
		return "", err
	}
	pruneBackups(directory, BackupKeep)
	return target, nil
}

func pruneBackups(directory string, keep int) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	byBase := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		base := stampPattern.ReplaceAllString(name, "")
		// Same-second applies get a numeric suffix; it is part of the
		// collision name, not a different logical basename.
		if index := strings.LastIndex(base, "-"); index > 0 {
			if _, err := strconv.Atoi(base[index+1:]); err == nil {
				base = base[:index]
			}
		}
		byBase[base] = append(byBase[base], name)
	}
	for _, names := range byBase {
		sort.Strings(names)
		if len(names) <= keep {
			continue
		}
		for _, stale := range names[:len(names)-keep] {
			_ = os.Remove(filepath.Join(directory, stale))
		}
	}
}

func pruneEmptyParents(path, home string) {
	home = filepath.Clean(home)
	directory := filepath.Dir(filepath.Clean(path))
	for {
		if directory == home || !strings.HasPrefix(directory, home+string(os.PathSeparator)) {
			return
		}
		if err := os.Remove(directory); err != nil {
			return
		}
		directory = filepath.Dir(directory)
	}
}
