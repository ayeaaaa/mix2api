package raccoon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/accountstate"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

func newTokenTestManager(t *testing.T, baseURL string, now time.Time) *TokenManager {
	t.Helper()
	client := transport.NewClient(transport.ClientOptions{})
	return NewTokenManager(client, TokenManagerOptions{
		BaseURL: baseURL,
		Now:     func() time.Time { return now },
	})
}

func TestRefreshRotatesAndPersists(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	oldExpiry := now.Add(60 * time.Second)
	newExpiry := now.Add(3600 * time.Second)
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-old", "exp": oldExpiry.Unix()})
	newToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-new", "exp": newExpiry.Unix()})

	var (
		mu            sync.Mutex
		method        string
		path          string
		authorization string
		body          map[string]string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requestBody map[string]string
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode refresh request: %v", err)
		}
		mu.Lock()
		method = r.Method
		path = r.URL.Path
		authorization = r.Header.Get("Authorization")
		body = requestBody
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{
			"access_token":%q,
			"refresh_token":"new-refresh-secret",
			"token_type":"Bearer",
			"office_identity":"organization",
			"office_org_name":"New Org",
			"office_org_role":"admin",
			"expires_at":%d
		}}`, newToken, newExpiry.Unix())
	}))
	defer server.Close()

	dir := t.TempDir()
	authPath := filepath.Join(dir, "raccoon", "raccoon-pu-old.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}

	// A realistic v1 file with known extras and an unknown future field. The
	// refresh write must preserve everything it did not rotate.
	extras := fmt.Sprintf(`{
		"uid":"pu-old",
		"access_token":%q,
		"refresh_token":"old-refresh-secret",
		"office_identity":"personal",
		"office_org_name":"Old Org",
		"expires_at":%d,
		"token_type":"Bearer",
		"office_org_role":"member",
		"proxySlot":"slot-a",
		"updated_at":1794024020,
		"future_metadata":{"flags":[1,2],"note":"keep"}
	}`, oldToken, oldExpiry.Unix())
	if err := os.WriteFile(authPath, []byte(extras), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAuth(authPath)
	if err != nil {
		t.Fatal(err)
	}
	account := &Account{Auth: loaded, Path: authPath}

	manager := newTokenTestManager(t, server.URL, now)
	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	mu.Lock()
	gotMethod, gotPath, gotAuthorization, gotBody := method, path, authorization, body
	mu.Unlock()
	if gotMethod != http.MethodPost || gotPath != "/api/web/auth/v1/refresh" {
		t.Fatalf("refresh request = %s %s, want POST /api/web/auth/v1/refresh", gotMethod, gotPath)
	}
	if gotAuthorization != "Bearer "+oldToken {
		t.Fatalf("Authorization = %q, want Bearer <old token>", gotAuthorization)
	}
	if len(gotBody) != 1 || gotBody["refresh_token"] != "old-refresh-secret" {
		t.Fatalf("refresh body = %#v, want only refresh_token", gotBody)
	}

	if account.Auth.AccessToken != newToken {
		t.Fatalf("in-memory access token was not rotated")
	}
	if account.Auth.RefreshToken != "new-refresh-secret" {
		t.Fatalf("RefreshToken = %q, want new-refresh-secret", account.Auth.RefreshToken)
	}
	if account.Auth.UID != "pu-new" {
		t.Fatalf("UID = %q, want pu-new derived from access token", account.Auth.UID)
	}
	if account.Auth.OfficeOrgName != "New Org" || account.Auth.OfficeIdentity != "organization" ||
		account.Auth.OfficeOrgRole != "admin" || account.Auth.TokenType != "Bearer" {
		t.Fatalf("refreshed auth fields = %#v", account.Auth)
	}
	if !account.Auth.ExpiresAt.Equal(newExpiry) {
		t.Fatalf("ExpiresAt = %v, want %v", account.Auth.ExpiresAt, newExpiry)
	}
	if account.Auth.UpdatedAt == 0 {
		t.Fatal("UpdatedAt was not set")
	}

	stored := readJSONObject(t, authPath)
	var storedAccess, storedRefresh, storedUID, storedOrg, storedRole, storedSlot string
	for key, destination := range map[string]*string{
		"access_token":    &storedAccess,
		"refresh_token":   &storedRefresh,
		"uid":             &storedUID,
		"office_org_name": &storedOrg,
		"office_org_role": &storedRole,
		"proxySlot":       &storedSlot,
	} {
		if err := json.Unmarshal(stored[key], destination); err != nil {
			t.Fatalf("decode stored %s: %v", key, err)
		}
	}
	if storedAccess != newToken || storedRefresh != "new-refresh-secret" {
		t.Fatal("rotated tokens were not persisted")
	}
	if storedUID != "pu-new" || storedOrg != "New Org" || storedRole != "admin" || storedSlot != "slot-a" {
		t.Fatalf("persisted auth fields = uid %q org %q role %q proxySlot %q", storedUID, storedOrg, storedRole, storedSlot)
	}
	if _, ok := stored["future_metadata"]; !ok {
		t.Fatalf("unknown v1 auth field was dropped: %v", keysOf(stored))
	}
	var storedExpiry int64
	if err := json.Unmarshal(stored["expires_at"], &storedExpiry); err != nil || storedExpiry != newExpiry.Unix() {
		t.Fatalf("stored expires_at = %d (err %v), want %d", storedExpiry, err, newExpiry.Unix())
	}
}

func TestRefreshDerivesExpiryAndAcceptsTopLevelResponse(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	newExpiry := now.Add(1800 * time.Second)
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(time.Minute).Unix()})
	newToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": newExpiry.Unix()})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No data wrapper and no expires_at: v1 accepts both shapes and derives
		// the expiry from the new access token's JWT exp claim.
		fmt.Fprintf(w, `{"access_token":%q}`, newToken)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json")
	account := &Account{
		Auth: Auth{
			UID:          "pu-test",
			AccessToken:  oldToken,
			RefreshToken: "refresh-secret",
			ExpiresAt:    now.Add(time.Minute),
		},
		Path: path,
	}
	manager := newTokenTestManager(t, server.URL, now)
	if err := manager.Refresh(context.Background(), account); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if account.Auth.AccessToken != newToken {
		t.Fatal("access token was not rotated")
	}
	if account.Auth.RefreshToken != "refresh-secret" {
		t.Fatalf("RefreshToken = %q, want the old token preserved", account.Auth.RefreshToken)
	}
	if !account.Auth.ExpiresAt.Equal(newExpiry) {
		t.Fatalf("ExpiresAt = %v, want JWT exp %v", account.Auth.ExpiresAt, newExpiry)
	}
}

func TestConcurrentEnsureRefreshesOnce(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	newExpiry := now.Add(3600 * time.Second)
	newToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": newExpiry.Unix()})
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(-time.Minute).Unix()})

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(w, `{"data":{"access_token":%q,"refresh_token":"next","expires_at":%d}}`, newToken, newExpiry.Unix())
	}))
	defer server.Close()

	account := &Account{
		Auth: Auth{
			UID:          "pu-test",
			AccessToken:  oldToken,
			RefreshToken: "refresh-secret",
			ExpiresAt:    now.Add(-time.Minute),
		},
		Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
	}
	manager := newTokenTestManager(t, server.URL, now)

	const goroutines = 8
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- manager.Ensure(context.Background(), account)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if account.Auth.AccessToken != newToken || account.Auth.RefreshToken != "next" {
		t.Fatal("accounts observed by concurrent callers did not receive the rotated credentials")
	}
}

func TestConcurrentRefreshCoalescesOncePerGeneration(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	expiry := now.Add(3600 * time.Second)
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(time.Minute).Unix()})

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		token := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": expiry.Unix()})
		fmt.Fprintf(w, `{"data":{"access_token":%q,"refresh_token":"next-%d","expires_at":%d}}`, token, call, expiry.Unix())
	}))
	defer server.Close()

	account := &Account{
		Auth: Auth{
			UID:          "pu-test",
			AccessToken:  oldToken,
			RefreshToken: "refresh-secret",
			ExpiresAt:    now.Add(time.Minute),
		},
		Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
	}
	manager := newTokenTestManager(t, server.URL, now)

	const goroutines = 8
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- manager.Refresh(context.Background(), account)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Refresh() error = %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent forced refresh calls = %d, want 1", got)
	}
	if got := manager.Generation(account); got != 1 {
		t.Fatalf("generation = %d, want 1", got)
	}

	if err := manager.Refresh(context.Background(), account); err != nil {
		t.Fatalf("second sequential Refresh() error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("sequential forced refresh calls = %d, want 2", got)
	}
	if got := manager.Generation(account); got != 2 {
		t.Fatalf("generation after sequential refresh = %d, want 2", got)
	}
}

func TestRefreshAuthFailureMarksTerminal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			now := time.Unix(1800000000, 0).UTC()
			oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(-time.Minute).Unix()})
			responseBody := `{"message":"invalid old-access-secret old-refresh-secret","access_token":"leaked-access-value","refresh_token":"leaked-refresh-value"}`
			if status == http.StatusForbidden {
				// No recognized message key: this exercises redaction of raw
				// token-shaped JSON fields in the fallback body.
				responseBody = `{"access_token":"leaked-access-value","refresh_token":"leaked-refresh-value"}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, responseBody)
			}))
			defer server.Close()

			authPath := filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json")
			account := &Account{
				Auth: Auth{
					UID:          "pu-test",
					AccessToken:  oldToken,
					RefreshToken: "old-refresh-secret",
					ExpiresAt:    now.Add(-time.Minute),
				},
				Path: authPath,
			}
			manager := newTokenTestManager(t, server.URL, now)
			err := manager.Refresh(context.Background(), account)
			if err == nil {
				t.Fatal("Refresh() error = nil, want auth failure")
			}
			if !IsAuthError(err) {
				t.Fatalf("IsAuthError(%v) = false, want true", err)
			}
			assertNoTokenValues(t, err.Error(), oldToken, "old-refresh-secret", "leaked-access-value", "leaked-refresh-value")
			if !account.State.Disabled {
				t.Fatal("terminal auth failure did not disable the account")
			}
			assertNoTokenValues(t, account.State.Reason, oldToken, "old-refresh-secret", "leaked-access-value", "leaked-refresh-value")
			if _, statErr := os.Stat(authPath); !os.IsNotExist(statErr) {
				t.Fatalf("terminal refresh wrote an auth file: %v", statErr)
			}
		})
	}
}

