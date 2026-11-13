package codearts

import (
	"context"
	"encoding/json"
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
	creditPluginPath   = "/snap-manager/v1/statistics/plugin"
	creditDeliveryPath = "/v1/ops/delivery"
	creditClaimPath    = "/v1/ops/claim"
	creditConfirmPath  = "/v1/ops/confirm"
)

type creditHTTPFailure struct {
	status int
	body   string
}

// creditFake is the httptest CodeArts upstream for Task 7. It records every
// request, selects fixtures by the Huawei Access key embedded in the signature,
// and can block a claim to exercise per-account check-in locking.
type creditFake struct {
	mu sync.Mutex

	calls      map[string]int
	callsByUID map[string]map[string]int
	headers    map[string][]http.Header
	queries    map[string][]url.Values
	bodies     map[string][]string
	uids       map[string][]string
	absolute   map[string][]bool

	plugins          map[string]map[string]any
	pluginFailures   map[string]creditHTTPFailure
	deliveries       map[string][]any
	deliveryFailures map[string]creditHTTPFailure
	claimFailures    map[string]creditHTTPFailure
	confirmFailures  map[string]creditHTTPFailure

	claimStarted chan struct{}
	claimRelease chan struct{}
}

func newCreditFake() *creditFake {
	return &creditFake{
		calls:            make(map[string]int),
		callsByUID:       make(map[string]map[string]int),
		headers:          make(map[string][]http.Header),
		queries:          make(map[string][]url.Values),
		bodies:           make(map[string][]string),
		uids:             make(map[string][]string),
		absolute:         make(map[string][]bool),
		plugins:          make(map[string]map[string]any),
		pluginFailures:   make(map[string]creditHTTPFailure),
		deliveries:       make(map[string][]any),
		deliveryFailures: make(map[string]creditHTTPFailure),
		claimFailures:    make(map[string]creditHTTPFailure),
		confirmFailures:  make(map[string]creditHTTPFailure),
	}
}

func fakePluginFixture() map[string]any {
	return map[string]any{
		"metrics": []any{
			map[string]any{
				"name": "usageTotalPackageCredit", "package_credit_amount": 4500.0,
				"package_credit_used": 0.81, "package_credit_remain": 4499.19,
				"package_credit_expiring_amount": 0,
			},
			map[string]any{
				"name": "usageBasicPackageCredit", "package_credit_amount": 500.0,
				"package_credit_used": 0.0, "package_credit_remain": 500.0,
				"package_credit_expiring_amount": 0,
			},
			map[string]any{
				"name": "usageOnDemandPackageCredit", "package_credit_amount": 0,
				"package_credit_used": 0, "package_credit_remain": 0,
				"package_credit_expiring_amount": 0,
			},
			map[string]any{
				"name": "usageBonusPackageCredit", "package_credit_amount": 4000.0,
				"package_credit_used": 0.81, "package_credit_remain": 3999.19,
				"package_credit_expiring_amount": 0,
			},
		},
		"model_metrics": []any{
			map[string]any{
				"model_id": "GLM-5.2", "model_name": "GLM-5.2",
				"model_desc": "最新旗舰模型", "model_type": "system",
				"enable": true, "status": "normal", "is_luxury": false,
			},
		},
		"package": map[string]any{
			"spec_code": "codearts.agent.individual.trial", "status": "normal",
			"package_name_cn": "体验版", "package_name_en": "Trial",
			"is_token_package": false, "is_credit_package": true,
		},
		"start_date": "2026-10-31",
		"end_date":   "2026-12-01",
	}
}

func fakePluginResponse(remain float64) map[string]any {
	return map[string]any{
		"metrics": []any{map[string]any{
			"name": "usageTotalPackageCredit", "package_credit_amount": 1000.0,
			"package_credit_used": 1000.0 - remain, "package_credit_remain": remain,
			"package_credit_expiring_amount": 5,
		}},
		"package": map[string]any{
			"spec_code": "codearts.agent.individual.trial", "status": "normal",
			"package_name_cn": "体验版", "is_credit_package": true,
		},
		"start_date": "2026-10-31",
		"end_date":   "2026-12-01",
	}
}

