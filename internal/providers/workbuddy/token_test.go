package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type tokenTestCall struct {
	Method      string
	Path        string
	Body        string
	Header      http.Header
	ProxyTarget string
}

// tokenTestFake is an httptest token endpoint and, when an account proxy points
// at it, an HTTP proxy. It records every request so headers and absolute proxy
// targets can be checked.
type tokenTestFake struct {
	mu      sync.Mutex
	calls   []tokenTestCall
	status  int
	body    string
	respond func(*http.Request) (int, string)
	delay   time.Duration
}

func (f *tokenTestFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	call := tokenTestCall{
		Method: r.Method,
		Path:   r.URL.Path,
		Body:   string(raw),
		Header: r.Header.Clone(),
	}
	if r.URL.IsAbs() {
		call.ProxyTarget = r.URL.String()
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	status := f.status
	body := f.body
	respond := f.respond
	delay := f.delay
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if respond != nil {
		status, body = respond(r)
	}
	w.Header().Set("Content-Type", "application/json")
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 300 {
		w.WriteHeader(status)
	}
	if body == "" {
		body = `{"code":0,"data":{}}`
	}
	_, _ = io.WriteString(w, body)
}

func (f *tokenTestFake) snapshot() []tokenTestCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tokenTestCall(nil), f.calls...)
}

type tokenTestAccountSpec struct {
	UID    string
	Fields map[string]any
}

func newTokenTestProvider(
	t *testing.T,
	realm string,
	now time.Time,
	fake *tokenTestFake,
	accounts ...tokenTestAccountSpec,
) (*Provider, *Pool, string, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	writePoolSettings(t, dir, map[string]any{})
	for _, spec := range accounts {
		writePoolAccount(t, dir, spec.UID, realm, spec.Fields)
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	provider, pool := newPoolProvider(t, realm, dir)
	provider.tokenBaseURL = server.URL
	provider.tokenNow = func() time.Time { return now }
	return provider, pool, dir, server
}

func tokenSuccessBody(access, refresh string, expiresAt time.Time) string {
	return fmt.Sprintf(`{"code":0,"data":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d}}`,
		access, refresh, expiresAt.Unix())
}

func fixedTokenNow() time.Time {
	return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
}

func TestTokenEnsureRefreshesDueAndNearExpiry(t *testing.T) {
	now := fixedTokenNow()
	fake := &tokenTestFake{body: tokenSuccessBody("new-access", "new-refresh", now.Add(time.Hour))}
	provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
		tokenTestAccountSpec{UID: "expired", Fields: map[string]any{"expiresAt": now.Add(-time.Minute).Unix()}},
		tokenTestAccountSpec{UID: "near", Fields: map[string]any{"expiresAt": now.Add(60 * time.Second).Unix()}},
		tokenTestAccountSpec{UID: "boundary", Fields: map[string]any{"expiresAt": now.Add(120 * time.Second).Unix()}},
	)

	for _, uid := range []string{"expired", "near", "boundary"} {
		if err := provider.EnsureToken(context.Background(), pool.Get(uid)); err != nil {
			t.Fatalf("EnsureToken(%s): %v", uid, err)
		}
	}
	calls := fake.snapshot()
	if len(calls) != 3 {
		t.Fatalf("refresh calls = %d, want 3", len(calls))
	}
	for _, uid := range []string{"expired", "near", "boundary"} {
		account := pool.Get(uid)
		if account.Auth.AccessToken != "new-access" || account.Auth.RefreshToken != "new-refresh" {
			t.Fatalf("%s credentials were not rotated: %+v", uid, account.Auth)
		}
		if !account.Auth.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("%s expiry = %v, want %v", uid, account.Auth.ExpiresAt, now.Add(time.Hour))
		}
	}

	// A token far enough from expiry must not be refreshed again.
	if err := provider.EnsureToken(context.Background(), pool.Get("near")); err != nil {
		t.Fatalf("EnsureToken after rotation: %v", err)
	}
	if got := len(fake.snapshot()); got != 3 {
		t.Fatalf("valid token triggered another refresh: calls = %d", got)
	}
}

