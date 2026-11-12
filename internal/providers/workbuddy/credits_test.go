package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const creditTestLayout = "2006-01-02 15:04:05"

func creditTestNow() time.Time {
	return time.Date(2026, 11, 9, 12, 0, 0, 0, time.Local)
}

func creditTestFloat(value float64) *float64 { return &value }

func creditTestInt64(value int64) *int64 { return &value }

func creditTestMillis(at time.Time) float64 {
	return float64(at.UnixNano()) / float64(time.Millisecond)
}

func creditTestJSON(t *testing.T, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(raw)
}

type creditsTestCall struct {
	Method         string
	Path           string
	Body           string
	Authorization  string
	UserID         string
	APIKey         string
	ClientPlatform string
	DeviceToken    string
	MachineID      string
	SessionID      string
	RequestID      string
}

type creditsTestServer struct {
	mu      sync.Mutex
	calls   []creditsTestCall
	respond func(*http.Request) (int, string)
}

func (s *creditsTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	call := creditsTestCall{
		Method:         r.Method,
		Path:           r.URL.Path,
		Body:           string(raw),
		Authorization:  r.Header.Get("Authorization"),
		UserID:         r.Header.Get("X-User-Id"),
		APIKey:         r.Header.Get("X-API-Key"),
		ClientPlatform: r.Header.Get("X-Client-Platform"),
		DeviceToken:    r.Header.Get("X-Device-Token"),
		MachineID:      r.Header.Get("X-Machine-ID"),
		SessionID:      r.Header.Get("X-Session-ID"),
		RequestID:      r.Header.Get("X-Request-ID"),
	}

	// The fixture records the body for assertions; restore it so a handler can
	// branch on request fields such as PageNumber.
	r.Body = io.NopCloser(bytes.NewReader(raw))

	s.mu.Lock()
	s.calls = append(s.calls, call)
	respond := s.respond
	s.mu.Unlock()

	status, body := http.StatusOK, ""
	if respond != nil {
		status, body = respond(r)
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (s *creditsTestServer) snapshot() []creditsTestCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]creditsTestCall(nil), s.calls...)
}

func newCreditsTestServer(t *testing.T, respond func(*http.Request) (int, string)) (*httptest.Server, *creditsTestServer) {
	t.Helper()
	fixture := &creditsTestServer{respond: respond}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	return server, fixture
}

func newCreditsPool(t *testing.T, realm string, fields map[string]map[string]any) (*Provider, *Pool, string) {
	t.Helper()
	dir := t.TempDir()
	writePoolSettings(t, dir, map[string]any{})
	for uid, values := range fields {
		writePoolAccount(t, dir, uid, realm, values)
	}
	provider, pool := newPoolProvider(t, realm, dir)
	return provider, pool, dir
}

func creditCallsWithPath(calls []creditsTestCall, path string) []creditsTestCall {
	out := make([]creditsTestCall, 0, len(calls))
	for _, call := range calls {
		if call.Path == path {
			out = append(out, call)
		}
	}
	return out
}

func TestCreditsParsePackageNoExpiryExpiredAndSort(t *testing.T) {
	now := creditTestNow()
	farFuture := creditTestMillis(now.Add(800 * 24 * time.Hour))
	created := now.Add(-48 * time.Hour)

	noExpiry := parseCreditPackage(map[string]any{
		"PackageName":              "Free Plan Subscription",
		"PackageCode":              "free-plan",
		"ProductName":              "WorkBuddy",
		"SubProductName":           "Free",
		"ResourceId":               "res-free",
		"DealName":                 "deal-free",
		"CycleCapacitySizePrecise": "100",
		"CycleCapacityRemain":      "100",
		"DeductionEndTime":         farFuture,
		"CycleEndTime":             now.Add(24 * time.Hour).Format(creditTestLayout),
		"CreateTime":               creditTestMillis(created),
		"AutoRenewFlag":            true,
		"Status":                   3,
		"AccountAttributes": []any{
			map[string]any{"Key": "other", "Value": "ignored"},
			map[string]any{"Key": "grantReason", "Value": "官方活动发放"},
		},
	}, now)
	if !noExpiry.NoExpiry || noExpiry.DaysLeft != nil || noExpiry.IsExpired {
		t.Fatalf("no-expiry package = %+v", noExpiry)
	}
	if noExpiry.Name != "Free Plan Subscription" || noExpiry.PackageCode != "free-plan" ||
		noExpiry.ProductName != "WorkBuddy" || noExpiry.SubProductName != "Free" ||
		noExpiry.ResourceID != "res-free" || noExpiry.DealName != "deal-free" {
		t.Fatalf("package identity fields = %+v", noExpiry)
	}
	if noExpiry.GrantReason != "官方活动发放" || !noExpiry.AutoRenew || noExpiry.Status != 3 {
		t.Fatalf("package metadata = %+v", noExpiry)
	}
	if noExpiry.CreateTime != created.Format(creditTestLayout) {
		t.Fatalf("create_time = %q", noExpiry.CreateTime)
	}
	if noExpiry.ExpireTime == "" || noExpiry.CycleEndTime == "" {
		t.Fatalf("expiry fields missing: %+v", noExpiry)
	}
	if noExpiry.Remain != 100 || noExpiry.Used != 0 || noExpiry.Size != 100 || noExpiry.Unit != "credits" {
		t.Fatalf("capacity fields = %+v", noExpiry)
	}
	if noExpiry.InUsage {
		t.Fatalf("unused package reported in_usage: %+v", noExpiry)
	}

	expired := parseCreditPackage(map[string]any{
		"PackageName":       "Expired Pack",
		"PackageCode":       "expired",
		"CycleCapacitySize": 5,
		"CapacityRemain":    5,
		"CycleEndTime":      now.Add(-2 * time.Hour).Format(creditTestLayout),
	}, now)
	if !expired.IsExpired || expired.DaysLeft == nil || *expired.DaysLeft >= 0 {
		t.Fatalf("expired package = %+v", expired)
	}
	if expired.InUsage {
		t.Fatalf("expired package was inferred in_usage: %+v", expired)
	}
	explicitUsage := parseCreditPackage(map[string]any{
		"PackageCode":       "explicit",
		"CycleCapacitySize": 10,
		"CapacityRemain":    10,
		"InUsage":           true,
		"CycleEndTime":      now.Add(24 * time.Hour).Format(creditTestLayout),
	}, now)
	if !explicitUsage.InUsage {
		t.Fatalf("explicit in_usage was lost: %+v", explicitUsage)
	}

	soon := parseCreditPackage(map[string]any{
		"PackageName":       "Soon",
		"PackageCode":       "soon",
		"CycleCapacitySize": 100,
		"CapacityRemain":    40,
		"CapacityUsed":      60,
		"DeductionEndTime":  creditTestMillis(now.Add(24 * time.Hour)),
		"SupportAutoRenew":  true,
	}, now)
	if soon.IsExpired || soon.NoExpiry || soon.DaysLeft == nil || *soon.DaysLeft != 1 {
		t.Fatalf("soon package = %+v", soon)
	}
	if !soon.InUsage {
		t.Fatalf("partially consumed package should be in_usage: %+v", soon)
	}
	if !soon.AutoRenew {
		t.Fatalf("SupportAutoRenew was not honoured: %+v", soon)
	}

	later := parseCreditPackage(map[string]any{
		"PackageCode":      "later",
		"DeductionEndTime": creditTestMillis(now.Add(5 * 24 * time.Hour)),
		"CapacitySize":     50,
		"CapacityRemain":   25,
		"CapacityUsed":     25,
	}, now)
	unknown := parseCreditPackage(map[string]any{
		"PackageName":       0,
		"ProductName":       0,
		"PackageCode":       0,
		"DeductionEndTime":  "not-a-timestamp",
		"CycleEndTime":      "also-not-a-timestamp",
		"Status":            "weird",
		"AccountAttributes": "not-a-list",
	}, now)
	if unknown.Name != "Package" || unknown.ProductName != "" || unknown.PackageCode != "" ||
		unknown.DaysLeft != nil || unknown.IsExpired || unknown.NoExpiry {
		t.Fatalf("unknown package = %+v", unknown)
	}
	if unknown.Status != "weird" {
		t.Fatalf("string status was not preserved: %#v", unknown.Status)
	}
	stringStatus := parseCreditPackage(map[string]any{
		"PackageCode": "status-string",
		"Status":      "3",
	}, now)
	if stringStatus.Status != "3" {
		t.Fatalf("numeric-looking string status was not preserved: %#v", stringStatus.Status)
	}

	packages := []CreditPackage{expired, unknown, noExpiry, later, soon}
	sortCreditPackages(packages)
	got := make([]string, 0, len(packages))
	for _, pkg := range packages {
		got = append(got, pkg.PackageCode)
	}
	want := []string{"soon", "later", "free-plan", "", "expired"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("package order = %v, want %v", got, want)
	}
}

