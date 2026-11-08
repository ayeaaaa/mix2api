package main

import "github.com/ayeaaaa/mix2api-v2/internal/app"

// setupProviders initializes the command's persistent directory layout and
// provider registry from ACCOUNTS_DIR/USAGE_DIR. The composition lives in
// internal/app so integration tests can exercise the exact startup path without
// importing package main.
func setupProviders() (*app.Setup, error) {
	return app.Build(app.PathsFromEnv())
}
