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

// Default data directories match v1's names. v1 resolves them relative to the
// script directory; v2 resolves them relative to the process working directory,
// so deployments should set ACCOUNTS_DIR/USAGE_DIR explicitly when the working
// directory is not the install root.
const (
	DefaultAccountsDir = "accounts"
	DefaultUsageDir    = "usage"
)

// Paths is the command's persistent data layout. UsageDir is retained for the
// usage/pricing plan; it is deliberately not created or validated until a
// component consumes it, so an unused invalid path cannot block startup.
type Paths struct {
	AccountsDir string
	UsageDir    string
}

// PathsFromEnv resolves ACCOUNTS_DIR and USAGE_DIR. Blank values fall back to
// the defaults above so the command starts without configuration.
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
// api.ServerOptions.Providers. Only the accounts directory is created now;
// UsageDir remains a deferred path until usage storage is implemented. Startup
// errors never include credential values.
func Build(paths Paths) (*Setup, error) {
	paths = normalizePaths(paths)
	if err := os.MkdirAll(paths.AccountsDir, 0o700); err != nil {
		return nil, fmt.Errorf("app: create accounts directory: %w", err)
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
