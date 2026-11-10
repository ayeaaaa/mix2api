package panel

import (
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

func testPassword(t *testing.T) (*Password, *config.Store) {
	t.Helper()
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	return NewPassword(store), store
}

func TestPBKDF2Vector(t *testing.T) {
	got, err := pbkdf2SHA256([]byte("passwd"), []byte("salt"), 1, 64)
	if err != nil {
		t.Fatal(err)
	}
	want := "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783"
	if hex.EncodeToString(got) != want {
		t.Fatalf("pbkdf2 vector = %x", got)
	}
}

func TestPanelPassword(t *testing.T) {
	password, store := testPassword(t)
	if !password.IsDefault() {
		t.Fatal("fresh install is not default")
	}
	valid, err := password.Verify("admin")
	if err != nil || !valid {
		t.Fatalf("default verify = %v %v", valid, err)
	}
	if err := password.Set("secret123"); err != nil {
		t.Fatal(err)
	}
	if password.IsDefault() {
		t.Fatal("custom password reported default")
	}
	valid, err = password.Verify("secret123")
	if err != nil || !valid {
		t.Fatalf("custom verify = %v %v", valid, err)
	}
	valid, _ = password.Verify("admin")
	if valid {
		t.Fatal("default password still opens after change")
	}
	data, _ := store.Load()
	if data["panel_password_hash"] == "" || data["panel_password_salt"] == "" {
		t.Fatalf("stored hash fields = %#v", data)
	}
	if strings.Contains(data["panel_password_hash"].(string), "secret123") {
		t.Fatal("plaintext password stored")
	}

	// Corrupt salt and rounds fail safely.
	hash := data["panel_password_hash"]
	_, _ = store.Merge(map[string]any{"panel_password_salt": "not-hex"})
	valid, err = password.Verify("secret123")
	if err != nil || valid {
		t.Fatalf("corrupt salt = %v %v", valid, err)
	}
	_, _ = store.Merge(map[string]any{"panel_password_salt": data["panel_password_salt"], "panel_password_hash": hash, "panel_password_rounds": 0})
	valid, err = password.Verify("secret123")
	if err != nil || valid {
		t.Fatalf("zero rounds = %v %v", valid, err)
	}

	// Setting the default back restores the sentinel.
	if err := password.Set("admin"); err != nil {
		t.Fatal(err)
	}
	if !password.IsDefault() {
		t.Fatal("default sentinel was not restored")
	}
	valid, _ = password.Verify("secret123")
	if valid {
		t.Fatal("old password still works after reset")
	}

	// Crypto-unavailable: default still opens; a custom digest errors.
	password, _ = testPassword(t)
	password.PBKDF2 = func([]byte, []byte, int, int) ([]byte, error) {
		return nil, errors.New("no pbkdf2")
	}
	ok, reason := password.CryptoAvailable()
	if ok || reason == "" {
		t.Fatalf("crypto available = %v reason=%q", ok, reason)
	}
	if valid, err := password.Verify("admin"); err != nil || !valid {
		t.Fatalf("default with crypto down = %v %v", valid, err)
	}
	if err := password.Set("secret123"); err == nil {
		t.Fatal("set succeeded with crypto down")
	}

	// A stored custom digest must surface the crypto error, not authenticate.
	password, _ = testPassword(t)
	if err := password.Set("secret123"); err != nil {
		t.Fatal(err)
	}
	password.PBKDF2 = func([]byte, []byte, int, int) ([]byte, error) {
		return nil, errors.New("no pbkdf2")
	}
	if _, err := password.Verify("secret123"); err == nil {
		t.Fatal("verify hid the crypto failure")
	}
}

func TestPanelSessions(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	sessions := NewSessions(time.Hour)
	sessions.Now = func() time.Time { return now }
	token := sessions.Create()
	if len(token) < 30 || strings.ContainsAny(token, "+/=") {
		t.Fatalf("token = %q", token)
	}
	if !sessions.Valid(token) {
		t.Fatal("fresh token invalid")
	}
	now = now.Add(2 * time.Hour)
	if sessions.Valid(token) {
		t.Fatal("expired token valid")
	}
	token = sessions.Create()
	sessions.Revoke(token)
	if sessions.Valid(token) {
		t.Fatal("revoked token valid")
	}
	first := sessions.Create()
	second := sessions.Create()
	sessions.RevokeAll()
	if sessions.Valid(first) || sessions.Valid(second) {
		t.Fatal("revoke-all left sessions")
	}

	// Header only: query and cookie must never authenticate.
	sessions = NewSessions(time.Hour)
	token = sessions.Create()
	protected := RequireSession(sessions)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	cases := []struct {
		name  string
		setup func(*http.Request)
		want  int
	}{
		{"header", func(r *http.Request) { r.Header.Set("X-Panel-Token", token) }, http.StatusOK},
		{"query", func(r *http.Request) { r.URL.RawQuery = "token=" + token }, http.StatusUnauthorized},
		{"cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "panel_token", Value: token}) }, http.StatusUnauthorized},
		{"missing", func(*http.Request) {}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/settings", nil)
			tc.setup(req)
			rec := httptest.NewRecorder()
			protected.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestPanelLoginRateLimit(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	limiter := NewLoginLimiter()
	limiter.Now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if ok, _ := limiter.Check("1.2.3.4"); !ok {
			t.Fatalf("attempt %d blocked too early", i+1)
		}
		limiter.Fail("1.2.3.4")
	}
	if ok, reset := limiter.Check("1.2.3.4"); ok || reset.IsZero() {
		t.Fatalf("sixth attempt = ok:%v reset:%v", ok, reset)
	}
	if ok, _ := limiter.Check("5.6.7.8"); !ok {
		t.Fatal("one client blocked another")
	}
	limiter.Clear("1.2.3.4")
	if ok, _ := limiter.Check("1.2.3.4"); !ok {
		t.Fatal("success did not clear attempts")
	}

	headers := http.Header{}
	headers.Set("X-Real-IP", "203.0.113.9")
	headers.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1")
	if got := LoginClientKey("127.0.0.1", headers); got != "203.0.113.9" {
		t.Fatalf("loopback client key = %q", got)
	}
	headers.Del("X-Real-IP")
	if got := LoginClientKey("::1", headers); got != "198.51.100.7" {
		t.Fatalf("forwarded client key = %q", got)
	}
	if got := LoginClientKey("192.0.2.10", headers); got != "192.0.2.10" {
		t.Fatalf("untrusted peer used forwarded header: %q", got)
	}
	headers.Set("X-Real-IP", "garbage")
	headers.Set("X-Forwarded-For", "also-garbage")
	if got := LoginClientKey("127.0.0.1", headers); got != "127.0.0.1" {
		t.Fatalf("bad headers did not fall back: %q", got)
	}
}

func TestPanelStatus(t *testing.T) {
	password, _ := testPassword(t)
	sessions := NewSessions(time.Hour)
	token := sessions.Create()
	status := &Status{
		Password:  password,
		Sessions:  sessions,
		APIKeySet: func() bool { return true },
		Raccoon: func() map[string]any {
			return map[string]any{"configured": false, "timeout_seconds": 60}
		},
	}
	view := status.View("")
	if view["panel_password_required"] != true || view["panel_password_is_default"] != true {
		t.Fatalf("fresh status = %#v", view)
	}
	if view["authenticated"] != false || view["crypto_ok"] != true || view["api_key_set"] != true {
		t.Fatalf("status flags = %#v", view)
	}
	if _, ok := view["raccoon"].(map[string]any); !ok {
		t.Fatalf("raccoon section = %#v", view["raccoon"])
	}
	if status.View(token)["authenticated"] != true {
		t.Fatal("authenticated token not reported")
	}

	status.Password.PBKDF2 = func([]byte, []byte, int, int) ([]byte, error) {
		return nil, errors.New("broken")
	}
	view = status.View("")
	if view["crypto_ok"] != false {
		t.Fatalf("crypto_ok = %#v", view["crypto_ok"])
	}
	if _, ok := view["crypto_error"].(string); !ok {
		t.Fatalf("crypto_error = %#v", view["crypto_error"])
	}
}
