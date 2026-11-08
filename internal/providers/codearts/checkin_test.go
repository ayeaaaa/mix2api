package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func checkinCampaignFixture(id any, title, typ, status string, extra map[string]any) map[string]any {
	item := map[string]any{
		"campaignId": id, "title": title, "type": typ,
		"status": status, "claimable": true, "benefitUnit": "CREDIT",
	}
	if extra != nil {
		item["extra"] = extra
	}
	return item
}

func TestCheckinCampaignClassification(t *testing.T) {
	cases := []struct {
		name string
		item map[string]any
		want bool
	}{
		{"historical id 1", map[string]any{"campaignId": 1}, true},
		{"historical id 4", map[string]any{"campaignId": 4}, true},
		{"daily user login", map[string]any{"campaignId": 34, "type": "USER_LOGIN"}, true},
		{"lowercase type", map[string]any{"campaignId": 35, "type": "user_login"}, true},
		{"trigger event", map[string]any{
			"campaignId": 99, "type": "MANUAL_CLAIM",
			"extra": map[string]any{"triggerEvent": "User.Login"},
		}, true},
		{"invite", map[string]any{"campaignId": 3, "type": "INVITE_USER"}, false},
		{"numeric string is not a historical id", map[string]any{"campaignId": "4", "type": "INVITE_USER"}, false},
		{"unknown", map[string]any{"campaignId": 77, "type": "SOMETHING_ELSE"}, false},
		{"other trigger", map[string]any{
			"campaignId": 98, "type": "MANUAL_CLAIM",
			"extra": map[string]any{"triggerEvent": "user.register"},
		}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isCheckinCampaign(testCase.item); got != testCase.want {
				t.Fatalf("isCheckinCampaign(%#v) = %v, want %v", testCase.item, got, testCase.want)
			}
		})
	}
}

