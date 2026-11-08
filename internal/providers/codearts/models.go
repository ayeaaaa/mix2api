package codearts

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const canonicalOneZeroOneSeven = "deepseek-v4.1-flash-1017"

// Models returns the provider-wide `/v1/models` catalogue: the union of every
// enabled account's last-good catalogue plus the published official and
// benefit-seed models. Disabled and manual-disabled accounts are skipped
// exactly like v1. Refreshes honor cfg.ModelsTTLSeconds (300 seconds by
// default); force bypasses the TTL. A failed refresh keeps the account's last
// good catalogue. Discovery failures never fail the whole catalogue.
func (p *Provider) Models(ctx context.Context, force bool) ([]Model, error) {
	if p == nil {
		return nil, errors.New("codearts: provider is nil")
	}
	if ctx == nil {
		return nil, errors.New("codearts: model catalog requires a context")
	}

	sets := make([][]Model, 0)
	if p.pool != nil {
		for _, account := range p.pool.Accounts() {
			account.mu.Lock()
			disabled := account.removed || account.State.Disabled || account.State.ManualDisabled
			prior := cloneModels(account.Models)
			fetchedAt := account.modelsFetchedAt
			account.mu.Unlock()
			if disabled {
				continue
			}
			// Match v1's model_entries: ensure the token before every refresh
			// decision. A refresh failure does not fail the catalogue.
			if p.tokenManager != nil {
				_ = p.tokenManager.Ensure(ctx, account)
			}

			models := prior
			if force || len(prior) == 0 || !p.catalogFresh(fetchedAt) {
				// FetchModels coalesces concurrent callers per account and
				// rechecks freshness under the account lock.
				refreshed, _ := p.ensureModels(ctx, account, force)
				if len(refreshed) > 0 {
					models = refreshed
				}
			}
			if len(models) > 0 {
				sets = append(sets, models)
			}
		}
	}

	merged := mergeModelCatalogs(sets...)
	merged = mergeModelCatalogs(merged, officialCoreModels(), benefitSeedCoreModels())
	if len(merged) == 0 {
		merged = seedFallbackModels()
	}
	return p.wireModels(merged), nil
}

