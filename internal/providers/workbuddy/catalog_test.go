package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

type catalogDoerFunc func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error)

func (f catalogDoerFunc) Do(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
	return f(ctx, req, proxyURL)
}

type catalogCall struct {
	Header   http.Header
	ProxyURL string
	Deadline time.Time
}

func catalogHTTPResponse(status int, payload any) *http.Response {
	body, _ := json.Marshal(payload)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func anyStringList(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func catalogPayload(ids []string, meta map[string]map[string]any, tiers []any) map[string]any {
	models := make([]any, 0, len(ids))
	for _, id := range ids {
		item := map[string]any{"id": id}
		for key, value := range meta[id] {
			item[key] = value
		}
		models = append(models, item)
	}
	data := map[string]any{
		"code":   0,
		"agents": []any{map[string]any{"name": "cli", "models": anyStringList(ids)}},
		"models": models,
	}
	if tiers != nil {
		data["modelTiers"] = tiers
	}
	return map[string]any{"code": 0, "data": data}
}

func desktopCatalogPayload(ids []string, meta map[string]map[string]any) map[string]any {
	models := make([]any, 0, len(ids))
	for _, id := range ids {
		item := map[string]any{"id": id}
		for key, value := range meta[id] {
			item[key] = value
		}
		models = append(models, item)
	}
	return map[string]any{
		"agents": []any{map[string]any{"name": "cli", "models": anyStringList(ids)}},
		"models": models,
	}
}

func writeCatalogSettings(t *testing.T, dir string, settings map[string]any) {
	t.Helper()
	if settings == nil {
		settings = map[string]any{}
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

func writeCatalogAccount(t *testing.T, dir string, fields map[string]any) {
	t.Helper()
	uid, _ := fields["uid"].(string)
	if uid == "" {
		t.Fatalf("catalog test account needs a uid")
	}
	if _, ok := fields["accessToken"]; !ok {
		fields["accessToken"] = "token-" + uid
	}
	if _, ok := fields["realm"]; !ok {
		fields["realm"] = realmIntl
	}
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		t.Fatalf("marshal account %s: %v", uid, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create accounts dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, uid+".json"), data, 0o600); err != nil {
		t.Fatalf("write account %s: %v", uid, err)
	}
}

// newCatalogProvider builds a real Provider/Pool pair in a temp directory and
// removes every accidental external fallback. Tests install their own
// catalogDoer and catalogDesktopPath.
func newCatalogProvider(t *testing.T, realm string, settings map[string]any, accounts ...map[string]any) (*Provider, *Pool) {
	t.Helper()
	resetTierState()
	dir := t.TempDir()
	writeCatalogSettings(t, dir, settings)
	for _, fields := range accounts {
		writeCatalogAccount(t, dir, fields)
	}
	provider, err := New(realm, config.New(filepath.Join(dir, "settings.json")))
	if err != nil {
		t.Fatalf("New(%s): %v", realm, err)
	}
	provider.catalogDesktopPath = func(string) string {
		return filepath.Join(dir, "missing-acc-product-config-v3.json")
	}
	provider.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return nil, errors.New("no test catalog doer installed")
	})
	return provider, provider.NewPool()
}

func providerWithAccount(t *testing.T, realm string) (*Provider, *Pool) {
	t.Helper()
	return newCatalogProvider(t, realm, nil, map[string]any{
		"uid":         "uid-" + realm,
		"accessToken": "token-" + realm,
		"realm":       realm,
	})
}

func catalogModelIDs(models []Model) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		if id, _ := model["id"].(string); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func catalogModelByID(t *testing.T, models []Model, id string) Model {
	t.Helper()
	for _, model := range models {
		if got, _ := model["id"].(string); got == id {
			return model
		}
	}
	t.Fatalf("model %q not found in %v", id, catalogModelIDs(models))
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func mapInt64(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		number, _ := typed.Int64()
		return number
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Static extraction and realm separation
// ---------------------------------------------------------------------------

func TestCatalogStaticExtractionRealmSeparation(t *testing.T) {
	intl := staticCatalogForRealm(realmIntl)
	cn := staticCatalogForRealm(realmCN)
	if len(intl) != 23 {
		t.Fatalf("intl static models = %d, want 23", len(intl))
	}
	if len(cn) != 39 {
		t.Fatalf("cn static models = %d, want 39", len(cn))
	}

	intlIDs := staticCatalogIDs(realmIntl)
	cnIDs := staticCatalogIDs(realmCN)
	for _, id := range []string{"default-model", "deepseek-v4.1-flash", "gpt-6-astra", "kimi-k3"} {
		if !containsString(intlIDs, id) {
			t.Fatalf("intl static catalog missing %q", id)
		}
	}
	if containsString(intlIDs, "hy3-x") || containsString(intlIDs, "glm-5.2") {
		t.Fatalf("intl static catalog leaked cn-only ids: %v", intlIDs)
	}
	for _, id := range []string{"hy3", "hy3-x", "hy4-preview-f", "glm-5.2", "deepseek-v4-pro"} {
		if !containsString(cnIDs, id) {
			t.Fatalf("cn static catalog missing %q", id)
		}
	}
	if containsString(cnIDs, "gpt-6-astra") || containsString(cnIDs, "grok-4.7") {
		t.Fatalf("cn static catalog leaked intl-only ids: %v", cnIDs)
	}

	intlMeta, ok := staticMetaForRealm(realmIntl, "deepseek-v4.1-flash")
	if !ok {
		t.Fatal("intl static metadata for deepseek-v4.1-flash is missing")
	}
	if got := mapInt64(intlMeta["maxInputTokens"]); got != 1000000 {
		t.Fatalf("deepseek maxInputTokens = %d, want 1000000", got)
	}
	if got := mapInt64(intlMeta["maxOutputTokens"]); got != 128000 {
		t.Fatalf("deepseek maxOutputTokens = %d, want 128000", got)
	}
	if credits, _ := intlMeta["credits"].(string); credits != "x0.11" {
		t.Fatalf("deepseek credits = %q, want x0.11", credits)
	}
	if !creditsIsFree(intlMetaFor(t, realmCN, "hy3")["credits"]) {
		t.Fatal("cn hy3 should be a free model in the static catalog")
	}
}

func intlMetaFor(t *testing.T, realm, id string) map[string]any {
	t.Helper()
	meta, ok := staticMetaForRealm(realm, id)
	if !ok {
		t.Fatalf("static metadata for %s/%s is missing", realm, id)
	}
	return meta
}

// ---------------------------------------------------------------------------
// Remote parsing, curation, and merging
// ---------------------------------------------------------------------------

func TestCatalogParseRemoteOrderMetaAndShapes(t *testing.T) {
	ids := []string{"default-model", "hy4-preview", "hy3", "grok-4.7"}
	meta := map[string]map[string]any{
		"hy4-preview": {"credits": "x0.29"},
		"grok-4.7":    {"credits": "x1.90", "maxInputTokens": float64(200000)},
	}
	gotIDs, gotMeta, ok := parseRemoteCatalog(catalogPayload(ids, meta, nil))
	if !ok {
		t.Fatal("parseRemoteCatalog rejected a valid payload")
	}
	if strings.Join(gotIDs, ",") != strings.Join(ids, ",") {
		t.Fatalf("ids = %v, want %v", gotIDs, ids)
	}
	if gotMeta["grok-4.7"]["credits"] != "x1.90" {
		t.Fatalf("grok credits = %v, want x1.90", gotMeta["grok-4.7"]["credits"])
	}

	for name, payload := range map[string]map[string]any{
		"empty":            {},
		"empty agents":     {"data": map[string]any{"agents": []any{}}},
		"empty model list": {"data": map[string]any{"agents": []any{map[string]any{"models": []any{}}}}},
	} {
		if _, _, ok := parseRemoteCatalog(payload); ok {
			t.Fatalf("%s: parseRemoteCatalog unexpectedly succeeded", name)
		}
	}
	if ids, _, ok := parseRemoteCatalog(nil); ok || ids != nil {
		t.Fatalf("nil payload should be rejected, got %v", ids)
	}

	mapAgents := map[string]any{"data": map[string]any{
		"agents": map[string]any{"cli": map[string]any{"models": []any{"a", "b"}}},
	}}
	gotIDs, _, ok = parseRemoteCatalog(mapAgents)
	if !ok || strings.Join(gotIDs, ",") != "a,b" {
		t.Fatalf("map agents parse = %v/%v, want a,b/true", gotIDs, ok)
	}

	desktop := map[string]any{
		"agents": []any{
			map[string]any{"name": "compact", "models": []any{"lite"}},
			map[string]any{"name": "cli", "models": []any{"a", "b"}},
		},
	}
	gotIDs, _, ok = parseRemoteCatalog(desktop)
	if !ok || strings.Join(gotIDs, ",") != "a,b" {
		t.Fatalf("desktop cache parse = %v/%v, want a,b/true", gotIDs, ok)
	}
}

func TestCatalogCurateRemoteVariantsAndFreeSiblings(t *testing.T) {
	ids := []string{
		"default-model", "fast-model", "balanced-model", "primary-model",
		"deep-model", "hy4-preview", "hy4-preview-f", "hy3", "hy3-x",
		"deepseek-v4.1-flash-sg", "grok-4.7", "glm-5.3",
	}
	meta := map[string]map[string]any{
		"hy4-preview":            {"credits": "x0.29"},
		"hy4-preview-f":          {"credits": "x0.00"},
		"hy3":                    {"credits": "x0.00"},
		"hy3-x":                  {"credits": "x0.05"},
		"deepseek-v4.1-flash-sg": {"credits": "x0.11"},
		"grok-4.7":               {"credits": "x1.90"},
		"glm-5.3":                {"credits": "x0.69"},
	}
	got := curateRemoteCatalog(realmIntl, ids, meta)
	want := []string{"hy4-preview-f", "hy3", "deepseek-v4.1-flash-sg", "grok-4.7", "glm-5.3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("curated = %v, want %v", got, want)
	}

	// A -sg build stays when the paid plain name is no longer listed.
	got = curateRemoteCatalog(realmIntl, []string{"deepseek-v4.1-flash-sg"}, map[string]map[string]any{
		"deepseek-v4.1-flash-sg": {"credits": "x0.11"},
	})
	if strings.Join(got, ",") != "deepseek-v4.1-flash-sg" {
		t.Fatalf("standalone -sg = %v", got)
	}

	// Free sibling rules also work in the other direction (-f first).
	got = curateRemoteCatalog(realmCN, []string{
		"hy4-preview", "hy4-preview-dev", "glm-5.3", "glm-5.3-f", "hy3",
	}, map[string]map[string]any{
		"hy4-preview":     {"credits": "x0.29"},
		"hy4-preview-dev": {"credits": "x0.29"},
		"glm-5.3":         {"credits": "x0.79"},
		"glm-5.3-f":       {"credits": "x0.00"},
		"hy3":             {"credits": "x0.00"},
	})
	want = []string{"hy4-preview", "hy4-preview-dev", "glm-5.3-f", "hy3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("cn curated = %v, want %v", got, want)
	}

	// Non-chat/media models never reach the advertised catalogue.
	got = curateRemoteCatalog(realmIntl, []string{"lite", "codewise-x", "completion-y", "hy3", "hy-image-3.5"}, nil)
	if strings.Join(got, ",") != "hy3" {
		t.Fatalf("non-chat filtered = %v, want hy3", got)
	}
}

func TestCatalogMergeRealmIsolationAndReasoning(t *testing.T) {
	remote := []catalogEntry{
		{ID: "remote-only", Meta: map[string]any{"credits": "x1.00"}},
		{ID: "deepseek-v4.1-flash", Meta: map[string]any{
			"credits":   "x0.00",
			"reasoning": map[string]any{"effort": "high", "summary": "auto"},
		}},
	}
	intl := mergeCatalog(remote, realmIntl, true)
	cn := mergeCatalog(remote, realmCN, true)
	intlIDs := catalogEntryIDs(intl)
	cnIDs := catalogEntryIDs(cn)
	if !containsString(intlIDs, "remote-only") {
		t.Fatalf("intl live-only model missing: %v", intlIDs)
	}
	if containsString(intlIDs, "glm-5.2") {
		t.Fatalf("intl merge leaked cn static glm-5.2: %v", intlIDs)
	}
	if !containsString(cnIDs, "glm-5.2") || !containsString(cnIDs, "deepseek-v4-pro") {
		t.Fatalf("cn merge lost cn models: %v", cnIDs)
	}
	if containsString(cnIDs, "gpt-6-astra") || containsString(cnIDs, "grok-4.7") {
		t.Fatalf("cn merge leaked intl models: %v", cnIDs)
	}
	if !containsString(cnIDs, "remote-only") {
		// extras is true, so a live-only model is allowed on either realm, but
		// it must come from the live payload, never from the other static table.
		t.Fatalf("cn merge lost the live-only model: %v", cnIDs)
	}

	// Known-paid price survives a placeholder x0.00 from the live catalogue.
	entries := mergeCatalog([]catalogEntry{{ID: "deepseek-v4.1-flash", Meta: map[string]any{
		"credits": "x0.00",
	}}}, realmIntl, true)
	for _, entry := range entries {
		if entry.ID == "deepseek-v4.1-flash" {
			if got := metaText(entry.Meta["credits"]); got != "x0.11" {
				t.Fatalf("credits = %q, want bundled x0.11", got)
			}
			reasoning, _ := entry.Meta["reasoning"].(map[string]any)
			if efforts, _ := reasoning["supportedEfforts"].([]any); len(efforts) != 3 {
				t.Fatalf("reasoning efforts were not preserved: %#v", reasoning)
			}
			if _, pinned := reasoning["effort"]; pinned {
				t.Fatalf("a selectable model must not stay pinned to effort: %#v", reasoning)
			}
			if got, _ := reasoning["defaultEffort"].(string); got != "high" {
				t.Fatalf("defaultEffort = %q, want high", got)
			}
		}
	}
}

func catalogEntryIDs(entries []catalogEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.ID)
	}
	return out
}

// ---------------------------------------------------------------------------
// Models: live, headers, capabilities, caching, fallbacks, refresh
// ---------------------------------------------------------------------------

func TestCatalogModelsLiveHeadersWireShapeAndTiers(t *testing.T) {
	provider, pool := providerWithAccount(t, realmIntl)
	ids := []string{"deepseek-v4.1-flash", "hy3", "glm-5.3-flash", "grok-4.7", "kimi-k3"}
	meta := map[string]map[string]any{
		"deepseek-v4.1-flash": {
			"credits": "x0.11",
			"reasoning": map[string]any{
				"supportedEfforts": []any{"low", "high", "max"},
				"defaultEffort":    "high",
			},
			"relatedModels": map[string]any{"lite": "deepseek-v4.1-flash", "reasoning": "deepseek-v4.1-flash"},
		},
		"hy3":           {"credits": "x0.00"},
		"glm-5.3-flash": {"credits": "x0.08", "supportsImages": true},
		"grok-4.7":      {"credits": "x1.90", "creditsPeak": "x2.10", "creditsOffpeak": "x1.70", "maxInputTokens": float64(200000), "maxOutputTokens": float64(50000)},
		"kimi-k3":       {"credits": "x1.62"},
	}
	tiers := []any{
		map[string]any{"requiredUserType": "standard", "modelIds": []any{"glm-5.3"}},
		map[string]any{"requiredUserType": "personal_free_user", "modelIds": []any{"hy3"}},
	}
	payload := catalogPayload(ids, meta, tiers)

	var mu sync.Mutex
	calls := make([]catalogCall, 0, 1)
	provider.catalogDoer = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
		mu.Lock()
		calls = append(calls, catalogCall{Header: req.Header.Clone(), ProxyURL: proxyURL})
		mu.Unlock()
		return catalogHTTPResponse(http.StatusOK, payload), nil
	})
	provider.catalogNow = func() time.Time { return time.Unix(1_700_000_000, 0) }

	models, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if got, want := len(models), len(intlUIOrder); got != want {
		t.Fatalf("model count = %d, want %d (%v)", got, want, catalogModelIDs(models))
	}
	if got := catalogModelIDs(models); strings.Join(got, ",") != strings.Join(intlUIOrder, ",") {
		t.Fatalf("model order = %v, want %v", got, intlUIOrder)
	}

	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(calls))
	}
	header := calls[0].Header
	if got := header.Get("Authorization"); got != "Bearer token-intl" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := header.Get("X-User-Id"); got != "uid-intl" {
		t.Fatalf("X-User-Id = %q", got)
	}
	if got := header.Get("Accept"); !strings.Contains(got, "application/json") {
		t.Fatalf("Accept = %q", got)
	}
	if got := header.Get("Origin"); got != "https://www.workbuddy.ai" {
		t.Fatalf("Origin = %q", got)
	}
	if got := header.Get("Referer"); got != "https://www.workbuddy.ai/" {
		t.Fatalf("Referer = %q", got)
	}
	if got := header.Get("User-Agent"); !strings.Contains(got, "workbuddy-ai/5.8.4") {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := header.Get("X-API-Key"); got != "" {
		t.Fatalf("OAuth account must not send X-API-Key: %q", got)
	}
	if calls[0].ProxyURL != "" {
		t.Fatalf("account without a proxy used proxy %q", calls[0].ProxyURL)
	}

	deepseek := catalogModelByID(t, models, "deepseek-v4.1-flash")
	if deepseek["object"] != "model" || deepseek["owned_by"] != "workbuddy" || deepseek["provider"] != "workbuddy" {
		t.Fatalf("wire identity fields = %#v", deepseek)
	}
	if got := mapInt64(deepseek["created"]); got != 1_700_000_000 {
		t.Fatalf("created = %d", got)
	}
	if got := mapInt64(deepseek["context_length"]); got != 1_000_000 {
		t.Fatalf("context_length = %d", got)
	}
	if got := mapInt64(deepseek["max_input_tokens"]); got != 1_000_000 {
		t.Fatalf("max_input_tokens = %d", got)
	}
	if got := mapInt64(deepseek["max_output_tokens"]); got != 128_000 {
		t.Fatalf("max_output_tokens = %d", got)
	}
	if got := mapInt64(deepseek["max_completion_tokens"]); got != 128_000 {
		t.Fatalf("max_completion_tokens = %d", got)
	}
	if got := deepseek["credits"]; got != "x0.11" {
		t.Fatalf("credits = %v", got)
	}
	if got, _ := deepseek["free"].(bool); got {
		t.Fatalf("deepseek-v4.1-flash should not be free")
	}
	caps, _ := deepseek["capabilities"].(map[string]any)
	if caps["vision"] != true || caps["tool_calls"] != true || caps["reasoning"] != true {
		t.Fatalf("capabilities = %#v", caps)
	}
	if efforts, _ := deepseek["reasoning_efforts"].([]string); len(efforts) != 3 {
		t.Fatalf("reasoning_efforts = %#v", deepseek["reasoning_efforts"])
	}
	if got := deepseek["reasoning_default_effort"]; got != "high" {
		t.Fatalf("reasoning_default_effort = %v", got)
	}
	if got, _ := deepseek["category"].(string); got != "chat" {
		t.Fatalf("category = %q, want chat", got)
	}

	hy3 := catalogModelByID(t, models, "hy3")
	if got, _ := hy3["free"].(bool); !got {
		t.Fatal("hy3 should be marked free")
	}
	glm := catalogModelByID(t, models, "glm-5.3-flash")
	glmCaps, _ := glm["capabilities"].(map[string]any)
	if glmCaps["vision"] != false {
		t.Fatalf("glm-5.3-flash vision = %v, want false", glmCaps["vision"])
	}
	if glm["vision_note"] == nil {
		t.Fatal("glm-5.3-flash should carry the vision correction note")
	}
	grok := catalogModelByID(t, models, "grok-4.7")
	if grok["credits_peak"] != "x2.10" || grok["credits_offpeak"] != "x1.70" {
		t.Fatalf("peak/offpeak credits missing: %#v", grok)
	}
	if got := mapInt64(grok["context_length"]); got != 200_000 {
		t.Fatalf("live context limit = %d, want 200000", got)
	}
	if got := mapInt64(grok["max_output_tokens"]); got != 50_000 {
		t.Fatalf("live output limit = %d, want 50000", got)
	}
	kimi := catalogModelByID(t, models, "kimi-k3")
	aliases, _ := kimi["aliases"].([]string)
	if !containsString(aliases, "kimi-k3-1") {
		t.Fatalf("kimi-k3 aliases = %#v, want kimi-k3-1", kimi["aliases"])
	}

	if !requiresPro("glm-5.3", realmIntl) {
		t.Fatal("modelTiers did not mark glm-5.3 Pro-only")
	}
	if requiresPro("hy3", realmIntl) {
		t.Fatal("free tier should not require Pro")
	}
	pool.mu.RLock()
	proOnly := pool.proOnly["glm-5.3"]
	hy3Gated := pool.proOnly["hy3"]
	pool.mu.RUnlock()
	if !proOnly || hy3Gated {
		t.Fatalf("pool proOnly map not wired: glm-5.3=%v hy3=%v", proOnly, hy3Gated)
	}
	if at, ok := ingestedAt(realmIntl); !ok || at.IsZero() {
		t.Fatalf("ingestedAt = %v/%v", at, ok)
	}
	if !provider.RequiresPro("glm-5.3") || provider.RequiresPro("hy3") {
		t.Fatal("Provider.RequiresPro does not match the realm tier map")
	}
	if !provider.IsFreeModel("hy3") || provider.IsFreeModel("glm-5.3") {
		t.Fatal("Provider.IsFreeModel does not match the current catalogue")
	}
	accounts := pool.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("pool accounts = %d, want 1", len(accounts))
	}
	accounts[0].mu.RLock()
	accountModels := catalogModelIDs(accounts[0].Models)
	accounts[0].mu.RUnlock()
	if strings.Join(accountModels, ",") != strings.Join(intlUIOrder, ",") {
		t.Fatalf("representative account catalogue = %v", accountModels)
	}

	proGlm := catalogModelByID(t, models, "glm-5.3")
	entitlement, _ := proGlm["entitlement"].(map[string]any)
	if entitlement == nil || entitlement["source"] != "catalog" || entitlement["required"] != "standard" {
		t.Fatalf("entitlement = %#v", proGlm["entitlement"])
	}

	metaWire, ok := provider.CatalogMeta()
	if !ok || metaWire.Source != "remote" || metaWire.FetchedAt != 1_700_000_000 {
		t.Fatalf("CatalogMeta = %#v/%v", metaWire, ok)
	}
}