func TestCheckinEligibleClaimsConfirmsAndPersists(t *testing.T) {
	fake := newCreditFake()
	provider, serverURL := newCreditsProvider(t, fake)
	now := time.Date(2026, 11, 8, 9, 30, 0, 0, time.Local)
	setCatalogNow(t, provider, now)
	account := addCreditsAccount(t, provider, "u1")
	account.mu.Lock()
	account.proxy = serverURL
	account.mu.Unlock()
	account.SetInfo(accountInfo(0, 0))
	if !account.CreditExhausted() {
		t.Fatal("zero snapshot should park the account")
	}

	first := checkinCampaignFixture(1, "每日签到", "USER_LOGIN", "ELIGIBLE", map[string]any{
		"startTime": "2026-01-01T00:00:00Z", "endTime": "2026-12-31T00:00:00Z",
	})
	first["pendingTotalAmount"] = 1000
	second := checkinCampaignFixture(34, "每日签到（34）", "USER_LOGIN", "ELIGIBLE", nil)
	second["benefitAmount"] = 100
	already := checkinCampaignFixture(4, "新用户注册送 4000 积分", "REGISTER", "CONFIRMED", nil)
	invite := checkinCampaignFixture(3, "邀请好友", "INVITE_USER", "ELIGIBLE", nil)
	invite["pendingTotalAmount"] = 9999
	unknown := checkinCampaignFixture(77, "未知活动", "SOMETHING_ELSE", "ELIGIBLE", nil)
	unknown["pendingTotalAmount"] = 9999
	pointCampaign := checkinCampaignFixture(88, "积分类型活动", "USER_LOGIN", "ELIGIBLE", nil)
	pointCampaign["benefitUnit"] = "POINT"
	pointCampaign["pendingTotalAmount"] = 9999
	fake.setDelivery("u1", []any{first, second, already, invite, unknown, pointCampaign})

	result, err := provider.Checkin(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !result.OK || result.Credits != 1100 {
		t.Fatalf("checkin result = %+v", result)
	}
	if len(result.Campaigns) != 3 {
		t.Fatalf("campaigns = %#v, want eligible+already only", result.Campaigns)
	}
	wantMessage := "每日签到: 领取成功（1000 积分）; 每日签到（34）: 领取成功（1100 积分）; 新用户注册送 4000 积分: 已领取"
	if result.Message != wantMessage {
		t.Fatalf("message = %q, want %q", result.Message, wantMessage)
	}
	if result.Campaigns[0].Status != "CONFIRMED" || result.Campaigns[0].Credits == nil || *result.Campaigns[0].Credits != 1000 {
		t.Fatalf("first campaign = %+v", result.Campaigns[0])
	}
	if result.Campaigns[1].Status != "CONFIRMED" || result.Campaigns[1].Credits == nil || *result.Campaigns[1].Credits != 1100 {
		t.Fatalf("second campaign = %+v", result.Campaigns[1])
	}
	if result.Campaigns[2].Status != "CONFIRMED" || result.Campaigns[2].Credits != nil || result.Campaigns[2].Message != "已领取" {
		t.Fatalf("already-claimed campaign = %+v", result.Campaigns[2])
	}

	if got := fake.callCount(creditClaimPath); got != 2 {
		t.Fatalf("claim calls = %d, want 2", got)
	}
	if got := fake.callCount(creditConfirmPath); got != 2 {
		t.Fatalf("confirm calls = %d, want 2", got)
	}
	claims := fake.recordedBodies(creditClaimPath)
	for index, id := range []float64{1, 34} {
		var body map[string]any
		if err := json.Unmarshal([]byte(claims[index]), &body); err != nil {
			t.Fatalf("claim body: %v", err)
		}
		campaignID, _ := numericValue(body["campaignId"])
		if campaignID != id || body["channel"] != "IDE" {
			t.Fatalf("claim body = %#v, want campaign %v via IDE", body, id)
		}
		key, _ := body["idempotentKey"].(string)
		if !strings.HasPrefix(key, "claim_"+formatCampaignID(id)+"_") {
			t.Fatalf("idempotentKey = %q", key)
		}
	}
	confirms := fake.recordedBodies(creditConfirmPath)
	for index, id := range []float64{1, 34} {
		var body map[string]any
		if err := json.Unmarshal([]byte(confirms[index]), &body); err != nil {
			t.Fatalf("confirm body: %v", err)
		}
		campaignID, _ := numericValue(body["campaignId"])
		if campaignID != id {
			t.Fatalf("confirm body = %#v, want campaign %v", body, id)
		}
	}

	deliveryQuery := fake.lastQuery(creditDeliveryPath)
	if got := deliveryQuery.Get("channel"); got != "IDE" {
		t.Fatalf("delivery channel = %q", got)
	}
	if !fake.lastAbsolute(creditDeliveryPath) || !fake.lastAbsolute(creditClaimPath) {
		t.Fatal("account proxy was not used for check-in")
	}
	deliveryAuth := fake.lastHeader(creditDeliveryPath).Get("Authorization")
	if !strings.Contains(deliveryAuth, "SignedHeaders=agent-type;content-type;host;x-language;") {
		t.Fatalf("delivery signed headers = %q", deliveryAuth)
	}
	claimAuth := fake.lastHeader(creditClaimPath).Get("Authorization")
	if !strings.Contains(claimAuth, "SignedHeaders=agent-type;content-type;host;x-language;") {
		t.Fatalf("claim signed headers = %q", claimAuth)
	}
	headers := fake.lastHeader(creditDeliveryPath)
	if headers.Get("Agent-Type") != "PromptCenter" || headers.Get("X-Language") != "zh-cn" ||
		headers.Get("Content-Type") != "application/json" {
		t.Fatalf("delivery headers = %#v", headers)
	}

	if !account.Healthy(time.Now()) {
		t.Fatal("successful check-in with credits should unpark a zero snapshot")
	}
	account.mu.Lock()
	info := account.Info
	checkin := cloneCheckin(account.Checkin)
	account.mu.Unlock()
	if info != nil {
		t.Fatalf("credits > 0 should clear the zero snapshot: %#v", info)
	}
	if checkin == nil || checkin["ok"] != true || checkin["message"] != wantMessage {
		t.Fatalf("account checkin = %#v", checkin)
	}
	if got := checkin["checked_at"]; got != now.Format("2006-01-02T15:04:05") {
		t.Fatalf("checked_at = %v", got)
	}

	states, err := LoadState(filepath.Join(provider.accountsDir, "codearts", "state.json"))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	persisted := states["u1"].Checkin
	if persisted == nil || persisted["ok"] != true || persisted["checked_at"] != now.Format("2006-01-02T15:04:05") {
		t.Fatalf("persisted checkin = %#v", persisted)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"cloud-token-u1", "secret-key-u1", "refresh-token-u1", "AK-u1"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("checkin result leaked %q: %s", secret, encoded)
		}
	}

	// The next refresh sees the current positive balance instead of the stale
	// zero snapshot.
	fake.setPlugin("u1", fakePluginResponse(321))
	refreshed, err := provider.FetchInfo(context.Background(), account, false)
	if err != nil {
		t.Fatalf("FetchInfo after checkin: %v", err)
	}
	if refreshed.Credits.Total == nil || refreshed.Credits.Total.Remain != 321 {
		t.Fatalf("refreshed credits = %+v", refreshed.Credits.Total)
	}
}

