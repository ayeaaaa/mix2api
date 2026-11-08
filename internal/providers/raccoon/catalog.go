package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const (
	// CatalogPath is the v1 Raccoon model catalog endpoint. Every model ID in
	// the response stays bare: no provider namespace is added or stripped.
	CatalogPath = "/api/web/llm/v2/model_catalog"

	// Desktop-client identity headers accepted by the catalog endpoint. Chat
	// reuses the same identity.
	ClientPlatform = "desktop-linux-x64"
	ClientVersion  = "v1.1.5"
	ClientLanguage = "zh"

	// maxModelDisplayRunes mirrors v1's display name cap.
	maxModelDisplayRunes = 120
)

// catalogCache is the last successful catalog shared by the pool. source is
// "raccoon" only for a live upstream result; seed fallbacks are never cached
// as fresh data.
type catalogCache struct {
	models       []providers.Model
	defaultModel string
	fetchedAt    time.Time
	source       string
}

// normalizedCatalog is the provider-owned shape between a raw upstream
// payload and the wire entries returned by Models.
type normalizedCatalog struct {
	models       []providers.Model
	defaultModel string
}

type seedModelDefinition struct {
	id            string
	name          string
	contextWindow int64
	maxTokens     int64
	tags          []string
}

// seedModelDefinitions is the verified v1 fallback catalog. IDs stay bare.
var seedModelDefinitions = []seedModelDefinition{
	{
		id:            "raccoon-090dd3",
		name:          "Raccoon-Work",
		contextWindow: 1000000,
		maxTokens:     100000,
		tags:          []string{"vision", "reasoning"},
	},
	{
		id:            "raccoon-19b265",
		name:          "Raccoon-Work-260817-A",
		contextWindow: 1000000,
		maxTokens:     100000,
		tags:          []string{},
	},
	{
		id:            "raccoon-405a1c",
		name:          "Raccoon-Work-260817-B",
		contextWindow: 1000000,
		maxTokens:     100000,
		tags:          []string{},
	},
	{
		id:            "sn-sensenova-6-8-flash",
		name:          "SenseNova 6.8 Flash",
		contextWindow: 256000,
		maxTokens:     63999,
		tags:          []string{"vision"},
	},
	{
		id:            "sn-sensenova-6-8-flash-lite",
		name:          "SenseNova 6.8 Flash Lite",
		contextWindow: 256000,
		maxTokens:     63999,
		tags:          []string{"vision"},
	},
	{
		id:            "sn-glm-5-3",
		name:          "GLM-5.3",
		contextWindow: 1000000,
		maxTokens:     100000,
		tags:          []string{"reasoning"},
	},
	{
		id:            "sn-kimi-k3",
		name:          "Kimi K3",
		contextWindow: 1000000,
		maxTokens:     100000,
		tags:          []string{"vision", "reasoning"},
	},
	{
		id:            "sn-glm-5-3-flash",
		name:          "GLM-5.3 Flash",
		contextWindow: 1000000,
		maxTokens:     100000,
		tags:          []string{"vision"},
	},
	{
		id:            "sn-deepseek-v4-1-flash-1017",
		name:          "DeepSeek V4.1 Flash 1017",
		contextWindow: 1000000,
		maxTokens:     100000,
		tags:          []string{"vision", "reasoning"},
	},
}

var seedModelsByID = func() map[string]seedModelDefinition {
	byID := make(map[string]seedModelDefinition, len(seedModelDefinitions))
	for _, definition := range seedModelDefinitions {
		byID[strings.ToLower(definition.id)] = definition
	}
	return byID
}()

// FetchCatalog fetches and normalizes the live catalog for one usable account.
// It always contacts upstream; force is accepted for interface symmetry with
// Models, whose force flag bypasses its TTL check. A failed fetch never
// replaces the last good catalog.
func (p *Pool) FetchCatalog(ctx context.Context, force bool) ([]providers.Model, error) {
	if p == nil {
		return nil, errors.New("raccoon: pool is nil")
	}
	_ = force

	p.catalogFetchMu.Lock()
	defer p.catalogFetchMu.Unlock()
	return p.fetchCatalog(ctx)
}

