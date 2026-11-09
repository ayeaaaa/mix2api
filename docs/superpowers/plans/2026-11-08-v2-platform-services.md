# mix2api v2 Platform Services Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement every non-UI platform service needed for Gate D: usage and pricing, runtime logs, Responses/Messages protocols, API keys and panel auth, proxy slots, media generation/tasks, agent configuration, web tools, probe/quota scheduling, update requests, daily usage guards, and the v1 data/contract compatibility checks that prove the slice is safe.

**Architecture:** Add an application-service layer under `internal/platform/*`. `internal/api` owns HTTP wire shapes and delegates to platform services; `internal/providers` keeps provider-specific behavior and implements optional hook interfaces structurally. `internal/app`/`cmd` compose the services. Platform services may import `internal/core` and `internal/providers`; providers never import `internal/platform` or `internal/api`.

**Tech Stack:** Go 1.22+ (module currently `go 1.22`), `net/http`, `httptest`, `encoding/json` with `UseNumber`, `database/sql` with the pure-Go `modernc.org/sqlite` driver, `go:embed`, `crypto/hmac`, PBKDF2-HMAC-SHA256 via `golang.org/x/crypto/pbkdf2` or a small in-repo implementation if dependency review prefers stdlib-only, `sync`, `context`, `time`.

**Spec:** `docs/superpowers/specs/2026-11-08-mix2api-v2-design.md`
**Master plan:** `docs/superpowers/plans/2026-11-08-mix2api-v2-master.md`
**Prior plans:** Foundation/Raccoon, CodeArts, WorkBuddy plans in this directory.

## Global Constraints

- The original v1.6.5 repository `/root/opencode/mix2api` is read-only. Never modify it, never run v2 against its live data directory.
- v2 is a single process, single binary, compile-time provider modules; no Go `plugin`, no runtime provider loading, no Node/Python runtime.
- Production builds use `CGO_ENABLED=0`. SQLite must be pure Go. Use `modernc.org/sqlite` (or a reviewed pure-Go equivalent); the database/sql contract must stay replaceable if the driver changes.
- Preserve v1.6.5 HTTP paths, aliases, status codes, JSON field names, error envelopes, SSE event semantics, headers, and timing behavior. Additive optional fields are allowed only where an old client can ignore them.
- Preserve v1.6.5 data files and formats: `usage.jsonl`, legacy `usage-summary.json`, aggregate checkpoints, `limit-events.jsonl`, `estimate-daily.json`, `usage-recent-index.json`, `logs.sqlite3`, `pricing-*.jsonl`, `pricing-overrides.json`, `pricing-markup.json`, `media/tasks.json`, `media/*-prices.json`, `accounts/integration-state.json`, `accounts/agent-backups/`, `accounts/settings.json`, `active_realm.json`, update request/status/backups files.
- Unknown JSON keys in `accounts/settings.json`, account files, state files, and update metadata must survive a read/modify/write cycle. Prefer merge writes; never replace a whole provider-owned document.
- `realm=all` remains WorkBuddy-only. No cross-provider model merging, no cross-provider fallback, no shared WorkBuddy/CodeArts/Raccoon credit semantics.
- API keys bind realm, model allowlist, account allowlist, and per-key concurrency. A bound key outranks `X-Realm`, `?realm`, and body `realm`, and suppresses WorkBuddy intl→cn fallback.
- Daily credits/token/model limits apply only to providers that opt in through `providers.LimitApplier`. Do not apply WorkBuddy credits to CodeArts or Raccoon, and do not sum credits across providers.
- Media, agents, probes, usage limits, and pricing hooks are optional narrow interfaces implemented by providers. A provider that does not implement a hook keeps its old behavior and returns the documented "not available" result.
- Automated tests use fake upstreams, injected fetchers/HTTP clients, synthetic or desensitized fixtures, and `httptest`. No test may call GitHub, OpenRouter, WorkBuddy, CodeArts, Raccoon, image/video hosts, or any real network.
- Real-account smoke tests are manual and outside this plan. They must not trigger check-in, rewards, media generation, or other mutable operations unless the owner provides a dedicated test account.
- Every task must keep `go test ./...` and `go vet ./...` green and must not introduce data races. Tasks touching shared maps, caches, task stores, or session stores also run `go test -race` on their packages.
- This plan is Markdown only. It does not implement production code. The only commit produced now is the docs-only commit described at the end of this plan.

## Review Focus

1. **Usage integrity and cache safety** — one JSONL line per completed request, no lost/merged/corrupt lines, checkpoints invalidated on truncation/replacement/price change, correct realm/account attribution, and no stale aggregate served after a rebuild.
2. **Data-format compatibility** — `logs.sqlite3` schema, settings merge behavior, pricing policy/timeline files, media task state, agent backups, and update request files must round-trip with v1.6.5. Unknown fields and file permissions must survive.
3. **Protocol parity** — Responses and Anthropic Messages request translation, non-stream aggregation, SSE event order, tool/image/reasoning handling, count_tokens, compaction, error envelopes, and termination/cancellation must match v1 behavior, not merely be "OpenAI-ish".
4. **Auth and binding isolation** — panel session vs API key, key realm/model/account allowlists, per-key concurrency, proxy-slot binding, and panel-only route gates must fail closed and must not leak across realms/providers.
5. **Operational safety** — media task ownership and signed downloads, local-only agent config writes, update request validation/backup completeness, no secrets in logs/exports, and no network side effects during tests.

---

## Service Boundaries and Dependency Rules

| Package | May import | Owns | Must not |
|---|---|---|---|
| `internal/platform/usage` | `core`, `platform/pricing` | usage paths, append, recent, snapshots, analytics, perf, series, by-account, remaining, checkpoint cache, export | provider packages, HTTP |
| `internal/platform/pricing` | `core` only | price snapshot, bands, OpenRouter fetch, policies, markup, cost lookup over `map[string]any` | provider packages, `platform/usage`, HTTP handlers |
| `internal/platform/logs` | `core`, `core/secrets` | log ring, SQLite store, redaction, query/export | provider packages, HTTP |
| `internal/platform/keys` | `core/config`, `core/secrets` | API key storage, matching, binding, per-key concurrency | provider packages, HTTP |
| `internal/platform/panel` | `core/config`, `core/secrets` | panel password, sessions, login rate limit, status | provider packages, HTTP |
| `internal/platform/settings` | `core/config`, `providers` | panel settings merge, realm selection, proxy-slot settings | provider internals |
| `internal/platform/proxy` | `core/config`, `providers` | proxy-slot storage/view/bindings | provider internals |
| `internal/platform/media` | `core`, `providers`, `platform/usage`, `platform/pricing` | media config, task store, workers, signed URLs, measured prices | provider-specific HTTP clients |
| `internal/platform/agents` | `core`, `providers` | client registry, apply/restore, model selection, backups | provider internals |
| `internal/platform/webtools` | `core` | search/fetch tools, SSRF guard, cache, tool definitions | provider internals |
| `internal/platform/probes` | `core`, `providers` | window/quota state, due scheduling, probe orchestration | provider internals |
| `internal/platform/update` | `core`, `core/secrets` | version metadata, update check, request/status/backups, executor contract | actual file replacement (Plan 6) |
| `internal/platform/limits` | `core`, `providers`, `platform/usage` | daily fold per realm, `LimitApplier` invocation | provider-specific guard logic |
| `internal/api` | `platform/*`, `providers`, `core` | routes, auth middleware, JSON/SSE wire shapes, ETag | provider-specific policy |
| `internal/providers` | `core` only | provider behavior and optional hooks | `platform/*`, `api` |
| `internal/app`/`cmd` | everything | composition, flags/env, lifecycle | business rules |

`internal/platform` is an application-service layer, not a replacement for `core`. `core` remains provider-neutral. `providers` implement narrow optional interfaces by method shape; they do not import `platform`. The only production files outside `internal/platform`/`internal/api` in this plan are provider-side hook implementations/adapters (`internal/providers/*`) and the `go.mod`/`go.sum` dependency records; they are deliberate because the interfaces belong to the provider contract, not to the platform layer.

## Shared Interfaces and Types

These are the contract definitions consumed by more than one task. Implement them in the package named in the comment; do not duplicate wire shapes in `internal/api`.

### Usage

```go
// internal/platform/usage
type Paths struct {
    Dir           string
    Log           string // usage.jsonl
    LimitEvents   string // limit-events.jsonl
    EstimateDaily string // estimate-daily.json
    RecentIndex   string // usage-recent-index.json
}

func ResolvePaths(accountsDir, usageDir string) (Paths, error)
func (p Paths) EnsureDir() error

type Row map[string]any // preserves unknown fields; numbers use json.Number

type RecentOptions struct {
    Limit int
    Page  int
    Realm string
}
type RecentPage struct {
    Total      int              `json:"total"`
    Page       int              `json:"page"`
    Limit      int              `json:"limit"`
    TotalPages int              `json:"total_pages"`
    USDCNY     float64          `json:"usd_cny"`
    Rows       []map[string]any `json:"rows"`
}

type ExportOptions struct {
    Realm    string
    Kind     string
    Since    *time.Time
    Until    *time.Time
    MaxRows  int
}

func (s *Store) Append(row map[string]any) error
func (s *Store) AppendRaw(line []byte) error
func (s *Store) Recent(opts RecentOptions) (RecentPage, error)
func (s *Store) Export(w io.Writer, opts ExportOptions) (int64, error)
```

`Row` is a map, not a struct, because v1.6.5 readers must preserve unknown keys and because different request kinds carry different optional fields. `At` is a float epoch; `iso` is `YYYY-MM-DDTHH:MM:SS`; `outcome` is `completed|client_aborted|upstream_aborted|failed`. Missing `outcome` on old rows falls back to `failed` when `error` is present, otherwise `completed`. Missing `kind` on old rows means `chat`.

### Pricing

```go
// internal/platform/pricing
type Meta struct {
    USDCNY      float64 `json:"usd_cny"`
    GeneratedAt string  `json:"generated_at"`
    BaseCurrency string `json:"base_currency"`
}
type Snapshot struct {
    Meta   Meta             `json:"meta"`
    Models map[string]Entry `json:"models"`
}
type Rates struct {
    InputCacheHit  float64 `json:"input_cache_hit"`
    InputCacheMiss float64 `json:"input_cache_miss"`
    Output         float64 `json:"output"`
}
type Band struct {
    Days           []string `json:"days,omitempty"`
    Start          int      `json:"start,omitempty"`
    End            int      `json:"end,omitempty"`
    MinPromptTokens int64   `json:"min_prompt_tokens,omitempty"`
    Flat           Rates    `json:"flat"`
}
type Entry struct {
    Display        string  `json:"display"`
    Source         string  `json:"source"`
    Currency       string  `json:"currency"`
    Unit           float64 `json:"unit"`
    ORID           string  `json:"or_id,omitempty"`
    Via            string  `json:"via,omitempty"`
    InheritedFrom  string  `json:"inherited_from,omitempty"`
    Flat           Rates   `json:"flat"`
    Bands          []Band  `json:"bands,omitempty"`
}
type Cost struct {
    Known         bool
    Disabled      bool
    CNY           float64
    Band          *int
    Rates         *Rates
    Unit          float64
    Currency      string
    USDCNY        float64
    ORID          string
    Source        string
    SourceAt      string
    Via           string
    InheritedFrom string
    OverrideFrom  string
    Backfilled    bool
    Policy        string
}

func Load(accountsDir, usageDir string) (*Snapshot, error)
func CostForRow(row map[string]any) Cost
func CurrentPolicyID(model string) string
func EnsurePolicy(model string) (string, error)
```

`Cost.Known == false` must never be turned into `0`. `Disabled == true` means the pricing master switch is off; aggregates must not count that as "missing a price". `Rates`/`Band`/`Policy` are diagnostic fields for the panel, not billing.

### Logs

```go
// internal/platform/logs
type Entry struct {
    ID    int64  `json:"id"`
    TS    string `json:"ts"`
    Time  string `json:"time"`
    Level string `json:"level"`
    Tag   string `json:"tag"`
    Msg   string `json:"msg"`
}
type Query struct {
    Limit   int
    Level   string
    Tag     string
    Search  string
    SinceID int64
}
type Result struct {
    Total int     `json:"total"`
    Logs  []Entry `json:"logs"`
    MaxID int64   `json:"max_id"`
}
type Stats struct {
    Rows   int64  `json:"rows"`
    Oldest string `json:"oldest"`
    Newest string `json:"newest"`
    Bytes  int64  `json:"bytes"`
    Path   string `json:"path,omitempty"`
}

type Store interface {
    Append(Entry) error
    Query(Query) (Result, error)
    Clear() error
    Stats() (Stats, error)
    Close() error
}
```

### API keys and panel

```go
// internal/platform/keys
type Entry struct {
    ID               string   `json:"id"`
    Name             string   `json:"name"`
    Key              string   `json:"key"`
    Realm            string   `json:"realm"`
    Models           []string `json:"models"`
    Accounts         []string `json:"accounts"`
    ConcurrencyLimit int      `json:"concurrency_limit"`
    Enabled          bool     `json:"enabled"`
    CreatedAt        string   `json:"created_at"`
    DeletedAt        string   `json:"deleted_at,omitempty"`
}
func (s *Store) List(includeDeleted bool) ([]Entry, error)
func (s *Store) Save(entries []Entry, deleteIDs []string) ([]Entry, error)
func Match(entries []Entry, supplied string) (*Entry, bool)
func AllowsModel(entry Entry, model string) bool
func AllowsAccount(entry Entry, uid string) bool
func (s *Store) AuthRequired() bool      // false when settings auth_disabled=true
func (s *Store) SetAuthDisabled(bool) error

// internal/platform/panel
type Sessions struct{}
func (s *Sessions) Create() string
func (s *Sessions) Valid(token string) bool
func (s *Sessions) Revoke(token string)
func (s *Sessions) RevokeAll()
```

### Media

```go
// internal/platform/media
type Task struct {
    ID          string
    Status      string // waiting|submitting|queued|running|completed|rejected|failed|lost|cancelled
    Model       string
    Realm       string
    Account     string
    KeyID       string
    UpstreamID  string
    Seconds     int
    Resolution  string
    AspectRatio string
    EnableAudio bool
    ImageToVideo bool
    HasReference bool
    PromptHead  string
    PromptLen   int
    CreatedAt   int64
    StartedAt   *int64
    CompletedAt *int64
    Credit      *float64
    Error       string
    Videos      []string
    Warnings    []string
}
type TaskStore struct{}
func (s *TaskStore) Create(model string, params map[string]any, accountUID string, body map[string]any, upstreamID, realm, keyID string) (Task, error)
func (s *TaskStore) CreatePending(model string, params map[string]any, body map[string]any, realm, keyID string) (Task, error)
func (s *TaskStore) Get(id string) (Task, bool)
func (s *TaskStore) List(limit int) []Task
func (s *TaskStore) Cancel(id string) (bool, error)
func (s *TaskStore) Wait(id string, timeout time.Duration) (Task, bool)
func (s *TaskStore) Stats() map[string]any
func (s *TaskStore) Sign(id string, index int, ttl time.Duration) string
func (s *TaskStore) Verify(id string, index int, token string) bool
func (s *TaskStore) ContentPath(id string, index int) (string, bool)
```