func TestCheckinAlreadyClaimedAndNonCheckinFiltered(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.Local)
	setCatalogNow(t, provider, now)
	account := addCreditsAccount(t, provider, "u1")
	account.SetInfo(accountInfo(50, 0))

	confirmed := checkinCampaignFixture(1, "每日签到", "USER_LOGIN", "CONFIRMED", nil)
	consumed := checkinCampaignFixture(4, "新用户注册送 4000 积分", "REGISTER", "CONSUMED", nil)
	invite := checkinCampaignFixture(3, "邀请好友", "INVITE_USER", "ELIGIBLE", nil)
	invite["pendingTotalAmount"] = 500
	unknown := checkinCampaignFixture(77, "未知活动", "SOMETHING_ELSE", "ELIGIBLE", nil)
	unknown["pendingTotalAmount"] = 500
	pointCampaign := checkinCampaignFixture(88, "积分类型活动", "USER_LOGIN", "ELIGIBLE", nil)
	pointCampaign["benefitUnit"] = "POINT"
	pointCampaign["pendingTotalAmount"] = 500
	notCheckinTrigger := checkinCampaignFixture(98, "注册活动", "MANUAL_CLAIM", "ELIGIBLE",
		map[string]any{"triggerEvent": "user.register"})
	notCheckinTrigger["pendingTotalAmount"] = 500
	fake.setDelivery("u1", []any{
		confirmed, consumed, invite, unknown, pointCampaign, notCheckinTrigger,
	})

	result, err := provider.Checkin(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !result.OK || result.Credits != 0 || len(result.Campaigns) != 2 {
		t.Fatalf("already/non-checkin result = %+v", result)
	}
	if result.Message != "每日签到: 已领取; 新用户注册送 4000 积分: 已领取" {
		t.Fatalf("message = %q", result.Message)
	}
	if got := fake.callCount(creditClaimPath); got != 0 {
		t.Fatalf("claim calls = %d, want 0", got)
	}
	if got := fake.callCount(creditConfirmPath); got != 0 {
		t.Fatalf("confirm calls = %d, want 0", got)
	}

	// A zero-credit check-in still invalidates the info cache timestamp while
	// keeping the last snapshot for display.
	account.mu.Lock()
	info := account.Info
	account.mu.Unlock()
	if info == nil || info.FetchedAt != 0 {
		t.Fatalf("info cache timestamp not invalidated: %#v", info)
	}
	if got := info.Credits.Total.Remain; got != 50 {
		t.Fatalf("snapshot remain = %v, want 50", got)
	}
}

func TestCheckinClaimedStatusConfirmsWithoutClaiming(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 11, 0, 0, 0, time.Local))
	addCreditsAccount(t, provider, "u1")
	claimed := checkinCampaignFixture(35, "每日签到", "USER_LOGIN", "CLAIMED", nil)
	claimed["benefitAmount"] = 100
	fake.setDelivery("u1", []any{claimed})

	result, err := provider.Checkin(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !result.OK || result.Credits != 0 || len(result.Campaigns) != 1 {
		t.Fatalf("claimed result = %+v", result)
	}
	campaign := result.Campaigns[0]
	if campaign.Status != "CONFIRMED" || campaign.Credits == nil || *campaign.Credits != 0 ||
		campaign.Message != "已补确认" {
		t.Fatalf("claimed campaign = %+v", campaign)
	}
	if got := fake.callCount(creditClaimPath); got != 0 {
		t.Fatalf("claim calls = %d, want 0", got)
	}
	if got := fake.callCount(creditConfirmPath); got != 1 {
		t.Fatalf("confirm calls = %d, want 1", got)
	}
}

