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
	srv := api.NewServer(api.ServerOptions{
		Addr:    ":" + port,
		Version: version.Version,
	})
	log.Printf("listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