### Updates

```go
// internal/platform/update
type Status struct {
    CurrentVersion  string `json:"current_version"`
    LatestVersion   string `json:"latest_version,omitempty"`
    LatestTag       string `json:"latest_tag,omitempty"`
    UpdateAvailable bool   `json:"update_available"`
    Enabled         bool   `json:"enabled"`
    Checking        bool   `json:"checking"`
    LastAttempt     string `json:"last_attempt,omitempty"`
    LastSuccess     string `json:"last_success,omitempty"`
    LastError       string `json:"last_error,omitempty"`
    ReleaseURL      string `json:"release_url,omitempty"`
    PublishedAt     string `json:"published_at,omitempty"`
}
type Installation struct {
    State   string `json:"state"`
    Version string `json:"version"`
    Message string `json:"message"`
}
```

### Provider hooks

These live in `internal/providers/hooks.go` so that provider modules can implement them without importing `internal/platform` or `internal/api`.

```go
// internal/providers/hooks.go
type Capabilities struct {
    MediaGeneration bool
    MediaImages     bool
    MediaVideos     bool
    ImageInputInChat bool
    AgentModelSpecs bool
    WindowProbe     bool
    QuotaScan       bool
    UsageLimits     bool
}

type LimitApplier interface {
    // values: {"reserve_credits":{"global":10,"intl":20}, ...}
    // usage:  {"tokens":{uid:int64},"credits":{uid:float64},
    //          "models":{uid:{model:int64}}}
    ApplyLimits(values map[string]any, usage map[string]any)
}

type SettingsContributor interface {
    SettingsView(ctx context.Context) (map[string]any, error)
    SaveSettings(ctx context.Context, patch map[string]any) error
}

type RealmSelector interface {
    ActiveRealm(ctx context.Context) string
    SetActiveRealm(ctx context.Context, realm string) error
    RealmOptions() []string
}

type ProxySlotBinder interface {
    // ApplyProxySlots re-resolves every account binding and returns how many
    // accounts were unbound because their slot disappeared.
    ApplyProxySlots(slots transport.Slots) int
    // ProxySlotBindings reports enabled-account counts per slot id.
    ProxySlotBindings() map[string]int
}

type AgentModelSpec struct {
    ID               string
    Name             string
    ContextWindow    int64
    MaxOutputTokens  int64
    ReasoningEfforts []string
    SupportsVision   bool
}
type AgentModelSpecProvider interface {
    AgentModelSpecs(ctx context.Context) ([]AgentModelSpec, error)
}

type MediaRequest struct {
    Realm  string
    Model  string
    Prompt string
    Params map[string]any
    Body   map[string]any
    Raw    map[string]json.RawMessage
    KeyID  string
}
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
type MediaVideoSubmit struct {
    UpstreamID string
    Account    string
    Credit     *float64
    Warnings   []string
}
type MediaVideoState struct {
    Status string // queued|running|completed|failed|cancelled
    Credit *float64
    Videos []string
    Error  string
}
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

type ProbeKind string
const (
    ProbeWindow ProbeKind = "window"
    ProbeQuota  ProbeKind = "quota"
)
type ProbeTarget struct {
    UID     string
    Realm   string
    Model   string
    ResetAt time.Time
}
type ProbeResult struct {
    OK      bool
    Status  int
    ResetAt time.Time
    Detail  string
}
type ProbeProvider interface {
    ProbeTargets(ctx context.Context, kind ProbeKind, now time.Time, limit int) ([]ProbeTarget, error)
    Probe(ctx context.Context, kind ProbeKind, target ProbeTarget) (ProbeResult, error)
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
type UsageSnapshotReporter interface {
    UsageSnapshot() UsageSnapshot
}
```

WorkBuddy implements `Capabilities` (media generation, image input, agent specs, window probe, quota scan, usage limits), `MediaProvider`, `AgentModelSpecProvider`, `ProbeProvider`, `LimitApplier`, `SettingsContributor`, and `RealmSelector`. CodeArts implements `Capabilities` with `ImageInputInChat=true` and media generation/probe/quota/limits false; it may implement `AgentModelSpecProvider`. Raccoon implements `Capabilities` with all optional capabilities false; no media/agents/probe/limit hooks.

## Preserved HTTP Surface

Platform routes are mounted exactly once. Provider-owned routes are not duplicated. Auth below is the effective v1 gate, including the panel-prefix gate.

| Method | Path | Auth | Owner | Notes |
|---|---|---|---|---|
| GET | `/health` | public | `api` | v1 shape plus provider map; no secrets |
| GET | `/models`, `/v1/models` | API key or panel | `api` | existing generic route |
| POST | `/chat/completions`, `/v1/chat/completions` | API key or panel | `api` | existing generic route |
| POST | `/completions`, `/v1/completions` | API key or panel | `api` | legacy chat-completions alias; no separate completion semantics |
| POST | `/responses`, `/v1/responses` | API key or panel | `api` | Responses request/response translation |
| POST | `/responses/compact`, `/v1/responses/compact` | API key or panel | `api` | legacy compaction alias |
| POST | `/messages`, `/v1/messages` | API key or panel (Anthropic error envelope) | `api` | default disabled |
| POST | `/messages/count_tokens`, `/v1/messages/count_tokens` | same as Messages | `api` | local estimate |
| GET | `/usage`, `/v1/usage` | panel prefix | `api`/platform | snapshot |
| GET | `/usage/recent` | panel prefix | `api`/platform | paginated rows |
| GET | `/usage/analytics` | panel prefix | `api`/platform | window/all-time analytics |
| GET | `/usage/perf` | panel prefix | `api`/platform | latency percentiles |
| GET | `/usage/timeseries` | panel prefix | `api`/platform | token/credit buckets |
| GET | `/usage/by-account` | panel prefix | `api`/platform | per-account totals |
| GET | `/usage/remaining` | panel prefix | `api`/platform | 24h window estimate |
| GET | `/logs` | panel prefix | `api`/platform | ring or SQLite |
| POST | `/logs/clear` | panel | `api`/platform | clears both |
| GET | `/logs/export` | panel prefix | `api`/platform | text file |
| GET | `/pricing` | panel prefix | `api`/platform | pricing status |
| POST | `/pricing/refresh` | panel | `api`/platform | one refresh |
| POST | `/pricing/mapping` | panel | `api`/platform | runtime override |
| GET | `/panel/status` | public | `api`/platform | login screen state |
| POST | `/panel/login` | public + rate limit | `api`/platform | token in response only |
| POST | `/panel/logout` | panel | `api`/platform | revoke token |
| POST | `/panel/password` | panel | `api`/platform | rotate/revoke sessions |
| GET | `/settings` | panel | `api`/platform | merged settings view |
| POST | `/settings/save` | panel | `api`/platform | platform + contributor merge |
| GET | `/settings/reveal` | panel | `api`/platform | one API key value |
| GET/POST | `/realm` | panel | `api`/platform | WorkBuddy active realm |
| GET | `/proxy/slots` | panel | `api`/platform | slots + bound counts |
| POST | `/proxy/slots/save` | panel | `api`/platform | replace list, stable ids |
| POST | `/proxy/slots/test` | panel | `api`/platform | exit probe (injected network) |
| POST | `/proxy/discover` | panel | `api`/platform | candidate slots |
| GET | `/media/models`, `/v1/media/models` | API key or panel | `api`/platform | media catalog, even when disabled |
| GET | `/v1/media/tasks` | API key or panel | `api`/platform | visible tasks + stats |
| POST | `/images/generations`, `/v1/images/generations` | API key | `api`/platform | OpenAI Images shape |
| POST | `/images/edits`, `/v1/images/edits` | API key | `api`/platform | data-URL input |
| POST | `/videos`, `/v1/videos` | API key | `api`/platform | 202 + task; `?wait=1` |
| GET | `/v1/videos/{id}` | API key or panel | `api`/platform | task status |
| GET | `/v1/videos/{id}/content` | signed token or API key | `api`/platform | mp4 bytes |
| POST | `/v1/videos/{id}/cancel` | API key | `api`/platform | queued/waiting only |
| GET | `/agents`, `/agents/available` | panel | `api`/platform | local-only gate for config writes |
| GET/POST | `/agents/model-selection` | panel | `api`/platform | selected model ids |
| POST | `/agents/apply`, `/agents/restore` | panel + loopback | `api`/platform | local config only |
| GET | `/agents/cli` | public | `api`/platform | standalone configurator source |
| GET | `/updates` | panel prefix | `api`/platform | last check, never checks |
| GET | `/updates/backups` | panel prefix | `api`/platform | rollback inventory |
| POST | `/updates/check` | panel | `api`/platform | one manual check |
| POST | `/updates/install` | panel | `api`/platform | queue only |
| POST | `/updates/rollback` | panel | `api`/platform | queue only |
| POST | `/quota-scan/run` | panel | `api`/platform | force due quota lines |
| GET | `/accounts*` | panel | WorkBuddy provider | provider-owned |
| GET/POST | `/activity/history`, `/tasks*` | panel | WorkBuddy provider | provider-owned; do not duplicate |
| GET/POST | `/codearts/*`, `/raccoon/*` | panel | provider modules | provider-owned |

Panel-prefix gates from v1.6.5 must be preserved even for unknown paths under the prefix: `/accounts`, `/usage`, `/v1/usage`, `/activity`, `/tasks`, `/scheduler`, `/settings`, `/updates`, `/logs`, `/console`, `/pricing`, `/agents`, `/codearts`, `/raccoon`. Admin routes not in that list (`/panel/*`, `/proxy/*`, `/quota-scan/run`) enforce panel auth individually.

## v1.6.5 Reference Map

| Area | v1.6.5 modules / functions | v1.6.5 behavior tests |
|---|---|---|
| Usage append | `app/wb_proxy.py` `record_usage`, `record_error`, `_persist_usage`; `app/wb_usage_io.py` `append_line`; `app/usage/README.txt` | `_test_usage_atomic_append.py`, `_test_usage_range.py`, `_test_usage_etag_cache.py` |
| Usage summary/recent | `app/wb_proxy.py` `usage_snapshot`, `_usage_snapshot_uncached`, `recent_usage`, `count_usage_rows`, `_usage_recent_*` | `_test_usage_range.py`, `_test_recent_index.py`, `_test_usage_aggregate_fold.py`, `_test_usage_cache_*.py` |
| Usage analytics/perf/series | `app/wb_proxy.py` `compute_usage_analytics`, `perf_stats`, `usage_timeseries`, `usage_by_account`, `remaining_usage`, `apply_daily_guards`, `daily_usage_stats` | `_test_analytics_*.py`, `_test_perf_incremental.py`, `_test_usage_timeseries.py`, `_test_usage_series_minutes.py`, `_test_remaining_*.py` |
| Logs | `app/wb_logstore.py`; `app/wb_proxy.py` `LOG_BUFFER`, `add_log_entry`, `get_logs`, `_get_logs_export`, `clear_logs`, `_mask_text`, `_redact_account_names` | `_test_console_tail.py`, `_test_release_assets.py` (SQLite packaging) |
| Pricing | `app/wb_pricing.py`; `app/wb_modelsdev.py`; `app/pricing/pricing.json`; `app/wb_proxy.py` `_get_pricing`, `_route_pricing_*`, `_row_cost`, `_fold_cost` | `_test_pricing*.py`, `_test_modelsdev.py` |
| Media | `app/wb_media.py`; `app/wb_media_client.py`; `app/wb_media_tasks.py`; `app/wb_proxy.py` media routes and `media_*` helpers | `_test_media_*.py` |
| Agents | `app/wb_agents.py`; `app/wb_webprompts.py`; `app/wb_proxy.py` `_get_agents*`, `_handle_agents*` | `_test_agents*.py`, `_test_dsh_*.py`, `_test_web_agent.py` |
| Protocol: Responses | `app/wb_proxy.py` `_responses_input_to_messages`, `responses_to_chat`, `chat_to_response`, `stream_responses_events`, `_responses_*_response`, `_responses_usage` | `_test_request_contract.py`, `_test_custom_tools.py`, `_test_namespace_tools.py`, `_test_remote_compaction.py`, `_test_orphan_function_call_output.py`, `_test_max_output_tokens.py` |
| Protocol: Messages | `app/wb_proxy.py` `messages_to_chat`, `chat_to_messages`, `stream_messages_events`, `_handle_messages_count_tokens`, `anthropic_*` | `_test_anthropic_messages.py`, `_test_anthropic_http.py` |
| API keys/panel/settings | `app/wb_settings.py` (`api_keys`, `set_api_keys`, `key_allows_model`, `key_allows_account`, `PanelSessions`, password helpers, `proxy_slots`); `app/wb_proxy.py` `_handle_panel`, `_get_panel_status`, `_handle_settings_save`, `_handle_proxy_slots` | `_test_api_key*.py`, `_test_key_*.py`, `_test_panel_*.py`, `_test_settings_preservation.py`, `_test_proxy_slots*.py` |
| Web tools/probes | `app/wb_webtools.py`; `app/wb_probes.py`; `app/wb_window_probe.py`; `app/wb_quota_scan.py`; `app/wb_tasks.py`; `app/wb_activity.py`; `app/wb_webagent.py` | `_test_local_web_tools.py`, `_test_web_agent.py`, `_test_window_probe.py`, `_test_quota_scan.py`, `_test_output_probes.py`, `_test_activity_history.py` |
| Updates | `app/wb_updates.py`; `app/wb_update_request.py`; `app/wb_update_apply.py`; `app/wb_update_external.py`; `app/wb_update_portable.py`; `app/wb_proxy.py` update routes | `_test_update_check.py`, `_test_update_install.py`, `_test_update_rollback.py`, `_test_portable_update.py` |

Note: v1.6.5 has `app/wb_logstore.py`, not `app/wb_logs*.py`. This plan treats `wb_logstore.py` as the `wb_logs*` reference. v1.6.5 has no `/usage/export` HTTP route; usage export is the JSONL ledger itself plus the account/log export routes. Task 1 provides an internal bounded `Export` helper and Task 3 exposes no new public usage route.

## Data Compatibility Matrix

