package main

import (
	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/app"
	"github.com/ayeaaaa/mix2api-v2/internal/core/version"
)

// buildServerOptions is the command's single production composition point.
// PANEL_PASSWORD is intentionally not passed as a PanelAuth middleware: the
// platform password store is the only panel auth source, so rotating the panel
// password invalidates the bootstrap environment value immediately.
func buildServerOptions(cfg commandConfig, setup *app.Setup, platform *app.Platform, listenerPort int) api.ServerOptions {
	return api.ServerOptions{
		Addr:                     cfg.Addr(),
		ListenerPort:             listenerPort,
		Version:                  version.Version,
		Providers:                setup.Providers,
		Platform:                 platform.HTTPServices(),
		APIKeyAuth:               api.RequireAPIKeyAuth(cfg.APIKey),
		StaticAPIKey:             cfg.APIKey,
		APIKeyRealm:              apiKeyRealmResolver(cfg.APIKeyRealm),
		AnthropicMessagesEnabled: platform.AnthropicMessagesEnabled,
		PreserveRequestContract:  envBool("WB_PRESERVE_REQUEST_CONTRACT"),
	}
}