func TestCreditsSnapshotTotalsEarliestCheckinAndISO(t *testing.T) {
	now := creditTestNow()
	checkin := &CheckinStatus{
		TodayCheckedIn: true,
		StreakDays:     4,
		DailyCredit:    10,
		TodayCredit:    10,
		Active:         true,
	}
	packages := []CreditPackage{
		{Name: "A", PackageCode: "a", Remain: 60, Used: 40, Size: 100, DaysLeft: creditTestFloat(5)},
		{Name: "B", PackageCode: "b", Remain: 20, Used: 30, Size: 50, DaysLeft: creditTestFloat(2)},
		{Name: "Expired", PackageCode: "x", Remain: 7, Used: 3, Size: 10, DaysLeft: creditTestFloat(-1), IsExpired: true},
		{Name: "NoExpiry", PackageCode: "n", Remain: 9, Used: 1, Size: 10, NoExpiry: true},
	}
	snapshot := newCreditSnapshot(packages, true, checkin, now)
	if snapshot.Remain != 96 || snapshot.Used != 74 || snapshot.Size != 170 {
		t.Fatalf("totals = %v/%v/%v, want 96/74/170", snapshot.Remain, snapshot.Used, snapshot.Size)
	}
	if snapshot.UsedPercent != "43.5%" || snapshot.RemainPercent != "56.5%" {
		t.Fatalf("percentages = %q/%q", snapshot.UsedPercent, snapshot.RemainPercent)
	}
	if !snapshot.IsPaidUser || snapshot.Checkin == nil || snapshot.Checkin.StreakDays != 4 {
		t.Fatalf("snapshot metadata = %+v", snapshot)
	}
	if snapshot.EarliestExpiring == nil || snapshot.EarliestExpiring.Name != "B" ||
		snapshot.EarliestExpiring.PackageCode != "b" ||
		snapshot.EarliestExpiring.Remain != 20 ||
		snapshot.EarliestExpiring.DaysLeft != 2 {
		t.Fatalf("earliest expiring = %+v", snapshot.EarliestExpiring)
	}
	if snapshot.UpdatedAt != float64(now.Unix()) || snapshot.UpdatedISO != now.Format(creditTestLayout) {
		t.Fatalf("updated fields = %v/%q", snapshot.UpdatedAt, snapshot.UpdatedISO)
	}
	if len(snapshot.Packages) != 4 || snapshot.Packages[0].PackageCode != "b" {
		t.Fatalf("packages were not sorted: %+v", snapshot.Packages)
	}
}

func TestCreditsSnapshotCloneDeepCopiesPointers(t *testing.T) {
	original := &CreditSnapshot{
		Packages: []CreditPackage{
			{Name: "A", PackageCode: "a", Remain: 1, DaysLeft: creditTestFloat(2)},
		},
		Checkin:          &CheckinStatus{TodayCheckedIn: true, StreakDays: 4, Active: true},
		EarliestExpiring: &EarliestExpiring{Name: "A", PackageCode: "a", Remain: 1, DaysLeft: 2},
	}
	copied := cloneCreditSnapshot(original)
	*copied.Packages[0].DaysLeft = 99
	copied.Packages[0].Name = "changed"
	copied.Packages[0].Remain = 55
	copied.Checkin.StreakDays = 123
	copied.EarliestExpiring.Name = "changed"
	if *original.Packages[0].DaysLeft != 2 || original.Packages[0].Name != "A" ||
		original.Packages[0].Remain != 1 || original.Checkin.StreakDays != 4 ||
		original.EarliestExpiring.Name != "A" {
		t.Fatalf("clone aliased the original: %+v", original)
	}
}

func TestCreditsCNRefreshPersistsTierAndUsage(t *testing.T) {
	now := creditTestNow()
	farFuture := creditTestMillis(now.AddDate(10, 0, 0))
	summaryBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"IsPaidUser":                true,
			"SubscriptionPackageName":   "WorkBuddy Pro",
			"SubscriptionPackageCode":   "pro",
			"SubscriptionPackageNameCN": "",
			"SubscriptionPackageCodeCN": "",
			"ProTrialStatus":            1,
			"IsProtectedPriceUser":      true,
			"Packages": []any{
				map[string]any{"PackageCode": "pkg_free"},
				map[string]any{"PackageCode": "pkg_paid"},
				map[string]any{"PackageCode": ""},
			},
		},
	})
	paidRaw := map[string]any{
		"AccountId":                "p1",
		"PackageName":              "Pro Pack",
		"PackageCode":              "pkg_paid",
		"ProductName":              "WorkBuddy",
		"SubProductName":           "Pro",
		"ResourceId":               "res-paid",
		"CycleCapacitySizePrecise": "200",
		"CycleCapacityRemain":      "150",
		"CycleCapacityUsed":        "50",
		"InUsage":                  true,
		"AutoRenewFlag":            true,
		"DeductionEndTime":         creditTestMillis(now.Add(48 * time.Hour)),
		"CycleStartTime":           now.Add(-30 * 24 * time.Hour).Format(creditTestLayout),
		"CycleEndTime":             now.Add(2 * 24 * time.Hour).Format(creditTestLayout),
		"CreateTime":               creditTestMillis(now.Add(-24 * time.Hour)),
		"Status":                   0,
	}
	freeRaw := map[string]any{
		"AccountId":                "f1",
		"PackageName":              "Free Plan Subscription",
		"PackageCode":              "pkg_free",
		"SubProductName":           "Free",
		"CycleCapacitySizePrecise": "100",
		"CycleCapacityRemain":      "100",
		"DeductionEndTime":         farFuture,
		"CycleEndTime":             now.AddDate(10, 0, 0).Format(creditTestLayout),
		"AccountAttributes": []any{
			map[string]any{"Key": "grantReason", "Value": "官方活动发放"},
		},
		"Status": 0,
	}
	// A duplicate AccountId across the free and paid endpoints must collapse
	// into one package.
	freeBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{"Accounts": []any{freeRaw, paidRaw}},
	})
	paidBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{"Accounts": []any{paidRaw}},
	})
	checkinBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"today_checked_in": true,
			"streak_days":      4,
			"daily_credit":     10,
			"today_credit":     10,
			"active":           true,
		},
	})

	server, fixture := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		switch r.URL.Path {
		case creditsSummaryPath:
			return http.StatusOK, summaryBody
		case creditsFreePackagesPath:
			return http.StatusOK, freeBody
		case creditsPaidPackagesPath:
			return http.StatusOK, paidBody
		case creditsCheckinPath:
			return http.StatusOK, checkinBody
		default:
			return http.StatusNotFound, `{"code":404}`
		}
	})

	provider, pool, dir := newCreditsPool(t, realmCN, map[string]map[string]any{
		"cn-a": nil,
	})
	writePoolSettings(t, dir, map[string]any{
		"upstream": map[string]any{"device_token": "device-secret-value"},
	})
	provider.creditsBillingBaseURL = server.URL
	provider.creditsCheckinBaseURL = server.URL
	provider.creditsNow = func() time.Time { return now }

	// The daily usage fold is published before the credits refresh; a credits
	// refresh must not erase it.
	provider.ApplyLimits(map[string]any{
		"daily_token_limit": map[string]any{"global": 1000, "cn": 2000},
	}, map[string]any{
		"tokens": map[string]any{"cn-a": 7},
	})

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("result rows = %+v", result.Results)
	}
	row := result.Results[0]
	if !row.OK || row.UID != "cn-a" || row.Error != "" || row.Credits == nil {
		t.Fatalf("refresh row = %+v", row)
	}
	snapshot := row.Credits
	if snapshot.Remain != 250 || snapshot.Used != 50 || snapshot.Size != 300 {
		t.Fatalf("CN totals = %v/%v/%v, want 250/50/300", snapshot.Remain, snapshot.Used, snapshot.Size)
	}
	if snapshot.UsedPercent != "16.7%" || snapshot.RemainPercent != "83.3%" {
		t.Fatalf("CN percentages = %q/%q", snapshot.UsedPercent, snapshot.RemainPercent)
	}
	if !snapshot.IsPaidUser || snapshot.IsEnterprise {
		t.Fatalf("CN paid/enterprise = %v/%v", snapshot.IsPaidUser, snapshot.IsEnterprise)
	}
	if snapshot.Checkin == nil || !snapshot.Checkin.TodayCheckedIn || snapshot.Checkin.StreakDays != 4 {
		t.Fatalf("checkin = %+v", snapshot.Checkin)
	}
	if snapshot.EarliestExpiring == nil || snapshot.EarliestExpiring.PackageCode != "pkg_paid" {
		t.Fatalf("earliest expiring = %+v", snapshot.EarliestExpiring)
	}
	if len(snapshot.Packages) != 2 {
		t.Fatalf("packages = %+v, want the duplicate collapsed", snapshot.Packages)
	}
	if snapshot.Packages[0].PackageCode != "pkg_paid" || !snapshot.Packages[0].InUsage {
		t.Fatalf("first package = %+v", snapshot.Packages[0])
	}
	if snapshot.Packages[1].PackageCode != "pkg_free" || !snapshot.Packages[1].NoExpiry {
		t.Fatalf("free package = %+v", snapshot.Packages[1])
	}
	if snapshot.Packages[1].GrantReason != "官方活动发放" {
		t.Fatalf("grant reason = %q", snapshot.Packages[1].GrantReason)
	}
	if snapshot.UpdatedAt != float64(now.Unix()) || snapshot.UpdatedISO != now.Format(creditTestLayout) {
		t.Fatalf("updated = %v/%q", snapshot.UpdatedAt, snapshot.UpdatedISO)
	}

	account := pool.Get("cn-a")
	if account.Credits == nil || account.Credits.Remain != 250 {
		t.Fatalf("runtime credits = %+v", account.Credits)
	}
	account.mu.RLock()
	tier := account.Tier
	paid := account.paidUserLocked()
	dailyTokens := account.DailyTokensToday
	extra := append([]byte(nil), account.Auth.Extra["credits"]...)
	account.mu.RUnlock()
	if tier != "WorkBuddy Pro" || !paid {
		t.Fatalf("tier = %q paid=%v", tier, paid)
	}
	if dailyTokens == nil || *dailyTokens != 7 {
		t.Fatalf("daily tokens were not preserved: %v", dailyTokens)
	}
	if len(extra) == 0 {
		t.Fatal("credits were not persisted into Auth.Extra")
	}

	// Reloading from disk proves the credentials file, not only the runtime
	// mirror, carries the refreshed snapshot and tier.
	reloaded, reloadedPool := newPoolProvider(t, realmCN, provider.AccountsDir())
	reloadedAccount := reloadedPool.Get("cn-a")
	if reloadedAccount.Credits == nil || len(reloadedAccount.Credits.Packages) != 2 ||
		reloadedAccount.Credits.Remain != 250 {
		t.Fatalf("reloaded credits = %+v", reloadedAccount.Credits)
	}
	if reloadedAccount.Tier != "WorkBuddy Pro" || !reloadedAccount.IsPaidUser() {
		t.Fatalf("reloaded tier = %q paid=%v", reloadedAccount.Tier, reloadedAccount.IsPaidUser())
	}
	if reloaded.AccountsDir() == "" {
		t.Fatal("reloaded provider has no accounts dir")
	}

	calls := fixture.snapshot()
	for _, path := range []string{
		creditsSummaryPath, creditsFreePackagesPath,
		creditsPaidPackagesPath, creditsCheckinPath,
	} {
		if len(creditCallsWithPath(calls, path)) != 1 {
			t.Fatalf("calls to %s = %d, want 1 (all: %+v)", path, len(creditCallsWithPath(calls, path)), calls)
		}
	}
	for _, call := range calls {
		if call.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", call.Method)
		}
		if call.Authorization != "Bearer access-cn-a" {
			t.Fatalf("Authorization = %q", call.Authorization)
		}
		if call.UserID != "cn-a" || call.ClientPlatform != "web" {
			t.Fatalf("billing headers = %+v", call)
		}
		if call.DeviceToken != "device-secret-value" {
			t.Fatalf("X-Device-Token = %q", call.DeviceToken)
		}
		if call.RequestID == "" || len(call.RequestID) != 32 {
			t.Fatalf("X-Request-ID = %q", call.RequestID)
		}
		if call.MachineID != creditDerivedID("cn-a", "machine") ||
			call.SessionID != creditDerivedID("cn-a", "session") {
			t.Fatalf("fingerprint headers = %q/%q", call.MachineID, call.SessionID)
		}
	}
	freeCall := creditCallsWithPath(calls, creditsFreePackagesPath)[0]
	if !strings.Contains(freeCall.Body, "pkg_free") || !strings.Contains(freeCall.Body, `"PageSize":200`) {
		t.Fatalf("free package body = %s", freeCall.Body)
	}
	paidCall := creditCallsWithPath(calls, creditsPaidPackagesPath)[0]
	if !strings.Contains(paidCall.Body, "NeedRenewInfo") || !strings.Contains(paidCall.Body, "pkg_paid") {
		t.Fatalf("paid package body = %s", paidCall.Body)
	}
}

