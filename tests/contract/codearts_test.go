// CodeArts v2 regression contract suite.
//
// These tests lock down the v2 foundation's scoped CodeArts behavior against a
// local fake upstream and the real provider/API composition. They are not
// captured v1.6.5 oracle output: v2 regression contract coverage is complete
// here, while v1.6.5 oracle parity remains pending. See tests/oracle/codearts.md.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
	"github.com/ayeaaaa/mix2api-v2/internal/providers/codearts"
)

const (
	codeartsContractVersion    = "v2-codearts-contract"
	codeartsContractPanelToken = "codearts-contract-panel-token"
	codeartsContractAPIKey     = "codearts-contract-api-key"
	codeartsCanonical1017      = "deepseek-v4.1-flash-1017"
	codeartsDisplay1017        = "DeepSeek-V4.1-Flash-1017"
)

var (
	codeartsTokenRemainingPattern = regexp.MustCompile(`^(?:[0-9]+h[0-9]+m|[0-9]+m)$`)
	codeartsWireTimePattern       = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}$`)
)

// codeartsExpectedModelIDs is the exact union the v1.6.5 fake upstream and the
// published official/benefit seed sets produce. The order is the provider's
// deterministic merge order; the golden records the complete entries.
var codeartsExpectedModelIDs = []string{
	"GLM-5.2",
	"GLM-4.7",
	"Qwen3-VL-235B",
	"deepseek-v4-pro-0813",
	"Kimi-K3.1-Pro",
	"GLM-5.3-Flash",
	"Kimi-K3",
	"DeepSeek-V4.1-Flash",
	codeartsDisplay1017,
	"GLM-5.3",
	"OpenPangu-2.0-Flash",
	"GLM-5.2-ArkTS-SPARK",
	"OpenPangu-2.0-Pro",
	"deepseek-v4-flash-0731",
}

func TestCodeArtsNormalizeJSONWireTimes(t *testing.T) {
	payload := []byte(`{
	  "expires_at": "2026-11-09T02:01:23",
	  "until": "2026-11-09T08:00:00",
	  "token_remaining": "3h59m"
	}`)
	got, err := NormalizeJSON(payload)
	if err != nil {
		t.Fatalf("NormalizeJSON: %v", err)
	}
	root := got.(map[string]any)
	if root["expires_at"] != TimestampValue {
		t.Fatalf("local-wire expires_at = %#v, want %q", root["expires_at"], TimestampValue)
	}
	if root["until"] != TimestampValue {
		t.Fatalf("until = %#v, want %q", root["until"], TimestampValue)
	}
	if root["token_remaining"] != "3h59m" {
		t.Fatalf("token_remaining = %#v, want the duration preserved", root["token_remaining"])
	}

	semantic, err := NormalizeJSON([]byte(`{"until":"eventually"}`))
	if err != nil {
		t.Fatalf("NormalizeJSON semantic until: %v", err)
	}
	if got := semantic.(map[string]any)["until"]; got != "eventually" {
		t.Fatalf("non-timestamp until = %#v, want it preserved", got)
	}
}

func TestCodeArtsGoldenHealthModelsChat(t *testing.T) {
	fixture := newCodeArtsContractFixture(t, true, codeartsContractAccounts(2)...)

	t.Run("health", func(t *testing.T) {
		rec := fixture.do(t, http.MethodGet, "/health?force=1", nil, nil)
		codeartsRequireStatus(t, "health", rec, http.StatusOK)
		root := decodeJSONBody(t, rec.Body.Bytes())
		if root["ok"] != true || root["version"] != codeartsContractVersion {
			t.Fatalf("health envelope = %#v", root)
		}
		providerHealth, ok := root["providers"].(map[string]any)["codearts"].(map[string]any)
		if !ok {
			t.Fatalf("providers.codearts missing: %#v", root)
		}
		if providerHealth["base_url"] != fixture.upstream.URL {
			t.Fatalf("base_url = %#v, want %q", providerHealth["base_url"], fixture.upstream.URL)
		}
		if codeartsNumber(providerHealth["accounts"]) != "2" ||
			codeartsNumber(providerHealth["healthy"]) != "2" {
			t.Fatalf("codearts health counts = %#v", providerHealth)
		}
		if providerHealth["ok"] != true || providerHealth["configured"] != true {
			t.Fatalf("codearts health status = %#v", providerHealth)
		}
		fixture.assertNoSecrets(t, "health", rec.Body.Bytes())
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_health.golden.json", func(body []byte) []byte {
			return bytes.ReplaceAll(body, []byte(fixture.upstream.URL), []byte("<upstream>"))
		})
	})

	t.Run("models", func(t *testing.T) {
		rec := fixture.doAPI(t, http.MethodGet, "/v1/models?realm=codearts", nil)
		codeartsRequireStatus(t, "models", rec, http.StatusOK)
		root := decodeJSONBody(t, rec.Body.Bytes())
		if root["object"] != "list" || root["realm"] != "codearts" {
			t.Fatalf("models envelope = %#v", root)
		}
		data, ok := root["data"].([]any)
		if !ok {
			t.Fatalf("models data = %T", root["data"])
		}
		ids := make([]string, 0, len(data))
		seen := make(map[string]bool, len(data))
		entries := make(map[string]map[string]any, len(data))
		for _, raw := range data {
			entry, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("model entry = %T, want object", raw)
			}
			id, _ := entry["id"].(string)
			if id == "" {
				t.Fatalf("model entry without id: %#v", entry)
			}
			if seen[id] {
				t.Fatalf("duplicate model id %q", id)
			}
			seen[id] = true
			entries[id] = entry
			ids = append(ids, id)
		}
		if !reflect.DeepEqual(ids, codeartsExpectedModelIDs) {
			t.Fatalf("model ids = %#v, want %#v", ids, codeartsExpectedModelIDs)
		}
		if seen[strings.ToLower(codeartsDisplay1017)] {
			t.Fatal("/v1/models advertised the lowercase chat alias instead of the display id")
		}
		entry := entries[codeartsDisplay1017]
		if entry["object"] != "model" || entry["owned_by"] != "codearts" ||
			entry["provider"] != "codearts" || entry["name"] != codeartsDisplay1017 {
			t.Fatalf("1017 model identity = %#v", entry)
		}
		timeOfDay := make(map[string]bool)
		for id, candidate := range entries {
			peak, _ := candidate["credits_peak"].(string)
			offpeak, _ := candidate["credits_offpeak"].(string)
			if peak == "" || offpeak == "" || peak == offpeak {
				continue
			}
			timeOfDay[id] = true
			credits, _ := candidate["credits"].(string)
			if credits != peak && credits != offpeak {
				t.Fatalf("%s credits = %q, want the current peak (%q) or off-peak (%q) rate",
					id, credits, peak, offpeak)
			}
		}
		for _, id := range []string{"DeepSeek-V4.1-Flash", codeartsDisplay1017} {
			if !timeOfDay[id] {
				t.Fatalf("%s did not expose distinct peak/off-peak credits (have %v)", id, timeOfDay)
			}
		}
		if len(timeOfDay) != 2 {
			t.Fatalf("time-of-day models = %v, want exactly DeepSeek-V4.1-Flash and %s",
				timeOfDay, codeartsDisplay1017)
		}
		fixture.assertNoSecrets(t, "models", rec.Body.Bytes())
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_models.golden.json", func(body []byte) []byte {
			return codeartsPrepareModels(t, body)
		})
	})

	t.Run("chat_nonstream", func(t *testing.T) {
		before := fixture.fake.totalChatCalls()
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=codearts", map[string]any{
			"model":    codeartsDisplay1017,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"stream":   false,
		})
		codeartsRequireStatus(t, "chat nonstream", rec, http.StatusOK)
		completion := decodeJSONBody(t, rec.Body.Bytes())
		if completion["object"] != "chat.completion" {
			t.Fatalf("completion object = %#v", completion["object"])
		}
		if completion["model"] != codeartsCanonical1017 {
			t.Fatalf("completion model = %#v, want canonical lowercase alias", completion["model"])
		}
		choices, ok := completion["choices"].([]any)
		if !ok || len(choices) != 1 {
			t.Fatalf("completion choices = %#v", completion["choices"])
		}
		choice := choices[0].(map[string]any)
		message := choice["message"].(map[string]any)
		if message["role"] != "assistant" || message["content"] != "你好" {
			t.Fatalf("completion message = %#v", message)
		}
		if choice["finish_reason"] != "stop" {
			t.Fatalf("completion finish_reason = %#v", choice["finish_reason"])
		}
		usage := completion["usage"].(map[string]any)
		if codeartsNumber(usage["prompt_tokens"]) != "3" ||
			codeartsNumber(usage["completion_tokens"]) != "2" ||
			codeartsNumber(usage["total_tokens"]) != "5" {
			t.Fatalf("completion usage = %#v", usage)
		}
		body, token := fixture.fake.lastChat()
		if body["model"] != codeartsCanonical1017 {
			t.Fatalf("upstream model = %#v, want canonical lowercase 1017", body["model"])
		}
		if body["stream"] != true {
			t.Fatalf("upstream stream = %#v, want forced true", body["stream"])
		}
		if _, leaked := body["realm"]; leaked {
			t.Fatalf("realm leaked upstream: %#v", body)
		}
		if token != fixture.accounts[0].Token && token != fixture.accounts[1].Token {
			t.Fatalf("upstream x-auth-token = %q, want a seeded cloud token", token)
		}
		if got := fixture.fake.totalChatCalls(); got != before+1 {
			t.Fatalf("fake chat calls = %d, want %d", got, before+1)
		}
		fixture.assertNoSecrets(t, "chat nonstream", rec.Body.Bytes())
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_chat_nonstream.golden.json", nil)
	})

	t.Run("chat_stream", func(t *testing.T) {
		beforeA := fixture.fake.chatCount(fixture.accounts[0].Token)
		beforeB := fixture.fake.chatCount(fixture.accounts[1].Token)
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=codearts", map[string]any{
			"model":    "GLM-5.2",
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"stream":   true,
		})
		codeartsRequireStatus(t, "chat stream", rec, http.StatusOK)
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
			t.Fatalf("chat stream content type = %q", got)
		}
		if got := fixture.fake.chatCount(fixture.accounts[0].Token) + fixture.fake.chatCount(fixture.accounts[1].Token); got != beforeA+beforeB+1 {
			t.Fatalf("fake chat calls = %d, want %d", got, beforeA+beforeB+1)
		}
		body, _ := fixture.fake.lastChat()
		if body["model"] != "GLM-5.2" || body["stream"] != true {
			t.Fatalf("upstream stream body = %#v", body)
		}
		events, err := NormalizeSSE(rec.Body.Bytes())
		if err != nil {
			t.Fatalf("NormalizeSSE: %v", err)
		}
		var want []Event
		decodeGoldenFile(t, "codearts_chat_stream.golden.json", &want)
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("stream golden mismatch:\ngot:  %#v\nwant: %#v", events, want)
		}
		fixture.assertNoSecrets(t, "chat stream", rec.Body.Bytes())
	})

	t.Run("chat_stream_tool_call", func(t *testing.T) {
		for _, account := range fixture.accounts {
			fixture.setChatMode(account.UID, codeartsChatToolCall)
		}
		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=codearts", map[string]any{
			"model":    "GLM-5.2",
			"messages": []any{map[string]any{"role": "user", "content": "look it up"}},
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "lookup",
					"description": "look something up",
					"parameters":  map[string]any{"type": "object"},
				},
			}},
			"stream": true,
		})
		codeartsRequireStatus(t, "chat stream tool call", rec, http.StatusOK)
		events, err := NormalizeSSE(rec.Body.Bytes())
		if err != nil {
			t.Fatalf("NormalizeSSE: %v", err)
		}
		var want []Event
		decodeGoldenFile(t, "codearts_chat_stream_tool.golden.json", &want)
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("tool stream golden mismatch:\ngot:  %#v\nwant: %#v", events, want)
		}
		if len(events) != 1 || len(events[0].ToolCalls) != 1 {
			t.Fatalf("tool stream events = %#v", events)
		}
		if events[0].ToolCalls[0].Name != "lookup" || events[0].ToolCalls[0].Arguments != `{"q":"go"}` {
			t.Fatalf("tool call = %#v", events[0].ToolCalls[0])
		}
		fixture.assertNoSecrets(t, "chat stream tool call", rec.Body.Bytes())
	})
}

func TestCodeArtsGoldenOverviewCheckinCreditsModelsRefresh(t *testing.T) {
	fixture := newCodeArtsContractFixture(t, true, codeartsContractAccounts(2)...)

	t.Run("overview", func(t *testing.T) {
		rec := fixture.doPanel(t, http.MethodGet, "/codearts/overview?force=1", nil)
		codeartsRequireStatus(t, "overview", rec, http.StatusOK)
		root := decodeJSONBody(t, rec.Body.Bytes())
		if root["service"] != "codearts" || root["online"] != true || root["region"] != "cn" {
			t.Fatalf("overview identity = %#v", root)
		}
		accounts, ok := root["accounts"].([]any)
		if !ok || len(accounts) != 2 {
			t.Fatalf("overview accounts = %#v", root["accounts"])
		}
		for _, raw := range accounts {
			account := raw.(map[string]any)
			remaining, _ := account["token_remaining"].(string)
			if !codeartsTokenRemainingPattern.MatchString(remaining) {
				t.Fatalf("overview token_remaining = %q, want a non-empty duration", remaining)
			}
			expiresAt, _ := account["expires_at"].(string)
			if !codeartsWireTimePattern.MatchString(expiresAt) {
				t.Fatalf("overview expires_at = %q, want YYYY-MM-DDTHH:MM:SS wire time", expiresAt)
			}
		}
		stats := root["stats"].(map[string]any)
		if codeartsNumber(stats["total"]) != "2" || codeartsNumber(stats["healthy"]) != "2" ||
			codeartsNumber(stats["credits"]) != "1800" {
			t.Fatalf("overview stats = %#v", stats)
		}
		fixture.assertNoSecrets(t, "overview", rec.Body.Bytes())
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_overview.golden.json", func(body []byte) []byte {
			return codeartsPrepareOverview(t, body, fixture.upstream.URL)
		})
	})

	t.Run("checkin_claim_and_confirm", func(t *testing.T) {
		uid := fixture.accounts[0].UID
		rec := fixture.doPanel(t, http.MethodPost, "/codearts/accounts/checkin", map[string]any{"uid": uid})
		codeartsRequireStatus(t, "checkin", rec, http.StatusOK)
		result := decodeJSONBody(t, rec.Body.Bytes())
		if result["ok"] != true || codeartsNumber(result["credits"]) != "1000" {
			t.Fatalf("checkin result = %#v", result)
		}
		campaigns, ok := result["campaigns"].([]any)
		if !ok || len(campaigns) != 1 {
			t.Fatalf("checkin campaigns = %#v", result["campaigns"])
		}
		campaign := campaigns[0].(map[string]any)
		if codeartsNumber(campaign["campaign_id"]) != "1" || campaign["ok"] != true ||
			campaign["status"] != "CONFIRMED" || codeartsNumber(campaign["credits"]) != "1000" {
			t.Fatalf("checkin campaign = %#v", campaign)
		}
		if got := fixture.fake.callCount(codeartsFakeOpsClaimPath); got != 1 {
			t.Fatalf("claim calls = %d, want 1", got)
		}
		if got := fixture.fake.callCount(codeartsFakeOpsConfirmPath); got != 1 {
			t.Fatalf("confirm calls = %d, want 1", got)
		}
		fixture.assertNoSecrets(t, "checkin", rec.Body.Bytes())
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_checkin.golden.json", nil)
	})

	t.Run("credits_refresh", func(t *testing.T) {
		before := fixture.fake.callCount(codeartsFakeStatisticsPath)
		rec := fixture.doPanel(t, http.MethodPost, "/codearts/credits/refresh", map[string]any{})
		codeartsRequireStatus(t, "credits refresh", rec, http.StatusOK)
		result := decodeJSONBody(t, rec.Body.Bytes())
		if result["ok"] != true || codeartsNumber(result["refreshed"]) != "2" {
			t.Fatalf("credits refresh = %#v", result)
		}
		rows, ok := result["accounts"].([]any)
		if !ok || len(rows) != 2 {
			t.Fatalf("credits accounts = %#v", result["accounts"])
		}
		for _, raw := range rows {
			row := raw.(map[string]any)
			if row["ok"] != true {
				t.Fatalf("credits row = %#v", row)
			}
			credits := row["credits"].(map[string]any)
			if codeartsNumber(credits["remain"]) != "900" || credits["unit"] != "credit" {
				t.Fatalf("credits row view = %#v", credits)
			}
		}
		if got := fixture.fake.callCount(codeartsFakeStatisticsPath); got < before+2 {
			t.Fatalf("statistics calls = %d, want at least %d", got, before+2)
		}
		fixture.assertNoSecrets(t, "credits refresh", rec.Body.Bytes())
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_credits_refresh.golden.json", nil)
	})

	t.Run("models_refresh", func(t *testing.T) {
		builtinBefore := fixture.fake.callCount(codeartsFakeBuiltinPath)
		agentBefore := fixture.fake.callCount(codeartsFakeAgentListPath)
		benefitBefore := fixture.fake.callCount(codeartsFakeBenefitConfigPath)
		rec := fixture.doPanel(t, http.MethodPost, "/codearts/models/refresh", map[string]any{})
		codeartsRequireStatus(t, "models refresh", rec, http.StatusOK)
		result := decodeJSONBody(t, rec.Body.Bytes())
		if result["ok"] != true || codeartsNumber(result["models"]) != "14" {
			t.Fatalf("models refresh = %#v", result)
		}
		catalog := result["catalog"].(map[string]any)
		if catalog["source"] != "codearts" || catalog["stale"] != false ||
			codeartsNumber(catalog["accounts"]) != "2" {
			t.Fatalf("models refresh catalog = %#v", catalog)
		}
		if _, ok := catalog["fetched_at"]; !ok {
			t.Fatalf("models refresh catalog has no fetched_at: %#v", catalog)
		}
		for name, before := range map[string]int{
			"builtin": builtinBefore,
			"agent":   agentBefore,
			"benefit": benefitBefore,
		} {
			path := map[string]string{
				"builtin": codeartsFakeBuiltinPath,
				"agent":   codeartsFakeAgentListPath,
				"benefit": codeartsFakeBenefitConfigPath,
			}[name]
			if got := fixture.fake.callCount(path); got < before+2 {
				t.Fatalf("%s discovery calls = %d, want one per account (at least %d)",
					name, got, before+2)
			}
		}
		fixture.assertNoSecrets(t, "models refresh", rec.Body.Bytes())
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_models_refresh.golden.json", nil)
	})
}

func TestCodeArtsModelsLiveSettingsToggle(t *testing.T) {
	fixture := newCodeArtsContractFixture(t, true, codeartsContractAccounts(1)...)

	rec := fixture.doAPI(t, http.MethodGet, "/v1/models?realm=codearts", nil)
	codeartsRequireStatus(t, "models enabled", rec, http.StatusOK)
	before := fixture.fake.callCount(codeartsFakeBuiltinPath)

	if _, err := fixture.store.Merge(map[string]any{
		"codearts": map[string]any{"enabled": false},
	}); err != nil {
		t.Fatalf("disable settings: %v", err)
	}
	rec = fixture.doAPI(t, http.MethodGet, "/v1/models?realm=codearts", nil)
	codeartsRequireStatus(t, "models disabled by live settings", rec, http.StatusServiceUnavailable)
	failure := codeartsErrorEnvelope(t, "models disabled by live settings", rec)
	if failure["message"] != "CodeArts 出口未启用：请到设置页填写 Base URL 和 API Key。" ||
		failure["type"] != "invalid_request_error" ||
		codeartsNumber(failure["code"]) != "503" {
		t.Fatalf("live-disabled models error = %#v", failure)
	}
	if got := fixture.fake.callCount(codeartsFakeBuiltinPath); got != before {
		t.Fatalf("disabled models triggered discovery: builtin calls %d, want %d", got, before)
	}

	if _, err := fixture.store.Merge(map[string]any{
		"codearts": map[string]any{"enabled": true},
	}); err != nil {
		t.Fatalf("re-enable settings: %v", err)
	}
	rec = fixture.doAPI(t, http.MethodGet, "/v1/models?realm=codearts", nil)
	codeartsRequireStatus(t, "models re-enabled by live settings", rec, http.StatusOK)
	root := decodeJSONBody(t, rec.Body.Bytes())
	if root["realm"] != "codearts" {
		t.Fatalf("re-enabled models envelope = %#v", root)
	}
	fixture.assertNoSecrets(t, "models live settings", rec.Body.Bytes())
}

func TestCodeArtsDisabledAndErrorEnvelopes(t *testing.T) {
	t.Run("disabled_realm", func(t *testing.T) {
		fixture := newCodeArtsContractFixture(t, false, codeartsContractAccounts(1)...)

		rec := fixture.do(t, http.MethodGet, "/health?force=1", nil, nil)
		codeartsRequireStatus(t, "disabled health", rec, http.StatusOK)
		health := decodeJSONBody(t, rec.Body.Bytes())["providers"].(map[string]any)["codearts"].(map[string]any)
		if health["ok"] != false || health["configured"] != false ||
			codeartsNumber(health["accounts"]) != "1" {
			t.Fatalf("disabled health = %#v", health)
		}
		fixture.assertNoSecrets(t, "disabled health", rec.Body.Bytes())
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_health_disabled.golden.json", func(body []byte) []byte {
			return bytes.ReplaceAll(body, []byte(fixture.upstream.URL), []byte("<upstream>"))
		})

		rec = fixture.doPanel(t, http.MethodGet, "/codearts/overview", nil)
		codeartsRequireStatus(t, "disabled overview", rec, http.StatusServiceUnavailable)
		failure := codeartsErrorEnvelope(t, "disabled overview", rec)
		if failure["message"] != "CodeArts 出口未启用：请到设置页填写 Base URL 和 API Key。" ||
			failure["type"] != "invalid_request_error" ||
			codeartsNumber(failure["code"]) != "503" {
			t.Fatalf("disabled overview error = %#v", failure)
		}
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_error_disabled_overview.golden.json", nil)

		rec = fixture.doAPI(t, http.MethodGet, "/v1/models?realm=codearts", nil)
		codeartsRequireStatus(t, "disabled models", rec, http.StatusServiceUnavailable)
		failure = codeartsErrorEnvelope(t, "disabled models", rec)
		if failure["message"] != "CodeArts 出口未启用：请到设置页填写 Base URL 和 API Key。" ||
			failure["type"] != "invalid_request_error" ||
			codeartsNumber(failure["code"]) != "503" {
			t.Fatalf("disabled models error = %#v", failure)
		}
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_error_disabled_models.golden.json", nil)

		rec = fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=codearts", map[string]any{
			"model":    "GLM-5.2",
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"stream":   false,
		})
		codeartsRequireStatus(t, "disabled chat", rec, http.StatusBadRequest)
		failure = codeartsErrorEnvelope(t, "disabled chat", rec)
		if failure["message"] != "CodeArts 出口未启用：请到设置页打开「启用 CodeArts 出口」。" ||
			failure["type"] != "invalid_request_error" ||
			codeartsNumber(failure["code"]) != "400" {
			t.Fatalf("disabled chat error = %#v", failure)
		}
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_error_disabled_chat.golden.json", nil)

		rec = fixture.doPanel(t, http.MethodGet, "/codearts/not-a-route", nil)
		codeartsRequireStatus(t, "unknown codearts route", rec, http.StatusNotFound)
		failure = codeartsErrorEnvelope(t, "unknown codearts route", rec)
		if failure["message"] != "unknown codearts endpoint" ||
			failure["type"] != "invalid_request_error" ||
			codeartsNumber(failure["code"]) != "404" {
			t.Fatalf("unknown route error = %#v", failure)
		}
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_error_unknown_route.golden.json", nil)

		rec = fixture.do(t, http.MethodGet, "/codearts/overview", nil, nil)
		codeartsRequireStatus(t, "unauthenticated panel", rec, http.StatusUnauthorized)
		failure = codeartsErrorEnvelope(t, "unauthenticated panel", rec)
		if failure["message"] != "panel password required" ||
			codeartsNumber(failure["code"]) != "401" {
			t.Fatalf("unauthenticated panel error = %#v", failure)
		}

		rec = fixture.do(t, http.MethodGet, "/v1/models?realm=codearts", nil, nil)
		codeartsRequireStatus(t, "unauthenticated models", rec, http.StatusUnauthorized)
		failure = codeartsErrorEnvelope(t, "unauthenticated models", rec)
		if failure["message"] != "invalid api key" || codeartsNumber(failure["code"]) != "401" {
			t.Fatalf("unauthenticated models error = %#v", failure)
		}

		if got := fixture.fake.totalCalls(); got != 0 {
			t.Fatalf("disabled fixture reached the fake upstream %d times", got)
		}
	})

	t.Run("upstream_bad_request", func(t *testing.T) {
		fixture := newCodeArtsContractFixture(t, true, codeartsContractAccounts(2)...)
		fixture.setChatMode(fixture.accounts[0].UID, codeartsChatBadRequest)
		fixture.setChatMode(fixture.accounts[1].UID, codeartsChatBadRequest)

		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=codearts", map[string]any{
			"model":    "GLM-5.2",
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"stream":   false,
		})
		codeartsRequireStatus(t, "upstream bad request", rec, http.StatusBadRequest)
		failure := codeartsErrorEnvelope(t, "upstream bad request", rec)
		if failure["message"] != "request param is invalid" ||
			failure["type"] != "invalid_request_error" ||
			codeartsNumber(failure["code"]) != "400" {
			t.Fatalf("upstream bad request error = %#v", failure)
		}
		if got := fixture.fake.totalChatCalls(); got != 1 {
			t.Fatalf("upstream chat calls = %d, want no failover on a validation 400", got)
		}
		fixture.assertNoSecrets(t, "upstream bad request", rec.Body.Bytes())
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_error_upstream_bad_request.golden.json", nil)
	})
}

func TestCodeArtsConcurrencyRetryThenFailover(t *testing.T) {
	fixture := newCodeArtsContractFixture(t, true, codeartsContractAccounts(2)...)
	fixture.setChatMode(fixture.accounts[0].UID, codeartsChatConcurrency)
	fixture.setChatMode(fixture.accounts[1].UID, codeartsChatSuccess)

	rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=codearts", map[string]any{
		"model":    "GLM-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":   false,
	})
	codeartsRequireStatus(t, "concurrency failover", rec, http.StatusOK)
	codeartsAssertContractCompletion(t, "concurrency failover", rec.Body.Bytes())

	if got := fixture.fake.chatCount(fixture.accounts[0].Token); got != 3 {
		t.Fatalf("first account chat calls = %d, want v1's three concurrency attempts", got)
	}
	if got := fixture.fake.chatCount(fixture.accounts[1].Token); got != 1 {
		t.Fatalf("second account chat calls = %d, want one failover", got)
	}
	if account := fixture.provider.Pool().Get(fixture.accounts[0].UID); account == nil || account.Healthy(time.Now()) {
		t.Fatalf("concurrency-exhausted account is still healthy: %#v", account)
	}
	if account := fixture.provider.Pool().Get(fixture.accounts[1].UID); account == nil || !account.Healthy(time.Now()) {
		t.Fatalf("failover account is not healthy: %#v", account)
	}
	fixture.assertNoSecrets(t, "concurrency failover", rec.Body.Bytes())
}

func TestCodeArtsCreditExhaustedFailoverAndAllAccounts(t *testing.T) {
	t.Run("failover_to_healthy_account", func(t *testing.T) {
		fixture := newCodeArtsContractFixture(t, true, codeartsContractAccounts(2)...)
		fixture.setChatMode(fixture.accounts[0].UID, codeartsChatCreditExhausted)
		fixture.setChatMode(fixture.accounts[1].UID, codeartsChatSuccess)

		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=codearts", map[string]any{
			"model":    "GLM-5.2",
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"stream":   false,
		})
		codeartsRequireStatus(t, "credit failover", rec, http.StatusOK)
		codeartsAssertContractCompletion(t, "credit failover", rec.Body.Bytes())
		if got := fixture.fake.chatCount(fixture.accounts[0].Token); got != 1 {
			t.Fatalf("exhausted account chat calls = %d, want 1", got)
		}
		if got := fixture.fake.chatCount(fixture.accounts[1].Token); got != 1 {
			t.Fatalf("healthy account chat calls = %d, want 1", got)
		}
		if account := fixture.provider.Pool().Get(fixture.accounts[0].UID); account == nil || !account.CreditExhausted() {
			t.Fatalf("credit-exhausted account was not parked")
		}
		if account := fixture.provider.Pool().Get(fixture.accounts[1].UID); account == nil || account.CreditExhausted() {
			t.Fatalf("healthy account was parked")
		}
		fixture.assertNoSecrets(t, "credit failover", rec.Body.Bytes())
	})

	t.Run("all_accounts_readable_400", func(t *testing.T) {
		fixture := newCodeArtsContractFixture(t, true, codeartsContractAccounts(2)...)
		for _, account := range fixture.accounts {
			fixture.setChatMode(account.UID, codeartsChatCreditExhausted)
		}

		rec := fixture.doAPI(t, http.MethodPost, "/v1/chat/completions?realm=codearts", map[string]any{
			"model":    "GLM-5.2",
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"stream":   false,
		})
		codeartsRequireStatus(t, "all credits exhausted", rec, http.StatusBadRequest)
		failure := codeartsErrorEnvelope(t, "all credits exhausted", rec)
		if failure["message"] != "所有可用的 CodeArts 账号积分已耗尽，请领取积分或升级套餐。" ||
			failure["type"] != "invalid_request_error" ||
			codeartsNumber(failure["code"]) != "400" {
			t.Fatalf("all credits error = %#v", failure)
		}
		for _, account := range fixture.accounts {
			if got := fixture.fake.chatCount(account.Token); got != 1 {
				t.Fatalf("%s chat calls = %d, want 1", account.UID, got)
			}
			if poolAccount := fixture.provider.Pool().Get(account.UID); poolAccount == nil || !poolAccount.CreditExhausted() {
				t.Fatalf("%s was not marked credit exhausted", account.UID)
			}
		}
		fixture.assertNoSecrets(t, "all credits exhausted", rec.Body.Bytes())
		codeartsAssertGoldenJSON(t, rec.Body.Bytes(), "codearts_error_credit_exhausted.golden.json", nil)
	})
}

// codeartsAssertContractCompletion checks the semantic non-stream shape the
// failover paths must preserve, not just the HTTP status.
func codeartsAssertContractCompletion(t *testing.T, where string, body []byte) {
	t.Helper()
	completion := decodeJSONBody(t, body)
	if completion["object"] != "chat.completion" || completion["model"] != "GLM-5.2" {
		t.Fatalf("%s completion identity = %#v", where, completion)
	}
	if id, _ := completion["id"].(string); strings.TrimSpace(id) == "" {
		t.Fatalf("%s completion id is empty: %#v", where, completion)
	}
	choices, ok := completion["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("%s completion choices = %#v", where, completion["choices"])
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("%s completion choice = %T, want object", where, choices[0])
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		t.Fatalf("%s completion message = %T, want object", where, choice["message"])
	}
	if message["role"] != "assistant" || message["content"] != "你好" {
		t.Fatalf("%s completion message = %#v", where, message)
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("%s completion finish_reason = %#v", where, choice["finish_reason"])
	}
	usage, ok := completion["usage"].(map[string]any)
	if !ok || codeartsNumber(usage["prompt_tokens"]) != "3" ||
		codeartsNumber(usage["completion_tokens"]) != "2" ||
		codeartsNumber(usage["total_tokens"]) != "5" {
		t.Fatalf("%s completion usage = %#v", where, completion["usage"])
	}
}

// codeartsContractAccount is one realistic fake-upstream credential document.
type codeartsContractAccount struct {
	UID           string
	Nickname      string
	Token         string
	AccessKeyID   string
	SecretKey     string
	RefreshToken  string
	ClientID      string
	CodeVerifier  string
	ExpirySeconds int
}

func codeartsContractAccounts(count int) []codeartsContractAccount {
	out := make([]codeartsContractAccount, 0, count)
	for index := 0; index < count; index++ {
		uid := fmt.Sprintf("codearts-contract-%d", index+1)
		out = append(out, codeartsContractAccount{
			UID:           uid,
			Nickname:      fmt.Sprintf("Contract User %d", index+1),
			Token:         fmt.Sprintf("cloud-dragon-token-contract-%d-0123456789", index+1),
			AccessKeyID:   fmt.Sprintf("AK-CODEARTS-CONTRACT-%d", index+1),
			SecretKey:     fmt.Sprintf("codearts-secret-key-contract-%d-0123456789", index+1),
			RefreshToken:  fmt.Sprintf("codearts-refresh-token-contract-%d-0123456789", index+1),
			ClientID:      "codearts-agent",
			CodeVerifier:  fmt.Sprintf("contract-verifier-%d", index+1),
			ExpirySeconds: 4 * 60 * 60,
		})
	}
	return out
}

type codeartsContractFixture struct {
	server    *http.Server
	provider  *codearts.Provider
	store     *config.Store
	upstream  *httptest.Server
	fake      *codeartsContractUpstream
	accounts  []codeartsContractAccount
	secrets   []string
	panelAuth string
	apiKey    string
}

func newCodeArtsContractFixture(
	t *testing.T,
	enabled bool,
	accounts ...codeartsContractAccount,
) *codeartsContractFixture {
	t.Helper()

	fake := newCodeArtsContractUpstream()
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	t.Setenv("WB_CODEARTS_SNAP_HOST", upstream.URL)
	t.Setenv("WB_CODEARTS_SNAP_MANAGER", upstream.URL+"/snap-manager")
	t.Setenv("WB_CODEARTS_BENEFIT_HOST", upstream.URL)
	t.Setenv("WB_CODEARTS_STS_HOST", upstream.URL)
	t.Setenv("WB_CODEARTS_PORTAL_HOST", "https://portal.contract.test/portal")

	root := t.TempDir()
	accountsDir := filepath.Join(root, "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatalf("create accounts dir: %v", err)
	}
	store := config.New(filepath.Join(accountsDir, "settings.json"))
	if _, err := store.Merge(map[string]any{
		"codearts": map[string]any{
			"enabled":                   enabled,
			"timeout_seconds":           5,
			"stream_timeout_seconds":    30,
			"models_ttl_seconds":        300,
			"max_concurrent":            1,
			"benefit_auto_claim":        true,
			"credits_auto_claim":        true,
			"credits_expiring_priority": true,
		},
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	secrets := make([]string, 0, len(accounts)*6)
	for _, account := range accounts {
		dpopPrivateKey, err := codearts.NewDPoPKey()
		if err != nil {
			t.Fatalf("NewDPoPKey(%s): %v", account.UID, err)
		}
		var dpopFields struct {
			D string `json:"d"`
		}
		if err := json.Unmarshal(dpopPrivateKey, &dpopFields); err != nil || dpopFields.D == "" {
			t.Fatalf("decode DPoP private scalar for %s: %v", account.UID, err)
		}
		authPath, err := codearts.AuthPath(accountsDir, account.UID)
		if err != nil {
			t.Fatalf("AuthPath(%s): %v", account.UID, err)
		}
		if err := codearts.SaveAuth(authPath, codearts.Auth{
			UserID:           account.UID,
			UserName:         account.Nickname,
			DomainID:         "domain-" + account.UID,
			CloudDragonToken: account.Token,
			AccessKeyID:      account.AccessKeyID,
			SecretAccessKey:  account.SecretKey,
			RefreshToken:     account.RefreshToken,
			CodeVerifier:     account.CodeVerifier,
			ClientID:         account.ClientID,
			Expiration:       now.Add(time.Duration(account.ExpirySeconds) * time.Second),
			DPopPrivateKey:   dpopPrivateKey,
			ProxySlot:        "",
			UpdatedAt:        now.Unix(),
		}); err != nil {
			t.Fatalf("SaveAuth(%s): %v", account.UID, err)
		}
		savedAuth, err := codearts.LoadAuth(authPath)
		if err != nil {
			t.Fatalf("LoadAuth(%s): %v", account.UID, err)
		}
		var savedDPoP struct {
			D string `json:"d"`
		}
		if err := json.Unmarshal(savedAuth.DPopPrivateKey, &savedDPoP); err != nil ||
			savedDPoP.D == "" || savedDPoP.D != dpopFields.D {
			t.Fatalf("auth file %s did not retain the seeded DPoP private scalar", authPath)
		}
		secrets = append(secrets,
			account.Token, account.AccessKeyID, account.SecretKey,
			account.RefreshToken, account.CodeVerifier, dpopFields.D)
	}

	provider, err := codearts.New(store)
	if err != nil {
		t.Fatalf("codearts.New: %v", err)
	}
	server := api.NewServer(api.ServerOptions{
		Version:    codeartsContractVersion,
		Providers:  []providers.Provider{provider},
		PanelAuth:  api.RequirePanelAuth(codeartsContractPanelToken),
		APIKeyAuth: api.RequireAPIKeyAuth(codeartsContractAPIKey),
	})

	fixture := &codeartsContractFixture{
		server:    server,
		provider:  provider,
		store:     store,
		upstream:  upstream,
		fake:      fake,
		accounts:  append([]codeartsContractAccount(nil), accounts...),
		secrets:   secrets,
		panelAuth: codeartsContractPanelToken,
		apiKey:    codeartsContractAPIKey,
	}
	return fixture
}

func (f *codeartsContractFixture) do(
	t *testing.T,
	method, path string,
	body any,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s body: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	f.server.Handler.ServeHTTP(recorder, request)
	return recorder
}

func (f *codeartsContractFixture) doPanel(
	t *testing.T,
	method, path string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, method, path, body, map[string]string{
		"X-Panel-Token": f.panelAuth,
	})
}

func (f *codeartsContractFixture) doAPI(
	t *testing.T,
	method, path string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, method, path, body, map[string]string{
		"Authorization": "Bearer " + f.apiKey,
	})
}

func (f *codeartsContractFixture) setChatMode(uid, mode string) {
	for _, account := range f.accounts {
		if account.UID == uid {
			f.fake.setMode(account.Token, mode)
		}
	}
}

func (f *codeartsContractFixture) assertNoSecrets(t *testing.T, where string, body []byte) {
	t.Helper()
	text := string(body)
	for _, secret := range f.secrets {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaked credential %q", where, secret)
		}
	}
	for _, key := range []string{
		"cloud_dragon_token", "access_key_id", "secret_access_key",
		"refresh_token", "dpop_private_key", "code_verifier",
	} {
		if strings.Contains(text, key) {
			t.Fatalf("%s leaked credential field %q: %s", where, key, text)
		}
	}
}

const (
	codeartsFakeAgentListPath     = "/v1/agent-center/agents/useragents"
	codeartsFakeAgentDetailPath   = "/v1/agent-center/agents/detail"
	codeartsFakeBuiltinPath       = "/v1/model/builtin"
	codeartsFakeBenefitConfigPath = "/api/v1/gateway/config"
	codeartsFakeBenefitClaimPath  = "/api/v1/benefit/claim"
	codeartsFakeStatisticsPath    = "/snap-manager/v1/statistics/plugin"
	codeartsFakeDeliveryPath      = "/v1/ops/delivery"
	codeartsFakeOpsClaimPath      = "/v1/ops/claim"
	codeartsFakeOpsConfirmPath    = "/v1/ops/confirm"
	codeartsFakeChatPath          = "/api/v2/chat/completions"

	codeartsChatSuccess         = "success"
	codeartsChatConcurrency     = "concurrency"
	codeartsChatCreditExhausted = "credit_exhausted"
	codeartsChatBadRequest      = "bad_request"
	codeartsChatToolCall        = "tool_call"
)

// codeartsContractUpstream is the deterministic local stand-in for the
// v1.6.5 `_fake_codearts.py` endpoints plus per-account chat failure modes.
type codeartsContractUpstream struct {
	mu sync.Mutex

	calls      map[string]int
	chatCalls  map[string]int
	chatBodies []map[string]any
	chatTokens []string
	modes      map[string]string
}

func newCodeArtsContractUpstream() *codeartsContractUpstream {
	return &codeartsContractUpstream{
		calls:     make(map[string]int),
		chatCalls: make(map[string]int),
		modes:     make(map[string]string),
	}
}

func (u *codeartsContractUpstream) setMode(token, mode string) {
	u.mu.Lock()
	u.modes[token] = mode
	u.mu.Unlock()
}

func (u *codeartsContractUpstream) mode(token string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	mode := u.modes[token]
	if mode == "" {
		return codeartsChatSuccess
	}
	return mode
}

func (u *codeartsContractUpstream) record(path string) {
	u.mu.Lock()
	u.calls[path]++
	u.mu.Unlock()
}

func (u *codeartsContractUpstream) callCount(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[path]
}

func (u *codeartsContractUpstream) totalCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	total := 0
	for _, count := range u.calls {
		total += count
	}
	return total
}

func (u *codeartsContractUpstream) chatCount(token string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chatCalls[token]
}

func (u *codeartsContractUpstream) totalChatCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	total := 0
	for _, count := range u.chatCalls {
		total += count
	}
	return total
}

func (u *codeartsContractUpstream) lastChat() (map[string]any, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.chatBodies) == 0 {
		return nil, ""
	}
	return u.chatBodies[len(u.chatBodies)-1], u.chatTokens[len(u.chatTokens)-1]
}

func (u *codeartsContractUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case r.Method == http.MethodGet && path == codeartsFakeAgentListPath:
		u.record(path)
		codeartsWriteFakeJSON(w, map[string]any{
			"agents": []any{map[string]any{
				"agent_id":         "agent-code",
				"agent_name":       "CodeAgent",
				"show_in_ide":      true,
				"alias":            map[string]any{"alias_zh_cn": "智能体"},
				"is_primary_agent": true,
			}},
		})

	case r.Method == http.MethodGet && path == codeartsFakeAgentDetailPath:
		u.record(path)
		codeartsWriteFakeJSON(w, map[string]any{
			"gpts": map[string]any{"models": []any{map[string]any{
				"model_name": "GLM-5.2",
				"model_parameters": map[string]any{
					"context_window": 131072,
					"max_tokens":     8192,
				},
			}}},
		})

	case r.Method == http.MethodGet && path == codeartsFakeBuiltinPath:
		u.record(path)
		codeartsWriteFakeJSON(w, map[string]any{
			"builtinModels": []any{
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
		})

	case r.Method == http.MethodGet && path == codeartsFakeBenefitConfigPath:
		u.record(path)
		codeartsWriteFakeJSON(w, map[string]any{
			"error_code": "0000",
			"result": map[string]any{"models": []any{map[string]any{
				"model_id":       "deepseek-v4-pro-0813",
				"model_name":     "deepseek-v4-pro-0813",
				"context_window": 65536,
				"max_tokens":     4096,
			}}},
		})

	case r.Method == http.MethodPost && path == codeartsFakeBenefitClaimPath:
		u.record(path)
		codeartsWriteFakeJSON(w, map[string]any{"error_code": "0000", "error_msg": ""})

	case r.Method == http.MethodGet && path == codeartsFakeStatisticsPath:
		u.record(path)
		codeartsWriteFakeJSON(w, map[string]any{
			"metrics": []any{map[string]any{
				"name":                           "usageTotalPackageCredit",
				"package_credit_amount":          1000,
				"package_credit_used":            100,
				"package_credit_remain":          900,
				"package_credit_expiring_amount": 5,
			}},
			"model_metrics": []any{map[string]any{
				"model_id": "GLM-5.2", "model_name": "GLM-5.2",
				"model_desc": "latest flagship", "model_type": "system",
				"enable": true, "status": "normal", "is_luxury": false,
			}},
			"package": map[string]any{
				"spec_code":         "codearts.agent.individual.trial",
				"status":            "normal",
				"package_name_cn":   "体验版",
				"package_name_en":   "Trial",
				"is_credit_package": true,
			},
			"start_date": "2026-10-31",
			"end_date":   "2026-12-01",
		})

	case r.Method == http.MethodGet && path == codeartsFakeDeliveryPath:
		u.record(path)
		codeartsWriteFakeJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{"items": []any{map[string]any{
				"campaignId":         1,
				"title":              "每日签到",
				"type":               "USER_LOGIN",
				"status":             "ELIGIBLE",
				"claimable":          true,
				"benefitUnit":        "CREDIT",
				"pendingTotalAmount": 1000,
			}}},
		})

	case r.Method == http.MethodPost && path == codeartsFakeOpsClaimPath:
		u.record(path)
		codeartsWriteFakeJSON(w, map[string]any{"code": 0, "data": map[string]any{}})

	case r.Method == http.MethodPost && path == codeartsFakeOpsConfirmPath:
		u.record(path)
		codeartsWriteFakeJSON(w, map[string]any{"code": 0, "data": map[string]any{}})

	case r.Method == http.MethodPost && path == codeartsFakeChatPath:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		token := r.Header.Get("x-auth-token")
		u.mu.Lock()
		u.calls[path]++
		u.chatCalls[token]++
		u.chatBodies = append(u.chatBodies, body)
		u.chatTokens = append(u.chatTokens, token)
		u.mu.Unlock()

		switch u.mode(token) {
		case codeartsChatConcurrency:
			codeartsWriteFakeError(w, http.StatusBadRequest,
				`{"error_msg":"并发会话数已达上限(3个)，请关闭部分会话后重试。"}`)
		case codeartsChatCreditExhausted:
			codeartsWriteFakeError(w, http.StatusBadRequest,
				`{"message":"您的积分已耗尽，请关注活动积分领取活动或者升级套餐。","type":"invalid_request_error","code":400}`)
		case codeartsChatBadRequest:
			codeartsWriteFakeError(w, http.StatusBadRequest,
				`{"error_msg":"request param is invalid"}`)
		case codeartsChatToolCall:
			model, _ := body["model"].(string)
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			for _, frame := range codeartsContractToolFrames(model) {
				if _, err := w.Write(frame); err != nil {
					return
				}
			}
		default:
			model, _ := body["model"].(string)
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			for _, frame := range codeartsContractChatFrames(model) {
				if _, err := w.Write(frame); err != nil {
					return
				}
			}
		}

	default:
		http.NotFound(w, r)
	}
}

func codeartsWriteFakeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func codeartsWriteFakeError(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// codeartsContractChatFrames mirrors the v1.6.5 fake: role, two text deltas, a
// finish frame, one trailing usage frame, and [DONE].
func codeartsContractChatFrames(model string) [][]byte {
	chunks := []map[string]any{
		{
			"id": "chatcmpl-contract-upstream", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": ""},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-contract-upstream", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "你"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-contract-upstream", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "好"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-contract-upstream", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
			}},
		},
		{
			"id": "chatcmpl-contract-upstream", "object": "chat.completion.chunk", "created": 1,
			"model":   model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5,
			},
		},
	}
	frames := make([][]byte, 0, len(chunks)+1)
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			panic(err)
		}
		frames = append(frames, []byte("data: "+string(encoded)+"\n\n"))
	}
	return append(frames, []byte("data: [DONE]\n\n"))
}

// codeartsContractToolFrames is one text delta followed by a complete function
// call, a tool_calls finish, usage, and [DONE].
func codeartsContractToolFrames(model string) [][]byte {
	chunks := []map[string]any{
		{
			"id": "chatcmpl-contract-tool", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": "checking"},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-contract-tool", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_contract", "type": "function",
					"function": map[string]any{
						"name": "lookup", "arguments": `{"q":`,
					},
				}}},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-contract-tool", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0,
					"function": map[string]any{
						"arguments": `"go"}`,
					},
				}}},
				"finish_reason": nil,
			}},
		},
		{
			"id": "chatcmpl-contract-tool", "object": "chat.completion.chunk", "created": 1,
			"model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls",
			}},
		},
		{
			"id": "chatcmpl-contract-tool", "object": "chat.completion.chunk", "created": 1,
			"model":   model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": 8, "completion_tokens": 4, "total_tokens": 12,
			},
		},
	}
	frames := make([][]byte, 0, len(chunks)+1)
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			panic(err)
		}
		frames = append(frames, []byte("data: "+string(encoded)+"\n\n"))
	}
	return append(frames, []byte("data: [DONE]\n\n"))
}

func codeartsRequireStatus(t *testing.T, where string, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("%s status = %d, want %d, body = %s", where, rec.Code, want, rec.Body)
	}
}

func codeartsErrorEnvelope(t *testing.T, where string, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	root := decodeJSONBody(t, rec.Body.Bytes())
	failure, ok := root["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s error envelope = %#v", where, root)
	}
	return failure
}

func codeartsNumber(value any) string {
	switch typed := value.(type) {
	case json.Number:
		return typed.String()
	case float64:
		return fmt.Sprintf("%v", typed)
	case int:
		return fmt.Sprint(typed)
	case int64:
		return fmt.Sprint(typed)
	case string:
		return typed
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func codeartsDecodeJSON(t *testing.T, body []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON %s: %v", body, err)
	}
	return value
}

func codeartsEncodeJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode JSON: %v", err)
	}
	return encoded
}

// codeartsPrepareModels replaces every time-of-day credit multiplier (models
// whose published peak and off-peak rates differ) with a stable placeholder.
// The test separately asserts each live value is one of those two rates.
func codeartsPrepareModels(t *testing.T, body []byte) []byte {
	t.Helper()
	value := codeartsDecodeJSON(t, body)
	root, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("models payload root = %T, want object", value)
	}
	data, _ := root["data"].([]any)
	for _, raw := range data {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		peak, _ := entry["credits_peak"].(string)
		offpeak, _ := entry["credits_offpeak"].(string)
		if peak == "" || offpeak == "" || peak == offpeak {
			continue
		}
		if _, present := entry["credits"]; present {
			entry["credits"] = "<time_of_day_credits>"
		}
	}
	return codeartsEncodeJSON(t, root)
}

// codeartsPrepareOverview replaces the fake upstream URL and the per-second
// token_remaining countdown. expires_at is a timestamp key and is normalized by
// NormalizeJSON itself.
func codeartsPrepareOverview(t *testing.T, body []byte, upstreamURL string) []byte {
	t.Helper()
	body = bytes.ReplaceAll(body, []byte(upstreamURL), []byte("<upstream>"))
	value := codeartsDecodeJSON(t, body)
	root, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("overview payload root = %T, want object", value)
	}
	accounts, _ := root["accounts"].([]any)
	for _, raw := range accounts {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		if _, present := entry["token_remaining"]; present {
			entry["token_remaining"] = "<duration>"
		}
	}
	return codeartsEncodeJSON(t, root)
}

func codeartsAssertGoldenJSON(
	t *testing.T,
	actual []byte,
	name string,
	prepare func([]byte) []byte,
) {
	t.Helper()
	prepared := actual
	if prepare != nil {
		prepared = prepare(actual)
	}
	if os.Getenv("UPDATE_CODEARTS_GOLDENS") == "1" {
		normalized, err := NormalizeJSON(prepared)
		if err != nil {
			t.Fatalf("normalize golden %s: %v", name, err)
		}
		encoded, err := json.MarshalIndent(normalized, "", "  ")
		if err != nil {
			t.Fatalf("encode golden %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join("testdata", name), append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if err := normalizedJSONMismatch(prepared, raw); err != nil {
		t.Fatalf("golden %s mismatch: %v", name, err)
	}
}
