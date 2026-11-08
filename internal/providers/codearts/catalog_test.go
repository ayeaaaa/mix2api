package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

const (
	catalogAgentListPath   = "/v1/agent-center/agents/useragents"
	catalogAgentDetailPath = "/v1/agent-center/agents/detail"
	catalogBuiltinPath     = "/v1/model/builtin"
	catalogBenefitPath     = "/api/v1/gateway/config"
	catalogClaimPath       = "/api/v1/benefit/claim"
)

type catalogFailure struct {
	status int
	body   string
}

type catalogUpstream struct {
	mu sync.Mutex

	calls     map[string]int
	queries   map[string][]url.Values
	headers   map[string][]http.Header
	bodies    map[string][]string
	absolute  map[string][]bool
	agentIDs  []string
	claimBody []string

	agents       []any
	detailModels []any
	builtin      []any
	benefit      []any
	failures     map[string]catalogFailure

	builtinStarted chan struct{}
	builtinRelease chan struct{}
}

func newCatalogUpstream() *catalogUpstream {
	return &catalogUpstream{
		calls:    make(map[string]int),
		queries:  make(map[string][]url.Values),
		headers:  make(map[string][]http.Header),
		bodies:   make(map[string][]string),
		absolute: make(map[string][]bool),
		agents: []any{
			map[string]any{
				"agent_id":         "agent-code",
				"agent_name":       "CodeAgent",
				"show_in_ide":      true,
				"alias":            map[string]any{"alias_zh_cn": "智能体"},
				"is_primary_agent": false,
			},
		},
		detailModels: []any{
			map[string]any{
				"model_name": "GLM-5.2",
				"model_parameters": map[string]any{
					"context_window": 131072,
					"max_tokens":     8192,
					"model_desc":     "最新旗舰模型",
					"model_category": "文本",
				},
			},
			map[string]any{
				"model_id":        "Agent-Only-Model",
				"supports_images": true,
				"model_parameters": map[string]any{
					"context_window": 64000,
					"max_tokens":     4096,
					"model_category": "图片理解",
				},
			},
		},
		builtin: []any{
			map[string]any{
				"model_id":       "GLM-5.2",
				"model_name":     "GLM-5.2",
				"context_window": 131072,
				"max_tokens":     8192,
				"credit": []any{
					map[string]any{"ratio_display": "0.7x", "time_slot_id": nil},
					map[string]any{"ratio_display": "0.7x", "time_slot_id": "default"},
				},
			},
			map[string]any{
				"model_id":   "GLM-4.7",
				"model_name": "GLM-4.7",
				"credit": []any{
					map[string]any{"ratio_display": "0.09x", "time_slot_id": "off_peak"},
					map[string]any{"ratio_display": "0.18x", "time_slot_id": "default"},
				},
			},
			map[string]any{
				"model_id":        "Qwen3-VL-235B",
				"model_name":      "Qwen3-VL-235B",
				"context_window":  131072,
				"max_tokens":      8192,
				"supports_images": true,
				"model_category":  "图片理解",
				"credit":          []any{},
			},
		},
		benefit: []any{
			map[string]any{
				"model_id":       "deepseek-v4-pro-0813",
				"model_name":     "deepseek-v4-pro-0813",
				"context_window": 65536,
				"max_tokens":     4096,
			},
		},
		failures: make(map[string]catalogFailure),
	}
}

func (f *catalogUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	path := r.URL.Path

	f.mu.Lock()
	f.calls[path]++
	f.queries[path] = append(f.queries[path], r.URL.Query())
	f.headers[path] = append(f.headers[path], r.Header.Clone())
	f.bodies[path] = append(f.bodies[path], string(raw))
	f.absolute[path] = append(f.absolute[path], r.URL.IsAbs())
	if path == catalogAgentDetailPath {
		f.agentIDs = append(f.agentIDs, r.URL.Query().Get("agent_id"))
	}
	if path == catalogClaimPath {
		f.claimBody = append(f.claimBody, string(raw))
	}
	failure, failed := f.failures[path]
	started := f.builtinStarted
	release := f.builtinRelease
	agents := append([]any(nil), f.agents...)
	detailModels := append([]any(nil), f.detailModels...)
	builtin := append([]any(nil), f.builtin...)
	benefit := append([]any(nil), f.benefit...)
	f.mu.Unlock()

	if path == catalogBuiltinPath && started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	if failed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(failure.status)
		_, _ = io.WriteString(w, failure.body)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	switch path {
	case catalogAgentListPath:
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": agents})
	case catalogAgentDetailPath:
		_ = json.NewEncoder(w).Encode(map[string]any{
			"gpts": map[string]any{"models": detailModels},
		})
	case catalogBuiltinPath:
		_ = json.NewEncoder(w).Encode(map[string]any{"builtinModels": builtin})
	case catalogBenefitPath:
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error_code": "0000",
			"result":     map[string]any{"models": benefit},
		})
	case catalogClaimPath:
		_ = json.NewEncoder(w).Encode(map[string]any{"error_code": "0000"})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error_msg":"not found"}`)
	}
}

func (f *catalogUpstream) fail(path string, status int, body string) {
	f.mu.Lock()
	f.failures[path] = catalogFailure{status: status, body: body}
	f.mu.Unlock()
}

func (f *catalogUpstream) clearFailures() {
	f.mu.Lock()
	f.failures = make(map[string]catalogFailure)
	f.mu.Unlock()
}

func (f *catalogUpstream) setCatalogs(agents, detail, builtin, benefit []any) {
	f.mu.Lock()
	if agents != nil {
		f.agents = agents
	}
	if detail != nil {
		f.detailModels = detail
	}
	if builtin != nil {
		f.builtin = builtin
	}
	if benefit != nil {
		f.benefit = benefit
	}
	f.mu.Unlock()
}

func (f *catalogUpstream) callCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

func (f *catalogUpstream) resetCalls() {
	f.mu.Lock()
	f.calls = make(map[string]int)
	f.queries = make(map[string][]url.Values)
	f.headers = make(map[string][]http.Header)
	f.bodies = make(map[string][]string)
	f.absolute = make(map[string][]bool)
	f.agentIDs = nil
	f.claimBody = nil
	f.mu.Unlock()
}