func TestRefreshInvalidGrantMarksTerminal(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(-time.Minute).Unix()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh old-refresh-secret expired"}`)
	}))
	defer server.Close()

	account := &Account{
		Auth: Auth{
			UID:          "pu-test",
			AccessToken:  oldToken,
			RefreshToken: "old-refresh-secret",
			ExpiresAt:    now.Add(-time.Minute),
		},
		Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
	}
	manager := newTokenTestManager(t, server.URL, now)
	err := manager.Refresh(context.Background(), account)
	if err == nil || !IsAuthError(err) {
		t.Fatalf("Refresh() error = %v, want terminal auth error", err)
	}
	assertNoTokenValues(t, err.Error(), "old-refresh-secret")
	if !account.State.Disabled {
		t.Fatal("invalid grant did not disable the account")
	}
}

func TestRefreshNetworkFailureIsTransient(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	current := now
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(time.Minute).Unix()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	account := &Account{
		Auth: Auth{
			UID:          "pu-test",
			AccessToken:  oldToken,
			RefreshToken: "refresh-secret",
			ExpiresAt:    now.Add(60 * time.Second),
		},
		Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
	}
	manager := NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
		BaseURL: serverURL,
		Now:     func() time.Time { return current },
	})

	err := manager.Refresh(context.Background(), account)
	if err == nil {
		t.Fatal("Refresh() error = nil, want network error")
	}
	if IsAuthError(err) {
		t.Fatalf("network error %v was classified as auth failure", err)
	}
	if !IsTransientError(err) {
		t.Fatalf("IsTransientError(%v) = false, want true", err)
	}
	if account.State.Disabled {
		t.Fatal("network failure disabled the account")
	}
	assertNoTokenValues(t, err.Error(), oldToken, "refresh-secret")

	// A still-valid token keeps working when a proactive refresh fails.
	if err := manager.Ensure(context.Background(), account); err != nil {
		t.Fatalf("Ensure() after transient failure = %v, want nil", err)
	}
	if account.State.Disabled {
		t.Fatal("Ensure() disabled the account on a transient failure")
	}

	// An expired token reports the transient failure but still does not mark
	// the account terminal.
	current = now.Add(2 * time.Minute)
	if err := manager.Ensure(context.Background(), account); err == nil {
		t.Fatal("Ensure() with expired token error = nil, want transient error")
	}
	if account.State.Disabled {
		t.Fatal("expired-token network failure disabled the account")
	}
}

