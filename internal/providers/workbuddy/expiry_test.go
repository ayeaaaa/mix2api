package workbuddy

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func expiryNumber(value any) float64 {
	switch typed := value.(type) {
	case json.Number:
		number, _ := typed.Float64()
		return number
	case float64:
		return typed
	case int:
		return float64(typed)
	}
	return 0
}

func TestAccountExpirySummaryCountsAllFinitePackages(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	account := &Account{
		ExpiringWindowDays: 7,
		Credits: &CreditSnapshot{Packages: []CreditPackage{
			{Name: "within", PackageCode: "a", Remain: 100, ExpireTime: now.AddDate(0, 0, 2).Format(creditsTimeLayout), DaysLeft: creditTestFloat(2)},
			{Name: "later", PackageCode: "b", Remain: 50, ExpireTime: now.AddDate(0, 0, 20).Format(creditsTimeLayout), DaysLeft: creditTestFloat(20)},
			{Name: "dated-no-days", PackageCode: "d", Remain: 5, CycleEndTime: now.AddDate(0, 0, 19).Format(creditsTimeLayout)},
			{Name: "no-expiry", PackageCode: "free", Remain: 100, NoExpiry: true},
			{Name: "expired", PackageCode: "x", Remain: 20, ExpireTime: now.AddDate(0, 0, -1).Format(creditsTimeLayout), DaysLeft: creditTestFloat(-1), IsExpired: true},
			{Name: "enterprise", PackageCode: "enterprise", Remain: 30, ExpireTime: now.AddDate(0, 0, 2).Format(creditsTimeLayout), DaysLeft: creditTestFloat(2)},
			{Name: "zero", PackageCode: "z", Remain: 0, ExpireTime: now.AddDate(0, 0, 2).Format(creditsTimeLayout), DaysLeft: creditTestFloat(2)},
		}},
	}

	summary := account.expirySummaryAt(now)
	if summary == nil {
		t.Fatal("ExpirySummary returned nil")
	}
	if summary.Credits != 155 || summary.Packages != 3 {
		t.Fatalf("all-expiry summary = %+v, want credits=155 packages=3", summary)
	}
	if summary.WithinWindow != 100 || summary.WithinPackages != 1 {
		t.Fatalf("window summary = %+v, want within=100 packages=1", summary)
	}
	if summary.WindowDays != 7 {
		t.Fatalf("window days = %d, want 7", summary.WindowDays)
	}

	if got := (&Account{}).ExpirySummary(); got != nil {
		t.Fatalf("account without credits = %+v, want nil", got)
	}
}

func TestAccountViewIncludesExpirySummary(t *testing.T) {
	account := &Account{
		Auth:               Auth{UID: "expiry-view", Realm: realmCN, Platform: "CLI", Source: sourceOAuth},
		ExpiringWindowDays: 7,
		Credits: &CreditSnapshot{Packages: []CreditPackage{
			{Name: "within", PackageCode: "a", Remain: 100, ExpireTime: time.Now().AddDate(0, 0, 2).Format(creditsTimeLayout), DaysLeft: creditTestFloat(2)},
			{Name: "later", PackageCode: "b", Remain: 50, ExpireTime: time.Now().AddDate(0, 0, 20).Format(creditsTimeLayout), DaysLeft: creditTestFloat(20)},
		}},
	}

	view := account.View()
	if view.Expiring == nil {
		t.Fatal("AccountView.Expiring is nil")
	}
	if view.Expiring.Credits != 150 || view.Expiring.Packages != 2 ||
		view.Expiring.WithinWindow != 100 || view.Expiring.WithinPackages != 1 {
		t.Fatalf("view expiring = %+v", view.Expiring)
	}
}

