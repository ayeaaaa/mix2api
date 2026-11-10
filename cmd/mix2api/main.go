// Command mix2api runs the mix2api server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/app"
	"github.com/ayeaaaa/mix2api-v2/internal/core/version"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	defaultPort = "58675"
	defaultHost = "127.0.0.1"
)

type commandConfig struct {
	Host          string
	Port          string
	PanelPassword string
	APIKey        string
	// APIKeyRealm binds every configured API key to one provider realm. When
	// set, generic chat/models treat it as policy ahead of request realm hints
	// and suppress WorkBuddy's intl→cn fallback.
	APIKeyRealm string
}

func configFromEnv() commandConfig {
	host := strings.TrimSpace(os.Getenv("HOST"))
	if host == "" {
		host = defaultHost
	}
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = defaultPort
	}
	return commandConfig{
		Host:          host,
		Port:          port,
		PanelPassword: strings.TrimSpace(os.Getenv("PANEL_PASSWORD")),
		APIKey:        strings.TrimSpace(os.Getenv("MIX2API_API_KEY")),
		APIKeyRealm:   strings.ToLower(strings.TrimSpace(os.Getenv("MIX2API_API_KEY_REALM"))),
	}
}

func (c commandConfig) Addr() string {
	return net.JoinHostPort(c.Host, c.Port)
}

// apiKeyRealmResolver returns the ServerOptions callback for the configured
// key/realm binding. A blank realm leaves keys unbound.
func apiKeyRealmResolver(realm string) func(*http.Request) string {
	realm = strings.ToLower(strings.TrimSpace(realm))
	if realm == "" {
		return nil
	}
	return func(*http.Request) string { return realm }
}

// validateAPIKeyRealm rejects a configured binding that names no registered
// provider. It runs after provider construction and before the listener opens.
func validateAPIKeyRealm(realm string, registry *providers.Registry) error {
	realm = strings.ToLower(strings.TrimSpace(realm))
	if realm == "" {
		return nil
	}
	if registry == nil {
		return fmt.Errorf("MIX2API_API_KEY_REALM %q cannot be validated without a provider registry", realm)
	}
	if _, ok := registry.Get(realm); !ok {
		return fmt.Errorf("MIX2API_API_KEY_REALM %q is not a registered provider realm", realm)
	}
	return nil
}

// Validate refuses a non-loopback bind without both auth surfaces. A loopback
// server may start without them, but the routes still fail closed until the
// operator supplies PANEL_PASSWORD / MIX2API_API_KEY.
func (c commandConfig) Validate() error {
	if isLoopbackHost(c.Host) {
		return nil
	}
	missing := make([]string, 0, 2)
	if c.PanelPassword == "" {
		missing = append(missing, "PANEL_PASSWORD")
	}
	if c.APIKey == "" {
		missing = append(missing, "MIX2API_API_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("refusing non-loopback bind %s without %s", c.Addr(), strings.Join(missing, " and "))
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func main() {
	cfg := configFromEnv()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("startup: %v", err)
	}
	if cfg.PanelPassword == "" || cfg.APIKey == "" {
		log.Printf("auth: PANEL_PASSWORD/MIX2API_API_KEY not fully configured; protected routes will reject requests")
	}

	setup, err := setupProviders()
	if err != nil {
		// Startup failures may name local paths but never credential values.
		log.Fatalf("startup: %v", err)
	}
	if err := validateAPIKeyRealm(cfg.APIKeyRealm, setup.Registry); err != nil {
		log.Fatalf("startup: %v", err)
	}

	listener, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		log.Fatalf("startup: listen on %s: %v", cfg.Addr(), err)
	}
	listenerPort := 0
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		listenerPort = tcpAddr.Port
	}

	platform, err := app.BuildPlatform(setup, app.PlatformOptions{
		Paths:         setup.Paths,
		Version:       version.Version,
		ListenerPort:  listenerPort,
		PanelPassword: cfg.PanelPassword,
		LAN:           !isLoopbackHost(cfg.Host),
		Logger:        func(message string) { log.Print(message) },
	})
	if err != nil {
		log.Fatalf("startup: platform: %v", err)
	}
	defer platform.Close()

	srv := api.NewServer(buildServerOptions(cfg, setup, platform, listenerPort))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The listener is bound, so provider and platform background loops are safe
	// to start without racing startup errors.
	platform.Start(ctx)
	log.Printf("listening on %s with %d provider(s)", listener.Addr(), len(setup.Providers))

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
		cancel()
	}

	// Stop background loops before closing the stores they use.
	stop()
	platform.Close()
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
