package raccoon

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func makeUnsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestAuthUIDFromJWT(t *testing.T) {
	const uid = "pu-test"
	tok := makeUnsignedJWT(t, map[string]any{"sub": uid, "exp": 1796616020})

	if got := UIDFromAccessToken(tok); got != uid {
		t.Fatalf("UIDFromAccessToken() = %q, want %q", got, uid)
	}
	if got := UIDFromAccessToken("  Bearer " + tok + "  "); got != uid {
		t.Fatalf("UIDFromAccessToken(Bearer) = %q, want %q", got, uid)
	}

	paddedPayload := base64.URLEncoding.EncodeToString([]byte(`{"sub":"pu-test"}`))
	if got := UIDFromAccessToken("header." + paddedPayload + ".signature"); got != uid {
		t.Fatalf("UIDFromAccessToken(padded) = %q, want %q", got, uid)
	}

	for _, token := range []string{
		"",
		"opaque-token",
		"only.two",
		"header." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".signature",
		"header." + base64.RawURLEncoding.EncodeToString([]byte(`["not","an","object"]`)) + ".signature",
		makeUnsignedJWT(t, map[string]any{"exp": 1796616020}),
		"header.!!!.signature",
	} {
		if got := UIDFromAccessToken(token); got != "" {
			t.Fatalf("UIDFromAccessToken(%q) = %q, want empty", token, got)
		}
	}

	if got := UIDFromAccessToken(makeUnsignedJWT(t, map[string]any{"sub": 123})); got != "123" {
		t.Fatalf("UIDFromAccessToken(numeric sub) = %q, want 123", got)
	}
}

func TestAuthRoundTrip(t *testing.T) {
	tok := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": 1796616020})
	auth := Auth{
		UID:            "pu-test",
		AccessToken:    tok,
		RefreshToken:   "refresh-token-value",
		OfficeIdentity: "personal",
		OfficeOrgName:  "Raccoon Test",
		ExpiresAt:      time.Unix(1796616020, 0).UTC(),
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "accounts", "raccoon", "raccoon-pu-test.json")
	if err := SaveAuth(path, auth); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("auth file mode = %v, want 0600", st.Mode().Perm())
	}
	dirSt, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirSt.Mode().Perm() != 0700 {
		t.Fatalf("auth directory mode = %v, want 0700", dirSt.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"uid", "access_token", "refresh_token", "office_identity", "office_org_name", "expires_at"}
	if len(doc) != len(wantKeys) {
		t.Fatalf("auth JSON keys = %v, want exactly %v", keysOf(doc), wantKeys)
	}
	for _, key := range wantKeys {
		if _, ok := doc[key]; !ok {
			t.Fatalf("auth JSON keys = %v, missing %q", keysOf(doc), key)
		}
	}

	var uid string
	if err := json.Unmarshal(doc["uid"], &uid); err != nil || uid != auth.UID {
		t.Fatalf("stored uid = %q (err %v), want %q", uid, err, auth.UID)
	}
	var storedAccess string
	if err := json.Unmarshal(doc["access_token"], &storedAccess); err != nil || storedAccess != auth.AccessToken {
		t.Fatalf("stored access_token mismatch (err %v)", err)
	}
	var epoch int64
	if err := json.Unmarshal(doc["expires_at"], &epoch); err != nil {
		t.Fatalf("expires_at is not epoch seconds: %v", err)
	}
	if epoch != auth.ExpiresAt.Unix() {
		t.Fatalf("expires_at = %d, want %d", epoch, auth.ExpiresAt.Unix())
	}

	loaded, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, auth) {
		t.Fatalf("LoadAuth() = %#v, want %#v", loaded, auth)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("unexpected leftover files: %v", entries)
	}
}

