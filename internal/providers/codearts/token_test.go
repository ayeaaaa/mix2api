package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

// tokenRefreshFake is an httptest token endpoint/proxy. It records every form,
// DPoP proof, and (when used as an HTTP proxy) absolute request target.
type tokenRefreshFake struct {
	mu sync.Mutex

	calls       int
	forms       []url.Values
	proofs      []DPoPProofInfo
	proofErrs   []error
	proxyTarget []string

	delay   time.Duration
	status  int
	body    string
	started chan struct{}
	release chan struct{}
	// payload builds a successful response. A nil payload uses
	// defaultTokenTestPayload.
	payload func(form url.Values) map[string]any
}

func (f *tokenRefreshFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(raw))
	info, proofErr := VerifyDPoPProof(r.Header.Get("DPoP"))

	f.mu.Lock()
	f.calls++
	f.forms = append(f.forms, form)
	f.proofs = append(f.proofs, info)
	f.proofErrs = append(f.proofErrs, proofErr)
	if r.URL.IsAbs() {
		f.proxyTarget = append(f.proxyTarget, r.URL.String())
	}
	status := f.status
	body := f.body
	delay := f.delay
	payload := f.payload
	started := f.started
	release := f.release
	f.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		return
	}
	if payload == nil {
		payload = defaultTokenTestPayload
	}
	_ = json.NewEncoder(w).Encode(payload(form))
}

func defaultTokenTestPayload(form url.Values) map[string]any {
	return tokenTestPayload("u1", form.Get("refresh_token")+"-new",
		time.Now().Add(time.Hour))
}

func tokenTestPayload(uid, refresh string, expiration time.Time) map[string]any {
	return map[string]any{
		"user_id":       uid,
		"user_name":     "Renewed " + uid,
		"domain_id":     "domain-new-" + uid,
		"refresh_token": refresh,
		"credentials": map[string]any{
			"access_key_id":     "AK-new-" + uid,
			"secret_access_key": "SK-new-" + uid,
			"security_token":    "ST-new-" + uid,
			"expiration":        expiration.UTC().Format(time.RFC3339),
		},
	}
}

func newTokenRefreshFake(t *testing.T) (*tokenRefreshFake, *httptest.Server) {
	t.Helper()
	fake := &tokenRefreshFake{}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	t.Setenv("WB_CODEARTS_STS_HOST", server.URL)
	return fake, server
}

func (f *tokenRefreshFake) snapshot() (int, []url.Values, []DPoPProofInfo, []error, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls,
		append([]url.Values(nil), f.forms...),
		append([]DPoPProofInfo(nil), f.proofs...),
		append([]error(nil), f.proofErrs...),
		append([]string(nil), f.proxyTarget...)
}

func (f *tokenRefreshFake) reset() {
	f.mu.Lock()
	f.calls = 0
	f.forms = nil
	f.proofs = nil
	f.proofErrs = nil
	f.proxyTarget = nil
	f.mu.Unlock()
}

// blockTokenRefreshFake parks the token endpoint after recording the request.
// The returned release function is safe to call more than once.
func blockTokenRefreshFake(fake *tokenRefreshFake) (<-chan struct{}, func()) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fake.mu.Lock()
	fake.started = started
	fake.release = release
	fake.mu.Unlock()
	var once sync.Once
	return started, func() { once.Do(func() { close(release) }) }
}

func waitTokenRefreshStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("token refresh did not reach the fake upstream")
	}
}

func waitTokenRefreshJoiners(t *testing.T, joined <-chan struct{}, want int) {
	t.Helper()
	for i := 0; i < want; i++ {
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d callers joined the refresh flight", i, want)
		}
	}
}

func tokenTestAuth(uid string, expiration time.Time) Auth {
	return Auth{
		UserID:           uid,
		UserName:         "User " + uid,
		DomainID:         "domain-" + uid,
		CloudDragonToken: "cloud-old-" + uid,
		AccessKeyID:      "AK-old-" + uid,
		SecretAccessKey:  "SK-old-" + uid,
		RefreshToken:     "refresh-" + uid + "-old",
		CodeVerifier:     "verifier-" + uid,
		ClientID:         "client-" + uid,
		Expiration:       expiration,
		DPopPrivateKey:   json.RawMessage(fixedDPoPJWK),
		UpdatedAt:        1,
		extraFields: map[string]json.RawMessage{
			"future_field": json.RawMessage(`{"keep":true}`),
		},
	}
}