func TestTokenEnsureSkipsValidUnknownExpiryAndAPIKeys(t *testing.T) {
	now := fixedTokenNow()
	fake := &tokenTestFake{}
	provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
		tokenTestAccountSpec{UID: "valid", Fields: map[string]any{"expiresAt": now.Add(time.Hour).Unix()}},
		tokenTestAccountSpec{UID: "just-valid", Fields: map[string]any{"expiresAt": now.Add(121 * time.Second).Unix()}},
		tokenTestAccountSpec{UID: "unknown", Fields: map[string]any{
			"accessToken": "opaque-token",
			"expiresAt":   0,
		}},
	)
	for _, uid := range []string{"valid", "just-valid", "unknown"} {
		if err := provider.EnsureToken(context.Background(), pool.Get(uid)); err != nil {
			t.Fatalf("EnsureToken(%s): %v", uid, err)
		}
	}
	if calls := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("valid/unknown tokens triggered refresh: %+v", calls)
	}

	cnFake := &tokenTestFake{}
	cnProvider, cnPool, _, _ := newTokenTestProvider(t, realmCN, now, cnFake,
		tokenTestAccountSpec{UID: "api-key", Fields: map[string]any{
			"accessToken":  "ck_apikey.secret",
			"refreshToken": "",
			"apiKey":       true,
			"expiresAt":    0,
		}},
	)
	apiKey := cnPool.Get("api-key")
	if apiKey == nil || !apiKey.Auth.APIKey {
		t.Fatalf("API-key fixture did not load: %+v", apiKey)
	}
	if err := cnProvider.EnsureToken(context.Background(), apiKey); err != nil {
		t.Fatalf("EnsureToken(api-key): %v", err)
	}
	if calls := cnFake.snapshot(); len(calls) != 0 {
		t.Fatalf("API key triggered refresh: %+v", calls)
	}
}

func TestTokenEnsureConcurrentSingleFlight(t *testing.T) {
	now := fixedTokenNow()
	fake := &tokenTestFake{
		body:  tokenSuccessBody("new-access", "new-refresh", now.Add(time.Hour)),
		delay: 100 * time.Millisecond,
	}
	provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
		tokenTestAccountSpec{UID: "concurrent", Fields: map[string]any{
			"expiresAt": now.Add(-time.Minute).Unix(),
		}},
	)
	account := pool.Get("concurrent")

	const callers = 20
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- provider.EnsureToken(context.Background(), account)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureToken: %v", err)
		}
	}
	if calls := fake.snapshot(); len(calls) != 1 {
		t.Fatalf("concurrent refresh calls = %d, want 1", len(calls))
	}
	if account.Auth.RefreshToken != "new-refresh" {
		t.Fatalf("refresh token = %q, want new-refresh", account.Auth.RefreshToken)
	}
}

func TestTokenEnsureRotatesAndPersists(t *testing.T) {
	now := fixedTokenNow()
	newToken := makeJWT(t, map[string]any{
		"sub": "persist",
		"exp": now.Add(2 * time.Hour).Unix(),
	})
	fake := &tokenTestFake{
		body: fmt.Sprintf(`{"code":0,"data":{"accessToken":%q,"refreshToken":"rotated-refresh"}}`, newToken),
	}
	provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
		tokenTestAccountSpec{UID: "persist", Fields: map[string]any{
			"expiresAt":    now.Add(-time.Minute).Unix(),
			"futureField":  map[string]any{"keep": true},
			"tasksEnabled": true,
		}},
	)
	account := pool.Get("persist")
	if err := provider.EnsureToken(context.Background(), account); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if account.Auth.AccessToken != newToken || account.Auth.RefreshToken != "rotated-refresh" {
		t.Fatalf("in-memory credentials = %+v", account.Auth)
	}
	if !account.Auth.ExpiresAt.Equal(time.Unix(now.Add(2*time.Hour).Unix(), 0)) {
		t.Fatalf("JWT expiry fallback = %v, want %v", account.Auth.ExpiresAt, now.Add(2*time.Hour))
	}

	info, err := os.Stat(account.Path)
	if err != nil {
		t.Fatalf("stat account: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("account mode = %#o, want 0600", info.Mode().Perm())
	}
	reloaded, err := LoadAccount(account.Path)
	if err != nil {
		t.Fatalf("LoadAccount: %v", err)
	}
	if reloaded.AccessToken != newToken || reloaded.RefreshToken != "rotated-refresh" {
		t.Fatalf("persisted credentials = %+v", reloaded)
	}
	var extra map[string]any
	if err := json.Unmarshal(reloaded.Extra["futureField"], &extra); err != nil {
		t.Fatalf("decode preserved extra: %v", err)
	}
	if extra["keep"] != true {
		t.Fatalf("extra field was not preserved: %v", extra)
	}
	if !reloaded.TasksEnabled {
		t.Fatal("tasksEnabled was not preserved")
	}
	raw, err := os.ReadFile(account.Path)
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	for _, runtime := range []string{"lastError", "cooldownUntil"} {
		if strings.Contains(string(raw), `"`+runtime+`"`) {
			t.Fatalf("runtime-only %s persisted: %s", runtime, raw)
		}
	}
}

