package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDailyChatSettingsViewAndSave(t *testing.T) {
	dir := t.TempDir()
	provider, _ := newPoolProvider(t, realmIntl, dir)
	view, err := provider.SettingsView(context.Background())
	if err != nil {
		t.Fatalf("settings view: %v", err)
	}
	checks := map[string]any{
		"daily_chat_budget":                    600,
		"daily_chat_budget_default":            600,
		"daily_chat_budget_max":                7200,
		"daily_chat_web_timeout":               120,
		"daily_chat_web_timeout_default":       120,
		"daily_chat_web_timeout_max":           3600,
		"daily_chat_web_retry_backoff":         5,
		"daily_chat_web_retry_backoff_default": 5,
		"daily_chat_web_retry_backoff_max":     60,
		"daily_chat_models_max":                8,
		"daily_chat_web_concurrency":           1,
		"daily_chat_web_concurrency_default":   1,
		"daily_chat_web_concurrency_max":       10,
		"daily_chat_web_fallback_activity":     false,
	}
	for key, want := range checks {
		if got := view[key]; got != want {
			t.Fatalf("%s = %#v, want %#v", key, got, want)
		}
	}
	if models, _ := view["daily_chat_models"].([]string); len(models) != 0 {
		t.Fatalf("default daily_chat_models = %#v, want empty", view["daily_chat_models"])
	}

	patch := map[string]any{
		"daily_chat_budget":                900,
		"daily_chat_web_timeout":           180,
		"daily_chat_web_retry_backoff":     10,
		"daily_chat_models":                []any{"model-b", "model-a", "model-b"},
		"daily_chat_web_concurrency":       3,
		"daily_chat_web_fallback_activity": true,
	}
	if err := provider.SaveSettings(context.Background(), patch); err != nil {
		t.Fatalf("save daily chat settings: %v", err)
	}
	view, _ = provider.SettingsView(context.Background())
	if got := view["daily_chat_budget"]; got != 900 {
		t.Fatalf("saved budget = %#v, want 900", got)
	}
	if got := view["daily_chat_web_timeout"]; got != 180 {
		t.Fatalf("saved timeout = %#v, want 180", got)
	}
	if got := view["daily_chat_web_retry_backoff"]; got != 10 {
		t.Fatalf("saved backoff = %#v, want 10", got)
	}
	if got := view["daily_chat_web_fallback_activity"]; got != true {
		t.Fatalf("saved fallback = %#v, want true", got)
	}
	if got := view["daily_chat_web_concurrency"]; got != 3 {
		t.Fatalf("saved concurrency = %#v, want 3", got)
	}
	if models, _ := view["daily_chat_models"].([]string); len(models) != 2 || models[0] != "model-b" || models[1] != "model-a" {
		t.Fatalf("saved models = %#v, want ordered deduplicated list", view["daily_chat_models"])
	}

	for key, value := range map[string]any{
		"daily_chat_budget":                -1,
		"daily_chat_web_timeout":           0,
		"daily_chat_web_retry_backoff":     61,
		"daily_chat_web_concurrency":       0,
		"daily_chat_web_fallback_activity": "true",
	} {
		if err := provider.SaveSettings(context.Background(), map[string]any{key: value}); err == nil {
			t.Fatalf("%s accepted %#v", key, value)
		}
	}
}

func TestDailyChatModelsUsesGlobalSettingAndPerAccountOverride(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-models", realmIntl, nil)
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{
		"daily_chat_models": []any{"global-a", "global-b"},
	})
	account := accountByUID(t, provider.Pool(), "intl-models")
	account.mu.RLock()
	auth := account.Auth
	account.mu.RUnlock()
	if got := provider.dailyChatModels(auth); len(got) != 2 || got[0] != "global-a" || got[1] != "global-b" {
		t.Fatalf("global models = %#v", got)
	}

	account.mu.Lock()
	if account.Auth.Extra == nil {
		account.Auth.Extra = map[string]json.RawMessage{}
	}
	account.Auth.Extra["dailyChatModels"] = json.RawMessage(`["account-model"]`)
	auth = account.Auth
	account.mu.Unlock()
	if got := provider.dailyChatModels(auth); len(got) != 1 || got[0] != "account-model" {
		t.Fatalf("per-account models = %#v", got)
	}
}

func TestDailyChatBudgetAndTimingSettings(t *testing.T) {
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-timing", realmIntl, nil)
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{
		"daily_chat_budget":            900,
		"daily_chat_web_timeout":       180,
		"daily_chat_web_retry_backoff": 10,
	})
	if got := provider.dailyChatBudgetDuration(); got != 900*time.Second {
		t.Fatalf("budget = %s, want 900s", got)
	}
	if got := provider.dailyChatTurnWaitDuration(); got != 180*time.Second {
		t.Fatalf("turn wait = %s, want 180s", got)
	}
	if got := provider.taskBackoffDelay(0); got != 10*time.Second {
		t.Fatalf("backoff round 0 = %s, want 10s", got)
	}
	if got := provider.taskBackoffDelay(1); got != 20*time.Second {
		t.Fatalf("backoff round 1 = %s, want 20s", got)
	}

	if _, err := provider.store.Merge(map[string]any{"daily_chat_budget": 0}); err != nil {
		t.Fatalf("save zero budget: %v", err)
	}
	if got := provider.dailyChatBudgetDuration(); got != 0 {
		t.Fatalf("zero budget = %s, want unlimited", got)
	}

	// The explicit test seam still wins when no panel value is stored.
	provider.tasksBudget = time.Second
	if _, err := provider.store.Update(func(data map[string]any) error {
		delete(data, "daily_chat_budget")
		return nil
	}); err != nil {
		t.Fatalf("clear budget: %v", err)
	}
	if got := provider.dailyChatBudgetDuration(); got != time.Second {
		t.Fatalf("test-seam budget = %s, want 1s", got)
	}
}

