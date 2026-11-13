package providers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

// SettingsContributor is implemented by providers that own a section of the
// panel settings document. Platform settings services merge the view into the
// base payload and dispatch saves without importing a concrete provider.
type SettingsContributor interface {
	SettingsView(ctx context.Context) (map[string]any, error)
	SaveSettings(ctx context.Context, patch map[string]any) error
}

// LimitApplier is implemented by providers that opt into the shared daily
// guards. values keeps v1's shape:
//
//	{"reserve_credits": {"global": 10, "intl": 20, "cn": 5}, ...}
//
// usage is nil when today's counters could not be read; a nil usage keeps the
// provider's last known counters, while a non-nil fold treats a missing uid as
// zero. Providers without this hook are untouched, so WorkBuddy credits never
// leak into CodeArts or Raccoon.
type LimitApplier interface {
	ApplyLimits(values map[string]any, usage map[string]any)
}

// UsageSnapshot is the provider-neutral per-request attribution returned by a
// chat stream. Providers that can report account/realm/token/credit do so;
// providers that cannot return the zero value and the API records the provider
// realm with an empty account.
type UsageSnapshot struct {
	Realm            string
	Account          string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64
	ReasoningTokens  int64
	Credit           float64
	CreditKnown      bool
	Free             bool
	CostPolicy       string
	Kind             string
}

// UsageSnapshotReporter is implemented by stream wrappers that track the
// upstream usage frame. The API records exactly one usage row from this
// snapshot without re-parsing the stream.
type UsageSnapshotReporter interface {
	UsageSnapshot() UsageSnapshot
}

// GatewayHint is implemented by provider errors that can add v1's optional
// `error.gateway_hint` explanation beside the upstream message. An empty hint
// means "nothing to add"; the API never invents one for an unknown failure.
type GatewayHint interface {
	error

	GatewayHint() string
}

// AccountSummary is the provider-neutral account row used by the panel health
// identity block and the usage summary's accounts_map/current-account fields.
// It contains no credential value; CredentialFile is only the file's basename.
type AccountSummary struct {
	UID            string
	Nickname       string
	Realm          string
	Domain         string
	Issuer         string
	CredentialFile string
	ExpiresAt      float64
	Enabled        bool
	Ready          bool
}

// AccountInventory is implemented by providers that expose their live account
// pool to platform-level panel summaries. Providers without it contribute no
// accounts, so Raccoon and CodeArts usage stays unchanged.
type AccountInventory interface {
	AccountSummaries(ctx context.Context) ([]AccountSummary, error)
}

// RealmSelector is implemented by providers that expose a switchable default
// upstream exit. Only WorkBuddy currently implements it.
type RealmSelector interface {
	ActiveRealm(ctx context.Context) string
	SetActiveRealm(ctx context.Context, realm string) error
	RealmOptions() []string
}

// ProxySlotBinder is implemented by providers with proxy-slot-bound accounts.
// ApplyProxySlots returns how many accounts were unbound because their slot
// disappeared; ProxySlotBindings returns enabled-account counts per slot id.
type ProxySlotBinder interface {
	ApplyProxySlots(slots transport.Slots) int
	ProxySlotBindings() map[string]int
}

// Capabilities describes the optional provider features compiled into an
// implementation. The platform checks these flags before dispatching an
// optional request, so an absent hook never causes a provider call.
type Capabilities struct {
	MediaGeneration  bool
	MediaImages      bool
	MediaVideos      bool
	ImageInputInChat bool
	AgentModelSpecs  bool
	WindowProbe      bool
	QuotaScan        bool
	UsageLimits      bool
}

// CapabilityProvider is implemented by every provider that participates in
// optional platform surfaces. Providers without it are treated as all-false.
type CapabilityProvider interface {
	Capabilities() Capabilities
}

// AgentModelSpec is the provider-neutral model metadata used by the agent
// configurator. Explicit panel payload fields override these values.
type AgentModelSpec struct {
	ID               string
	Name             string
	ContextWindow    int64
	MaxOutputTokens  int64
	ReasoningEfforts []string
	SupportsVision   bool
}

// AgentModelSpecProvider optionally exposes richer model specs without making
// the platform scan a provider's chat catalog.
type AgentModelSpecProvider interface {
	AgentModelSpecs(ctx context.Context) ([]AgentModelSpec, error)
}

