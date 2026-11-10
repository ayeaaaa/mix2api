package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OpenRouterURL is the catalogue endpoint production uses. Tests inject an
// httptest.Server base URL instead.
const OpenRouterURL = "https://openrouter.ai/api/v1/models"

// SourceOverrides are the names that differ from OpenRouter's so the automatic
// matcher cannot find them. Kept deliberately short, exactly like v1.
var SourceOverrides = map[string]string{
	"hy4-preview-f":        "tencent/hy4-preview",
	"hy4-preview-dev":      "tencent/hy4-preview",
	"hy4-preview-x":        "tencent/hy4-preview",
	"hy3-x":                "tencent/hy3",
	"kimi-k3-1":            "moonshotai/kimi-k3",
	"kimi-k2.7":            "moonshotai/kimi-k2.7-code",
	"glm-5.0-turbo":        "z-ai/glm-5-turbo",
	"deepseek-v3-1":        "deepseek/deepseek-chat-v3.1",
	"deepseek-v3-1-volc":   "deepseek/deepseek-chat-v3.1",
	"deepseek-v3-2-volc":   "deepseek/deepseek-v3.2",
	"deepseek-v3-0324":     "deepseek/deepseek-chat-v3-0324",
	"hunyuan-2.0-instruct": "tencent/hunyuan-a13b-instruct",
	"hunyuan-chat":         "tencent/hunyuan-a13b-instruct",
}

// Variant suffixes are stripped only when the base resolves uniquely. -f,
// -dev and -x deliberately stay distinct tiers.
var variantSuffixes = []struct {
	Suffix  string
	Enabled bool
}{
	{"-lkeap", true},
	{"-taiji", true},
	{"-volc", true},
	{"-sg", true},
	{"-f", false},
	{"-dev", false},
	{"-x", false},
}

// OpenRouterPrice is one catalogue row in per-token USD.
type OpenRouterPrice struct {
	Prompt                float64          `json:"prompt"`
	Completion            float64          `json:"completion"`
	InputCacheRead        float64          `json:"input_cache_read"`
	InputCacheReadPresent bool             `json:"-"`
	Overrides             []OpenRouterBand `json:"overrides,omitempty"`
	Raw                   map[string]any   `json:"-"`
}

// OpenRouterBand is one conditional price in the source catalogue.
type OpenRouterBand struct {
	Prompt                float64
	Completion            float64
	InputCacheRead        float64
	InputCacheReadPresent bool
	MinPromptTokens       *int64
	UTCDays               []string
	UTCStart              *int
	UTCEnd                *int
}

// FlexFloat accepts the numeric strings OpenRouter uses for prices.
type FlexFloat float64

func (f *FlexFloat) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "null" || text == `""` {
		*f = 0
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		var decoded string
		if err := json.Unmarshal(data, &decoded); err != nil {
			return err
		}
		text = strings.TrimSpace(decoded)
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	*f = FlexFloat(number)
	return nil
}

type openRouterPayload struct {
	Data []struct {
		ID      string `json:"id"`
		Pricing struct {
			Prompt         FlexFloat  `json:"prompt"`
			Completion     FlexFloat  `json:"completion"`
			InputCacheRead *FlexFloat `json:"input_cache_read"`
		} `json:"pricing"`
		Overrides []struct {
			Prompt          FlexFloat  `json:"prompt"`
			Completion      FlexFloat  `json:"completion"`
			InputCacheRead  *FlexFloat `json:"input_cache_read"`
			MinPromptTokens *int64     `json:"min_prompt_tokens"`
			UTCDays         []string   `json:"utc_days"`
			UTCStart        *int       `json:"utc_start"`
			UTCEnd          *int       `json:"utc_end"`
		} `json:"overrides"`
	} `json:"data"`
}