func TestCatalogAPIKeyHeadersProxyAndCNOrigin(t *testing.T) {
	settings := map[string]any{
		"proxy_slots": []any{
			map[string]any{"id": "slot-1", "url": "http://127.0.0.1:18080", "enabled": true},
		},
	}
	provider, _ := newCatalogProvider(t, realmCN, settings, map[string]any{
		"uid":         "apikey-secret",
		"accessToken": "ck_secret.value",
		"realm":       realmCN,
		"proxySlot":   "slot-1",
	})
	var mu sync.Mutex
	var call catalogCall
	provider.catalogDoer = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
		mu.Lock()
		call = catalogCall{Header: req.Header.Clone(), ProxyURL: proxyURL}
		mu.Unlock()
		return catalogHTTPResponse(http.StatusOK, catalogPayload([]string{"glm-5.3"}, map[string]map[string]any{
			"glm-5.3": {"credits": "x0.79"},
		}, nil)), nil
	})
	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if got := call.Header.Get("X-API-Key"); got != "ck_secret.value" {
		t.Fatalf("X-API-Key = %q", got)
	}
	if got := call.Header.Get("Authorization"); got != "Bearer ck_secret.value" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := call.Header.Get("X-User-Id"); got != "apikey-secret" {
		t.Fatalf("X-User-Id = %q", got)
	}
	if got := call.Header.Get("Origin"); got != "https://www.codebuddy.cn" {
		t.Fatalf("CN Origin = %q", got)
	}
	if got := call.Header.Get("User-Agent"); !strings.Contains(got, "WorkBuddy/5.8.3") {
		t.Fatalf("CN User-Agent = %q", got)
	}
	if call.ProxyURL != "http://127.0.0.1:18080" {
		t.Fatalf("proxy = %q, want slot-1 URL", call.ProxyURL)
	}
}

