package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

type oauthCall struct {
	Method string
	Path   string
	Query  map[string]string
	Header http.Header
	Body   string
	Proxy  string
}

type oauthFake struct {
	mu      sync.Mutex
	calls   []oauthCall
	handler func(req *http.Request, proxy string) (int, string, error)
}

func (f *oauthFake) Do(ctx context.Context, req *http.Request, proxy string) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		body = string(raw)
	}
	call := oauthCall{
		Method: req.Method,
		Path:   req.URL.Path,
		Query:  map[string]string{},
		Header: req.Header.Clone(),
		Body:   body,
		Proxy:  proxy,
	}
	for key, values := range req.URL.Query() {
		if len(values) > 0 {
			call.Query[key] = values[0]
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	status, payload, err := f.handler(req, proxy)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}, nil
}

func (f *oauthFake) snapshot() []oauthCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]oauthCall(nil), f.calls...)
}

func newOAuthTestProvider(t *testing.T, realm string) *Provider {
	t.Helper()
	dir := t.TempDir()
	provider, err := New(realm, config.New(filepath.Join(dir, "settings.json")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider.oauthBaseURL = "https://oauth.test"
	provider.oauthNow = func() time.Time { return time.Unix(1000, 0) }
	return provider
}

type oauthUpstream struct {
	mu          sync.Mutex
	tokenPolls  int
	tokenBody   string
	profileBody string
	profileCode int
}

func (u *oauthUpstream) handler(t *testing.T) func(*http.Request, string) (int, string, error) {
	return func(req *http.Request, proxy string) (int, string, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return 200, `{"code":0,"data":{"state":"st-1","authUrl":"https://browser.test/authorize?state=st-1"}}`, nil
		case "/v2/plugin/auth/token":
			u.mu.Lock()
			defer u.mu.Unlock()
			u.tokenPolls++
			if u.tokenBody != "" {
				return 200, u.tokenBody, nil
			}
			if u.tokenPolls <= 1 {
				return 200, `{"code":11217,"msg":"waiting for browser login"}`, nil
			}
			return 200, `{"code":0,"data":{"accessToken":"` + testAccessToken(t) +
				`","refreshToken":"refresh-token","expiresAt":4102444800000,"domain":"www.workbuddy.ai"}}`, nil
		case "/v2/plugin/login/account":
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.profileCode != 0 {
				return u.profileCode, `{"error":"upstream failure"}`, nil
			}
			if u.profileBody != "" {
				return 200, u.profileBody, nil
			}
			return 200, `{"code":0,"data":{"nickname":"Alice"}}`, nil
		default:
			return 404, `{"error":"not found"}`, nil
		}
	}
}

func testAccessToken(t *testing.T) string {
	t.Helper()
	return makeJWT(t, map[string]any{
		"sub":           "uid-oauth",
		"iss":           "https://www.workbuddy.ai/auth/realms/copilot",
		"exp":           4102444800,
		"enterprise_id": "ent-oauth",
	})
}

func TestOAuthStartPollPendingSuccessAndNoTokenLeak(t *testing.T) {
	provider := newOAuthTestProvider(t, realmIntl)
	upstream := &oauthUpstream{}
	fake := &oauthFake{handler: upstream.handler(t)}
	provider.oauthClient = fake

	start, err := provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatalf("BeginOAuth: %v", err)
	}
	if !start.OK || start.State != "st-1" || start.Realm != realmIntl ||
		start.Platform != "CLI" || start.ExpiresIn != 600 ||
		start.ProxySlot != "" || start.Proxy != "" ||
		!strings.Contains(start.AuthURL, "state=st-1") {
		t.Fatalf("OAuthStart = %+v", start)
	}
	if encoded, _ := json.Marshal(start); strings.Contains(string(encoded), "accessToken") {
		t.Fatalf("start response leaked credential fields: %s", encoded)
	}

	calls := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("start made %d requests, want 1", len(calls))
	}
	call := calls[0]
	if call.Method != http.MethodPost || call.Path != "/v2/plugin/auth/state" ||
		call.Query["platform"] != "CLI" || call.Body != "{}" {
		t.Fatalf("start request = %+v", call)
	}
	if got := call.Header.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Fatalf("start X-Requested-With = %q", got)
	}
	if !strings.HasPrefix(call.Header.Get("User-Agent"), "WorkBuddy/") {
		t.Fatalf("start User-Agent = %q", call.Header.Get("User-Agent"))
	}

	pending, err := provider.PollOAuth(context.Background(), start.State)
	if err != nil {
		t.Fatalf("PollOAuth pending: %v", err)
	}
	if pending.Status != "pending" || !pending.OK ||
		!strings.Contains(pending.Message, "waiting") {
		t.Fatalf("pending poll = %+v", pending)
	}

	poll, err := provider.PollOAuth(context.Background(), start.State)
	if err != nil {
		t.Fatalf("PollOAuth success: %v", err)
	}
	if poll.Status != "ok" || !poll.OK || poll.Account == nil {
		t.Fatalf("success poll = %+v", poll)
	}
	if poll.Account.UID != "uid-oauth" || poll.Account.Nickname != "Alice" ||
		poll.Account.Realm != realmIntl || poll.Account.APIKey {
		t.Fatalf("poll account = %+v", poll.Account)
	}
	if encoded, _ := json.Marshal(poll); strings.Contains(string(encoded), "eyJ") ||
		strings.Contains(string(encoded), "refresh-token") {
		t.Fatalf("poll response leaked credentials: %s", encoded)
	}

	accounts, err := LoadAccounts(provider.AccountsDir())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("LoadAccounts = %+v, %v", accounts, err)
	}
	account := accounts[0]
	if account.UID != "uid-oauth" || account.Nickname != "Alice" ||
		account.AccessToken != testAccessToken(t) ||
		account.RefreshToken != "refresh-token" ||
		account.Realm != realmIntl ||
		account.Domain != "www.workbuddy.ai" ||
		account.Platform != "CLI" ||
		account.EnterpriseID != "ent-oauth" ||
		account.Source != "oauth" || !account.Enabled {
		t.Fatalf("saved OAuth account = %+v", account)
	}
	if account.ExpiresAt.Unix() != 4102444800 {
		t.Fatalf("expiresAt = %v", account.ExpiresAt)
	}
	if account.AddedAt.IsZero() {
		t.Fatal("addedAt was not initialized")
	}

	calls = fake.snapshot()
	var tokenCall, profileCall *oauthCall
	for i := range calls {
		call := calls[i]
		if call.Path == "/v2/plugin/auth/token" {
			tokenCall = &calls[i]
		}
		if call.Path == "/v2/plugin/login/account" {
			profileCall = &calls[i]
		}
	}
	if tokenCall == nil || tokenCall.Query["state"] != "st-1" {
		t.Fatalf("token poll request = %+v", tokenCall)
	}
	if profileCall == nil || profileCall.Query["state"] != "st-1" ||
		profileCall.Header.Get("Authorization") != "Bearer "+testAccessToken(t) {
		t.Fatalf("profile request = %+v", profileCall)
	}

	unknown, err := provider.PollOAuth(context.Background(), start.State)
	if err != nil {
		t.Fatalf("PollOAuth after success: %v", err)
	}
	if unknown.Status != "unknown" || unknown.OK {
		t.Fatalf("consumed session poll = %+v", unknown)
	}
}