// Models returns the pool catalog, honoring the configured TTL. force bypasses
// both the shared cache and the account's last-good stamp. A failed refresh
// keeps the account's last good catalog, then the shared cache, then the nine
// verified seed models; it therefore never blanks model discovery.
func (p *Pool) Models(ctx context.Context, force bool) ([]providers.Model, error) {
	if p == nil {
		return nil, errors.New("raccoon: pool is nil")
	}

	if !force {
		if cached := p.freshCatalog(); cached != nil {
			return wireCatalogModels(cached, p.timeNow().Unix()), nil
		}
	}

	// Serialize fetches so concurrent stale reads do not stampede upstream.
	// Re-check after acquiring the lock: another request may have just filled
	// the cache while this one waited.
	p.catalogFetchMu.Lock()
	defer p.catalogFetchMu.Unlock()
	if !force {
		if cached := p.freshCatalog(); cached != nil {
			return wireCatalogModels(cached, p.timeNow().Unix()), nil
		}
	}

	models, err := p.fetchCatalog(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if lastGood := p.lastGoodCatalog(); lastGood != nil {
			return wireCatalogModels(lastGood, p.timeNow().Unix()), nil
		}
		if cached := p.cachedCatalog(); cached != nil {
			return wireCatalogModels(cached, p.timeNow().Unix()), nil
		}
		return wireCatalogModels(seedCatalogModels(), p.timeNow().Unix()), nil
	}
	return wireCatalogModels(models, p.timeNow().Unix()), nil
}

// fetchCatalog performs one upstream fetch. The caller holds catalogFetchMu.
func (p *Pool) fetchCatalog(ctx context.Context) ([]providers.Model, error) {
	client := p.HTTPClient()
	if client == nil {
		return nil, errors.New("raccoon: catalog http client is not configured")
	}
	account, err := p.catalogAccount()
	if err != nil {
		return nil, err
	}
	tokens := p.TokenManager()
	if tokens != nil {
		// v1 ensures the token before the request; a transient failure while
		// the current token is still valid is already swallowed by Ensure.
		if err := tokens.Ensure(ctx, account); err != nil {
			return nil, err
		}
	}

	payload, generation, err := p.requestCatalog(ctx, account, client, tokens)
	if err != nil {
		if !IsAuthError(err) || tokens == nil {
			return nil, err
		}
		// A 401/403 triggers exactly one forced refresh for the credential
		// generation that produced the rejected bearer token, then one retry.
		if refreshErr := tokens.RefreshIfGeneration(ctx, account, generation); refreshErr != nil {
			return nil, refreshErr
		}
		payload, _, err = p.requestCatalog(ctx, account, client, tokens)
		if err != nil {
			return nil, err
		}
	}

	catalog, err := normalizeCatalog(payload)
	if err != nil {
		return nil, err
	}
	fetchedAt := p.timeNow()

	p.catalogMu.Lock()
	p.catalog = catalogCache{
		models:       cloneModels(catalog.models),
		defaultModel: catalog.defaultModel,
		fetchedAt:    fetchedAt,
		source:       "raccoon",
	}
	p.catalogMu.Unlock()

	// The account snapshot is the pool's last-good fallback when a later forced
	// refresh fails. Mutating it follows Task 9's lock contract.
	account.mu.Lock()
	account.Models = cloneModels(catalog.models)
	account.mu.Unlock()

	return cloneModels(catalog.models), nil
}

// requestCatalog sends one catalog GET and returns the credential generation
// that produced its bearer token.
func (p *Pool) requestCatalog(
	ctx context.Context,
	account *Account,
	client *HTTPClient,
	tokens *TokenManager,
) (map[string]any, uint64, error) {
	generation := uint64(0)
	if tokens != nil {
		generation = tokens.Generation(account)
	}
	snapshot := account.refreshCredentials()
	if snapshot.Deleted {
		return nil, 0, ErrAccountDeleted
	}
	// A refresh may have rotated credentials between Generation and the header
	// snapshot. Pass the generation of the token actually sent so
	// RefreshIfGeneration does not repeat an already-completed refresh.
	if snapshot.Generation != generation {
		generation = snapshot.Generation
	}

	headers := catalogHeaders(snapshot.Auth)
	payload, err := client.DoJSON(
		ctx, http.MethodGet, CatalogPath, headers, nil, p.ProxyURL(account),
		authSecretValues(snapshot.Auth)...,
	)
	if err != nil {
		return nil, generation, err
	}
	return payload, generation, nil
}