func TestCheckinAllSkipsDisabledAndAggregatesErrors(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local))
	good := addCreditsAccount(t, provider, "u-good")
	addCreditsAccount(t, provider, "u-bad")
	addCreditsAccount(t, provider, "u-off")
	if err := provider.Pool().Disable("u-off", "operator"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	eligible := checkinCampaignFixture(1, "每日签到", "USER_LOGIN", "ELIGIBLE", nil)
	eligible["pendingTotalAmount"] = 100
	fake.setDelivery("u-good", []any{eligible})
	fake.setDelivery("u-off", []any{eligible})
	fake.failDelivery("u-bad", http.StatusBadGateway,
		`{"error_msg":"bad secret-key-u-bad cloud-token-u-bad AK-u-bad"}`)

	result, err := provider.CheckinAll(context.Background())
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if result.OK {
		t.Fatalf("mixed CheckinAll OK = true: %+v", result)
	}
	if result.Credits != 100 || result.Message != "已处理 2 个账号，共领取 100 积分" {
		t.Fatalf("CheckinAll result = %+v", result)
	}
	if len(result.Results) != 2 {
		t.Fatalf("results = %#v", result.Results)
	}
	var goodRow, badRow *CheckinAccountResult
	for index := range result.Results {
		switch result.Results[index].UID {
		case "u-good":
			goodRow = &result.Results[index]
		case "u-bad":
			badRow = &result.Results[index]
		}
	}
	if goodRow == nil || !goodRow.OK || goodRow.Credits != 100 {
		t.Fatalf("good row = %#v", goodRow)
	}
	if badRow == nil || badRow.OK || badRow.Message == "" {
		t.Fatalf("bad row = %#v", badRow)
	}
	for _, secret := range []string{"secret-key-u-bad", "cloud-token-u-bad", "AK-u-bad"} {
		if strings.Contains(badRow.Message, secret) {
			t.Fatalf("bad row leaked %q: %s", secret, badRow.Message)
		}
	}
	if got := fake.callCountFor("u-off", creditDeliveryPath); got != 0 {
		t.Fatalf("disabled delivery calls = %d, want 0", got)
	}
	if got := fake.callCountFor("u-good", creditClaimPath); got != 1 {
		t.Fatalf("good claim calls = %d, want 1", got)
	}
	if !good.Healthy(time.Now()) {
		t.Fatal("successful all-checkin should leave the good account healthy")
	}
}

func TestCheckinManualSingleBypassesDisabledAndUnknownUID(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 13, 0, 0, 0, time.Local))
	addCreditsAccount(t, provider, "u1")
	if err := provider.Pool().Disable("u1", "operator"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	eligible := checkinCampaignFixture(1, "每日签到", "USER_LOGIN", "ELIGIBLE", nil)
	eligible["pendingTotalAmount"] = 100
	fake.setDelivery("u1", []any{eligible})

	result, err := provider.Checkin(context.Background(), "u1")
	if err != nil {
		t.Fatalf("manual Checkin: %v", err)
	}
	if !result.OK || result.Credits != 100 {
		t.Fatalf("manual disabled result = %+v", result)
	}
	if _, err := provider.Checkin(context.Background(), "missing"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing Checkin error = %v, want ErrAccountNotFound", err)
	}
}

func TestCheckinConcurrentCallersHaveOneWinner(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 14, 0, 0, 0, time.Local))
	addCreditsAccount(t, provider, "u1")
	eligible := checkinCampaignFixture(1, "每日签到", "USER_LOGIN", "ELIGIBLE", nil)
	eligible["pendingTotalAmount"] = 100
	fake.setDelivery("u1", []any{eligible})
	started, release := fake.blockNextClaim()

	firstResult := make(chan CheckinResult, 1)
	firstErr := make(chan error, 1)
	go func() {
		result, err := provider.Checkin(context.Background(), "u1")
		firstResult <- result
		firstErr <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first check-in never reached claim")
	}

	secondResult := make(chan CheckinResult, 1)
	secondErr := make(chan error, 1)
	go func() {
		result, err := provider.Checkin(context.Background(), "u1")
		secondResult <- result
		secondErr <- err
	}()
	var loser CheckinResult
	select {
	case loser = <-secondResult:
	case <-time.After(2 * time.Second):
		t.Fatal("second check-in blocked instead of reporting the per-account flight")
	}
	release()

	winner := <-firstResult
	if err := <-firstErr; err != nil {
		t.Fatalf("winning Checkin: %v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("losing Checkin: %v", err)
	}
	if !winner.OK || winner.Credits != 100 {
		t.Fatalf("winner = %+v", winner)
	}
	if loser.OK || loser.Credits != 0 || len(loser.Campaigns) != 0 ||
		loser.Message != "该账号签到正在进行中，稍后刷新即可" {
		t.Fatalf("loser = %+v", loser)
	}
	busyJSON, err := json.Marshal(loser)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(busyJSON), `"campaigns":[]`) {
		t.Fatalf("busy result must keep the v1 campaigns array: %s", busyJSON)
	}
	if got := fake.callCount(creditClaimPath); got != 1 {
		t.Fatalf("claim calls = %d, want 1", got)
	}
	if got := fake.callCount(creditConfirmPath); got != 1 {
		t.Fatalf("confirm calls = %d, want 1", got)
	}
}