func TestOAuthPollErrorsPendingNetworkExpiryAndCancel(t *testing.T) {
	provider := newOAuthTestProvider(t, realmIntl)
	upstream := &oauthUpstream{}
	fake := &oauthFake{handler: upstream.handler(t)}
	provider.oauthClient = fake

	start, err := provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatal(err)
	}
	upstream.tokenBody = `{"code":4001,"msg":"denied"}`
	poll, err := provider.PollOAuth(context.Background(), start.State)
	if err != nil {
		t.Fatalf("PollOAuth error status: %v", err)
	}
	if poll.Status != "error" || poll.OK || !strings.Contains(poll.Message, "4001") {
		t.Fatalf("error poll = %+v", poll)
	}
	if strings.Contains(poll.Message, "accessToken") || strings.Contains(poll.Message, "eyJ") {
		t.Fatalf("poll error leaked credentials: %+v", poll)
	}
	if !provider.CancelOAuth(start.State) {
		t.Fatal("CancelOAuth returned false for a live session")
	}
	if provider.CancelOAuth(start.State) {
		t.Fatal("CancelOAuth returned true after cancel")
	}
	if poll, _ := provider.PollOAuth(context.Background(), start.State); poll.Status != "unknown" {
		t.Fatalf("cancelled session poll = %+v", poll)
	}

	start, err = provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatal(err)
	}
	fake.handler = func(req *http.Request, proxy string) (int, string, error) {
		if req.URL.Path == "/v2/plugin/auth/token" {
			return 0, "", errors.New("Get \"https://oauth.test/v2/plugin/auth/token?state=st-1\": connection refused")
		}
		return 200, `{"code":0,"data":{"state":"st-1","authUrl":"https://browser.test/x"}}`, nil
	}
	poll, err = provider.PollOAuth(context.Background(), start.State)
	if err != nil {
		t.Fatalf("PollOAuth network error: %v", err)
	}
	if poll.Status != "pending" || !strings.Contains(poll.Message, "poll error") {
		t.Fatalf("network poll = %+v", poll)
	}
	provider.CancelOAuth(start.State)

	provider.oauthNow = func() time.Time { return time.Unix(1000, 0) }
	provider.oauthClient = fake
	fake.handler = upstream.handler(t)
	start, err = provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatal(err)
	}
	provider.oauthNow = func() time.Time { return time.Unix(1601, 0) }
	poll, err = provider.PollOAuth(context.Background(), start.State)
	if err != nil {
		t.Fatalf("PollOAuth expiry: %v", err)
	}
	if poll.Status != "expired" || poll.OK {
		t.Fatalf("expired poll = %+v", poll)
	}
	if provider.CancelOAuth(start.State) {
		t.Fatal("expired session was still cancellable")
	}

	poll, err = provider.PollOAuth(context.Background(), "missing")
	if err != nil || poll.Status != "unknown" {
		t.Fatalf("unknown poll = %+v, %v", poll, err)
	}
}