func (f *catalogUpstream) lastHeader(path string) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	headers := f.headers[path]
	if len(headers) == 0 {
		return nil
	}
	return headers[len(headers)-1].Clone()
}

func (f *catalogUpstream) lastQuery(path string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	queries := f.queries[path]
	if len(queries) == 0 {
		return nil
	}
	return queries[len(queries)-1]
}

func (f *catalogUpstream) lastAbsolute(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	values := f.absolute[path]
	if len(values) == 0 {
		return false
	}
	return values[len(values)-1]
}

func (f *catalogUpstream) lastClaimBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.claimBody) == 0 {
		return ""
	}
	return f.claimBody[len(f.claimBody)-1]
}

func (f *catalogUpstream) blockedBuiltin() (<-chan struct{}, func()) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	f.mu.Lock()
	f.builtinStarted = started
	f.builtinRelease = release
	f.mu.Unlock()
	var once sync.Once
	return started, func() { once.Do(func() { close(release) }) }
}

func newCatalogProvider(t *testing.T, upstream *catalogUpstream) (*Provider, *Account) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	t.Setenv("WB_CODEARTS_SNAP_HOST", server.URL)
	t.Setenv("WB_CODEARTS_BENEFIT_HOST", server.URL)
	t.Setenv("WB_CODEARTS_STS_HOST", server.URL)

	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"codearts": map[string]any{"enabled": true},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	provider, err := New(store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	auth := testAuth("u1")
	auth.Expiration = time.Now().Add(4 * time.Hour)
	account, err := provider.Pool().Add(auth)
	if err != nil {
		t.Fatalf("Pool.Add: %v", err)
	}
	return provider, account
}

func newEmptyCatalogProvider(t *testing.T) *Provider {
	t.Helper()
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"codearts": map[string]any{"enabled": true},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	provider, err := New(store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider
}

func setCatalogNow(t *testing.T, provider *Provider, now time.Time) {
	t.Helper()
	provider.pool.mu.Lock()
	provider.pool.now = func() time.Time { return now }
	provider.pool.mu.Unlock()
}

func setCatalogBenefitAutoClaim(t *testing.T, provider *Provider, enabled bool) {
	t.Helper()
	cfg := LoadConfig(provider.store)
	cfg.BenefitAutoClaim = enabled
	provider.pool.SetConfig(cfg)
}

func setAccountCatalog(t *testing.T, account *Account, models []Model, fetchedAt time.Time) {
	t.Helper()
	account.mu.Lock()
	account.Models = cloneModels(models)
	account.modelBenefit = nil
	account.modelsFetchedAt = fetchedAt
	account.modelsAttemptedAt = time.Time{}
	account.mu.Unlock()
}

func catalogModelByID(t *testing.T, models []Model, id string) Model {
	t.Helper()
	for _, item := range models {
		if got, _ := item["id"].(string); strings.EqualFold(got, id) {
			return item
		}
	}
	t.Fatalf("model %q not found in %v", id, catalogModelIDs(models))
	return nil
}

func catalogModelIDs(models []Model) []string {
	ids := make([]string, 0, len(models))
	for _, item := range models {
		if id, ok := item["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func catalogHasID(models []Model, id string) bool {
	for _, item := range models {
		if got, _ := item["id"].(string); strings.EqualFold(got, id) {
			return true
		}
	}
	return false
}

func catalogHasExactID(models []Model, id string) bool {
	for _, item := range models {
		if got, _ := item["id"].(string); got == id {
			return true
		}
	}
	return false
}

func catalogNumber(t *testing.T, value any) int64 {
	t.Helper()
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			t.Fatalf("number %q: %v", typed, err)
		}
		return number
	default:
		t.Fatalf("value %v (%T) is not numeric", value, value)
		return 0
	}
}

func catalogString(t *testing.T, value any) string {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("value %v (%T) is not a string", value, value)
	}
	return text
}

func catalogStrings(t *testing.T, value any) []string {
	t.Helper()
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, catalogString(t, item))
		}
		return out
	default:
		t.Fatalf("value %v (%T) is not a string list", value, value)
		return nil
	}
}

func catalogMap(t *testing.T, value any) map[string]any {
	t.Helper()
	typed, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value %v (%T) is not a map", value, value)
	}
	return typed
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestCatalogFetchModelsMergesParallelSources(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))

	models, err := provider.FetchModels(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	for _, id := range []string{
		"GLM-5.2", "GLM-4.7", "Qwen3-VL-235B", "Agent-Only-Model",
		"deepseek-v4-pro-0813",
		"DeepSeek-V4.1-Flash-1017", "Kimi-K3",
	} {
		catalogModelByID(t, models, id)
	}
	seen := make(map[string]bool)
	for _, item := range models {
		id := strings.ToLower(catalogString(t, item["id"]))
		if seen[id] {
			t.Fatalf("duplicate model id %q in %v", id, catalogModelIDs(models))
		}
		seen[id] = true
	}

	glm := catalogModelByID(t, models, "GLM-5.2")
	if got := catalogNumber(t, glm["context_window"]); got != 131072 {
		t.Fatalf("GLM-5.2 context_window = %d", got)
	}
	if got := catalogString(t, glm["desc"]); got != "最新旗舰模型" {
		t.Fatalf("GLM-5.2 desc = %q", got)
	}
	if got := catalogString(t, glm["credits"]); got != "x0.7" {
		t.Fatalf("GLM-5.2 credits = %q", got)
	}
	wrong := catalogModelByID(t, models, "GLM-4.7")
	if got := catalogString(t, wrong["credits"]); got != "x0.18" {
		t.Fatalf("GLM-4.7 credits = %q, off-peak discount leaked", got)
	}
	qwen := catalogModelByID(t, models, "Qwen3-VL-235B")
	if got, _ := qwen["supports_images"].(bool); !got {
		t.Fatalf("Qwen3-VL-235B supports_images = %v", qwen["supports_images"])
	}
	if got := catalogString(t, qwen["model_category"]); got != "图片理解" {
		t.Fatalf("Qwen3-VL-235B model_category = %q", got)
	}
	benefit := catalogModelByID(t, models, "deepseek-v4-pro-0813")
	if got, _ := benefit["benefit"].(bool); !got {
		t.Fatalf("benefit model flag = %v", benefit["benefit"])
	}
	if got := catalogNumber(t, benefit["context_window"]); got != 65536 {
		t.Fatalf("benefit context_window = %d", got)
	}
	if !account.IsBenefit("DeepSeek-v4-Pro-0813") {
		t.Fatal("account benefit map did not record the benefit model")
	}

	if got := upstream.callCount(catalogAgentListPath); got != 1 {
		t.Fatalf("agent list calls = %d", got)
	}
	query := upstream.lastQuery(catalogAgentListPath)
	if query.Get("is_primary_agent") != "true" || query.Get("limit") != "100" {
		t.Fatalf("agent list query = %v", query)
	}
	if query := upstream.lastQuery(catalogAgentDetailPath); query.Get("agent_id") != "agent-code" {
		t.Fatalf("agent detail query = %v", query)
	}
	if got := upstream.callCount(catalogClaimPath); got != 1 {
		t.Fatalf("benefit claim calls = %d, want 1", got)
	}
}