func TestDailyChatWebTLSFallbackActivity(t *testing.T) {
	f := newJobsFixture(t)
	account := f.addIntlTaskAccount(t, "intl-tls-fallback", true, true)
	if _, err := f.store.Merge(map[string]any{
		"daily_chat_web_fallback_activity": true,
	}); err != nil {
		t.Fatalf("save fallback setting: %v", err)
	}
	f.intl.dailyWebRun = func(context.Context, *Account, string) (DailyResult, error) {
		return DailyResult{Error: "SSLCertVerificationError: certificate verify failed: unable to get local issuer certificate"}, nil
	}

	result, err := f.intl.runDailyActivity(context.Background(), account, taskTriggerScheduler)
	if err != nil {
		t.Fatalf("runDailyActivity: %v", err)
	}
	if !result.OK {
		t.Fatalf("fallback result = %+v, want OK", result)
	}
	if !strings.Contains(result.Msg, "回退") {
		t.Fatalf("fallback message = %q, want fallback explanation", result.Msg)
	}
	if got := f.fake.count(http.MethodPost, "/v2/report"); got != 1 {
		t.Fatalf("activity report posts = %d, want 1", got)
	}
	account.mu.RLock()
	last := account.Auth.LastDailyChat
	account.mu.RUnlock()
	if !strings.HasPrefix(last, time.Now().Format("2006-01-02")) {
		t.Fatalf("lastDailyChat = %q, want today", last)
	}
}

func TestDailyChatWebTLSFallbackOnlyWhenEnabledAndTLS(t *testing.T) {
	f := newJobsFixture(t)
	account := f.addIntlTaskAccount(t, "intl-no-fallback", true, true)
	f.intl.dailyWebRun = func(context.Context, *Account, string) (DailyResult, error) {
		return DailyResult{Error: "certificate verify failed"}, nil
	}
	result, err := f.intl.runDailyActivity(context.Background(), account, taskTriggerScheduler)
	if err != nil {
		t.Fatalf("runDailyActivity: %v", err)
	}
	if result.OK {
		t.Fatalf("disabled fallback result = %+v, want failure", result)
	}
	if got := f.fake.count(http.MethodPost, "/v2/report"); got != 0 {
		t.Fatalf("disabled fallback sent %d report(s)", got)
	}

	if _, err := f.store.Merge(map[string]any{"daily_chat_web_fallback_activity": true}); err != nil {
		t.Fatalf("save fallback setting: %v", err)
	}
	f.intl.dailyWebRun = func(context.Context, *Account, string) (DailyResult, error) {
		return DailyResult{Error: "network timeout"}, nil
	}
	result, err = f.intl.runDailyActivity(context.Background(), account, taskTriggerScheduler)
	if err != nil {
		t.Fatalf("runDailyActivity non-TLS: %v", err)
	}
	if result.OK {
		t.Fatalf("non-TLS fallback result = %+v, want failure", result)
	}
	if got := f.fake.count(http.MethodPost, "/v2/report"); got != 0 {
		t.Fatalf("non-TLS fallback sent %d report(s)", got)
	}
}

func TestJobsDailyConcurrencyRunsAccountsInParallel(t *testing.T) {
	f := newJobsFixture(t)
	for i := 0; i < 3; i++ {
		f.addIntlTaskAccount(t, fmt.Sprintf("intl-conc-%d", i), true, true)
	}
	if _, err := f.store.Merge(map[string]any{
		"daily_chat_web":             false,
		"daily_chat_web_concurrency": 3,
	}); err != nil {
		t.Fatalf("save concurrency setting: %v", err)
	}
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	f.fake.handler = func(_ *taskTestUpstream, r *http.Request, _ []byte) (int, any) {
		if r.URL.Path == chatPath {
			started <- struct{}{}
			<-release
			return http.StatusOK, map[string]any{"choices": []any{}}
		}
		return http.StatusOK, map[string]any{"code": 0, "data": map[string]any{}}
	}
	f.setNow(time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local))

	done := make(chan struct{})
	var report JobRunResult
	var runErr error
	go func() {
		report, runErr = jobsByName(t, f.intl)["workbuddy-daily-activity"].run(context.Background(), false)
		close(done)
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 3 daily requests started; concurrency setting was not honored", i)
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("daily job did not finish")
	}
	if runErr != nil {
		t.Fatalf("daily job: %v", runErr)
	}
	if report.Succeeded != 3 || report.Failed != 0 || report.Checked != 3 {
		t.Fatalf("concurrent daily report = %+v, want 3 succeeded", report)
	}
}