func TestOAuthStartErrorsAreSafe(t *testing.T) {
	provider := newOAuthTestProvider(t, realmIntl)
	provider.oauthClient = &oauthFake{handler: func(req *http.Request, proxy string) (int, string, error) {
		return 200, `{"code":0,"data":{"state":"st"}}`, nil
	}}
	_, err := provider.BeginOAuth(context.Background(), "CLI", "")
	if err == nil || !strings.Contains(err.Error(), "authUrl") {
		t.Fatalf("missing authUrl error = %v", err)
	}

	provider.oauthClient = &oauthFake{handler: func(req *http.Request, proxy string) (int, string, error) {
		return 502, `<html>bad gateway</html>`, nil
	}}
	_, err = provider.BeginOAuth(context.Background(), "CLI", "")
	if err == nil {
		t.Fatal("HTTP 502 start was accepted")
	}
	if strings.Contains(err.Error(), "<html>") || strings.Contains(err.Error(), "bad gateway") {
		t.Fatalf("start error leaked upstream body: %v", err)
	}
}

func TestOAuthDuplicateReloginPreservesOperatorState(t *testing.T) {
	provider := newOAuthTestProvider(t, realmIntl)
	dir := provider.AccountsDir()
	existingToken := makeJWT(t, map[string]any{"sub": "uid-oauth", "exp": 4102444800})
	added := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	existing := Auth{
		UID:                "uid-oauth",
		Nickname:           "Old",
		Realm:              realmIntl,
		Domain:             "www.workbuddy.ai",
		AccessToken:        existingToken,
		RefreshToken:       "old-refresh",
		ExpiresAt:          time.Unix(4102444800, 0),
		Platform:           "CLI",
		Product:            "workbuddy",
		Enabled:            true,
		TasksEnabled:       false,
		TasksEnabledSet:    true,
		Weight:             7,
		Note:               "operator note",
		Group:              "batch-a",
		DisabledModels:     []string{"model-x"},
		ProxySlot:          "slot-9",
		Source:             "oauth",
		AddedAt:            added,
		Tier:               json.RawMessage(`{"paid":true}`),
		LastCheckin:        "2026-11-01 09:00:00",
		LastDailyChat:      "2026-11-01 10:00:00",
		LastActivityReport: "2026-11-01 11:00:00",
		Extra: map[string]json.RawMessage{
			"restrictedUntil": json.RawMessage(`12345`),
			"modelCaps":       json.RawMessage(`{"model-x":{"at":1,"reset":2}}`),
		},
	}
	if _, err := SaveAccount(dir, existing); err != nil {
		t.Fatalf("seed existing account: %v", err)
	}

	upstream := &oauthUpstream{}
	provider.oauthClient = &oauthFake{handler: upstream.handler(t)}
	start, err := provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatal(err)
	}
	poll, err := provider.PollOAuth(context.Background(), start.State)
	if err != nil || poll.Status != "pending" {
		t.Fatalf("first poll = %+v, %v", poll, err)
	}
	poll, err = provider.PollOAuth(context.Background(), start.State)
	if err != nil || poll.Status != "ok" {
		t.Fatalf("relogin poll = %+v, %v", poll, err)
	}

	accounts, err := LoadAccounts(dir)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("LoadAccounts = %+v, %v", accounts, err)
	}
	got := accounts[0]
	if got.AccessToken != testAccessToken(t) || got.RefreshToken != "refresh-token" {
		t.Fatalf("credentials were not refreshed: %+v", got)
	}
	if got.Nickname != "Alice" || !got.Enabled {
		t.Fatalf("login identity/enabled = %+v", got)
	}
	if got.TasksEnabled {
		t.Fatal("tasksEnabled false was not preserved")
	}
	if got.Weight != 7 || got.Note != "operator note" || got.Group != "batch-a" ||
		!equalStrings(got.DisabledModels, []string{"model-x"}) ||
		got.ProxySlot != "slot-9" {
		t.Fatalf("operator state was not preserved: %+v", got)
	}
	if !got.AddedAt.Equal(added) {
		t.Fatalf("addedAt = %v, want %v", got.AddedAt, added)
	}
	if got.LastCheckin != existing.LastCheckin ||
		got.LastDailyChat != existing.LastDailyChat ||
		got.LastActivityReport != existing.LastActivityReport {
		t.Fatalf("task timestamps were not preserved: %+v", got)
	}
	if !jsonEqual(got.Tier, existing.Tier) {
		t.Fatalf("tier = %s, want %s", got.Tier, existing.Tier)
	}
	if got.Extra["restrictedUntil"] == nil || got.Extra["modelCaps"] == nil {
		t.Fatalf("state extras were not preserved: %v", got.Extra)
	}
}

