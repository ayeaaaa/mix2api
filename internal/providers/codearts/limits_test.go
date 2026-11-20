package codearts

import (
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

var _ providers.LimitApplier = (*Provider)(nil)

func limitValues(reserve, dailyToken, dailyCredit, modelDailyToken int) map[string]any {
	return map[string]any{
		"reserve_credits":         map[string]any{"global": reserve},
		"daily_token_limit":       map[string]any{"global": dailyToken},
		"daily_credit_limit":      map[string]any{"global": dailyCredit},
		"model_daily_token_limit": map[string]any{"global": modelDailyToken},
	}
}

func TestPoolApplyLimitsReserveBlocksDispatch(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u-limits", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 2})
	account := p.Get("u-limits")
	account.SetInfo(accountInfo(900, 0))

	p.ApplyLimits(limitValues(1000, 0, 0, 0), nil)
	if got := account.View().Guard; got != "reserve" {
		t.Fatalf("guard = %q, want reserve", got)
	}
	if _, err := p.Pick("model-a", "", nil); err == nil {
		t.Fatal("Pick returned a reserve-blocked account")
	}
}

func TestPoolApplyLimitsDailyTokenBlocksDispatch(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u-limits", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 2})
	account := p.Get("u-limits")
	account.SetInfo(accountInfo(5000, 0))

	p.ApplyLimits(limitValues(0, 100, 0, 0), map[string]any{
		"tokens": map[string]any{"u-limits": 120},
	})
	if got := account.View().Guard; got != "daily_token" {
		t.Fatalf("guard = %q, want daily_token", got)
	}
	if _, err := p.Pick("model-a", "", nil); err == nil {
		t.Fatal("Pick returned a daily-token-blocked account")
	}
}

func TestPoolApplyLimitsModelDailyTokenOnlyBlocksThatModel(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u-limits", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 2})
	account := p.Get("u-limits")
	account.SetInfo(accountInfo(5000, 0))

	p.ApplyLimits(limitValues(0, 0, 0, 50), map[string]any{
		"models": map[string]any{"u-limits": map[string]any{"model-a": 60}},
	})
	if got := account.View().Guard; got != "" {
		t.Fatalf("general guard = %q, want empty (model-specific only)", got)
	}
	if _, err := p.Pick("model-a", "", nil); err == nil {
		t.Fatal("Pick returned an account over the model daily token limit")
	}
	if picked, err := p.Pick("model-b", "", nil); err != nil || picked == nil {
		t.Fatalf("Pick(model-b) = %v, %v, want the account", picked, err)
	}
}

func TestPoolApplyLimitsDailyCreditAllowsBenefitModels(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u-limits", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 2})
	account := p.Get("u-limits")
	account.SetInfo(accountInfo(5000, 0))
	account.SetModelBenefit(map[string]bool{"free-model": true})

	p.ApplyLimits(limitValues(0, 0, 10, 0), map[string]any{
		"credits": map[string]any{"u-limits": 12},
	})
	if got := account.View().Guard; got != "daily_credit" {
		t.Fatalf("general guard = %q, want daily_credit", got)
	}
	if _, err := p.Pick("paid-model", "", nil); err == nil {
		t.Fatal("Pick returned an account over the daily credit limit for a paid model")
	}
	if picked, err := p.Pick("free-model", "", nil); err != nil || picked == nil {
		t.Fatalf("Pick(free-model) = %v, %v, want the account", picked, err)
	}
}

func TestPoolApplyLimitsNilUsageKeepsLastCounters(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u-limits", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 2})
	account := p.Get("u-limits")
	account.SetInfo(accountInfo(5000, 0))

	values := limitValues(0, 100, 0, 0)
	p.ApplyLimits(values, map[string]any{
		"tokens": map[string]any{"u-limits": 120},
	})
	if got := account.View().Guard; got != "daily_token" {
		t.Fatalf("guard before nil usage = %q, want daily_token", got)
	}
	p.ApplyLimits(values, nil)
	if got := account.View().Guard; got != "daily_token" {
		t.Fatalf("guard after nil usage = %q, want counters preserved", got)
	}
}

func TestProviderApplyLimitsAndSettingsReload(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u-provider", "")
	store := config.New(filepath.Join(dir, "settings.json"))
	provider, err := New(store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	account := provider.Pool().Get("u-provider")
	account.SetInfo(accountInfo(900, 0))

	provider.ApplyLimits(limitValues(1000, 0, 0, 0), nil)
	if got := account.View().Guard; got != "reserve" {
		t.Fatalf("provider ApplyLimits guard = %q, want reserve", got)
	}

	account.SetInfo(accountInfo(5000, 0))
	if _, err := store.Merge(map[string]any{
		"limits": map[string]any{
			"reserve_credits": map[string]any{"global": 1000},
		},
	}); err != nil {
		t.Fatalf("save limits: %v", err)
	}
	if _, err := provider.applySettings(); err != nil {
		t.Fatalf("applySettings: %v", err)
	}
	account.SetInfo(accountInfo(900, 0))
	if got := account.View().Guard; got != "reserve" {
		t.Fatalf("settings reload guard = %q, want reserve", got)
	}
}

func TestPoolApplyLimitsProjectsToNewAccounts(t *testing.T) {
	dir := t.TempDir()
	writeTestAuth(t, dir, "u-existing", "")
	p := newTestPool(t, dir, Config{MaxConcurrent: 2})
	p.ApplyLimits(limitValues(1000, 0, 0, 0), nil)

	account, err := p.Add(testAuth("u-new"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	account.SetInfo(accountInfo(900, 0))
	if got := account.View().Guard; got != "reserve" {
		t.Fatalf("new account guard = %q, want reserve", got)
	}
}