func TestCatalogIDsStayExactAndOneZeroOneSevenAlias(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))
	if _, err := provider.FetchModels(context.Background(), account, true); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	models, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	catalogModelByID(t, models, "DeepSeek-V4.1-Flash-1017")
	catalogModelByID(t, models, "Kimi-K3")
	catalogModelByID(t, models, "deepseek-v4-flash-0731")
	if catalogHasExactID(models, "deepseek-v4.1-flash-1017") {
		t.Fatalf("API catalog lowercased the upstream 1017 id: %v", catalogModelIDs(models))
	}

	if got := CanonicalModel(account, "kimi-k3"); got != "Kimi-K3" {
		t.Fatalf("CanonicalModel(Kimi-K3) = %q", got)
	}
	if got := CanonicalModel(account, "glm-5.2"); got != "GLM-5.2" {
		t.Fatalf("CanonicalModel(GLM-5.2) = %q", got)
	}
	if got := CanonicalModel(account, "snap-chat"); got != "GLM-5.2" {
		t.Fatalf("CanonicalModel(snap-chat) = %q", got)
	}
	if got := CanonicalModel(account, "DeepSeek-V4.1-Flash-1017"); got != "deepseek-v4.1-flash-1017" {
		t.Fatalf("CanonicalModel(1017) = %q, want lowercase upstream id", got)
	}
	if got := CanonicalModel(account, "deepseek-v4.1-flash-1017"); got != "deepseek-v4.1-flash-1017" {
		t.Fatalf("CanonicalModel(lower 1017) = %q", got)
	}
	if got := CanonicalModel(account, "Custom-Model"); got != "Custom-Model" {
		t.Fatalf("CanonicalModel(unknown) = %q", got)
	}
	if got := CanonicalModel(account, "  "); got != "" {
		t.Fatalf("CanonicalModel(blank) = %q", got)
	}

	picker, err := provider.AgentPickerModels(context.Background(), false)
	if err != nil {
		t.Fatalf("AgentPickerModels: %v", err)
	}
	if !catalogHasExactID(picker, "deepseek-v4.1-flash-1017") {
		t.Fatalf("agent picker missing lowercase 1017 alias: %v", catalogModelIDs(picker))
	}
	if catalogHasExactID(picker, "DeepSeek-V4.1-Flash-1017") {
		t.Fatalf("agent picker kept a mixed-case 1017 duplicate: %v", catalogModelIDs(picker))
	}
	if !catalogHasExactID(picker, "kimi-k3") {
		t.Fatalf("agent picker did not lowercase an official model id: %v", catalogModelIDs(picker))
	}
	if !catalogHasExactID(picker, "Agent-Only-Model") || catalogHasExactID(picker, "agent-only-model") {
		t.Fatalf("agent picker changed a live/non-official id: %v", catalogModelIDs(picker))
	}
}

func TestCatalogModelsWithoutAccountsUsesOfficialAndBenefitSeeds(t *testing.T) {
	provider := newEmptyCatalogProvider(t)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))

	models, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	for _, id := range []string{
		"GLM-5.2", "DeepSeek-V4.1-Flash", "DeepSeek-V4.1-Flash-1017",
		"Kimi-K3", "Kimi-K3.1-Pro", "GLM-5.3-Flash",
		"deepseek-v4-flash-0731", "deepseek-v4-pro-0813",
	} {
		catalogModelByID(t, models, id)
	}
	if catalogHasID(models, "Qwen3.5-397B-A17B-VL") {
		t.Fatalf("official/benefit output leaked the wider cold seed: %v", catalogModelIDs(models))
	}
	benefit := catalogModelByID(t, models, "deepseek-v4-pro-0813")
	if got, _ := benefit["benefit"].(bool); !got {
		t.Fatalf("benefit seed not flagged: %v", benefit)
	}
}

func TestCatalogFetchFailureRetainsLastGood(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)
	prior := []Model{{"id": "Prior-Only", "name": "Prior-Only", "context_window": int64(1234), "max_tokens": int64(5678)}}
	setAccountCatalog(t, account, prior, now.Add(-time.Hour))

	for _, path := range []string{catalogAgentListPath, catalogBuiltinPath, catalogBenefitPath} {
		upstream.fail(path, http.StatusInternalServerError, `{"error_msg":"upstream down"}`)
	}
	models, err := provider.FetchModels(context.Background(), account, true)
	if err == nil {
		t.Fatal("FetchModels should report that every discovery source failed")
	}
	if len(models) != 1 || models[0]["id"] != "Prior-Only" {
		t.Fatalf("last good catalog was replaced: %v", catalogModelIDs(models))
	}
	account.mu.Lock()
	accountIDs := catalogModelIDs(account.Models)
	account.mu.Unlock()
	if len(accountIDs) != 1 || accountIDs[0] != "Prior-Only" {
		t.Fatalf("account catalog was replaced: %v", accountIDs)
	}

	catalog, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models after failed refresh: %v", err)
	}
	catalogModelByID(t, catalog, "Prior-Only")
	catalogModelByID(t, catalog, "Kimi-K3")
}