| v1 path | v2 owner | Required behavior |
|---|---|---|
| `usage/usage.jsonl` | `platform/usage` | Append one compact JSON object per line with `\n`; open `O_APPEND`; never rewrite existing rows; tolerate unknown fields; durable enough that a crash does not merge lines |
| `usage/usage-summary.json` | none | Legacy file is not written and not required for correctness. Preserve it if present; never treat it as the source of truth |
| `usage/usage-aggregate-cache-<kind>.json` | `platform/usage` | Read/write v1 schema: `{schema, kind, entries:[...]}`; each entry carries offset/log identity/tail signature and kind-specific state. Legacy single-file `usage-aggregate-cache.json` is read as fallback, never written |
| `usage/limit-events.jsonl` | `platform/usage` | Append one event per 429 window; fields `uid`, `model`, `reset`, `sample`, `at`; tolerate bad lines |
| `usage/estimate-daily.json` | `platform/usage` | Preserve `{schema, days:{date:{budgets,segments}}}`; atomic write; keep 3 budget days and 2 segment days |
| `usage/usage-recent-index.json` | `platform/usage` | Preserve row-offset index schema; validate log identity/segment width/tail; fall back to backward scan on any mismatch |
| `usage/logs.sqlite3` | `platform/logs` | Exact table `logs(id INTEGER PRIMARY KEY, ts TEXT NOT NULL, time TEXT NOT NULL, level TEXT NOT NULL, tag TEXT NOT NULL, msg TEXT NOT NULL)` plus `idx_logs_level`, `idx_logs_tag`; journal mode DELETE; off by default |
| `pricing-policies.jsonl`, `pricing-timeline.jsonl` | `platform/pricing` | Preserve JSONL policy records and timeline refs; append-only; no rewrite of old records |
| `pricing-overrides.json`, `pricing-markup.json` | `platform/pricing` | Preserve both maps; overrides map hub id → OpenRouter id; markup positive factor per model |
| `media/tasks.json` | `platform/media` | Preserve task rows/state names; prompt only `prompt_head`/`prompt_len`; artifacts under `media/` |
| `media/video-prices.json`, `media/image-prices.json` | `platform/media` | Measured prices only; keys `model`, `model|resolution`, `model|resolution|ref`; never invent a price |
| `accounts/settings.json` | `platform/keys`, `platform/settings`, providers | Merge writes; unknown keys survive; API keys, panel password, proxy slots, media/pricing/log/update settings keys preserved |
| `accounts/integration-state.json`, `accounts/agent-backups/` | `platform/agents` | Preserve applied-file hashes, backup paths, timestamps; backups keep newest 10 per basename |
| `active_realm.json` | `platform/settings`/WorkBuddy | Preserve `realm`, `updated_at`, `updated_iso`; read settings fallback |
| update `request.json`/`status.json`/`backups.json` | `platform/update` | Preserve fields and ownership/mode checks; requests are create-only `0600`; status/backups are read-only and size/owner-checked |
| `accounts/model_windows.json`, `accounts/quota_scan.json`, `accounts/output_probes.json` | `platform/probes` | Preserve schemas/field names; atomic writes; prune by age |

## Task Overview

| Task | Depends on | Deliverable |
|---|---|---|
| 1 | — | Usage JSONL store, paths, recent, export |
| 2 | 1 | Pricing snapshot, bands, OpenRouter fetch, policy/timeline, markup, cost |
| 3 | 1, 2 | Usage snapshot/analytics/perf/series/by-account/remaining/cache |
| 4 | 1 | Runtime log ring, SQLite log store, redaction, export |
| 5 | 3 | Responses + compact routes and SSE |
| 6 | 3 | Anthropic Messages + count_tokens routes and SSE |
| 7 | 1, 3 | API keys, panel auth/settings/realm, proxy slots |
| 8 | 2, 7 | Media images/videos, task store, provider hooks |
| 9 | 7 | Agent config apply/restore/selection/CLI |
| 10 | 3, 8 | Web tools, probes/window/quota scan, provider-owned task/activity audit |
| 11 | 7 | Update check/install/rollback request, backup/executor contract |
| 12 | 1–11 | Daily guard integration, platform wiring, contract suite, Gate D |

Execute in order. Tasks 5 and 6 may be reviewed together because they share the chat-event normalizer, but each has its own commit. Tasks 8–11 depend on the key/panel contract from Task 7 and must not invent a second auth path.

---

### Task 1: Usage JSONL store, paths, recent, export

**Files:**
- Create: `internal/platform/usage/paths.go`
- Create: `internal/platform/usage/row.go`
- Create: `internal/platform/usage/store.go`
- Create: `internal/platform/usage/recent.go`
- Create: `internal/platform/usage/export.go`
- Test: `internal/platform/usage/paths_test.go`
- Test: `internal/platform/usage/store_test.go`
- Test: `internal/platform/usage/recent_test.go`
- Test: `internal/platform/usage/export_test.go`

**Interfaces:**
- `Paths`, `Row`, `RecentOptions`, `RecentPage`, `ExportOptions`, `Store.Append`, `Store.Recent`, `Store.Export` from the shared section.
- `ResolvePaths` precedence: explicit `usageDir` argument, then `USAGE_DIR`, then `WB_PROXY_USAGE_DIR`, then `<working dir>/usage`. `accountsDir` is separate and is not created here.
- `Append` accepts the row written by `record_usage`/`record_error`; it must not add, drop, or reorder semantic fields. It serializes with `json.Encoder` configured `SetEscapeHTML(false)` and a single `Write` of the complete line.

- [ ] **Step 1: Write failing tests**

Add table-driven tests covering:
- `TestResolvePathsPrecedence` — explicit dir wins; `USAGE_DIR` beats `WB_PROXY_USAGE_DIR`; defaults to `usage`; filenames are exactly `usage.jsonl`, `limit-events.jsonl`, `estimate-daily.json`, `usage-recent-index.json`.
- `TestAppendWritesExactlyOneJSONLine` — row with unknown `future_field` round-trips; output is one line ending `\n`; no HTML escaping for `<`, `>`, `&`.
- `TestAppendConcurrentWritersDoNotLoseRows` — 8 goroutines × 300 rows; every line parses; count is 2400; no partial line.
- `TestAppendToleratesMissingParentDirectory` — creates `usage/` with v1-compatible permissions.
- `TestRecentPaginationAndRealmFilter` — page 1 is newest; page 2 continues; realm filter matches `row.realm` and legacy `row_realm` fallback; `limit` clamps to 1..1000; `page` clamps; `total_pages` matches v1.
- `TestRecentSkipsBlankAndMalformedLines` — malformed middle line is skipped, not fatal; trailing half-line is treated as a row only when complete; count stays consistent with v1 backward scan.
- `TestRecentLegacyRowsWithoutRealm` — rows with only `model`/`account` use the same fallback as v1 `row_realm`.
- `TestExportFiltersAndPreservesOrder` — realm/kind/since/until filter; output is NDJSON in file order; `MaxRows` caps; no mutation of the source file.
- `TestLegacyUsageSummaryIsNeverWritten` — pre-create `usage-summary.json`; append rows; file bytes unchanged.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/platform/usage -run 'TestResolvePaths|TestAppend|TestRecent|TestExport|TestLegacy'`
Expected: FAIL, missing package/types.

- [ ] **Step 3: Implement**

- `paths.go`: resolve once at startup, `filepath.Clean`, do not create `accounts/`. `EnsureDir` uses `0755` for the usage directory (v1 creates it with default umask).
- `row.go`: normalize only fields readers need (`at`, `iso`, `model`, `stream`, `outcome`, `kind`, `realm`, account/key attribution). Keep the original map for output. Implement `RowOutcome` and `RowRealm` helpers matching `row_outcome`/`row_realm` from `app/wb_proxy.py`.
- `store.go`: one process-wide `sync.Mutex` for writes; `os.OpenFile(log, O_WRONLY|O_CREATE|O_APPEND, 0644)`; encode to a `bytes.Buffer` first and issue one `Write`; on `ENOSPC`/permission error return it so callers can log without failing the request. Do not use a text-mode buffered writer.
- `recent.go`: implement v1 backward chunk scan (256 KiB chunks) and the row-offset index reader when available. The index is an accelerator only; any malformed/mismatched index falls back to the scan. `Recent` decorates rows only with fields owned here (`key_name` is Task 3). Keep `total`, `page`, `limit`, `total_pages`, `usd_cny` (0 until Task 2 is wired), `rows`.
- `export.go`: bounded streaming copy of filtered rows. It is an internal/tooling export; do not register `/usage/export` because v1.6.5 has no such public route. The existing `/accounts/export` and `/logs/export` remain owned by their current modules.
- Never write `usage-summary.json`, never rotate/rename `usage.jsonl`, never truncate on startup.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/platform/usage`
Expected: PASS.
Run: `go test -race ./internal/platform/usage`
Expected: PASS, no race.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/usage
git commit -m "feat(platform): add usage jsonl store and export"
```

---

### Task 2: Pricing snapshot, bands, OpenRouter, policy, markup, cost

**Files:**
- Create: `internal/platform/pricing/data/pricing.json` (copy of v1 `app/pricing/pricing.json`, MIT attribution preserved)
- Create: `internal/platform/pricing/snapshot.go`
- Create: `internal/platform/pricing/bands.go`
- Create: `internal/platform/pricing/estimate.go`
- Create: `internal/platform/pricing/policies.go`
- Create: `internal/platform/pricing/openrouter.go`
- Create: `internal/platform/pricing/refresher.go`
- Test: `internal/platform/pricing/snapshot_test.go`
- Test: `internal/platform/pricing/estimate_test.go`
- Test: `internal/platform/pricing/policies_test.go`
- Test: `internal/platform/pricing/openrouter_test.go`
- Test: `internal/platform/pricing/refresher_test.go`

**Interfaces:**
- `Snapshot`, `Entry`, `Rates`, `Band`, `Cost`, `Load`, `CostForRow`, `CurrentPolicyID`, `EnsurePolicy` from the shared section.
- `Load` precedence: `WB_PRICING_FILE`, then `<install>/pricing/pricing.json`, then embedded `data/pricing.json`. A malformed external file falls back to the embedded copy; no startup failure.
- `FetchOpenRouter(ctx, client, baseURL)` takes an injected `*http.Client` and base URL. Production passes `https://openrouter.ai/api/v1/models`; tests pass an `httptest.Server`.
- `Refresher` takes an interval source (`pricing_refresh_minutes` with one-time `pricing_refresh_hours` migration), a fetch function, and a logger. It never blocks a request.

- [ ] **Step 1: Write failing tests**

Port the semantic cases from v1:
- `TestSnapshotLoadsEmbeddedAndFileOverride` — >10 models, every entry USD, every `flat` has cache-hit/cache-miss/output, bands carry exactly one condition (length threshold or UTC window, never both/neither).
- `TestBandSelectionByUTCTime` — weekend band, wrapping window, no-match falls back to flat.
- `TestLengthBandsTakeTightestMatch` — ascending thresholds; below lowest uses base.
- `TestComputeRowCacheSplitAndUSDConversion` — cached vs uncached prompt, completion, USD→CNY at snapshot rate.
- `TestUnknownModelNeverGuesses` — `Known=false`, `CNY=0`, `Rates=nil`, `Band=nil`; no zero/guessed price.
- `TestPricingDisabled` — master switch off returns `Disabled=true`, empty details, no policy minted, and aggregates do not label it `cost_missing`.
- `TestMarkupFactor` — `pricing-markup.json` factor >0 scales; invalid/negative/zero/corrupt falls back to 1.0.
- `TestPolicyTimelineLookup` — row at time T uses the policy in force at T; missing policy falls back with `Backfilled=true`; current policy id is stable/dedupable.
- `TestOpenRouterNormalization` — injected fake response maps model ids/rates/bands/variants; no real network.
- `TestRefreshIntervalMigration` — legacy hours converted once to minutes; 0 disables; cap 43200; invalid falls back to 5; setter clamps.
- `TestNextRefreshWait` — first run after delay; interval applies; stop is prompt.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/platform/pricing`
Expected: FAIL, missing package/types.

- [ ] **Step 3: Implement**

- Embed `data/pricing.json`; keep `Load` mtime/size cached and invalidate on write. Preserve `meta.usd_cny`, `models`, `flat`, `bands`, `via`, `inherited_from`, `override_from`.
- `bands.go`: UTC weekday/hour/minute and prompt-length band selection; the tightest applicable length threshold wins; wrap-around time windows allowed; equal start/end invalid.
- `estimate.go`: `prompt_tokens` split by `cached_tokens`/best cache alias; apply markup; `unit=1_000_000`; round only at the wire boundary; return `Known=false` for unknown models and `Disabled=true` for the master switch.
- `policies.go`: read/write `pricing-policies.jsonl` and `pricing-timeline.jsonl` append-only; runtime overrides in `pricing-overrides.json`; current policy id derived from `(model, flat, bands, usd_cny)`; `EnsurePolicy` mints on demand only when pricing is enabled; never rewrite an old policy record.
- `openrouter.go`: normalize `data[]` entries; `pricing.prompt/completion/input_cache_read` conversion; preserve conditional bands; variant inheritance rules (`-lkeap`, `-taiji`, `-volc`, `-sg`, `-f`, `-dev`, `-x`); runtime overrides win over source overrides; unmatched variants stay unpriced.
- `refresher.go`: modeled after v1 `PriceRefresher`; no overlapping runs; failures log a short reason and keep the previous snapshot; no secrets/URL bodies in logs.
- Do not import provider packages. Do not write `pricing.json` from settings; it is an external operator override.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/platform/pricing`
Expected: PASS.
Run: `go test -race ./internal/platform/pricing`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/pricing
git commit -m "feat(platform): add pricing engine and policy cache"
```

---

### Task 3: Usage snapshot, analytics, perf, series, by-account, remaining, cache

**Files:**
- Create: `internal/platform/usage/service.go`
- Create: `internal/platform/usage/range.go`
- Create: `internal/platform/usage/snapshot.go`
- Create: `internal/platform/usage/analytics.go`
- Create: `internal/platform/usage/perf.go`
- Create: `internal/platform/usage/timeseries.go`
- Create: `internal/platform/usage/byaccount.go`
- Create: `internal/platform/usage/remaining.go`
- Create: `internal/platform/usage/cache.go`
- Create: `internal/platform/usage/recorder.go`
- Modify: `internal/platform/usage/recent.go`
- Create: `internal/api/usage.go`
- Test: `internal/platform/usage/range_test.go`
- Test: `internal/platform/usage/snapshot_test.go`
- Test: `internal/platform/usage/analytics_test.go`
- Test: `internal/platform/usage/perf_test.go`
- Test: `internal/platform/usage/timeseries_test.go`
- Test: `internal/platform/usage/byaccount_test.go`
- Test: `internal/platform/usage/remaining_test.go`
- Test: `internal/platform/usage/cache_test.go`
- Test: `internal/platform/usage/recorder_test.go`
- Test: `internal/api/usage_test.go`

**Interfaces:**
```go
type Range struct { Since, Until *time.Time }
func ParseRange(raw string, since, until *time.Time) Range

type Service struct { Store *Store; Pricing *pricing.Snapshot; Cache *Cache; Now func() time.Time }
func (s *Service) Snapshot(ctx context.Context, opts SnapshotOptions) (map[string]any, error)
func (s *Service) Analytics(ctx context.Context, opts AnalyticsOptions) (map[string]any, error)
func (s *Service) Perf(ctx context.Context, opts PerfOptions) (map[string]any, error)
func (s *Service) Timeseries(ctx context.Context, opts SeriesOptions) (map[string]any, error)
func (s *Service) ByAccount(ctx context.Context) (map[string]any, error)
func (s *Service) Remaining(ctx context.Context) (map[string]any, error)
func (s *Service) Recent(ctx context.Context, opts RecentOptions) (RecentPage, error)

