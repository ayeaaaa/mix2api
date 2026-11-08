package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	catalogDefaultBenefitHost = "https://opengw.developer.huaweicloud.com"
	catalogHTTPTimeout        = 20 * time.Second
	catalogMaxBodyBytes       = 1 << 20
	catalogErrorMessageRunes  = 500
)

const (
	catalogEndpointBuiltin     = "/v1/model/builtin"
	catalogEndpointAgentList   = "/v1/agent-center/agents/useragents"
	catalogEndpointAgentDetail = "/v1/agent-center/agents/detail"
	catalogEndpointBenefit     = "/api/v1/gateway/config"
	catalogEndpointClaim       = "/api/v1/benefit/claim"
)

// seedKnownModels is v1's cold-start catalogue. It is only used when an
// account has never had a successful discovery and every source is empty or
// unreachable.
var seedKnownModels = []string{
	"GLM-5.2", "GLM-5.2-ArkTS-SPARK", "GLM-5.3",
	"GLM-5.1", "GLM-4.7",
	"Kimi-K3.1-Pro", "Kimi-K3",
	"DeepSeek-V4.1-Flash", "DeepSeek-V4.1-Flash-1017",
	"OpenPangu-2.0-Pro", "OpenPangu-2.0-Flash",
	"Qwen3-VL-235B", "Qwen3.5-397B-A17B-VL", "Qwen3.6-27B-VL",
}

var seedBenefitModels = []string{
	"deepseek-v4-flash-0731", "deepseek-v4-pro-0813", "glm-5.3-flash",
}

var seedModels = func() []string {
	out := append([]string(nil), seedKnownModels...)
	return append(out, seedBenefitModels...)
}()

// codeartsOfficialModels is the published Huawei Cloud console catalogue.
// Discovery only returns an account-scoped subset, so the official list stays
// visible for the panel and for clients that do not own the discovered models.
var codeartsOfficialModels = []string{
	"Kimi-K3.1-Pro", "GLM-5.3-Flash", "Kimi-K3",
	"DeepSeek-V4.1-Flash", "DeepSeek-V4.1-Flash-1017",
	"GLM-5.3", "GLM-5.2", "OpenPangu-2.0-Flash",
	"GLM-5.2-ArkTS-SPARK", "OpenPangu-2.0-Pro",
}

// codeartsCredits is the published multiplier per model. It is metadata for
// the panel and does not change token accounting.
var codeartsCredits = map[string]string{
	"kimi-k3.1-pro":       "1.63",
	"glm-5.3-flash":       "0.08",
	"kimi-k3":             "2.38",
	"glm-5.3":             "0.79",
	"glm-5.2":             "0.7",
	"openpangu-2.0-flash": "0.32",
	"glm-5.2-arkts-spark": "0.7",
	"openpangu-2.0-pro":   "0.7",
}

// codeartsPeakCredits holds time-of-day rates: (peak, off-peak). Peak is
// 08:00-20:59 local time; off-peak is 21:00-07:59.
var codeartsPeakCredits = map[string][2]string{
	"deepseek-v4.1-flash":      {"0.18", "0.09"},
	"deepseek-v4.1-flash-1017": {"0.18", "0.12"},
}

// reasoningEfforts is the upstream-validated reasoning level list per model.
var reasoningEfforts = map[string][]string{
	"deepseek-v4.1-flash":      {"none", "low", "medium", "high", "xhigh", "max"},
	"deepseek-v4.1-flash-1017": {"none", "low", "medium", "high", "xhigh", "max"},
	"glm-5.2":                  {"none", "minimal", "low", "medium", "high", "xhigh", "max"},
	"glm-5.2-sft-harmony":      {"none", "minimal", "low", "medium", "high", "xhigh", "max"},
	"glm-5.3":                  {"none", "minimal", "low", "medium", "high", "xhigh", "max"},
	"glm-5.3-flash":            {"low", "high", "max"},
}