func (f *creditFake) uidForRequest(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	index := strings.Index(auth, "Access=")
	if index < 0 {
		return ""
	}
	access := auth[index+len("Access="):]
	if end := strings.Index(access, ","); end >= 0 {
		access = access[:end]
	}
	return strings.TrimPrefix(strings.TrimSpace(access), "AK-")
}

func (f *creditFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	uid := f.uidForRequest(r)

	f.mu.Lock()
	f.calls[path]++
	if f.callsByUID[uid] == nil {
		f.callsByUID[uid] = make(map[string]int)
	}
	f.callsByUID[uid][path]++
	f.headers[path] = append(f.headers[path], r.Header.Clone())
	f.queries[path] = append(f.queries[path], r.URL.Query())
	f.bodies[path] = append(f.bodies[path], string(raw))
	f.uids[path] = append(f.uids[path], uid)
	f.absolute[path] = append(f.absolute[path], r.URL.IsAbs())

	plugin, hasPlugin := f.plugins[uid]
	pluginFailure, pluginFailed := f.pluginFailures[uid]
	items, hasDelivery := f.deliveries[uid]
	deliveryFailure, deliveryFailed := f.deliveryFailures[uid]
	claimFailure, claimFailed := f.claimFailures[uid]
	confirmFailure, confirmFailed := f.confirmFailures[uid]
	started := f.claimStarted
	release := f.claimRelease
	f.mu.Unlock()

	if path == creditPluginPath {
		if pluginFailed {
			writeCreditFakeResponse(w, pluginFailure.status, pluginFailure.body)
			return
		}
		if !hasPlugin {
			plugin = fakePluginFixture()
		}
		writeCreditFakeJSON(w, plugin)
		return
	}

	switch path {
	case creditDeliveryPath:
		if deliveryFailed {
			writeCreditFakeResponse(w, deliveryFailure.status, deliveryFailure.body)
			return
		}
		if !hasDelivery {
			items = []any{}
		}
		writeCreditFakeJSON(w, map[string]any{
			"code": 0, "message": "ok", "data": map[string]any{"items": items},
		})
	case creditClaimPath:
		if claimFailed {
			writeCreditFakeResponse(w, claimFailure.status, claimFailure.body)
			return
		}
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		if release != nil {
			<-release
		}
		writeCreditFakeJSON(w, map[string]any{"code": 0, "message": "ok", "data": map[string]any{}})
	case creditConfirmPath:
		if confirmFailed {
			writeCreditFakeResponse(w, confirmFailure.status, confirmFailure.body)
			return
		}
		writeCreditFakeJSON(w, map[string]any{"code": 0, "message": "ok", "data": map[string]any{}})
	default:
		writeCreditFakeResponse(w, http.StatusNotFound, `{"error_msg":"not found"}`)
	}
}

func writeCreditFakeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func writeCreditFakeResponse(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *creditFake) setPlugin(uid string, response map[string]any) {
	f.mu.Lock()
	f.plugins[uid] = response
	delete(f.pluginFailures, uid)
	f.mu.Unlock()
}

func (f *creditFake) failPlugin(uid string, status int, body string) {
	f.mu.Lock()
	f.pluginFailures[uid] = creditHTTPFailure{status: status, body: body}
	f.mu.Unlock()
}

func (f *creditFake) setDelivery(uid string, items []any) {
	f.mu.Lock()
	f.deliveries[uid] = items
	delete(f.deliveryFailures, uid)
	f.mu.Unlock()
}

func (f *creditFake) failDelivery(uid string, status int, body string) {
	f.mu.Lock()
	f.deliveryFailures[uid] = creditHTTPFailure{status: status, body: body}
	f.mu.Unlock()
}

func (f *creditFake) failClaim(uid string, status int, body string) {
	f.mu.Lock()
	f.claimFailures[uid] = creditHTTPFailure{status: status, body: body}
	f.mu.Unlock()
}