func tokenTestPool(t *testing.T, auths ...Auth) (*Pool, string) {
	t.Helper()
	dir := t.TempDir()
	for _, auth := range auths {
		path, err := AuthPath(dir, auth.UserID)
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveAuth(path, auth); err != nil {
			t.Fatalf("SaveAuth(%s): %v", auth.UserID, err)
		}
	}
	pool, err := NewPool(dir, Config{MaxConcurrent: 1, TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool, dir
}

func newTokenTestManager(t *testing.T, now time.Time) *TokenManager {
	t.Helper()
	return newTokenTestManagerTimeout(t, now, 5*time.Second)
}

func newTokenTestManagerTimeout(t *testing.T, now time.Time, timeout time.Duration) *TokenManager {
	t.Helper()
	client := transport.NewClient(transport.ClientOptions{})
	manager := NewTokenManager(client, TokenManagerOptions{
		Timeout:  timeout,
		Now:      func() time.Time { return now },
		ProxyURL: func(a *Account) string { return a.ProxyURL() },
	})
	if manager == nil {
		t.Fatal("NewTokenManager returned nil")
	}
	return manager
}

func TestTokenEnsureNoopForValidAndZeroExpiration(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)

	valid := tokenTestAuth("valid", now.Add(2*time.Hour))
	noExpiry := tokenTestAuth("no-expiry", time.Time{})
	pool, _ := tokenTestPool(t, valid, noExpiry)

	for _, uid := range []string{"valid", "no-expiry"} {
		account := pool.Get(uid)
		if account == nil {
			t.Fatalf("account %q not loaded", uid)
		}
		if err := manager.Ensure(context.Background(), account); err != nil {
			t.Fatalf("Ensure(%s) = %v", uid, err)
		}
	}

	calls, _, _, _, _ := fake.snapshot()
	if calls != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", calls)
	}
}

func TestTokenEnsureRefreshBoundary(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		expiration time.Time
		wantCalls  int
	}{
		{"well before skew", now.Add(30*time.Minute + time.Second), 0},
		{"exactly at skew", now.Add(30 * time.Minute), 1},
		{"inside skew", now.Add(29 * time.Minute), 1},
		{"expired", now.Add(-time.Minute), 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, _ := newTokenRefreshFake(t)
			manager := newTokenTestManager(t, now)
			auth := tokenTestAuth("u1", tc.expiration)
			pool, _ := tokenTestPool(t, auth)
			account := pool.Get("u1")
			fake.payload = func(form url.Values) map[string]any {
				return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
			}

			if err := manager.Ensure(context.Background(), account); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			calls, _, _, _, _ := fake.snapshot()
			if calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantCalls == 1 && account.Auth.RefreshToken != "refresh-u1-new" {
				t.Fatalf("refresh token = %q, want rotated token", account.Auth.RefreshToken)
			}
			if tc.wantCalls == 0 && account.Auth.RefreshToken != "refresh-u1-old" {
				t.Fatalf("refresh token = %q, want untouched token", account.Auth.RefreshToken)
			}
		})
	}
}