// knownVisionIDs is deliberately CodeArts-specific: overlapping ids may be
// multimodal in another catalogue while the CodeArts backend rejects images.
var knownVisionIDs = map[string]bool{
	"qwen3-vl-235b":        true,
	"qwen3.5-397b-a17b-vl": true,
	"qwen3.6-27b-vl":       true,
	"kimi-k2.6-vl":         true,
}

// workbuddyFallbackLimits mirrors the v1 WorkBuddy catalogue lookup used when
// CodeArts' builtin list omits a model's limits. It is a local table rather
// than a WorkBuddy import so the CodeArts provider stays self-contained.
var workbuddyFallbackLimits = map[string][2]int64{
	"glm-5.2":             {1000000, 48000},
	"glm-5.3":             {1000000, 48000},
	"glm-5.3-flash":       {1000000, 32000},
	"glm-5.1":             {200000, 48000},
	"kimi-k3":             {1000000, 32000},
	"kimi-k3.1-pro":       {1000000, 32000},
	"deepseek-v4.1-flash": {1000000, 128000},
}

// canonicalStaticModels mirrors v1's local alias table for ids that do not
// appear in a discovered catalogue.
var canonicalStaticModels = map[string]string{
	"snap-chat":            "GLM-5.2",
	"glm-5.2":              "GLM-5.2",
	"glm-5.1":              "GLM-5.1",
	"glm-4.7":              "GLM-4.7",
	"qwen3-vl-235b":        "Qwen3-VL-235B",
	"qwen3.5-397b-a17b-vl": "Qwen3.5-397B-A17B-VL",
	"qwen3.6-27b-vl":       "Qwen3.6-27B-VL",
}

func benefitHost() string {
	return strings.TrimRight(envString("WB_CODEARTS_BENEFIT_HOST", catalogDefaultBenefitHost), "/")
}

func knownVision(id string) bool {
	return knownVisionIDs[strings.ToLower(strings.TrimSpace(id))]
}

func benefitSeedSet() map[string]bool {
	out := make(map[string]bool, len(seedBenefitModels))
	for _, id := range seedBenefitModels {
		out[strings.ToLower(id)] = true
	}
	return out
}

func officialCoreModels() []Model {
	benefit := benefitSeedSet()
	out := make([]Model, 0, len(codeartsOfficialModels))
	for _, id := range codeartsOfficialModels {
		out = append(out, Model{
			"id":              id,
			"name":            id,
			"benefit":         benefit[strings.ToLower(id)],
			"supports_images": knownVision(id),
		})
	}
	return out
}

func benefitSeedCoreModels() []Model {
	out := make([]Model, 0, len(seedBenefitModels))
	for _, id := range seedBenefitModels {
		out = append(out, Model{
			"id":              id,
			"name":            id,
			"benefit":         true,
			"supports_images": knownVision(id),
		})
	}
	return out
}

func seedFallbackModels() []Model {
	benefit := benefitSeedSet()
	out := make([]Model, 0, len(seedModels))
	for _, id := range seedModels {
		out = append(out, Model{
			"id":              id,
			"name":            id,
			"benefit":         benefit[strings.ToLower(id)],
			"supports_images": knownVision(id),
		})
	}
	return out
}

// modelCatalog is an insertion-ordered, case-insensitive core catalogue union.
type modelCatalog struct {
	order []string
	items map[string]Model
}

func newModelCatalog() *modelCatalog {
	return &modelCatalog{items: make(map[string]Model)}
}

func (c *modelCatalog) add(models []Model) {
	for _, item := range models {
		c.merge(item)
	}
}

