package workbuddy

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// catalogRemotePath is the desktop client's product-config endpoint. The
	// desktop cache file is this same response written to disk.
	catalogRemotePath = "/v3/config"

	catalogHTTPTimeout     = 10 * time.Second
	catalogMaxBodyBytes    = 32 << 20
	catalogRefreshCooldown = 30 * time.Second
	catalogMaxTTL          = 300 * time.Second
	catalogDailyInterval   = 24 * time.Hour
	defaultCatalogContext  = int64(1_000_000)

	// maxCatalogRepresentativeAccounts bounds the fallback across credentials
	// when the first representative cannot answer /v3/config.
	maxCatalogRepresentativeAccounts = 3
)

// Model is one WorkBuddy /v1/models entry. It is the shared provider model
// map, kept as an alias so the catalog code matches the provider contract.
type Model = providers.Model

//go:embed static_intl.json
var staticIntlJSON []byte

//go:embed static_cn.json
var staticCNJSON []byte

// catalogEntry is the provider-owned core model item. Meta is the exact v1
// desktop model object (or a live remote object) so unknown metadata survives
// and can be exposed by later tasks.
type catalogEntry struct {
	ID   string
	Meta map[string]any
}

// catalogSnapshot is the last successfully built catalogue for this provider.
type catalogSnapshot struct {
	entries    []catalogEntry
	fetchedAt  time.Time
	source     string
	fileMTime  *time.Time
	lastRemote time.Time
}

// CatalogMeta is the provenance of the last built list, matching v1
// catalog_meta().
type CatalogMeta struct {
	Source    string   `json:"source"`
	FetchedAt float64  `json:"fetched_at"`
	FileMTime *float64 `json:"file_mtime"`
}

// CatalogResult is the manual refresh result rendered by the panel. A soft
// upstream failure keeps the previous list and returns OK=false with no error.
type CatalogResult struct {
	OK         bool    `json:"ok"`
	Realm      string  `json:"realm"`
	Source     *string `json:"source"`
	Count      int     `json:"count"`
	Cooldown   bool    `json:"cooldown,omitempty"`
	RetryAfter float64 `json:"retry_after,omitempty"`
	Msg        string  `json:"msg"`
}

var (
	staticIntlModels = decodeStaticCatalog(staticIntlJSON)
	staticCNModels   = decodeStaticCatalog(staticCNJSON)
)

// intlUIOrder and cnUIOrder are the v1 curated picker order tables. A static
// entry outside its realm's table is a legacy model and is only advertised
// when the live desktop catalogue still lists it.
var intlUIOrder = []string{
	"deepseek-v4.1-flash", "deepseek-v4.1-flash-sg", "hy4-preview", "hy3",
	"gpt-6-astra", "gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol",
	"gpt-5.6-terra", "gpt-5.6-luna", "grok-4.7", "gemini-3.8-flash",
	"glm-5.3", "glm-5.3-flash", "kimi-k3.1-pro",
	"kimi-k3.1-pro-highspeed", "kimi-k3", "minimax-m3.1-flash",
}

var cnUIOrder = []string{
	"hy4-preview-f", "hy3", "deepseek-v4.1-flash", "glm-5.3",
	"glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo", "minimax-m3",
	"kimi-k3-1", "kimi-k2.8-preview", "kimi-k2.7", "kimi-k2.6",
	"deepseek-v4-pro",
}

// v1's virtual aliases and non-chat entries are picker shorthands, helper
// models, text-completion/IDE-inline models, or media generators. None is a
// chat model a client can pin.
var virtualAliasModels = map[string]bool{
	"default-model":  true,
	"fast-model":     true,
	"balanced-model": true,
	"primary-model":  true,
	"deep-model":     true,
	"auto":           true,
}

var nonChatPrefixes = []string{"codewise-", "completion-"}

var nonChatSuffixes = []string{"-image-alpha", "-image-alpha-edit", "-taco-completion"}

var nonChatIDs = map[string]bool{
	"lite":           true,
	"hy-image-3.5":   true,
	"kling-video-v3": true,
}

// variantSuffixes mark a paid regional/experimental build of a plain name.
var variantSuffixes = []string{"-sg", "-x"}

// modelsDevLimits is v1 wb_modelsdev.CONTEXT_FALLBACK: the bundled models.dev
// consensus used only when a live-only model carries no limits of its own.
var modelsDevLimits = map[string][2]int64{
	"glm-5.2":                 {1000000, 131072},
	"glm-5.1":                 {200000, 131072},
	"glm-5.3":                 {1000000, 131072},
	"glm-5.3-flash":           {1000000, 131072},
	"glm-5v-turbo":            {200000, 131072},
	"kimi-k2.7":               {256000, 65536},
	"kimi-k2.6":               {256000, 262144},
	"kimi-k2.5":               {164000, 262144},
	"kimi-k3":                 {1048576, 131072},
	"kimi-k3.1-pro":           {1048576, 131072},
	"kimi-k3.1-pro-highspeed": {1048576, 131072},
	"kimi-k2.8-preview":       {1048576, 0},
	"minimax-m3":              {512000, 512000},
	"minimax-m3.1-flash":      {1048576, 128000},
	"hy3":                     {192000, 64000},
	"hy3-preview":             {262144, 64000},
	"hy4-preview":             {1000000, 64000},
	"hy4-preview-x":           {1000000, 64000},
	"deepseek-v4-pro":         {1000000, 384000},
	"deepseek-v4-flash":       {1000000, 384000},
	"deepseek-v4.1-flash":     {1000000, 384000},
	"deepseek-v4.1-flash-sg":  {1000000, 384000},
	"gpt-6-astra":             {1050000, 128000},
	"gpt-6.1-sol":             {1050000, 128000},
	"gpt-6-sol":               {1050000, 128000},
	"gpt-6-luna":              {1050000, 128000},
	"gpt-5.6-sol":             {1050000, 128000},
	"gpt-5.6-terra":           {1050000, 128000},
	"gpt-5.6-luna":            {1050000, 128000},
	"gpt-5.5":                 {1050000, 128000},
	"gpt-5.4":                 {1050000, 128000},
	"gpt-5.3-codex":           {400000, 128000},
	"gemini-3.8-flash":        {1048576, 65536},
	"gemini-3.5-flash":        {1048576, 65536},
	"auto":                    {168000, 0},
}

