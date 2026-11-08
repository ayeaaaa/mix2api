// Command mix2api runs the mix2api server.
package main

import (
	"log"
	"os"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/core/version"
)

const defaultPort = "58675"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	setup, err := setupProviders()
	if err != nil {
		// Startup failures may name local paths but never credential values.
		log.Fatalf("startup: %v", err)
	}
	srv := api.NewServer(api.ServerOptions{
		Addr:      ":" + port,
		Version:   version.Version,
		Providers: setup.Providers,
	})
	log.Printf("listening on %s with %d provider(s)", srv.Addr, len(setup.Providers))
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