func TestTokenEnsureRefreshesAndPersistsRotation(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(10*time.Minute))
	pool, dir := tokenTestPool(t, auth)
	account := pool.Get("u1")
	wantExpiration := now.Add(45 * time.Minute)
	fake.payload = func(form url.Values) map[string]any {
		return map[string]any{
			"user_id":       "u1",
			"user_name":     "Renewed Name",
			"domain_id":     "domain-renewed",
			"refresh_token": "refresh-u1-new",
			"credentials": map[string]any{
				"access_key_id":     "AK-new",
				"secret_access_key": "SK-new",
				"security_token":    "ST-new",
				"expiration":        wantExpiration.UTC().Format(time.RFC3339),
			},
		}
	}

	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	calls, forms, proofs, proofErrs, _ := fake.snapshot()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if len(proofErrs) != 1 || proofErrs[0] != nil {
		t.Fatalf("DPoP proof error = %v", proofErrs)
	}
	form := forms[0]
	if form.Get("grant_type") != "refresh_token" ||
		form.Get("refresh_token") != "refresh-u1-old" ||
		form.Get("client_id") != "client-u1" ||
		form.Get("code_verifier") != "verifier-u1" {
		t.Fatalf("refresh form = %v", form)
	}
	if len(form) != 4 {
		t.Fatalf("refresh form has extra keys: %v", form)
	}
	if proofs[0].Htm != http.MethodPost || !strings.HasSuffix(proofs[0].Htu, "/v1/oauth2/tokens") {
		t.Fatalf("DPoP proof = %+v", proofs[0])
	}

	reloaded, err := LoadAuth(account.Path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	for _, got := range []Auth{account.Auth, reloaded} {
		if got.UserName != "Renewed Name" || got.DomainID != "domain-renewed" {
			t.Fatalf("identity not updated: %+v", got)
		}
		if got.CloudDragonToken != "ST-new" || got.AccessKeyID != "AK-new" ||
			got.SecretAccessKey != "SK-new" {
			t.Fatalf("credentials not updated: %+v", got)
		}
		if !got.Expiration.Equal(wantExpiration) {
			t.Fatalf("expiration = %v, want %v", got.Expiration, wantExpiration)
		}
		if got.RefreshToken != "refresh-u1-new" {
			t.Fatalf("refresh token = %q", got.RefreshToken)
		}
		if got.CodeVerifier != "verifier-u1" || got.ClientID != "client-u1" {
			t.Fatalf("PKCE/client identity not preserved: %+v", got)
		}
		if !sameDPoPKey(got.DPopPrivateKey, json.RawMessage(fixedDPoPJWK)) {
			t.Fatalf("DPoP key changed: %s", got.DPopPrivateKey)
		}
		if got.UpdatedAt != now.Unix() {
			t.Fatalf("updated_at = %d, want %d", got.UpdatedAt, now.Unix())
		}
		if !sameDPoPKey(got.extraFields["future_field"], json.RawMessage(`{"keep":true}`)) {
			t.Fatalf("unknown field = %s", got.extraFields["future_field"])
		}
	}

	path, _ := AuthPath(dir, "u1")
	if account.Path != path {
		t.Fatalf("account path = %q, want %q", account.Path, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestTokenEnsureKeepsEstablishedUserID(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	originalPath := account.Path
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u2", "refresh-u1-new", now.Add(time.Hour))
	}

	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if account.Auth.UserID != "u1" {
		t.Fatalf("user_id changed to %q, want established u1", account.Auth.UserID)
	}
	if account.Path != originalPath {
		t.Fatalf("path changed to %q, want %q", account.Path, originalPath)
	}
	reloaded, err := LoadAuth(originalPath)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if reloaded.UserID != "u1" {
		t.Fatalf("persisted user_id = %q, want u1", reloaded.UserID)
	}
	if pool.Get("u1") != account {
		t.Fatal("pool lost the established u1 account")
	}
	if pool.Get("u2") != nil {
		t.Fatal("refresh response created a u2 account")
	}
}

func TestTokenAuthGenerationAdvancesOnCredentialReplacementNotProxySlot(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	auth := tokenTestAuth("u1", now.Add(2*time.Hour))
	pool, _ := tokenTestPool(t, auth)
	pool.ApplyProxySlots(transport.Slots{
		{ID: "slot-1", URL: "http://proxy.example.test:8080", Enabled: true},
	})
	account := pool.Get("u1")
	generation := account.tokenSnapshot().generation

	if err := pool.SetProxySlot("u1", "slot-1"); err != nil {
		t.Fatalf("SetProxySlot: %v", err)
	}
	if got := account.tokenSnapshot().generation; got != generation {
		t.Fatalf("proxy-only save advanced generation from %d to %d", generation, got)
	}

	replacement := tokenTestAuth("u1", now.Add(2*time.Hour))
	replacement.RefreshToken = "refresh-u1-reimport"
	replacement.CloudDragonToken = "cloud-reimport"
	if _, err := pool.Add(replacement); err != nil {
		t.Fatalf("Add re-import: %v", err)
	}
	if got := account.tokenSnapshot().generation; got <= generation {
		t.Fatalf("re-import generation = %d, want > %d", got, generation)
	}
	if account.Auth.RefreshToken != "refresh-u1-reimport" {
		t.Fatalf("re-import credentials = %+v", account.Auth)
	}
}

func TestTokenStillCurrentTracksGenerationNotProxySlot(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	auth := tokenTestAuth("u1", now.Add(2*time.Hour))
	pool, _ := tokenTestPool(t, auth)
	pool.ApplyProxySlots(transport.Slots{
		{ID: "slot-1", URL: "http://proxy.example.test:8080", Enabled: true},
	})
	account := pool.Get("u1")
	manager := newTokenTestManager(t, now)
	snapshot := account.tokenSnapshot()

	if err := pool.SetProxySlot("u1", "slot-1"); err != nil {
		t.Fatal(err)
	}
	if !manager.stillCurrent(account, snapshot) {
		t.Fatal("proxy-only save made the credential snapshot stale")
	}

	replacement := tokenTestAuth("u1", now.Add(2*time.Hour))
	replacement.RefreshToken = "refresh-u1-reimport"
	if _, err := pool.Add(replacement); err != nil {
		t.Fatal(err)
	}
	if manager.stillCurrent(account, snapshot) {
		t.Fatal("credential re-import did not invalidate the snapshot")
	}
}

func TestTokenEnsureConcurrentSetProxySlotMergesRefresh(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, auth)
	const proxyURL = "http://proxy.example.test:8080"
	pool.ApplyProxySlots(transport.Slots{
		{ID: "slot-1", URL: proxyURL, Enabled: true},
	})
	account := pool.Get("u1")
	generation := account.tokenSnapshot().generation
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
	}
	started, release := blockTokenRefreshFake(fake)

	errs := make(chan error, 1)
	go func() { errs <- manager.Ensure(context.Background(), account) }()
	waitTokenRefreshStarted(t, started)
	if err := pool.SetProxySlot("u1", "slot-1"); err != nil {
		t.Fatal(err)
	}
	release()

	if err := <-errs; err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if account.Auth.RefreshToken != "refresh-u1-new" {
		t.Fatalf("refresh was discarded after proxy-only save: %+v", account.Auth)
	}
	if account.Auth.ProxySlot != "slot-1" {
		t.Fatalf("proxySlot = %q, want slot-1", account.Auth.ProxySlot)
	}
	if account.ProxyURL() != proxyURL {
		t.Fatalf("resolved proxy = %q, want %q", account.ProxyURL(), proxyURL)
	}
	reloaded, err := LoadAuth(account.Path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if reloaded.RefreshToken != "refresh-u1-new" || reloaded.ProxySlot != "slot-1" {
		t.Fatalf("persisted auth = %+v", reloaded)
	}
	if !sameDPoPKey(account.Auth.DPopPrivateKey, json.RawMessage(fixedDPoPJWK)) {
		t.Fatalf("DPoP key changed: %s", account.Auth.DPopPrivateKey)
	}
	if got := account.tokenSnapshot().generation; got <= generation {
		t.Fatalf("refresh generation = %d, want > %d", got, generation)
	}
}

func TestTokenEnsureConcurrentReimportWins(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
	}
	started, release := blockTokenRefreshFake(fake)

	errs := make(chan error, 1)
	go func() { errs <- manager.Ensure(context.Background(), account) }()
	waitTokenRefreshStarted(t, started)
	replacement := tokenTestAuth("u1", now.Add(2*time.Hour))
	replacement.RefreshToken = "refresh-u1-reimport"
	replacement.CloudDragonToken = "cloud-reimport"
	if _, err := pool.Add(replacement); err != nil {
		t.Fatalf("Add re-import: %v", err)
	}
	release()

	if err := <-errs; err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if account.Auth.RefreshToken != "refresh-u1-reimport" ||
		account.Auth.CloudDragonToken != "cloud-reimport" {
		t.Fatalf("in-flight refresh overwrote re-imported credentials: %+v", account.Auth)
	}
	reloaded, err := LoadAuth(account.Path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if reloaded.RefreshToken != "refresh-u1-reimport" {
		t.Fatalf("persisted credentials = %+v", reloaded)
	}
	if calls, _, _, _, _ := fake.snapshot(); calls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", calls)
	}
}