type RecordInput struct {
    At             time.Time
    Model          string
    RequestedModel string
    Realm          string
    Account        string
    KeyID          string
    Kind           string
    Outcome        string
    Stream         bool
    Usage          map[string]any
    ElapsedMS      int64
    TTFTMS         int64
    GenMS          int64
    Effort         string
    Media          map[string]any
}
type Recorder struct{ Store *Store; Now func() time.Time }
func (r *Recorder) Record(ctx context.Context, in RecordInput) error
```
- `Range` matches v1 `range_window`: `today|day|1d`, `week|w`, `month|m`, `custom`; local midnight/Monday/first-of-month; custom swaps reversed bounds and drops unparseable/negative values; everything else is unbounded.
- `SnapshotOptions`/`AnalyticsOptions`/`PerfOptions`/`SeriesOptions` carry realm plus range/since/until; `SeriesOptions` also `BucketSeconds`.
- `Snapshot` is the v1 "usage summary" API view (`/usage`). The legacy `usage-summary.json` file is not its storage and is not written or read for correctness.
- `Cache` writes `usage-aggregate-cache-<kind>.json` with `schema`, `kind`, `entries`; kinds: `snapshot`, `by_account`, `analytics`, `series`, `remaining`. Legacy `usage-aggregate-cache.json` is read-only fallback.

- [ ] **Step 1: Write failing tests**

- `TestParseRangeCalendarWindows` — v1 cases, including `all`, empty, unknown, `7d`, custom one-sided/reversed/invalid/negative.
- `TestSnapshotFieldsAndWindow` — no range = all rows; today/week/custom filters apply to requests, tokens, by-model, by-realm, by-account; `started`, `since`, `log_file`, `realm`, `usd_cny`, `accounts_map`, `account` are present.
- `TestSnapshotCountsErrorsAndCredits` — failed rows count as errors and credit; client-aborted rows count errors but not credit/token totals; completed-only token totals.
- `TestAnalyticsPerKeyAndAccount` — `all_time` and window halves; per-key `realms`, `last_at`, top 5 models; anonymous/pre-key buckets `__no_key__`, `__before_keys__`, `__unknown_key__`.
- `TestPerfPercentiles` — only tail sample; p50/p90/p99 from raw samples; success rate excludes client aborts; `by_model`, `by_model_realm`, `by_model_acct`; `sample_capped`/`sample_from`.
- `TestTimeseriesBuckets` — minute/hour/day alignment to local grid; custom bucket anchored at window start; credits top 50; client aborts excluded from credit.
- `TestByAccountTotals` — skips error rows; models sorted top 5; total tokens sort.
- `TestRemainingUsage` — limit events + 429 rows merged/deduped; 3-day weighted estimate; capped/estimated segments; no budget for unknown; active/expired windows; `remaining` nil vs 0 distinction.
- `TestCacheCheckpointRoundTrip` — write/read each kind; append-only catch-up; truncated/replaced/same-size rewritten log invalidates; corrupt JSON/schema mismatch/kind mismatch falls back; pricing/realm fingerprint invalidation.
- `TestETagPairing` — stable within TTL, changes on rebuild; `If-None-Match` 304 has no body/Content-Length, keeps Cache-Control/ETag.
- `TestRecorderShape` — exactly one row per completed/failed/aborted request; missing usage writes `usage_missing=true` and still counts the request; `requested_model` only when different; `stream`, `kind`, timing, `key`, `account`, `realm`, `effort`, `cost_policy`; no prompt text; append failure logs but does not fail the client.
- `TestNoCrossRealmMerge` — `intl`/`cn` snapshots never include each other; `realm=all` is not a generic union.
- `TestUsageRoutes` — `/usage`, `/v1/usage`, `/usage/recent`, `/usage/analytics`, `/usage/perf`, `/usage/timeseries`, `/usage/by-account`, `/usage/remaining` preserve status, content type, query params (`realm`, `range`, `since`, `until`, `bucket`, `sample`, `limit`, `page`), and body shape.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/platform/usage ./internal/api -run 'TestParseRange|TestSnapshot|TestAnalytics|TestPerf|TestTimeseries|TestByAccount|TestRemaining|TestCache|TestETag|TestUsageRoutes'`
Expected: FAIL, missing service/routes.

- [ ] **Step 3: Implement**

- `snapshot.go`: fold rows into the v1 `_empty_stats` shape. Include `cost_missing` only for `Known=false && !Disabled`. Aggregate `by_model`, `by_model_realm`, `by_model_acct`; include account counts only when an account id exists. `credit` sums every non-client-aborted row; token fields sum completed rows only.
- `analytics.go`: produce v1 `compute_usage_analytics` shape (`summary`, `all_time`, `accounts`, `models`, `keys`, `window`, `key_model_top_n`, `usd_cny`). Do not merge `intl`/`cn` unless the request is `realm=all` and only WorkBuddy providers contributed; the generic API must pass the explicit realm.
- `perf.go`: port `_perf_block`, `_perf_stats_uncached`, incremental ring logic; sort raw samples for percentiles; keep `sample` clamp 10..20000; do not approximate from aggregates.
- `timeseries.go`: auto bucket by span (`<=6h` minute, `<=14d` hour, else day), explicit `bucket_seconds` minimum 60; align day/hour/minute to local wall clock; include `series`, `credits`, `since`, `until`, `bucket_seconds`, `ok`, `realm`.
- `byaccount.go`: whole-log fold, errors skipped, models top 5, sorted by total tokens; cache one entry.
- `remaining.go`: merge `limit-events.jsonl` with 429 usage rows; key by `(uid, model, reset)`; 24h window; 3/2/1 weighted budget; `estimate-daily.json` archive; cap/segmented accounting; `activity_floor`; `budgets`/`models`/`rows`; TTL 15s (`WB_REMAINING_TTL`).
- `cache.go`: one writer per kind; validate `(log path, device, inode, size, mtime, tail signature)`; atomic temp+rename; `WB_USAGE_CACHE=0` disables writes and reads; corrupted cache is deleted or ignored, never fatal.
- `recent.go` extension: add `key_name` (via keys store when wired) and cost fields `cost_cny`, `cost_band`, `cost_source`, `cost_source_at`, `cost_backfilled`, `cost_rates`, `cost_unit`, `cost_currency`, `cost_usd_cny`, `cost_or_id`, `cost_via`, `cost_inherited_from`, `cost_override_from`, `cost_band_note`, `cost_via_derived`. Unknown pricing leaves cost fields `nil`, never 0.
- `recorder.go`: build the v1 row shape once. `At`/`ISO`; `model`, `stream`, `outcome`, `kind` (default `chat`), timing; optional `requested_model`; usage fields from `Usage` (prompt/completion/reasoning/cached/total/credit); `account`/`realm`; `key` always present, empty when absent; `cost_policy` (`media`, `<realm>:unpriced`, or pricing current policy); `reasoning_effort`; media fields only when present; derived `tokens_per_sec`/`cache_hit_pct`. Missing usage writes `usage_missing=true`. Never store prompts, headers, tokens, or key secrets. A record failure is logged and never changes the client response.
- `api/usage.go`: use `writeJSONResponse` with `SetEscapeHTML(false)`, no pretty; return exact `text/html`/JSON content types only where v1 did. Apply panel gate from Task 7; until then, tests may mount handlers directly with a fake gate.
- No new `/usage/export` route.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/platform/usage ./internal/api`
Expected: PASS.
Run: `go test -race ./internal/platform/usage`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/usage internal/api/usage.go internal/api/usage_test.go
git commit -m "feat(platform): add usage aggregates and remaining estimate"
```

---

### Task 4: Runtime logs, SQLite compatibility, redaction, export

**Files:**
- Modify: `go.mod`, `go.sum` (add pure-Go SQLite driver; pin version)
- Create: `internal/platform/logs/entry.go`
- Create: `internal/platform/logs/ring.go`
- Create: `internal/platform/logs/sqlite.go`
- Create: `internal/platform/logs/store.go`
- Create: `internal/platform/logs/redact.go`
- Create: `internal/platform/logs/export.go`
- Create: `internal/api/logs.go`
- Test: `internal/platform/logs/ring_test.go`
- Test: `internal/platform/logs/sqlite_test.go`
- Test: `internal/platform/logs/redact_test.go`
- Test: `internal/platform/logs/export_test.go`
- Test: `internal/api/logs_test.go`

**Interfaces:**
- `Entry`, `Query`, `Result`, `Store` from the shared section.
- `NewRing(capacity int) *Ring` with default 2000; `NewSQLite(path string) (*SQLiteStore, error)`; `Open(path, enabled, caps, logger) *Store`.
- `RedactText(text string) string`, `RedactQuery(text string) string`, `RedactPanelJSON(value any) any` (copy-on-write).
- `WriteExport(w io.Writer, result Result) error` emits `[ts] [level] [tag] msg` lines and redacts legacy DB rows again.

- [ ] **Step 1: Write failing tests**

- `TestRingCapacityAndFilters` — newest 2000 retained; level case-insensitive; tag lowercased; search matches msg/tag; `since_id > 0` returns ascending IDs and ignores limit, like v1.
- `TestSQLiteSchemaMatchesV1` — `PRAGMA table_info(logs)` exactly `id, ts, time, level, tag, msg`; indexes `idx_logs_level`, `idx_logs_tag`; `PRAGMA journal_mode=DELETE`; `PRAGMA auto_vacuum=INCREMENTAL` where supported.
- `TestSQLiteDisabledCreatesNoFile` — default off; no `logs.sqlite3`; memory ring still works.
- `TestSQLiteRetentionRowsDaysBytes` — rows cap deletes oldest; days cap uses `ts` and default 7; bytes cap deletes oldest/VACUUMs; caps blank/null mean unlimited; `msg` capped at 64 KiB.
- `TestSQLiteReopenResumesIDs` — append, close, reopen; `MAX(id)` resumes; panel `since_id` keeps working.
- `TestConcurrentAppendAndQuery` — 4 writers + readers; no race; queries return completed batches; clear is serialized.
- `TestPhoneAndOAuthRedaction` — `13812345678` → `138****5678`; separators `-`, space, `.`, `·`; OAuth query `?code=...&state=...` → `<redacted>`; idempotent.
- `TestRedactPanelJSONCopyOnWrite` — `nickname` masked; identity keys (`uid`, `account`, `id`, `key`, `token`, `path`) unchanged; no mutation of shared cache objects; arrays nested.
- `TestLogExportRedactsLegacyRows` — a legacy row with plaintext phone is redacted at export time.
- `TestLogsRoutes` — `/logs` returns `mode` (`memory|db`), `stored`, `db_bytes` when DB active; `/logs/export` exact text headers/filename; `/logs/clear` clears both and is panel-only.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/platform/logs ./internal/api -run 'TestRing|TestSQLite|TestPhone|TestRedact|TestLogExport|TestLogsRoutes'`
Expected: FAIL, missing package/routes.

- [ ] **Step 3: Implement**

- `sqlite.go`: use `database/sql` with `_ "modernc.org/sqlite"`, driver name `sqlite`; `db.SetMaxOpenConns(1)`. Apply `PRAGMA auto_vacuum=INCREMENTAL`, `journal_mode=DELETE`, `synchronous=NORMAL`, then schema. Use `INSERT OR REPLACE INTO logs(id,ts,time,level,tag,msg)`. Batch up to 256 queued appends per transaction. Trim rows/days every 5s and bytes every 30s; a trim error logs but never stops appends.
- `store.go`: `Append` never blocks the request path; enqueue. `Query` serializes through the same worker and returns `Result` or falls back to the ring on DB errors. `Close` flushes and stops. `Configure(enabled, maxRows, maxDays, maxBytes)` is idempotent and leaves the file/rows when disabled.
- `redact.go`: port v1 `_mask_text`, `_CALLBACK_LOG_SECRET_RE`, `_redact_account_names`; copy-on-write, identity-key allowlist. The phone mask must run after level/tag inference in the caller, as in v1.
- `api/logs.go`: query params `limit`, `level`, `tag`, `search`, `since_id`; defaults 200; `mode`/`stored`/`db_bytes`; export filename `wb-proxy-<stamp>.log`; content type `text/plain; charset=utf-8`.
- `logs.sqlite3` remains opt-in through `log_db_enabled`; default days cap 7; max rows 100,000,000; max days 36,500; max bytes 100 GiB; blank clears a cap.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/platform/logs ./internal/api`
Expected: PASS.
Run: `CGO_ENABLED=0 go test ./internal/platform/logs`
Expected: PASS, proving the SQLite driver is pure Go.
Run: `go test -race ./internal/platform/logs`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/platform/logs internal/api/logs.go internal/api/logs_test.go
git commit -m "feat(platform): add log store and sqlite compatibility"
```

---

### Task 5: OpenAI Responses API and compaction

**Files:**
- Create: `internal/api/protocol/chat.go`
- Create: `internal/api/responses.go`
- Create: `internal/api/responses_stream.go`
- Create: `internal/api/responses_compact.go`
- Modify: `internal/api/server.go`
- Modify: `internal/api/chat.go` only to reuse the extracted chat-event types; no Chat wire change
- Test: `internal/api/protocol/chat_test.go`
- Test: `internal/api/responses_test.go`
- Test: `internal/api/responses_stream_test.go`
- Test: `internal/api/responses_compact_test.go`

**Interfaces:**
```go
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request)
func responsesToChat(payload map[string]json.RawMessage) (map[string]any, error)
func chatToResponse(chat map[string]any, model string, meta responseMeta) map[string]any
func writeResponsesStream(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, model string, meta responseMeta)
func responsesCompactionResponse(chat map[string]any, model string) map[string]any
```
- `responseMeta` carries `tools`, `tool_choice`, `parallel_tool_calls`, namespace map, custom tool names, `input` text for usage estimation, and whether local web tools are active.
- `protocol/chat.go` exposes a `Chunk` parser and a `Reader` over `providers.ChatStream`; both Chat and Responses/Messages use it. Chat behavior remains pinned by existing tests.

- [ ] **Step 1: Write failing tests**

- `TestResponsesRequestMapsToChat` — `input` string/array, `instructions`, message roles, `input_text`/`output_text`, `input_image`/data URL, `function_call_output`, `previous_response_id` ignored like v1, `text.format` mapping only when preserve-contract is on.
- `TestResponsesToolsCustomAndNamespace` — function tools, custom tools (`type: custom`), namespace tools, `additional_tools` inside input, `tool_choice` name resolution, `parallel_tool_calls`; unknown server tools dropped without breaking.
- `TestResponsesReasoningAndMaxTokens` — `reasoning.effort`, `reasoning_effort`, explicit/default `max_output_tokens`.
- `TestResponsesNonStreamShape` — `object=response`, `output` items (reasoning/message/function_call/custom_tool_call), `output_text`, `status`, `model`, `parallel_tool_calls`, `tool_choice`, `tools`, `usage` mapping; `incomplete_details` on length; reasoning-only returns 502.
- `TestResponsesStreamEventSequence` — `response.created`, `response.in_progress`, reasoning summary events, content part/text deltas, function/custom call deltas, `response.output_item.done`, `response.completed`; one lifecycle; no `[DONE]`; usage in final response.
- `TestResponsesCompactLegacyAndTrigger` — `/responses/compact` and `compaction_trigger` produce exactly one `compaction` output item; ordinary turns are not compaction; summary encoding round-trips.
- `TestResponsesClientAbort` — client context cancellation stops reads, closes the stream once, records `client_aborted`, and does not emit a second terminal event.
- `TestResponsesSSEError` — upstream error becomes `response.failed` if headers are sent, error envelope before headers otherwise; no panic.
- `TestResponsesAliases` — `/responses` and `/v1/responses`, `/responses/compact` and `/v1/responses/compact` are identical; method other than POST is 405 with v1 body.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api -run 'TestResponses'`
Expected: FAIL, missing handlers/types.

