package main

import (
	"strings"
	"testing"
)

func TestConfigDefaultsToLoopback(t *testing.T) {
	t.Setenv("HOST", "")
	t.Setenv("PORT", "")
	cfg := configFromEnv()
	if cfg.Host != "127.0.0.1" || cfg.Port != defaultPort {
		t.Fatalf("defaults = %#v, want 127.0.0.1:%s", cfg, defaultPort)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("loopback without auth should start (routes fail closed): %v", err)
	}
}

func TestNonLoopbackRequiresBothAuths(t *testing.T) {
	cfg := commandConfig{Host: "0.0.0.0", Port: "58675"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "PANEL_PASSWORD") || !strings.Contains(err.Error(), "MIX2API_API_KEY") {
		t.Fatalf("missing-auth error = %v", err)
	}
	cfg.PanelPassword = "panel"
	cfg.APIKey = "key"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fully configured non-loopback bind rejected: %v", err)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost", ""} {
		if !isLoopbackHost(host) {
			t.Fatalf("%q should be loopback", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "192.168.1.2", "example.com", "::"} {
		if isLoopbackHost(host) {
			t.Fatalf("%q should not be loopback", host)
		}
	}
}