// AgentPickerModelProvider optionally exposes the provider's v1 one-click
// picker catalogue. Unlike Models it preserves the picker's order and spelling
// rules, so the panel does not have to merge every provider catalogue.
type AgentPickerModelProvider interface {
	AgentPickerModels(ctx context.Context, force bool) ([]Model, error)
}

// AgentOfficialModelProvider optionally exposes the provider's official
// picker-only ids. v1 appends these after the WorkBuddy catalogue when the
// exit is configured, without fetching the live catalogue.
type AgentOfficialModelProvider interface {
	AgentOfficialModels() []Model
}

// CatalogProvenanceProvider optionally reports the v1 top-level catalog block
// for /v1/models. Providers without it keep the v2 envelope unchanged.
type CatalogProvenanceProvider interface {
	CatalogProvenance() map[string]any
}

// RealmAvailableProvider reports whether a non-selector provider is configured
// and should appear in /realm options. WorkBuddy uses RealmSelector instead.
type RealmAvailableProvider interface {
	RealmAvailable() bool
}

// MediaRequest carries one normalized media operation. Body is the provider-
// shaped upstream body when the platform already built one; Params retains the
// semantic values used for pricing, validation and task rows.
type MediaRequest struct {
	Realm  string
	Model  string
	Prompt string
	Params map[string]any
	Body   map[string]any
	Raw    map[string]json.RawMessage
	KeyID  string
}

// MediaImageResult is one generated or edited image.
type MediaImageResult struct {
	URL           string
	B64JSON       string
	RevisedPrompt string
	Credit        *float64
	Created       int64
	Account       string
	Kind          string
	Warnings      []string
}

// MediaVideoSubmit is the result of submitting an asynchronous video task.
type MediaVideoSubmit struct {
	UpstreamID string
	Account    string
	Credit     *float64
	Warnings   []string
}

// MediaVideoState is one poll result. Status uses the upstream vocabulary
// queued|running|completed|failed|cancelled.
type MediaVideoState struct {
	Status string
	Credit *float64
	Videos []string
	Error  string
}

// MediaProvider is the optional provider hook for image and video generation.
// The platform never type-asserts a provider without first checking this hook.
type MediaProvider interface {
	Capabilities() Capabilities
	MediaModels(ctx context.Context, realm string) ([]Model, error)
	GenerateImage(ctx context.Context, req MediaRequest) (MediaImageResult, error)
	EditImage(ctx context.Context, req MediaRequest) (MediaImageResult, error)
	SubmitVideo(ctx context.Context, req MediaRequest) (MediaVideoSubmit, error)
	PollVideo(ctx context.Context, account, upstreamID string) (MediaVideoState, error)
	DownloadVideo(ctx context.Context, account, url, dest string) error
	CancelVideo(ctx context.Context, account, upstreamID string) error
}

// ProbeKind selects the probe state machine that owns a target.
type ProbeKind string

const (
	ProbeWindow ProbeKind = "window"
	ProbeQuota  ProbeKind = "quota"
)

// ProbeTarget identifies one account/model quota line.
type ProbeTarget struct {
	UID     string
	Realm   string
	Model   string
	ResetAt time.Time
}

// ProbeResult reports one minimal probe request.
type ProbeResult struct {
	OK      bool
	Status  int
	ResetAt time.Time
	Detail  string
}

// ProbeProvider is the optional provider hook for window and quota probes.
// The provider owns authentication and account selection; the platform owns
// due state, retry budgets and scheduling.
type ProbeProvider interface {
	ProbeTargets(ctx context.Context, kind ProbeKind, now time.Time, limit int) ([]ProbeTarget, error)
	Probe(ctx context.Context, kind ProbeKind, target ProbeTarget) (ProbeResult, error)
}

// MediaError is an optional provider media failure. Code uses the v1 stable
// vocabulary (invalid_request, no_media_model, no_route, auth, no_credit,
// rate_limit, rejected, upstream, empty_result) and is safe to map to HTTP
// status by the platform without parsing messages.
type MediaError struct {
	Code    string
	Message string
	Status  int
	Detail  string
}

func (e *MediaError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}