func TestCreditsCNSummaryTotalsFallback(t *testing.T) {
	now := creditTestNow()
	summaryBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"IsPaidUser": true,
			"Packages": []any{
				map[string]any{
					"PackageCode":         "summary-only",
					"CycleTotalCapacity":  100,
					"CycleRemainCapacity": 80,
					"CycleUsedCapacity":   20,
				},
			},
		},
	})
	emptyAccounts := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{"Accounts": []any{}},
	})
	server, _ := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		switch r.URL.Path {
		case creditsSummaryPath:
			return http.StatusOK, summaryBody
		case creditsFreePackagesPath, creditsPaidPackagesPath:
			return http.StatusOK, emptyAccounts
		case creditsCheckinPath:
			return http.StatusOK, `{"code":0,"data":{}}`
		default:
			return http.StatusNotFound, `{"code":404}`
		}
	})
	provider, _, _ := newCreditsPool(t, realmCN, map[string]map[string]any{"cn-a": nil})
	provider.creditsBillingBaseURL = server.URL
	provider.creditsCheckinBaseURL = server.URL
	provider.creditsNow = func() time.Time { return now }

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	snapshot := result.Results[0].Credits
	if snapshot == nil {
		t.Fatal("missing snapshot")
	}
	if len(snapshot.Packages) != 0 || snapshot.Checkin != nil {
		t.Fatalf("summary fallback packages/checkin = %+v/%+v", snapshot.Packages, snapshot.Checkin)
	}
	if snapshot.Remain != 80 || snapshot.Used != 20 || snapshot.Size != 100 {
		t.Fatalf("summary totals = %v/%v/%v", snapshot.Remain, snapshot.Used, snapshot.Size)
	}
	if snapshot.UsedPercent != "20.0%" || snapshot.RemainPercent != "80.0%" {
		t.Fatalf("summary percentages = %q/%q", snapshot.UsedPercent, snapshot.RemainPercent)
	}
	if !snapshot.IsPaidUser || snapshot.EarliestExpiring != nil {
		t.Fatalf("summary paid/earliest = %v/%+v", snapshot.IsPaidUser, snapshot.EarliestExpiring)
	}
}

func TestCreditsEnterpriseAPIKeyAndIntlFixtures(t *testing.T) {
	now := creditTestNow()

	t.Run("api key uses enterprise v2 path", func(t *testing.T) {
		enterpriseBody := creditTestJSON(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"credit":         25,
				"limitNum":       100,
				"cycleStartTime": "2026-11-01 00:00:00",
				"cycleEndTime":   "2026-12-01 00:00:00",
				"cycleResetTime": "2026-12-01 00:00:00",
			},
		})
		server, fixture := newCreditsTestServer(t, func(r *http.Request) (int, string) {
			if r.URL.Path != creditsEnterprisePathV2 {
				return http.StatusNotFound, `{"code":404}`
			}
			return http.StatusOK, enterpriseBody
		})
		provider, pool, _ := newCreditsPool(t, realmCN, map[string]map[string]any{
			"key-a": {
				"accessToken": "ck_abcdefgh.secret-value",
				"apiKey":      true,
				"source":      sourceAPIKey,
			},
		})
		provider.creditsBillingBaseURL = server.URL
		provider.creditsNow = func() time.Time { return now }

		result, err := provider.RefreshCredits(context.Background())
		if err != nil {
			t.Fatalf("RefreshCredits: %v", err)
		}
		row := result.Results[0]
		if !row.OK || row.Credits == nil {
			t.Fatalf("API key row = %+v", row)
		}
		snapshot := row.Credits
		if snapshot.Remain != 75 || snapshot.Used != 25 || snapshot.Size != 100 {
			t.Fatalf("enterprise totals = %v/%v/%v", snapshot.Remain, snapshot.Used, snapshot.Size)
		}
		if !snapshot.IsEnterprise || !snapshot.IsPaidUser || snapshot.Checkin != nil || snapshot.EarliestExpiring != nil {
			t.Fatalf("enterprise snapshot = %+v", snapshot)
		}
		if len(snapshot.Packages) != 1 || snapshot.Packages[0].PackageCode != "enterprise" ||
			!snapshot.Packages[0].NoExpiry || snapshot.Packages[0].Name != "企业额度" {
			t.Fatalf("enterprise package = %+v", snapshot.Packages)
		}
		calls := fixture.snapshot()
		if len(calls) != 1 || calls[0].Path != creditsEnterprisePathV2 {
			t.Fatalf("API key calls = %+v", calls)
		}
		if calls[0].APIKey != "ck_abcdefgh.secret-value" ||
			calls[0].Authorization != "Bearer ck_abcdefgh.secret-value" {
			t.Fatalf("API key headers = %+v", calls[0])
		}
		if pool.Get("key-a").Auth.Extra["credits"] == nil {
			t.Fatal("API key credits were not persisted")
		}
	})

	t.Run("cn enterprise falls back from personal billing", func(t *testing.T) {
		summaryBody := creditTestJSON(t, map[string]any{"code": 0, "data": map[string]any{"Packages": []any{}}})
		enterpriseBody := creditTestJSON(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"credit":         10,
				"limitNum":       40,
				"cycleStartTime": "2026-11-01 00:00:00",
				"cycleEndTime":   "2026-12-01 00:00:00",
			},
		})
		server, fixture := newCreditsTestServer(t, func(r *http.Request) (int, string) {
			switch r.URL.Path {
			case creditsSummaryPath:
				return http.StatusOK, summaryBody
			case creditsCheckinPath:
				return http.StatusOK, `{"code":0,"data":{}}`
			case creditsEnterprisePath:
				return http.StatusOK, enterpriseBody
			default:
				return http.StatusNotFound, `{"code":404}`
			}
		})
		provider, _, _ := newCreditsPool(t, realmCN, map[string]map[string]any{
			"ent-a": {"enterpriseId": "ent-1"},
		})
		provider.creditsBillingBaseURL = server.URL
		provider.creditsCheckinBaseURL = server.URL
		provider.creditsNow = func() time.Time { return now }

		result, err := provider.RefreshCredits(context.Background())
		if err != nil {
			t.Fatalf("RefreshCredits: %v", err)
		}
		snapshot := result.Results[0].Credits
		if snapshot == nil || !snapshot.IsEnterprise || snapshot.Size != 40 || snapshot.Remain != 30 {
			t.Fatalf("CN enterprise snapshot = %+v", snapshot)
		}
		if len(creditCallsWithPath(fixture.snapshot(), creditsEnterprisePath)) != 1 {
			t.Fatalf("enterprise endpoint not called: %+v", fixture.snapshot())
		}
	})

	t.Run("cn enterprise falls back after personal billing error", func(t *testing.T) {
		enterpriseBody := creditTestJSON(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"credit":         8,
				"limitNum":       32,
				"cycleStartTime": "2026-11-01 00:00:00",
				"cycleEndTime":   "2026-12-01 00:00:00",
			},
		})
		server, fixture := newCreditsTestServer(t, func(r *http.Request) (int, string) {
			switch r.URL.Path {
			case creditsSummaryPath:
				return http.StatusInternalServerError, `{"error":"personal billing down"}`
			case creditsEnterprisePath:
				return http.StatusOK, enterpriseBody
			default:
				return http.StatusNotFound, `{"code":404}`
			}
		})
		provider, _, _ := newCreditsPool(t, realmCN, map[string]map[string]any{
			"ent-err": {"enterpriseId": "ent-1"},
		})
		provider.creditsBillingBaseURL = server.URL
		provider.creditsNow = func() time.Time { return now }

		result, err := provider.RefreshCredits(context.Background())
		if err != nil {
			t.Fatalf("RefreshCredits: %v", err)
		}
		snapshot := result.Results[0].Credits
		if !result.Results[0].OK || snapshot == nil || !snapshot.IsEnterprise ||
			snapshot.Size != 32 || snapshot.Remain != 24 {
			t.Fatalf("fallback snapshot = %+v", snapshot)
		}
		if len(creditCallsWithPath(fixture.snapshot(), creditsEnterprisePath)) != 1 {
			t.Fatalf("enterprise endpoint not called after personal error: %+v", fixture.snapshot())
		}
	})

	t.Run("intl get-user-resource fallback preserves checkin and paid tier", func(t *testing.T) {
		body := creditTestJSON(t, map[string]any{
			"code": 0,
			"data": map[string]any{
				"Response": map[string]any{
					"Data": map[string]any{
						"Accounts": []any{
							map[string]any{
								"PackageName":              "Intl Pack",
								"PackageCode":              "intl-pack",
								"CycleCapacitySizePrecise": "100",
								"CycleCapacityRemain":      "80",
								"CycleCapacityUsed":        "20",
								"DeductionEndTime":         creditTestMillis(now.Add(48 * time.Hour)),
								"Status":                   0,
							},
						},
					},
				},
			},
		})
		server, fixture := newCreditsTestServer(t, func(r *http.Request) (int, string) {
			if r.URL.Path != creditsGetUserResourcePath {
				return http.StatusNotFound, `{"code":404}`
			}
			return http.StatusOK, body
		})
		provider, pool, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{
			"intl-a": {
				"tier": map[string]any{
					"paid":       true,
					"package":    "Pro",
					"checked_at": float64(now.Unix()),
				},
			},
		})
		provider.creditsBillingBaseURL = server.URL
		provider.creditsNow = func() time.Time { return now }
		pool.Get("intl-a").SetCredits(&CreditSnapshot{
			Checkin: &CheckinStatus{TodayCheckedIn: true, StreakDays: 6, Active: true},
		})

		result, err := provider.RefreshCredits(context.Background())
		if err != nil {
			t.Fatalf("RefreshCredits: %v", err)
		}
		snapshot := result.Results[0].Credits
		if snapshot == nil || snapshot.Remain != 80 || snapshot.Used != 20 || snapshot.Size != 100 {
			t.Fatalf("intl fallback totals = %+v", snapshot)
		}
		if !snapshot.IsPaidUser || snapshot.IsEnterprise {
			t.Fatalf("intl fallback paid/enterprise = %v/%v", snapshot.IsPaidUser, snapshot.IsEnterprise)
		}
		if snapshot.Checkin == nil || snapshot.Checkin.StreakDays != 6 {
			t.Fatalf("intl checkin was not preserved: %+v", snapshot.Checkin)
		}
		if snapshot.EarliestExpiring == nil || snapshot.EarliestExpiring.PackageCode != "intl-pack" {
			t.Fatalf("intl earliest = %+v", snapshot.EarliestExpiring)
		}
		calls := fixture.snapshot()
		if len(calls) != 1 || calls[0].Path != creditsGetUserResourcePath {
			t.Fatalf("intl calls = %+v", calls)
		}
		if !strings.Contains(calls[0].Body, `"ProductCode":"p_tcaca"`) ||
			!strings.Contains(calls[0].Body, `"PageSize":100`) {
			t.Fatalf("fallback body = %s", calls[0].Body)
		}
		if calls[0].DeviceToken != "" {
			t.Fatalf("intl request carried X-Device-Token: %q", calls[0].DeviceToken)
		}
		if calls[0].MachineID != creditDerivedID("intl-a", "machine") ||
			calls[0].SessionID != creditDerivedID("intl-a", "session") {
			t.Fatalf("intl fingerprint headers = %q/%q", calls[0].MachineID, calls[0].SessionID)
		}
	})
}