func TestCatalogEmptySuccessAndSeedFallback(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)

	t.Run("empty success still produces official catalogue", func(t *testing.T) {
		upstream := newCatalogUpstream()
		upstream.setCatalogs(nil, []any{}, []any{}, []any{})
		provider, account := newCatalogProvider(t, upstream)
		setCatalogNow(t, provider, now)
		models, err := provider.FetchModels(context.Background(), account, true)
		if err != nil {
			t.Fatalf("a valid empty discovery must not error: %v", err)
		}
		for _, id := range []string{"GLM-5.2", "Kimi-K3", "GLM-5.3-Flash", "DeepSeek-V4.1-Flash-1017"} {
			catalogModelByID(t, models, id)
		}
		if catalogHasID(models, "Qwen3.5-397B-A17B-VL") || catalogHasID(models, "deepseek-v4-flash-0731") {
			t.Fatalf("empty success used the wider cold seed: %v", catalogModelIDs(models))
		}
		account.mu.Lock()
		stored := len(account.Models)
		fetchedAt := account.modelsFetchedAt
		attemptedAt := account.modelsAttemptedAt
		account.mu.Unlock()
		if stored != len(codeartsOfficialModels) || fetchedAt.IsZero() || attemptedAt.IsZero() {
			t.Fatalf("empty success was not adopted: stored=%d fetched=%v attempted=%v", stored, fetchedAt, attemptedAt)
		}
	})

	t.Run("empty success keeps prior as base", func(t *testing.T) {
		upstream := newCatalogUpstream()
		upstream.setCatalogs(nil, []any{}, []any{}, []any{})
		provider, account := newCatalogProvider(t, upstream)
		setCatalogNow(t, provider, now)
		prior := []Model{{"id": "Prior-Only", "name": "Prior-Only"}}
		setAccountCatalog(t, account, prior, now.Add(-time.Hour))
		models, err := provider.FetchModels(context.Background(), account, true)
		if err != nil {
			t.Fatalf("FetchModels: %v", err)
		}
		catalogModelByID(t, models, "Prior-Only")
		catalogModelByID(t, models, "Kimi-K3")
		if catalogHasID(models, "Qwen3.5-397B-A17B-VL") {
			t.Fatalf("empty success fell back to the cold seed: %v", catalogModelIDs(models))
		}
		account.mu.Lock()
		fetchedAt := account.modelsFetchedAt
		attemptedAt := account.modelsAttemptedAt
		account.mu.Unlock()
		if !fetchedAt.Equal(now) || !attemptedAt.Equal(now) {
			t.Fatalf("empty success did not stamp fetched/attempted: %v/%v", fetchedAt, attemptedAt)
		}
	})

	t.Run("total failure without prior seeds", func(t *testing.T) {
		upstream := newCatalogUpstream()
		for _, path := range []string{catalogAgentListPath, catalogBuiltinPath, catalogBenefitPath} {
			upstream.fail(path, http.StatusInternalServerError, `{"error_msg":"down"}`)
		}
		provider, account := newCatalogProvider(t, upstream)
		setCatalogNow(t, provider, now)
		models, err := provider.FetchModels(context.Background(), account, true)
		if err == nil {
			t.Fatal("total discovery failure should be reported")
		}
		for _, id := range []string{"GLM-5.2", "Qwen3.5-397B-A17B-VL", "deepseek-v4-pro-0813"} {
			catalogModelByID(t, models, id)
		}
	})

	t.Run("one source survives", func(t *testing.T) {
		upstream := newCatalogUpstream()
		upstream.fail(catalogAgentListPath, http.StatusInternalServerError, `{"error_msg":"agent down"}`)
		upstream.fail(catalogBenefitPath, http.StatusInternalServerError, `{"error_msg":"benefit down"}`)
		provider, account := newCatalogProvider(t, upstream)
		setCatalogNow(t, provider, now)
		models, err := provider.FetchModels(context.Background(), account, true)
		if err != nil {
			t.Fatalf("one failed source must not fail discovery: %v", err)
		}
		catalogModelByID(t, models, "GLM-4.7")
		catalogModelByID(t, models, "Kimi-K3")
	})
}

func TestCatalogModelsTTLAndForce(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, _ := newCatalogProvider(t, upstream)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)

	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("first Models: %v", err)
	}
	first := upstream.callCount(catalogBuiltinPath)
	if first != 1 {
		t.Fatalf("first discovery builtin calls = %d", first)
	}

	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("cached Models: %v", err)
	}
	if got := upstream.callCount(catalogBuiltinPath); got != first {
		t.Fatalf("fresh TTL refetched: calls = %d", got)
	}

	setCatalogNow(t, provider, now.Add(301*time.Second))
	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("stale Models: %v", err)
	}
	if got := upstream.callCount(catalogBuiltinPath); got != first+1 {
		t.Fatalf("stale TTL did not refetch: calls = %d", got)
	}

	if _, err := provider.Models(context.Background(), true); err != nil {
		t.Fatalf("forced Models: %v", err)
	}
	if got := upstream.callCount(catalogBuiltinPath); got != first+2 {
		t.Fatalf("force did not bypass TTL: calls = %d", got)
	}
}

func TestCatalogConcurrentFetchCoalescesInFlight(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))

	started, release := upstream.blockedBuiltin()
	defer release()
	joined := make(chan struct{}, 1)
	provider.modelFetchJoinHook = func() {
		select {
		case joined <- struct{}{}:
		default:
		}
	}

	var wg sync.WaitGroup
	results := make([][]Model, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = provider.FetchModels(context.Background(), account, true)
		}(i)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not reach the builtin endpoint")
	}
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("second caller did not join the in-flight discovery")
	}
	before := upstream.callCount(catalogBuiltinPath)
	release()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent FetchModels[%d]: %v", i, err)
		}
		if len(results[i]) == 0 {
			t.Fatalf("concurrent FetchModels[%d] returned no models", i)
		}
	}
	if got := upstream.callCount(catalogBuiltinPath); got != before {
		t.Fatalf("concurrent force fetch stamped out: calls = %d, before = %d", got, before)
	}
}