- [ ] **Step 3: Implement**

- Port `_responses_input_to_messages` semantics: content arrays, tool call history, image inputs, `additional_tools`, compaction trigger. Do not forward gateway-only fields (`realm`, `_namespace_map`, `_web_tools`) upstream.
- Port `responses_to_chat`: temperature/top_p/seed, max tokens/default, reasoning effort, tools/tool_choice/parallel, text format; preserve request meta for response echo.
- Port `chat_to_response` and `stream_responses_events`: stable per-response IDs, output indices, reasoning summary, text/message item, structured and custom tool calls, namespace stamping, DSML fallback only where v1 had it, citation annotations from local web sources.
- Streaming: write only after the first event? Keep v1's early 200 SSE headers for Responses; use `sseWriter` for keepalive and flush. Ensure `response.completed`/`response.failed` is emitted once and the upstream is closed exactly once. Compact requests do not emit ordinary text.
- `responsesCompactionResponse`: call the same chat pipeline with the compaction prompt and return one `compaction` item; legacy `/responses/compact` uses the legacy encoding, trigger uses the v2 item shape.
- Register aliases in `server.mount`; `parseChatBody` may be shared but must not reject Responses `input` for missing `messages`.
- Error bodies: OpenAI envelope for Responses, Anthropic envelope only for `/messages*`. Preserve 400/401/403/404/429/502/503 semantics and 429 reset metadata.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api -run 'TestResponses|TestChat'`
Expected: PASS.
Run: `go test ./internal/api`
Expected: PASS, proving no Chat regression.

- [ ] **Step 5: Commit**

```bash
git add internal/api/protocol internal/api/responses*.go internal/api/server.go internal/api/chat.go
git commit -m "feat(api): add openai responses protocol"
```

---

### Task 6: Anthropic Messages and count_tokens

**Files:**
- Create: `internal/api/protocol/anthropic.go`
- Create: `internal/api/messages.go`
- Create: `internal/api/messages_stream.go`
- Create: `internal/api/messages_count.go`
- Modify: `internal/api/server.go`
- Test: `internal/api/messages_test.go`
- Test: `internal/api/messages_stream_test.go`
- Test: `internal/api/messages_count_test.go`

**Interfaces:**
```go
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request)
func messagesToChat(payload map[string]any) (map[string]any, error)
func chatToMessages(chat map[string]any) map[string]any
func writeMessagesStream(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, model string)
func estimateMessagesInputTokens(chat map[string]any) int
func writeAnthropicError(w http.ResponseWriter, status int, message, errType string, reset time.Time)
```
- `messagesToChat` mirrors v1: top-level `system`, user/assistant content blocks, text/image/tool_use/tool_result/thinking/document; server tools skipped with a system note; client tools only; `tool_choice`; `stop_sequences`; `metadata.user_id`; reasoning effort from `output_config.effort`/`thinking`; `top_k` dropped.
- `chatToMessages` mirrors v1: thinking block from `reasoning_content`, text block, `tool_use` blocks, stop reason mapping, usage mapping.

- [ ] **Step 1: Write failing tests**

- `TestMessagesRequestMapsToChat` — v1 `_test_anthropic_messages` cases: system blocks joined; image base64 → data URL; thinking not forwarded; tool_use → OpenAI tool call; tool_result → tool message; mixed tool_result image preserved; server tool skipped with note; `tool_choice`; `max_tokens` default.
- `TestMessagesNonStreamShape` — `type=message`, `role=assistant`, content blocks, `stop_reason`, `stop_sequence`, usage (`input_tokens` excludes cache read/creation; output; cache fields).
- `TestMessagesStreamEvents` — `message_start`, `content_block_start`, `content_block_delta` (`text_delta`, `thinking_delta`, `input_json_delta`), `content_block_stop`, `message_delta`, `message_stop`; no `[DONE]`; event `data` has `type` equal to event name.
- `TestMessagesAliases` — `/messages` and `/v1/messages`, `/messages/count_tokens` and `/v1/messages/count_tokens`; other methods 405.
- `TestMessagesCountTokens` — local estimator shape `{"input_tokens": N}`; CJK-aware; includes tools/messages; never calls upstream.
- `TestMessagesDisabledByDefault` — missing `enable_anthropic_messages` returns 400 with the v1 opt-in message (issue #367 wording); env `WB_ENABLE_ANTHROPIC_MESSAGES=1` and settings true both enable.
- `TestAnthropicErrors` — 400/401/403/404/429/503 map to `invalid_request_error`, `authentication_error`, `permission_error`, `not_found_error`, `rate_limit_error`, `overloaded_error`; 429 carries reset metadata and message suffix.
- `TestMessagesStreamAbort` — client cancellation closes upstream once and records `client_aborted`; upstream error before headers uses Anthropic envelope; after headers emits `error` event and stops.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api -run 'TestMessages|TestAnthropic'`
Expected: FAIL, missing handlers/types.

- [ ] **Step 3: Implement**

- Port v1 translation helpers exactly, including `_anthropic_text`, `_anthropic_json_text`, `_anthropic_blocks_to_messages`, `_anthropic_tool_result_content`, `_anthropic_usage`, `_anthropic_stop_reason`.
- `messagesToChat`: reject non-object body, missing/blank model, missing/empty messages, invalid roles; keep v1 error strings where clients/tests depend on them.
- `chatToMessages`: `thinking` block first, text second, tool calls in order; `max_tokens` stop → `max_tokens`; tool calls → `tool_use`; content filter/refusal → `refusal`; else `end_turn`.
- Streaming: do not emit `ping` unless v1 did; emit start before first delta; ensure thinking/text blocks close before tool blocks; final usage from upstream; holder records usage for the usage store.
- Count tokens: use the same estimator as v1 `estimate_tokens` over serialized messages/tools; response content type JSON; no usage row.
- Default disabled: check settings `enable_anthropic_messages` or env; reject before forwarding or recording paid usage.
- Register aliases in `server.mount`; keep panel/API-key gate same as Chat (v1 uses `_key_ok`, so a panel session also passes).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api -run 'TestMessages|TestAnthropic'`
Expected: PASS.
Run: `go test ./internal/api`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/protocol/anthropic.go internal/api/messages*.go internal/api/server.go
git commit -m "feat(api): add anthropic messages protocol"
```

---

### Task 7: API keys, panel auth/settings/realm, proxy slots

**Files:**
- Modify: `go.mod`, `go.sum` only if `golang.org/x/crypto/pbkdf2` is chosen
- Create: `internal/platform/keys/store.go`
- Create: `internal/platform/keys/binding.go`
- Create: `internal/platform/keys/limiter.go`
- Create: `internal/platform/panel/password.go`
- Create: `internal/platform/panel/session.go`
- Create: `internal/platform/panel/status.go`
- Create: `internal/platform/settings/service.go`
- Create: `internal/platform/settings/realm.go`
- Create: `internal/platform/proxy/slots.go`
- Create: `internal/api/panel.go`
- Create: `internal/api/settings.go`
- Create: `internal/api/proxy.go`
- Modify: `internal/api/auth.go`
- Modify: `internal/api/server.go`
- Modify: `internal/providers/workbuddy/routes.go` (route ownership migration only)
- Create: `internal/providers/workbuddy/slots.go`
- Create: `internal/providers/codearts/settings.go`
- Create: `internal/providers/codearts/slots.go`
- Create: `internal/providers/raccoon/settings.go`
- Create: `internal/providers/raccoon/slots.go`
- Modify: `internal/providers/workbuddy/provider.go`, `internal/providers/codearts/provider.go`, `internal/providers/raccoon/provider.go` (add contributor/binder methods)
- Test: `internal/providers/workbuddy/slots_test.go`
- Test: `internal/providers/codearts/settings_test.go`
- Test: `internal/providers/codearts/slots_test.go`
- Test: `internal/providers/raccoon/settings_test.go`
- Test: `internal/providers/raccoon/slots_test.go`
- Test: `internal/platform/keys/store_test.go`
- Test: `internal/platform/keys/binding_test.go`
- Test: `internal/platform/panel/panel_test.go`
- Test: `internal/platform/settings/settings_test.go`
- Test: `internal/platform/proxy/slots_test.go`
- Test: `internal/api/panel_test.go`
- Test: `internal/api/settings_test.go`
- Test: `internal/api/proxy_test.go`

**Interfaces:**
- `keys.Entry`, `keys.Store`, `Match`, `AllowsModel`, `AllowsAccount` from the shared section.
- Key store reads `accounts/settings.json`:
  - `api_keys` list is authoritative; each entry normalized exactly like v1 `_clean_key_entry` (id, name, key, realm, models, accounts, concurrency_limit, enabled, created_at, deleted_at).
  - Legacy `api_key`/`api_key_set` becomes one synthetic entry with id `legacy`, empty realm, no restrictions.
  - Soft delete: clearing `key` and setting `deleted_at`; usage history keeps the id. `Save(entries, nil)` uses replace-by-omission; `Save(entries, deleteIDs)` uses upsert semantics. Blank submitted key keeps the stored value; new keys require at least 4 chars; duplicate secrets collapse; duplicate ids get deterministic `-2`, `-3` suffixes.
- `keys.Limiter`: `Acquire(entryID string) (release func(), ok bool)`; lease released once, including on client abort/SSE end. 429 body: `Rate limit reached: API key '<name>' reached its concurrency limit (<n> in flight); retry shortly`, `type=rate_limit_error`, `reset_at=now+2`, `Retry-After`.
- `panel.Sessions`: in-memory only, TTL 7 days, 24-byte URL-safe tokens, token from `X-Panel-Token` only (never query/cookie).
- `panel.Password`: default `admin`; PBKDF2-HMAC-SHA256 120,000 rounds; salt 16 random bytes hex; `panel_password_hash`, `panel_password_salt`, `panel_password_rounds`, `panel_password_default`; constant-time compare; crypto-unavailable returns 500 with v1 reason; login rate limit 5 attempts/60s per IP + 0.5s failure delay; successful login clears attempts.
- `settings.Service`: `View(ctx) (map[string]any, error)`, `Save(ctx, patch) (map[string]any, error)`, `RevealKey(id) (string, bool)`. Merge base platform settings with provider `SettingsContributor` views. All unknown keys preserved. The base view must reproduce the platform-owned part of v1 `runtime_settings_view()`: `api_keys`, `deleted_api_keys`, `api_key_masked`, `api_key_set`, `api_key_set_by_panel`, `auth_required`, `panel_password_is_default`, `limits`, `reserve_credits`, `daily_token_limit`, `daily_credit_limit`, `model_daily_token_limit`, `pricing_refresh_minutes`, `pricing_variant_inherit`, `pricing_enabled`, `media*`, `log_db_*`, `update_check_*`, `keep_backups*`, `usage`, `prompt`, `local_web_tools`, `window_probe`, `quota_scan_*`, `remaining_priority_enabled`, `egress_backoff_enabled`, `account_max_concurrent*`, `section_max_height`, `ui_language`, `hidden_pages`, `accounts_*`, `settings_file`, `accounts_dir`, `usage_dir`, `project`, `version`. Provider contributors supply `workbuddy`/`schedule`/`daily_activity`/`daily_chat_web`/`identity`/`upstream` and the `codearts`/`raccoon` masked sections.
- `proxy.Slots`: `List()`, `Replace([]Slot)`, `Update(id, fields, defaults)`, `Bindings()`; stable `slot-N` ids from `proxy_slot_seq`; normalize URL; duplicate URL first-wins; empty name stays empty (auto-label by exit); preserve `ip`, `country`, `country_code`, `ip_type`, `isp`, `asn`, `probed_at`. Replace calls each provider's optional `ProxySlotBinder.ApplyProxySlots(transport.Slots)` and sums `ProxySlotBindings()` for the `bound` column.

- [ ] **Step 1: Write failing tests**