func TestEnsureRefreshWindow(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	cases := []struct {
		name      string
		expiresIn time.Duration
		unknown   bool
		want      int32
	}{
		{name: "outside window", expiresIn: 121 * time.Second, want: 0},
		{name: "boundary", expiresIn: 120 * time.Second, want: 1},
		{name: "inside window", expiresIn: 119 * time.Second, want: 1},
		{name: "expired", expiresIn: -time.Second, want: 1},
		{name: "unknown expiry", unknown: true, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newExpiry := now.Add(3600 * time.Second)
			newToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": newExpiry.Unix()})
			oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(tc.expiresIn).Unix()})
			if tc.unknown {
				// An opaque token has no JWT exp claim, so the account's
				// expiry really is unknown.
				oldToken = "opaque-old-token"
			}

			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				fmt.Fprintf(w, `{"data":{"access_token":%q,"refresh_token":"next","expires_at":%d}}`, newToken, newExpiry.Unix())
			}))
			defer server.Close()

			auth := Auth{
				UID:          "pu-test",
				AccessToken:  oldToken,
				RefreshToken: "refresh-secret",
			}
			if !tc.unknown {
				auth.ExpiresAt = now.Add(tc.expiresIn)
			}
			account := &Account{
				Auth: auth,
				Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
			}
			manager := newTokenTestManager(t, server.URL, now)
			if err := manager.Ensure(context.Background(), account); err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			if got := calls.Load(); got != tc.want {
				t.Fatalf("refresh calls = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRefreshStateTransitions(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	newExpiry := now.Add(3600 * time.Second)
	newToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": newExpiry.Unix()})
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(time.Minute).Unix()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":{"access_token":%q,"refresh_token":"next","expires_at":%d}}`, newToken, newExpiry.Unix())
	}))
	defer server.Close()

	t.Run("manual disable survives", func(t *testing.T) {
		account := &Account{
			Auth: Auth{
				UID:          "pu-test",
				AccessToken:  oldToken,
				RefreshToken: "refresh-secret",
				ExpiresAt:    now.Add(time.Minute),
			},
			State: accountstate.State{
				UID:            "pu-test",
				Disabled:       true,
				ManualDisabled: true,
				Reason:         "disabled from the panel",
			},
			Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
		}
		manager := newTokenTestManager(t, server.URL, now)
		if err := manager.Ensure(context.Background(), account); err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		if !account.State.Disabled || !account.State.ManualDisabled || account.State.Reason != "disabled from the panel" {
			t.Fatalf("manual disabled state changed after refresh: %#v", account.State)
		}
	})

	t.Run("non-manual disable recovers", func(t *testing.T) {
		account := &Account{
			Auth: Auth{
				UID:          "pu-test",
				AccessToken:  oldToken,
				RefreshToken: "refresh-secret",
				ExpiresAt:    now.Add(time.Minute),
			},
			State: accountstate.State{
				UID:      "pu-test",
				Disabled: true,
				Reason:   "old auth failure",
			},
			Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
		}
		manager := newTokenTestManager(t, server.URL, now)
		if err := manager.Ensure(context.Background(), account); err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
		if account.State.Disabled || account.State.Reason != "" {
			t.Fatalf("non-manual disable did not recover: %#v", account.State)
		}
	})
}