func (f *creditFake) clearClaimFailure(uid string) {
	f.mu.Lock()
	delete(f.claimFailures, uid)
	f.mu.Unlock()
}

func (f *creditFake) failConfirm(uid string, status int, body string) {
	f.mu.Lock()
	f.confirmFailures[uid] = creditHTTPFailure{status: status, body: body}
	f.mu.Unlock()
}

func (f *creditFake) clearConfirmFailure(uid string) {
	f.mu.Lock()
	delete(f.confirmFailures, uid)
	f.mu.Unlock()
}

func (f *creditFake) blockNextClaim() (<-chan struct{}, func()) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	f.mu.Lock()
	f.claimStarted = started
	f.claimRelease = release
	f.mu.Unlock()
	var once sync.Once
	return started, func() { once.Do(func() { close(release) }) }
}

func (f *creditFake) callCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

func (f *creditFake) callCountFor(uid, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callsByUID[uid][path]
}

func (f *creditFake) lastHeader(path string) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	headers := f.headers[path]
	if len(headers) == 0 {
		return nil
	}
	return headers[len(headers)-1].Clone()
}

func (f *creditFake) lastQuery(path string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	queries := f.queries[path]
	if len(queries) == 0 {
		return nil
	}
	return queries[len(queries)-1]
}

func (f *creditFake) lastAbsolute(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	values := f.absolute[path]
	if len(values) == 0 {
		return false
	}
	return values[len(values)-1]
}

func (f *creditFake) recordedBodies(path string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies[path]...)
}

func newCreditsProvider(t *testing.T, fake *creditFake) (*Provider, string) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	t.Setenv("WB_CODEARTS_SNAP_HOST", server.URL)
	t.Setenv("WB_CODEARTS_STS_HOST", server.URL)

	dir := t.TempDir()
	provider, err := New(config.New(filepath.Join(dir, "settings.json")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider, server.URL
}

func addCreditsAccount(t *testing.T, provider *Provider, uid string) *Account {
	t.Helper()
	auth := testAuth(uid)
	auth.Expiration = time.Now().Add(4 * time.Hour)
	account, err := provider.Pool().Add(auth)
	if err != nil {
		t.Fatalf("Pool.Add(%s): %v", uid, err)
	}
	return account
}

func TestCreditsFetchInfoMetricsBucketsPackageAndModels(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)
	account := addCreditsAccount(t, provider, "u1")

	info, err := provider.FetchInfo(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchInfo: %v", err)
	}
	if info == nil {
		t.Fatal("FetchInfo returned nil")
	}

	if !info.IsCreditPackage || info.IsTokenPackage {
		t.Fatalf("package flags = credit:%v token:%v", info.IsCreditPackage, info.IsTokenPackage)
	}
	if info.SpecCode != "codearts.agent.individual.trial" ||
		info.PackageName != "体验版" || info.PackageStatus != "normal" ||
		info.StartDate != "2026-10-31" || info.EndDate != "2026-12-01" {
		t.Fatalf("package fields = %+v", info)
	}
	if info.FetchedAt != float64(now.Unix()) {
		t.Fatalf("FetchedAt = %v, want %v", info.FetchedAt, now.Unix())
	}
	if info.Credits.Unit != "credit" {
		t.Fatalf("credits unit = %q", info.Credits.Unit)
	}
	assertCreditTotal(t, "total", info.Credits.Total, 4500, 0.81, 4499.19, 0)
	assertCreditTotal(t, "basic", info.Credits.Basic, 500, 0, 500, 0)
	assertCreditTotal(t, "on_demand", info.Credits.OnDemand, 0, 0, 0, 0)
	assertCreditTotal(t, "bonus", info.Credits.Bonus, 4000, 0.81, 3999.19, 0)

	if len(info.Models) != 1 {
		t.Fatalf("model metrics = %#v", info.Models)
	}
	model := info.Models[0]
	if model.ID != "GLM-5.2" || model.Name != "GLM-5.2" ||
		model.Description != "最新旗舰模型" || model.Type != "system" ||
		!model.Enabled || model.Status != "normal" || model.Luxury {
		t.Fatalf("model metric = %+v", model)
	}

	if _, err := provider.FetchInfo(context.Background(), account, false); err != nil {
		t.Fatalf("cached FetchInfo: %v", err)
	}
	if got := fake.callCount(creditPluginPath); got != 1 {
		t.Fatalf("plugin calls = %d, want 1", got)
	}

	headers := fake.lastHeader(creditPluginPath)
	if headers == nil {
		t.Fatal("statistics request was not recorded")
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := headers.Get("Agent-Type"); got != "PromptCenter" {
		t.Fatalf("Agent-Type = %q", got)
	}
	if got := headers.Get("X-Language"); got != "zh-cn" {
		t.Fatalf("X-Language = %q", got)
	}
	if got := headers.Get("X-Security-Token"); got != "cloud-token-u1" {
		t.Fatalf("X-Security-Token = %q", got)
	}
	authorization := headers.Get("Authorization")
	if !strings.Contains(authorization, "SignedHeaders=content-type;host;x-sdk-content-sha256;x-sdk-date;x-security-token") {
		t.Fatalf("statistics signed headers = %q", authorization)
	}
	if strings.Contains(authorization, "agent-type") || strings.Contains(authorization, "x-language") {
		t.Fatalf("Agent-Type/X-Language must be appended after signing: %q", authorization)
	}
	if bodies := fake.recordedBodies(creditPluginPath); len(bodies) != 1 || bodies[0] != "" {
		t.Fatalf("statistics GET body = %#v", bodies)
	}

	// SetInfo installs a private copy; a caller mutation cannot race the panel.
	info.Credits.Total.Remain = -1
	account.mu.Lock()
	remain := account.Info.Credits.Total.Remain
	account.mu.Unlock()
	if remain != 4499.19 {
		t.Fatalf("account snapshot aliased caller info: remain = %v", remain)
	}

	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"cloud-token-u1", "secret-key-u1", "refresh-token-u1"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("account info leaked %q: %s", secret, encoded)
		}
	}
}

