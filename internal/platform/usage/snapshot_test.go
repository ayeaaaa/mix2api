package usage

import (
	"context"
	"testing"
	"time"
)

func TestSnapshotFieldsAndWindow(t *testing.T) {
	now := time.Now()
	today := float64(localMidnight(now).Unix()) + 3600
	yesterday := today - 86400
	rows := []map[string]any{
		row(today, "m", "intl", "acct-a", 100, 10, map[string]any{"cached_tokens": 40, "credit": 1.0}),
		row(today-60, "m", "cn", "acct-b", 200, 20, map[string]any{"credit": 2.0}),
		row(yesterday, "m", "intl", "acct-a", 50, 5, nil),
	}
	env := newTestEnv(t, rows)
	service := env.service()

	all, err := service.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, all["requests"]) != 3 || mustNumber(t, all["total_tokens"]) != 385 {
		t.Fatalf("all-time totals: %+v", all)
	}
	for _, field := range []string{"started", "since", "log_file", "realm", "usd_cny", "accounts_map", "account"} {
		if _, ok := all[field]; !ok {
			t.Fatalf("missing %s in snapshot", field)
		}
	}
	byModel := mustMap(t, all["by_model"])
	bucket := mustMap(t, byModel["m"])
	if mustNumber(t, bucket["requests"]) != 3 || mustNumber(t, bucket["cached_tokens"]) != 40 {
		t.Fatalf("by_model = %+v", bucket)
	}
	accounts := mustMap(t, bucket["accounts"])
	if mustNumber(t, accounts["acct-a"]) != 2 || mustNumber(t, accounts["acct-b"]) != 1 {
		t.Fatalf("account counts = %+v", accounts)
	}
	byRealm := mustMap(t, mustMap(t, all["by_model_realm"])["m"])
	if _, ok := byRealm["cn"]; !ok {
		t.Fatalf("by_model_realm = %+v", byRealm)
	}
	byAcct := mustMap(t, mustMap(t, mustMap(t, all["by_model_acct"])["m"])["cn"])
	if _, ok := byAcct["acct-b"]; !ok {
		t.Fatalf("by_model_acct = %+v", byAcct)
	}

	todaySnap, err := service.Snapshot(context.Background(), SnapshotOptions{Range: "today"})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, todaySnap["requests"]) != 2 || mustNumber(t, todaySnap["total_tokens"]) != 330 {
		t.Fatalf("today window = %+v", todaySnap)
	}
	lo := time.Unix(int64(today-60), 0)
	hi := time.Unix(int64(today), 0)
	custom, err := service.Snapshot(context.Background(), SnapshotOptions{Range: "custom", Since: &lo, Until: &hi})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, custom["requests"]) != 2 {
		t.Fatalf("custom window = %+v", custom)
	}
}

func TestSnapshotCountsErrorsAndCredits(t *testing.T) {
	now := float64(time.Now().Unix())
	rows := []map[string]any{
		row(now, "m", "intl", "a", 100, 10, map[string]any{"credit": 1.0}),
		row(now-1, "m", "intl", "a", 0, 0, map[string]any{"error": true, "outcome": "failed", "credit": 2.0, "prompt_tokens": 50, "total_tokens": 50}),
		row(now-2, "m", "intl", "a", 0, 0, map[string]any{"outcome": "client_aborted", "credit": 5.0, "prompt_tokens": 70, "total_tokens": 70}),
	}
	env := newTestEnv(t, rows)
	snapshot, err := env.service().Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, snapshot["requests"]) != 1 {
		t.Fatalf("requests = %+v", snapshot)
	}
	if mustNumber(t, snapshot["errors"]) != 2 {
		t.Fatalf("errors = %+v", snapshot)
	}
	if mustNumber(t, snapshot["credit"]) != 3.0 {
		t.Fatalf("credit should include failed but not client abort: %+v", snapshot["credit"])
	}
	if mustNumber(t, snapshot["prompt_tokens"]) != 100 {
		t.Fatalf("token totals must be completed-only: %+v", snapshot["prompt_tokens"])
	}
}

