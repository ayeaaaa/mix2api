package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// Service is the agent configurator application service.
type Service struct {
	AccountsDir string
	Home        string
	Providers   []providers.Provider
	Now         func() time.Time
}

var serviceLocks sync.Map

func (s *Service) lock() *sync.Mutex {
	value, _ := serviceLocks.LoadOrStore(s, &sync.Mutex{})
	return value.(*sync.Mutex)
}

// New returns a service with the v1 process-home default.
func New(opts Service) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &opts
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) home() string {
	if s != nil && strings.TrimSpace(s.Home) != "" {
		return filepath.Clean(s.Home)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

func (s *Service) settingsStore() *config.Store {
	dir := strings.TrimSpace(s.AccountsDir)
	if dir == "" {
		dir = "accounts"
	}
	return config.New(filepath.Join(dir, "settings.json"))
}

// Overview returns per-client install/apply summaries keyed by client ID.
func (s *Service) Overview(ctx context.Context) map[string]any {
	_ = ctx
	home := s.home()
	state := loadState(s.AccountsDir)
	out := map[string]any{}
	clients := Clients()
	sort.Slice(clients, func(i, j int) bool { return clients[i].ID < clients[j].ID })
	for _, client := range clients {
		paths := client.Files(home)
		pathList := make([]string, 0, len(paths))
		existing := 0
		keys := make([]string, 0, len(paths))
		for key := range paths {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			path := paths[key]
			pathList = append(pathList, path)
			if _, err := os.Stat(path); err == nil {
				existing++
			}
		}
		installed := client.Probe(home) || existing > 0
		entry := map[string]any{
			"id": client.ID, "label": client.Label, "desc": client.Desc,
			"protocol": client.Protocol, "installed": installed,
			"configured": false, "config_paths": pathList, "applied": nil,
		}
		if record, ok := state.Clients[client.ID]; ok {
			external := false
			for _, file := range record.Files {
				info, err := os.Stat(file.Path)
				if err != nil || info.IsDir() {
					external = true
					continue
				}
				data, err := readConfig(file.Path)
				if err != nil || hashBytes(data) != file.SHA256 {
					external = true
				}
			}
			entry["configured"] = true
			entry["applied"] = map[string]any{
				"at": record.AppliedAt, "base_url": record.BaseURL, "model": record.Model,
				"files": len(record.Files), "external_change": external,
			}
		}
		out[client.ID] = entry
	}
	return out
}

// Apply resolves the model list and integrates one client.
func (s *Service) Apply(ctx context.Context, clientID, baseURL, apiKey, model string, models []ModelSpec) (map[string]any, error) {
	mu := s.lock()
	mu.Lock()
	defer mu.Unlock()
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("base_url must start with http:// or https://")
	}
	if _, ok := ClientByID(clientID); !ok {
		return nil, errors.New("unknown client " + clientID)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	specs := GatewayModels(ctx, s.Providers)
	if models == nil {
		if selection, configured := s.Selection(); configured {
			models = make([]ModelSpec, 0, len(selection))
			for _, id := range selection {
				models = append(models, ModelSpec{ID: id})
			}
		} else {
			models = specs
		}
	}
	models = Enrich(models, specs)
	if len(models) == 0 && embeddingClients[clientID] {
		return nil, errors.New("请先在「网关模型清单」中勾选至少一个模型：只有勾选的模型才会写入客户端配置")
	}
	if len(models) > MaxModels {
		models = models[:MaxModels]
	}
	plan, err := s.plan(clientID, baseURL, apiKey, model, models)
	if err != nil {
		return nil, err
	}
	return s.integrate(ctx, plan)
}

// Restore undoes one client integration.
func (s *Service) Restore(ctx context.Context, clientID string) (map[string]any, error) {
	mu := s.lock()
	mu.Lock()
	defer mu.Unlock()
	_ = ctx
	return s.restoreClient(clientID)
}

// Selection returns the persisted model ids and whether the key was ever set.
func (s *Service) Selection() ([]string, bool) {
	data, err := s.settingsStore().Load()
	if err != nil {
		return nil, false
	}
	raw, present := data["agent_models"]
	if !present {
		return nil, false
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := []string{}
	seen := map[string]bool{}
	for _, item := range list {
		id, ok := item.(string)
		if !ok {
			continue
		}
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, true
}

// SetSelection stores the selected model ids; an empty list is meaningful.
func (s *Service) SetSelection(models []string) error {
	mu := s.lock()
	mu.Lock()
	defer mu.Unlock()
	clean := []string{}
	seen := map[string]bool{}
	for _, item := range models {
		id := strings.TrimSpace(item)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
		if len(clean) >= MaxModels {
			break
		}
	}
	if dir := strings.TrimSpace(s.AccountsDir); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	_, err := s.settingsStore().Merge(map[string]any{"agent_models": clean})
	return err
}

func clientIDsText() string {
	ids := make([]string, 0)
	for _, client := range Clients() {
		ids = append(ids, client.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}
