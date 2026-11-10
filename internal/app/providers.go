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
	"github.com/ayeaaaa/mix2api-v2/internal/providers/codearts"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/raccoon"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/workbuddy"
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
// WB_PROXY_USAGE_DIR (v1's name) and then the defaults above so the command
// starts without configuration.
func PathsFromEnv() Paths {
	usageDir := envDir("USAGE_DIR", "")
	if strings.TrimSpace(usageDir) == "" {
		usageDir = envDir("WB_PROXY_USAGE_DIR", DefaultUsageDir)
	}
	return Paths{
		AccountsDir: envDir("ACCOUNTS_DIR", DefaultAccountsDir),
		UsageDir:    usageDir,
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

	// Exactly one CodeArts Provider/Pool is created per accounts directory.
	// Pool locks are instance-local, so a second Provider over the same
	// state.json would race even though each Pool is internally consistent.
	codeartsProvider, err := codearts.New(store)
	if err != nil {
		return nil, fmt.Errorf("app: initialize codearts provider: %w", err)
	}
	if err := registry.Register(codeartsProvider); err != nil {
		return nil, fmt.Errorf("app: register codearts provider: %w", err)
	}

	// WorkBuddy registers one instance per realm. Both share the same code and
	// settings file but own separate account pools filtered by realm.
	intlProvider, err := workbuddy.New(workbuddy.ProviderRealmIntl, store)
	if err != nil {
		return nil, fmt.Errorf("app: initialize workbuddy intl provider: %w", err)
	}
	cnProvider, err := workbuddy.New(workbuddy.ProviderRealmCN, store)
	if err != nil {
		return nil, fmt.Errorf("app: initialize workbuddy cn provider: %w", err)
	}
	intlProvider.SetTwin(cnProvider)
	cnProvider.SetTwin(intlProvider)

	// v1 has one process-wide chat ceiling, not one per realm. Both instances
	// share this limiter so intl, cn, and the opt-in fallback consume the same
	// permits. The default is 32; a larger per-realm panel value raises it.
	sharedLimit := intlProvider.Config().MaxConcurrent
	if cnLimit := cnProvider.Config().MaxConcurrent; cnLimit > sharedLimit {
		sharedLimit = cnLimit
	}
	sharedAdmission := workbuddy.NewAdmission(sharedLimit)
	intlProvider.SetAdmission(sharedAdmission)
	cnProvider.SetAdmission(sharedAdmission)

	// One shared activity history keeps the intl daily-activity and cn
	// report/growth histories in the same panel timeline.
	activityPath := filepath.Join(paths.AccountsDir, workbuddy.ActivityHistoryFile)
	intlProvider.SetActivityPath(activityPath)
	cnProvider.SetActivityPath(activityPath)

	// WorkBuddy browser-login auto slot selection must not reuse a proxy slot
	// already bound to a CodeArts account. The callback is evaluated lazily
	// because CodeArts was constructed above but accounts can change later.
	takenSlots := codeartsProxySlots(codeartsProvider)
	intlProvider.SetLoginTakenSlots(takenSlots)
	cnProvider.SetLoginTakenSlots(takenSlots)

	if err := registry.Register(intlProvider); err != nil {
		return nil, fmt.Errorf("app: register workbuddy intl provider: %w", err)
	}
	if err := registry.Register(cnProvider); err != nil {
		return nil, fmt.Errorf("app: register workbuddy cn provider: %w", err)
	}

	return &Setup{
		Paths:     paths,
		Store:     store,
		Registry:  registry,
		Providers: registry.All(),
	}, nil
}

// codeartsProxySlots returns a lazy snapshot of proxy slot ids bound to
// CodeArts accounts. WorkBuddy uses it only to exclude those slots from browser
// login auto-selection; an empty or nil result means no exclusions.
func codeartsProxySlots(provider *codearts.Provider) func() []string {
	return func() []string {
		if provider == nil {
			return nil
		}
		pool := provider.Pool()
		if pool == nil {
			return nil
		}
		slots := make([]string, 0)
		for _, account := range pool.Accounts() {
			if account == nil {
				continue
			}
			if slot := account.ProxySlot(); slot != "" {
				slots = append(slots, slot)
			}
		}
		return slots
	}
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