func TestNoCrossRealmMerge(t *testing.T) {
	now := float64(time.Now().Unix())
	rows := []map[string]any{
		row(now, "m", "intl", "a", 10, 1, nil),
		row(now, "m", "cn", "b", 20, 2, nil),
	}
	env := newTestEnv(t, rows)
	service := env.service()
	intl, err := service.Snapshot(context.Background(), SnapshotOptions{Realm: "intl"})
	if err != nil {
		t.Fatal(err)
	}
	cn, err := service.Snapshot(context.Background(), SnapshotOptions{Realm: "cn"})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, intl["requests"]) != 1 || mustNumber(t, intl["total_tokens"]) != 11 {
		t.Fatalf("intl = %+v", intl)
	}
	if mustNumber(t, cn["requests"]) != 1 || mustNumber(t, cn["total_tokens"]) != 22 {
		t.Fatalf("cn = %+v", cn)
	}
	all, err := service.Snapshot(context.Background(), SnapshotOptions{Realm: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if mustNumber(t, all["requests"]) != 2 {
		t.Fatalf("realm=all should not be a generic union omission: %+v", all)
	}
}

func TestSnapshotUsesLiveAccountInventory(t *testing.T) {
	now := float64(time.Now().Unix())
	rows := []map[string]any{
		row(now, "m", "intl", "oracle-intl", 10, 1, nil),
		row(now, "m", "cn", "oracle-cn", 20, 2, nil),
	}
	env := newTestEnv(t, rows)
	service := env.service()
	accounts := map[string]AccountInfo{
		"oracle-intl": {UID: "oracle-intl", Nickname: "Oracle intl", Realm: "intl", Domain: "www.workbuddy.ai"},
		"oracle-cn":   {UID: "oracle-cn", Nickname: "Oracle cn", Realm: "cn", Domain: "www.workbuddy.cn"},
	}
	service.AccountsFunc = func() map[string]AccountInfo { return accounts }
	service.CurrentAccountFunc = func(realm string) AccountInfo {
		info := AccountInfo{Accounts: len(accounts), AccountsReady: len(accounts)}
		if realm == "all" {
			realm = ""
		}
		for uid, account := range accounts {
			if realm != "" && account.Realm != realm {
				continue
			}
			info.UID = uid
			info.Nickname = account.Nickname
			info.Realm = account.Realm
			info.Domain = account.Domain
			info.CredentialFile = uid + ".json"
			info.ExpiresAt = 1794380055
			break
		}
		return info
	}

	for _, tc := range []struct {
		realm string
		uid   string
	}{{"intl", "oracle-intl"}, {"cn", "oracle-cn"}} {
		snapshot, err := service.Snapshot(context.Background(), SnapshotOptions{Realm: tc.realm})
		if err != nil {
			t.Fatal(err)
		}
		mapped := mustMap(t, snapshot["accounts_map"])
		if len(mapped) != 2 {
			t.Fatalf("%s accounts_map = %#v", tc.realm, mapped)
		}
		if _, ok := mapped[tc.uid]; !ok {
			t.Fatalf("%s accounts_map missing %s: %#v", tc.realm, tc.uid, mapped)
		}
		current := mustMap(t, snapshot["account"])
		if current["uid"] != tc.uid || mustNumber(t, current["accounts"]) != 2 ||
			mustNumber(t, current["accounts_ready"]) != 2 {
			t.Fatalf("%s current account = %#v", tc.realm, current)
		}
	}
}

// TestSnapshotGroupsByRequestedModelKey keeps by_model keyed by the
// exit-qualified id when the row carries one; bare rows still key by model.
func TestSnapshotGroupsByRequestedModelKey(t *testing.T) {
	now := float64(time.Now().Unix())
	env := newTestEnv(t, []map[string]any{
		row(now, "deepseek", "cn", "acct-a", 10, 1, map[string]any{"requested_model": "cn/deepseek"}),
		row(now-10, "deepseek", "codearts", "acct-b", 20, 2, map[string]any{"requested_model": "codearts/deepseek"}),
		row(now-20, "deepseek", "intl", "acct-c", 30, 3, nil),
	})
	all, err := env.service().Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byModel := mustMap(t, all["by_model"])
	want := []string{"cn/deepseek", "codearts/deepseek", "deepseek"}
	if len(byModel) != len(want) {
		t.Fatalf("by_model keys = %#v, want %#v", byModel, want)
	}
	for _, id := range want {
		bucket := mustMap(t, byModel[id])
		if mustNumber(t, bucket["requests"]) != 1 {
			t.Fatalf("by_model[%q] = %#v, want exactly one request", id, bucket)
		}
	}
}

// TestSnapshotCostMissingUsesBilledModel keeps cost_missing on the model the
// pricer looked up: the billed upstream ID, not the canonical requested ID.
func TestSnapshotCostMissingUsesBilledModel(t *testing.T) {
	now := float64(time.Now().Unix())
	env := newTestEnv(t, []map[string]any{
		row(now, "unpriced-upstream", "cn", "acct-a", 10, 1, map[string]any{
			"requested_model": "cn/unpriced-upstream",
		}),
	})
	snapshot, err := env.service().Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	missing := mustMap(t, snapshot["cost_missing"])
	if mustNumber(t, missing["unpriced-upstream"]) != 1 {
		t.Fatalf("cost_missing = %#v, want one unpriced-upstream entry", missing)
	}
	if _, ok := missing["cn/unpriced-upstream"]; ok {
		t.Fatalf("cost_missing points at the canonical id: %#v", missing)
	}
}