// catalogAccount picks the first non-disabled, non-deleted account. Cooling and
// budget guards do not prevent a read-only catalog refresh, matching v1's
// model discovery behavior.
func (p *Pool) catalogAccount() (*Account, error) {
	for _, account := range p.Accounts() {
		account.mu.Lock()
		disabled := account.State.Disabled || account.State.ManualDisabled
		account.mu.Unlock()
		if disabled || account.isDeleted() {
			continue
		}
		return account, nil
	}
	return nil, ErrNoAccount
}

// freshCatalog returns a cached live catalog while its TTL is valid.
func (p *Pool) freshCatalog() []providers.Model {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	if p.catalog.source != "raccoon" || len(p.catalog.models) == 0 {
		return nil
	}
	if p.timeNow().Sub(p.catalog.fetchedAt) >= p.modelsTTL() {
		return nil
	}
	return cloneModels(p.catalog.models)
}

// cachedCatalog returns the shared catalog even after its TTL expired.
func (p *Pool) cachedCatalog() []providers.Model {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	if p.catalog.source != "raccoon" || len(p.catalog.models) == 0 {
		return nil
	}
	return cloneModels(p.catalog.models)
}

// lastGoodCatalog returns the first account's last successful catalog. The
// slice copy is made under Account.mu.
func (p *Pool) lastGoodCatalog() []providers.Model {
	for _, account := range p.Accounts() {
		account.mu.Lock()
		models := cloneModels(account.Models)
		account.mu.Unlock()
		if len(models) > 0 {
			return models
		}
	}
	return nil
}

// modelsTTL reads the configured cache lifetime. Tests and partially built
// pools may leave the field zero; v1's 600-second default then applies.
func (p *Pool) modelsTTL() time.Duration {
	p.mu.RLock()
	seconds := p.cfg.ModelsTTLSeconds
	p.mu.RUnlock()
	if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		seconds = defaultModelsTTLSeconds
	}
	return time.Duration(seconds * float64(time.Second))
}

func (p *Pool) timeNow() time.Time {
	if p != nil && p.now != nil {
		return p.now()
	}
	return time.Now()
}

// catalogHeaders mirrors v1's desktop-client headers. X-Org-Code is only sent
// for a real organization identity.
func catalogHeaders(auth Auth) http.Header {
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	if token := cleanToken(auth.AccessToken); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	headers.Set("X-Client-Platform", ClientPlatform)
	headers.Set("X-Client-Version", ClientVersion)
	headers.Set("X-Raccoon-Language", ClientLanguage)
	headers.Set("Cache-Control", "no-store")
	identity := strings.TrimSpace(auth.OfficeIdentity)
	if identity != "" &&
		!strings.EqualFold(identity, "personal") &&
		!strings.EqualFold(identity, "none") {
		headers.Set("X-Org-Code", identity)
	}
	return headers
}

// normalizeCatalog normalizes the first `chat` category of a model_catalog
// response. An empty or missing chat category is an upstream failure.
func normalizeCatalog(payload map[string]any) (normalizedCatalog, error) {
	if payload == nil {
		return normalizedCatalog{}, errors.New("raccoon: catalog response is empty")
	}
	data := payload
	if nested, ok := payload["data"].(map[string]any); ok {
		data = nested
	}

	var chat map[string]any
	if categories, ok := data["categories"].([]any); ok {
		for _, rawCategory := range categories {
			category, ok := rawCategory.(map[string]any)
			if !ok {
				continue
			}
			if strings.ToLower(strings.TrimSpace(catalogText(category["type"]))) != "chat" {
				continue
			}
			chat = category
			break
		}
	}

	models := make([]providers.Model, 0)
	seen := make(map[string]bool)
	if chat != nil {
		if items, ok := chat["models"].([]any); ok {
			for _, rawItem := range items {
				item, ok := rawItem.(map[string]any)
				if !ok {
					continue
				}
				id := catalogTruthyText(item["name"])
				if strings.TrimSpace(id) == "" {
					id = catalogTruthyText(item["model_name"])
				}
				if strings.TrimSpace(id) == "" {
					id = catalogTruthyText(item["id"])
				}
				id = strings.TrimSpace(id)
				if id == "" || seen[strings.ToLower(id)] {
					continue
				}
				seen[strings.ToLower(id)] = true

				params, _ := item["params"].(map[string]any)
				seed, _ := seedModelsByID[strings.ToLower(id)]
				contextWindow := firstCatalogInt(
					params["context_window"],
					item["context_window"],
					params["context_length"],
					item["context_length"],
					params["max_input_tokens"],
					item["max_input_tokens"],
					seed.contextWindow,
				)
				maxTokens := firstCatalogInt(
					params["max_tokens"],
					item["max_tokens"],
					params["max_output_tokens"],
					item["max_output_tokens"],
					params["max_completion_tokens"],
					item["max_completion_tokens"],
					seed.maxTokens,
				)

				tagsValue, hasTags := item["tags"]
				if !hasTags {
					tagsValue = params["tags"]
				}
				tags := catalogTagList(tagsValue)

				display := catalogTruthyText(item["display_name"])
				if strings.TrimSpace(display) == "" {
					display = catalogTruthyText(item["description"])
				}
				display = truncateRunes(strings.TrimSpace(display), maxModelDisplayRunes)
				if display == "" {
					display = id
				}

				models = append(models, catalogCoreEntry(id, display, contextWindow, maxTokens, tags))
			}
		}
	}

	if len(models) == 0 {
		return normalizedCatalog{}, errors.New("raccoon: catalog returned no chat models")
	}

	defaultModel := ""
	if chat != nil {
		defaultModel = strings.TrimSpace(catalogTruthyText(chat["default_model"]))
		if defaultModel == "" {
			defaultModel = strings.TrimSpace(catalogTruthyText(chat["defaultModel"]))
		}
	}
	if defaultModel == "" {
		defaultModel, _ = models[0]["id"].(string)
	}
	return normalizedCatalog{models: models, defaultModel: defaultModel}, nil
}