func TestCatalogCNEndpointOverride(t *testing.T) {
	t.Setenv("WB_CN_CHAT_UPSTREAM", "https://catalog-proxy.example.test/base/")
	provider, _ := providerWithAccount(t, realmCN)
	var gotURL string
	provider.catalogDoer = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
		gotURL = req.URL.String()
		return catalogHTTPResponse(http.StatusOK, catalogPayload([]string{"glm-5.3"}, map[string]map[string]any{
			"glm-5.3": {"credits": "x0.79"},
		}, nil)), nil
	})
	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotURL != "https://catalog-proxy.example.test/base/v3/config" {
		t.Fatalf("CN override catalog URL = %q", gotURL)
	}
}

func TestCatalogSecondUAAndTenSecondBudget(t *testing.T) {
	provider, _ := providerWithAccount(t, realmIntl)
	var mu sync.Mutex
	calls := make([]catalogCall, 0, 2)
	provider.catalogDoer = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
		deadline, _ := ctx.Deadline()
		mu.Lock()
		calls = append(calls, catalogCall{
			Header:   req.Header.Clone(),
			ProxyURL: proxyURL,
			Deadline: deadline,
		})
		index := len(calls)
		mu.Unlock()
		if index == 1 {
			return catalogHTTPResponse(http.StatusOK, map[string]any{"code": 0}), nil
		}
		return catalogHTTPResponse(http.StatusOK, catalogPayload([]string{"hy3"}, map[string]map[string]any{
			"hy3": {"credits": "x0.00"},
		}, nil)), nil
	})
	now := time.Now()
	provider.catalogNow = func() time.Time { return now }
	models, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("Models returned no entries")
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (second UA fallback)", len(calls))
	}
	firstUA := calls[0].Header.Get("User-Agent")
	secondUA := calls[1].Header.Get("User-Agent")
	if firstUA == "" || secondUA == "" || firstUA == secondUA {
		t.Fatalf("UAs = %q / %q, want two distinct desktop UAs", firstUA, secondUA)
	}
	if !strings.Contains(secondUA, "WorkBuddy/") {
		t.Fatalf("second UA = %q", secondUA)
	}
	remaining := time.Until(calls[0].Deadline)
	if remaining <= 8*time.Second || remaining > 10*time.Second+500*time.Millisecond {
		t.Fatalf("catalog budget = %v, want about 10s", remaining)
	}
}