func (c *modelCatalog) merge(item Model) {
	id := strings.TrimSpace(modelTruthyText(item["id"]))
	if id == "" {
		return
	}
	key := strings.ToLower(id)
	current, exists := c.items[key]
	if !exists {
		c.items[key] = normalizeCoreModel(item, id)
		c.order = append(c.order, key)
		return
	}

	if modelInt(current["context_window"]) == 0 {
		if value := modelInt(item["context_window"]); value > 0 {
			current["context_window"] = value
		}
	}
	if modelInt(current["max_tokens"]) == 0 {
		if value := modelInt(item["max_tokens"]); value > 0 {
			current["max_tokens"] = value
		}
	}
	if got, _ := current["benefit"].(bool); !got {
		if value, _ := item["benefit"].(bool); value {
			current["benefit"] = true
		}
	}
	if strings.TrimSpace(modelText(current["desc"])) == "" {
		if value := strings.TrimSpace(modelText(item["desc"])); value != "" {
			current["desc"] = value
		}
	}
	if strings.TrimSpace(modelText(current["model_category"])) == "" {
		if value := strings.TrimSpace(modelText(item["model_category"])); value != "" {
			current["model_category"] = value
		}
	}
	if strings.TrimSpace(modelText(current["credits"])) == "" {
		if value := strings.TrimSpace(modelText(item["credits"])); value != "" {
			current["credits"] = value
		}
	}

	_, currentHasVision := current["supports_images"].(bool)
	incomingVision, incomingHasVision := item["supports_images"].(bool)
	if incomingHasVision && (incomingVision || !currentHasVision) {
		// A true from any source wins. A false is useful only while no source
		// has asserted vision support.
		current["supports_images"] = incomingVision
	}
}

func normalizeCoreModel(item Model, id string) Model {
	name := strings.TrimSpace(modelText(item["name"]))
	if name == "" {
		name = id
	}
	out := Model{
		"id":              id,
		"name":            name,
		"context_window":  modelInt(item["context_window"]),
		"max_tokens":      modelInt(item["max_tokens"]),
		"desc":            strings.TrimSpace(modelTruthyText(item["desc"])),
		"benefit":         modelBool(item["benefit"]),
		"supports_images": nil,
		"model_category":  strings.TrimSpace(modelTruthyText(item["model_category"])),
		"credits":         strings.TrimSpace(modelTruthyText(item["credits"])),
	}
	if value, ok := item["supports_images"].(bool); ok {
		out["supports_images"] = value
	}
	return out
}

func mergeModelCatalogs(sets ...[]Model) []Model {
	union := newModelCatalog()
	for _, set := range sets {
		union.add(set)
	}
	out := make([]Model, 0, len(union.order))
	for _, key := range union.order {
		out = append(out, cloneModel(union.items[key]))
	}
	return out
}

func cloneModels(models []Model) []Model {
	if len(models) == 0 {
		return nil
	}
	out := make([]Model, len(models))
	for index, item := range models {
		out[index] = cloneModel(item)
	}
	return out
}

func cloneModel(model Model) Model {
	if model == nil {
		return nil
	}
	out := make(Model, len(model))
	for key, value := range model {
		out[key] = cloneModelValue(value)
	}
	return out
}

func cloneModelValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, nested := range typed {
			out[key] = cloneModelValue(nested)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]any, len(typed))
		for index, nested := range typed {
			out[index] = cloneModelValue(nested)
		}
		return out
	default:
		return value
	}
}

// modelText converts a decoded JSON scalar to the text Python's str(value)
// would produce. Maps and slices intentionally yield an empty string because
// every catalogue id/name/description field is scalar.
func modelText(value any) string {
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
		return ""
	}
}

// modelTruthyText mirrors Python's `value or ""` for optional catalogue
// fields: nil, false, zero numbers, and empty strings are absent.
func modelTruthyText(value any) string {
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
	case int8:
		if typed == 0 {
			return ""
		}
	case int16:
		if typed == 0 {
			return ""
		}
	case int32:
		if typed == 0 {
			return ""
		}
	case int64:
		if typed == 0 {
			return ""
		}
	case uint:
		if typed == 0 {
			return ""
		}
	case uint8:
		if typed == 0 {
			return ""
		}
	case uint16:
		if typed == 0 {
			return ""
		}
	case uint32:
		if typed == 0 {
			return ""
		}
	case uint64:
		if typed == 0 {
			return ""
		}
	}
	return modelText(value)
}