func TestCatalogEnrichesCapabilitiesReasoningAndCredits(t *testing.T) {
	provider := newEmptyCatalogProvider(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)
	account := &Account{
		Auth:   testAuth("enrich"),
		Models: nil,
	}
	models := []Model{
		{
			"id": "GLM-5.2", "name": "GLM-5.2",
			"context_window": int64(131072), "max_tokens": int64(8192),
			"desc": "最新旗舰模型", "model_category": "文本",
			"supports_images": false, "credits": "x0.7",
		},
		{
			"id": "Qwen3-VL-235B", "name": "Qwen3-VL-235B",
			"context_window": int64(131072), "max_tokens": int64(8192),
			"model_category": "图片理解",
		},
		{
			"id": "GLM-5.3", "name": "GLM-5.3",
			"context_window": int64(0), "max_tokens": int64(0),
		},
		{
			"id": "GLM-5.3-Flash", "name": "GLM-5.3-Flash",
		},
		{
			"id": "DeepSeek-V4.1-Flash-1017", "name": "DeepSeek-V4.1-Flash-1017",
		},
		{
			"id": "Kimi-K3", "name": "Kimi-K3",
		},
	}
	setAccountCatalog(t, account, models, now)
	provider.pool.mu.Lock()
	provider.pool.accounts = append(provider.pool.accounts, account)
	provider.pool.mu.Unlock()

	entries, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	glm := catalogModelByID(t, entries, "GLM-5.2")
	if got := catalogString(t, glm["object"]); got != "model" {
		t.Fatalf("object = %q", got)
	}
	if got := catalogString(t, glm["owned_by"]); got != "codearts" {
		t.Fatalf("owned_by = %q", got)
	}
	if got := catalogString(t, glm["provider"]); got != "codearts" {
		t.Fatalf("provider = %q", got)
	}
	if got := catalogNumber(t, glm["created"]); got != now.Unix() {
		t.Fatalf("created = %d, want %d", got, now.Unix())
	}
	for key, want := range map[string]int64{
		"context_window":        131072,
		"context_length":        131072,
		"max_input_tokens":      131072,
		"max_tokens":            8192,
		"max_output_tokens":     8192,
		"max_completion_tokens": 8192,
		"max_output":            8192,
	} {
		if got := catalogNumber(t, glm[key]); got != want {
			t.Fatalf("GLM-5.2 %s = %d, want %d", key, got, want)
		}
	}
	if got := catalogString(t, glm["description"]); got != "最新旗舰模型" {
		t.Fatalf("description = %q", got)
	}
	if got := catalogString(t, glm["category"]); got != "文本" {
		t.Fatalf("category = %q", got)
	}
	capabilities := catalogMap(t, glm["capabilities"])
	if vision, _ := capabilities["vision"].(bool); vision {
		t.Fatalf("GLM-5.2 vision = %v", capabilities["vision"])
	}
	if toolCalls, _ := capabilities["tool_calls"].(bool); !toolCalls {
		t.Fatalf("GLM-5.2 tool_calls = %v", capabilities["tool_calls"])
	}
	if reasoning, _ := capabilities["reasoning"].(bool); !reasoning {
		t.Fatalf("GLM-5.2 reasoning = %v", capabilities["reasoning"])
	}
	if got, _ := glm["supports_reasoning"].(bool); !got {
		t.Fatalf("GLM-5.2 supports_reasoning = %v", glm["supports_reasoning"])
	}
	if got := catalogStrings(t, glm["reasoning_efforts"]); !containsString(got, "high") || !containsString(got, "none") {
		t.Fatalf("GLM-5.2 reasoning_efforts = %v", got)
	}
	if got := catalogString(t, glm["credits"]); got != "x0.7" {
		t.Fatalf("GLM-5.2 credits = %q", got)
	}
	if got, _ := glm["supports_vision"].(bool); got {
		t.Fatalf("GLM-5.2 supports_vision = %v", glm["supports_vision"])
	}

	qwen := catalogModelByID(t, entries, "Qwen3-VL-235B")
	if got, _ := qwen["supports_vision"].(bool); !got {
		t.Fatalf("Qwen supports_vision = %v", qwen["supports_vision"])
	}
	if got, _ := qwen["supports_images"].(bool); !got {
		t.Fatalf("Qwen supports_images = %v", qwen["supports_images"])
	}
	if got, _ := qwen["vision"].(bool); !got {
		t.Fatalf("Qwen vision = %v", qwen["vision"])
	}
	if got, _ := qwen["multimodal"].(bool); !got {
		t.Fatalf("Qwen multimodal = %v", qwen["multimodal"])
	}
	if got := catalogStrings(t, qwen["input_modalities"]); !containsString(got, "image") {
		t.Fatalf("Qwen input_modalities = %v", got)
	}
	architecture := catalogMap(t, qwen["architecture"])
	if got := catalogString(t, architecture["modality"]); got != "text+image->text" {
		t.Fatalf("Qwen architecture modality = %q", got)
	}
	abilities := catalogMap(t, qwen["abilities"])
	if got, _ := abilities["functionCall"].(bool); !got {
		t.Fatalf("Qwen abilities.functionCall = %v", abilities["functionCall"])
	}
	capabilities = catalogMap(t, qwen["capabilities"])
	if _, ok := capabilities["reasoning"]; ok {
		t.Fatalf("non-reasoning model advertised capabilities.reasoning: %v", capabilities)
	}
	if _, ok := qwen["supports_reasoning"]; ok {
		t.Fatalf("non-reasoning model advertised supports_reasoning: %v", qwen["supports_reasoning"])
	}
	if _, ok := abilities["reasoning"]; ok {
		t.Fatalf("non-reasoning model advertised abilities.reasoning: %v", abilities)
	}
	if _, ok := qwen["credits"]; ok {
		t.Fatalf("Qwen advertised a price without a rate: %v", qwen["credits"])
	}

	glm53 := catalogModelByID(t, entries, "GLM-5.3")
	if got := catalogNumber(t, glm53["context_window"]); got != 1000000 {
		t.Fatalf("GLM-5.3 fallback context_window = %d", got)
	}
	if got := catalogNumber(t, glm53["max_tokens"]); got != 48000 {
		t.Fatalf("GLM-5.3 fallback max_tokens = %d", got)
	}
	if got := catalogString(t, glm53["credits"]); got != "x0.79" {
		t.Fatalf("GLM-5.3 credits = %q", got)
	}

	flash := catalogModelByID(t, entries, "GLM-5.3-Flash")
	if got, _ := flash["always_reasoning"].(bool); !got {
		t.Fatalf("GLM-5.3-Flash always_reasoning = %v", flash["always_reasoning"])
	}
	if got, ok := flash["reasoning_can_disable"].(bool); !ok || got {
		t.Fatalf("GLM-5.3-Flash reasoning_can_disable = %v", flash["reasoning_can_disable"])
	}
	if got := catalogStrings(t, flash["reasoning_efforts"]); len(got) != 3 || !containsString(got, "high") {
		t.Fatalf("GLM-5.3-Flash reasoning_efforts = %v", got)
	}

	deepseek := catalogModelByID(t, entries, "DeepSeek-V4.1-Flash-1017")
	if got := catalogString(t, deepseek["credits"]); got != "x0.18" {
		t.Fatalf("1017 peak credits = %q", got)
	}
	if got := catalogString(t, deepseek["credits_peak"]); got != "x0.18" {
		t.Fatalf("1017 credits_peak = %q", got)
	}
	if got := catalogString(t, deepseek["credits_offpeak"]); got != "x0.12" {
		t.Fatalf("1017 credits_offpeak = %q", got)
	}
	if got := catalogString(t, deepseek["credits_note"]); got != "peak 0.18x / off-peak 0.12x" {
		t.Fatalf("1017 credits_note = %q", got)
	}

	kimi := catalogModelByID(t, entries, "Kimi-K3")
	if got := catalogNumber(t, kimi["context_window"]); got != 1000000 {
		t.Fatalf("Kimi-K3 fallback context = %d", got)
	}
	if got := catalogModelByID(t, entries, "Kimi-K3")["credits"]; got != "x2.38" {
		t.Fatalf("Kimi-K3 credits = %v", got)
	}
}