func TestCatalogCacheTTLForceLastGoodAndCooldown(t *testing.T) {
	provider, pool := providerWithAccount(t, realmIntl)
	now := time.Unix(1_700_000_000, 0)
	provider.catalogNow = func() time.Time { return now }
	var mu sync.Mutex
	calls := 0
	fail := false
	provider.catalogDoer = catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
		mu.Lock()
		calls++
		shouldFail := fail
		mu.Unlock()
		if shouldFail {
			return nil, errors.New("upstream unavailable for token-intl")
		}
		return catalogHTTPResponse(http.StatusOK, catalogPayload([]string{"hy3"}, map[string]map[string]any{
			"hy3": {"credits": "x0.00"},
		}, nil)), nil
	})

	first, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("first Models: %v", err)
	}
	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("cached Models: %v", err)
	}
	if calls != 1 {
		t.Fatalf("cache miss count = %d, want 1", calls)
	}

	now = now.Add(299 * time.Second)
	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("Models at 299s: %v", err)
	}
	if calls != 1 {
		t.Fatalf("300s cache refreshed too early: calls=%d", calls)
	}

	now = now.Add(2 * time.Second)
	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("Models after TTL: %v", err)
	}
	if calls != 2 {
		t.Fatalf("stale cache did not refresh: calls=%d", calls)
	}

	if _, err := provider.Models(context.Background(), true); err != nil {
		t.Fatalf("force Models: %v", err)
	}
	if calls != 3 {
		t.Fatalf("force did not bypass the cache: calls=%d", calls)
	}

	// A configured TTL longer than 300s is capped at 300 seconds.
	cfg := pool.Config()
	cfg.ModelsTTLSeconds = 86400
	pool.SetConfig(cfg)
	now = now.Add(299 * time.Second)
	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("Models before capped TTL: %v", err)
	}
	if calls != 3 {
		t.Fatalf("configured TTL bypassed the 300s cap: calls=%d", calls)
	}
	now = now.Add(2 * time.Second)
	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("Models after capped TTL: %v", err)
	}
	if calls != 4 {
		t.Fatalf("cap did not expire at 300s: calls=%d", calls)
	}

	// Failure retains the last live catalogue ahead of an older desktop cache
	// file (an outage must not regress a fresh remote result).
	cachePath := filepath.Join(t.TempDir(), "desktop.json")
	cachePayload := desktopCatalogPayload([]string{"glm-5.3"}, map[string]map[string]any{
		"glm-5.3": {"credits": "x9.99"},
	})
	cacheData, _ := json.Marshal(cachePayload)
	if err := os.WriteFile(cachePath, cacheData, 0o600); err != nil {
		t.Fatalf("write desktop cache: %v", err)
	}
	provider.catalogDesktopPath = func(string) string { return cachePath }
	mu.Lock()
	fail = true
	mu.Unlock()
	stale, err := provider.Models(context.Background(), true)
	if err != nil {
		t.Fatalf("Models force failure returned an error: %v", err)
	}
	if len(stale) != len(first) {
		t.Fatalf("last-good entries = %d, want %d", len(stale), len(first))
	}
	if got := catalogModelIDs(stale); strings.Join(got, ",") != strings.Join(intlUIOrder, ",") {
		t.Fatalf("last-good ids changed: %v", got)
	}
	if got := catalogModelByID(t, stale, "glm-5.3")["credits"]; got != "x0.69" {
		t.Fatalf("last-good credits = %v, want bundled x0.69 (desktop cache regressed it)", got)
	}

	// Manual refresh cooldown is per provider and covers failed attempts.
	res, err := provider.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}
	if res.OK {
		t.Fatalf("RefreshCatalog unexpectedly succeeded with a failing upstream: %#v", res)
	}
	if !strings.Contains(res.Msg, "保留") {
		t.Fatalf("failure message = %q, want retention notice", res.Msg)
	}
	before := calls
	res, err = provider.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatalf("RefreshCatalog cooldown: %v", err)
	}
	if res.OK || !res.Cooldown || res.RetryAfter <= 0 {
		t.Fatalf("cooldown result = %#v", res)
	}
	if calls != before {
		t.Fatalf("cooldown still contacted upstream: calls %d -> %d", before, calls)
	}

	now = now.Add(31 * time.Second)
	mu.Lock()
	fail = false
	mu.Unlock()
	res, err = provider.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatalf("RefreshCatalog after cooldown: %v", err)
	}
	if !res.OK || res.Source != "remote" || res.Count != len(first) {
		t.Fatalf("manual refresh result = %#v", res)
	}
	if got, ok := provider.CatalogMeta(); !ok || got.Source != "remote" {
		t.Fatalf("CatalogMeta after manual refresh = %#v/%v", got, ok)
	}
}