func TestCreditsIntlTierRefreshAndTTL(t *testing.T) {
	now := creditTestNow()
	fallbackBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"Response": map[string]any{
				"Data": map[string]any{
					"Accounts": []any{
						map[string]any{
							"PackageName":      "Intl Pack",
							"PackageCode":      "intl-pack",
							"CapacitySize":     50,
							"CapacityRemain":   40,
							"CapacityUsed":     10,
							"DeductionEndTime": creditTestMillis(now.Add(48 * time.Hour)),
						},
					},
				},
			},
		},
	})
	summaryBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"IsPaidUser":              true,
			"SubscriptionPackageName": "WorkBuddy Pro",
			"SubscriptionPackageCode": "pro",
			"ProTrialStatus":          1,
		},
	})
	var mu sync.Mutex
	fallbackCalls, summaryCalls := 0, 0
	server, _ := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case creditsGetUserResourcePath:
			fallbackCalls++
			return http.StatusOK, fallbackBody
		case creditsSummaryPath:
			summaryCalls++
			return http.StatusOK, summaryBody
		default:
			return http.StatusNotFound, `{"code":404}`
		}
	})
	provider, pool, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{
		"intl-a": nil,
	})
	provider.creditsBillingBaseURL = server.URL
	provider.creditsNow = func() time.Time { return now }

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	if !result.Results[0].OK || !result.Results[0].Credits.IsPaidUser {
		t.Fatalf("upgraded row = %+v", result.Results[0])
	}
	account := pool.Get("intl-a")
	if account.Tier != "WorkBuddy Pro" || !account.IsPaidUser() {
		t.Fatalf("tier after upgrade = %q paid=%v", account.Tier, account.IsPaidUser())
	}
	account.mu.RLock()
	persistedTier := append([]byte(nil), account.Auth.Tier...)
	account.mu.RUnlock()
	if len(persistedTier) == 0 {
		t.Fatal("upgraded tier was not persisted")
	}

	// The same fixed clock is inside the 24h TTL: the second balance refresh
	// must not spend another summary request.
	if _, err := provider.RefreshCredits(context.Background()); err != nil {
		t.Fatalf("second RefreshCredits: %v", err)
	}
	mu.Lock()
	gotFallback, gotSummary := fallbackCalls, summaryCalls
	mu.Unlock()
	if gotFallback != 2 {
		t.Fatalf("fallback calls = %d, want 2", gotFallback)
	}
	if gotSummary != 1 {
		t.Fatalf("summary calls = %d, want 1 (TTL)", gotSummary)
	}
	_, reloadedPool := newPoolProvider(t, realmIntl, provider.AccountsDir())
	reloaded := reloadedPool.Get("intl-a")
	if reloaded.Tier != "WorkBuddy Pro" || !reloaded.IsPaidUser() {
		t.Fatalf("reloaded tier = %q paid=%v", reloaded.Tier, reloaded.IsPaidUser())
	}
}

func TestCreditsRefreshIsolatesErrorsAndRedactsSecrets(t *testing.T) {
	now := creditTestNow()
	goodBody := creditTestJSON(t, map[string]any{"code": 0, "data": map[string]any{"Packages": []any{}}})
	server, _ := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		if r.Header.Get("X-User-Id") == "bad" {
			return http.StatusInternalServerError, `{"error":"access-bad refresh-bad secret-value"}`
		}
		switch r.URL.Path {
		case creditsSummaryPath:
			return http.StatusOK, goodBody
		case creditsCheckinPath:
			return http.StatusOK, `{"code":0,"data":{}}`
		default:
			return http.StatusInternalServerError, `{"error":"upstream down"}`
		}
	})
	provider, pool, _ := newCreditsPool(t, realmCN, map[string]map[string]any{
		"bad":  nil,
		"good": nil,
	})
	provider.creditsBillingBaseURL = server.URL
	provider.creditsCheckinBaseURL = server.URL
	provider.creditsNow = func() time.Time { return now }
	pool.Get("bad").SetCredits(&CreditSnapshot{Remain: 1, Used: 9, Size: 10})

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits should not fail the whole sweep: %v", err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("rows = %+v", result.Results)
	}
	if result.Results[0].UID != "bad" || result.Results[0].OK || result.Results[0].Error == "" {
		t.Fatalf("bad row = %+v", result.Results[0])
	}
	for _, secret := range []string{"access-bad", "refresh-bad", "secret-value"} {
		if strings.Contains(result.Results[0].Error, secret) {
			t.Fatalf("row error leaked %q: %q", secret, result.Results[0].Error)
		}
	}
	if result.Results[1].UID != "good" || !result.Results[1].OK || result.Results[1].Credits == nil {
		t.Fatalf("good row = %+v", result.Results[1])
	}
	if pool.Get("bad").Credits == nil || pool.Get("bad").Credits.Remain != 1 {
		t.Fatalf("failed refresh replaced the previous snapshot: %+v", pool.Get("bad").Credits)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for _, secret := range []string{"access-bad", "refresh-bad", "secret-value", "access-good"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("refresh result leaked %q: %s", secret, raw)
		}
	}
}