func TestTokenEnsureIdenticalReimportKeepsRotatedToken(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	base := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, base)
	account := pool.Get("u1")
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
	}
	started, release := blockTokenRefreshFake(fake)

	errs := make(chan error, 1)
	go func() { errs <- manager.Ensure(context.Background(), account) }()
	waitTokenRefreshStarted(t, started)

	// Same credential material, different metadata: this is the identical
	// re-import that used to bump authGeneration and discard the rotation.
	identical := base
	identical.UpdatedAt = base.UpdatedAt + 99
	identical.extraFields = map[string]json.RawMessage{
		"future_field": json.RawMessage(`{"keep":true,"reimport":true}`),
		"new_field":    json.RawMessage(`42`),
	}
	if _, err := pool.Add(identical); err != nil {
		t.Fatalf("identical re-import: %v", err)
	}
	reimportGeneration := account.tokenSnapshot().generation
	release()

	if err := <-errs; err != nil {
		t.Fatalf("Ensure after identical re-import: %v", err)
	}
	if calls, _, _, _, _ := fake.snapshot(); calls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", calls)
	}
	if account.Auth.RefreshToken != "refresh-u1-new" {
		t.Fatalf("rotated token was discarded: %q", account.Auth.RefreshToken)
	}
	reloaded, err := LoadAuth(account.Path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if reloaded.RefreshToken != "refresh-u1-new" {
		t.Fatalf("persisted token = %q, want refresh-u1-new", reloaded.RefreshToken)
	}
	if reloaded.UpdatedAt != now.Unix() {
		t.Fatalf("updated_at = %d, want refresh timestamp %d", reloaded.UpdatedAt, now.Unix())
	}
	var future map[string]any
	if err := json.Unmarshal(reloaded.extraFields["future_field"], &future); err != nil {
		t.Fatalf("decode merged extra: %v", err)
	}
	if future["reimport"] != true || future["keep"] != true {
		t.Fatalf("latest extras were not merged: %v", future)
	}
	if !bytes.Equal(bytes.TrimSpace(reloaded.extraFields["new_field"]), []byte(`42`)) {
		t.Fatalf("new extra was not merged: %s", reloaded.extraFields["new_field"])
	}
	if got := account.tokenSnapshot().generation; got <= reimportGeneration {
		t.Fatalf("generation = %d, want > re-import generation %d", got, reimportGeneration)
	}

	// The account must remain usable for the next rotation.
	account.mu.Lock()
	account.Auth.Expiration = now.Add(-time.Minute)
	account.mu.Unlock()
	fake.mu.Lock()
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-second", now.Add(time.Hour))
	}
	fake.mu.Unlock()
	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("next refresh: %v", err)
	}
	if account.Auth.RefreshToken != "refresh-u1-second" {
		t.Fatalf("next refresh token = %q", account.Auth.RefreshToken)
	}
}

func TestTokenEnsureConcurrentSingleFlightFailure(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	fake.status = http.StatusInternalServerError
	fake.body = `{"error":"upstream","error_msg":"refresh-u1-old"}`
	started, release := blockTokenRefreshFake(fake)

	const callers = 20
	joined := make(chan struct{}, callers-1)
	manager.flightJoinHook = func() { joined <- struct{}{} }
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- manager.Ensure(context.Background(), account)
		}()
	}
	close(start)
	waitTokenRefreshStarted(t, started)
	waitTokenRefreshJoiners(t, joined, callers-1)
	release()
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil {
			t.Fatal("failed shared flight returned nil")
		}
	}
	if calls, _, _, _, _ := fake.snapshot(); calls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", calls)
	}
}

func TestTokenEnsureTimeoutReturnsTransportError(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManagerTimeout(t, now, 25*time.Millisecond)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	fake.delay = 200 * time.Millisecond

	start := time.Now()
	err := manager.Ensure(context.Background(), account)
	if err == nil {
		t.Fatal("Ensure returned nil, want timeout error")
	}
	if !errors.Is(err, ErrTokenTransport) {
		t.Fatalf("error = %v, want ErrTokenTransport", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("refresh took %v, timeout was not enforced", elapsed)
	}
}

func breakTokenStatePersistence(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "codearts", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("replace state file with directory: %v", err)
	}
}