func assertCreditTotal(t *testing.T, name string, total *CreditTotal, amount, used, remain, expiring float64) {
	t.Helper()
	if total == nil {
		t.Fatalf("%s credit total is nil", name)
	}
	if total.Amount != amount || total.Used != used || total.Remain != remain || total.Expiring != expiring {
		t.Fatalf("%s = %+v, want amount=%v used=%v remain=%v expiring=%v",
			name, *total, amount, used, remain, expiring)
	}
}

func TestCreditsFetchInfoCacheForceAndPriorFallback(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)
	account := addCreditsAccount(t, provider, "u1")

	first, err := provider.FetchInfo(context.Background(), account, false)
	if err != nil {
		t.Fatalf("first FetchInfo: %v", err)
	}
	if _, err := provider.FetchInfo(context.Background(), account, false); err != nil {
		t.Fatalf("cached FetchInfo: %v", err)
	}
	if got := fake.callCount(creditPluginPath); got != 1 {
		t.Fatalf("cached plugin calls = %d, want 1", got)
	}

	setCatalogNow(t, provider, now.Add(30*time.Second))
	if _, err := provider.FetchInfo(context.Background(), account, false); err != nil {
		t.Fatalf("30s FetchInfo: %v", err)
	}
	if got := fake.callCount(creditPluginPath); got != 1 {
		t.Fatalf("plugin calls inside TTL = %d, want 1", got)
	}

	setCatalogNow(t, provider, now.Add(61*time.Second))
	if _, err := provider.FetchInfo(context.Background(), account, false); err != nil {
		t.Fatalf("expired FetchInfo: %v", err)
	}
	if got := fake.callCount(creditPluginPath); got != 2 {
		t.Fatalf("plugin calls after TTL = %d, want 2", got)
	}

	if _, err := provider.FetchInfo(context.Background(), account, false); err != nil {
		t.Fatalf("fresh FetchInfo: %v", err)
	}
	if got := fake.callCount(creditPluginPath); got != 2 {
		t.Fatalf("fresh cache plugin calls = %d, want 2", got)
	}
	if _, err := provider.FetchInfo(context.Background(), account, true); err != nil {
		t.Fatalf("forced FetchInfo: %v", err)
	}
	if got := fake.callCount(creditPluginPath); got != 3 {
		t.Fatalf("forced plugin calls = %d, want 3", got)
	}

	prior := first.Credits.Total.Remain
	retained, err := fetchCreditsFailureFixture(t, fake, provider, account, now.Add(70*time.Second))
	if err != nil {
		t.Fatalf("stale FetchInfo must swallow the upstream failure like v1: %v", err)
	}
	if retained == nil || retained.Credits.Total == nil || retained.Credits.Total.Remain != prior {
		t.Fatalf("failure did not retain prior info: %#v", retained)
	}
	account.mu.Lock()
	current := account.Info
	account.mu.Unlock()
	if current == nil || current.Credits.Total == nil || current.Credits.Total.Remain != prior {
		t.Fatalf("account lost prior info after failure: %#v", current)
	}

	// RefreshCredits must still distinguish a stale fallback from a real
	// refresh, even though the public FetchInfo call returns nil error.
	refresh, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("stale RefreshCredits: %v", err)
	}
	if refresh.OK || refresh.Refreshed != 0 || len(refresh.Errors) != 1 || len(refresh.Accounts) != 1 {
		t.Fatalf("stale refresh counted as success: %+v", refresh)
	}
	if refresh.Accounts[0].OK || refresh.Accounts[0].Credits != nil {
		t.Fatalf("stale refresh row = %+v", refresh.Accounts[0])
	}
	for _, secret := range []string{"cloud-token-u1", "secret-key-u1", "refresh-token-u1", "AK-u1"} {
		if strings.Contains(refresh.Errors[0], secret) {
			t.Fatalf("refresh error leaked %q: %s", secret, refresh.Errors[0])
		}
	}

	// No prior snapshot: the failure is returned without installing one.
	cold := addCreditsAccount(t, provider, "u-cold")
	fake.failPlugin("u-cold", http.StatusBadGateway,
		`{"error_msg":"bad cloud-token-u-cold secret-key-u-cold"}`)
	coldInfo, coldErr := provider.FetchInfo(context.Background(), cold, true)
	if coldErr == nil || coldInfo != nil {
		t.Fatalf("cold failure = info:%#v err:%v", coldInfo, coldErr)
	}
	if strings.Contains(coldErr.Error(), "secret-key-u-cold") {
		t.Fatalf("cold error leaked secret: %v", coldErr)
	}
}