func TestCreditsPoolSetCreditsPersistsAndClears(t *testing.T) {
	provider, pool, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{"a": nil})
	snapshot := &CreditSnapshot{
		Remain:        12,
		Used:          3,
		Size:          15,
		UsedPercent:   "20.0%",
		RemainPercent: "80.0%",
		IsPaidUser:    true,
		Packages: []CreditPackage{
			{
				Name:        "Pack",
				PackageCode: "pack",
				Remain:      12,
				Used:        3,
				Size:        15,
			},
		},
		UpdatedAt:  123,
		UpdatedISO: "2026-11-09 12:00:00",
	}
	if err := pool.SetCredits("a", snapshot); err != nil {
		t.Fatalf("SetCredits: %v", err)
	}
	if got := pool.Get("a").Credits; got == nil || got.Remain != 12 || len(got.Packages) != 1 {
		t.Fatalf("runtime credits = %+v", got)
	}
	reloaded, reloadedPool := newPoolProvider(t, realmIntl, provider.AccountsDir())
	if got := reloadedPool.Get("a").Credits; got == nil || got.Remain != 12 || !got.IsPaidUser {
		t.Fatalf("reloaded credits = %+v", got)
	}
	if got := pool.Get("a").Auth.Extra["credits"]; len(got) == 0 {
		t.Fatal("credits extra missing after SetCredits")
	}
	if reloaded == nil {
		t.Fatal("reloaded provider is nil")
	}

	if err := pool.SetCredits("a", nil); err != nil {
		t.Fatalf("SetCredits(nil): %v", err)
	}
	if got := pool.Get("a").Credits; got != nil {
		t.Fatalf("runtime credits after clear = %+v", got)
	}
	if _, exists := pool.Get("a").Auth.Extra["credits"]; exists {
		t.Fatal("credits extra survived SetCredits(nil)")
	}
	_, clearedPool := newPoolProvider(t, realmIntl, provider.AccountsDir())
	if got := clearedPool.Get("a").Credits; got != nil {
		t.Fatalf("reloaded credits after clear = %+v", got)
	}
}

func TestCreditsRefreshForTargetSelection(t *testing.T) {
	now := creditTestNow()
	fallbackBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"Response": map[string]any{
				"Data": map[string]any{
					"Accounts": []any{
						map[string]any{
							"PackageName":      "Pack",
							"PackageCode":      "pack",
							"CapacitySize":     10,
							"CapacityRemain":   10,
							"CapacityUsed":     0,
							"DeductionEndTime": creditTestMillis(now.Add(24 * time.Hour)),
						},
					},
				},
			},
		},
	})
	var mu sync.Mutex
	counts := map[string]int{}
	server, _ := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		if r.URL.Path != creditsGetUserResourcePath {
			return http.StatusNotFound, `{"code":404}`
		}
		mu.Lock()
		counts[r.Header.Get("X-User-Id")]++
		mu.Unlock()
		return http.StatusOK, fallbackBody
	})
	tier := map[string]any{
		"paid":       true,
		"package":    "Pro",
		"checked_at": float64(now.Unix()),
	}
	provider, _, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{
		"a": {"group": "G1", "tier": tier},
		"b": {"group": "G2", "tier": tier},
		"c": {"group": "G2", "tier": tier},
	})
	provider.creditsBillingBaseURL = server.URL
	provider.creditsNow = func() time.Time { return now }
	reset := func() {
		mu.Lock()
		counts = map[string]int{}
		mu.Unlock()
	}
	snapshotCounts := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]int, len(counts))
		for uid, count := range counts {
			out[uid] = count
		}
		return out
	}

	ctx := context.Background()
	if result, err := provider.RefreshCreditsFor(ctx, CreditsRefreshTargets{UID: "a"}); err != nil {
		t.Fatalf("UID refresh: %v", err)
	} else if len(result.Results) != 1 || result.Results[0].UID != "a" ||
		len(result.Accounts) != 3 {
		t.Fatalf("UID result = %+v", result)
	}
	if got := snapshotCounts(); got["a"] != 1 || got["b"] != 0 || got["c"] != 0 {
		t.Fatalf("UID selection counts = %v", got)
	}

	reset()
	if result, err := provider.RefreshCreditsFor(ctx, CreditsRefreshTargets{UIDs: []string{"b", "b"}}); err != nil {
		t.Fatalf("UIDs refresh: %v", err)
	} else if len(result.Results) != 1 || result.Results[0].UID != "b" {
		t.Fatalf("UIDs result = %+v", result)
	}
	if got := snapshotCounts(); got["b"] != 1 || got["a"] != 0 || got["c"] != 0 {
		t.Fatalf("UIDs selection counts = %v", got)
	}

	reset()
	if result, err := provider.RefreshCreditsFor(ctx, CreditsRefreshTargets{UIDs: []string{}}); err != nil {
		t.Fatalf("empty UIDs refresh: %v", err)
	} else if len(result.Results) != 0 {
		t.Fatalf("empty UIDs refreshed: %+v", result.Results)
	}
	if got := snapshotCounts(); len(got) != 0 {
		t.Fatalf("empty UIDs made requests: %v", got)
	}

	reset()
	if result, err := provider.RefreshCreditsFor(ctx, CreditsRefreshTargets{Realm: realmCN}); err != nil {
		t.Fatalf("realm refresh: %v", err)
	} else if len(result.Results) != 0 {
		t.Fatalf("other realm refreshed: %+v", result.Results)
	}
	if got := snapshotCounts(); len(got) != 0 {
		t.Fatalf("other realm made requests: %v", got)
	}

	reset()
	if result, err := provider.RefreshCreditsFor(ctx, CreditsRefreshTargets{Realm: "all", Group: "G2"}); err != nil {
		t.Fatalf("group refresh: %v", err)
	} else if len(result.Results) != 2 {
		t.Fatalf("group result = %+v", result.Results)
	}
	if got := snapshotCounts(); got["b"] != 1 || got["c"] != 1 || got["a"] != 0 {
		t.Fatalf("group selection counts = %v", got)
	}
}

func TestCreditsRefreshSingleFlightPerAccount(t *testing.T) {
	now := creditTestNow()
	fallbackBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"Response": map[string]any{
				"Data": map[string]any{
					"Accounts": []any{
						map[string]any{
							"PackageName":      "Pack",
							"PackageCode":      "pack",
							"CapacitySize":     10,
							"CapacityRemain":   10,
							"CapacityUsed":     0,
							"DeductionEndTime": creditTestMillis(now.Add(24 * time.Hour)),
						},
					},
				},
			},
		},
	})

	var mu sync.Mutex
	calls := 0
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	server, _ := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		mu.Lock()
		calls++
		mu.Unlock()
		startOnce.Do(func() { started <- struct{}{} })
		<-release
		return http.StatusOK, fallbackBody
	})
	provider, _, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{
		"intl-a": {
			"tier": map[string]any{
				"paid":       true,
				"package":    "Pro",
				"checked_at": float64(now.Unix()),
			},
		},
	})
	provider.creditsBillingBaseURL = server.URL
	provider.creditsNow = func() time.Time { return now }
	joined := make(chan struct{}, 1)
	provider.creditJoinHook = func() {
		select {
		case joined <- struct{}{}:
		default:
		}
	}

	type refreshOutcome struct {
		result CreditsRefreshResult
		err    error
	}
	first := make(chan refreshOutcome, 1)
	second := make(chan refreshOutcome, 1)
	go func() {
		result, err := provider.RefreshCredits(context.Background())
		first <- refreshOutcome{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first refresh did not reach upstream")
	}
	go func() {
		result, err := provider.RefreshCredits(context.Background())
		second <- refreshOutcome{result: result, err: err}
	}()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("second refresh did not join the in-flight account")
	}
	releaseAll()

	for index, outcome := range []refreshOutcome{<-first, <-second} {
		if outcome.err != nil {
			t.Fatalf("refresh %d: %v", index, outcome.err)
		}
		if len(outcome.result.Results) != 1 || !outcome.result.Results[0].OK ||
			outcome.result.Results[0].Credits == nil {
			t.Fatalf("refresh %d rows = %+v", index, outcome.result.Results)
		}
		if outcome.result.Results[0].Credits.Remain != 10 {
			t.Fatalf("refresh %d remain = %v", index, outcome.result.Results[0].Credits.Remain)
		}
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("upstream calls = %d, want one shared fetch", gotCalls)
	}
}

func TestCreditsRefreshIncludesDisabledAccounts(t *testing.T) {
	now := creditTestNow()
	body := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"Response": map[string]any{
				"Data": map[string]any{
					"Accounts": []any{
						map[string]any{
							"PackageName":      "Disabled Pack",
							"PackageCode":      "disabled",
							"CapacitySize":     10,
							"CapacityRemain":   10,
							"DeductionEndTime": creditTestMillis(now.Add(24 * time.Hour)),
						},
					},
				},
			},
		},
	})
	server, _ := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		if r.URL.Path != creditsGetUserResourcePath {
			return http.StatusNotFound, `{"code":404}`
		}
		return http.StatusOK, body
	})
	provider, pool, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{
		"disabled-a": {"enabled": false},
	})
	if pool.Get("disabled-a").Healthy(time.Now()) {
		t.Fatal("fixture account should be disabled")
	}
	provider.creditsBillingBaseURL = server.URL
	provider.creditsNow = func() time.Time { return now }

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	if len(result.Results) != 1 || !result.Results[0].OK {
		t.Fatalf("disabled account was not refreshed: %+v", result.Results)
	}
	if pool.Get("disabled-a").Credits == nil {
		t.Fatal("disabled account snapshot missing")
	}
}