func TestOAuthProxySlotsAutoExplicitAndRedaction(t *testing.T) {
	provider := newOAuthTestProvider(t, realmIntl)
	if _, err := provider.store.Merge(map[string]any{
		"proxy_slots": []any{
			map[string]any{"id": "slot-1", "url": "http://user:secret@127.0.0.1:8080", "enabled": true},
			map[string]any{"id": "slot-2", "url": "http://127.0.0.1:8081", "enabled": true},
			map[string]any{"id": "slot-3", "url": "http://127.0.0.1:8082", "enabled": false},
		},
	}); err != nil {
		t.Fatal(err)
	}
	boundToken := makeJWT(t, map[string]any{"sub": "uid-bound"})
	if _, err := SaveAccount(provider.AccountsDir(), Auth{
		UID: "uid-bound", AccessToken: boundToken, Realm: realmIntl, Enabled: true,
		ProxySlot: "slot-1",
	}); err != nil {
		t.Fatal(err)
	}

	fake := &oauthFake{handler: func(req *http.Request, proxy string) (int, string, error) {
		return 200, `{"code":0,"data":{"state":"st-auto","authUrl":"https://browser.test/x"}}`, nil
	}}
	provider.oauthClient = fake
	start, err := provider.BeginOAuth(context.Background(), "CLI", "auto")
	if err != nil {
		t.Fatalf("BeginOAuth auto: %v", err)
	}
	if start.ProxySlot != "slot-2" {
		t.Fatalf("auto slot = %q, want the only unbound enabled slot", start.ProxySlot)
	}
	if start.Proxy != "127.0.0.1:8081" {
		t.Fatalf("auto proxy display = %q", start.Proxy)
	}
	if calls := fake.snapshot(); len(calls) != 1 || calls[0].Proxy != "http://127.0.0.1:8081" {
		t.Fatalf("auto request proxy = %+v", fake.snapshot())
	}
	provider.CancelOAuth(start.State)

	start, err = provider.BeginOAuth(context.Background(), "CLI", "slot-1")
	if err != nil {
		t.Fatalf("BeginOAuth explicit: %v", err)
	}
	if start.ProxySlot != "slot-1" || start.Proxy != "127.0.0.1:8080" {
		t.Fatalf("explicit start = %+v", start)
	}
	if strings.Contains(start.Proxy, "secret") || strings.Contains(start.Proxy, "user") {
		t.Fatalf("proxy credentials leaked: %+v", start)
	}
	provider.CancelOAuth(start.State)

	if _, err := provider.BeginOAuth(context.Background(), "CLI", "slot-3"); err == nil {
		t.Fatal("disabled slot was accepted")
	}
	if _, err := provider.BeginOAuth(context.Background(), "CLI", "missing"); err == nil {
		t.Fatal("unknown slot was accepted")
	}
}