func TestTokenEnsureNestedPayloadAndExpiryPrecedence(t *testing.T) {
	now := fixedTokenNow()
	expires := now.Add(3 * time.Hour)
	fake := &tokenTestFake{
		body: fmt.Sprintf(`{"code":0,"data":{"data":{"accessToken":"nested-access","expiresAt":%d}}}`, expires.Unix()),
	}
	provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
		tokenTestAccountSpec{UID: "nested", Fields: map[string]any{
			"expiresAt": now.Add(-time.Minute).Unix(),
		}},
	)
	account := pool.Get("nested")
	if err := provider.EnsureToken(context.Background(), account); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if account.Auth.AccessToken != "nested-access" {
		t.Fatalf("nested accessToken = %q", account.Auth.AccessToken)
	}
	if account.Auth.RefreshToken != "refresh-nested" {
		t.Fatalf("missing rotated token should preserve old refresh token, got %q", account.Auth.RefreshToken)
	}
	if !account.Auth.ExpiresAt.Equal(expires) {
		t.Fatalf("expiresAt = %v, want %v", account.Auth.ExpiresAt, expires)
	}
}

func TestTokenEnsureTerminalFailures(t *testing.T) {
	now := fixedTokenNow()
	cases := []struct {
		name         string
		status       int
		body         string
		fields       map[string]any
		wantCalls    int
		manual       bool
		wantDisabled bool
	}{
		{
			name:         "401",
			status:       http.StatusUnauthorized,
			body:         `{"msg":"old-refresh is no longer valid"}`,
			wantCalls:    1,
			wantDisabled: true,
		},
		{
			name:         "403",
			status:       http.StatusForbidden,
			body:         `{"msg":"forbidden"}`,
			wantCalls:    1,
			wantDisabled: true,
		},
		{
			name:         "no-token",
			status:       http.StatusOK,
			body:         `{"code":0,"data":{}}`,
			wantCalls:    1,
			wantDisabled: true,
		},
		{
			name:         "no-refresh-token",
			fields:       map[string]any{"refreshToken": ""},
			wantCalls:    0,
			wantDisabled: true,
		},
		{
			name:         "manual-disabled-401",
			status:       http.StatusUnauthorized,
			body:         `{"msg":"old-refresh is no longer valid"}`,
			fields:       map[string]any{"enabled": false, "tasksEnabled": true},
			wantCalls:    1,
			manual:       true,
			wantDisabled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &tokenTestFake{status: tc.status, body: tc.body}
			fields := map[string]any{"expiresAt": now.Add(-time.Minute).Unix()}
			for key, value := range tc.fields {
				fields[key] = value
			}
			provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
				tokenTestAccountSpec{UID: "terminal", Fields: fields},
			)
			account := pool.Get("terminal")
			oldAccess, oldRefresh := account.Auth.AccessToken, account.Auth.RefreshToken
			err := provider.EnsureToken(context.Background(), account)
			if err == nil {
				t.Fatal("terminal refresh returned nil error")
			}
			if got := len(fake.snapshot()); got != tc.wantCalls {
				t.Fatalf("refresh calls = %d, want %d", got, tc.wantCalls)
			}
			if !account.viewAt(now).TokenExpired {
				t.Fatalf("terminal failure did not mark token expired: %+v", account.viewAt(now))
			}
			if account.State.LastError == "" {
				t.Fatal("terminal failure did not set last error")
			}
			if account.State.Disabled != tc.wantDisabled {
				t.Fatalf("State.Disabled = %v, want %v", account.State.Disabled, tc.wantDisabled)
			}
			if tc.manual {
				if !account.State.ManualDisabled || account.Auth.Enabled {
					t.Fatalf("manual disable was cleared: state=%+v auth=%+v", account.State, account.Auth)
				}
				if !account.Auth.TasksEnabled {
					t.Fatal("manual disable terminal failure cleared tasks")
				}
			}
			if account.Auth.AccessToken != oldAccess || account.Auth.RefreshToken != oldRefresh {
				t.Fatalf("terminal failure replaced credentials: %+v", account.Auth)
			}
			if tokenTextLeaks(err.Error(), oldAccess, oldRefresh) ||
				tokenTextLeaks(account.State.LastError, oldAccess, oldRefresh) {
				t.Fatalf("terminal failure leaked a credential: err=%q state=%q", err, account.State.LastError)
			}
		})
	}
}