func TestCatalogSkipsDisabledManualAndRemovedAccounts(t *testing.T) {
	cases := []struct {
		name  string
		state AccountState
	}{
		{name: "disabled", state: AccountState{Disabled: true}},
		{name: "manual disabled", state: AccountState{ManualDisabled: true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := newCatalogUpstream()
			provider, account := newCatalogProvider(t, upstream)
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
			setCatalogNow(t, provider, now)
			setAccountCatalog(t, account, []Model{{"id": "Hidden-Only", "name": "Hidden-Only"}}, now)
			account.mu.Lock()
			account.State.Disabled = testCase.state.Disabled
			account.State.ManualDisabled = testCase.state.ManualDisabled
			account.mu.Unlock()

			models, err := provider.Models(context.Background(), false)
			if err != nil {
				t.Fatalf("Models: %v", err)
			}
			if catalogHasID(models, "Hidden-Only") {
				t.Fatalf("disabled account catalog leaked: %v", catalogModelIDs(models))
			}
			catalogModelByID(t, models, "GLM-5.2")
			if got := upstream.callCount(catalogBuiltinPath); got != 0 {
				t.Fatalf("disabled account was refreshed: calls = %d", got)
			}
		})
	}

	t.Run("removed", func(t *testing.T) {
		upstream := newCatalogUpstream()
		provider, account := newCatalogProvider(t, upstream)
		now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
		setCatalogNow(t, provider, now)
		setAccountCatalog(t, account, []Model{{"id": "Hidden-Only", "name": "Hidden-Only"}}, now)
		account.mu.Lock()
		account.removed = true
		account.mu.Unlock()
		models, err := provider.Models(context.Background(), false)
		if err != nil {
			t.Fatalf("Models: %v", err)
		}
		if catalogHasID(models, "Hidden-Only") {
			t.Fatalf("removed account catalog leaked: %v", catalogModelIDs(models))
		}
		if got := upstream.callCount(catalogBuiltinPath); got != 0 {
			t.Fatalf("removed account was refreshed: calls = %d", got)
		}
	})
}

