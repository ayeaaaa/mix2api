// Command mix2api runs the mix2api server.
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/core/version"
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
	}
}

func (c commandConfig) Addr() string {
	return net.JoinHostPort(c.Host, c.Port)
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

	listener, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		log.Fatalf("startup: listen on %s: %v", cfg.Addr(), err)
	}
	listenerPort := 0
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		listenerPort = tcpAddr.Port
	}

	srv := api.NewServer(api.ServerOptions{
		Addr:         cfg.Addr(),
		ListenerPort: listenerPort,
		Version:      version.Version,
		Providers:    setup.Providers,
		PanelAuth:    api.RequirePanelAuth(cfg.PanelPassword),
		APIKeyAuth:   api.RequireAPIKeyAuth(cfg.APIKey),
	})
	log.Printf("listening on %s with %d provider(s)", listener.Addr(), len(setup.Providers))
	if err := srv.Serve(listener); err != nil {
		log.Fatal(err)
	}
}