func modelInt(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return clampModelInt(number)
		}
		if number, err := typed.Float64(); err == nil {
			return modelIntFromFloat(number)
		}
	case float64:
		return modelIntFromFloat(typed)
	case float32:
		return modelIntFromFloat(float64(typed))
	case int:
		return clampModelInt(int64(typed))
	case int8:
		return clampModelInt(int64(typed))
	case int16:
		return clampModelInt(int64(typed))
	case int32:
		return clampModelInt(int64(typed))
	case int64:
		return clampModelInt(typed)
	case uint:
		return clampModelInt(int64(typed))
	case uint8:
		return clampModelInt(int64(typed))
	case uint16:
		return clampModelInt(int64(typed))
	case uint32:
		return clampModelInt(int64(typed))
	case uint64:
		if typed > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(typed)
	case string:
		text := strings.TrimSpace(typed)
		if number, err := strconv.ParseInt(text, 10, 64); err == nil {
			return clampModelInt(number)
		}
		if number, err := strconv.ParseFloat(text, 64); err == nil {
			return modelIntFromFloat(number)
		}
	}
	return 0
}

func modelIntFromFloat(number float64) int64 {
	if math.IsNaN(number) || number <= 0 {
		return 0
	}
	if number >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(number)
}

func clampModelInt(number int64) int64 {
	if number < 0 {
		return 0
	}
	return number
}

func modelBool(value any) bool {
	result, _ := value.(bool)
	return result
}

// modelTruthy mirrors Python truthiness for boolean-ish upstream flags.
func modelTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		number, err := typed.Float64()
		return err == nil && number != 0
	case float64:
		return typed != 0
	case float32:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case uint64:
		return typed != 0
	default:
		return false
	}
}

func modelMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func modelList(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []map[string]any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = item
		}
		return out
	default:
		return nil
	}
}

func creditLabel(rows any) string {
	list, ok := rows.([]any)
	if !ok {
		return ""
	}
	for _, raw := range list {
		row := modelMap(raw)
		if row == nil {
			continue
		}
		slot := strings.TrimSpace(modelText(row["time_slot_id"]))
		if slot != "" && slot != "default" {
			continue
		}
		text := strings.ToLower(strings.TrimSpace(modelText(row["ratio_display"])))
		if !strings.HasSuffix(text, "x") {
			continue
		}
		if _, err := strconv.ParseFloat(strings.TrimSuffix(text, "x"), 64); err != nil {
			continue
		}
		return "x" + strings.TrimSuffix(text, "x")
	}
	return ""
}

// FetchModels refreshes one account's live agent-center, builtin, and benefit
// catalogues in parallel and merges the official console catalogue. A valid
// response with zero live models still produces the official catalogue; only
// a total source failure retains the previous account catalogue, and the cold
// seed is installed only when such a failure also has no catalog/prior. TTL
// and failure backoff are honored unless force is set. Concurrent discovery
// for one account is coalesced.
func (p *Provider) FetchModels(ctx context.Context, a *Account, force bool) ([]Model, error) {
	return p.ensureModels(ctx, a, force)
}