func fetchCreditsFailureFixture(t *testing.T, fake *creditFake, provider *Provider, account *Account, now time.Time) (*AccountInfo, error) {
	t.Helper()
	setCatalogNow(t, provider, now)
	fake.failPlugin("u1", http.StatusBadGateway,
		`{"error_msg":"bad cloud-token-u1 secret-key-u1 refresh-token-u1 AK-u1"}`)
	return provider.FetchInfo(context.Background(), account, true)
}

func TestCreditsRefreshErrorCapAndRemovedAccountSkip(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 18, 0, 0, 0, time.Local))
	for _, uid := range []string{"u1", "u2", "u3", "u4"} {
		addCreditsAccount(t, provider, uid)
		fake.failPlugin(uid, http.StatusBadGateway, `{"error_msg":"bad"}`)
	}

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits: %v", err)
	}
	if result.OK || result.Refreshed != 0 {
		t.Fatalf("all-failed refresh = %+v", result)
	}
	if len(result.Accounts) != 4 {
		t.Fatalf("rows = %d, want 4", len(result.Accounts))
	}
	if len(result.Errors) != creditRefreshErrorLimit {
		t.Fatalf("errors = %#v, want v1 cap %d", result.Errors, creditRefreshErrorLimit)
	}
	for _, row := range result.Accounts {
		if row.OK || row.Credits != nil {
			t.Fatalf("failed row = %+v", row)
		}
	}

	// A removed account left in the in-memory pool is not live and must be
	// skipped by both refresh and all-account check-in.
	fake = newCreditFake()
	provider, _ = newCreditsProvider(t, fake)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 18, 30, 0, 0, time.Local))
	live := addCreditsAccount(t, provider, "u-live")
	removed := addCreditsAccount(t, provider, "u-removed")
	removed.mu.Lock()
	removed.removed = true
	removed.mu.Unlock()
	fake.setDelivery("u-live", []any{})

	refresh, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits with removed account: %v", err)
	}
	if !refresh.OK || refresh.Refreshed != 1 || len(refresh.Accounts) != 1 || refresh.Accounts[0].UID != "u-live" {
		t.Fatalf("removed account not skipped by refresh: %+v", refresh)
	}
	checkins, err := provider.CheckinAll(context.Background())
	if err != nil {
		t.Fatalf("CheckinAll with removed account: %v", err)
	}
	if !checkins.OK || len(checkins.Results) != 1 || checkins.Results[0].UID != "u-live" {
		t.Fatalf("removed account not skipped by check-in: %+v", checkins)
	}
	if got := fake.callCountFor("u-removed", creditPluginPath); got != 0 {
		t.Fatalf("removed account plugin calls = %d", got)
	}
	if got := fake.callCountFor("u-removed", creditDeliveryPath); got != 0 {
		t.Fatalf("removed account delivery calls = %d", got)
	}
	if !live.Healthy(time.Now()) {
		t.Fatal("live account should remain healthy")
	}
}