func TestCreditsRefresh401ForceRefreshesLocallyValidToken(t *testing.T) {
	now := time.Now()
	tokenFake := &tokenTestFake{
		body: tokenSuccessBody("new-access", "new-refresh", now.Add(time.Hour)),
	}
	tokenServer := httptest.NewServer(tokenFake)
	t.Cleanup(tokenServer.Close)

	billingBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"Response": map[string]any{
				"Data": map[string]any{
					"Accounts": []any{
						map[string]any{
							"PackageName":      "Pack",
							"PackageCode":      "pack",
							"CapacitySize":     10,
							"CapacityRemain":   10,
							"CapacityUsed":     0,
							"DeductionEndTime": creditTestMillis(now.Add(24 * time.Hour)),
						},
					},
				},
			},
		},
	})
	var billingCalls int
	var mu sync.Mutex
	server, _ := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		mu.Lock()
		billingCalls++
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer new-access" {
			return http.StatusUnauthorized, `{"code":401}`
		}
		return http.StatusOK, billingBody
	})
	provider, pool, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{
		"intl-a": {
			// Locally valid, but upstream revokes the token: EnsureToken
			// would be a no-op, so the 401 path must ForceRefresh.
			"expiresAt": now.Add(time.Hour).Unix(),
			"tier": map[string]any{
				"paid":       true,
				"package":    "Pro",
				"checked_at": float64(now.Unix()),
			},
		},
	})
	provider.creditsBillingBaseURL = server.URL
	provider.creditsNow = func() time.Time { return now }
	provider.tokenBaseURL = tokenServer.URL
	provider.tokenNow = func() time.Time { return now }
	if expires := pool.Get("intl-a").Auth.ExpiresAt; !expires.After(now) {
		t.Fatalf("fixture token should be locally valid, expires %v", expires)
	}

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	if len(result.Results) != 1 || !result.Results[0].OK {
		t.Fatalf("token refresh row = %+v", result.Results)
	}
	mu.Lock()
	calls := billingCalls
	mu.Unlock()
	if calls != 2 {
		t.Fatalf("billing calls = %d, want one 401 plus one retry", calls)
	}
	if len(tokenFake.snapshot()) != 1 {
		t.Fatalf("token refresh calls = %d, want 1", len(tokenFake.snapshot()))
	}
	if pool.Get("intl-a").Auth.AccessToken != "new-access" {
		t.Fatalf("token was not rotated: %q", pool.Get("intl-a").Auth.AccessToken)
	}
}

func TestCreditsTimeoutParityAndAuthHeuristics(t *testing.T) {
	provider := &Provider{}
	if got := provider.creditTimeout(creditsBillingTimeout); got != 15*time.Second {
		t.Fatalf("billing timeout = %v", got)
	}
	if got := provider.creditTimeout(creditsFallbackTimeout); got != 30*time.Second {
		t.Fatalf("fallback timeout = %v", got)
	}
	if got := provider.creditTimeout(creditsCheckinTimeout); got != 10*time.Second {
		t.Fatalf("checkin timeout = %v", got)
	}
	provider.creditsTimeout = time.Second
	if got := provider.creditTimeout(creditsFallbackTimeout); got != time.Second {
		t.Fatalf("test override timeout = %v", got)
	}

	for _, text := range []string{
		"invalid_token", "invalid-token", "HTTP 401", "Unauthorized",
		"Authorization failed", "token expired", "登录已过期",
	} {
		if !creditAuthFailureText(text) {
			t.Fatalf("%q was not treated as an auth failure", text)
		}
	}
	for _, text := range []string{"", "<html><body>401 Unauthorized</body></html>"} {
		if creditAuthFailureText(text) {
			t.Fatalf("%q was treated as an auth failure", text)
		}
	}
	if !creditAuthFailureText("<html>invalid_token</html>") {
		t.Fatal("invalid_token should outrank the HTML bail-out")
	}
}

func TestCreditsAccountGuardMethodsAndFreeExemption(t *testing.T) {
	free := []string{"hy3"}

	reserve := &Account{
		Credits:        &CreditSnapshot{Remain: 5},
		ReserveCredits: 10,
	}
	reserve.SetFreeModels(free)
	if !reserve.ReserveBlocked("paid") {
		t.Fatal("reserve guard did not block a paid model")
	}
	if reserve.ReserveBlocked("hy3") {
		t.Fatal("reserve guard blocked a free model")
	}
	if got := reserve.GuardBlocked("paid"); got != "reserve" {
		t.Fatalf("guard = %q, want reserve", got)
	}
	if got := reserve.GuardBlocked("hy3"); got != "" {
		t.Fatalf("free model guard = %q, want none", got)
	}

	dailyTokens := &Account{
		DailyTokenLimit:  100,
		DailyTokensToday: creditTestInt64(100),
	}
	dailyTokens.SetFreeModels(free)
	if !dailyTokens.DailyLimitBlocked() {
		t.Fatal("daily token guard did not block")
	}
	if got := dailyTokens.GuardBlocked("hy3"); got != "daily_token" {
		t.Fatalf("free model daily token guard = %q, want daily_token", got)
	}

	dailyCredits := &Account{
		Credits:           &CreditSnapshot{Remain: 100},
		DailyCreditLimit:  10,
		DailyCreditsToday: creditTestFloat(10.5),
	}
	dailyCredits.SetFreeModels(free)
	if !dailyCredits.CreditLimitReached() {
		t.Fatal("daily credit guard did not register")
	}
	if !dailyCredits.CreditLimitBlocked("paid") {
		t.Fatal("daily credit guard did not block a paid model")
	}
	if dailyCredits.CreditLimitBlocked("hy3") {
		t.Fatal("daily credit guard blocked a free model")
	}
	if got := dailyCredits.GuardBlocked("paid"); got != "daily_credit" {
		t.Fatalf("guard = %q, want daily_credit", got)
	}

	modelDaily := &Account{
		ModelDailyTokenLimit: 50,
		ModelDailyTokens:     map[string]int64{"m1": 50},
	}
	modelDaily.SetFreeModels(free)
	if !modelDaily.ModelTokenLimitBlocked("m1") {
		t.Fatal("model daily guard did not block m1")
	}
	if modelDaily.ModelTokenLimitBlocked("m2") {
		t.Fatal("model daily guard leaked to m2")
	}
	if got := modelDaily.GuardBlocked("m1"); got != "model_daily_token" {
		t.Fatalf("guard = %q, want model_daily_token", got)
	}
	if got := modelDaily.GuardBlocked("hy3"); got != "" {
		t.Fatalf("unused free model guard = %q", got)
	}

	unknown := &Account{}
	if unknown.ReserveBlocked("paid") || unknown.DailyLimitBlocked() ||
		unknown.CreditLimitReached() || unknown.ModelTokenLimitBlocked("m1") {
		t.Fatal("unknown counters or balances must not block")
	}
}

func TestCreditsPoolReadsRealmLimitsFromSettings(t *testing.T) {
	dir := t.TempDir()
	writePoolSettings(t, dir, map[string]any{
		"limits": map[string]any{
			"reserve_credits": map[string]any{"global": 10, "intl": 20, "cn": 5},
			"daily_token_limit": map[string]any{
				"global": 100, "cn": 40,
			},
			"daily_credit_limit": map[string]any{
				// Malformed intl override must inherit global 9, not turn
				// corruption into an explicit "off".
				"global": 9, "intl": "junk", "cn": 0,
			},
			"model_daily_token_limit": map[string]any{
				"global": 50, "cn": "not-a-number",
			},
			"expiring_window_days": map[string]any{"global": 4, "intl": nil, "cn": nil},
		},
	})
	writePoolAccount(t, dir, "a", realmIntl, nil)
	writePoolAccount(t, dir, "c", realmCN, nil)

	_, intlPool := newPoolProvider(t, realmIntl, dir)
	intl := intlPool.Get("a")
	if intl == nil {
		t.Fatal("missing intl account")
	}
	if intl.ReserveCredits != 20 || intl.DailyTokenLimit != 100 ||
		intl.DailyCreditLimit != 9 || intl.ModelDailyTokenLimit != 50 ||
		intl.ExpiringWindowDays != 4 {
		t.Fatalf("intl settings limits = %d/%d/%d/%d/%d", intl.ReserveCredits,
			intl.DailyTokenLimit, intl.DailyCreditLimit,
			intl.ModelDailyTokenLimit, intl.ExpiringWindowDays)
	}

	_, cnPool := newPoolProvider(t, realmCN, dir)
	cn := cnPool.Get("c")
	if cn == nil {
		t.Fatal("missing cn account")
	}
	if cn.ReserveCredits != 5 || cn.DailyTokenLimit != 40 ||
		cn.DailyCreditLimit != 0 || cn.ModelDailyTokenLimit != 50 ||
		cn.ExpiringWindowDays != 4 {
		t.Fatalf("cn settings limits = %d/%d/%d/%d/%d", cn.ReserveCredits,
			cn.DailyTokenLimit, cn.DailyCreditLimit,
			cn.ModelDailyTokenLimit, cn.ExpiringWindowDays)
	}

	legacyDir := t.TempDir()
	writePoolSettings(t, legacyDir, map[string]any{
		"reserve_credits":      7,
		"expiring_window_days": 2,
	})
	writePoolAccount(t, legacyDir, "a", realmIntl, nil)
	_, legacyPool := newPoolProvider(t, realmIntl, legacyDir)
	legacy := legacyPool.Get("a")
	if legacy.ReserveCredits != 7 || legacy.ExpiringWindowDays != 2 {
		t.Fatalf("legacy limits = %d/%d", legacy.ReserveCredits, legacy.ExpiringWindowDays)
	}
}