func TestCatalogDesktopCacheAndBundledFallback(t *testing.T) {
	provider, _ := providerWithAccount(t, realmCN)
	provider.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return nil, errors.New("offline")
	})
	cachePath := filepath.Join(t.TempDir(), "acc-product-config-v3.json")
	payload := desktopCatalogPayload([]string{"glm-5.3", "grok-4.7", "hy4-preview-f"}, map[string]map[string]any{
		"glm-5.3":       {"credits": "x0.99"},
		"grok-4.7":      {"credits": "x1.90"},
		"hy4-preview-f": {"credits": "x0.00"},
	})
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatalf("write desktop cache: %v", err)
	}
	fileTime := time.Unix(1_700_000_100, 0)
	if err := os.Chtimes(cachePath, fileTime, fileTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	provider.catalogDesktopPath = func(string) string { return cachePath }
	provider.catalogNow = func() time.Time { return time.Unix(1_700_000_200, 0) }

	models, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models cache fallback: %v", err)
	}
	ids := catalogModelIDs(models)
	if !containsString(ids, "grok-4.7") {
		t.Fatalf("desktop cache live-only model missing: %v", ids)
	}
	glm := catalogModelByID(t, models, "glm-5.3")
	if glm["credits"] != "x0.99" {
		t.Fatalf("desktop cache credits = %v, want x0.99", glm["credits"])
	}
	meta, ok := provider.CatalogMeta()
	if !ok || meta.Source != "cache" || meta.FileMTime == nil {
		t.Fatalf("cache CatalogMeta = %#v/%v", meta, ok)
	}
	if *meta.FileMTime != float64(fileTime.Unix()) {
		t.Fatalf("file_mtime = %v, want %v", *meta.FileMTime, fileTime.Unix())
	}

	// With no remote and no desktop file the embedded snapshot still serves the
	// full realm catalogue (and never the other realm's).
	provider2, _ := providerWithAccount(t, realmCN)
	provider2.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return nil, errors.New("offline")
	})
	provider2.catalogDesktopPath = func(string) string { return filepath.Join(t.TempDir(), "none.json") }
	models, err = provider2.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models bundled fallback: %v", err)
	}
	ids = catalogModelIDs(models)
	if len(ids) != len(cnUIOrder) {
		t.Fatalf("bundled cn ids = %d, want %d (%v)", len(ids), len(cnUIOrder), ids)
	}
	if !containsString(ids, "glm-5.3") || containsString(ids, "gpt-6-astra") {
		t.Fatalf("bundled cn realm isolation broken: %v", ids)
	}
	meta, ok = provider2.CatalogMeta()
	if !ok || meta.Source != "bundled" || meta.FileMTime != nil {
		t.Fatalf("bundled CatalogMeta = %#v/%v", meta, ok)
	}
}

