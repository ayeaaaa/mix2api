package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// RealmService reads and writes active_realm.json and delegates the switch to
// providers implementing providers.RealmSelector.
type RealmService struct {
	Store       *config.Store
	AccountsDir string
	Providers   []providers.Provider
	Now         func() time.Time
}

// NewRealm returns a realm service.
func NewRealm(store *config.Store, accountsDir string, list []providers.Provider) *RealmService {
	return &RealmService{Store: store, AccountsDir: accountsDir, Providers: list}
}

// Active returns the persisted active realm, falling back to settings
// active_realm and then "intl". A persisted realm no longer offered by any
// selector collapses to "intl".
func (s *RealmService) Active(ctx context.Context) string {
	options := s.Options()
	allowed := make(map[string]bool, len(options))
	for _, option := range options {
		allowed[option] = true
	}
	if raw, err := os.ReadFile(s.realmPath()); err == nil {
		var payload map[string]any
		if json.Unmarshal(raw, &payload) == nil {
			realm := normalizeRealm(textValue(payload["realm"]))
			if realm != "" && allowed[realm] {
				return realm
			}
		}
	}
	if s.Store != nil {
		if data, err := s.Store.Load(); err == nil {
			realm := normalizeRealm(textValue(data["active_realm"]))
			if realm != "" && allowed[realm] {
				return realm
			}
		}
	}
	return "intl"
}

// Options returns the union of provider realm options in registration order.
// WorkBuddy supplies intl/cn; configured Raccoon and CodeArts exits follow,
// matching v1's conditional option list.
func (s *RealmService) Options() []string {
	out := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, provider := range s.Providers {
		selector, ok := provider.(providers.RealmSelector)
		if !ok {
			continue
		}
		for _, option := range selector.RealmOptions() {
			realm := normalizeRealm(option)
			if realm == "" || seen[realm] {
				continue
			}
			seen[realm] = true
			out = append(out, realm)
		}
	}
	for _, provider := range s.Providers {
		if _, ok := provider.(providers.RealmSelector); ok {
			continue
		}
		available, ok := provider.(providers.RealmAvailableProvider)
		if !ok || !available.RealmAvailable() {
			continue
		}
		realm := normalizeRealm(provider.Realm())
		if realm == "" || seen[realm] {
			continue
		}
		seen[realm] = true
		out = append(out, realm)
	}
	if len(out) == 0 {
		return []string{"intl", "cn"}
	}
	return out
}

// Set validates realm against the available options, writes active_realm.json,
// notifies selectors, and refreshes provider health.
func (s *RealmService) Set(ctx context.Context, realm string) error {
	realm = normalizeRealm(realm)
	if realm == "" {
		return errors.New("realm must be intl or cn")
	}
	valid := false
	for _, option := range s.Options() {
		if option == realm {
			valid = true
			break
		}
	}
	if !valid {
		return errors.New("realm must be intl or cn")
	}
	payload := map[string]any{
		"realm":       realm,
		"updated_at":  float64(s.now().UnixNano()) / float64(time.Second),
		"updated_iso": s.now().Format("2006-01-02 15:04:05"),
	}
	if err := s.writeRealm(payload); err != nil {
		return err
	}
	for _, provider := range s.Providers {
		if selector, ok := provider.(providers.RealmSelector); ok {
			if err := selector.SetActiveRealm(ctx, realm); err != nil {
				return err
			}
		}
	}
	for _, provider := range s.Providers {
		_, _ = provider.Health(ctx, true)
	}
	return nil
}

func (s *RealmService) realmPath() string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.AccountsDir, "active_realm.json")
}

func (s *RealmService) writeRealm(payload map[string]any) error {
	if s == nil || strings.TrimSpace(s.AccountsDir) == "" {
		return errors.New("settings: accounts directory is required")
	}
	if err := os.MkdirAll(s.AccountsDir, 0o700); err != nil {
		return fmt.Errorf("settings: create accounts directory: %w", err)
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := s.realmPath()
	tmp, err := os.CreateTemp(s.AccountsDir, ".active_realm-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	remove := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		remove()
		return err
	}
	return nil
}

func (s *RealmService) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func normalizeRealm(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "intl":
		return "intl"
	case "cn":
		return "cn"
	case "codearts":
		return "codearts"
	case "raccoon":
		return "raccoon"
	default:
		return ""
	}
}