func TestCreditsApplyLimitsRealmOverridesAndDefaultWindow(t *testing.T) {
	values := map[string]any{
		"reserve_credits":         map[string]any{"global": 10, "intl": 20, "cn": 5},
		"daily_token_limit":       map[string]any{"global": 100, "intl": 150},
		"daily_credit_limit":      map[string]any{"global": 5, "intl": 8},
		"model_daily_token_limit": map[string]any{"global": 50, "intl": 60},
		"expiring_window_days":    map[string]any{"global": 7, "intl": 3, "cn": nil},
	}
	usage := map[string]any{
		"tokens":  map[string]any{"a": 12},
		"credits": map[string]any{"a": 1.5},
		"models":  map[string]any{"a": map[string]any{"m1": 7}},
	}

	provider, pool, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{"a": nil})
	// A freshly constructed pool reads the shared limits; with no saved value
	// only the expiring window ships enabled, at its v1 default.
	if got := pool.Get("a").ExpiringWindowDays; got != 7 {
		t.Fatalf("fresh pool expiring window = %d, want 7", got)
	}
	if got := pool.Get("a").ReserveCredits; got != 0 {
		t.Fatalf("fresh pool reserve = %d, want 0", got)
	}
	provider.ApplyLimits(values, usage)
	account := pool.Get("a")
	account.mu.RLock()
	if account.ReserveCredits != 20 || account.DailyTokenLimit != 150 ||
		account.DailyCreditLimit != 8 || account.ModelDailyTokenLimit != 60 ||
		account.ExpiringWindowDays != 3 {
		t.Fatalf("intl limits = reserve %d token %d credit %d model %d window %d",
			account.ReserveCredits, account.DailyTokenLimit, account.DailyCreditLimit,
			account.ModelDailyTokenLimit, account.ExpiringWindowDays)
	}
	if account.DailyTokensToday == nil || *account.DailyTokensToday != 12 ||
		account.DailyCreditsToday == nil || *account.DailyCreditsToday != 1.5 ||
		account.ModelDailyTokens["m1"] != 7 {
		t.Fatalf("usage projection = %+v %+v %+v",
			account.DailyTokensToday, account.DailyCreditsToday, account.ModelDailyTokens)
	}
	if !account.FreeModels["hy3"] {
		t.Fatalf("intl free models = %+v", account.FreeModels)
	}
	account.mu.RUnlock()

	// A nil usage argument keeps the last known counters, matching v1's
	// apply_* (usage=None) contract.
	provider.ApplyLimits(values, nil)
	account.mu.RLock()
	if account.DailyTokensToday == nil || *account.DailyTokensToday != 12 {
		t.Fatalf("nil usage reset tokens: %v", account.DailyTokensToday)
	}
	account.mu.RUnlock()

	// No limits at all: the expiring window ships enabled at its v1 default,
	// while every guard defaults to off.
	provider.ApplyLimits(nil, nil)
	account.mu.RLock()
	if account.ReserveCredits != 0 || account.DailyTokenLimit != 0 ||
		account.DailyCreditLimit != 0 || account.ModelDailyTokenLimit != 0 {
		t.Fatalf("defaults were not off: %+v", account)
	}
	if account.ExpiringWindowDays != 7 {
		t.Fatalf("default expiring window = %d, want 7", account.ExpiringWindowDays)
	}
	if account.DailyTokensToday == nil || *account.DailyTokensToday != 12 {
		t.Fatalf("defaults reset usage: %v", account.DailyTokensToday)
	}
	account.mu.RUnlock()

	// An explicit zero turns the window off; it is not treated as "missing".
	provider.ApplyLimits(map[string]any{
		"expiring_window_days": map[string]any{"global": 0},
	}, nil)
	if got := pool.Get("a").ExpiringWindowDays; got != 0 {
		t.Fatalf("explicit zero window = %d", got)
	}

	cnProvider, cnPool, _ := newCreditsPool(t, realmCN, map[string]map[string]any{"a": nil})
	cnProvider.ApplyLimits(values, nil)
	cnAccount := cnPool.Get("a")
	cnAccount.mu.RLock()
	if cnAccount.ReserveCredits != 5 || cnAccount.DailyTokenLimit != 100 ||
		cnAccount.ExpiringWindowDays != 7 {
		t.Fatalf("cn limits = %d/%d/%d", cnAccount.ReserveCredits,
			cnAccount.DailyTokenLimit, cnAccount.ExpiringWindowDays)
	}
	if !cnAccount.FreeModels["hy4-preview-f"] {
		t.Fatalf("cn free models = %+v", cnAccount.FreeModels)
	}
	cnAccount.mu.RUnlock()

	// Malformed values are off, not a crash or a stray guard.
	badProvider, badPool, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{"a": nil})
	badProvider.ApplyLimits(map[string]any{
		"reserve_credits": map[string]any{"global": "junk"},
	}, map[string]any{
		"tokens": "junk",
	})
	if got := badPool.Get("a").ReserveCredits; got != 0 {
		t.Fatalf("malformed reserve = %d", got)
	}
}

func TestCreditsPoolGuardHookBlocksOnlyPaidModels(t *testing.T) {
	provider, pool, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{
		"a": nil,
		"b": nil,
	})
	provider.ApplyLimits(map[string]any{
		"reserve_credits": map[string]any{"global": 10},
	}, nil)
	pool.Get("a").SetCredits(&CreditSnapshot{Remain: 5})

	for i := 0; i < 12; i++ {
		account, err := pool.Pick("paid-model", "", nil)
		if err != nil {
			t.Fatalf("Pick paid-model: %v", err)
		}
		if account.Auth.UID == "a" {
			t.Fatal("guarded account served a paid model")
		}
	}
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		account, err := pool.Pick("hy3", "", nil)
		if err != nil {
			t.Fatalf("Pick hy3: %v", err)
		}
		counts[account.Auth.UID]++
	}
	if counts["a"] == 0 || counts["b"] == 0 {
		t.Fatalf("reserve guard wrongly blocked a free model: %v", counts)
	}

	// Daily credit is the other guard free models escape; daily token still
	// applies to every model, exactly like v1.
	provider.ApplyLimits(map[string]any{
		"daily_credit_limit": map[string]any{"global": 1},
	}, map[string]any{
		"credits": map[string]any{"a": 2},
	})
	counts = map[string]int{}
	for i := 0; i < 8; i++ {
		account, err := pool.Pick("hy3", "", nil)
		if err != nil {
			t.Fatalf("Pick hy3 after credit cap: %v", err)
		}
		counts[account.Auth.UID]++
	}
	if counts["a"] == 0 || counts["b"] == 0 {
		t.Fatalf("daily credit guard wrongly blocked a free model: %v", counts)
	}
	if _, err := pool.Pick("paid-model", "", map[string]bool{"b": true}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("daily credit guard Pick = %v, want ErrNoAccount", err)
	}

	provider.ApplyLimits(map[string]any{
		"daily_token_limit": map[string]any{"global": 10},
	}, map[string]any{
		"tokens": map[string]any{"a": 10},
	})
	if _, err := pool.Pick("hy3", "", map[string]bool{"b": true}); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("daily token guard exempted a free model: %v", err)
	}
}

func TestCreditsFetchGrantsRowsSummaryAndNoUpstream(t *testing.T) {
	now := creditTestNow()
	failing := &creditsFailDoer{}
	provider, pool, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{
		"alpha": nil,
		"beta":  nil,
	})
	provider.creditsDoer = failing
	provider.creditsNow = func() time.Time { return now }

	pool.Get("alpha").SetCredits(&CreditSnapshot{
		UpdatedAt: 100,
		Packages: []CreditPackage{
			{
				Name:         "Older",
				PackageCode:  "older",
				Remain:       0,
				Used:         10,
				Size:         10,
				CreateTime:   "2026-10-01 10:00:00",
				CycleEndTime: "2026-11-10 00:00:00",
				ExpireTime:   "2026-11-10 00:00:00",
			},
			{
				Name:         "New",
				PackageCode:  "new",
				Remain:       10,
				Used:         5,
				Size:         15,
				InUsage:      true,
				CreateTime:   "2026-11-05 10:00:00",
				CycleEndTime: "2026-11-20 10:00:00",
				ExpireTime:   "2026-11-20 10:00:00",
				DaysLeft:     creditTestFloat(11),
			},
		},
	})
	pool.Get("beta").SetCredits(&CreditSnapshot{
		UpdatedAt: 200,
		Packages: []CreditPackage{
			{
				Name:        "Expired",
				PackageCode: "expired",
				Remain:      5,
				Used:        1,
				Size:        6,
				CreateTime:  "2026-11-06 10:00:00",
				ExpireTime:  "2026-11-01 10:00:00",
			},
			{
				Name:         "NoExpiry",
				PackageCode:  "no-expiry",
				Remain:       5,
				Size:         5,
				NoExpiry:     true,
				CreateTime:   "2026-11-04 10:00:00",
				CycleEndTime: "2026-11-01 10:00:00",
			},
			{
				Name:        "Unknown",
				PackageCode: "unknown",
				Remain:      1,
				Size:        1,
				NoExpiry:    true,
				CreateTime:  "not-a-timestamp",
			},
		},
	})

	result, err := provider.Grants(context.Background())
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if !result.OK || len(result.Rows) != 5 {
		t.Fatalf("grants result = %+v", result)
	}
	if failing.calls != 0 {
		t.Fatalf("Grants made %d upstream calls", failing.calls)
	}
	order := make([]string, 0, len(result.Rows))
	for _, row := range result.Rows {
		order = append(order, row.Name)
	}
	wantOrder := []string{"Expired", "New", "NoExpiry", "Older", "Unknown"}
	if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("grant order = %v, want %v", order, wantOrder)
	}
	wantStatus := map[string]string{
		"Expired":  "expired",
		"New":      "active",
		"NoExpiry": "available",
		"Older":    "used_up",
		"Unknown":  "available",
	}
	for _, row := range result.Rows {
		if row.Status != wantStatus[row.Name] {
			t.Fatalf("%s status = %q, want %q", row.Name, row.Status, wantStatus[row.Name])
		}
		if row.Realm != realmIntl || row.Action != nil {
			t.Fatalf("%s row metadata = %+v", row.Name, row)
		}
	}
	if result.Rows[0].Nickname != "beta" || result.Rows[0].CreateAt == nil {
		t.Fatalf("first row = %+v", result.Rows[0])
	}
	if result.Rows[4].CreateAt != nil {
		t.Fatalf("unknown create time = %v, want nil", *result.Rows[4].CreateAt)
	}
	if result.Rows[2].NoExpiry != true || result.Rows[1].DaysLeft == nil ||
		*result.Rows[1].DaysLeft <= 0 {
		t.Fatalf("expiry fields = %+v / %+v", result.Rows[1], result.Rows[2])
	}
	if result.Rows[2].ExpireISO == "" {
		t.Fatal("no-expiry row should still expose its cycle boundary")
	}
	rows, err := provider.FetchGrants(context.Background())
	if err != nil {
		t.Fatalf("FetchGrants wrapper: %v", err)
	}
	if len(rows) != len(result.Rows) || rows[0].Name != result.Rows[0].Name {
		t.Fatalf("row wrapper = %+v", rows)
	}

	summary := result.Summary
	if summary.Count != 5 || summary.Accounts != 2 || summary.Size != 37 ||
		summary.Remain != 21 || summary.Used != 16 {
		t.Fatalf("grant summary = %+v", summary)
	}
	if result.GeneratedAt != float64(now.Unix()) ||
		result.GeneratedISO != now.Format(creditTestLayout) {
		t.Fatalf("generated = %v/%q", result.GeneratedAt, result.GeneratedISO)
	}
	if result.FetchedAt == nil || *result.FetchedAt != 200 ||
		result.FetchedISO == nil || *result.FetchedISO == "" {
		t.Fatalf("fetched = %v/%v", result.FetchedAt, result.FetchedISO)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal grants: %v", err)
	}
	for _, secret := range []string{"access-alpha", "refresh-alpha", "access-beta", "refresh-beta"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("grants leaked %q: %s", secret, raw)
		}
	}
}