func TestRefreshMissingRefreshTokenIsTerminal(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(time.Minute).Unix()})
	account := &Account{
		Auth: Auth{
			UID:         "pu-test",
			AccessToken: oldToken,
			ExpiresAt:   now.Add(time.Minute),
		},
		Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
	}
	manager := newTokenTestManager(t, server.URL, now)
	err := manager.Ensure(context.Background(), account)
	if err == nil || !IsAuthError(err) {
		t.Fatalf("Ensure() error = %v, want terminal auth error", err)
	}
	if calls.Load() != 0 {
		t.Fatal("missing refresh token still called the refresh endpoint")
	}
	if !account.State.Disabled {
		t.Fatal("missing refresh token did not disable the account")
	}
}

func TestRefreshIfGenerationSkipsRotatedGeneration(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	newExpiry := now.Add(3600 * time.Second)
	newToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": newExpiry.Unix()})
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(time.Minute).Unix()})

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintf(w, `{"data":{"access_token":%q,"refresh_token":"next","expires_at":%d}}`, newToken, newExpiry.Unix())
	}))
	defer server.Close()

	account := &Account{
		Auth: Auth{
			UID:          "pu-test",
			AccessToken:  oldToken,
			RefreshToken: "refresh-secret",
			ExpiresAt:    now.Add(time.Minute),
		},
		Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
	}
	manager := newTokenTestManager(t, server.URL, now)
	observed := manager.Generation(account)
	if err := manager.RefreshIfGeneration(context.Background(), account, observed); err != nil {
		t.Fatalf("RefreshIfGeneration() error = %v", err)
	}
	if got := manager.Generation(account); got != observed+1 {
		t.Fatalf("generation = %d, want %d", got, observed+1)
	}

	// A second 401 handler that captured the pre-refresh generation must not
	// consume the rotated refresh token again.
	if err := manager.RefreshIfGeneration(context.Background(), account, observed); err != nil {
		t.Fatalf("stale RefreshIfGeneration() error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestConcurrentEnsureFailureCoalesces(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(-time.Minute).Unix()})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"temporary upstream failure"}`)
	}))
	defer server.Close()

	account := &Account{
		Auth: Auth{
			UID:          "pu-test",
			AccessToken:  oldToken,
			RefreshToken: "refresh-secret",
			ExpiresAt:    now.Add(-time.Minute),
		},
		Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
	}
	manager := newTokenTestManager(t, server.URL, now)

	const goroutines = 8
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- manager.Ensure(context.Background(), account)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("Ensure() error = nil, want the shared transient failure")
		}
		if IsAuthError(err) || !IsTransientError(err) {
			t.Fatalf("Ensure() error = %v, want transient", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 shared failure", got)
	}
	if account.State.Disabled {
		t.Fatal("transient failure disabled the account")
	}
}