func TestCatalogAgentSelectionFallsBack(t *testing.T) {
	cases := []struct {
		name   string
		agents []any
		want   string
	}{
		{
			name: "primary",
			agents: []any{
				map[string]any{"agent_id": "agent-plain", "agent_name": "Other", "is_primary_agent": true},
				map[string]any{"agent_id": "agent-second", "agent_name": "CodeAgent", "show_in_ide": true},
			},
			want: "agent-plain",
		},
		{
			name: "first",
			agents: []any{
				map[string]any{"agent_id": "agent-first", "agent_name": "First"},
				map[string]any{"agent_id": "agent-later", "agent_name": "Later"},
			},
			want: "agent-first",
		},
		{
			name: "exact preferred over primary",
			agents: []any{
				map[string]any{"agent_id": "agent-primary", "agent_name": "CodeAgent", "is_primary_agent": true, "show_in_ide": true},
				map[string]any{
					"agent_id": "agent-exact", "agent_name": "CodeAgent", "show_in_ide": true,
					"alias": map[string]any{"alias_zh_cn": "智能体"},
				},
			},
			want: "agent-exact",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := newCatalogUpstream()
			upstream.setCatalogs(testCase.agents, nil, nil, nil)
			provider, account := newCatalogProvider(t, upstream)
			setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))
			if _, err := provider.FetchModels(context.Background(), account, true); err != nil {
				t.Fatalf("FetchModels: %v", err)
			}
			if got := upstream.lastQuery(catalogAgentDetailPath).Get("agent_id"); got != testCase.want {
				t.Fatalf("selected agent = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestCatalogBenefitClaimCanBeDisabled(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))
	setCatalogBenefitAutoClaim(t, provider, false)

	if _, err := provider.FetchModels(context.Background(), account, true); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if got := upstream.callCount(catalogClaimPath); got != 0 {
		t.Fatalf("benefit claim ran while disabled: calls = %d", got)
	}
	models, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	benefit := catalogModelByID(t, models, "deepseek-v4-pro-0813")
	if got, _ := benefit["benefit"].(bool); !got {
		t.Fatalf("benefit model flag = %v", benefit["benefit"])
	}
}

func TestCatalogSignedHeadersAndProxy(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))

	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	account.mu.Lock()
	account.proxy = server.URL
	account.mu.Unlock()

	if _, err := provider.FetchModels(context.Background(), account, true); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	headers := upstream.lastHeader(catalogBuiltinPath)
	if headers == nil {
		t.Fatal("builtin request was not recorded")
	}
	authorization := headers.Get("Authorization")
	if !strings.HasPrefix(authorization, "SDK-HMAC-SHA256 Access=AK-u1") {
		t.Fatalf("Authorization = %q", authorization)
	}
	if !strings.Contains(authorization, "SignedHeaders=") || !strings.Contains(authorization, "Signature=") {
		t.Fatalf("Authorization is not a Huawei signature: %q", authorization)
	}
	if got := headers.Get("X-Security-Token"); got != "cloud-token-u1" {
		t.Fatalf("X-Security-Token = %q", got)
	}
	if headers.Get("X-Sdk-Date") == "" {
		t.Fatal("X-Sdk-Date is missing")
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := headers.Get("X-Language"); got != "zh-cn" {
		t.Fatalf("X-Language = %q", got)
	}
	if got := headers.Get("Agent-Type"); got != "PromptCenter" {
		t.Fatalf("builtin Agent-Type = %q", got)
	}
	if !upstream.lastAbsolute(catalogBuiltinPath) {
		t.Fatal("account proxy was not used for model discovery")
	}
	claimHeader := upstream.lastHeader(catalogClaimPath)
	if got := claimHeader.Get("Agent-Type"); got != "" {
		t.Fatalf("benefit claim Agent-Type = %q", got)
	}
	if got := upstream.lastClaimBody(); got != "{}" {
		t.Fatalf("benefit claim body = %q, want {}", got)
	}
}

func TestCatalogErrorsRedactSecrets(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))
	body := `{"error_msg":"bad cloud-token-u1 secret-key-u1 refresh-token-u1 AK-u1"}`
	for _, path := range []string{catalogAgentListPath, catalogBuiltinPath, catalogBenefitPath} {
		upstream.fail(path, http.StatusBadGateway, body)
	}
	_, err := provider.FetchModels(context.Background(), account, true)
	if err == nil {
		t.Fatal("FetchModels should report upstream failures")
	}
	message := err.Error()
	for _, secret := range []string{"cloud-token-u1", "secret-key-u1", "refresh-token-u1", "AK-u1"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked %q: %s", secret, message)
		}
	}
}

// TestCatalogTTLDefault documents the 300-second default even when tests build
// a sparse Config directly.
func TestCatalogTTLDefault(t *testing.T) {
	provider := newEmptyCatalogProvider(t)
	provider.pool.SetConfig(Config{})
	if got := provider.modelsTTL(); got != 300*time.Second {
		t.Fatalf("modelsTTL = %v, want 300s", got)
	}
	provider.pool.SetConfig(Config{ModelsTTLSeconds: 86400})
	if got := provider.modelsTTL(); got != 3600*time.Second {
		t.Fatalf("modelsTTL = %v, want the 1h v1 ceiling", got)
	}
}

func TestCatalogFetchModelsHonorsTTLAndForce(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))

	if _, err := provider.FetchModels(context.Background(), account, false); err != nil {
		t.Fatalf("first FetchModels: %v", err)
	}
	if got := upstream.callCount(catalogBuiltinPath); got != 1 {
		t.Fatalf("first FetchModels calls = %d", got)
	}
	if _, err := provider.FetchModels(context.Background(), account, false); err != nil {
		t.Fatalf("cached FetchModels: %v", err)
	}
	if got := upstream.callCount(catalogBuiltinPath); got != 1 {
		t.Fatalf("FetchModels ignored TTL: calls = %d", got)
	}
	if _, err := provider.FetchModels(context.Background(), account, true); err != nil {
		t.Fatalf("forced FetchModels: %v", err)
	}
	if got := upstream.callCount(catalogBuiltinPath); got != 2 {
		t.Fatalf("FetchModels force did not bypass TTL: calls = %d", got)
	}
}

func TestCatalogWireNormalizesLower1017ForOldClients(t *testing.T) {
	provider := newEmptyCatalogProvider(t)
	now := time.Date(2026, 11, 8, 22, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)
	account := &Account{Auth: testAuth("lower-1017")}
	setAccountCatalog(t, account, []Model{{
		"id": "deepseek-v4.1-flash-1017", "name": "deepseek-v4.1-flash-1017",
	}}, now)
	provider.pool.mu.Lock()
	provider.pool.accounts = append(provider.pool.accounts, account)
	provider.pool.mu.Unlock()

	models, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	catalogModelByID(t, models, "DeepSeek-V4.1-Flash-1017")
	if catalogHasExactID(models, canonicalOneZeroOneSeven) {
		t.Fatalf("API catalog kept the live lowercase 1017 id: %v", catalogModelIDs(models))
	}
	entry := catalogModelByID(t, models, "DeepSeek-V4.1-Flash-1017")
	if got := catalogString(t, entry["credits"]); got != "x0.12" {
		t.Fatalf("off-peak 1017 credits = %q", got)
	}
}

