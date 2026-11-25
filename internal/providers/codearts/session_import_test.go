package codearts

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionJSONImportMapsIntlFields(t *testing.T) {
	payload := map[string]any{
		"account":       map[string]any{"id": "acct-1", "label": "u1"},
		"accessKey":     "AK",
		"secretKey":     "SK",
		"securityToken": "ST",
		"refresh_token": "refresh",
		"domainId":      "domain",
		"expiresAt":     "2026-12-01T00:00:00Z",
		"loginContext": map[string]any{
			"pkcePair":    map[string]any{"codeVerifier": "verifier"},
			"dpopKeyPair": map[string]any{"privateKeyJwk": map[string]any{"kty": "EC"}},
		},
		"stationType": "international",
	}
	auth, failure := authFromSessionJSON(payload, International)
	if failure != nil {
		t.Fatalf("failure = %v", failure)
	}
	if auth.UserID != "acct-1" || auth.UserName != "u1" || auth.AccessKeyID != "AK" ||
		auth.SecretAccessKey != "SK" || auth.SecurityToken != "ST" ||
		auth.CloudDragonToken != "" || auth.RefreshToken != "refresh" ||
		auth.DomainID != "domain" || auth.CodeVerifier != "verifier" ||
		auth.Station != "international" || auth.Expiration.IsZero() {
		t.Fatalf("auth = %#v", auth)
	}
	if string(auth.DPopPrivateKey) != `{"kty":"EC"}` {
		t.Fatalf("dpop = %s", auth.DPopPrivateKey)
	}
}

func TestSessionJSONImportUIDFromRefreshJWT(t *testing.T) {
	jwt := "x." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"jwt-uid"}`)) + ".y"
	payload := map[string]any{
		"accessKey": "AK", "secretKey": "SK", "securityToken": "ST",
		"refresh_token": jwt, "stationType": "international",
	}
	auth, failure := authFromSessionJSON(payload, International)
	if failure != nil {
		t.Fatalf("failure = %v", failure)
	}
	if auth.UserID != "jwt-uid" {
		t.Fatalf("uid = %q", auth.UserID)
	}
}

func TestSessionJSONImportRejectsWrongStation(t *testing.T) {
	payload := map[string]any{
		"account":   map[string]any{"id": "u1"},
		"accessKey": "AK", "secretKey": "SK", "securityToken": "ST",
		"stationType": "mainland",
	}
	if _, failure := authFromSessionJSON(payload, International); failure == nil {
		t.Fatal("accepted mainland session for intl")
	}
	payload["stationType"] = "international"
	if _, failure := authFromSessionJSON(payload, Mainland); failure == nil {
		t.Fatal("accepted intl session for mainland")
	}
}

func TestCodeArtsIntlImportHandler(t *testing.T) {
	store := newTestStore(t)
	p, err := NewWithStation(store, International)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"session_json":{"account":{"id":"u1","label":"U1"},"accessKey":"AK","secretKey":"SK","securityToken":"ST","refresh_token":"refresh","stationType":"international"}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/codearts-intl/accounts/import", strings.NewReader(body))
	p.handleImport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	path := filepath.Join(filepath.Dir(store.Path), "codearts-intl", "codearts-u1.json")
	if _, err := LoadAuthForStation(path, International); err != nil {
		t.Fatalf("load imported auth: %v", err)
	}
}

func TestSessionJSONImportAcceptsStringPayload(t *testing.T) {
	raw := `{"account":{"id":"u1"},"accessKey":"AK","secretKey":"SK","securityToken":"ST","stationType":"international"}`
	session, failure := decodeSessionJSON(raw)
	if failure != nil || modelMap(session) == nil {
		t.Fatalf("session=%v failure=%v", session, failure)
	}
	_ = json.Valid([]byte(raw))
}