func decodeStaticCatalog(raw []byte) []catalogEntry {
	var models []map[string]any
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil
	}
	out := make([]catalogEntry, 0, len(models))
	for _, meta := range models {
		id := metaText(meta["id"])
		if id == "" {
			continue
		}
		out = append(out, catalogEntry{ID: id, Meta: meta})
	}
	return out
}

func staticCatalogForRealm(realm string) []catalogEntry {
	if realmOrDefault(realm) == realmCN {
		return staticCNModels
	}
	return staticIntlModels
}

func staticCatalogIDs(realm string) []string {
	models := staticCatalogForRealm(realm)
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ID)
	}
	return out
}

func staticMetaForRealm(realm, modelID string) (map[string]any, bool) {
	id := strings.TrimSpace(modelID)
	for _, model := range staticCatalogForRealm(realm) {
		if model.ID == id {
			return model.Meta, true
		}
	}
	return nil, false
}

func catalogUIOrder(realm string) []string {
	if realmOrDefault(realm) == realmCN {
		return cnUIOrder
	}
	return intlUIOrder
}

func isChatModel(modelID string) bool {
	id := strings.TrimSpace(modelID)
	if id == "" {
		return false
	}
	if virtualAliasModels[id] || nonChatIDs[id] {
		return false
	}
	for _, prefix := range nonChatPrefixes {
		if strings.HasPrefix(id, prefix) {
			return false
		}
	}
	for _, suffix := range nonChatSuffixes {
		if strings.HasSuffix(id, suffix) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Remote/desktop parsing
// ---------------------------------------------------------------------------

func agentModelLists(payload map[string]any) ([][]string, [][]string) {
	roots := []map[string]any{payload}
	if data, ok := payload["data"].(map[string]any); ok {
		roots = append(roots, data)
	}
	cli := make([][]string, 0)
	other := make([][]string, 0)
	for _, root := range roots {
		switch agents := root["agents"].(type) {
		case map[string]any:
			keys := make([]string, 0, len(agents))
			for key := range agents {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				entry, _ := agents[key].(map[string]any)
				if list, ok := agentModelList(key, entry); ok {
					if strings.EqualFold(strings.TrimSpace(key), "cli") {
						cli = append(cli, list)
					} else {
						other = append(other, list)
					}
				}
			}
		case []any:
			for _, raw := range agents {
				entry, _ := raw.(map[string]any)
				name := metaText(entry["name"])
				if list, ok := agentModelList(name, entry); ok {
					if strings.EqualFold(strings.TrimSpace(name), "cli") {
						cli = append(cli, list)
					} else {
						other = append(other, list)
					}
				}
			}
		}
	}
	return cli, other
}

func agentModelList(name string, entry map[string]any) ([]string, bool) {
	if entry == nil {
		return nil, false
	}
	rawModels, ok := entry["models"].([]any)
	if !ok || len(rawModels) == 0 {
		return nil, false
	}
	// v1 only accepts a bare-string list. A product metadata array is read by
	// the metadata walker, never as the picker list.
	if _, ok := rawModels[0].(string); !ok {
		return nil, false
	}
	out := make([]string, 0, len(rawModels))
	for _, raw := range rawModels {
		model, ok := raw.(string)
		if !ok {
			continue
		}
		model = strings.TrimSpace(model)
		if model != "" {
			out = append(out, model)
		}
	}
	_ = name
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func parseRemoteCatalog(payload map[string]any) ([]string, map[string]map[string]any, bool) {
	if payload == nil {
		return nil, nil, false
	}
	cli, other := agentModelLists(payload)
	pool := cli
	if len(pool) == 0 {
		pool = other
	}
	best := []string(nil)
	for _, list := range pool {
		if len(list) > len(best) {
			best = list
		}
	}
	ids := make([]string, 0, len(best))
	seen := make(map[string]bool, len(best))
	for _, model := range best {
		if !seen[model] {
			seen[model] = true
			ids = append(ids, model)
		}
	}
	if len(ids) == 0 {
		return nil, nil, false
	}

	meta := make(map[string]map[string]any)
	walkRemoteMeta(payload, meta)
	return ids, meta, true
}

func walkRemoteMeta(node any, meta map[string]map[string]any) {
	switch typed := node.(type) {
	case map[string]any:
		if models, ok := typed["models"].([]any); ok && len(models) > 0 {
			if _, ok := models[0].(map[string]any); ok {
				for _, raw := range models {
					item, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					id := metaText(item["id"])
					if id != "" {
						if _, exists := meta[id]; !exists {
							meta[id] = item
						}
					}
				}
			}
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			walkRemoteMeta(typed[key], meta)
		}
	case []any:
		for _, value := range typed {
			walkRemoteMeta(value, meta)
		}
	}
}

func curateRemoteCatalog(realm string, ids []string, meta map[string]map[string]any) []string {
	credits := make(map[string]string)
	for _, source := range [][]catalogEntry{staticIntlModels, staticCNModels} {
		for _, item := range source {
			credit := metaText(item.Meta["credits"])
			if _, exists := credits[item.ID]; !exists {
				credits[item.ID] = credit
			}
		}
	}
	for mid, item := range meta {
		credits[mid] = strings.ToLower(metaText(item["credits"]))
	}

	listed := make(map[string]bool, len(ids))
	for _, id := range ids {
		listed[id] = true
	}
	free := func(id string) bool {
		return creditsIsFree(credits[id])
	}

	out := make([]string, 0, len(ids))
	for _, mid := range ids {
		if !isChatModel(mid) {
			continue
		}
		if base := variantBase(mid); base != "" && listed[base] {
			// The plain name is in this same catalogue: the suffixed build is
			// its paid variant and the free-sibling rules decide below.
			continue
		}
		if strings.HasSuffix(mid, "-f") {
			base := strings.TrimSuffix(mid, "-f")
			if listed[base] && free(base) && !free(mid) {
				continue
			}
		} else if sibling := mid + "-f"; listed[sibling] && free(sibling) && !free(mid) {
			continue
		}
		out = append(out, mid)
	}
	return out
}

func variantBase(modelID string) string {
	for _, suffix := range variantSuffixes {
		if strings.HasSuffix(modelID, suffix) && modelID != suffix {
			return strings.TrimSuffix(modelID, suffix)
		}
	}
	return ""
}

func mergeCatalog(primary []catalogEntry, realm string, extras bool) []catalogEntry {
	realm = realmOrDefault(realm)
	primaryIDs := make(map[string]bool, len(primary))
	for _, item := range primary {
		primaryIDs[item.ID] = true
	}

	merged := make(map[string]map[string]any)
	seenStatic := make(map[string]bool)
	for _, item := range staticCatalogForRealm(realm) {
		if item.ID == "hy4-preview-f" && primaryIDs["hy4-preview"] && !primaryIDs["hy4-preview-f"] {
			continue
		}
		if !isChatModel(item.ID) || seenStatic[item.ID] {
			continue
		}
		seenStatic[item.ID] = true
		merged[item.ID] = cloneMeta(item.Meta)
	}

	for _, item := range primary {
		if !isChatModel(item.ID) {
			continue
		}
		base, exists := merged[item.ID]
		if len(item.Meta) == 0 {
			if !exists {
				merged[item.ID] = map[string]any{}
			}
			continue
		}
		if !exists {
			base = map[string]any{}
			merged[item.ID] = base
		}
		live := cloneMeta(item.Meta)
		// Capture the bundled reasoning block before the live overlay replaces
		// it. A live entry often carries only a bare "effort" while the
		// bundled table still knows the selectable effort list.
		baseReasoning := base["reasoning"]
		// A live x0.00 placeholder must not downgrade a model the bundled
		// table knows is paid (v1 issue #774).
		if creditsIsFree(live["credits"]) && metaText(base["credits"]) != "" && !creditsIsFree(base["credits"]) {
			delete(live, "credits")
		}
		for key, value := range live {
			base[key] = value
		}
		if liveReasoning, ok := metaMap(item.Meta["reasoning"]); ok {
			base["reasoning"] = mergeReasoning(baseReasoning, liveReasoning)
		}
	}

	out := make([]catalogEntry, 0, len(merged))
	seen := make(map[string]bool, len(merged))
	for _, id := range effectiveCatalogUIOrder(realm, primaryIDs) {
		if item, ok := merged[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, catalogEntry{ID: id, Meta: item})
		}
	}
	if extras {
		// A model the curated table has never heard of still ships when the
		// live catalogue lists it with a configured credit multiplier. The
		// bundled snapshot alone is not enough: it carries legacy entries the
		// picker may not show.
		for _, item := range primary {
			if seen[item.ID] {
				continue
			}
			meta, ok := merged[item.ID]
			if !ok || metaText(meta["credits"]) == "" {
				continue
			}
			seen[item.ID] = true
			out = append(out, catalogEntry{ID: item.ID, Meta: meta})
		}
	}
	return out
}

// effectiveCatalogUIOrder applies v1's hy4-preview remap: when the live
// catalogue lists the paid plain name but not the free -f sibling, the first
// UI slot (which normally holds hy4-preview-f) advertises hy4-preview. The
// same remap runs for remote and legacy desktop lists because both pass
// through mergeCatalog.
func effectiveCatalogUIOrder(realm string, primaryIDs map[string]bool) []string {
	order := append([]string(nil), catalogUIOrder(realm)...)
	if primaryIDs["hy4-preview"] && !primaryIDs["hy4-preview-f"] {
		for i, id := range order {
			if id == "hy4-preview-f" {
				order[i] = "hy4-preview"
			}
		}
	}
	return order
}

func mergeReasoning(base any, live map[string]any) map[string]any {
	out := make(map[string]any)
	if baseMap, ok := metaMap(base); ok {
		for key, value := range baseMap {
			out[key] = cloneJSONValue(value)
		}
	}
	for key, value := range live {
		out[key] = cloneJSONValue(value)
	}
	if efforts := metaStringList(out["supportedEfforts"]); len(efforts) > 0 {
		if effort := metaText(out["effort"]); effort != "" {
			if metaText(out["defaultEffort"]) == "" {
				out["defaultEffort"] = effort
			}
			delete(out, "effort")
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Provider catalogue lifecycle
// ---------------------------------------------------------------------------

// Models returns this realm's /v1/models catalogue. The cache honours the
// configured models TTL, capped at v1's 300 seconds. force bypasses the cache.
// A failed refresh never blanks discovery: it returns the last good catalogue,
// then the bundled snapshot.
func (p *Provider) Models(ctx context.Context, force bool) ([]Model, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("workbuddy: model catalog requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := p.catalogNowTime()
	if !force {
		if entries, ok := p.freshCatalog(now); ok {
			p.applyProOnlyModels()
			return p.modelEntries(entries), nil
		}
	}

	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()

	now = p.catalogNowTime()
	if !force {
		if entries, ok := p.freshCatalog(now); ok {
			p.applyProOnlyModels()
			return p.modelEntries(entries), nil
		}
	}

	previous := p.snapshotCatalog()
	entries, source, fileMTime, ok := p.loadCatalog(ctx, previous)
	if !ok || len(entries) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(previous.entries) > 0 {
			// Last good wins over the bundled table. The cache timestamp is
			// not advanced, so the next read after the TTL retries upstream.
			p.applyProOnlyModels()
			return p.modelEntries(previous.entries), nil
		}
		entries = mergeCatalog(nil, p.realm, false)
		source = "bundled"
		now = p.catalogNowTime()
	}
	p.storeCatalog(entries, source, p.catalogNowTime(), fileMTime)
	p.applyProOnlyModels()
	return p.modelEntries(entries), nil
}

// ModelEntries is the explicit wire-entry form of Models. The API layer uses
// Models; this alias keeps the catalogue contract discoverable for later
// WorkBuddy-specific routes.
func (p *Provider) ModelEntries(ctx context.Context, force bool) ([]Model, error) {
	return p.Models(ctx, force)
}

// AgentPickerModels returns the current realm's catalogue for the agent picker.
// Unlike CodeArts there is no lowercase/canonicalisation step: WorkBuddy model
// IDs are preserved exactly. realm=all is an API-layer union, so a provider
// never reads the sibling realm.
func (p *Provider) AgentPickerModels(ctx context.Context, force bool) ([]Model, error) {
	models, err := p.Models(ctx, force)
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(models))
	for _, model := range models {
		clone := make(Model, len(model))
		for key, value := range model {
			clone[key] = value
		}
		out = append(out, clone)
	}
	return out, nil
}

func (p *Provider) loadCatalog(ctx context.Context, previous catalogSnapshot) ([]catalogEntry, string, *time.Time, bool) {
	if entries, ok := p.fetchRemoteCatalog(ctx); ok {
		return entries, "remote", nil, true
	}
	// A previously live-refreshed list is the most recent evidence this
	// process has. Keep it ahead of the desktop file, which may be far older,
	// so an upstream outage cannot silently regress a fresh catalogue.
	if previous.source == "remote" && len(previous.entries) > 0 {
		return nil, "", nil, false
	}
	if entries, source, fileMTime, ok := p.readDesktopCatalog(); ok {
		return entries, source, fileMTime, true
	}
	return nil, "", nil, false
}

func (p *Provider) fetchRemoteCatalog(ctx context.Context) ([]catalogEntry, bool) {
	accounts := p.representativeCatalogAccounts()
	if len(accounts) == 0 {
		return nil, false
	}
	url := p.catalogURL()
	doer := p.catalogDoer
	if doer == nil {
		doer = p.transport
	}
	if doer == nil {
		return nil, false
	}
	timeout := p.catalogTimeout
	if timeout <= 0 {
		timeout = catalogHTTPTimeout
	}

	for _, account := range accounts {
		if strings.TrimSpace(account.AccessToken) == "" {
			continue
		}
		for _, ua := range p.catalogUAs() {
			attemptCtx, cancel := context.WithTimeout(ctx, timeout)
			req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, url, nil)
			if err != nil {
				cancel()
				continue
			}
			req.Header = p.catalogHeaders(account, ua)
			resp, err := doer.Do(attemptCtx, req, account.ProxyURL)
			if err != nil {
				cancel()
				continue
			}
			payload, readErr := readCatalogPayload(resp)
			cancel()
			if readErr != nil {
				continue
			}
			ids, meta, ok := parseRemoteCatalog(payload)
			if !ok {
				continue
			}
			if ingestModelTiers(p.realm, payload) {
				p.applyProOnlyModels()
			}
			live := make([]catalogEntry, 0, len(ids))
			for _, id := range curateRemoteCatalog(p.realm, ids, meta) {
				live = append(live, catalogEntry{ID: id, Meta: meta[id]})
			}
			entries := mergeCatalog(live, p.realm, true)
			if len(entries) == 0 {
				continue
			}
			p.markRemoteCatalog(p.catalogNowTime())
			if account.Account != nil {
				// Keep the representative account's per-account catalogue fresh
				// for diagnostics and later per-account model selection. The
				// provider cache remains the source shared by /v1/models.
				account.Account.SetModels(p.modelEntries(entries))
			}
			return entries, true
		}
	}
	return nil, false
}

func readCatalogPayload(resp *http.Response) (map[string]any, error) {
	if resp == nil || resp.Body == nil {
		return nil, errors.New("workbuddy: empty catalog response")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, catalogMaxBodyBytes+1))
	if err != nil {
		return nil, errors.New("workbuddy: catalog response body unreadable")
	}
	if len(body) > catalogMaxBodyBytes {
		return nil, errors.New("workbuddy: catalog response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("workbuddy: catalog endpoint rejected the request")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, errors.New("workbuddy: catalog response is not a JSON object")
	}
	return payload, nil
}

func (p *Provider) readDesktopCatalog() ([]catalogEntry, string, *time.Time, bool) {
	path := p.catalogDesktopFilePath()
	if strings.TrimSpace(path) == "" {
		return nil, "", nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil || payload == nil {
		return nil, "", nil, false
	}
	var fileMTime *time.Time
	if info, statErr := os.Stat(path); statErr == nil {
		value := info.ModTime()
		fileMTime = &value
	}
	// v1 ingests the tier map from the desktop cache even when the cached
	// catalogue itself is unusable; only a payload without modelTiers keeps
	// the previous map.
	if ingestModelTiers(p.realm, payload) {
		p.applyProOnlyModels()
	}

	if ids, meta, ok := parseRemoteCatalog(payload); ok {
		live := make([]catalogEntry, 0, len(ids))
		for _, id := range curateRemoteCatalog(p.realm, ids, meta) {
			live = append(live, catalogEntry{ID: id, Meta: meta[id]})
		}
		return mergeCatalog(live, p.realm, true), "cache", fileMTime, true
	}

	legacy := legacyDesktopEntries(payload)
	if len(legacy) == 0 {
		return nil, "", nil, false
	}
	ids := make([]string, 0, len(legacy))
	meta := make(map[string]map[string]any, len(legacy))
	for _, entry := range legacy {
		ids = append(ids, entry.ID)
		meta[entry.ID] = entry.Meta
	}
	live := make([]catalogEntry, 0, len(ids))
	for _, id := range curateRemoteCatalog(p.realm, ids, meta) {
		live = append(live, catalogEntry{ID: id, Meta: meta[id]})
	}
	return mergeCatalog(live, p.realm, false), "legacy", fileMTime, true
}

// legacyDesktopEntries reads the old desktop cache shape: a top-level models
// array without the agents[].models picker list.
func legacyDesktopEntries(payload map[string]any) []catalogEntry {
	var models []any
	var find func(any) []any
	find = func(node any) []any {
		if models != nil {
			return nil
		}
		switch typed := node.(type) {
		case map[string]any:
			if rawList, ok := typed["models"].([]any); ok && len(rawList) > 0 {
				if _, ok := rawList[0].(map[string]any); ok {
					models = rawList
					return nil
				}
			}
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				find(typed[key])
				if models != nil {
					break
				}
			}
		case []any:
			for _, value := range typed {
				find(value)
				if models != nil {
					break
				}
			}
		}
		return nil
	}
	find(payload)
	out := make([]catalogEntry, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, raw := range models {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := metaText(item["id"])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, catalogEntry{ID: id, Meta: item})
	}
	return out
}

func (p *Provider) catalogDesktopFilePath() string {
	if p.catalogDesktopPath != nil {
		return p.catalogDesktopPath(p.realm)
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	dir := ".workbuddy-ai"
	if p.realm == realmCN {
		dir = ".workbuddy"
	}
	return filepath.Join(home, dir, "cache", "acc-product-config-v3.json")
}

type catalogAccountSnapshot struct {
	UID         string
	AccessToken string
	APIKey      bool
	ProxyURL    string
	Account     *Account
}

// representativeCatalogAccounts returns usable accounts in pool order. v1
// tried one representative per realm; v2 falls back to the next credential when
// the first one is dead so a stale account cannot freeze /v1/models for the
// whole realm. The list is capped to avoid turning an outage into a credential
// sweep.
func (p *Provider) representativeCatalogAccounts() []catalogAccountSnapshot {
	if p == nil || p.pool == nil {
		return nil
	}
	snapshots := make([]catalogAccountSnapshot, 0, maxCatalogRepresentativeAccounts)
	for _, account := range p.pool.Accounts() {
		if account == nil {
			continue
		}
		account.mu.RLock()
		removed := account.removed
		auth := cloneAuth(account.Auth)
		account.mu.RUnlock()
		if removed || auth.Realm != p.realm || strings.TrimSpace(auth.AccessToken) == "" {
			continue
		}
		snapshots = append(snapshots, catalogAccountSnapshot{
			UID:         strings.TrimSpace(auth.UID),
			AccessToken: strings.TrimSpace(auth.AccessToken),
			APIKey:      auth.APIKey || IsAPIKeyToken(auth.AccessToken),
			ProxyURL:    strings.TrimSpace(auth.ProxyURL),
			Account:     account,
		})
		if len(snapshots) >= maxCatalogRepresentativeAccounts {
			break
		}
	}
	return snapshots
}

func (p *Provider) representativeCatalogAccount() (catalogAccountSnapshot, bool) {
	accounts := p.representativeCatalogAccounts()
	if len(accounts) == 0 {
		return catalogAccountSnapshot{}, false
	}
	return accounts[0], true
}

func (p *Provider) hasUsableCatalogAccount() bool {
	account, ok := p.representativeCatalogAccount()
	return ok && account.AccessToken != ""
}

func (p *Provider) catalogURL() string {
	baseURL := strings.TrimSpace(p.catalogBaseURL)
	if baseURL == "" {
		baseURL = p.EndpointFor(productDesktop)
	}
	return strings.TrimRight(baseURL, "/") + catalogRemotePath
}

func (p *Provider) catalogUAs() []string {
	cfg := p.Config()
	first := desktopUAWithVersions(p.realm, cfg.IdentityVersions)
	second := "WorkBuddy/" + desktopVersionWithVersions(p.realm, cfg.IdentityVersions)
	out := make([]string, 0, 2)
	for _, ua := range []string{first, second} {
		ua = strings.TrimSpace(ua)
		if ua == "" {
			continue
		}
		duplicate := false
		for _, existing := range out {
			if existing == ua {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, ua)
		}
	}
	return out
}

func (p *Provider) catalogHeaders(account catalogAccountSnapshot, ua string) http.Header {
	origin := catalogOriginForRealm(p.realm)
	headers := make(http.Header)
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("User-Agent", ua)
	headers.Set("Origin", origin)
	headers.Set("Referer", origin+"/")
	headers.Set("Authorization", "Bearer "+account.AccessToken)
	// v1 writes X-User-Id unconditionally, including an empty uid.
	headers.Set("X-User-Id", account.UID)
	if account.APIKey {
		// A static API key must send both auth headers. Authorization alone
		// made the endpoint omit four models while still answering HTTP 200.
		headers.Set("X-API-Key", account.AccessToken)
	}
	return headers
}

func catalogOriginForRealm(realm string) string {
	if realmOrDefault(realm) == realmCN {
		return "https://www.codebuddy.cn"
	}
	return "https://www.workbuddy.ai"
}

// ---------------------------------------------------------------------------
// Manual refresh, provenance, daily hook
// ---------------------------------------------------------------------------

// RefreshCatalog performs a manual catalogue refresh. It always bypasses the
// cache and never reads the desktop cache file; the panel button must report
// whether the upstream API really answered. A 30s per-provider cooldown blocks
// double clicks and also covers failed attempts. Failure keeps the previous
// list unchanged.
func (p *Provider) RefreshCatalog(ctx context.Context) (CatalogResult, error) {
	if p == nil {
		return CatalogResult{}, errors.New("workbuddy: provider is nil")
	}
	result := CatalogResult{Realm: p.realm}
	if ctx == nil {
		return result, errors.New("workbuddy: catalog refresh requires a context")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()

	now := p.catalogNowTime()
	cooldown := p.catalogCooldown
	if cooldown <= 0 {
		cooldown = catalogRefreshCooldown
	}
	if !p.catalogLastRefresh.IsZero() {
		wait := cooldown - now.Sub(p.catalogLastRefresh)
		if wait > 0 {
			return CatalogResult{
				Realm:      p.realm,
				Cooldown:   true,
				RetryAfter: math.Round(wait.Seconds()*10) / 10,
				Msg:        "刷新太频繁：约 " + strconv.Itoa(maxInt(1, int(wait.Seconds()+0.5))) + " 秒后可再试",
			}, nil
		}
	}
	p.catalogLastRefresh = now

	entries, ok := p.fetchRemoteCatalog(ctx)
	if !ok {
		if err := ctx.Err(); err != nil {
			return CatalogResult{Realm: p.realm}, err
		}
		return CatalogResult{
			Realm: p.realm,
			Msg:   "上游未返回可用目录（账号不可用或网络异常），已保留现有清单",
		}, nil
	}
	p.storeCatalog(entries, "remote", p.catalogNowTime(), nil)
	p.applyProOnlyModels()
	source := "remote"
	return CatalogResult{
		OK:     true,
		Realm:  p.realm,
		Source: &source,
		Count:  len(entries),
		Msg:    "已从上游 API 更新清单，共 " + strconv.Itoa(len(entries)) + " 个模型",
	}, nil
}

// CatalogMeta returns the provenance of the last built catalogue.
func (p *Provider) CatalogMeta() (CatalogMeta, bool) {
	if p == nil {
		return CatalogMeta{}, false
	}
	p.catalogStateMu.RLock()
	state := p.catalogState
	p.catalogStateMu.RUnlock()
	if len(state.entries) == 0 || state.fetchedAt.IsZero() {
		return CatalogMeta{}, false
	}
	meta := CatalogMeta{
		Source:    state.source,
		FetchedAt: float64(state.fetchedAt.UnixNano()) / float64(time.Second),
	}
	if state.fileMTime != nil {
		value := float64(state.fileMTime.UnixNano()) / float64(time.Second)
		meta.FileMTime = &value
	}
	return meta, true
}

// CatalogDailyDue reports whether this realm has gone at least 24h without a
// successful /v3/config fetch (or has never fetched).
func (p *Provider) CatalogDailyDue(now time.Time) bool {
	if p == nil {
		return false
	}
	p.catalogStateMu.RLock()
	last := p.catalogState.lastRemote
	p.catalogStateMu.RUnlock()
	return last.IsZero() || now.Sub(last) >= catalogDailyInterval
}

// CatalogDailyRefresh is the daily background hook. It silently skips realms
// with no usable account and returns done=false when the realm is not due.
func (p *Provider) CatalogDailyRefresh(ctx context.Context) (CatalogResult, bool, error) {
	if p == nil {
		return CatalogResult{}, false, errors.New("workbuddy: provider is nil")
	}
	if !p.CatalogDailyDue(p.catalogNowTime()) {
		return CatalogResult{Realm: p.realm}, false, nil
	}
	if !p.hasUsableCatalogAccount() {
		return CatalogResult{Realm: p.realm}, false, nil
	}
	result, err := p.RefreshCatalog(ctx)
	return result, true, err
}

// CatalogProvenance returns the v1 /v1/models catalog block.
func (p *Provider) CatalogProvenance() map[string]any {
	if p == nil {
		return nil
	}
	meta, ok := p.CatalogMeta()
	if !ok {
		return nil
	}
	return map[string]any{
		"source":     meta.Source,
		"fetched_at": meta.FetchedAt,
		"file_mtime": meta.FileMTime,
	}
}

// CatalogDailyPass runs the once-per-day bundled fallback refresh.
func (p *Provider) CatalogDailyPass(ctx context.Context) (CatalogResult, bool, error) {
	return p.CatalogDailyRefresh(ctx)
}

// ---------------------------------------------------------------------------
// Wire entries
// ---------------------------------------------------------------------------

func (p *Provider) modelEntries(entries []catalogEntry) []Model {
	created := p.catalogNowTime().Unix()
	out := make([]Model, 0, len(entries))
	for _, entry := range entries {
		out = append(out, wireModelEntry(entry.ID, entry.Meta, p.realm, created))
	}
	return out
}

// capabilitiesFor resolves a model's capability map from its realm's bundled
// metadata. It is the static answer for callers without a live metadata map.
func capabilitiesFor(modelID, realm string) map[string]any {
	meta, ok := staticMetaForRealm(realm, modelID)
	if !ok {
		return map[string]any{
			"vision":     false,
			"tool_calls": false,
			"reasoning":  false,
		}
	}
	return capabilitiesFromMeta(modelID, meta)
}

func capabilitiesFromMeta(modelID string, meta map[string]any) map[string]any {
	vision := metaBool(meta["supportsImages"]) && !metaBool(meta["disabledMultimodal"])
	if strings.EqualFold(strings.TrimSpace(modelID), "glm-5.3-flash") {
		vision = false
	}
	return map[string]any{
		"vision":     vision,
		"tool_calls": metaBool(meta["supportsToolCall"]),
		"reasoning":  metaBool(meta["supportsReasoning"]),
	}
}

func wireModelEntry(modelID string, meta map[string]any, realm string, created int64) Model {
	if meta == nil {
		meta = map[string]any{}
	}
	name := metaText(meta["name"])
	if name == "" {
		name = modelID
	}
	entry := Model{
		"id":       modelID,
		"object":   "model",
		"created":  created,
		"owned_by": "workbuddy",
		"provider": "workbuddy",
		"name":     name,
	}
	description := metaText(meta["descriptionEn"])
	if description == "" {
		description = metaText(meta["descriptionZh"])
	}
	if description != "" {
		entry["description"] = description
	}
	category := metaText(meta["category"])
	if category == "" {
		category = "chat"
	}
	entry["category"] = category

	capabilities := capabilitiesFromMeta(modelID, meta)
	vision, _ := capabilities["vision"].(bool)
	tools, _ := capabilities["tool_calls"].(bool)
	thinks, _ := capabilities["reasoning"].(bool)
	inputs := []string{"text"}
	if vision {
		inputs = append(inputs, "image")
	}
	entry["capabilities"] = capabilities
	entry["supports_vision"] = vision
	entry["supports_images"] = vision
	entry["supports_tool_calls"] = tools
	entry["supports_reasoning"] = thinks
	entry["vision"] = vision
	entry["multimodal"] = vision
	entry["abilities"] = map[string]any{
		"vision":        vision,
		"functionCall":  tools,
		"function_call": tools,
		"reasoning":     thinks,
	}
	entry["input_modalities"] = inputs
	entry["output_modalities"] = []string{"text"}
	entry["modalities"] = map[string]any{
		"input":  inputs,
		"output": []string{"text"},
	}
	entry["architecture"] = map[string]any{
		"input_modalities":  inputs,
		"output_modalities": []string{"text"},
		"modality":          strings.Join(inputs, "+") + "->text",
	}
	if strings.EqualFold(modelID, "glm-5.3-flash") {
		entry["vision_note"] = "上游宣告多模态但实际通道不支持读图，已修正为纯文本"
	}

	contextLength, maxOutput := modelLimits(modelID, meta)
	entry["context_length"] = contextLength
	entry["context_window"] = contextLength
	entry["max_input_tokens"] = contextLength
	if maxOutput > 0 {
		entry["max_tokens"] = maxOutput
		entry["max_output_tokens"] = maxOutput
		entry["max_completion_tokens"] = maxOutput
		entry["max_output"] = maxOutput
	}
	if contextWindow, ok := metaMap(meta["contextWindow"]); ok {
		if lengths := metaIntSlice(contextWindow["supportedLengths"]); len(lengths) > 0 {
			entry["context_windows"] = lengths
		}
	}

	reasoning, _ := metaMap(meta["reasoning"])
	if efforts := metaStringList(reasoning["supportedEfforts"]); len(efforts) > 0 {
		entry["reasoning_efforts"] = efforts
	}
	if effort := metaText(reasoning["effort"]); effort != "" {
		entry["reasoning_fixed_effort"] = effort
	}
	if effort := metaText(reasoning["defaultEffort"]); effort != "" {
		entry["reasoning_default_effort"] = effort
	}
	if canDisable, ok := boolValue(reasoning["canDisableThinking"]); ok {
		entry["reasoning_can_disable"] = canDisable
	} else if canDisable, ok := boolValue(meta["canDisableThinking"]); ok {
		entry["reasoning_can_disable"] = canDisable
	}
	if onlyReasoning, ok := boolValue(meta["onlyReasoning"]); ok {
		entry["always_reasoning"] = onlyReasoning
	}

	credits := metaText(meta["credits"])
	if credits != "" {
		entry["credits"] = credits
	}
	entry["free"] = creditsIsFree(credits)
	if peak := firstMetaText(meta, "creditsPeak", "credits_peak", "peakCredits"); peak != "" {
		entry["credits_peak"] = peak
	}
	if offpeak := firstMetaText(meta, "creditsOffpeak", "credits_offpeak", "offpeakCredits"); offpeak != "" {
		entry["credits_offpeak"] = offpeak
	}
	if vendor := metaText(meta["vendor"]); vendor != "" {
		entry["vendor"] = vendor
	}
	if temperature, ok := numberValue(meta["temperature"]); ok {
		entry["temperature"] = temperature
	}
	if topP, ok := numberValue(meta["top_p"]); ok {
		entry["top_p"] = topP
	}
	if isDefault, ok := boolValue(meta["isDefault"]); ok && isDefault {
		entry["is_default"] = true
	}
	tags := make([]string, 0)
	for _, tag := range metaStringList(meta["tags"]) {
		if !strings.HasPrefix(tag, "badge:") {
			tags = append(tags, tag)
		}
	}
	if len(tags) > 0 {
		entry["tags"] = tags
	}
	if aliases := modelAliases(modelID, meta); len(aliases) > 0 {
		entry["aliases"] = aliases
	}
	if requiresPro(modelID, realm) {
		entitlement := map[string]any{
			"source":   "catalog",
			"required": "standard",
			"at":       nil,
		}
		if at, ok := ingestedAt(realm); ok {
			entitlement["at"] = at.Unix()
		}
		entry["entitlement"] = entitlement
	}
	return entry
}

func modelAliases(modelID string, meta map[string]any) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || value == modelID || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	for _, alias := range metaStringList(meta["aliases"]) {
		add(alias)
	}
	if related, ok := metaMap(meta["relatedModels"]); ok {
		keys := make([]string, 0, len(related))
		for key := range related {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if value, ok := related[key].(string); ok {
				add(value)
			}
		}
	}
	sort.Strings(out)
	return out
}

func modelLimits(modelID string, meta map[string]any) (int64, int64) {
	contextLength := metaInt(meta["maxInputTokens"])
	maxOutput := metaInt(meta["maxOutputTokens"])
	if contextLength <= 0 || maxOutput <= 0 {
		fallback, ok := modelsDevLimits[strings.ToLower(modelID)]
		if ok {
			if contextLength <= 0 {
				contextLength = fallback[0]
			}
			if maxOutput <= 0 {
				maxOutput = fallback[1]
			}
		}
	}
	if contextLength <= 0 {
		contextLength = defaultCatalogContext
	}
	return contextLength, maxOutput
}

// ---------------------------------------------------------------------------
// Cache, provenance, and free-model helpers
// ---------------------------------------------------------------------------

func (p *Provider) catalogNowTime() time.Time {
	if p != nil && p.catalogNow != nil {
		return p.catalogNow()
	}
	return time.Now()
}

func (p *Provider) catalogTTL() time.Duration {
	cfg := p.Config()
	if p.pool != nil {
		cfg = p.pool.Config()
	}
	seconds := cfg.ModelsTTLSeconds
	if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		seconds = defaultModelsTTLSeconds
	}
	ttl := time.Duration(seconds * float64(time.Second))
	if ttl > catalogMaxTTL || ttl <= 0 {
		ttl = catalogMaxTTL
	}
	return ttl
}

func (p *Provider) freshCatalog(now time.Time) ([]catalogEntry, bool) {
	ttl := p.catalogTTL()
	p.catalogStateMu.RLock()
	state := p.catalogState
	p.catalogStateMu.RUnlock()
	if len(state.entries) == 0 || state.fetchedAt.IsZero() {
		return nil, false
	}
	if !now.Before(state.fetchedAt.Add(ttl)) {
		return nil, false
	}
	return cloneCatalogEntries(state.entries), true
}

func (p *Provider) snapshotCatalog() catalogSnapshot {
	p.catalogStateMu.RLock()
	defer p.catalogStateMu.RUnlock()
	state := p.catalogState
	state.entries = cloneCatalogEntries(state.entries)
	if state.fileMTime != nil {
		value := *state.fileMTime
		state.fileMTime = &value
	}
	return state
}

func (p *Provider) storeCatalog(entries []catalogEntry, source string, fetchedAt time.Time, fileMTime *time.Time) {
	copied := cloneCatalogEntries(entries)
	var fileCopy *time.Time
	if fileMTime != nil {
		value := *fileMTime
		fileCopy = &value
	}
	p.catalogStateMu.Lock()
	p.catalogState.entries = copied
	p.catalogState.fetchedAt = fetchedAt
	p.catalogState.source = source
	p.catalogState.fileMTime = fileCopy
	p.catalogStateMu.Unlock()
}

func (p *Provider) markRemoteCatalog(now time.Time) {
	p.catalogStateMu.Lock()
	p.catalogState.lastRemote = now
	p.catalogStateMu.Unlock()
}

// FreeModelIDs returns the current catalog's free ("x0.00") model IDs. Before
// any catalogue build it falls back to the bundled snapshot. It performs no
// network I/O.
func (p *Provider) FreeModelIDs() []string {
	if p == nil {
		return nil
	}
	entries := p.snapshotCatalog().entries
	if len(entries) == 0 {
		entries = mergeCatalog(nil, p.realm, false)
	}
	set := make(map[string]bool)
	for _, entry := range entries {
		if creditsIsFree(entry.Meta["credits"]) {
			set[entry.ID] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RequiresPro reports whether this provider realm gates modelID to paid
// accounts. It is the instance form of requiresPro.
func (p *Provider) RequiresPro(modelID string) bool {
	if p == nil {
		return false
	}
	return requiresPro(modelID, p.realm)
}

// TierSnapshot returns a copy of this realm's model-tier map.
func (p *Provider) TierSnapshot() map[string]string {
	if p == nil {
		return map[string]string{}
	}
	return tierSnapshot(p.realm)
}

// TierIngestedAt returns when this realm's tier map was last refreshed.
func (p *Provider) TierIngestedAt() (time.Time, bool) {
	if p == nil {
		return time.Time{}, false
	}
	return ingestedAt(p.realm)
}

// IsFreeModel reports whether the current catalogue marks modelID free. Before
// a catalogue build it falls back to the embedded static metadata.
func (p *Provider) IsFreeModel(modelID string) bool {
	if p == nil {
		return false
	}
	for _, entry := range p.snapshotCatalog().entries {
		if entry.ID == modelID {
			return creditsIsFree(entry.Meta["credits"])
		}
	}
	return isFreeModel(modelID, p.realm)
}

func (p *Provider) applyProOnlyModels() {
	if p == nil || p.pool == nil {
		return
	}
	snapshot := tierSnapshot(p.realm)
	models := make([]string, 0, len(snapshot))
	for model, tier := range snapshot {
		if tier == tierStandard {
			models = append(models, model)
		}
	}
	sort.Strings(models)
	p.pool.SetProOnlyModels(models)
}

// ---------------------------------------------------------------------------
// Small metadata helpers
// ---------------------------------------------------------------------------

func cloneCatalogEntries(entries []catalogEntry) []catalogEntry {
	out := make([]catalogEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, catalogEntry{ID: entry.ID, Meta: cloneMeta(entry.Meta)})
	}
	return out
}

func cloneMeta(meta map[string]any) map[string]any {
	if meta == nil {
		return nil
	}
	out := make(map[string]any, len(meta))
	for key, value := range meta {
		out[key] = cloneJSONValue(value)
	}
	return out
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, entry := range typed {
			out[key] = cloneJSONValue(entry)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, entry := range typed {
			out[i] = cloneJSONValue(entry)
		}
		return out
	case []map[string]any:
		out := make([]any, len(typed))
		for i, entry := range typed {
			out[i] = cloneMeta(entry)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case map[string]string:
		out := make(map[string]any, len(typed))
		for key, entry := range typed {
			out[key] = entry
		}
		return out
	default:
		return value
	}
}

func metaText(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return ""
	}
}

func firstMetaText(meta map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := metaText(meta[key]); value != "" {
			return value
		}
	}
	return ""
}

func metaBool(value any) bool {
	typed, _ := value.(bool)
	return typed
}

func boolValue(value any) (bool, bool) {
	typed, ok := value.(bool)
	return typed, ok
}

func metaInt(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0
		}
		return int64(typed)
	case json.Number:
		number, _ := typed.Int64()
		return number
	case string:
		number, _ := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return number
	default:
		return 0
	}
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

func metaMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[string]string:
		out := make(map[string]any, len(typed))
		for key, entry := range typed {
			out[key] = entry
		}
		return out, true
	default:
		return nil, false
	}
}

func metaStringList(value any) []string {
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			if text, ok := entry.(string); ok {
				text = strings.TrimSpace(text)
				if text != "" {
					out = append(out, text)
				}
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			entry = strings.TrimSpace(entry)
			if entry != "" {
				out = append(out, entry)
			}
		}
		return out
	case string:
		if text := strings.TrimSpace(typed); text != "" {
			return []string{text}
		}
		return nil
	default:
		return nil
	}
}

func metaIntSlice(value any) []int64 {
	switch typed := value.(type) {
	case []any:
		out := make([]int64, 0, len(typed))
		for _, entry := range typed {
			if number := metaInt(entry); number > 0 {
				out = append(out, number)
			}
		}
		return out
	case []int64:
		return append([]int64(nil), typed...)
	case []int:
		out := make([]int64, len(typed))
		for i, entry := range typed {
			out[i] = int64(entry)
		}
		return out
	default:
		return nil
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