// ensureModels performs discovery under the account lock. Non-forced callers
// return a fresh catalogue or a recent last-good result during failure backoff
// without contacting upstream. Concurrent callers join the current flight.
func (p *Provider) ensureModels(ctx context.Context, a *Account, force bool) ([]Model, error) {
	if p == nil {
		return nil, errors.New("codearts: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("codearts: model discovery requires a context")
	}
	if a == nil {
		return nil, errors.New("codearts: model discovery requires an account")
	}

	a.modelsMu.Lock()
	if flight := a.modelsFlight; flight != nil {
		a.modelsMu.Unlock()
		if p.modelFetchJoinHook != nil {
			p.modelFetchJoinHook()
		}
		return waitModelFetch(ctx, flight)
	}
	if !force {
		models, fetchedAt, attemptedAt := accountModelsSnapshot(a)
		if len(models) > 0 && p.catalogFresh(catalogLastAttempt(fetchedAt, attemptedAt)) {
			a.modelsMu.Unlock()
			return models, nil
		}
	}

	// Capture the publish epoch while the flight lock is held. A re-import
	// takes the same lock, so it either invalidates this flight or increments
	// the epoch before the fetch can publish a stale catalogue.
	a.mu.Lock()
	epoch := a.modelsEpoch
	a.mu.Unlock()
	flight := &modelFetchFlight{done: make(chan struct{}), epoch: epoch}
	a.modelsFlight = flight
	a.modelsMu.Unlock()

	models, err := p.fetchModelsLocked(ctx, a, epoch)

	a.modelsMu.Lock()
	flight.models = cloneModels(models)
	flight.err = err
	if a.modelsFlight == flight {
		a.modelsFlight = nil
	}
	close(flight.done)
	a.modelsMu.Unlock()
	return models, err
}

// modelFetchFlight coalesces concurrent discovery for one account. The leader
// publishes its result and closes done before removing the flight from the
// account. The epoch ties the result to the credential generation that started
// the fetch; a re-import clears the flight and advances the epoch so a late
// fetch cannot repopulate the cleared account.
type modelFetchFlight struct {
	done   chan struct{}
	models []Model
	err    error
	epoch  uint64
}

func waitModelFetch(ctx context.Context, flight *modelFetchFlight) ([]Model, error) {
	select {
	case <-flight.done:
		return cloneModels(flight.models), flight.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *Provider) fetchModelsLocked(ctx context.Context, a *Account, epoch uint64) ([]Model, error) {
	// A token refresh failure does not invalidate the current signed
	// credential pair for a read-only catalogue request.
	if p.tokenManager != nil {
		_ = p.tokenManager.Ensure(ctx, a)
	}

	type sourceResult struct {
		name  string
		items []Model
		err   error
	}
	results := make([]sourceResult, 3)
	var wait sync.WaitGroup
	wait.Add(3)
	go func() {
		defer wait.Done()
		items, err := p.fetchAgentModels(ctx, a)
		results[0] = sourceResult{name: "agent", items: items, err: err}
	}()
	go func() {
		defer wait.Done()
		items, err := p.fetchBuiltinModels(ctx, a)
		results[1] = sourceResult{name: "builtin", items: items, err: err}
	}()
	go func() {
		defer wait.Done()
		items, err := p.fetchBenefitModels(ctx, a)
		results[2] = sourceResult{name: "benefit", items: items, err: err}
	}()
	wait.Wait()

	sets := make([][]Model, 0, 3)
	failures := make([]error, 0, 3)
	successes := 0
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, fmt.Errorf("codearts: model discovery %s: %w", result.name, result.err))
			continue
		}
		successes++
		if len(result.items) > 0 {
			sets = append(sets, result.items)
		}
	}
	now := p.catalogNow()

	if successes > 0 {
		// A successful source is valid even when it returns no models. A true
		// empty response keeps the last-good account catalogue as its base; an
		// account with no prior gets the official catalogue, never the wider
		// cold seed.
		base := mergeModelCatalogs(sets...)
		if len(sets) == 0 {
			prior, _, _ := accountModelsSnapshot(a)
			if len(prior) > 0 {
				base = prior
			}
		}
		merged := mergeModelCatalogs(base, officialCoreModels())
		p.storeAccountModels(a, merged, now, epoch)
		return cloneModels(merged), nil
	}

	failure := errors.Join(failures...)
	// Every source failed. Keep the last good catalogue; only an account that
	// has genuinely never had a catalogue falls back to the cold seed.
	prior, _, _ := accountModelsSnapshot(a)
	if len(prior) > 0 {
		p.noteModelAttempt(a, now, epoch)
		return prior, failure
	}
	seed := seedFallbackModels()
	p.storeAccountModels(a, seed, now, epoch)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return cloneModels(seed), ctxErr
	}
	return cloneModels(seed), failure
}