func TestCreditsNonScalarFieldsAreSafe(t *testing.T) {
	now := time.Date(2026, 11, 8, 19, 0, 0, 0, time.Local)
	info := parseAccountInfo(map[string]any{
		"metrics": []any{map[string]any{
			"name":                           creditTotalPackageMetric,
			"package_credit_amount":          []any{1, 2},
			"package_credit_used":            map[string]any{"bad": true},
			"package_credit_remain":          []any{3},
			"package_credit_expiring_amount": nil,
		}},
		"package": map[string]any{
			"is_credit_package": map[string]any{"truthy": true},
			"package_name_cn":   []any{"ignored"},
		},
		"model_metrics": []any{map[string]any{
			"model_id": "m1", "enable": []any{}, "is_luxury": map[string]any{"x": 1},
		}},
	}, now)
	if info.IsCreditPackage {
		t.Fatal("non-boolean is_credit_package must not enable the flag")
	}
	if info.PackageName != "" {
		t.Fatalf("non-scalar package name = %q", info.PackageName)
	}
	if info.Credits.Total == nil || info.Credits.Total.Amount != 0 || info.Credits.Total.Remain != 0 {
		t.Fatalf("non-scalar metrics must normalize to zero: %+v", info.Credits.Total)
	}
	if len(info.Models) != 1 || !info.Models[0].Enabled || info.Models[0].Luxury {
		t.Fatalf("non-scalar model flags = %+v", info.Models)
	}
	if got := campaignCreditAmount(map[string]any{
		"pendingTotalAmount": []any{100}, "benefitAmount": 42.5,
	}); got != 42.5 {
		t.Fatalf("non-scalar pending fallback = %v, want benefitAmount 42.5", got)
	}
	if isCheckinCampaign(map[string]any{"campaignId": map[string]any{"x": 1}, "type": []any{"USER_LOGIN"}}) {
		t.Fatal("non-scalar campaign fields must not classify as check-in")
	}
}