// FetchOpenRouter fetches and normalizes the live catalogue. The caller
// supplies the HTTP client and endpoint so tests never touch the network.
func FetchOpenRouter(ctx context.Context, client *http.Client, baseURL string) (map[string]OpenRouterPrice, error) {
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = OpenRouterURL
	}
	if parsed, err := url.Parse(baseURL); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("pricing: invalid OpenRouter URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "mix2api-pricing/1.0")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch OpenRouter: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Never echo a response body: it can contain endpoint details.
		return nil, fmt.Errorf("pricing: OpenRouter returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("pricing: read OpenRouter response: %w", err)
	}
	return NormalizeOpenRouter(data)
}

// NormalizeOpenRouter converts a decoded catalogue payload.
func NormalizeOpenRouter(data []byte) (map[string]OpenRouterPrice, error) {
	var payload openRouterPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("pricing: decode OpenRouter response: %w", err)
	}
	out := make(map[string]OpenRouterPrice, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		price := OpenRouterPrice{
			Prompt:     float64(item.Pricing.Prompt),
			Completion: float64(item.Pricing.Completion),
		}
		if item.Pricing.InputCacheRead != nil {
			price.InputCacheRead = float64(*item.Pricing.InputCacheRead)
			price.InputCacheReadPresent = true
		}
		for _, band := range item.Overrides {
			normalized := OpenRouterBand{
				Prompt:          float64(band.Prompt),
				Completion:      float64(band.Completion),
				MinPromptTokens: band.MinPromptTokens,
				UTCDays:         append([]string(nil), band.UTCDays...),
				UTCStart:        band.UTCStart,
				UTCEnd:          band.UTCEnd,
			}
			if band.InputCacheRead != nil {
				normalized.InputCacheRead = float64(*band.InputCacheRead)
				normalized.InputCacheReadPresent = true
			}
			price.Overrides = append(price.Overrides, normalized)
		}
		out[id] = price
	}
	return out, nil
}

func perMillion(value float64) *float64 {
	if value == 0 {
		zero := 0.0
		return &zero
	}
	rounded := float64(int64(value*1_000_000*1_000_000+0.5)) / 1_000_000
	return &rounded
}

func ratesFrom(price OpenRouterPrice) Rates {
	rates := Rates{InputCacheMiss: *perMillion(price.Prompt), Output: *perMillion(price.Completion)}
	if !price.InputCacheReadPresent && price.InputCacheRead == 0 {
		rates.NullInputCacheHit = true
	} else {
		rates.InputCacheHit = *perMillion(price.InputCacheRead)
	}
	return rates
}

func bandsFrom(price OpenRouterPrice) []Band {
	out := make([]Band, 0, len(price.Overrides))
	weekdayLongNames := map[string]string{
		"monday": "mon", "tuesday": "tue", "wednesday": "wed",
		"thursday": "thu", "friday": "fri", "saturday": "sat", "sunday": "sun",
	}
	for _, item := range price.Overrides {
		band := Band{Flat: ratesFrom(OpenRouterPrice{
			Prompt: item.Prompt, Completion: item.Completion,
			InputCacheRead: item.InputCacheRead, InputCacheReadPresent: item.InputCacheReadPresent,
		})}
		if item.MinPromptTokens != nil {
			band.MinPromptTokens = *item.MinPromptTokens
		} else if item.UTCStart != nil || item.UTCEnd != nil || len(item.UTCDays) > 0 {
			for _, raw := range item.UTCDays {
				name := strings.ToLower(strings.TrimSpace(raw))
				if short, ok := weekdayLongNames[name]; ok {
					band.Days = append(band.Days, short)
				} else if len(name) == 3 {
					band.Days = append(band.Days, name)
				}
			}
			if item.UTCStart != nil {
				band.Start = *item.UTCStart
			}
			if item.UTCEnd != nil && *item.UTCEnd != 0 {
				band.End = *item.UTCEnd
			} else {
				band.End = 2400
			}
		} else {
			continue
		}
		out = append(out, band)
	}
	return out
}

func normalize(name string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(name) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func indexOpenRouter(models map[string]OpenRouterPrice) map[string][]string {
	byNorm := map[string][]string{}
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if strings.Contains(id, ":") {
			continue
		}
		leaf := id
		if position := strings.LastIndex(id, "/"); position >= 0 {
			leaf = id[position+1:]
		}
		key := normalize(leaf)
		byNorm[key] = append(byNorm[key], id)
	}
	return byNorm
}