func TestTokenEnsureTransientFailureKeepsValidToken(t *testing.T) {
	now := fixedTokenNow()
	for _, tc := range []struct {
		name        string
		expiry      time.Time
		wantErr     bool
		status      int
		networkFail bool
	}{
		{name: "valid-500", expiry: now.Add(time.Minute), status: http.StatusInternalServerError},
		{name: "expired-500", expiry: now.Add(-time.Minute), wantErr: true, status: http.StatusInternalServerError},
		{name: "valid-network", expiry: now.Add(time.Minute), networkFail: true},
		{name: "expired-network", expiry: now.Add(-time.Minute), wantErr: true, networkFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &tokenTestFake{
				status: tc.status,
				body:   `{"msg":"transient old-refresh old-access"}`,
			}
			provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
				tokenTestAccountSpec{UID: "transient", Fields: map[string]any{"expiresAt": tc.expiry.Unix()}},
			)
			account := pool.Get("transient")
			oldAccess, oldRefresh := account.Auth.AccessToken, account.Auth.RefreshToken
			if tc.networkFail {
				provider.tokenBaseURL = "http://127.0.0.1:1"
			}
			err := provider.EnsureToken(context.Background(), account)
			if tc.wantErr && err == nil {
				t.Fatal("expired token transient failure returned nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("valid token transient failure returned error: %v", err)
			}
			if account.Auth.AccessToken != oldAccess || account.Auth.RefreshToken != oldRefresh {
				t.Fatalf("transient failure replaced credentials: %+v", account.Auth)
			}
			if account.tokenExpired {
				t.Fatal("transient failure marked the token terminally expired")
			}
			if account.State.LastError == "" {
				t.Fatal("transient failure did not record the error")
			}
			if tokenTextLeaks(account.State.LastError, oldAccess, oldRefresh) {
				t.Fatalf("transient failure leaked a credential: %q", account.State.LastError)
			}
			if err != nil && tokenTextLeaks(err.Error(), oldAccess, oldRefresh) {
				t.Fatalf("transient error leaked a credential: %v", err)
			}
		})
	}
}

func TestTokenEnsureManualDisableSurvivesSuccess(t *testing.T) {
	now := fixedTokenNow()
	fake := &tokenTestFake{body: tokenSuccessBody("new-access", "new-refresh", now.Add(time.Hour))}
	provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
		tokenTestAccountSpec{UID: "manual", Fields: map[string]any{
			"enabled":      false,
			"tasksEnabled": true,
			"expiresAt":    now.Add(-time.Minute).Unix(),
		}},
	)
	account := pool.Get("manual")
	account.State.LastError = "previous manual disable"
	account.State.CoolUntil = now.Add(time.Hour)

	if err := provider.EnsureToken(context.Background(), account); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if account.Auth.Enabled || !account.State.ManualDisabled || !account.State.Disabled {
		t.Fatalf("manual disable was cleared: auth=%+v state=%+v", account.Auth, account.State)
	}
	if !account.Auth.TasksEnabled {
		t.Fatal("task participation was cleared")
	}
	if account.Auth.AccessToken != "new-access" || account.Auth.RefreshToken != "new-refresh" {
		t.Fatalf("credentials were not rotated: %+v", account.Auth)
	}
	if account.State.LastError != "" || !account.State.CoolUntil.IsZero() {
		t.Fatalf("success did not clear runtime errors/cooldown: %+v", account.State)
	}
	reloaded, err := LoadAccount(account.Path)
	if err != nil {
		t.Fatalf("LoadAccount: %v", err)
	}
	if reloaded.Enabled || !reloaded.TasksEnabled ||
		reloaded.AccessToken != "new-access" || reloaded.RefreshToken != "new-refresh" {
		t.Fatalf("persisted manual state = %+v", reloaded)
	}
}

