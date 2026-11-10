package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// planResult is the exact file contents apply would write.
type planResult struct {
	Client  Client
	BaseURL string
	Model   string
	Files   []plannedFile
}

type plannedFile struct {
	Path    string
	Content []byte
	Existed bool
}

func (s *Service) plan(clientID, baseURL, apiKey, model string, models []ModelSpec) (planResult, error) {
	client, ok := ClientByID(clientID)
	if !ok {
		return planResult{}, fmt.Errorf("unknown client %q (known: %s)", clientID, clientIDsText())
	}
	home := s.home()
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return planResult{}, errors.New("base_url must start with http:// or https://")
	}
	baseURLV1 := baseURL
	if !strings.HasSuffix(baseURLV1, "/v1") {
		baseURLV1 += "/v1"
	}
	ctx := Context{
		BaseURL: baseURL, BaseURLV1: baseURLV1, APIKey: apiKey,
		Model: strings.TrimSpace(model), Models: models, Home: home,
	}
	paths := client.Files(home)
	newContents, err := client.Apply(paths, ctx)
	if err != nil {
		return planResult{}, fmt.Errorf("failed to build %s config: %w", clientID, err)
	}
	planned := planResult{Client: client, BaseURL: baseURL, Model: ctx.Model}
	orderedKeys := make([]string, 0, len(paths))
	for key := range paths {
		orderedKeys = append(orderedKeys, key)
	}
	sort.Strings(orderedKeys)
	for _, key := range orderedKeys {
		path := paths[key]
		content, ok := newContents[path]
		if !ok {
			continue
		}
		_, statErr := os.Stat(path)
		planned.Files = append(planned.Files, plannedFile{Path: path, Content: []byte(content), Existed: statErr == nil})
	}
	return planned, nil
}

func (s *Service) integrate(ctx context.Context, plan planResult) (map[string]any, error) {
	state := loadState(s.AccountsDir)
	record := state.Clients[plan.Client.ID]
	previous := map[string]FileRecord{}
	for _, entry := range record.Files {
		previous[entry.Path] = entry
	}

	type stagedFile struct {
		plannedFile
		Backup string
	}
	staged := make([]stagedFile, 0, len(plan.Files))
	for _, entry := range plan.Files {
		existed := entry.Existed
		prior, hadPrior := previous[entry.Path]
		if hadPrior && !prior.Existed && existed {
			if data, err := os.ReadFile(entry.Path); err == nil && hashBytes(data) == prior.SHA256 {
				existed = false
			}
		}
		backup := ""
		if existed && hadPrior && prior.Backup != "" {
			backup = prior.Backup
		} else if existed {
			original, err := readConfig(entry.Path)
			if err != nil {
				return nil, err
			}
			backup, err = backupFile(s.AccountsDir, plan.Client.ID, entry.Path, original, s.now())
			if err != nil {
				return nil, err
			}
		}
		staged = append(staged, stagedFile{plannedFile: plannedFile{Path: entry.Path, Content: entry.Content, Existed: existed}, Backup: backup})
	}

	type writtenFile struct {
		Path    string
		Existed bool
		Backup  string
	}
	written := []writtenFile{}
	fileRecords := []FileRecord{}
	for _, entry := range staged {
		if err := writeFileAtomic(entry.Path, entry.Content); err != nil {
			for _, done := range written {
				if done.Existed && done.Backup != "" {
					if original, readErr := os.ReadFile(done.Backup); readErr == nil {
						_ = writeFileAtomic(done.Path, original)
						continue
					}
				}
				if !done.Existed {
					_ = os.Remove(done.Path)
				}
			}
			return nil, fmt.Errorf("failed to write %s files: %w", plan.Client.ID, err)
		}
		written = append(written, writtenFile{Path: entry.Path, Existed: entry.Existed, Backup: entry.Backup})
		fileRecords = append(fileRecords, FileRecord{
			Path: entry.Path, Existed: entry.Existed, Backup: entry.Backup,
			SHA256: hashBytes(entry.Content), Bytes: len(entry.Content),
		})
	}
	appliedAt := s.now().Format("2006-01-02 15:04:05")
	state.Clients[plan.Client.ID] = Record{
		AppliedAt: appliedAt, BaseURL: plan.BaseURL, Model: plan.Model, Files: fileRecords,
	}
	if err := saveState(s.AccountsDir, state); err != nil {
		return nil, err
	}
	return map[string]any{
		"client": plan.Client.ID, "label": plan.Client.Label,
		"applied_at": appliedAt, "base_url": plan.BaseURL, "model": plan.Model,
		"files": fileRecords,
	}, nil
}

func (s *Service) restoreClient(clientID string) (map[string]any, error) {
	client, ok := ClientByID(clientID)
	if !ok {
		return nil, fmt.Errorf("unknown client %q (known: %s)", clientID, clientIDsText())
	}
	state := loadState(s.AccountsDir)
	record, ok := state.Clients[clientID]
	if !ok || len(record.Files) == 0 {
		return nil, fmt.Errorf("%s has no recorded integration to restore", client.Label)
	}
	restored := []map[string]any{}
	for _, entry := range record.Files {
		path := entry.Path
		if path == "" {
			continue
		}
		if entry.Existed && entry.Backup != "" {
			if _, err := os.Stat(entry.Backup); err != nil {
				return nil, fmt.Errorf("backup missing for %s: %s", path, entry.Backup)
			}
			original, err := readConfig(entry.Backup)
			if err != nil {
				return nil, err
			}
			if err := writeFileAtomic(path, original); err != nil {
				return nil, err
			}
			restored = append(restored, map[string]any{"path": path, "action": "restored"})
			continue
		}
		err := os.Remove(path)
		action := "deleted"
		if errors.Is(err, os.ErrNotExist) {
			action = "already-absent"
		} else if err != nil {
			return nil, err
		}
		restored = append(restored, map[string]any{"path": path, "action": action})
		pruneEmptyParents(path, s.home())
	}
	delete(state.Clients, clientID)
	if err := saveState(s.AccountsDir, state); err != nil {
		return nil, err
	}
	return map[string]any{"client": clientID, "label": client.Label, "restored": restored}, nil
}

// ClientStatePath is exposed for tests and composition diagnostics.
func ClientStatePath(accountsDir string) string { return statePath(accountsDir) }

var _ = filepath.Separator
var _ = time.Second