// wireModels formats the merged core catalogue into the v1 `/v1/models` wire
// entries. Display IDs are preserved exactly; only CanonicalModel and the
// agent-picker helper lowercase them.
func (p *Provider) wireModels(models []Model) []Model {
	now := p.catalogNow()
	created := now.Unix()
	out := make([]Model, 0, len(models))
	for _, item := range models {
		id := strings.TrimSpace(modelText(item["id"]))
		if id == "" {
			continue
		}
		name := strings.TrimSpace(modelTruthyText(item["name"]))
		if name == "" {
			name = id
		}
		wireID := id
		if strings.EqualFold(id, canonicalOneZeroOneSeven) {
			// The upstream-observed live spelling is lowercase, but the v1
			// /v1/models contract advertises the display id. Chat and the agent
			// picker use the lowercase alias instead.
			wireID = "DeepSeek-V4.1-Flash-1017"
			if strings.EqualFold(name, canonicalOneZeroOneSeven) {
				name = wireID
			}
		}
		entry := Model{
			"id":       wireID,
			"object":   "model",
			"created":  created,
			"owned_by": "codearts",
			"provider": "codearts",
			"name":     name,
		}

		contextWindow := modelInt(item["context_window"])
		maxTokens := modelInt(item["max_tokens"])
		if contextWindow == 0 || maxTokens == 0 {
			fallback := workbuddyFallbackLimits[strings.ToLower(id)]
			if contextWindow == 0 {
				contextWindow = fallback[0]
			}
			if maxTokens == 0 {
				maxTokens = fallback[1]
			}
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
		if modelBool(item["benefit"]) {
			entry["benefit"] = true
		}

		rate, rateNote, peakRate, offpeakRate := codeartsCreditInfo(id, now)
		switch {
		case rate != "":
			entry["credits"] = "x" + rate
			entry["credits_peak"] = "x" + peakRate
			entry["credits_offpeak"] = "x" + offpeakRate
			if rateNote != "" {
				entry["credits_note"] = rateNote
			}
		case strings.TrimSpace(modelText(item["credits"])) != "":
			entry["credits"] = strings.TrimSpace(modelText(item["credits"]))
		}

		supports, hasSupports := item["supports_images"].(bool)
		if !hasSupports {
			supports = knownVision(id)
		}
		vision := supports
		efforts := reasoningEfforts[strings.ToLower(id)]
		reasoning := len(efforts) > 0
		inputs := []string{"text"}
		if vision {
			inputs = append(inputs, "image")
		}

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
		if description := strings.TrimSpace(modelText(item["desc"])); description != "" {
			entry["description"] = description
		}
		if category := strings.TrimSpace(modelText(item["model_category"])); category != "" {
			entry["category"] = category
		}
		if reasoning {
			entry["reasoning_efforts"] = append([]string(nil), efforts...)
		}
		if strings.EqualFold(id, "glm-5.3-flash") {
			entry["reasoning_can_disable"] = false
			entry["always_reasoning"] = true
		}
		out = append(out, entry)
	}
	return out
}

// CanonicalModel resolves a client-supplied model id to the exact spelling
// v1 would send upstream: a discovered catalogue id first, then the known
// seed/static aliases. The 1017 model is the one explicit exception: the
// upstream-observed lowercase spelling is the canonical chat/agent id even
// when the `/v1/models` catalogue advertises the display spelling.
func CanonicalModel(account *Account, model string) string {
	mid := strings.TrimSpace(model)
	if mid == "" {
		return ""
	}
	lower := strings.ToLower(mid)
	if lower == canonicalOneZeroOneSeven {
		return canonicalOneZeroOneSeven
	}
	if account != nil {
		account.mu.Lock()
		for _, item := range account.Models {
			id := strings.TrimSpace(modelText(item["id"]))
			if strings.ToLower(id) == lower {
				account.mu.Unlock()
				return id
			}
		}
		account.mu.Unlock()
	}
	for _, seed := range seedKnownModels {
		if strings.ToLower(seed) == lower {
			return seed
		}
	}
	for _, seed := range seedBenefitModels {
		if strings.ToLower(seed) == lower {
			return seed
		}
	}
	if canonical, ok := canonicalStaticModels[lower]; ok {
		return canonical
	}
	return mid
}

// AgentPickerModels returns the catalogue as the agent/one-click picker needs
// it: lowercase ids, including the explicit lowercase 1017 alias, while the
// `/v1/models` catalogue keeps the upstream display ids.
func (p *Provider) AgentPickerModels(ctx context.Context, force bool) ([]Model, error) {
	models, err := p.Models(ctx, force)
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(models)+1)
	seen := make(map[string]bool, len(models)+1)
	for _, item := range models {
		id := strings.TrimSpace(modelText(item["id"]))
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if seen[key] {
			continue
		}
		seen[key] = true
		entry := cloneModel(item)
		entry["id"] = key
		out = append(out, entry)
	}
	if !seen[canonicalOneZeroOneSeven] {
		out = append(out, Model{
			"id":   canonicalOneZeroOneSeven,
			"name": "DeepSeek-V4.1-Flash-1017",
		})
	}
	return out, nil
}

func (p *Provider) modelsTTL() time.Duration {
	seconds := defaultModelsTTLSeconds
	if p != nil && p.pool != nil {
		p.pool.mu.RLock()
		configured := p.pool.cfg.ModelsTTLSeconds
		p.pool.mu.RUnlock()
		if configured > 0 && !math.IsNaN(configured) && !math.IsInf(configured, 0) {
			seconds = configured
		}
	}
	return time.Duration(seconds * float64(time.Second))
}

// codeartsCreditInfo returns (current rate, note, peak rate, off-peak rate).
func codeartsCreditInfo(mid string, now time.Time) (string, string, string, string) {
	key := strings.ToLower(strings.TrimSpace(mid))
	if peak, ok := codeartsPeakCredits[key]; ok {
		hour := now.In(time.Local).Hour()
		rate := peak[1]
		if hour >= 8 && hour <= 20 {
			rate = peak[0]
		}
		note := fmt.Sprintf("peak %sx / off-peak %sx", peak[0], peak[1])
		return rate, note, peak[0], peak[1]
	}
	rate := codeartsCredits[key]
	return rate, "", rate, rate
}