func TestTokenEnsureSuccessClearsAutomaticFailure(t *testing.T) {
	now := fixedTokenNow()
	fake := &tokenTestFake{body: tokenSuccessBody("new-access", "new-refresh", now.Add(time.Hour))}
	provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
		tokenTestAccountSpec{UID: "recover", Fields: map[string]any{
			"tasksEnabled": true,
			"expiresAt":    now.Add(-time.Minute).Unix(),
		}},
	)
	account := pool.Get("recover")
	account.SetTokenExpired(true)
	account.mu.Lock()
	account.State.LastError = "automatically disabled"
	account.State.Reason = "automatically disabled"
	account.State.ErrCount = 4
	account.State.CoolUntil = now.Add(time.Hour)
	account.State.Disabled = true
	account.mu.Unlock()

	if err := provider.EnsureToken(context.Background(), account); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if account.tokenExpired || account.State.Disabled || account.State.LastError != "" ||
		account.State.Reason != "" || account.State.ErrCount != 0 || !account.State.CoolUntil.IsZero() {
		t.Fatalf("success did not clear automatic failure state: %+v", account.State)
	}
	if !account.Auth.Enabled || !account.Auth.TasksEnabled {
		t.Fatalf("success changed operator flags: %+v", account.Auth)
	}
}

func TestTokenEnsureRefreshOnlyAccountRecovers(t *testing.T) {
	now := fixedTokenNow()
	fake := &tokenTestFake{body: tokenSuccessBody("recovered-access", "recovered-refresh", now.Add(time.Hour))}
	provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
		tokenTestAccountSpec{UID: "refresh-only", Fields: map[string]any{
			"accessToken": "",
			"expiresAt":   0,
		}},
	)
	account := pool.Get("refresh-only")
	if account.Auth.AccessToken != "" || account.Auth.RefreshToken == "" {
		t.Fatalf("refresh-only fixture = %+v", account.Auth)
	}
	if err := provider.EnsureToken(context.Background(), account); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if account.Auth.AccessToken != "recovered-access" || account.Auth.RefreshToken != "recovered-refresh" {
		t.Fatalf("refresh-only recovery = %+v", account.Auth)
	}
	if view := account.viewAt(now); view.TokenExpired || view.NeedRelogin {
		t.Fatalf("recovered account still needs login: %+v", view)
	}
	reloaded, err := LoadAccount(account.Path)
	if err != nil {
		t.Fatalf("LoadAccount: %v", err)
	}
	if reloaded.AccessToken != "recovered-access" || reloaded.RefreshToken != "recovered-refresh" {
		t.Fatalf("persisted recovery = %+v", reloaded)
	}
}

