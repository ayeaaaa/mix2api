package api

import (
	"context"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/agents"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/keys"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/logs"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/media"
	panelplatform "github.com/ayeaaaa/mix2api-v2/internal/platform/panel"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/pricing"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/probes"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/proxy"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/settings"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/update"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/webtools"
)

// PlatformServices is the narrow composition view of app.Platform. The command
// fills it once; New merges it into ServerOptions so every platform route and
// the usage recorder share exactly one store/session/registry.
type PlatformServices struct {
	Usage                 *usage.Service
	LogStore              *logs.LogStore
	Pricing               *pricing.Snapshot
	PricingRefresher      *pricing.Refresher
	PricingRefreshMinutes func() float64
	PricingVariantInherit func() bool
	Keys                  *keys.Store
	Settings              *settings.Service
	Realm                 *settings.RealmService
	Proxy                 *proxy.Slots
	Media                 *media.Service
	Agents                *agents.Service
	Probes                *probes.Service
	Updates               *update.Checker
	// UpdateRequests is the validated install/rollback request bridge.
	UpdateRequests *update.RequestManager
	WebTools       *webtools.Service

	PanelSessions *panelplatform.Sessions
	PanelPassword *panelplatform.Password

	// RecordUsage appends exactly one ledger row and runs the daily guard.
	// A failure is logged by the API and never changes the client response.
	RecordUsage func(context.Context, usage.RecordInput) error

	// DefaultMaxOutputTokens resolves the catalog max_output for Responses and
	// Messages. The API caches positive values for the process lifetime.
	DefaultMaxOutputTokens func(string) int
}

func (s *Server) mergePlatform(platform *PlatformServices) {
	if platform == nil {
		return
	}
	if s.opts.Usage == nil && s.opts.UsageSvc == nil {
		s.opts.Usage = platform.Usage
	}
	if s.opts.LogStore == nil && s.opts.Logs == nil {
		s.opts.LogStore = platform.LogStore
	}
	if s.opts.Pricing == nil {
		s.opts.Pricing = platform.Pricing
	}
	if s.opts.PricingRefresher == nil {
		s.opts.PricingRefresher = platform.PricingRefresher
	}
	if s.opts.PricingRefreshMinutes == nil {
		s.opts.PricingRefreshMinutes = platform.PricingRefreshMinutes
	}
	if s.opts.PricingVariantInherit == nil {
		s.opts.PricingVariantInherit = platform.PricingVariantInherit
	}
	if s.opts.KeyStore == nil && s.opts.Keys == nil {
		s.keyStore = platform.Keys
	}
	if s.opts.Settings == nil && s.opts.SettingsService == nil {
		s.settings = platform.Settings
	}
	if s.opts.Realm == nil {
		s.opts.Realm = platform.Realm
	}
	if s.opts.ProxySlots == nil && s.opts.Proxy == nil {
		s.proxySlots = platform.Proxy
	}
	if s.opts.Media == nil {
		s.opts.Media = platform.Media
	}
	if s.opts.Agents == nil {
		s.opts.Agents = platform.Agents
	}
	if s.opts.Probes == nil {
		s.opts.Probes = platform.Probes
	}
	if s.opts.Updates == nil {
		s.opts.Updates = platform.Updates
	}
	if s.opts.UpdateRequests == nil {
		s.opts.UpdateRequests = platform.UpdateRequests
	}
	if s.opts.WebTools == nil {
		s.opts.WebTools = platform.WebTools
	}
	if s.opts.PanelSessions == nil {
		s.sessions = platform.PanelSessions
	}
	if s.opts.PanelPassword == nil && s.opts.PanelPasswords == nil {
		s.password = platform.PanelPassword
	}
	if s.opts.RecordUsage == nil {
		s.opts.RecordUsage = platform.RecordUsage
	}
	if s.opts.DefaultMaxOutputTokens == nil {
		s.opts.DefaultMaxOutputTokens = platform.DefaultMaxOutputTokens
	}
}

// UsageRecorder returns the configured recorder callback, or nil.
func (s *Server) UsageRecorder() func(context.Context, usage.RecordInput) error {
	if s == nil {
		return nil
	}
	return s.opts.RecordUsage
}