// storeAccountModels publishes a successful discovery (or seed fallback). It
// refuses when a re-import advanced the epoch while the fetch was running.
func (p *Provider) storeAccountModels(a *Account, models []Model, fetchedAt time.Time, epoch uint64) bool {
	if a == nil {
		return false
	}
	copied := cloneModels(models)
	benefit := make(map[string]bool, len(copied))
	for _, item := range copied {
		id := strings.TrimSpace(modelTruthyText(item["id"]))
		if id == "" {
			continue
		}
		benefit[strings.ToLower(id)] = modelBool(item["benefit"])
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.removed || a.modelsEpoch != epoch {
		return false
	}
	a.Models = copied
	a.modelBenefit = benefit
	a.modelsFetchedAt = fetchedAt
	a.modelsAttemptedAt = fetchedAt
	return true
}

// noteModelAttempt records a failed refresh for the TTL/backoff window without
// replacing the last-good catalogue. A re-import invalidates the attempt too.
func (p *Provider) noteModelAttempt(a *Account, attemptedAt time.Time, epoch uint64) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.removed || a.modelsEpoch != epoch {
		return false
	}
	a.modelsAttemptedAt = attemptedAt
	return true
}

func accountModelsSnapshot(a *Account) ([]Model, time.Time, time.Time) {
	if a == nil {
		return nil, time.Time{}, time.Time{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneModels(a.Models), a.modelsFetchedAt, a.modelsAttemptedAt
}

func catalogLastAttempt(fetchedAt, attemptedAt time.Time) time.Time {
	if attemptedAt.After(fetchedAt) {
		return attemptedAt
	}
	return fetchedAt
}

func (p *Provider) catalogFresh(lastAttempt time.Time) bool {
	if lastAttempt.IsZero() {
		return false
	}
	age := p.catalogNow().Sub(lastAttempt)
	return age >= 0 && age < p.modelsTTL()
}

func (p *Provider) catalogNow() time.Time {
	if p != nil && p.pool != nil {
		return p.pool.nowTime()
	}
	return time.Now()
}

func (p *Provider) catalogTimeout() time.Duration {
	timeout := catalogHTTPTimeout
	if p != nil && p.catalogHTTPTimeout > 0 {
		timeout = p.catalogHTTPTimeout
	}
	if p != nil && p.pool != nil {
		p.pool.mu.RLock()
		configured := p.pool.cfg.TimeoutSeconds
		p.pool.mu.RUnlock()
		if configured > 0 && !math.IsNaN(configured) && !math.IsInf(configured, 0) {
			if candidate := time.Duration(configured * float64(time.Second)); candidate > 0 && candidate < timeout {
				timeout = candidate
			}
		}
	}
	return timeout
}

func (p *Provider) fetchAgentModels(ctx context.Context, a *Account) ([]Model, error) {
	query := url.Values{}
	query.Set("offset", "0")
	query.Set("limit", "100")
	query.Set("is_primary_agent", "true")
	listing, err := p.catalogJSON(ctx, a, http.MethodGet,
		snapHost()+catalogEndpointAgentList+"?"+query.Encode(), nil, "PromptCenter")
	if err != nil {
		return nil, err
	}
	agents := modelList(listing["agents"])
	agentID := selectAgentID(agents)
	if agentID == "" {
		return nil, nil
	}

	detailQuery := url.Values{}
	detailQuery.Set("agent_id", agentID)
	detail, err := p.catalogJSON(ctx, a, http.MethodGet,
		snapHost()+catalogEndpointAgentDetail+"?"+detailQuery.Encode(), nil, "PromptCenter")
	if err != nil {
		return nil, err
	}
	gpts := modelMap(detail["gpts"])
	items := modelList(gpts["models"])
	out := make([]Model, 0, len(items))
	for _, raw := range items {
		item := modelMap(raw)
		if item == nil {
			continue
		}
		params := modelMap(item["model_parameters"])
		mid := firstModelText(item["model_name"], item["model_id"], item["model_alias"], params["model_id"])
		if mid == "" {
			continue
		}
		supportsValue, hasSupports := params["supports_images"]
		if !hasSupports {
			supportsValue = item["supports_images"]
		}
		category := modelTruthyText(params["model_category"])
		if category == "" {
			category = modelTruthyText(item["model_category"])
		}
		entry := Model{
			"id":              mid,
			"name":            mid,
			"context_window":  modelInt(params["context_window"]),
			"max_tokens":      modelInt(params["max_tokens"]),
			"desc":            modelTruthyText(params["model_desc"]),
			"benefit":         false,
			"supports_images": nil,
			"model_category":  category,
		}
		if value, ok := supportsValue.(bool); ok {
			entry["supports_images"] = value
		}
		out = append(out, entry)
	}
	return out, nil
}

func selectAgentID(agents []any) string {
	for _, raw := range agents {
		agent := modelMap(raw)
		if agent == nil {
			continue
		}
		alias := modelMap(agent["alias"])
		if modelText(agent["agent_name"]) == "CodeAgent" &&
			modelText(alias["alias_zh_cn"]) == "智能体" &&
			modelTruthy(agent["show_in_ide"]) {
			if id := strings.TrimSpace(modelTruthyText(agent["agent_id"])); id != "" {
				return id
			}
		}
	}
	for _, raw := range agents {
		agent := modelMap(raw)
		if agent == nil || !modelTruthy(agent["is_primary_agent"]) {
			continue
		}
		if id := strings.TrimSpace(modelTruthyText(agent["agent_id"])); id != "" {
			return id
		}
	}
	for _, raw := range agents {
		agent := modelMap(raw)
		if agent == nil {
			continue
		}
		if id := strings.TrimSpace(modelTruthyText(agent["agent_id"])); id != "" {
			return id
		}
	}
	return ""
}

func (p *Provider) fetchBuiltinModels(ctx context.Context, a *Account) ([]Model, error) {
	data, err := p.catalogJSON(ctx, a, http.MethodGet,
		snapHost()+catalogEndpointBuiltin, nil, "PromptCenter")
	if err != nil {
		return nil, err
	}
	items := modelList(data["builtinModels"])
	out := make([]Model, 0, len(items))
	for _, raw := range items {
		item := modelMap(raw)
		if item == nil {
			continue
		}
		mid := firstModelText(item["model_id"], item["model_name"])
		if mid == "" {
			continue
		}
		entry := Model{
			"id":              mid,
			"name":            mid,
			"context_window":  modelInt(item["context_window"]),
			"max_tokens":      modelInt(item["max_tokens"]),
			"desc":            modelTruthyText(item["model_desc"]),
			"benefit":         false,
			"supports_images": nil,
			"model_category":  modelTruthyText(item["model_category"]),
			"credits":         creditLabel(item["credit"]),
		}
		if value, ok := item["supports_images"].(bool); ok {
			entry["supports_images"] = value
		}
		out = append(out, entry)
	}
	return out, nil
}

func (p *Provider) fetchBenefitModels(ctx context.Context, a *Account) ([]Model, error) {
	if p.poolBenefitAutoClaim() {
		// Claim failures never block reading the benefit catalogue.
		_, _ = p.catalogJSON(ctx, a, http.MethodPost,
			benefitHost()+catalogEndpointClaim, map[string]any{}, "")
	}
	data, err := p.catalogJSON(ctx, a, http.MethodGet,
		benefitHost()+catalogEndpointBenefit, nil, "")
	if err != nil {
		return nil, err
	}
	result := modelMap(data["result"])
	items := modelList(result["models"])
	out := make([]Model, 0, len(items))
	for _, raw := range items {
		item := modelMap(raw)
		if item == nil {
			continue
		}
		mid := firstModelText(item["model_id"], item["model_name"])
		if mid == "" {
			continue
		}
		name := strings.TrimSpace(modelTruthyText(item["model_name"]))
		if name == "" {
			name = mid
		}
		entry := Model{
			"id":              mid,
			"name":            name,
			"context_window":  modelInt(item["context_window"]),
			"max_tokens":      modelInt(item["max_tokens"]),
			"desc":            modelTruthyText(item["model_desc"]),
			"benefit":         true,
			"supports_images": nil,
			"model_category":  modelTruthyText(item["model_category"]),
		}
		if value, ok := item["supports_images"].(bool); ok {
			entry["supports_images"] = value
		}
		out = append(out, entry)
	}
	return out, nil
}

// poolBenefitAutoClaim reads the live pool configuration. Provider.Config is a
// construction-time snapshot; Task 9 may update the pool config without
// rebuilding the provider.
func (p *Provider) poolBenefitAutoClaim() bool {
	if p == nil || p.pool == nil {
		return true
	}
	p.pool.mu.RLock()
	value := p.pool.cfg.BenefitAutoClaim
	p.pool.mu.RUnlock()
	return value
}

func firstModelText(values ...any) string {
	for _, value := range values {
		if text := strings.TrimSpace(modelTruthyText(value)); text != "" {
			return text
		}
	}
	return ""
}

// catalogJSON sends one Huawei-signed catalogue request through the shared
// transport and decodes a JSON object. The per-request timeout bounds every
// individual upstream call, including body reads.
func (p *Provider) catalogJSON(
	ctx context.Context,
	a *Account,
	method, rawURL string,
	payload map[string]any,
	agentType string,
) (map[string]any, error) {
	if p == nil || p.transport == nil {
		return nil, errors.New("codearts: model discovery transport is nil")
	}
	if ctx == nil {
		return nil, errors.New("codearts: model discovery requires a context")
	}
	if a == nil {
		return nil, errors.New("codearts: model discovery requires an account")
	}

	requestCtx, cancel := context.WithTimeout(ctx, p.catalogTimeout())
	defer cancel()

	var body []byte
	if method == http.MethodPost {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, errors.New("codearts: encode model discovery request failed")
		}
		body = encoded
	}

	request, err := http.NewRequestWithContext(requestCtx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("codearts: build model discovery request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Language", "zh-cn")
	if agentType != "" {
		request.Header.Set("Agent-Type", agentType)
	}

	a.mu.Lock()
	auth := a.Auth
	proxyURL := a.proxy
	a.mu.Unlock()

	credentials := SignCredentials{
		AccessKeyID:     auth.AccessKeyID,
		SecretAccessKey: auth.SecretAccessKey,
		SecurityToken:   auth.CloudDragonToken,
	}
	if err := SignRequest(request, body, credentials); err != nil {
		return nil, errors.New("codearts: sign model discovery request failed")
	}

	response, err := p.transport.Do(requestCtx, request, proxyURL)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr == context.Canceled {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("codearts: model discovery transport failed: %s",
			catalogSafeError(err.Error(), auth))
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, catalogMaxBodyBytes))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr == context.Canceled {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("codearts: read model discovery response failed: %s",
			catalogSafeError(err.Error(), auth))
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("codearts: model discovery HTTP %d: %s",
			response.StatusCode, catalogSafeError(catalogErrorText(raw), auth))
	}

	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil || decoded == nil {
		return nil, fmt.Errorf("codearts: model discovery returned a non-JSON response (HTTP %d)", response.StatusCode)
	}
	return decoded, nil
}

func catalogErrorText(raw []byte) string {
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err == nil && decoded != nil {
		for _, key := range []string{"error_msg", "message", "error_description", "error"} {
			if text := strings.TrimSpace(modelText(decoded[key])); text != "" {
				return text
			}
		}
		for _, key := range []string{"error_code", "code"} {
			code := strings.TrimSpace(modelText(decoded[key]))
			if code != "" && code != "0" {
				return key + "=" + code
			}
		}
	}
	return strings.TrimSpace(string(raw))
}

func catalogSafeError(message string, auth Auth) string {
	values := codeartsSecretValues(auth)
	if strings.TrimSpace(auth.AccessKeyID) != "" {
		values = append(values, auth.AccessKeyID)
	}
	message = redactText(message, values)
	message = maskProxyURL(message)
	return truncateRunes(message, catalogErrorMessageRunes)
}