func TestSaveAuthDerivesUIDAndCleansBearerPrefix(t *testing.T) {
	tok := makeUnsignedJWT(t, map[string]any{"sub": "pu-derived"})
	path := filepath.Join(t.TempDir(), "raccoon-derived.json")
	if err := SaveAuth(path, Auth{AccessToken: "Bearer " + tok}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		UID         string `json:"uid"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.UID != "pu-derived" {
		t.Fatalf("uid = %q, want pu-derived", doc.UID)
	}
	if doc.AccessToken != tok {
		t.Fatalf("access_token was not stripped of Bearer prefix")
	}
}

func TestSaveAuthOpaqueTokenUIDFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raccoon-opaque.json")
	auth := Auth{AccessToken: "opaque-access-token", RefreshToken: "opaque-refresh-token"}
	if err := SaveAuth(path, auth); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loaded.UID, "raccoon-") {
		t.Fatalf("opaque uid = %q, want raccoon- prefix", loaded.UID)
	}
	if len(loaded.UID) != len("raccoon-")+12 {
		t.Fatalf("opaque uid = %q, want 12 hex chars after prefix", loaded.UID)
	}

	if err := SaveAuth(path, auth); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.UID != loaded.UID {
		t.Fatalf("opaque uid changed: %q != %q", reloaded.UID, loaded.UID)
	}
}

func TestSaveAuthRejectsEmptyPath(t *testing.T) {
	tok := makeUnsignedJWT(t, map[string]any{"sub": "pu-test"})
	if err := SaveAuth("   ", Auth{UID: "pu-test", AccessToken: tok}); err == nil {
		t.Fatal("SaveAuth(empty path) error = nil, want error")
	}
}

func TestSaveAuthRejectsEmptyAccessToken(t *testing.T) {
	for _, token := range []string{"", "   ", "Bearer   "} {
		path := filepath.Join(t.TempDir(), "raccoon-pu-test.json")
		err := SaveAuth(path, Auth{
			UID:          "pu-test",
			AccessToken:  token,
			RefreshToken: "refresh-secret-value",
		})
		if err == nil {
			t.Fatalf("SaveAuth(access_token=%q) error = nil, want error", token)
		}
		if strings.Contains(err.Error(), "refresh-secret-value") {
			t.Fatalf("SaveAuth() error leaks a secret: %v", err)
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("SaveAuth() wrote a file for an empty access token: %v", statErr)
		}
	}
}

func TestLoadAuthExpiryFormats(t *testing.T) {
	tok := makeUnsignedJWT(t, map[string]any{"sub": "pu-test"}) // no exp claim
	dir := t.TempDir()
	path := filepath.Join(dir, "raccoon-pu-test.json")
	rfc3339, err := time.Parse(time.RFC3339, "2027-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		value string
		want  time.Time
	}{
		{"seconds", "1796616020", time.Unix(1796616020, 0).UTC()},
		{"milliseconds", "1796616020000", time.Unix(1796616020, 0).UTC()},
		{"fractional", "1796616020.5", time.Unix(1796616020, 500000000).UTC()},
		{"rfc3339", `"2027-01-01T00:00:00Z"`, rfc3339},
		{"numeric string", `"1796616020"`, time.Unix(1796616020, 0).UTC()},
		{"malformed", `"never"`, time.Time{}},
		{"empty string", `""`, time.Time{}},
		{"too large", "999999999999999", time.Time{}},
		{"null", "null", time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"uid":"pu-test","access_token":%q,"refresh_token":"refresh","office_identity":"","office_org_name":"","expires_at":%s}`, tok, tc.value)
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadAuth(path)
			if err != nil {
				t.Fatal(err)
			}
			if !got.ExpiresAt.Equal(tc.want) {
				t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, tc.want)
			}
		})
	}
}

func TestLoadAuthDerivesExpiryFromJWT(t *testing.T) {
	tok := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": 1796616020})
	path := filepath.Join(t.TempDir(), "raccoon-pu-test.json")
	body := fmt.Sprintf(`{"uid":"pu-test","access_token":%q,"refresh_token":"refresh"}`, tok)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(1796616020, 0).UTC()
	if !got.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, want)
	}
}

func TestLoadAuthRejectsInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raccoon-pu-test.json")

	if _, err := LoadAuth(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("LoadAuth(missing) error = nil, want error")
	}

	cases := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"uid":"pu-test","access_token":"`},
		{"missing access token", `{"uid":"pu-test","refresh_token":"refresh"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadAuth(path); err == nil {
				t.Fatal("LoadAuth() error = nil, want error")
			}
		})
	}
}

func TestAuthErrorsDoNotLeakTokens(t *testing.T) {
	const access = "access-secret-value"
	const refresh = "refresh-secret-value"
	dir := t.TempDir()

	malformed := filepath.Join(dir, "raccoon-broken.json")
	if err := os.WriteFile(malformed, []byte(`{"uid":"pu-test","access_token":"`+access+`","refresh_token":"`+refresh+`"`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuth(malformed); err == nil {
		t.Fatal("LoadAuth(malformed) error = nil, want error")
	} else if strings.Contains(err.Error(), access) || strings.Contains(err.Error(), refresh) {
		t.Fatalf("LoadAuth() error leaks a token: %v", err)
	}

	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	err := SaveAuth(filepath.Join(blocker, "raccoon-pu-test.json"), Auth{
		UID:          "pu-test",
		AccessToken:  access,
		RefreshToken: refresh,
	})
	if err == nil {
		t.Fatal("SaveAuth() error = nil, want directory error")
	}
	if strings.Contains(err.Error(), access) || strings.Contains(err.Error(), refresh) {
		t.Fatalf("SaveAuth() error leaks a token: %v", err)
	}
}

func TestAuthV1ExtrasRoundTrip(t *testing.T) {
	const uid = "pu-01a1b125-f8ea-7260-b72c-a04d678ca4e9"
	tok := makeUnsignedJWT(t, map[string]any{"sub": uid, "exp": 1796616020})
	dir := t.TempDir()
	path, err := AuthPath(dir, uid)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "raccoon", "raccoon-"+uid+".json"); path != want {
		t.Fatalf("AuthPath() = %q, want %q", path, want)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{
  "access_token": %q,
  "refresh_token": "refresh-1",
  "token_type": "Bearer",
  "office_identity": "organization",
  "office_org_name": "Raccoon Test Org",
  "office_org_role": "admin",
  "updated_at": 1794024020,
  "uid": %q,
  "proxySlot": "slot-1",
  "future_metadata": {"flags": [1, 2], "note": "keep"},
  "future_counter": 18446744073709551615
}`, tok, uid)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	original := readJSONObject(t, path)

	auth, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if auth.UID != uid {
		t.Fatalf("UID = %q, want %q", auth.UID, uid)
	}
	if auth.TokenType != "Bearer" {
		t.Fatalf("TokenType = %q, want Bearer", auth.TokenType)
	}
	if auth.OfficeOrgRole != "admin" {
		t.Fatalf("OfficeOrgRole = %q, want admin", auth.OfficeOrgRole)
	}
	if auth.ProxySlot != "slot-1" {
		t.Fatalf("ProxySlot = %q, want slot-1", auth.ProxySlot)
	}
	if auth.UpdatedAt != 1794024020 {
		t.Fatalf("UpdatedAt = %d, want 1794024020", auth.UpdatedAt)
	}
	if want := time.Unix(1796616020, 0).UTC(); !auth.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want JWT exp %v", auth.ExpiresAt, want)
	}

	if err := SaveAuth(path, auth); err != nil {
		t.Fatal(err)
	}
	assertOriginalFieldsPreserved(t, original, readJSONObject(t, path))
	if _, ok := readJSONObject(t, path)["expires_at"]; !ok {
		t.Fatal("saved auth is missing expires_at")
	}

	// A second load/save cycle must not lose the unknown fields either.
	reloaded, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ProxySlot != "slot-1" || reloaded.TokenType != "Bearer" ||
		reloaded.OfficeOrgRole != "admin" || reloaded.UpdatedAt != 1794024020 {
		t.Fatalf("reloaded extras = %#v", reloaded)
	}
	if err := SaveAuth(path, reloaded); err != nil {
		t.Fatal(err)
	}
	assertOriginalFieldsPreserved(t, original, readJSONObject(t, path))
}