func autoMatch(hubID string, byNorm map[string][]string) string {
	leaf := hubID
	if position := strings.LastIndex(hubID, "/"); position >= 0 {
		leaf = hubID[position+1:]
	}
	candidates := byNorm[normalize(leaf)]
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

func (s *Snapshot) resolveOnce(hubID string, models map[string]OpenRouterPrice, byNorm map[string][]string) (string, bool) {
	overrides := s.RuntimeOverrides()
	for _, table := range []map[string]string{overrides, SourceOverrides} {
		if ref := table[hubID]; ref != "" {
			if _, ok := models[ref]; ok {
				return ref, true
			}
		}
	}
	if auto := autoMatch(hubID, byNorm); auto != "" {
		return auto, false
	}
	return "", false
}

func (s *Snapshot) resolve(hubID string, models map[string]OpenRouterPrice, byNorm map[string][]string) (string, bool) {
	ref, viaOverride := s.resolveOnce(hubID, models, byNorm)
	if ref != "" || !s.VariantInherit() {
		return ref, viaOverride
	}
	for _, base := range variantBases(hubID) {
		ref, _ = s.resolveOnce(base, models, byNorm)
		if ref != "" {
			return ref, true
		}
	}
	return "", false
}

func inheritedFrom(hubID string, models map[string]OpenRouterPrice, byNorm map[string][]string) (string, bool) {
	// Source-level only; used by EnsurePolicy where runtime overrides cannot
	// change the inheritance decision once a direct match is absent.
	if autoMatch(hubID, byNorm) != "" {
		return "", false
	}
	if _, ok := SourceOverrides[hubID]; ok {
		return "", false
	}
	for _, base := range variantBases(hubID) {
		if _, ok := SourceOverrides[base]; ok {
			return base, true
		}
		if autoMatch(base, byNorm) != "" {
			return base, true
		}
	}
	return "", false
}

func variantBases(hubID string) []string {
	name := strings.TrimSpace(hubID)
	out := make([]string, 0)
	for _, suffix := range variantSuffixes {
		if !suffix.Enabled || !strings.HasSuffix(name, suffix.Suffix) {
			continue
		}
		base := strings.TrimSpace(strings.TrimSuffix(name, suffix.Suffix))
		if base != "" {
			out = append(out, base)
		}
	}
	return out
}

func entryFor(hubID, ref string, models map[string]OpenRouterPrice, inherited string, overridden bool) Entry {
	price := models[ref]
	entry := Entry{
		Display:  hubID,
		Source:   "openrouter",
		Currency: "USD",
		Unit:     1_000_000,
		ORID:     ref,
		Flat:     ratesFrom(price),
	}
	if bands := bandsFrom(price); len(bands) > 0 {
		entry.Bands = bands
	}
	if inherited != "" {
		entry.Via = "variant"
		entry.InheritedFrom = inherited
	} else if overridden {
		entry.Via = "override"
		entry.OverrideFrom = hubID
	}
	return entry
}

// VariantInherit reports the panel setting (default true).
func (s *Snapshot) VariantInherit() bool {
	if s == nil {
		return true
	}
	path := filepath.Join(s.accountsDir, "settings.json")
	value := true
	if data, err := os.ReadFile(path); err == nil {
		var settings map[string]any
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.UseNumber()
		if err := decoder.Decode(&settings); err == nil {
			if raw, ok := settings["pricing_variant_inherit"]; ok {
				flag, ok := raw.(bool)
				value = ok && flag
			}
		}
	}
	return value
}

// RememberOpenRouter stores the last successful fetch for on-demand pricing.
func (s *Snapshot) RememberOpenRouter(models map[string]OpenRouterPrice) {
	if s == nil || len(models) == 0 {
		return
	}
	s.liveMu.Lock()
	s.live = cloneOpenRouter(models)
	s.liveNorm = indexOpenRouter(models)
	s.liveAt = time.Now()
	s.liveMu.Unlock()
}

func cloneOpenRouter(source map[string]OpenRouterPrice) map[string]OpenRouterPrice {
	out := make(map[string]OpenRouterPrice, len(source))
	for id, price := range source {
		price.Overrides = append([]OpenRouterBand(nil), price.Overrides...)
		out[id] = price
	}
	return out
}

func (s *Snapshot) liveIndex() (map[string]OpenRouterPrice, map[string][]string) {
	if s == nil {
		return nil, map[string][]string{}
	}
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	return s.live, s.liveNorm
}

// BuildSnapshot resolves every baseline model against a fetched catalogue.
// previous supplies the fallback when the fetch failed (orModels == nil).
func (s *Snapshot) BuildSnapshot(models map[string]OpenRouterPrice, previous *Snapshot, extraIDs []string, variants bool) (*Snapshot, []string, []string) {
	baseline := make([]string, 0)
	seen := map[string]bool{}
	if previous != nil {
		for _, id := range sortModels(previous.Models) {
			if !seen[id] {
				seen[id] = true
				baseline = append(baseline, id)
			}
		}
	}
	if len(baseline) == 0 {
		for _, id := range sortModels(s.Models) {
			if !seen[id] {
				seen[id] = true
				baseline = append(baseline, id)
			}
		}
	}
	for _, id := range extraIDs {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			baseline = append(baseline, id)
		}
	}

	byNorm := map[string][]string{}
	if models != nil {
		byNorm = indexOpenRouter(models)
	}
	doc := &Snapshot{
		Meta: Meta{
			GeneratedAt:  time.Now().Format("2006-01-02T15:04:05"),
			BaseCurrency: "CNY",
			USDCNY:       7.10,
			Sources:      map[string]string{"openrouter": OpenRouterURL},
		},
		Models: map[string]Entry{},
	}
	if previous != nil && previous.Meta.USDCNY > 0 {
		doc.Meta.USDCNY = previous.Meta.USDCNY
	} else if s != nil && s.Meta.USDCNY > 0 {
		doc.Meta.USDCNY = s.Meta.USDCNY
	}
	unpriced := make([]string, 0)
	overridden := make([]string, 0)
	resolver := s
	if resolver == nil {
		resolver = &Snapshot{}
	}
	for _, hubID := range baseline {
		if models == nil {
			if previous != nil {
				if entry, ok := previous.Models[hubID]; ok {
					doc.Models[hubID] = entry
					continue
				}
			}
			unpriced = append(unpriced, hubID)
			continue
		}
		ref, viaOverride := resolver.resolveWithVariants(hubID, models, byNorm, variants)
		if ref == "" {
			unpriced = append(unpriced, hubID)
			continue
		}
		if viaOverride {
			overridden = append(overridden, hubID)
		}
		inherited := ""
		if variants {
			if base, ok := inheritedFromWithOverrides(resolver, hubID, models, byNorm); ok {
				inherited = base
			}
		}
		doc.Models[hubID] = entryFor(hubID, ref, models, inherited, viaOverride)
	}
	return doc, unpriced, overridden
}

func (s *Snapshot) resolveWithVariants(hubID string, models map[string]OpenRouterPrice, byNorm map[string][]string, variants bool) (string, bool) {
	ref, via := s.resolveOnce(hubID, models, byNorm)
	if ref != "" || !variants {
		return ref, via
	}
	for _, base := range variantBases(hubID) {
		ref, _ := s.resolveOnce(base, models, byNorm)
		if ref != "" {
			return ref, true
		}
	}
	return "", false
}

func inheritedFromWithOverrides(s *Snapshot, hubID string, models map[string]OpenRouterPrice, byNorm map[string][]string) (string, bool) {
	if ref, _ := s.resolveOnce(hubID, models, byNorm); ref != "" {
		return "", false
	}
	for _, base := range variantBases(hubID) {
		if ref, _ := s.resolveOnce(base, models, byNorm); ref != "" {
			return base, true
		}
	}
	return "", false
}