func TestCreditsFetchGrantsEmpty(t *testing.T) {
	provider, _, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{"a": nil})
	provider.creditsNow = func() time.Time { return creditTestNow() }
	result, err := provider.Grants(context.Background())
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if !result.OK || result.Rows == nil || len(result.Rows) != 0 {
		t.Fatalf("empty grants = %+v", result)
	}
	if result.FetchedAt != nil || result.FetchedISO != nil {
		t.Fatalf("empty fetched fields = %v/%v", result.FetchedAt, result.FetchedISO)
	}
	if result.Summary.Count != 0 || result.Summary.Accounts != 0 ||
		result.Summary.Size != 0 || result.Summary.Remain != 0 || result.Summary.Used != 0 {
		t.Fatalf("empty summary = %+v", result.Summary)
	}
}

type creditsFailDoer struct {
	calls int
}

func (d *creditsFailDoer) Do(context.Context, *http.Request, string) (*http.Response, error) {
	d.calls++
	return nil, fmt.Errorf("unexpected upstream request")
}

func TestCreditsRefreshPacesMultipleAccounts(t *testing.T) {
	summary := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"IsPaidUser": false,
			"Packages":   []any{},
		},
	})
	emptyAccounts := creditTestJSON(t, map[string]any{"code": 0, "data": map[string]any{"Accounts": []any{}}})
	server, _ := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		switch r.URL.Path {
		case creditsSummaryPath:
			return http.StatusOK, summary
		case creditsFreePackagesPath, creditsPaidPackagesPath:
			return http.StatusOK, emptyAccounts
		case creditsCheckinPath:
			return http.StatusOK, `{"code":0,"data":{}}`
		default:
			return http.StatusNotFound, `{"code":404}`
		}
	})
	provider, _, _ := newCreditsPool(t, realmIntl, map[string]map[string]any{
		"a": nil, "b": nil, "c": nil,
	})
	provider.creditsBillingBaseURL = server.URL
	provider.creditsCheckinBaseURL = server.URL
	provider.tasksGap = 10 * time.Millisecond
	var sleeps []time.Duration
	provider.tasksSleep = func(delay time.Duration) { sleeps = append(sleeps, delay) }

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	if len(result.Results) != 3 {
		t.Fatalf("refresh rows = %+v", result.Results)
	}
	if len(sleeps) != 2 {
		t.Fatalf("inter-account pacing sleeps = %v, want 2", sleeps)
	}
	for _, delay := range sleeps {
		if delay != 10*time.Millisecond {
			t.Fatalf("pacing delay = %v", delay)
		}
	}
}

func TestCreditsCNPaginatesPackageHistory(t *testing.T) {
	now := creditTestNow()
	summaryBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"Packages": []any{map[string]any{"PackageCode": "pkg_free"}},
		},
	})
	makeFree := func(index int) map[string]any {
		return map[string]any{
			"AccountId":                fmt.Sprintf("free-%03d", index),
			"PackageName":              "Daily credit",
			"PackageCode":              "pkg_free",
			"CycleCapacitySizePrecise": "1",
			"CycleCapacityRemain":      "1",
			"Status":                   0,
		}
	}
	pageOne := make([]any, 0, 200)
	for i := 0; i < 200; i++ {
		pageOne = append(pageOne, makeFree(i))
	}
	pageTwo := []any{makeFree(200)}
	freePage := func(page int) string {
		accounts := pageOne
		if page == 2 {
			accounts = pageTwo
		}
		return creditTestJSON(t, map[string]any{
			"code": 0,
			"data": map[string]any{"Accounts": accounts},
		})
	}
	empty := creditTestJSON(t, map[string]any{"code": 0, "data": map[string]any{"Accounts": []any{}}})
	server, fixture := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		switch r.URL.Path {
		case creditsSummaryPath:
			return http.StatusOK, summaryBody
		case creditsFreePackagesPath:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			page := int(creditNumber(body["PageNumber"]))
			return http.StatusOK, freePage(page)
		case creditsPaidPackagesPath:
			return http.StatusOK, empty
		case creditsCheckinPath:
			return http.StatusOK, `{"code":0,"data":{}}`
		default:
			return http.StatusNotFound, `{"code":404}`
		}
	})
	provider, _, _ := newCreditsPool(t, realmCN, map[string]map[string]any{"cn-a": nil})
	provider.creditsBillingBaseURL = server.URL
	provider.creditsCheckinBaseURL = server.URL
	provider.creditsNow = func() time.Time { return now }

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	snapshot := result.Results[0].Credits
	if snapshot == nil {
		t.Fatal("missing snapshot")
	}
	if len(snapshot.Packages) != 201 {
		t.Fatalf("packages = %d, want 201", len(snapshot.Packages))
	}
	calls := creditCallsWithPath(fixture.snapshot(), creditsFreePackagesPath)
	if len(calls) != 2 {
		t.Fatalf("free package calls = %d, want 2: %+v", len(calls), calls)
	}
	if !strings.Contains(calls[0].Body, `"Status":[0,1,2,3]`) {
		t.Fatalf("first page status filter = %s", calls[0].Body)
	}
	if !strings.Contains(calls[1].Body, `"PageNumber":2`) {
		t.Fatalf("second page request = %s", calls[1].Body)
	}
}

func TestCreditsCNUsesOfficialSummaryTotalsWhenRowsExist(t *testing.T) {
	now := creditTestNow()
	summaryBody := creditTestJSON(t, map[string]any{
		"code": 0,
		"data": map[string]any{
			"Packages": []any{map[string]any{
				"PackageCode":         "pkg_free",
				"CycleTotalCapacity":  1000,
				"CycleRemainCapacity": 800,
				"CycleUsedCapacity":   200,
			}},
		},
	})
	freeRaw := map[string]any{
		"AccountId":                "f1",
		"PackageName":              "Free Plan Subscription",
		"PackageCode":              "pkg_free",
		"CycleCapacitySizePrecise": "100",
		"CycleCapacityRemain":      "100",
		"Status":                   0,
	}
	freeBody := creditTestJSON(t, map[string]any{
		"code": 0, "data": map[string]any{"Accounts": []any{freeRaw}},
	})
	empty := creditTestJSON(t, map[string]any{"code": 0, "data": map[string]any{"Accounts": []any{}}})
	server, _ := newCreditsTestServer(t, func(r *http.Request) (int, string) {
		switch r.URL.Path {
		case creditsSummaryPath:
			return http.StatusOK, summaryBody
		case creditsFreePackagesPath:
			return http.StatusOK, freeBody
		case creditsPaidPackagesPath:
			return http.StatusOK, empty
		case creditsCheckinPath:
			return http.StatusOK, `{"code":0,"data":{}}`
		default:
			return http.StatusNotFound, `{"code":404}`
		}
	})
	provider, _, _ := newCreditsPool(t, realmCN, map[string]map[string]any{"cn-a": nil})
	provider.creditsBillingBaseURL = server.URL
	provider.creditsCheckinBaseURL = server.URL
	provider.creditsNow = func() time.Time { return now }

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	snapshot := result.Results[0].Credits
	if snapshot == nil {
		t.Fatal("missing snapshot")
	}
	if snapshot.Size != 1000 || snapshot.Remain != 800 || snapshot.Used != 200 {
		t.Fatalf("summary totals = %v/%v/%v, want 1000/800/200", snapshot.Size, snapshot.Remain, snapshot.Used)
	}
	if snapshot.UsedPercent != "20.0%" || snapshot.RemainPercent != "80.0%" {
		t.Fatalf("summary percentages = %q/%q", snapshot.UsedPercent, snapshot.RemainPercent)
	}
}