func TestTokenEnsureTerminalFailureReportsStatePersistenceError(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, dir := tokenTestPool(t, auth)
	account := pool.Get("u1")
	breakTokenStatePersistence(t, dir)
	fake.status = http.StatusUnauthorized
	fake.body = `{"error":"invalid_grant","error_msg":"refresh-u1-old has been used"}`

	err := manager.Ensure(context.Background(), account)
	if err == nil {
		t.Fatal("Ensure returned nil, want joined state persistence error")
	}
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("error = %v, want ErrTokenRejected", err)
	}
	if !strings.Contains(err.Error(), "state") {
		t.Fatalf("error did not report state persistence failure: %v", err)
	}
	if !account.State.Disabled || account.State.ManualDisabled {
		t.Fatalf("state = %+v, want in-memory terminal disable", account.State)
	}
}

func TestTokenEnsureSuccessReportsStatePersistenceError(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, dir := tokenTestPool(t, auth)
	account := pool.Get("u1")
	breakTokenStatePersistence(t, dir)
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
	}

	err := manager.Ensure(context.Background(), account)
	if err == nil {
		t.Fatal("Ensure returned nil, want state persistence error")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Fatalf("error did not report state persistence failure: %v", err)
	}
	if account.Auth.RefreshToken != "refresh-u1-new" {
		t.Fatalf("credentials were not rotated before state failure: %+v", account.Auth)
	}
}

func TestTokenEnsureMissingRefreshTokenReportsStatePersistenceError(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	auth.RefreshToken = ""
	pool, dir := tokenTestPool(t, auth)
	account := pool.Get("u1")
	breakTokenStatePersistence(t, dir)

	err := manager.Ensure(context.Background(), account)
	if !errors.Is(err, ErrRefreshTokenMissing) {
		t.Fatalf("Ensure = %v, want ErrRefreshTokenMissing", err)
	}
	if !strings.Contains(err.Error(), "state") {
		t.Fatalf("error did not report state persistence failure: %v", err)
	}
	if calls, _, _, _, _ := fake.snapshot(); calls != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", calls)
	}
	if !account.State.Disabled || account.State.ManualDisabled {
		t.Fatalf("state = %+v, want in-memory terminal disable", account.State)
	}
}

func TestTokenEnsureConcurrentSingleFlight(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
	}
	started, release := blockTokenRefreshFake(fake)

	const callers = 20
	joined := make(chan struct{}, callers-1)
	manager.flightJoinHook = func() { joined <- struct{}{} }
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- manager.Ensure(context.Background(), account)
		}()
	}
	close(start)
	waitTokenRefreshStarted(t, started)
	waitTokenRefreshJoiners(t, joined, callers-1)
	release()
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}
	calls, _, _, _, _ := fake.snapshot()
	if calls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", calls)
	}
	if account.Auth.RefreshToken != "refresh-u1-new" {
		t.Fatalf("refresh token = %q", account.Auth.RefreshToken)
	}
}