func TestCatalogV1WireSetParity(t *testing.T) {
	upstream := newCatalogUpstream()
	upstream.setCatalogs(nil, []any{
		map[string]any{
			"model_name": "GLM-5.2",
			"model_parameters": map[string]any{
				"context_window": 131072,
				"max_tokens":     8192,
			},
		},
	}, nil, nil)
	provider, account := newCatalogProvider(t, upstream)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))
	if _, err := provider.FetchModels(context.Background(), account, true); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	models, err := provider.Models(context.Background(), false)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}

	want := map[string]bool{
		"deepseek-v4-flash-0731": true, "deepseek-v4-pro-0813": true,
		"DeepSeek-V4.1-Flash": true, "DeepSeek-V4.1-Flash-1017": true,
		"GLM-4.7": true, "GLM-5.2": true, "GLM-5.2-ArkTS-SPARK": true,
		"GLM-5.3": true, "GLM-5.3-Flash": true, "Kimi-K3": true,
		"Kimi-K3.1-Pro": true, "OpenPangu-2.0-Flash": true,
		"OpenPangu-2.0-Pro": true, "Qwen3-VL-235B": true,
	}
	got := make(map[string]bool, len(models))
	for _, item := range models {
		got[catalogString(t, item["id"])] = true
	}
	if len(got) != len(want) {
		t.Fatalf("wire ids = %v", catalogModelIDs(models))
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("wire catalog missing %q: %v", id, catalogModelIDs(models))
		}
	}
	// Exact ordering is a consequence of the current fixture/merge order, not
	// a v1 wire guarantee, so this test deliberately checks membership only.
	entry1017 := catalogModelByID(t, models, "DeepSeek-V4.1-Flash-1017")
	if _, ok := entry1017["context_window"]; ok {
		t.Fatalf("1017 advertised a context limit without a catalogue value: %v", entry1017)
	}
	if _, ok := entry1017["max_tokens"]; ok {
		t.Fatalf("1017 advertised an output limit without a catalogue value: %v", entry1017)
	}
}

func TestCatalogFailureBackoffHonorsTTL(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)
	setAccountCatalog(t, account, []Model{{"id": "Prior-Only", "name": "Prior-Only"}}, now.Add(-time.Hour))
	for _, path := range []string{catalogAgentListPath, catalogBuiltinPath, catalogBenefitPath} {
		upstream.fail(path, http.StatusBadGateway, `{"error_msg":"down"}`)
	}

	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("first failed Models: %v", err)
	}
	if got := upstream.callCount(catalogBuiltinPath); got != 1 {
		t.Fatalf("first failed refresh calls = %d", got)
	}
	account.mu.Lock()
	fetchedAt := account.modelsFetchedAt
	attemptedAt := account.modelsAttemptedAt
	account.mu.Unlock()
	if !fetchedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("failed refresh replaced fetchedAt: %v", fetchedAt)
	}
	if !attemptedAt.Equal(now) {
		t.Fatalf("failed refresh did not record attemptedAt: %v", attemptedAt)
	}

	for i := 0; i < 2; i++ {
		if _, err := provider.Models(context.Background(), false); err != nil {
			t.Fatalf("backoff Models[%d]: %v", i, err)
		}
		if _, err := provider.FetchModels(context.Background(), account, false); err != nil {
			t.Fatalf("backoff FetchModels[%d]: %v", i, err)
		}
	}
	if got := upstream.callCount(catalogBuiltinPath); got != 1 {
		t.Fatalf("failure backoff hammered upstream: calls = %d", got)
	}

	setCatalogNow(t, provider, now.Add(301*time.Second))
	if _, err := provider.Models(context.Background(), false); err != nil {
		t.Fatalf("retry Models: %v", err)
	}
	if got := upstream.callCount(catalogBuiltinPath); got != 2 {
		t.Fatalf("expired failure backoff did not retry: calls = %d", got)
	}

	if _, err := provider.Models(context.Background(), true); err != nil {
		t.Fatalf("forced Models: %v", err)
	}
	if got := upstream.callCount(catalogBuiltinPath); got != 3 {
		t.Fatalf("force did not bypass failure backoff: calls = %d", got)
	}
}

func TestCatalogTokenEnsureFailureDoesNotBlockDiscovery(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)
	account.mu.Lock()
	account.Auth.Expiration = now.Add(-time.Hour)
	account.mu.Unlock()

	models, err := provider.FetchModels(context.Background(), account, true)
	if err != nil {
		t.Fatalf("token refresh failure blocked discovery: %v", err)
	}
	catalogModelByID(t, models, "GLM-4.7")
	catalogModelByID(t, models, "Kimi-K3")
	if got := upstream.callCount("/v1/oauth2/tokens"); got == 0 {
		upstream.mu.Lock()
		calls := upstream.calls
		upstream.mu.Unlock()
		t.Fatalf("expired account did not attempt a token refresh; calls=%v", calls)
	}
}

func TestCatalogReimportInvalidatesInFlightDiscovery(t *testing.T) {
	upstream := newCatalogUpstream()
	provider, account := newCatalogProvider(t, upstream)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)

	started, release := upstream.blockedBuiltin()
	defer release()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = provider.FetchModels(context.Background(), account, true)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not reach the builtin endpoint")
	}

	reimport := testAuth("u1")
	reimport.Expiration = now.Add(4 * time.Hour)
	if _, err := provider.Pool().Add(reimport); err != nil {
		t.Fatalf("reimport: %v", err)
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight discovery did not finish")
	}

	account.mu.Lock()
	stored := len(account.Models)
	fetchedAt := account.modelsFetchedAt
	attemptedAt := account.modelsAttemptedAt
	account.mu.Unlock()
	if stored != 0 || !fetchedAt.IsZero() || !attemptedAt.IsZero() {
		t.Fatalf("late discovery repopulated reimported account: models=%d fetched=%v attempted=%v",
			stored, fetchedAt, attemptedAt)
	}
	account.modelsMu.Lock()
	flight := account.modelsFlight
	account.modelsMu.Unlock()
	if flight != nil {
		t.Fatal("reimport did not clear the in-flight discovery")
	}
}

func TestModelsDisabledRealmReturnsUnavailable(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"codearts": map[string]any{"enabled": false},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	provider, err := New(store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = provider.Models(context.Background(), false)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Models error = %v, want *APIError", err)
	}
	if apiErr.Kind != KindUnavailable || apiErr.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("disabled Models error = %#v, want unavailable/503", apiErr)
	}
	if apiErr.SafeMessage() != disabledAdminMessage {
		t.Fatalf("disabled Models message = %q", apiErr.SafeMessage())
	}
}