func TestCheckinSequentialIdempotencyDoesNotClaimTwice(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 15, 0, 0, 0, time.Local))
	addCreditsAccount(t, provider, "u1")
	eligible := checkinCampaignFixture(1, "每日签到", "USER_LOGIN", "ELIGIBLE", nil)
	eligible["pendingTotalAmount"] = 100
	fake.setDelivery("u1", []any{eligible})

	first, err := provider.Checkin(context.Background(), "u1")
	if err != nil {
		t.Fatalf("first Checkin: %v", err)
	}
	if !first.OK || first.Credits != 100 {
		t.Fatalf("first = %+v", first)
	}
	fake.setDelivery("u1", []any{
		checkinCampaignFixture(1, "每日签到", "USER_LOGIN", "CONFIRMED", nil),
	})
	second, err := provider.Checkin(context.Background(), "u1")
	if err != nil {
		t.Fatalf("second Checkin: %v", err)
	}
	if !second.OK || second.Credits != 0 || second.Campaigns[0].Message != "已领取" {
		t.Fatalf("second = %+v", second)
	}
	if got := fake.callCount(creditClaimPath); got != 1 {
		t.Fatalf("claim calls after two check-ins = %d, want 1", got)
	}
	if got := fake.callCount(creditConfirmPath); got != 1 {
		t.Fatalf("confirm calls after two check-ins = %d, want 1", got)
	}
}

func TestCheckinResultsRedactSecretsFromCampaignText(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 17, 0, 0, 0, time.Local))
	account := addCreditsAccount(t, provider, "u1")
	leaky := checkinCampaignFixture(1,
		"签到 cloud-token-u1 secret-key-u1 refresh-token-u1 AK-u1",
		"USER_LOGIN", "CONFIRMED", nil)
	fake.setDelivery("u1", []any{leaky})

	result, err := provider.Checkin(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	account.mu.Lock()
	checkin := cloneCheckin(account.Checkin)
	account.mu.Unlock()
	persisted, err := json.Marshal(checkin)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"cloud-token-u1", "secret-key-u1", "refresh-token-u1", "AK-u1"} {
		for name, body := range map[string][]byte{"result": encoded, "persisted": persisted} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("%s leaked %q: %s", name, secret, body)
			}
		}
	}
	if !strings.Contains(result.Message, "<redacted>") {
		t.Fatalf("redaction marker missing: %q", result.Message)
	}
}

func TestCheckinZeroSnapshotClearedOnlyWhenCreditsArrive(t *testing.T) {
	fake := newCreditFake()
	provider, _ := newCreditsProvider(t, fake)
	setCatalogNow(t, provider, time.Date(2026, 11, 8, 16, 0, 0, 0, time.Local))
	account := addCreditsAccount(t, provider, "u1")
	account.SetInfo(accountInfo(0, 0))
	if account.Healthy(time.Now()) {
		t.Fatal("zero-credit snapshot should park the account")
	}
	eligible := checkinCampaignFixture(1, "每日签到", "USER_LOGIN", "ELIGIBLE", nil)
	eligible["pendingTotalAmount"] = 100
	fake.setDelivery("u1", []any{eligible})
	fake.setPlugin("u1", fakePluginResponse(100))

	result, err := provider.Checkin(context.Background(), "u1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !result.OK || result.Credits != 100 {
		t.Fatalf("result = %+v", result)
	}
	account.mu.Lock()
	info := account.Info
	account.mu.Unlock()
	if info != nil {
		t.Fatalf("positive check-in must clear the parked snapshot: %#v", info)
	}
	if !account.Healthy(time.Now()) {
		t.Fatal("account stayed parked after a positive check-in")
	}
	loaded, err := provider.FetchInfo(context.Background(), account, false)
	if err != nil {
		t.Fatalf("FetchInfo: %v", err)
	}
	if loaded.Credits.Total == nil || loaded.Credits.Total.Remain != 100 {
		t.Fatalf("post-checkin overview credits = %+v", loaded.Credits.Total)
	}
}