func TestCatalogLegacyDesktopFallback(t *testing.T) {
	provider, _ := providerWithAccount(t, realmCN)
	provider.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return nil, errors.New("offline")
	})
	cachePath := filepath.Join(t.TempDir(), "legacy.json")
	payload := map[string]any{"models": []any{
		map[string]any{"id": "glm-5.3", "credits": "x0.87"},
		map[string]any{"id": "hy3", "credits": "x0.00"},
	}}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatalf("write legacy cache: %v", err)
	}
	provider.catalogDesktopPath = func(string) string { return cachePath }
	models, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models legacy fallback: %v", err)
	}
	glm := catalogModelByID(t, models, "glm-5.3")
	if glm["credits"] != "x0.87" {
		t.Fatalf("legacy credits = %v", glm["credits"])
	}
	if meta, ok := provider.CatalogMeta(); !ok || meta.Source != "legacy" {
		t.Fatalf("legacy CatalogMeta = %#v/%v", meta, ok)
	}
}

func TestCatalogFreeAliasesCapabilitiesAndModelsDevLimits(t *testing.T) {
	if !creditsIsFree("x0.00") || !creditsIsFree("x0") || !creditsIsFree("0.00") {
		t.Fatal("free credits aliases were not recognised")
	}
	if creditsIsFree("x0.01") || creditsIsFree("") {
		t.Fatal("paid/unknown credits were treated as free")
	}

	provider, _ := providerWithAccount(t, realmIntl)
	provider.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return nil, errors.New("offline")
	})
	models, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}

	kimi := catalogModelByID(t, models, "kimi-k3")
	if got := mapInt64(kimi["context_length"]); got != 1_000_000 {
		t.Fatalf("kimi context = %d", got)
	}
	if got := mapInt64(kimi["max_output_tokens"]); got != 32_000 {
		t.Fatalf("kimi output = %d", got)
	}
	if aliases, _ := kimi["aliases"].([]string); !containsString(aliases, "kimi-k3-1") {
		t.Fatalf("kimi aliases = %#v", kimi["aliases"])
	}
	hy3 := catalogModelByID(t, models, "hy3")
	if free, _ := hy3["free"].(bool); !free {
		t.Fatal("hy3 should be free")
	}
	freeIDs := provider.FreeModelIDs()
	if !containsString(freeIDs, "hy3") || containsString(freeIDs, "deepseek-v4.1-flash") {
		t.Fatalf("FreeModelIDs = %v", freeIDs)
	}
	caps := capabilitiesFor("glm-5.3-flash", realmIntl)
	if caps["vision"] != false {
		t.Fatalf("glm-5.3-flash capabilities = %#v", caps)
	}
	caps = capabilitiesFor("kimi-k3", realmIntl)
	if caps["vision"] != true || caps["reasoning"] != true {
		t.Fatalf("kimi-k3 capabilities = %#v", caps)
	}

	// A live-only model absent from the static tables uses the bundled
	// models.dev knowledge limits; a completely unknown model still gets the
	// safe 1M context default and omits the unknown output limit.
	provider2, _ := providerWithAccount(t, realmIntl)
	provider2.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return catalogHTTPResponse(http.StatusOK, catalogPayload(
			[]string{"kimi-k2.8-preview", "unknown-model-xyz"},
			map[string]map[string]any{
				"kimi-k2.8-preview": {"credits": "x0.77"},
				"unknown-model-xyz": {"credits": "x1.00"},
			}, nil)), nil
	})
	models, err = provider2.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models live-only: %v", err)
	}
	kimi28 := catalogModelByID(t, models, "kimi-k2.8-preview")
	if got := mapInt64(kimi28["context_length"]); got != 1_048_576 {
		t.Fatalf("models.dev context = %d, want 1048576", got)
	}
	if _, present := kimi28["max_output_tokens"]; present {
		t.Fatalf("kimi-k2.8-preview has no known output limit, got %v", kimi28["max_output_tokens"])
	}
	unknown := catalogModelByID(t, models, "unknown-model-xyz")
	if got := mapInt64(unknown["context_length"]); got != 1_000_000 {
		t.Fatalf("unknown context = %d, want 1M default", got)
	}
	if _, present := unknown["max_output_tokens"]; present {
		t.Fatal("unknown output limit should be omitted")
	}
}