func TestAuthPreservesEmptyKnownExtras(t *testing.T) {
	tok := makeUnsignedJWT(t, map[string]any{"sub": "pu-test"})
	path := filepath.Join(t.TempDir(), "raccoon-pu-test.json")
	body := fmt.Sprintf(`{"uid":"pu-test","access_token":%q,"token_type":"","office_org_role":"","proxySlot":"","updated_at":0}`, tok)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	original := readJSONObject(t, path)

	auth, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAuth(path, auth); err != nil {
		t.Fatal(err)
	}
	saved := readJSONObject(t, path)
	for _, key := range []string{"token_type", "office_org_role", "proxySlot", "updated_at"} {
		raw, ok := saved[key]
		if !ok {
			t.Fatalf("saved auth dropped empty %q", key)
		}
		if compactJSON(t, raw) != compactJSON(t, original[key]) {
			t.Fatalf("saved %q = %s, want %s", key, raw, original[key])
		}
	}
}

func TestAuthPathSanitizesUID(t *testing.T) {
	base := t.TempDir()
	cases := []struct {
		name     string
		uid      string
		filename string
	}{
		{"simple", "pu-test", "raccoon-pu-test.json"},
		{"traversal", "../../etc/passwd", "raccoon-.._.._etc_passwd.json"},
		{"slashes", `a/b\c`, "raccoon-a_b_c.json"},
		{"unicode", "用户-测试", "raccoon-_-_.json"},
		{"dots", "..", "raccoon-_.json"},
		{"only separators", "///", "raccoon-_.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AuthPath(base, tc.uid)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(base, "raccoon", tc.filename)
			if got != want {
				t.Fatalf("AuthPath() = %q, want %q", got, want)
			}
			rel, err := filepath.Rel(base, got)
			if err != nil {
				t.Fatal(err)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("AuthPath() escaped base: %q", rel)
			}
		})
	}

	for _, uid := range []string{"", "   "} {
		if got, err := AuthPath(base, uid); err == nil {
			t.Fatalf("AuthPath(%q) = %q, want error", uid, got)
		} else if got != "" {
			t.Fatalf("AuthPath(%q) = %q, want empty path on error", uid, got)
		}
	}
}

func TestLoadAuthPrefersJWTExpiry(t *testing.T) {
	jwtExp := int64(1796616020)
	storedExpiresAt := int64(1700000000)
	tok := makeUnsignedJWT(t, map[string]any{"sub": "pu-test", "exp": jwtExp})
	path := filepath.Join(t.TempDir(), "raccoon-pu-test.json")
	body := fmt.Sprintf(`{"uid":"pu-test","access_token":%q,"expires_at":%d}`, tok, storedExpiresAt)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(jwtExp, 0).UTC()
	if !got.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want JWT exp %v", got.ExpiresAt, want)
	}
}

func readJSONObject(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func assertOriginalFieldsPreserved(t *testing.T, original, saved map[string]json.RawMessage) {
	t.Helper()
	for key, raw := range original {
		got, ok := saved[key]
		if !ok {
			t.Fatalf("saved auth dropped %q", key)
		}
		if compactJSON(t, got) != compactJSON(t, raw) {
			t.Fatalf("saved %q = %s, want %s", key, got, raw)
		}
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