func TestCreditsDetailIncludesExpirySummary(t *testing.T) {
	f := newWBRoutesFixture(t)
	account := wbRouteAddAccount(t, f.cn, "uid-expiry", realmCN, nil)
	f.cn.Pool().ApplyExpiringWindow(7)
	account.mu.Lock()
	account.ExpiringWindowDays = 7
	account.mu.Unlock()
	if err := f.cn.Pool().SetCredits("uid-expiry", &CreditSnapshot{Packages: []CreditPackage{
		{Name: "within", PackageCode: "a", Remain: 100, ExpireTime: time.Now().AddDate(0, 0, 2).Format(creditsTimeLayout), DaysLeft: creditTestFloat(2)},
		{Name: "later", PackageCode: "b", Remain: 50, ExpireTime: time.Now().AddDate(0, 0, 20).Format(creditsTimeLayout), DaysLeft: creditTestFloat(20)},
		{Name: "no-expiry", PackageCode: "free", Remain: 100, NoExpiry: true},
	}}); err != nil {
		t.Fatalf("seed credits: %v", err)
	}

	rec := f.do(http.MethodGet, "/accounts/credits/detail?realm=cn&uid=uid-expiry", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", rec.Code, rec.Body)
	}
	detail := decodeWBRoutesJSON(t, rec)
	expiring, _ := detail["expiring"].(map[string]any)
	if expiring == nil {
		t.Fatalf("detail expiring = %#v, want summary", detail)
	}
	if expiryNumber(expiring["credits"]) != 150 || expiryNumber(expiring["packages"]) != 2 ||
		expiryNumber(expiring["within_window"]) != 100 ||
		expiryNumber(expiring["within_packages"]) != 1 ||
		expiryNumber(expiring["window_days"]) != 7 {
		t.Fatalf("detail expiring = %#v", expiring)
	}
}

func TestExpirySummaryRecomputesStaleDaysLeft(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	account := &Account{
		ExpiringWindowDays: 7,
		Credits: &CreditSnapshot{Packages: []CreditPackage{{
			Name: "stale", PackageCode: "a", Remain: 100,
			ExpireTime: now.AddDate(0, 0, 2).Format(creditsTimeLayout),
			DaysLeft:   creditTestFloat(30),
		}}},
	}
	summary := account.expirySummaryAt(now)
	if summary == nil || summary.WithinWindow != 100 || summary.WithinPackages != 1 {
		t.Fatalf("stale days_left must be recomputed from expire_time: %+v", summary)
	}
	if days, _, ok := account.expiringPackageAt(now); !ok || days != 2 {
		t.Fatalf("scheduler days = %v ok=%v, want 2 true", days, ok)
	}
}

func TestExpirySummaryDropsPackageAfterExpiry(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	account := &Account{
		ExpiringWindowDays: 7,
		Credits: &CreditSnapshot{Packages: []CreditPackage{{
			Name: "soon", PackageCode: "a", Remain: 100,
			ExpireTime: now.Add(time.Hour).Format(creditsTimeLayout),
			DaysLeft:   creditTestFloat(0.1),
		}}},
	}
	if summary := account.expirySummaryAt(now); summary == nil || summary.Credits != 100 {
		t.Fatalf("before expiry summary = %+v", summary)
	}
	if summary := account.expirySummaryAt(now.Add(2 * time.Hour)); summary != nil {
		t.Fatalf("after expiry summary = %+v, want nil", summary)
	}
	if _, _, ok := account.expiringPackageAt(now.Add(2 * time.Hour)); ok {
		t.Fatal("scheduler still picked an expired package")
	}
}

func TestExpirySummaryRejectsInvalidAndNoExpiryDates(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	account := &Account{
		ExpiringWindowDays: 7,
		Credits: &CreditSnapshot{Packages: []CreditPackage{
			{Name: "invalid", PackageCode: "a", Remain: 10, ExpireTime: "-", DaysLeft: creditTestFloat(2)},
			{Name: "no-expiry", PackageCode: "b", Remain: 20, NoExpiry: true},
			{Name: "enterprise", PackageCode: "Enterprise", Remain: 30, ExpireTime: now.AddDate(0, 0, 2).Format(creditsTimeLayout)},
			{Name: "zero", PackageCode: "c", Remain: 0, ExpireTime: now.AddDate(0, 0, 2).Format(creditsTimeLayout)},
		}},
	}
	if summary := account.expirySummaryAt(now); summary != nil {
		t.Fatalf("summary = %+v, want nil", summary)
	}
}

func TestExpirySummaryBoundaryAtWindowEnd(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.Local)
	account := &Account{
		ExpiringWindowDays: 7,
		Credits: &CreditSnapshot{Packages: []CreditPackage{{
			Name: "boundary", PackageCode: "a", Remain: 42,
			ExpireTime: now.Add(7 * 24 * time.Hour).Format(creditsTimeLayout),
		}}},
	}
	summary := account.expirySummaryAt(now)
	if summary == nil || summary.WithinWindow != 42 || summary.WithinPackages != 1 {
		t.Fatalf("boundary summary = %+v, want within=42 packages=1", summary)
	}
}