func TestCatalogAgentPickerUsesCurrentRealmAndPreservesIDs(t *testing.T) {
	provider, _ := providerWithAccount(t, realmIntl)
	provider.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return catalogHTTPResponse(http.StatusOK, catalogPayload(
			[]string{"X-Case-Model"},
			map[string]map[string]any{"X-Case-Model": {"credits": "x1.00"}}, nil)), nil
	})
	picker, err := provider.AgentPickerModels(context.Background(), false)
	if err != nil {
		t.Fatalf("AgentPickerModels: %v", err)
	}
	ids := catalogModelIDs(picker)
	if !containsString(ids, "X-Case-Model") {
		t.Fatalf("picker lost the live-only model's spelling: %v", ids)
	}
	if containsString(ids, "glm-5.2") || containsString(ids, "hy3-x") {
		t.Fatalf("picker leaked the other realm's curated table: %v", ids)
	}
	if !containsString(ids, "gpt-6.1-sol") {
		t.Fatalf("picker omitted a current-realm bundled model: %v", ids)
	}
	// Every picker entry is a real model entry, never a virtual alias.
	for _, id := range []string{"default-model", "fast-model", "balanced-model", "primary-model", "deep-model", "auto"} {
		if containsString(ids, id) {
			t.Fatalf("picker advertised virtual alias %q: %v", id, ids)
		}
	}
}

func TestCatalogConcurrentColdReadsShareOneFetch(t *testing.T) {
	provider, _ := providerWithAccount(t, realmIntl)
	var mu sync.Mutex
	calls := 0
	provider.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return catalogHTTPResponse(http.StatusOK, catalogPayload([]string{"hy3"}, map[string]map[string]any{
			"hy3": {"credits": "x0.00"},
		}, nil)), nil
	})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := provider.Models(context.Background(), false); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Models: %v", err)
	}
	if calls != 1 {
		t.Fatalf("concurrent cold reads fetched %d times, want 1", calls)
	}
}

func TestCatalogErrorsRedactCredentials(t *testing.T) {
	provider, _ := providerWithAccount(t, realmIntl)
	provider.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		return nil, errors.New("dial failed with token-intl and refresh-secret")
	})
	res, err := provider.RefreshCatalog(context.Background())
	if err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}
	if res.OK {
		t.Fatal("RefreshCatalog unexpectedly succeeded")
	}
	for _, secret := range []string{"token-intl", "refresh-secret"} {
		if strings.Contains(res.Msg, secret) {
			t.Fatalf("refresh message leaked %q: %q", secret, res.Msg)
		}
	}
}

func TestCatalogDailyRefreshHook(t *testing.T) {
	provider, _ := providerWithAccount(t, realmIntl)
	now := time.Unix(1_700_000_000, 0)
	provider.catalogNow = func() time.Time { return now }
	var mu sync.Mutex
	calls := 0
	provider.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return catalogHTTPResponse(http.StatusOK, catalogPayload([]string{"hy3"}, map[string]map[string]any{
			"hy3": {"credits": "x0.00"},
		}, nil)), nil
	})
	if !provider.CatalogDailyDue(now) {
		t.Fatal("a catalog that was never fetched should be due")
	}
	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if provider.CatalogDailyDue(now) {
		t.Fatal("a successful remote fetch should clear the daily due flag")
	}
	now = now.Add(catalogDailyInterval)
	if !provider.CatalogDailyDue(now) {
		t.Fatal("the catalog should be due again after 24h")
	}
	res, done, err := provider.CatalogDailyRefresh(context.Background())
	if err != nil {
		t.Fatalf("CatalogDailyRefresh: %v", err)
	}
	if !done || !res.OK {
		t.Fatalf("CatalogDailyRefresh = %#v done=%v", res, done)
	}
	if calls != 2 {
		t.Fatalf("daily refresh calls = %d, want 2", calls)
	}
	if provider.CatalogDailyDue(now) {
		t.Fatal("daily refresh did not mark the new success time")
	}
}