func TestOAuthProfileFailureStillCompletesLogin(t *testing.T) {
	provider := newOAuthTestProvider(t, realmIntl)
	upstream := &oauthUpstream{profileCode: 500}
	provider.oauthClient = &oauthFake{handler: upstream.handler(t)}
	start, err := provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.PollOAuth(context.Background(), start.State); err != nil {
		t.Fatal(err)
	}
	poll, err := provider.PollOAuth(context.Background(), start.State)
	if err != nil || poll.Status != "ok" || poll.Account == nil {
		t.Fatalf("poll = %+v, %v", poll, err)
	}
	if poll.Account.Nickname != "uid-oaut" {
		t.Fatalf("fallback nickname = %q, want uid prefix", poll.Account.Nickname)
	}
}

func TestOAuthCNRealmUsesRealmEndpointsAndDefaults(t *testing.T) {
	provider := newOAuthTestProvider(t, realmCN)
	upstream := &oauthUpstream{}
	fake := &oauthFake{handler: func(req *http.Request, proxy string) (int, string, error) {
		if req.URL.Path == "/v2/plugin/auth/token" {
			return 200, `{"code":0,"data":{"accessToken":"` + makeJWT(t,
				map[string]any{"sub": "uid-cn-oauth", "exp": 4102444800}) +
				`","refreshToken":"r","domain":""}}`, nil
		}
		return upstream.handler(t)(req, proxy)
	}}
	provider.oauthClient = fake
	start, err := provider.BeginOAuth(context.Background(), "WorkBuddy", "")
	if err != nil {
		t.Fatal(err)
	}
	if start.Realm != realmCN || start.Platform != "WorkBuddy" {
		t.Fatalf("CN start = %+v", start)
	}
	poll, err := provider.PollOAuth(context.Background(), start.State)
	if err != nil || poll.Status != "ok" || poll.Account == nil {
		t.Fatalf("CN poll = %+v, %v", poll, err)
	}
	if poll.Account.Realm != realmCN || poll.Account.Domain != "copilot.tencent.com" {
		t.Fatalf("CN account = %+v", poll.Account)
	}
	calls := fake.snapshot()
	if calls[0].Header.Get("Origin") != "https://www.codebuddy.cn" {
		t.Fatalf("CN Origin = %q", calls[0].Header.Get("Origin"))
	}
	if calls[0].Query["platform"] != "WorkBuddy" {
		t.Fatalf("CN platform query = %q", calls[0].Query["platform"])
	}
}

