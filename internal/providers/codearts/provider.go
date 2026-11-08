package codearts

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

// Provider is one CodeArts realm instance. It owns the concrete pool,
// provider configuration, and the shared core transport that later tasks wire
// into OAuth, credits, check-in, and chat. The full providers.Provider contract
// is implemented incrementally by the remaining CodeArts tasks.
type Provider struct {
	store       *config.Store
	accountsDir string

	mu           sync.RWMutex
	cfg          Config
	pool         *Pool
	transport    *transport.Client
	tokenManager *TokenManager

	// catalogHTTPTimeout bounds each upstream model-discovery call. Tests may
	// lower it; zero means the package default.
	catalogHTTPTimeout time.Duration
}

// New returns the production CodeArts provider. The accounts directory is the
// directory containing the shared settings file, matching v1's layout.
//
// Task 9 composition must create exactly one Provider/Pool per accounts
// directory in a process: Pool locks are instance-local, so two Pools over the
// same state.json would still race with each other.
func New(store *config.Store) (*Provider, error) {
	if store == nil || strings.TrimSpace(store.Path) == "" {
		return nil, errors.New("codearts: config store is required")
	}
	accountsDir := filepath.Dir(store.Path)
	if strings.TrimSpace(accountsDir) == "" {
		return nil, errors.New("codearts: accounts directory is empty")
	}

	cfg := LoadConfig(store)
	pool, err := NewPool(accountsDir, cfg)
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(cfg.TimeoutSeconds * float64(time.Second))
	if timeout <= 0 {
		timeout = defaultTokenRefreshTimeout
	}
	client := transport.NewClient(transport.ClientOptions{})
	manager := NewTokenManager(client, TokenManagerOptions{
		Pool:     pool,
		Timeout:  timeout,
		Now:      func() time.Time { return pool.nowTime() },
		ProxyURL: func(account *Account) string { return account.ProxyURL() },
	})

	return &Provider{
		store:              store,
		accountsDir:        accountsDir,
		cfg:                cfg,
		pool:               pool,
		transport:          client,
		tokenManager:       manager,
		catalogHTTPTimeout: catalogHTTPTimeout,
	}, nil
}

// NewPool returns the provider-owned account pool. It exists for route and
// scheduler wiring in later tasks; accounts are loaded once by New.
func (p *Provider) NewPool() *Pool {
	if p == nil {
		return nil
	}
	return p.pool
}

// Pool returns the provider-owned account pool.
func (p *Provider) Pool() *Pool {
	return p.NewPool()
}

// TokenManager returns the provider-owned token refresh manager. It is built
// during New and performs no network I/O until Ensure or Keepalive runs.
func (p *Provider) TokenManager() *TokenManager {
	if p == nil {
		return nil
	}
	return p.tokenManager
}

// Config returns the current CodeArts configuration snapshot.
func (p *Provider) Config() Config {
	if p == nil {
		return Config{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}