func TestRefreshMissingAccessTokenIsTerminal(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(time.Minute).Unix()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"refresh_token":"new-refresh-secret"}}`)
	}))
	defer server.Close()

	authPath := filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json")
	account := &Account{
		Auth: Auth{
			UID:          "pu-test",
			AccessToken:  oldToken,
			RefreshToken: "old-refresh-secret",
			ExpiresAt:    now.Add(time.Minute),
		},
		Path: authPath,
	}
	manager := newTokenTestManager(t, server.URL, now)
	err := manager.Refresh(context.Background(), account)
	if err == nil || !IsAuthError(err) {
		t.Fatalf("Refresh() error = %v, want terminal auth error", err)
	}
	if !account.State.Disabled {
		t.Fatal("a refresh response without access_token did not disable the account")
	}
	if _, statErr := os.Stat(authPath); !os.IsNotExist(statErr) {
		t.Fatalf("refresh with no access token wrote an auth file: %v", statErr)
	}
}

func TestRefreshDoesNotPublishWhenSaveFails(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	newExpiry := now.Add(3600 * time.Second)
	newToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": newExpiry.Unix()})
	oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(time.Minute).Unix()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":{"access_token":%q,"refresh_token":"next","expires_at":%d}}`, newToken, newExpiry.Unix())
	}))
	defer server.Close()

	// A directory in place of the auth file makes the atomic save fail.
	authPath := t.TempDir()
	account := &Account{
		Auth: Auth{
			UID:          "pu-test",
			AccessToken:  oldToken,
			RefreshToken: "refresh-secret",
			ExpiresAt:    now.Add(time.Minute),
		},
		Path: authPath,
	}
	manager := newTokenTestManager(t, server.URL, now)
	if err := manager.Refresh(context.Background(), account); err == nil {
		t.Fatal("Refresh() error = nil, want save failure")
	}
	if account.Auth.AccessToken != oldToken || account.Auth.RefreshToken != "refresh-secret" {
		t.Fatal("refresh published credentials before SaveAuth succeeded")
	}
	if account.State.Disabled {
		t.Fatal("save failure was treated as a terminal auth failure")
	}
}

func TestRefreshHTTPErrorsAreTransient(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			now := time.Unix(1800000000, 0).UTC()
			oldToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": now.Add(time.Minute).Unix()})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"upstream unavailable"}`)
			}))
			defer server.Close()

			account := &Account{
				Auth: Auth{
					UID:          "pu-test",
					AccessToken:  oldToken,
					RefreshToken: "refresh-secret",
					ExpiresAt:    now.Add(time.Minute),
				},
				Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-test.json"),
			}
			manager := newTokenTestManager(t, server.URL, now)
			err := manager.Refresh(context.Background(), account)
			if err == nil || IsAuthError(err) || !IsTransientError(err) {
				t.Fatalf("Refresh() error = %v, want transient", err)
			}
			if account.State.Disabled {
				t.Fatal("transient HTTP failure disabled the account")
			}
		})
	}
}

func assertNoTokenValues(t *testing.T, text string, values ...string) {
	t.Helper()
	for _, value := range values {
		if value != "" && strings.Contains(text, value) {
			t.Fatalf("text %q leaked secret %q", text, value)
		}
	}
}