func TestCreditsErrorTextCodeFallback(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"error_msg":"boom","code":400}`, "boom"},
		{`{"code":400}`, "code=400"},
		{`{"error_code":"InferHub.002002009.404"}`, "error_code=InferHub.002002009.404"},
		{`{"code":0,"error_code":"0"}`, `{"code":0,"error_code":"0"}`},
	}
	for _, testCase := range cases {
		if got := catalogErrorText([]byte(testCase.body)); got != testCase.want {
			t.Fatalf("catalogErrorText(%s) = %q, want %q", testCase.body, got, testCase.want)
		}
	}
}

func TestCreditsRefreshMixedResultsSkipDisabledAndNoOverallFailure(t *testing.T) {
	fake := newCreditFake()
	fake.setPlugin("u-good", fakePluginResponse(123))
	fake.setPlugin("u-bad", fakePluginResponse(1))
	provider, _ := newCreditsProvider(t, fake)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)

	good := addCreditsAccount(t, provider, "u-good")
	bad := addCreditsAccount(t, provider, "u-bad")
	disabled := addCreditsAccount(t, provider, "u-off")
	if err := provider.Pool().Disable("u-off", "operator"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	fake.failPlugin("u-bad", http.StatusBadGateway,
		`{"error_msg":"bad secret-key-u-bad cloud-token-u-bad AK-u-bad"}`)

	result, err := provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("RefreshCredits should not fail the whole pool: %v", err)
	}
	if result.OK {
		t.Fatalf("mixed refresh OK = true: %+v", result)
	}
	if result.Refreshed != 1 {
		t.Fatalf("refreshed = %d, want 1", result.Refreshed)
	}
	if len(result.Accounts) != 2 {
		t.Fatalf("rows = %#v, want good+bad only", result.Accounts)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "bad") {
		t.Fatalf("errors = %#v", result.Errors)
	}
	for _, secret := range []string{"secret-key-u-bad", "cloud-token-u-bad", "AK-u-bad"} {
		if strings.Contains(result.Errors[0], secret) {
			t.Fatalf("refresh error leaked %q: %s", secret, result.Errors[0])
		}
	}
	if !strings.Contains(result.Message, "已刷新 1 个账号的积分") {
		t.Fatalf("message = %q", result.Message)
	}

	var goodRow, badRow *CreditsRefreshRow
	for index := range result.Accounts {
		switch result.Accounts[index].UID {
		case "u-good":
			goodRow = &result.Accounts[index]
		case "u-bad":
			badRow = &result.Accounts[index]
		}
	}
	if goodRow == nil || !goodRow.OK || goodRow.Credits == nil || goodRow.Credits.Remain != 123 {
		t.Fatalf("good row = %#v", goodRow)
	}
	if badRow == nil || badRow.OK || badRow.Credits != nil || badRow.Message == "" {
		t.Fatalf("bad row = %#v", badRow)
	}
	if got := fake.callCountFor("u-off", creditPluginPath); got != 0 {
		t.Fatalf("disabled account plugin calls = %d, want 0", got)
	}
	good.mu.Lock()
	loaded := good.Info
	good.mu.Unlock()
	if loaded == nil || loaded.Credits.Total == nil || loaded.Credits.Total.Remain != 123 {
		t.Fatalf("good snapshot = %#v", loaded)
	}
	bad.mu.Lock()
	badInfo := bad.Info
	bad.mu.Unlock()
	if badInfo != nil {
		t.Fatalf("failed account should not install a snapshot: %#v", badInfo)
	}
	if disabled.CreditExhausted() {
		t.Fatal("disabled account should be skipped")
	}

	fake.setPlugin("u-bad", fakePluginResponse(77))
	result, err = provider.RefreshCredits(context.Background())
	if err != nil {
		t.Fatalf("clean RefreshCredits: %v", err)
	}
	if !result.OK || result.Refreshed != 2 || len(result.Errors) != 0 || len(result.Accounts) != 2 {
		t.Fatalf("clean refresh = %+v", result)
	}
	if result.Errors == nil || result.Accounts == nil {
		t.Fatalf("refresh JSON slices must be present, got %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "u-off") {
		t.Fatalf("disabled account appeared in result: %s", encoded)
	}
}