func TestTokenEnsureProxyHeadersAndTimeout(t *testing.T) {
	now := fixedTokenNow()
	for _, tc := range []struct {
		name           string
		realm          string
		wantOrigin     string
		wantSource     string
		wantLanguage   string
		wantDomain     string
		wantPathSuffix string
	}{
		{
			name:           "intl",
			realm:          realmIntl,
			wantOrigin:     "https://www.workbuddy.ai",
			wantSource:     "plugin",
			wantLanguage:   "en-US",
			wantDomain:     "custom.example",
			wantPathSuffix: "/v2/plugin/auth/token/refresh",
		},
		{
			name:           "cn",
			realm:          realmCN,
			wantOrigin:     "https://www.codebuddy.cn",
			wantSource:     "workbuddy",
			wantLanguage:   "zh-CN",
			wantDomain:     "custom.example",
			wantPathSuffix: "/v2/plugin/auth/token/refresh",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &tokenTestFake{body: tokenSuccessBody("proxied-access", "proxied-refresh", now.Add(time.Hour))}
			provider, pool, _, server := newTokenTestProvider(t, tc.realm, now, fake,
				tokenTestAccountSpec{UID: "proxied", Fields: map[string]any{
					"domain":       tc.wantDomain,
					"enterpriseId": "ent-1",
					"expiresAt":    now.Add(-time.Minute).Unix(),
				}},
			)
			account := pool.Get("proxied")
			account.Auth.ProxyURL = server.URL
			provider.tokenBaseURL = "http://upstream.test"
			if err := provider.EnsureToken(context.Background(), account); err != nil {
				t.Fatalf("EnsureToken: %v", err)
			}
			calls := fake.snapshot()
			if len(calls) != 1 {
				t.Fatalf("refresh calls = %d, want 1", len(calls))
			}
			call := calls[0]
			if call.Method != http.MethodPost || call.Path != tc.wantPathSuffix {
				t.Fatalf("request = %s %s", call.Method, call.Path)
			}
			if call.ProxyTarget != "http://upstream.test"+tc.wantPathSuffix {
				t.Fatalf("proxy target = %q", call.ProxyTarget)
			}
			if call.Body != "{}" {
				t.Fatalf("body = %q, want {}", call.Body)
			}
			wantHeaders := map[string]string{
				"Content-Type":          "application/json",
				"Accept":                "application/json, text/plain, */*",
				"X-Requested-With":      "XMLHttpRequest",
				"User-Agent":            "WorkBuddy/" + provider.DesktopVersion(),
				"Origin":                tc.wantOrigin,
				"Referer":               tc.wantOrigin + "/",
				"X-Refresh-Token":       "refresh-proxied",
				"X-Auth-Refresh-Source": tc.wantSource,
				"X-User-Id":             "proxied",
				"X-Domain":              tc.wantDomain,
				"X-CodeBuddy-Request":   "1",
				"Accept-Language":       tc.wantLanguage,
				"X-Enterprise-Id":       "ent-1",
			}
			for key, want := range wantHeaders {
				if got := call.Header.Get(key); got != want {
					t.Fatalf("%s = %q, want %q", key, got, want)
				}
			}
			if call.Header.Get("Authorization") != "" {
				t.Fatalf("refresh request carried Authorization: %q", call.Header.Get("Authorization"))
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		fake := &tokenTestFake{
			body:  tokenSuccessBody("late-access", "late-refresh", now.Add(time.Hour)),
			delay: 250 * time.Millisecond,
		}
		provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
			tokenTestAccountSpec{UID: "timeout", Fields: map[string]any{
				"expiresAt": now.Add(-time.Minute).Unix(),
			}},
		)
		provider.tokenTimeout = 25 * time.Millisecond
		start := time.Now()
		err := provider.EnsureToken(context.Background(), pool.Get("timeout"))
		if err == nil {
			t.Fatal("timed-out refresh returned nil")
		}
		if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
			t.Fatalf("refresh took %v; timeout was not enforced", elapsed)
		}
	})
}

// tokenTextLeaks reports whether text contains any non-empty secret. Empty
// credentials are ignored because strings.Contains matches everything.
func tokenTextLeaks(text string, secrets ...string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			return true
		}
	}
	return false
}

func TestTokenKeepaliveCountsSkipsAndNoCheckin(t *testing.T) {
	now := fixedTokenNow()
	fake := &tokenTestFake{body: tokenSuccessBody("keepalive-access", "keepalive-refresh", now.Add(time.Hour))}
	provider, pool, _, _ := newTokenTestProvider(t, realmCN, now, fake,
		tokenTestAccountSpec{UID: "valid", Fields: map[string]any{"expiresAt": now.Add(time.Hour).Unix()}},
		tokenTestAccountSpec{UID: "due", Fields: map[string]any{"expiresAt": now.Add(time.Minute).Unix()}},
		tokenTestAccountSpec{UID: "disabled", Fields: map[string]any{
			"enabled":   false,
			"expiresAt": now.Add(time.Minute).Unix(),
		}},
		tokenTestAccountSpec{UID: "api-key", Fields: map[string]any{
			"accessToken":  "ck_keepalive.secret",
			"refreshToken": "",
			"apiKey":       true,
			"expiresAt":    0,
		}},
	)

	result, err := provider.Keepalive(context.Background())
	if err != nil {
		t.Fatalf("Keepalive: %v", err)
	}
	if result.Accounts != 4 || result.Refreshed != 1 || result.Skipped != 3 || result.Failed != 0 {
		t.Fatalf("KeepaliveResult = %+v, want accounts=4 refreshed=1 skipped=3 failed=0", result)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("Keepalive errors = %v", result.Errors)
	}
	calls := fake.snapshot()
	if len(calls) != 1 {
		t.Fatalf("keepalive refresh calls = %d, want 1", len(calls))
	}
	for _, call := range calls {
		if strings.Contains(strings.ToLower(call.Path), "checkin") {
			t.Fatalf("keepalive touched a check-in endpoint: %s", call.Path)
		}
	}
	if pool.Get("due").Auth.AccessToken != "keepalive-access" {
		t.Fatalf("due account was not refreshed: %+v", pool.Get("due").Auth)
	}
	if pool.Get("disabled").Auth.AccessToken == "keepalive-access" {
		t.Fatal("disabled account was refreshed")
	}
}