func TestCatalogDailyRefreshSkipsMissingAccount(t *testing.T) {
	provider, _ := newCatalogProvider(t, realmIntl, nil)
	now := time.Unix(1_700_000_000, 0)
	provider.catalogNow = func() time.Time { return now }
	calls := 0
	provider.catalogDoer = catalogDoerFunc(func(context.Context, *http.Request, string) (*http.Response, error) {
		calls++
		return nil, errors.New("must not be called")
	})
	if !provider.CatalogDailyDue(now) {
		t.Fatal("empty pool catalog should be due")
	}
	result, done, err := provider.CatalogDailyRefresh(context.Background())
	if err != nil {
		t.Fatalf("CatalogDailyRefresh: %v", err)
	}
	if done || result.OK || calls != 0 {
		t.Fatalf("missing-account daily pass = %#v done=%v calls=%d", result, done, calls)
	}
}

// ---------------------------------------------------------------------------
// Tiers
// ---------------------------------------------------------------------------

func TestTiersIngestSnapshotAndRequiresPro(t *testing.T) {
	resetTierState()
	if requiresPro("unknown", realmIntl) {
		t.Fatal("unknown model without a tier map must not be Pro-only")
	}
	payload := map[string]any{"modelTiers": []any{
		map[string]any{"requiredUserType": "personal_free_user", "modelIds": []any{"free-model"}},
		map[string]any{"requiredUserType": "standard", "modelIds": []any{"pro-model", " shared-model "}},
		map[string]any{"requiredUserType": "standard", "modelIds": []any{"disabled-model"}, "enabled": false},
		map[string]any{"requiredUserType": "standard", "modelIds": []any{"priority-model"}, "badge": map[string]any{"label": "订阅优先"}},
		map[string]any{"requiredUserType": "standard", "modelIds": []any{"priority-flag-model"}, "priority": true},
		map[string]any{"requiredUserType": "standard", "modelIds": []any{"exclusive-model"}, "badge": map[string]any{"label": "订阅优先"}, "accessMode": "exclusive"},
	}}
	if !ingestModelTiers(realmIntl, payload) {
		t.Fatal("ingestModelTiers rejected a valid payload")
	}
	if requiresPro("free-model", realmIntl) {
		t.Fatal("free tier was treated as Pro-only")
	}
	if !requiresPro("pro-model", realmIntl) || !requiresPro("shared-model", realmIntl) {
		t.Fatal("standard tier was not recorded")
	}
	if requiresPro("disabled-model", realmIntl) || requiresPro("priority-model", realmIntl) || requiresPro("priority-flag-model", realmIntl) {
		t.Fatal("disabled/priority-only tiers must not gate models")
	}
	if !requiresPro("exclusive-model", realmIntl) {
		t.Fatal("exclusive badge must gate the model")
	}

	snapshot := tierSnapshot(realmIntl)
	if snapshot["pro-model"] != "standard" || snapshot["free-model"] != "free" {
		t.Fatalf("tier snapshot = %#v", snapshot)
	}
	snapshot["pro-model"] = "free"
	if tierSnapshot(realmIntl)["pro-model"] != "standard" {
		t.Fatal("tierSnapshot returned the live map instead of a copy")
	}
	firstAt, ok := ingestedAt(realmIntl)
	if !ok || firstAt.IsZero() {
		t.Fatalf("ingestedAt = %v/%v", firstAt, ok)
	}

	// data envelope, and realm isolation.
	if !ingestModelTiers(realmCN, map[string]any{"data": map[string]any{"modelTiers": []any{
		map[string]any{"tier": "standard", "modelIds": []any{"cn-pro"}},
		map[string]any{"tier": "free", "modelIds": []any{"pro-model"}},
	}}}) {
		t.Fatal("data-enveloped tiers were not ingested")
	}
	if !requiresPro("cn-pro", realmCN) || requiresPro("cn-pro", realmIntl) {
		t.Fatal("realm tier maps are not isolated")
	}
	if requiresPro("pro-model", realmCN) {
		t.Fatal("cn free tier did not override the intl standard tier")
	}
	if !requiresPro("pro-model", realmIntl) {
		t.Fatal("cn ingest changed the intl map")
	}

	// Missing/absent modelTiers keeps the previous map and stamp.
	if ingestModelTiers(realmIntl, map[string]any{"agents": []any{}}) {
		t.Fatal("payload without modelTiers was accepted")
	}
	if tierSnapshot(realmIntl)["pro-model"] != "standard" {
		t.Fatal("missing modelTiers wiped the previous map")
	}
	if at, ok := ingestedAt(realmIntl); !ok || !at.Equal(firstAt) {
		t.Fatalf("missing modelTiers moved ingestedAt: %v -> %v", firstAt, at)
	}
}

func TestTiersEmptyArrayClearsAndUnknownUserTypeIsPaid(t *testing.T) {
	resetTierState()
	ingestModelTiers(realmIntl, map[string]any{"modelTiers": []any{
		map[string]any{"requiredUserType": "standard", "modelIds": []any{"old-pro"}},
	}})
	if !ingestModelTiers(realmIntl, map[string]any{"modelTiers": []any{}}) {
		t.Fatal("empty modelTiers array should still be ingested")
	}
	if requiresPro("old-pro", realmIntl) {
		t.Fatal("empty modelTiers array did not clear the old map")
	}
	ingestModelTiers(realmCN, map[string]any{"modelTiers": []any{
		map[string]any{"requiredUserType": "enterprise_vip", "modelIds": []any{"paid-unknown"}},
	}})
	if !requiresPro("paid-unknown", realmCN) {
		t.Fatal("unknown non-empty user type should be treated as paid")
	}
	if snapshot := tierSnapshot(""); snapshot["paid-unknown"] != "standard" {
		t.Fatalf("merged snapshot = %#v", snapshot)
	}
}

func TestTiersPoolGateWire(t *testing.T) {
	provider, pool := providerWithAccount(t, realmIntl)
	ingestModelTiers(realmIntl, map[string]any{"modelTiers": []any{
		map[string]any{"requiredUserType": "standard", "modelIds": []any{"pro-model"}},
	}})
	provider.applyProOnlyModels()
	pool.mu.RLock()
	gated := pool.proOnly["pro-model"]
	pool.mu.RUnlock()
	if !gated {
		t.Fatal("applyProOnlyModels did not wire Pool.SetProOnlyModels")
	}
	if requiresPro("pro-model", realmIntl) != true {
		t.Fatal("requiresPro lost the realm map")
	}
}