- `TestKeyStoreLegacyAndList` — legacy single key; `api_keys` newest shape; duplicate ids/secrets; deleted hidden by default; include-deleted preserves rows.
- `TestKeyStoreSaveSemantics` — replace-by-omission vs upsert `delete_ids`; blank key keeps stored secret; <4 chars rejected; soft-deleted secret wiped; stale submission cannot retire unmentioned key.
- `TestKeyModelAllowlist` — empty = unrestricted; exact and `fnmatch` wildcard case-insensitive; restricted + empty model denied.
- `TestKeyAccountAllowlist` — exact uid only; empty = all; max 200/128-byte cap.
- `TestKeyRealmBinding` — bound realm wins over `X-Realm`, `?realm`, body `realm`, and `realm=all`; unknown/`all` binding rejected 400; binding suppresses WorkBuddy fallback.
- `TestKeyConcurrency` — acquire/release; 429 at limit; independent keys; release on early return and client abort; no leak after 200 requests.
- `TestAuthDisabled` — `auth_disabled=true` lets API routes work without a key but panel routes still require a session; `auth_required` reports false; re-enabling restores 401; no key is synthesized into usage rows.
- `TestPanelPassword` — default login; set/verify; default sentinel; wrong salt/rounds; crypto-unavailable; password change revokes all sessions and returns a new token.
- `TestPanelSessions` — TTL, revoke, revoke-all, token header only, no cookie/query.
- `TestPanelLoginRateLimit` — 5 failures/60s; 429 wait; success clears; v1 proxy-aware client key (forwarded header only from loopback peer) so a reverse proxy does not collapse all clients into one bucket.
- `TestPanelStatus` — public; `panel_password_required`, `panel_password_is_default`, `authenticated`, `crypto_ok`, `api_key_set`, optional `crypto_error`, `raccoon` masked config.
- `TestSettingsMerge` — unknown top-level/nested keys survive; provider contributor sections merge (`workbuddy`, `codearts`, `raccoon`); `api_keys` handled by key store; media/pricing/log/update/panel groups round-trip.
- `TestProviderSettingsContributors` — CodeArts/Raccoon masked config has no secrets; SaveSettings only touches their settings key and preserves unknown nested keys; disabled provider still returns a safe masked view.
- `TestRealmEndpoint` — GET returns current/options; POST validates intl/cn, writes `active_realm.json`, refreshes provider health; invalid 400.
- `TestProxySlots` — stable ids never recycled; duplicate URL dropped; URL normalization; `bound` counts across WorkBuddy/CodeArts/Raccoon via `ProxySlotBinder`; removed slot unbinds accounts and reports the count; `proxy_slot_seq` preserved; providers without the hook are simply not counted.
- `TestRouteOwnershipMigration` — WorkBuddy `Routes()` no longer declares `/settings`, `/settings/save`, `/realm`; platform registers them once; all WorkBuddy admin tests still pass.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/platform/keys ./internal/platform/panel ./internal/platform/settings ./internal/platform/proxy ./internal/api -run 'TestKey|TestAuthDisabled|TestPanel|TestSettings|TestRealm|TestProxy|TestRouteOwnership'`
Expected: FAIL, missing packages/routes.

- [ ] **Step 3: Implement**

- `keys/store.go`: read through `core/config.Store`; atomic writes `0600`; no secrets in errors/logs. Legacy migration is read-only until a save. `Match` constant-time compares supplied secret across enabled entries; panel session independently unlocks management routes.
- `keys/binding.go`: model pattern normalization exactly v1 (`lower`, comma/semicolon/newline split, de-dupe, fnmatch); account refs exact; realm validation against registered providers happens in API, not store. `AuthRequired` reads `auth_disabled`; disabling auth never disables panel password or management gates.
- `keys/limiter.go`: mutex map id→count; `Acquire` returns a once-release closure; no global limit when `concurrency_limit<=0`.
- `panel/password.go`: implement PBKDF2 with `golang.org/x/crypto/pbkdf2`; if dependency review rejects it, use a small in-repo PBKDF2-HMAC-SHA256 loop and pin with RFC 6070 vectors. Compare with `hmac.Equal`. Preserve v1 `login_rate_limit_key` semantics: peer IP, with `X-Forwarded-For` considered only when the peer is loopback.
- `panel/session.go`: `sync.Mutex`, expiry map, lazy prune; random tokens from `crypto/rand`.
- `settings/service.go`: base view must include everything v1 `runtime_settings_view()` returned that is still owned by platform: `api_keys` masked summaries, `proxySlots`, media, pricing, log DB, usage keep, panel language, update settings. Provider contributors return their own keys (`workbuddy`, `schedule`, `daily_activity`, etc.). Save dispatches `api_keys`, `deleted_api_key_ids`, `panel_password` handling in `/panel/password`, and provider patches. Preserve v1 400 messages for invalid groups.
- `settings/realm.go`: active realm from `active_realm.json`, fallback settings `active_realm`, default `intl`; only WorkBuddy implements `RealmSelector`.
- `proxy/slots.go`: store in `accounts/settings.json` under `proxy_slots` and `proxy_slot_seq`; do not delete account bindings silently; call `ProxySlotBinder` on every provider after a replace. WorkBuddy/CodeArts/Raccoon wrap their existing pool `ApplyProxySlots` and enabled-account counts; a provider without the hook is untouched.
- `api/panel.go`, `api/settings.go`, `api/proxy.go`: exact v1 status/body shapes, panel gate, CORS for panel? v1 does not add CORS to panel JSON except shared helper; keep v1.
- Route migration: remove `/settings`, `/settings/save`, `/realm` from WorkBuddy `Routes()`; add exported `SettingsView`, `SaveSettings`, `ActiveRealm`, `SetActiveRealm`, `RealmOptions` methods. This is a route-ownership change only; do not change WorkBuddy config behavior.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/platform/keys ./internal/platform/panel ./internal/platform/settings ./internal/platform/proxy ./internal/api ./internal/providers/workbuddy`
Expected: PASS.
Run: `go test -race ./internal/platform/keys ./internal/platform/panel ./internal/platform/settings`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/platform/keys internal/platform/panel internal/platform/settings internal/platform/proxy internal/api internal/providers
git commit -m "feat(platform): add api keys panel auth and proxy slots"
```

---

### Task 8: Media images, videos, task store, provider hooks

**Files:**
- Create: `internal/platform/media/config.go`
- Create: `internal/platform/media/service.go`
- Create: `internal/platform/media/tasks.go`
- Create: `internal/platform/media/store.go`
- Create: `internal/platform/media/sign.go`
- Create: `internal/platform/media/pricing.go`
- Create: `internal/api/media.go`
- Create: `internal/providers/workbuddy/media.go`
- Create: `internal/providers/workbuddy/media_client.go`
- Create: `internal/providers/codearts/capabilities.go`
- Create: `internal/providers/raccoon/capabilities.go`
- Modify: `internal/providers/workbuddy/provider.go` (expose capabilities/hooks)
- Test: `internal/platform/media/config_test.go`
- Test: `internal/platform/media/tasks_test.go`
- Test: `internal/platform/media/pricing_test.go`
- Test: `internal/api/media_test.go`
- Test: `internal/providers/workbuddy/media_test.go`
- Test: `internal/providers/codearts/capabilities_test.go`
- Test: `internal/providers/raccoon/capabilities_test.go`

**Interfaces:**
- `media.Config` fields: `Enabled`, `Realm` (`cn|intl`), `Identity` (`desktop|cli`), `ImageMaxPerMinute` (1..60, default 6), `VideoMaxSeconds` (1..60, default 15), `DailyCreditsPerKey` (0..100000, default 0), `VideoConcurrency` (1..8, default 3). `WB_MEDIA_ENABLED=1` may force only `enabled`.
- `media.Service`: `Models(ctx, realm, tag)`, `GenerateImage`, `EditImage`, `SubmitVideo`, `Task`, `Cancel`, `Tasks`, `Content`.
- `media.TaskStore` from shared section; state file `media/tasks.json`; artifacts in `media/`; default caps workers/concurrency 3, max pending 20, max tasks 200, max disk 2048 MB, TTL 7 days, signed URL TTL 1 hour.
- `media.Pricing`: `video-prices.json`, `image-prices.json`; `LearnImage(model, credit)`, `ImagePrice(model)`, `LearnVideo(model, resolution, credit, seconds, withReference)`, `PricePerSecond(...)`, `CostHint(...)`; unknown returns nil, never a guess.
- Provider hook signatures from `providers.MediaProvider`. WorkBuddy implements all methods; CodeArts/Raccoon return capabilities false and no media methods.

- [ ] **Step 1: Write failing tests**

- `TestMediaConfigDefaultsAndValidation` — defaults exactly v1; unknown keys/type/range errors; malformed stored values fall back; env only forces enabled; key-bound realm mismatch returns v1 message.
- `TestMediaDisabled404` — disabled routes return 404 `not found`, not 403; `/v1/media/models` still works when disabled.
- `TestImageGeneration` — fake provider returns URL; response `{created,model,mode,data:[{url}],credit?,hint?}`; `response_format=b64_json` downloads and converts, falls back to URL with hint on failure; `n` validation; size parsing.
- `TestImageEdit` — data URL input, size/type checks, 8 MB decoded cap, max pixels; no local paths; 24 MB media body cap.
- `TestVideoTaskLifecycle` — 202 submit; waiting→submitting→queued→running→completed; status mapping `queued|in_progress|completed|failed`; `seconds` string; `error.code` `invalid_request|upstream_error|task_lost|cancelled`; `?wait=1`; cancel waiting/queued only, 409 otherwise.
- `TestVideoOwnershipAndSignedURL` — task visible only to owning key or panel; content route signed token works without Authorization; expired/wrong token 401; wrong owner 404; filename/content type.
- `TestMediaQueueAndConcurrency` — pending cap 20 → 429 queue full before spending; active concurrency queues rather than submits; busy accounts spread; queue position.
- `TestMediaTaskRetention` — max tasks/disk/TTL pruning; `tasks.json` round-trip; prompt head/len only.
- `TestMediaPricingLedger` — measured image/video prices; resolution/ref keys; unknown nil; corrupt file ignored.
- `TestMediaCapabilities` — WorkBuddy true; CodeArts false for media generation, image input true; Raccoon all false; platform never calls a provider without the hook.
- `TestMediaUsageRows` — one usage row per image/video with `kind=image|video`, `credit`, `cost_policy=media`, media params, no prompt.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/platform/media ./internal/api ./internal/providers/workbuddy ./internal/providers/codearts ./internal/providers/raccoon -run 'TestMedia|TestImage|TestVideo|TestCapabilities'`
Expected: FAIL, missing services/hooks.

- [ ] **Step 3: Implement**

- `platform/media/config.go`: exact v1 defaults/ranges; read `accounts/settings.json` `media`; strict patch validation for `/settings/save`; unknown key error.
- `platform/media/tasks.go`: one mutex, worker pool, per-task goroutine polling with 5s interval/600s timeout; `tasks.json` atomic write; state transitions and `status_out`/`error_code_for`; `waiting` bodies only in memory (restart marks failed/rejected with v1 message); cancel semantics; signed HMAC token `exp.mac` over `safeTaskID|index|exp`, 1h TTL, process secret.
- `platform/media/service.go`: provider selection by `providers.MediaProvider`; realm from key binding first, then config; image sync; video submit/poll/download; media slots default 4; per-key image rate 6/min in memory; per-key daily spend from usage rows kind image/video (0 = unlimited); provider handles account rotation/budget and returns account/credit.
- `platform/media/pricing.go`: copy v1 ledger behavior; only learn positive credits; `image-prices.json` map; video key `model|resolution|ref`; atomic writes.
- WorkBuddy `media.go`/`media_client.go`: implement provider hook using existing account pool, HTTP transport, identity headers, upstream paths `/v3/config`, `/v2/images/generations`, `/v2/images/edits`, `/v2/videos/generations`, `/v2/videos/tasks`; build bodies per v1 `wb_media.py`; no local path reads; SSRF-safe public URL/data URL handling; no secrets in errors. This is provider behavior, not platform logic.
- CodeArts/Raccoon capabilities only; do not add media routes to their providers.
- `api/media.go`: exact route aliases, auth, JSON shapes, status codes, `202`, `409`, `404`, `429`, `503`; media routes do not take chat slots and do not apply chat model aliases; media body limit 24 MB only raises the generic cap.
- Record usage through the platform usage service: `kind`, `credit`, `seconds`, `resolution`, `aspect_ratio`, `image_to_video`, `cost_policy="media"`. Do not store prompt text.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/platform/media ./internal/api ./internal/providers/workbuddy ./internal/providers/codearts ./internal/providers/raccoon`
Expected: PASS.
Run: `go test -race ./internal/platform/media`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/media internal/api/media.go internal/api/media_test.go internal/providers/workbuddy/media*.go internal/providers/codearts/capabilities.go internal/providers/raccoon/capabilities.go
git commit -m "feat(platform): add media tasks and provider hooks"
```

---

### Task 9: Agent configuration apply/restore/selection/CLI

**Files:**
- Create: `internal/platform/agents/clients.go`
- Create: `internal/platform/agents/apply.go`
- Create: `internal/platform/agents/state.go`
- Create: `internal/platform/agents/yaml.go`
- Create: `internal/platform/agents/toml.go`
- Create: `internal/platform/agents/models.go`
- Create: `internal/platform/agents/service.go`
- Create: `internal/api/agents.go`
- Test: `internal/platform/agents/clients_test.go`
- Test: `internal/platform/agents/apply_test.go`
- Test: `internal/platform/agents/state_test.go`
- Test: `internal/platform/agents/models_test.go`
- Test: `internal/api/agents_test.go`

**Interfaces:**
```go
type Client struct {
    ID, Label, Desc, Protocol string
    Files func(home string) map[string]string
    Probe func(home string) bool
    Apply func(paths map[string]string, ctx Context) (map[string]string, error)
}
type Context struct { BaseURL, BaseURLV1, APIKey, Model string; Models []ModelSpec; Home string }
type Service struct { AccountsDir, Home string; Providers []providers.Provider }
func (s *Service) Overview(ctx) map[string]any
func (s *Service) Apply(ctx, clientID, baseURL, apiKey, model string, models []ModelSpec) (map[string]any, error)
func (s *Service) Restore(ctx, clientID) (map[string]any, error)
func (s *Service) Selection() ([]string, bool)
func (s *Service) SetSelection([]string) error
```
- Clients: `claude-code` (`~/.claude/settings.json`, `CLAUDE_CONFIG_DIR`), `codex` (`~/.codex/config.toml` + `auth.json`, `CODEX_HOME`), `opencode` (`~/.config/opencode/opencode.json`), `dsh` (`~/.dsh/settings.yaml` + profile patch), `crush` (`~/.config/crush/crush.json`), `zcode` (`~/.zcode/v2/provider_config.json`).
- State: `accounts/integration-state.json`; backups `accounts/agent-backups/<client>/<stamp>-<basename>`, newest 10 per basename; atomic writes; mode `0600` for secrets-bearing files; restore puts original bytes back and deletes files created by apply; prune empty parent dirs.
- Model selection: settings key `agent_models`; `nil` = never configured, empty list = explicitly zero; apply with no models uses selection when configured, otherwise full catalog; cap 80; model-embedding clients reject empty with v1 message.

- [ ] **Step 1: Write failing tests**