func TestTokenKeepaliveIsolatesAndRedactsErrors(t *testing.T) {
	now := fixedTokenNow()
	fake := &tokenTestFake{respond: func(r *http.Request) (int, string) {
		if r.Header.Get("X-Refresh-Token") == "refresh-good" {
			return http.StatusOK, tokenSuccessBody("good-access", "good-refresh", now.Add(time.Hour))
		}
		return http.StatusUnauthorized, `{"msg":"refresh-bad and refresh-bad access-bad leaked"}`
	}}
	provider, pool, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
		tokenTestAccountSpec{UID: "bad", Fields: map[string]any{"expiresAt": now.Add(-time.Minute).Unix()}},
		tokenTestAccountSpec{UID: "good", Fields: map[string]any{"expiresAt": now.Add(-time.Minute).Unix()}},
	)

	result, err := provider.Keepalive(context.Background())
	if err == nil {
		t.Fatal("Keepalive with one terminal account returned nil error")
	}
	if result.Accounts != 2 || result.Refreshed != 1 || result.Failed != 1 || result.Skipped != 0 {
		t.Fatalf("KeepaliveResult = %+v", result)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Keepalive errors = %v, want one", result.Errors)
	}
	for _, text := range append(append([]string(nil), result.Errors...), err.Error()) {
		for _, secret := range []string{"refresh-good", "good-access", "refresh-bad", "access-bad"} {
			if strings.Contains(text, secret) {
				t.Fatalf("keepalive error leaked %q: %s", secret, text)
			}
		}
	}
	if pool.Get("good").Auth.RefreshToken != "good-refresh" {
		t.Fatalf("good account was not refreshed: %+v", pool.Get("good").Auth)
	}
	bad := pool.Get("bad")
	if !bad.viewAt(now).TokenExpired || bad.State.LastError == "" {
		t.Fatalf("bad account terminal state missing: %+v", bad.viewAt(now))
	}
	if len(fake.snapshot()) != 2 {
		t.Fatalf("keepalive stopped after the first failure: calls = %d", len(fake.snapshot()))
	}
}

func TestTokenKeepaliveCanceledContext(t *testing.T) {
	now := fixedTokenNow()
	fake := &tokenTestFake{body: tokenSuccessBody("new-access", "new-refresh", now.Add(time.Hour))}
	provider, _, _, _ := newTokenTestProvider(t, realmIntl, now, fake,
		tokenTestAccountSpec{UID: "due", Fields: map[string]any{"expiresAt": now.Add(-time.Minute).Unix()}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := provider.Keepalive(ctx)
	if err == nil {
		t.Fatal("canceled Keepalive returned nil error")
	}
	if len(fake.snapshot()) != 0 {
		t.Fatalf("canceled keepalive made %d request(s)", len(fake.snapshot()))
	}
	if result.Refreshed != 0 {
		t.Fatalf("canceled keepalive refreshed %d", result.Refreshed)
	}
}

func TestTokenEnsureRequiresContextAndAccount(t *testing.T) {
	provider := &Provider{realm: realmIntl}
	if err := provider.EnsureToken(nil, &Account{}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if err := provider.EnsureToken(context.Background(), nil); err == nil {
		t.Fatal("nil account was accepted")
	}
	var nilProvider *Provider
	if err := nilProvider.EnsureToken(context.Background(), &Account{}); err == nil {
		t.Fatal("nil provider was accepted")
	}
}