func TestOAuthGeneratedStateIsRejectedCallerState(t *testing.T) {
	provider := newOAuthTestProvider(t, realmIntl)
	provider.oauthClient = &oauthFake{handler: func(req *http.Request, proxy string) (int, string, error) {
		return 200, `{"code":0,"data":{"state":"","authUrl":"https://browser.test/x"}}`, nil
	}}
	if _, err := provider.BeginOAuth(context.Background(), "CLI", ""); err == nil {
		t.Fatal("empty upstream state was accepted")
	}
}

func TestOAuthHTTPTimeoutsAreBounded(t *testing.T) {
	provider := newOAuthTestProvider(t, realmIntl)
	if got := provider.oauthStartBudget(); got != 30*time.Second {
		t.Fatalf("default start budget = %v, want 30s", got)
	}
	if got := provider.oauthTokenBudget(); got != 30*time.Second {
		t.Fatalf("default token budget = %v, want 30s", got)
	}
	if got := provider.oauthProfileBudget(); got != 15*time.Second {
		t.Fatalf("default profile budget = %v, want 15s", got)
	}
	provider.oauthStartTimeout = 25 * time.Millisecond
	provider.oauthTokenTimeout = 25 * time.Millisecond
	provider.oauthProfileTimeout = 25 * time.Millisecond
	fake := &oauthFake{handler: func(req *http.Request, proxy string) (int, string, error) {
		<-req.Context().Done()
		return 0, "", req.Context().Err()
	}}
	provider.oauthClient = fake

	begin := time.Now()
	if _, err := provider.BeginOAuth(context.Background(), "CLI", ""); err == nil {
		t.Fatal("blocking OAuth state call did not time out")
	}
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Fatalf("OAuth state timeout took %v", elapsed)
	}

	fake.handler = func(req *http.Request, proxy string) (int, string, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return 200, `{"code":0,"data":{"state":"st-timeout","authUrl":"https://browser.test/x"}}`, nil
		case "/v2/plugin/auth/token":
			<-req.Context().Done()
			return 0, "", req.Context().Err()
		default:
			return 404, `{}`, nil
		}
	}
	start, err := provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatalf("BeginOAuth token timeout setup: %v", err)
	}
	pollStart := time.Now()
	poll, err := provider.PollOAuth(context.Background(), start.State)
	if err != nil || poll.Status != "pending" {
		t.Fatalf("token timeout poll = %+v, %v", poll, err)
	}
	if elapsed := time.Since(pollStart); elapsed > 2*time.Second {
		t.Fatalf("OAuth token timeout took %v", elapsed)
	}

	token := makeJWT(t, map[string]any{"sub": "uid-timeout", "exp": 4102444800})
	fake.handler = func(req *http.Request, proxy string) (int, string, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return 200, `{"code":0,"data":{"state":"st-profile-timeout","authUrl":"https://browser.test/x"}}`, nil
		case "/v2/plugin/auth/token":
			return 200, `{"code":0,"data":{"accessToken":"` + token +
				`","refreshToken":"r","domain":"www.workbuddy.ai"}}`, nil
		case "/v2/plugin/login/account":
			<-req.Context().Done()
			return 0, "", req.Context().Err()
		default:
			return 404, `{}`, nil
		}
	}
	start, err = provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatalf("BeginOAuth profile timeout setup: %v", err)
	}
	pollStart = time.Now()
	poll, err = provider.PollOAuth(context.Background(), start.State)
	if err != nil || poll.Status != "ok" || poll.Account == nil {
		t.Fatalf("profile timeout poll = %+v, %v", poll, err)
	}
	if elapsed := time.Since(pollStart); elapsed > 2*time.Second {
		t.Fatalf("OAuth profile timeout took %v", elapsed)
	}
	if poll.Account.Nickname != "uid-time" {
		t.Fatalf("profile-timeout fallback nickname = %q", poll.Account.Nickname)
	}
}