- `TestAgentsOverview` — all six clients; installed/configured/applied/external change; config paths; local-only gate returns `enabled:false` for non-loopback.
- `TestApplyClaudeCode` — `ANTHROPIC_BASE_URL` strips trailing `/v1`; auth token; model env for all four vars only when model supplied; existing keys preserved.
- `TestApplyCodexTOML` — top-level `model_provider`, `model`, `[model_providers.wb-proxy]` with `base_url` `/v1`, `wire_api=responses`, `experimental_bearer_token`; drops `env_key`; `auth.json` merge.
- `TestApplyOpenCode` — `$schema`, provider npm/name/options, model limits from `context_window`/`max_output`, selection snapshot replaces old models, user keys preserved.
- `TestApplyDSH` — profile patch layer, reasoning levels/efforts, model default, no clobber of unrelated YAML; `settings.yaml` remains.
- `TestApplyCrushAndZCode` — provider entry shapes and file paths.
- `TestRestore` — byte-for-byte restore; created files deleted; empty dirs pruned; missing backup 400; no state record 400.
- `TestExternalChange` — hash differs after apply → `external_change:true`; hand edit detected.
- `TestModelSelection` — nil vs empty; apply fallback semantics; 400 for empty on model-embedding clients; max 80; specs enriched from provider `AgentModelSpecProvider`/Models.
- `TestAgentsLocalOnly` — `/agents` read may be remote but reports disabled; `/agents/apply`/`restore` 403 from non-loopback/private address; `/agents/cli` public.
- `TestAgentsCLI` — standalone configurator served without panel token; fetch `/v1/models` with key; model cap 80.
- `TestAgentsRoutes` — `/agents`, `/agents/available`, `/agents/model-selection`, `/agents/apply`, `/agents/restore` shapes/status; no duplicate with provider routes.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/platform/agents ./internal/api -run 'TestAgent|TestApply|TestRestore|TestModelSelection'`
Expected: FAIL, missing package/routes.

- [ ] **Step 3: Implement**

- Port v1 `wb_agents.py` behavior, not Python code shape. Use small line-based TOML/YAML upsert helpers; preserve unrelated keys/comments where v1 did.
- `state.go`: atomic JSON, hashes, backups, prune, copy-on-write state; never write outside `AccountsDir` except client config paths under `Home`.
- `models.go`: merge gateway `Model` entries and provider `AgentModelSpec`s; fields `id`, `name`, `context_window`, `max_output`, `reasoning_efforts`, `supports_vision`; explicit payload fields win.
- `api/agents.go`: panel auth, local-only check for apply/restore, exact v1 messages; `/agents/available` must be cheap (no provider calls, no agent package import in the hot path); `/agents/cli` public.
- Do not make provider-specific agent behavior; use generic model specs and the provider's `Models`.
- Keep `/agents/cli` body as the standalone configurator source, not a server-side config write.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/platform/agents ./internal/api`
Expected: PASS.
Run: `go test -race ./internal/platform/agents`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/agents internal/api/agents.go internal/api/agents_test.go
git commit -m "feat(platform): add agent config services"
```

---

### Task 10: Web tools, probes, window/quota scan, task/activity ownership

**Files:**
- Create: `internal/platform/webtools/tooldefs.go`
- Create: `internal/platform/webtools/guard.go`
- Create: `internal/platform/webtools/search.go`
- Create: `internal/platform/webtools/fetch.go`
- Create: `internal/platform/webtools/cache.go`
- Create: `internal/platform/probes/state.go`
- Create: `internal/platform/probes/window.go`
- Create: `internal/platform/probes/quota.go`
- Create: `internal/platform/probes/service.go`
- Create: `internal/providers/workbuddy/probes.go` (adapt existing probe/account state to `ProbeProvider`)
- Create: `internal/api/quota.go`
- Modify: `internal/api/responses.go` to wire local web-tool rounds (Task 5 leaves a hook)
- Test: `internal/providers/workbuddy/probes_test.go`
- Test: `internal/platform/webtools/webtools_test.go`
- Test: `internal/platform/probes/window_test.go`
- Test: `internal/platform/probes/quota_test.go`
- Test: `internal/platform/probes/service_test.go`
- Test: `internal/api/quota_test.go`
- Test: `internal/api/taskactivity_ownership_test.go`

**Interfaces:**
- Web tools: `ToolDefs(wants Wants) []map[string]any`, `WantsFromTools([]map[string]any) Wants`, `Execute(ctx, name, args) string`, `Search(ctx, query, n) string`, `Fetch(ctx, url, startIndex) string`, `Sources(result) []Source`. Names exactly `web_search`, `web_fetch`; `MAX_WEB_ROUNDS` default 3 (cap 8, env `WB_MAX_WEB_ROUNDS`).
- Guard: absolute http/https only; resolve DNS and reject loopback/private/link-local/multicast; no redirects (or per-hop revalidation); max body bytes; textual content only; strip tags; keep query URL.
- Probes: `State` for `accounts/model_windows.json` and `accounts/quota_scan.json`; `WindowScheduler`, `QuotaScheduler`; optional `providers.ProbeProvider`; due budgets (window batch 8, quota batch 6), gaps 0.4s, retries, max age, fresh-window threshold 30s.
- `quota-scan/run` forces due quota lines, refuses lines with a registered 429 window, returns `{ok, sent, ...}`.
- Task/activity ownership: `/tasks`, `/tasks/*`, `/activity/history` remain WorkBuddy provider routes. Platform must not register them. The ownership test asserts exactly one handler per path and provider precedence.

- [ ] **Step 1: Write failing tests**

- `TestWebToolDefsAndDeclarations` — server tool `{"type":"web_search"}` and function-shaped declarations both recognized; install replaces same-name duplicates; only declared tools added.
- `TestWebSearchFallbackAndCache` — fake HTTP for DuckDuckGo/Bing/StackOverflow/Wikipedia/News; first engine blocked → next engine; cache hit; cooling; failure returns v1 "temporarily unavailable" text; no real network.
- `TestWebFetchGuard` — rejects `file://`, localhost, 127.0.0.1, private/link-local, redirect to private; accepts public fake server; non-text content type rejected; `startIndex` truncation message.
- `TestWebToolExecution` — `query`/`queries`/`q`, `numResults` clamp 1..10, `url`/`urls`, `startIndex`; unknown tool returns error string, never panics; `sources_from_result` parses titles/URLs.
- `TestWindowProbeState` — `model_windows.json` schema/version; schedule/drop/prune; due order; cap; reset at; attempts; max age; atomic write.
- `TestQuotaScanState` — `quota_scan.json` schema/version; first delay/stagger; 6h interval; 15m retry; max attempts 6; max age 30d; batch 6; no duplicates.
- `TestProbeNoDuplicate429Window` — quota scan skips targets with a window; window probe fires at reset; success clears model cooldown; new 429 re-registers.
- `TestProviderProbeHook` — WorkBuddy implements `ProbeProvider` and maps its existing account/model state to probe results; CodeArts/Raccoon do not; platform returns empty/no-op without a hook.
- `TestQuotaScanRunRoute` — panel-only; 503 when pool unavailable; `{ok, sent}`; force ignores 6h but not 429 window.
- `TestTaskActivityOwnership` — WorkBuddy owns `/tasks`, `/tasks/run`, `/tasks/claim`, `/tasks/travel`, `/tasks/streak-bonus`, `/tasks/buddy`, `/activity/history`; platform has no duplicates; HTTP dispatch reaches provider handler.
- `TestWebRoundsInResponses` — local web tool call is executed, not forwarded; next chat round gets tool results; max 3 rounds; client sees one continuous response and `web_search_call` items.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/platform/webtools ./internal/platform/probes ./internal/api -run 'TestWeb|TestWindow|TestQuota|TestProbe|TestTaskActivity'`
Expected: FAIL, missing packages/routes.

- [ ] **Step 3: Implement**

- Web tools: port v1 engine chain and parsers with injected `*http.Client`; `guard.go` must resolve every address and reject non-public; disable redirects; cap body; strip scripts/styles/tags; cache TTL and single-flight; no secrets in logs.
- Probes: implement v1 state files and scheduling semantics; use provider hook for actual probe request. The provider owns auth/account selection; platform owns due state, retries, logging, and scheduling. Do not duplicate account pools.
- `api/quota.go`: panel-only `/quota-scan/run`; error mapping 503/500; no mutation of provider state outside hook.
- Wire web tools into Responses: detect declared tools, replace server declarations with local function defs, execute internal calls, append tool results, suppress duplicate lifecycle, cap rounds. Chat (`/v1/chat/completions`) keeps v1 behavior: if local web tools are enabled, do the same only where v1 did; otherwise pass through.
- Do not add platform `/tasks` or `/activity` routes. The provider plan owns them. If a provider lacks them, the route returns the provider's v1-shaped 404, not a generic empty success.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/platform/webtools ./internal/platform/probes ./internal/api`
Expected: PASS.
Run: `go test -race ./internal/platform/webtools ./internal/platform/probes`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/webtools internal/platform/probes internal/providers/workbuddy/probes.go internal/providers/workbuddy/probes_test.go internal/api/quota.go internal/api/quota_test.go internal/api/taskactivity_ownership_test.go internal/api/responses.go
git commit -m "feat(platform): add web tools probes and quota scan"
```

---

### Task 11: Update check, install/rollback request, backup/executor contract

**Files:**
- Create: `internal/platform/update/version.go`
- Create: `internal/platform/update/checker.go`
- Create: `internal/platform/update/request.go`
- Create: `internal/platform/update/backups.go`
- Create: `internal/platform/update/executor.go`
- Create: `internal/api/update.go`
- Create: `docs/update-executor-contract.md`
- Test: `internal/platform/update/version_test.go`
- Test: `internal/platform/update/checker_test.go`
- Test: `internal/platform/update/request_test.go`
- Test: `internal/platform/update/backups_test.go`
- Test: `internal/api/update_test.go`

**Interfaces:**
- `ParseVersion(text) (major, minor, patch int, ok bool)`; `IsNewer(candidate,current) bool`; stable `vX.Y.Z` only.
- `Checker`: `NewChecker(currentVersion, settings, fetcher, now, logger)`, `Status()`, `Check(ctx, manual bool)`, `Due(now)`, `StartupDue(now)`, `Stop()`. Fetcher interface takes a URL and returns body; tests inject.
- `RequestManager`: `Install(tag, currentVersion, port, lan, repo) (Installation, error)`, `Rollback(version, currentVersion, port, lan) (Installation, error)`, `Status()`, `Backups()`.
- `Executor`: `Enabled()`, `Kind()` (`systemd|external|portable|none`), `ReadRequest()`, `ReadStatus()`, `ReadBackups()`; actual install/rollback file replacement is Plan 6, not this task.
- Request dir: `WB_UPDATE_REQUEST_DIR` default `/run/workbuddy2api-hub-updater`; `request.json` create-only with `O_EXCL|O_NOFOLLOW`, mode `0600`, fields `action`, `version`, `tag`, `requested_at`, `pid`, `port`, `root`, optional `profile`; `status.json` must be regular, owned by root/current owner per env, size ≤ 8192, mode not group/world writable, state in visible set; `backups.json` same checks, versions/sizes/times only, never paths in panel payload.

- [ ] **Step 1: Write failing tests**

- `TestParseVersionAndCompare` — `v1.2.3`/`1.2.3`; prerelease/date tags rejected; strict newer.
- `TestPickLatestStable` — drafts/prereleases skipped; highest version wins, not list order; malformed releases ignored; release URL only `https://github.com/...`.
- `TestCheckerStatusShape` — current/latest/tag/update_available/enabled/checking/last_attempt/last_success/last_error/release_url/published_at; timestamps formatted `2006-01-02 15:04:05`; no raw URL/body strings persisted except safe release URL.
- `TestCheckerThrottleAndStartupBudget` — 24h window; manual ignores; startup budget 6/hour; disabled sends zero; failure keeps serving; stop prompt.
- `TestCheckerInjectedFetcher` — fake GitHub JSON and redirect fallback; no real network; HTTP errors/timeouts/invalid JSON produce short error, not panic.
- `TestInstallValidation` — install only when enabled; target > current; stable tag; queued 202; missing latest 409; active request 409; no overwrite.
- `TestRollbackValidation` — target valid, differs from current, complete backup exists; arbitrary/partial/missing backup 409; request carries `tag: "v<version>"` for old executors.
- `TestRequestFilePermissions` — create-only `0600`; symlink refused; directory missing → safe error; write failure removes partial request; `fsync`.
- `TestStatusAndBackupReaders` — ownership/mode/size checks; malformed → idle/unavailable; active request with non-active status reports `queued`; complete flag/version only.
- `TestBackups` — systemd inventory source, local/portable source, empty source; `keep_backups`/`keep_backups_max` fields added by route; no paths leaked.
- `TestUpdateRoutes` — `/updates` read-only, `/updates/backups`, `/updates/check` manual, `/updates/install` 202/409, `/updates/rollback` 202/409; panel-only; bodyless POST routes drain body.
- `TestExecutorContract` — documented fields/actions/state set; old executor ignoring `action` treats rollback as reinstall; external marker `executor.json` or `WB_UPDATE_EXECUTOR=external`; systemd requires `WB_UPDATE_INSTALL_ENABLED=1` + unit + enabled link.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/platform/update ./internal/api -run 'TestUpdate|TestVersion|TestPickLatest|TestChecker|TestInstall|TestRollback|TestRequest|TestBackups|TestExecutor'`
Expected: FAIL, missing package/routes.

- [ ] **Step 3: Implement**

- `version.go`/`checker.go`: port v1 `wb_updates.py`; GitHub Releases API first, `https://github.com/<repo>/releases/latest` redirect fallback; 10s timeout; max body 2 MiB; optional `GITHUB_TOKEN` from env only; repository shape `owner/repo`; no token/URL body persistence; state in settings keys `update_check_*`.
- `request.go`: port v1 `wb_update_request.py`; never run systemctl or write application files; only queue request; `install_enabled` = systemd || external || portable; portable path may be reported but actual portable update stays Plan 6.
- `backups.go`: read inventory; validate complete flag, version, size, time; keep newest `keep_backups` (0..3) where v1 did; prune only when allowed.
- `executor.go`: define the contract interface; systemd/external/portable detection; `docs/update-executor-contract.md` states request/status/backups schema, ownership/mode, active/visible states, idempotency, rollback backup requirements, old-executor compatibility.
- `api/update.go`: panel-only routes; `POST /updates/check` inline one check; install reads last status and requires no error/update_available/latest_tag/release_url; rollback reads payload `version`; return 202 queued; 409 Chinese reasons preserved; no secret paths.
- Do not perform network checks on startup in tests; inject fetcher and clock.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/platform/update ./internal/api`
Expected: PASS.
Run: `go test -race ./internal/platform/update`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/update internal/api/update.go internal/api/update_test.go docs/update-executor-contract.md
git commit -m "feat(platform): add update manager and executor contract"
```

---

### Task 12: Daily guard integration, platform wiring, contract suite, Gate D

**Files:**
- Create: `internal/platform/limits/fold.go`
- Create: `internal/platform/limits/guard.go`
- Create: `internal/providers/workbuddy/usage.go`
- Create: `internal/providers/codearts/usage.go`
- Create: `internal/providers/raccoon/usage.go`
- Modify: `internal/providers/workbuddy/stream.go`, `internal/providers/codearts/stream.go`, `internal/providers/raccoon/stream.go` (implement `UsageSnapshotReporter` on stream wrappers)
- Create: `internal/app/platform.go`
- Modify: `internal/app/providers.go`
- Modify: `internal/api/server.go`
- Modify: `internal/api/auth.go`
- Modify: `internal/api/chat.go`
- Modify: `internal/api/responses.go`
- Modify: `internal/api/messages.go`
- Modify: `cmd/mix2api/main.go`
- Create: `tests/integration/platform_services_test.go`
- Create: `tests/contract/platform_services_test.go`
- Create: `tests/contract/testdata/platform/` fixtures
- Create: `tests/oracle/platform-services.md`
- Modify: `tests/boundaries/boundaries_test.go`
- Test: `internal/providers/workbuddy/usage_test.go`
- Test: `internal/providers/codearts/usage_test.go`
- Test: `internal/providers/raccoon/usage_test.go`
- Test: `internal/platform/limits/fold_test.go`
- Test: `internal/platform/limits/guard_test.go`
- Test: `internal/app/platform_test.go`

**Interfaces:**
```go
type Folds struct {
    Tokens  map[string]int64
    Credits map[string]float64
    Models  map[string]map[string]int64
}
func Fold(rows []usage.Row, dayStart, dayEnd time.Time) Folds
func (g *Guard) Apply(ctx context.Context, refresh bool) int
```
- `Fold` groups by provider realm and account uid; only rows with `at` inside the local day; completed rows contribute tokens; non-client-aborted rows contribute credits; per-model tokens keyed by uid/model.
- `Guard.Apply` reads settings `reserve_credits`, `daily_token_limit`, `daily_credit_limit`, `model_daily_token_limit`, `expiring_window_days`; builds v1 `values` (`global` + realm keys) and `usage`; calls each provider implementing `LimitApplier` with only that provider's realm rows. A provider without the hook is untouched. A nil/unknown fold keeps last counters (v1 `usage=None`); disabled limits are pushed as zero/empty to clear stale account fields.
- `internal/app.Platform` composes usage/pricing/logs/keys/panel/settings/media/agents/updates/limits; `cmd` starts background refreshers/checker only when configured and wires `ServerOptions`.
- `ServerOptions` adds `Platform *app.Platform` (or narrow interfaces) and `KeyStore`; existing env `PANEL_PASSWORD`/`MIX2API_API_KEY` become a bootstrap panel session/password and a synthetic legacy key. Do not keep a second independent auth path for platform routes.

- [ ] **Step 1: Write failing tests**

- `TestFoldPerRealmAndDay` — local midnight boundaries; `intl`/`cn`/`codearts`/`raccoon` separated; same uid across realms not merged; completed-only tokens; non-client-aborted credits; per-model map.
- `TestApplyLimitsWorkBuddyOnly` — WorkBuddy `ApplyLimits` receives v1 shapes and realm values; CodeArts/Raccoon do not receive credits; no cross-provider sums; `reserve_credits`/expiring window pass through.
- `TestGuardDisableClearsLimits` — settings save with all zero calls `ApplyLimits` with zero/empty to clear stale account fields; request hot path with no limits does not scan.
- `TestGuardUnknownUsageDoesNotPark` — unreadable log/partial fold passes nil; accounts do not park; no zeros fabricated.
- `TestGuardRefresh` — settings save forces fold bypass TTL; request path uses TTL; manual refresh visible immediately; a guard error never changes the successful chat response.
- `TestGuardCallSites` — every usage append path invokes `Guard.Apply`; `/settings/save` with limit keys invokes refresh; unrelated settings save does not force a scan.
- `TestUsageRecordsCrossProvider` — CodeArts/Raccoon rows get `cost_policy="<realm>:unpriced"`; media rows `"media"`; no WorkBuddy credit price applied; `realm=all` only WorkBuddy.
- `TestProviderUsageSnapshots` — WorkBuddy/CodeArts/Raccoon stream wrappers implement `providers.UsageSnapshotReporter`; account/realm/token/credit values propagate through admission/stream wrappers; providers without the hook yield empty account and provider realm.
- `TestUsageRecordCallSites` — Chat, Responses, Messages, and media call the recorder on success/error/abort; client abort records `client_aborted`; no double record on `[DONE]` + close.
- `TestPlatformComposition` — one store/registry/usage dir; no duplicate provider registration; no duplicate route; shutdown closes log store/media workers.
- `TestPlatformRoutesContract` — all preserved routes/aliases from the table; status codes, JSON/SSE shapes, content types, `Server` header, CORS, ETag/304, 429 headers.
- `TestGateDDataRoundTrip` — synthetic v1 data dir read by v2; settings/usage/logs/pricing/media/agents/update files preserved; v1-compatible JSON round-trip; unknown keys; file modes; no production data.
- `TestNoNetworkInTests` — fail if any test constructs a real `http.Client` without an injected transport to a non-loopback host; or enforce with a test-only `http.DefaultTransport` guard in `tests/integration`.
- `TestBoundariesPlatformImports` — `providers` does not import `platform`/`api`; `core` does not import `providers`/`api`/`platform`; `platform` may import providers/core; `api` may import platform/providers/core.
- `TestProviderHookOptionality` — dummy provider without hooks compiles and platform returns not-available/no-op; WorkBuddy implements expected hooks; CodeArts/Raccoon capabilities false.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/platform/limits ./internal/app ./tests/integration ./tests/contract ./tests/boundaries -run 'TestFold|TestApplyLimits|TestGuard|TestUsage|TestProviderUsage|TestPlatform|TestGateD|TestBoundaries|TestProviderHook'`
Expected: FAIL, missing composition/contracts.

- [ ] **Step 3: Implement**

- Use the `usage.Recorder` from Task 3 at every API completion point. Fill `RecordInput` from the provider stream's optional `UsageSnapshotReporter`; if absent, use the provider realm and leave `account` empty. Missing usage still records `usage_missing=true`. Never log/record prompts, headers, tokens, or API key secrets. A record failure is logged and never changes the client response.
- `limits/fold.go`: one pass over usage rows; no provider imports; `dayStart`/`dayEnd` from local time; guard only today's rows. Match v1 `_scan_daily_usage` exactly: skip `client_aborted`; skip rows without an account; add `total_tokens` to tokens; add non-zero `credit` to credits (this includes media rows); add tokens to `models[uid][model]`. Do not invent cross-provider aggregation.
- `limits/guard.go`: read settings once; build realm-specific values; call `providers.LimitApplier`; expose a TTL (e.g., 15s) and `refresh=true` bypass; never block request path on a full scan if no limit is configured; log only counts/errors, no secrets. Call `Guard.Apply(ctx, false)` after each completed usage append (cheap cached fold; do not fail the request if it errors) and `Guard.Apply(ctx, true)` from `/settings/save` whenever a limit key changed. The request path must not park an account on a fold error: pass `nil` usage and keep last counters.
- `app/platform.go`: construct usage store/cache, pricing snapshot/refresher, logs store, key store, panel sessions, settings service, media service/task store, agents service, update checker/request manager, limits guard. Wire `SettingsContributor` from WorkBuddy (and any future provider), `RealmSelector`, `MediaProvider`, `ProbeProvider`, `LimitApplier`.
- `api/server.go`: mount platform routes exactly once; add `Server` header `mix2api/2.0.0` (or the exact v2 value chosen by packaging) and CORS for v1 prefixes; preserve `Date`/keepalive. Add `KeyStore` auth that returns key entry in context; panel session bypass; API key middleware remains for provider routes.
- Chat/Responses/Messages: after the stream reaches a terminal state (completed, upstream failed, client aborted), call `usage.Recorder.Record` exactly once, then `Guard.Apply`. Capture TTFT as first byte/frame time, generation as wall minus TTFT, and total elapsed. For non-stream, elapsed covers aggregation. On errors before any upstream response, record `failed` with the status and no usage. Do not record count_tokens. Do not record twice when `[DONE]` and deferred close both run.
- Media: Task 8 already records through this recorder; do not add a second media row in the API handler.
- Provider adapters in `internal/providers/*/usage.go` translate existing stream state into `providers.UsageSnapshot`; do not re-parse frames if the provider already tracks usage. Preserve WorkBuddy credit/free/cost policy, CodeArts token usage, Raccoon credit frame.
- `cmd/mix2api/main.go`: env/flags for `ACCOUNTS_DIR`, `USAGE_DIR`, `WB_PROXY_USAGE_DIR`, `WB_PRICING_FILE`, `WB_MEDIA_DIR`, `WB_UPDATE_*`; start background workers only after listener is ready; graceful shutdown closes logs/media/checker.
- Contract suite: use `tests/contract/normalize.go`; add golden fixtures for every platform route, protocol SSE, usage payloads, logs, pricing, media, agents, updates. Normalize timestamps/IDs/elapsed; compare semantic values. Oracle instructions in `tests/oracle/platform-services.md` require a test-only v1.6.5 copy and fake upstream; never patch the original repo.
- Boundary test: add `go list`/AST checks for the rules above. `platform` may import providers; providers must not import platform/api.
- Gate D checklist must be copied into the final PR description with evidence.

- [ ] **Step 4: Run tests**

Run: `go test ./...`
Expected: PASS.
Run: `go vet ./...`
Expected: PASS.
Run: `go test -race ./internal/platform/... ./internal/api/... ./internal/providers/...`
Expected: PASS.
Run: `CGO_ENABLED=0 go build ./cmd/mix2api`
Expected: PASS.
Run: `CGO_ENABLED=0 go test ./internal/platform/logs ./internal/platform/usage ./internal/platform/pricing`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/limits internal/app internal/api cmd tests
git commit -m "feat(platform): wire daily limits and gate d contracts"
```

