// Package api exposes the generic mix2api HTTP surface.
package api

import (
	"encoding/json"
	"net/http"
)

// ServerOptions configures the generic API server.
type ServerOptions struct {
	Addr    string
	Version string
}

// healthResponse is the wire shape of GET /health.
type healthResponse struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

// NewServer returns an HTTP server exposing the generic health endpoint.
func NewServer(opts ServerOptions) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(healthResponse{OK: true, Version: opts.Version})
	})
	return &http.Server{
		Addr:    opts.Addr,
		Handler: mux,
	}
}