func TestOAuthPollMessagesRedactTokensInAllBranches(t *testing.T) {
	provider := newOAuthTestProvider(t, realmIntl)
	access := "access-token-secret-12345"
	refresh := "refresh-token-secret-67890"
	fake := &oauthFake{handler: func(req *http.Request, proxy string) (int, string, error) {
		if req.URL.Path == "/v2/plugin/auth/state" {
			return 200, `{"code":0,"data":{"state":"st-redact","authUrl":"https://browser.test/x"}}`, nil
		}
		return 200, `{"code":11217,"msg":"pending access=` + access + ` refresh=` + refresh + `",
			"accessToken":"` + access + `","refreshToken":"` + refresh + `",
			"data":{"accessToken":"` + access + `","refreshToken":"` + refresh + `"}}`, nil
	}}
	provider.oauthClient = fake
	start, err := provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatal(err)
	}
	poll, err := provider.PollOAuth(context.Background(), start.State)
	if err != nil {
		t.Fatalf("pending poll: %v", err)
	}
	if poll.Status != "pending" {
		t.Fatalf("pending poll status = %q", poll.Status)
	}
	if strings.Contains(poll.Message, access) || strings.Contains(poll.Message, refresh) {
		t.Fatalf("pending message leaked a token: %q", poll.Message)
	}
	if !strings.Contains(poll.Message, "<redacted>") {
		t.Fatalf("pending message was not redacted: %q", poll.Message)
	}
	provider.CancelOAuth(start.State)

	fake.handler = func(req *http.Request, proxy string) (int, string, error) {
		if req.URL.Path == "/v2/plugin/auth/state" {
			return 200, `{"code":0,"data":{"state":"st-redact","authUrl":"https://browser.test/x"}}`, nil
		}
		return 200, `{"code":4001,"msg":"rejected access=` + access + ` refresh=` + refresh + `",
			"data":{"accessToken":"` + access + `","refreshToken":"` + refresh + `"}}`, nil
	}
	start, err = provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatal(err)
	}
	poll, err = provider.PollOAuth(context.Background(), start.State)
	if err != nil {
		t.Fatalf("error poll: %v", err)
	}
	if poll.Status != "error" {
		t.Fatalf("error poll status = %q", poll.Status)
	}
	if strings.Contains(poll.Message, access) || strings.Contains(poll.Message, refresh) {
		t.Fatalf("error message leaked a token: %q", poll.Message)
	}
	if !strings.Contains(poll.Message, "<redacted>") {
		t.Fatalf("error message was not redacted: %q", poll.Message)
	}
}

func TestOAuthRejectsReservedUIDWithoutTouchingConfig(t *testing.T) {
	provider := newOAuthTestProvider(t, realmIntl)
	settingsPath := filepath.Join(provider.AccountsDir(), "settings.json")
	sentinel := []byte(`{"sentinel":"keep"}`)
	if err := os.WriteFile(settingsPath, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	token := makeJWT(t, map[string]any{"sub": "settings", "exp": 4102444800})
	fake := &oauthFake{handler: func(req *http.Request, proxy string) (int, string, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return 200, `{"code":0,"data":{"state":"st-reserved","authUrl":"https://browser.test/x"}}`, nil
		case "/v2/plugin/auth/token":
			return 200, `{"code":0,"data":{"accessToken":"` + token +
				`","refreshToken":"r","domain":"www.workbuddy.ai"}}`, nil
		default:
			return 200, `{"code":0,"data":{"nickname":"Evil"}}`, nil
		}
	}}
	provider.oauthClient = fake
	start, err := provider.BeginOAuth(context.Background(), "CLI", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.PollOAuth(context.Background(), start.State); err == nil {
		t.Fatal("OAuth accepted a reserved account uid")
	}
	got, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("OAuth overwrote settings.json: %s", got)
	}
	entries, err := os.ReadDir(provider.AccountsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "settings.json" {
		t.Fatalf("OAuth created unexpected account files: %+v", entries)
	}
}