// catalogCoreEntry is the normalized provider catalog item. Models formats
// these into the OpenAI-style entry v1 serves; FetchCatalog returns this shape.
func catalogCoreEntry(
	id, name string,
	contextWindow, maxTokens int64,
	tags []string,
) providers.Model {
	if tags == nil {
		tags = []string{}
	}
	return providers.Model{
		"id":             id,
		"name":           name,
		"context_window": contextWindow,
		"max_tokens":     maxTokens,
		"tags":           tags,
		"vision":         containsCatalogTag(tags, "vision"),
		"reasoning":      containsCatalogTag(tags, "reasoning"),
	}
}

// catalogModelEntry builds one OpenAI-style entry with every capability
// spelling v1 clients read.
func catalogModelEntry(
	id, name string,
	contextWindow, maxTokens int64,
	tags []string,
	created int64,
) providers.Model {
	entry := providers.Model{
		"id":       id,
		"object":   "model",
		"created":  created,
		"owned_by": "sensetime",
		"provider": "raccoon",
		"name":     name,
	}
	if contextWindow > 0 {
		entry["context_window"] = contextWindow
		entry["context_length"] = contextWindow
		entry["max_input_tokens"] = contextWindow
	}
	if maxTokens > 0 {
		entry["max_tokens"] = maxTokens
		entry["max_output_tokens"] = maxTokens
		entry["max_completion_tokens"] = maxTokens
		entry["max_output"] = maxTokens
	}

	vision := containsCatalogTag(tags, "vision")
	reasoning := containsCatalogTag(tags, "reasoning")
	entry["capabilities"] = map[string]any{
		"vision":     vision,
		"tool_calls": true,
		"reasoning":  reasoning,
	}
	entry["supports_vision"] = vision
	entry["supports_images"] = vision
	entry["supports_tool_calls"] = true
	entry["supports_reasoning"] = reasoning
	entry["vision"] = vision
	entry["multimodal"] = vision
	entry["abilities"] = map[string]any{
		"vision":        vision,
		"functionCall":  true,
		"function_call": true,
		"reasoning":     reasoning,
	}

	inputs := []string{"text"}
	if vision {
		inputs = append(inputs, "image")
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
	if len(tags) > 0 {
		entry["tags"] = append([]string(nil), tags...)
	}
	return entry
}

// seedCatalogModels returns a fresh copy of the nine verified v1 fallbacks.
func seedCatalogModels() []providers.Model {
	models := make([]providers.Model, 0, len(seedModelDefinitions))
	for _, definition := range seedModelDefinitions {
		models = append(models, catalogCoreEntry(
			definition.id,
			definition.name,
			definition.contextWindow,
			definition.maxTokens,
			definition.tags,
		))
	}
	return models
}

// wireCatalogModels formats normalized catalog items into the v1 `/v1/models`
// entries. created is the discovery timestamp, matching v1's per-call value.
func wireCatalogModels(models []providers.Model, created int64) []providers.Model {
	entries := make([]providers.Model, 0, len(models))
	for _, item := range models {
		id, _ := item["id"].(string)
		name, _ := item["name"].(string)
		tags := catalogTagList(item["tags"])
		entries = append(entries, catalogModelEntry(
			id,
			name,
			catalogInt(item["context_window"]),
			catalogInt(item["max_tokens"]),
			tags,
			created,
		))
	}
	return entries
}

func containsCatalogTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

func catalogTagList(value any) []string {
	raw := make([]string, 0)
	switch typed := value.(type) {
	case string:
		raw = strings.FieldsFunc(typed, func(r rune) bool {
			return r == ',' || unicode.IsSpace(r)
		})
	case []string:
		raw = typed
	case []any:
		for _, item := range typed {
			raw = append(raw, catalogTruthyText(item))
		}
	}

	tags := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		text := strings.ToLower(strings.TrimSpace(item))
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		tags = append(tags, text)
	}
	return tags
}