---

## Cross-Task Contract Suite

`tests/contract/platform_services_test.go` must cover, at minimum:

1. **Usage**: `/usage` snapshot fields, `/usage/recent` pagination/realm, `/usage/analytics` all-time/window, `/usage/perf` percentiles, `/usage/timeseries` bucket boundaries, `/usage/by-account`, `/usage/remaining`; ETag/304; malformed/legacy rows; no cross-realm merge.
2. **Logs**: memory mode, DB mode, query filters, `since_id`, export redaction, clear; SQLite schema compatibility; disabled mode creates no file.
3. **Pricing**: known/unknown/disabled, bands, markup, policy timeline, override, refresh interval; no network.
4. **Protocols**: Chat, Responses, Responses compact, Messages, count_tokens; non-stream shapes; SSE event order/termination; tools/images/reasoning; error envelopes; aliases; 429 metadata.
5. **Auth/panel**: key realm/model/account/concurrency; panel login/logout/password/status; settings merge; realm; proxy slots; panel-only gates.
6. **Media**: config, models, image generate/edit, video submit/status/cancel/content, queue/retention, ownership, signed URL; capability gating.
7. **Agents**: overview, apply/restore for all six clients, model selection, local-only gate, CLI.
8. **Updates**: status, check, install, rollback, backups, request/status permissions, executor contract.
9. **Limits**: fold per realm, WorkBuddy-only application, disable clears, unknown usage does not park.
10. **Data compatibility**: v1-shaped synthetic fixtures round-trip; settings unknown keys; `usage-summary.json` untouched; `logs.sqlite3` readable; media task/price files; agent state/backups; update files.

Normalization rules follow the master plan: ignore key order, `Date`, `Server`, dynamic UUID/session/request IDs, elapsed/timestamps; retain field presence, types, numeric values, error codes, SSE event types/text concatenation/finish/tool structure/usage. No real network.

## Gate D Checklist

- [ ] `go test ./...` passes.
- [ ] `go vet ./...` passes.
- [ ] `go test -race ./...` passes for platform/api/provider packages (or the whole tree).
- [ ] `CGO_ENABLED=0 go build ./cmd/mix2api` passes on Linux; Windows/OpenWrt build is rechecked in Plan 6.
- [ ] Every preserved route/alias in the table is mounted exactly once; no duplicate `/settings`, `/realm`, `/tasks`, `/activity`, `/codearts`, `/raccoon` handlers.
- [ ] Chat/Responses/Messages non-stream and SSE contract tests pass; termination, client abort, keepalive, tool calls, images, reasoning, compaction, count_tokens, and error envelopes covered.
- [ ] Usage JSONL append is concurrency-safe; snapshot/recent/analytics/perf/series/by-account/remaining shapes match v1; cache checkpoints invalidate on truncation/replacement/pricing change and never serve stale data.
- [ ] `logs.sqlite3` exact schema/indexes/journal mode; disabled mode creates no file; retention caps; redaction on write and export; v1 DB fixture readable.
- [ ] Pricing unknown/disabled/markup/bands/policy timeline/OpenRouter normalization green; no guessed price.
- [ ] API key realm/model/account/concurrency binding green; panel password/session/status/settings/realm/proxy slots green; panel-only gates fail closed.
- [ ] Media images/edits/videos/tasks/queue/cancel/content/signed URL/ownership/retention green; WorkBuddy hook implements media; CodeArts/Raccoon report unsupported without semantic leakage.
- [ ] Agents apply/restore/selection/backups/local-only/CLI green; no config writes outside the gateway machine.
- [ ] Web tools search/fetch/SSRF/cache/rounds green; window/quota probe state and due scheduling green; `/tasks`/`/activity` stay provider-owned.
- [ ] Updates check/install/rollback/backups/request permissions/executor contract green; no real install/restart in this plan.
- [ ] Daily guard applies WorkBuddy limits only; disabled limits clear; unknown usage does not park; no cross-provider credit/token sums.
- [ ] Data compatibility matrix checked with synthetic v1 fixtures; `usage-summary.json` untouched; unknown settings keys survive; file modes preserved.
- [ ] Contract/oracle docs list included and excluded routes; no real network; no production data.
- [ ] The only new external dependency is the pure-Go SQLite driver (and optional PBKDF2 helper), recorded in `go.mod` with `CGO_ENABLED=0` evidence.

## Self-Review

- **Spec coverage:** usage/pricing/logs (§4, §6, §9), protocols (§7), panel/auth/proxy (§8, §9), media/agents/web tools/probes (§9), updates (§9), daily guards (§5.4, §9), contracts/data compatibility (§10). UI remains Plan 5; packaging/real install/performance remain Plan 6.
- **Type consistency:** shared types are defined once in this plan; provider hooks live in `internal/providers`; platform service signatures use `map[string]any` only at the wire boundary; no duplicate `Route`/`AuthClass`/`Provider` definitions.
- **Ordering:** each task depends only on earlier tasks; Task 5/6 share the chat-event parser; Task 7 owns auth before media/agents/update routes; Task 12 is the only composition/gate task.
- **Review Focus mapping:** usage integrity → Tasks 1–3, 12; data compatibility → Tasks 4, 7–11, 12; protocol parity → Tasks 5–6, 12; auth isolation → Task 7, 12; operational safety → Tasks 8–12.
- **No production code:** this document is the only artifact; implementation commits are for later tasks.

## Excluded / Deferred

- Dashboard HTML/CSS/JS, panel layout, login UI, language/theme behavior: Plan 5. This plan only provides the HTTP/data contracts the dashboard consumes.
- Actual update file replacement, portable package extraction, Docker/Windows/OpenWrt packaging, release assets, performance comparison, real-account smoke tests: Plan 6.
- `console` window controls (`/console/state`, `/console/toggle`), shutdown, and other host-specific UI actions: Plan 5 unless an existing route is required by a shipped dashboard flow; if required, preserve the v1 response without adding new semantics.
- `/usage/export` as a new public route: excluded because v1.6.5 has no such route. Internal `Export` remains for tooling/tests. The public exports are `/accounts/export` (provider-owned) and `/logs/export` (Task 4).
- `usage-summary.json` regeneration: excluded; v1.6.5 never reads it and explicitly documents it as delete-safe.
- Provider-specific media/agents/probe implementations beyond the hooks described here: WorkBuddy is the only media generator; CodeArts/Raccoon must not inherit WorkBuddy semantics. New providers implement the optional hooks or return unsupported.

## Concerns and Review Questions

1. **SQLite dependency:** `modernc.org/sqlite` is pure Go and CGO-free but increases binary size. If packaging rejects it, the only allowed alternative is another pure-Go driver with the same `database/sql` schema contract; do not switch to CGO without a spec exception.
2. **Usage export naming:** the task brief mentions "usage export"; v1.6.5 has no `/usage/export`. This plan provides internal NDJSON export and keeps the v1 public exports. Confirm with the owner before adding a new public route.
3. **`wb_logs*` naming:** v1.6.5's module is `wb_logstore.py`; no `wb_logs.py` exists. The plan uses the actual file.
4. **Route ownership migration:** platform must own `/settings`, `/settings/save`, `/realm` to avoid duplicate mux patterns. WorkBuddy's route list changes but its settings/realm behavior moves behind exported contributor/selector methods. This needs a focused review to ensure no WorkBuddy admin regression.
5. **Media hook placement:** v1 media account rotation/budget lived in the HTTP handler. Moving it into WorkBuddy `MediaProvider` is required for provider modularity; the platform must not reimplement WorkBuddy credit/rotation rules.
6. **Update executor:** Plan 4 only queues and validates requests. Actual install/rollback/restart is Plan 6; Gate D must not claim a real update was performed.
7. **Web tools scope:** the full v1 engine chain is large. If schedule pressure appears, keep the same public tool names/response strings and fake-HTTP tests; do not silently drop SSRF/redirect protections.
8. **Usage append on Windows:** v1 used `msvcrt.locking` for cross-process append because Python's CRT could interleave writes. Go's `O_APPEND` maps to append semantics and a single `Write` is expected to be atomic for line-sized writes, but this must be proven with a multi-process Windows test in Plan 6. If it is not guaranteed, add a platform-specific lock-file implementation (`LockFileEx` on Windows) rather than weakening the ledger.

---

**Docs-only commit for this plan (run now):**

```bash
git add docs/superpowers/plans/2026-11-08-v2-platform-services.md
git commit -m "docs: add v2 platform services plan"
```
