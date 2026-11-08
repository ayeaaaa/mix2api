// Package app composes the mix2api command from shared components. It is the
// only non-command package that imports concrete providers, keeping provider
// registration and filesystem layout in one testable bootstrap path.
package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/raccoon"
)

// Default data directories match v1's relative layout.
const (
	DefaultAccountsDir = "accounts"
	DefaultUsageDir    = "usage"
)

// Paths is the command's persistent data layout.
type Paths struct {
	AccountsDir string
	UsageDir    string
}

// PathsFromEnv resolves ACCOUNTS_DIR and USAGE_DIR. Blank values fall back to
// the v1 defaults so the command starts without configuration.
func PathsFromEnv() Paths {
	return Paths{
		AccountsDir: envDir("ACCOUNTS_DIR", DefaultAccountsDir),
		UsageDir:    envDir("USAGE_DIR", DefaultUsageDir),
	}
}

func envDir(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// Setup is the provider composition handed to the HTTP server. The store is
// exposed so routes and future usage/account wiring read the same settings
// file the providers own.
type Setup struct {
	Paths     Paths
	Store     *config.Store
	Registry  *providers.Registry
	Providers []providers.Provider
}

// Build prepares the persistent settings store, registers every provider
// available in this build, and returns the provider list for
// api.ServerOptions.Providers. Startup errors never include credential values.
func Build(paths Paths) (*Setup, error) {
	paths = normalizePaths(paths)
	if err := os.MkdirAll(paths.AccountsDir, 0o700); err != nil {
		return nil, fmt.Errorf("app: create accounts directory: %w", err)
	}
	if err := os.MkdirAll(paths.UsageDir, 0o700); err != nil {
		return nil, fmt.Errorf("app: create usage directory: %w", err)
	}

	store := config.New(filepath.Join(paths.AccountsDir, "settings.json"))
	registry := providers.NewRegistry()

	raccoonProvider, err := raccoon.New(store)
	if err != nil {
		return nil, fmt.Errorf("app: initialize raccoon provider: %w", err)
	}
	if err := registry.Register(raccoonProvider); err != nil {
		return nil, fmt.Errorf("app: register raccoon provider: %w", err)
	}

	return &Setup{
		Paths:     paths,
		Store:     store,
		Registry:  registry,
		Providers: registry.All(),
	}, nil
}

func normalizePaths(paths Paths) Paths {
	if value := strings.TrimSpace(paths.AccountsDir); value != "" {
		paths.AccountsDir = filepath.Clean(value)
	} else {
		paths.AccountsDir = DefaultAccountsDir
	}
	if value := strings.TrimSpace(paths.UsageDir); value != "" {
		paths.UsageDir = filepath.Clean(value)
	} else {
		paths.UsageDir = DefaultUsageDir
	}
	return paths
}