func firstCatalogInt(values ...any) int64 {
	for _, value := range values {
		if number := catalogInt(value); number > 0 {
			return number
		}
	}
	return 0
}

// catalogInt mirrors v1 _int_value: booleans and invalid values are zero,
// float strings are truncated, and negative values clamp to zero.
func catalogInt(value any) int64 {
	switch typed := value.(type) {
	case nil, bool:
		return 0
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return clampCatalogInt(number)
		}
		if number, err := typed.Float64(); err == nil {
			return catalogIntFromFloat(number)
		}
	case float64:
		return catalogIntFromFloat(typed)
	case float32:
		return catalogIntFromFloat(float64(typed))
	case int:
		return clampCatalogInt(int64(typed))
	case int8:
		return clampCatalogInt(int64(typed))
	case int16:
		return clampCatalogInt(int64(typed))
	case int32:
		return clampCatalogInt(int64(typed))
	case int64:
		return clampCatalogInt(typed)
	case uint:
		return clampCatalogInt(int64(typed))
	case uint8:
		return clampCatalogInt(int64(typed))
	case uint16:
		return clampCatalogInt(int64(typed))
	case uint32:
		return clampCatalogInt(int64(typed))
	case uint64:
		if typed > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(typed)
	case string:
		text := strings.TrimSpace(typed)
		if number, err := strconv.ParseInt(text, 10, 64); err == nil {
			return clampCatalogInt(number)
		}
		if number, err := strconv.ParseFloat(text, 64); err == nil {
			return catalogIntFromFloat(number)
		}
	}
	return 0
}

func catalogIntFromFloat(number float64) int64 {
	if math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
		return 0
	}
	if number >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(number)
}

func clampCatalogInt(number int64) int64 {
	if number < 0 {
		return 0
	}
	return number
}

// catalogText converts JSON scalar values to the text v1's str() would use.
func catalogText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

// catalogTruthyText mirrors Python's `value or ""` for optional string fields:
// nils, false, zero numbers, and empty strings are treated as absent.
func catalogTruthyText(value any) string {
	text := catalogText(value)
	switch typed := value.(type) {
	case nil:
		return ""
	case bool:
		if !typed {
			return ""
		}
	case string:
		return typed
	case json.Number:
		if number, err := typed.Float64(); err == nil && number == 0 {
			return ""
		}
	case float64:
		if typed == 0 {
			return ""
		}
	case float32:
		if typed == 0 {
			return ""
		}
	case int:
		if typed == 0 {
			return ""
		}
	case int64:
		if typed == 0 {
			return ""
		}
	case uint64:
		if typed == 0 {
			return ""
		}
	}
	return text
}

// cloneModels deep-copies catalog entries, including nested capability maps and
// tag slices, so callers cannot mutate the shared cache or account snapshot.
func cloneModels(models []providers.Model) []providers.Model {
	if len(models) == 0 {
		return nil
	}
	cloned := make([]providers.Model, len(models))
	for index, model := range models {
		entry := make(providers.Model, len(model))
		for key, value := range model {
			entry[key] = cloneCatalogValue(value)
		}
		cloned[index] = entry
	}
	return cloned
}

func cloneCatalogValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, nested := range typed {
			cloned[key] = cloneCatalogValue(nested)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	case []any:
		cloned := make([]any, len(typed))
		for index, nested := range typed {
			cloned[index] = cloneCatalogValue(nested)
		}
		return cloned
	default:
		return value
	}
}