func TestTokenEnsureManualDisableSkipsRefresh(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(10*time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	if err := pool.Disable("u1", "manual disable"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
	}

	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if calls, _, _, _, _ := fake.snapshot(); calls != 0 {
		t.Fatalf("token endpoint calls = %d, want 0 for a manual disable", calls)
	}
	if !account.State.Disabled || !account.State.ManualDisabled {
		t.Fatalf("manual disable cleared: %+v", account.State)
	}
	if account.State.Reason != "manual disable" {
		t.Fatalf("reason = %q, want manual disable", account.State.Reason)
	}
	if account.Auth.RefreshToken != "refresh-u1-old" {
		t.Fatalf("manual-disabled account spent its refresh token: %q", account.Auth.RefreshToken)
	}
}

func TestTokenEnsureSuccessClearsOnlyV1Fields(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(10*time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	if err := pool.NoteError(account, "previous terminal failure", false, true); err != nil {
		t.Fatalf("NoteError: %v", err)
	}
	if !account.State.Disabled || account.State.ManualDisabled {
		t.Fatalf("setup state = %+v", account.State)
	}
	account.mu.Lock()
	account.State.ErrCount = 2
	account.State.CoolUntil = unixSeconds(now.Add(time.Hour))
	account.mu.Unlock()
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
	}

	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if account.State.Disabled || account.State.ManualDisabled ||
		account.State.Reason != "" || account.State.LastError != "" {
		t.Fatalf("v1 failure fields not cleared: %+v", account.State)
	}
	if account.State.ErrCount != 2 {
		t.Fatalf("err_count = %d, want scheduler history preserved at 2", account.State.ErrCount)
	}
	if account.State.CoolUntil != unixSeconds(now.Add(time.Hour)) {
		t.Fatalf("cool_until = %v, want preserved", account.State.CoolUntil)
	}
}

func TestTokenEnsureTerminalFailuresDisableNonManual(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"401", http.StatusUnauthorized, `{"error":"unauthorized","error_msg":"refresh-u1-old verifier-u1 AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE"}`},
		{"403", http.StatusForbidden, `{"error":"forbidden","error_msg":"refresh-u1-old"}`},
		{"400 invalid_grant", http.StatusBadRequest, `{"error":"invalid_grant","error_msg":"refresh-u1-old has been used"}`},
		{"400 expired refresh token", http.StatusBadRequest, `{"error":"expiredRefreshToken","error_msg":"refresh-u1-old"}`},
		{"400 invalid DPoP header", http.StatusBadRequest, `{"error":"invalidDPoPHeader","error_msg":"refresh-u1-old"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, _ := newTokenRefreshFake(t)
			manager := newTokenTestManager(t, now)
			auth := tokenTestAuth("u1", now.Add(-time.Minute))
			pool, _ := tokenTestPool(t, auth)
			account := pool.Get("u1")
			fake.status = tc.status
			fake.body = tc.body

			err := manager.Ensure(context.Background(), account)
			if err == nil {
				t.Fatal("Ensure returned nil, want terminal error")
			}
			if !account.State.Disabled || account.State.ManualDisabled {
				t.Fatalf("state = %+v, want non-manual disabled", account.State)
			}
			if strings.TrimSpace(account.State.Reason) == "" {
				t.Fatalf("terminal reason is empty: %+v", account.State)
			}
			if strings.Contains(err.Error(), "refresh-u1-old") ||
				strings.Contains(err.Error(), "verifier-u1") ||
				strings.Contains(err.Error(), "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE") {
				t.Fatalf("error leaked a credential: %v", err)
			}
			if strings.Contains(account.State.LastError, "refresh-u1-old") ||
				strings.Contains(account.State.Reason, "refresh-u1-old") ||
				strings.Contains(account.State.LastError, "verifier-u1") ||
				strings.Contains(account.State.Reason, "verifier-u1") {
				t.Fatalf("state leaked a credential: %+v", account.State)
			}
			raw, readErr := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(account.Path)), "codearts", "state.json"))
			if readErr != nil {
				t.Fatalf("read state: %v", readErr)
			}
			if bytes.Contains(raw, []byte("refresh-u1-old")) ||
				bytes.Contains(raw, []byte("verifier-u1")) ||
				bytes.Contains(raw, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE")) {
				t.Fatalf("state file leaked a credential: %s", raw)
			}
		})
	}
}

func TestTokenEnsureConcurrentDisableVsRefreshPreservesManualState(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
	}
	started, release := blockTokenRefreshFake(fake)

	errs := make(chan error, 1)
	go func() { errs <- manager.Ensure(context.Background(), account) }()
	waitTokenRefreshStarted(t, started)
	if err := pool.Disable("u1", "manual disable"); err != nil {
		t.Fatal(err)
	}
	release()

	if err := <-errs; err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if account.Auth.RefreshToken != "refresh-u1-new" {
		t.Fatalf("token not rotated: %q", account.Auth.RefreshToken)
	}
	if !account.State.Disabled || !account.State.ManualDisabled {
		t.Fatalf("manual disable cleared: %+v", account.State)
	}
	if account.State.Reason != "manual disable" {
		t.Fatalf("reason = %q, want manual disable", account.State.Reason)
	}
}

func TestTokenEnsureNonTerminal400DoesNotDisable(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	fake.status = http.StatusBadRequest
	fake.body = `{"error":"temporarily unavailable","error_msg":"try later"}`

	err := manager.Ensure(context.Background(), account)
	if err == nil {
		t.Fatal("expired token refresh should return the transient error")
	}
	if account.State.Disabled || account.State.ManualDisabled {
		t.Fatalf("transient failure disabled the account: %+v", account.State)
	}
}

func TestTokenEnsureTransientFailureWithValidTokenIsSwallowed(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(10*time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	fake.status = http.StatusInternalServerError
	fake.body = `{"error":"upstream","error_msg":"refresh-u1-old"}`

	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("Ensure = %v, want swallowed transient failure", err)
	}
	calls, _, _, _, _ := fake.snapshot()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if account.Auth.CloudDragonToken != "cloud-old-u1" ||
		account.Auth.RefreshToken != "refresh-u1-old" {
		t.Fatalf("credentials changed after transient failure: %+v", account.Auth)
	}
}

func TestTokenEnsureExpiredTransientFailureReturnsError(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	fake.status = http.StatusInternalServerError
	fake.body = `{"error":"upstream","error_msg":"refresh-u1-old"}`

	err := manager.Ensure(context.Background(), account)
	if err == nil {
		t.Fatal("expired token refresh should return the transient error")
	}
	if !errors.Is(err, ErrTokenRejected) && !errors.Is(err, ErrTokenTransport) &&
		!errors.Is(err, ErrTokenResponse) {
		t.Fatalf("unexpected error kind: %v", err)
	}
	if strings.Contains(err.Error(), "refresh-u1-old") {
		t.Fatalf("error leaked refresh token: %v", err)
	}
}

func TestTokenEnsureMissingDPoPKeyGeneratesAndPersists(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(10*time.Minute))
	auth.DPopPrivateKey = nil
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	if usableDPoPKey(account.Auth.DPopPrivateKey) {
		t.Fatalf("setup DPoP key = %s", account.Auth.DPopPrivateKey)
	}
	fake.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
	}

	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(account.Auth.DPopPrivateKey) == 0 {
		t.Fatal("in-memory DPoP key was not generated")
	}
	reloaded, err := LoadAuth(account.Path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if !sameDPoPKey(reloaded.DPopPrivateKey, account.Auth.DPopPrivateKey) {
		t.Fatalf("persisted DPoP key = %s, want %s", reloaded.DPopPrivateKey, account.Auth.DPopPrivateKey)
	}
	if _, err := parseDPoPPrivateKey(reloaded.DPopPrivateKey); err != nil {
		t.Fatalf("persisted DPoP key is invalid: %v", err)
	}
	_, _, _, proofErrs, _ := fake.snapshot()
	for _, proofErr := range proofErrs {
		if proofErr != nil {
			t.Fatalf("DPoP proof error: %v", proofErr)
		}
	}
}

func TestTokenEnsureMissingDPoPKeyPersistedEvenWhenRefreshFails(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(10*time.Minute))
	auth.DPopPrivateKey = nil
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	fake.status = http.StatusInternalServerError

	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("valid-token transient failure should be swallowed: %v", err)
	}
	reloaded, err := LoadAuth(account.Path)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if len(reloaded.DPopPrivateKey) == 0 {
		t.Fatal("generated DPoP key was not persisted before the failed refresh")
	}
}

func TestTokenEnsureUsesAccountProxyAndDefaultClientID(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	proxy := &tokenRefreshFake{}
	proxy.payload = func(form url.Values) map[string]any {
		return tokenTestPayload("u1", "refresh-u1-new", now.Add(time.Hour))
	}
	proxyServer := httptest.NewServer(proxy)
	t.Cleanup(proxyServer.Close)
	t.Setenv("WB_CODEARTS_STS_HOST", "http://sts.codearts.invalid")
	t.Setenv("WB_CODEARTS_CLIENT_ID", "")

	auth := tokenTestAuth("u1", now.Add(10*time.Minute))
	auth.ClientID = ""
	auth.ProxySlot = "slot-1"
	pool, _ := tokenTestPool(t, auth)
	pool.ApplyProxySlots(transport.Slots{
		{ID: "slot-1", URL: proxyServer.URL, Enabled: true},
	})
	account := pool.Get("u1")
	manager := newTokenTestManager(t, now)

	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	calls, forms, proofs, proofErrs, targets := proxy.snapshot()
	if calls != 1 {
		t.Fatalf("proxy calls = %d, want 1", calls)
	}
	if len(targets) != 1 || targets[0] != "http://sts.codearts.invalid/v1/oauth2/tokens" {
		t.Fatalf("proxy targets = %v", targets)
	}
	if proofErrs[0] != nil {
		t.Fatalf("DPoP proof error: %v", proofErrs[0])
	}
	if proofs[0].Htu != "http://sts.codearts.invalid/v1/oauth2/tokens" {
		t.Fatalf("DPoP htu = %q", proofs[0].Htu)
	}
	form := forms[0]
	if form.Get("client_id") != "codearts-agent" {
		t.Fatalf("default client_id = %q", form.Get("client_id"))
	}
	if form.Get("code_verifier") != "verifier-u1" ||
		form.Get("grant_type") != "refresh_token" ||
		form.Get("refresh_token") != "refresh-u1-old" {
		t.Fatalf("form = %v", form)
	}
}

func TestTokenEnsureRejectsMissingRefreshToken(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	auth.RefreshToken = ""
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")

	err := manager.Ensure(context.Background(), account)
	if !errors.Is(err, ErrRefreshTokenMissing) {
		t.Fatalf("Ensure = %v, want ErrRefreshTokenMissing", err)
	}
	if calls, _, _, _, _ := fake.snapshot(); calls != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", calls)
	}
	if !account.State.Disabled || account.State.ManualDisabled {
		t.Fatalf("state = %+v, want non-manual disabled", account.State)
	}
}

func TestTokenKeepaliveSkipsDisabledAndCollectsErrors(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auths := []Auth{
		tokenTestAuth("u1", now.Add(2*time.Hour)),    // valid, skip
		tokenTestAuth("u2", now.Add(10*time.Minute)), // refresh succeeds
		tokenTestAuth("u3", now.Add(10*time.Minute)), // manual disabled, skip
		tokenTestAuth("u4", now.Add(10*time.Minute)), // non-manual disabled, skip
		tokenTestAuth("u5", now.Add(-time.Minute)),   // refresh fails
		tokenTestAuth("u6", now.Add(10*time.Minute)), // removed, skip
	}
	pool, _ := tokenTestPool(t, auths...)
	manager.pool = pool
	if err := pool.Disable("u3", "manual disable"); err != nil {
		t.Fatal(err)
	}
	if err := pool.NoteError(pool.Get("u4"), "terminal", false, true); err != nil {
		t.Fatal(err)
	}
	removed := pool.Get("u6")
	removed.mu.Lock()
	removed.removed = true
	removed.mu.Unlock()

	fake.payload = func(form url.Values) map[string]any {
		refresh := form.Get("refresh_token")
		if refresh == "refresh-u5-old" {
			return map[string]any{}
		}
		uid := strings.TrimSuffix(strings.TrimPrefix(refresh, "refresh-"), "-old")
		return tokenTestPayload(uid, "refresh-"+uid+"-new", now.Add(time.Hour))
	}

	refreshed, err := manager.Keepalive(context.Background())
	if err == nil {
		t.Fatal("Keepalive should report the failed account")
	}
	if refreshed != 1 {
		t.Fatalf("refreshed = %d, want 1", refreshed)
	}
	calls, forms, _, _, _ := fake.snapshot()
	if calls != 2 {
		t.Fatalf("token endpoint calls = %d, want 2", calls)
	}
	seen := map[string]bool{}
	for _, form := range forms {
		seen[form.Get("refresh_token")] = true
	}
	if !seen["refresh-u2-old"] || !seen["refresh-u5-old"] ||
		seen["refresh-u1-old"] || seen["refresh-u3-old"] ||
		seen["refresh-u4-old"] || seen["refresh-u6-old"] {
		t.Fatalf("refreshed accounts = %v", seen)
	}
	if pool.Get("u2").Auth.RefreshToken != "refresh-u2-new" {
		t.Fatalf("u2 refresh token = %q", pool.Get("u2").Auth.RefreshToken)
	}
	if pool.Get("u5").State.Disabled {
		t.Fatalf("u5 was disabled on a transient failure: %+v", pool.Get("u5").State)
	}
	if strings.Contains(err.Error(), "refresh-u5-old") {
		t.Fatalf("Keepalive error leaked a refresh token: %v", err)
	}
}

func TestTokenEnsureTerminalFailureAfterReimportDoesNotDisableNewCredentials(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auth := tokenTestAuth("u1", now.Add(-time.Minute))
	pool, _ := tokenTestPool(t, auth)
	account := pool.Get("u1")
	replacement := tokenTestAuth("u1", now.Add(2*time.Hour))
	replacement.RefreshToken = "refresh-u1-reimport"
	replacement.CloudDragonToken = "cloud-reimport"
	fake.status = http.StatusUnauthorized
	fake.body = `{"error":"invalid_grant","error_msg":"refresh-u1-old has been used"}`
	started, release := blockTokenRefreshFake(fake)

	errs := make(chan error, 1)
	go func() { errs <- manager.Ensure(context.Background(), account) }()
	waitTokenRefreshStarted(t, started)
	if _, err := pool.Add(replacement); err != nil {
		t.Fatalf("Add re-import: %v", err)
	}
	release()

	if err := <-errs; err != nil {
		t.Fatalf("superseded terminal failure should be ignored, got %v", err)
	}
	if account.Auth.RefreshToken != "refresh-u1-reimport" {
		t.Fatalf("re-import credentials = %+v", account.Auth)
	}
	if account.State.Disabled || account.State.ManualDisabled {
		t.Fatalf("new credentials were disabled by the old failure: %+v", account.State)
	}
}

func TestTokenKeepaliveStopsAfterCancellation(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fake, _ := newTokenRefreshFake(t)
	manager := newTokenTestManager(t, now)
	auths := []Auth{
		tokenTestAuth("u1", now.Add(-time.Minute)),
		tokenTestAuth("u2", now.Add(-time.Minute)),
	}
	pool, _ := tokenTestPool(t, auths...)
	manager.pool = pool
	fake.payload = func(form url.Values) map[string]any {
		refresh := form.Get("refresh_token")
		uid := strings.TrimSuffix(strings.TrimPrefix(refresh, "refresh-"), "-old")
		return tokenTestPayload(uid, "refresh-"+uid+"-new", now.Add(time.Hour))
	}
	started, release := blockTokenRefreshFake(fake)

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		refreshed int
		err       error
	}
	done := make(chan result, 1)
	go func() {
		refreshed, err := manager.Keepalive(ctx)
		done <- result{refreshed: refreshed, err: err}
	}()
	waitTokenRefreshStarted(t, started)
	cancel()
	release()

	got := <-done
	if got.refreshed != 1 {
		t.Fatalf("refreshed = %d, want 1", got.refreshed)
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Keepalive error = %v, want context.Canceled", got.err)
	}
	if calls, _, _, _, _ := fake.snapshot(); calls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1 after cancellation", calls)
	}
	if pool.Get("u2").Auth.RefreshToken != "refresh-u2-old" {
		t.Fatalf("keepalive refreshed an account after cancellation: %q",
			pool.Get("u2").Auth.RefreshToken)
	}
}

func TestTokenKeepaliveRequiresPoolAndContext(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	manager := newTokenTestManager(t, now)
	if _, err := manager.Keepalive(context.Background()); err == nil {
		t.Fatal("Keepalive without a pool should fail")
	}
	if err := manager.Ensure(nil, nil); err == nil {
		t.Fatal("Ensure(nil, nil) should fail")
	}
	manager.pool = nil
	if _, err := manager.Keepalive(context.Background()); err == nil {
		t.Fatal("Keepalive without a pool should fail after reset")
	}
}

func TestTokenProviderWiresManager(t *testing.T) {
	dir := t.TempDir()
	store := config.New(filepath.Join(dir, "settings.json"))
	provider, err := New(store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if provider.TokenManager() == nil {
		t.Fatal("Provider.TokenManager() is nil")
	}
	if provider.TokenManager() != provider.tokenManager {
		t.Fatal("Provider.TokenManager() did not return the owned manager")
	}
	if provider.TokenManager().pool != provider.Pool() {
		t.Fatal("token manager is not bound to the provider pool")
	}
	if provider.TokenManager().timeout != 30*time.Second {
		t.Fatalf("token manager timeout = %v, want 30s", provider.TokenManager().timeout)
	}
}
