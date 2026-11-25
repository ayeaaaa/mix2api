package codearts

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
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

func TestSessionJSONImportPreservesLargeNumericUID(t *testing.T) {
	raw := `{"account":{"id":9007199254740993},"accessKey":"AK","secretKey":"SK","securityToken":"ST","stationType":"international"}`
	session, failure := decodeSessionJSON(raw)
	if failure != nil {
		t.Fatal(failure)
	}
	auth, failure := authFromSessionJSON(session, International)
	if failure != nil {
		t.Fatal(failure)
	}
	if auth.UserID != "9007199254740993" {
		t.Fatalf("uid = %q", auth.UserID)
	}
}

func TestSessionJSONImportKeepsObjectDPoPAlias(t *testing.T) {
	payload := map[string]any{
		"account":   map[string]any{"id": "u1"},
		"accessKey": "AK", "secretKey": "SK", "securityToken": "ST",
		"stationType":      "international",
		"dpop_private_key": map[string]any{"kty": "EC", "d": "secret"},
	}
	auth, failure := authFromSessionJSON(payload, International)
	if failure != nil {
		t.Fatal(failure)
	}
	if !strings.Contains(string(auth.DPopPrivateKey), `"kty":"EC"`) {
		t.Fatalf("dpop = %s", auth.DPopPrivateKey)
	}
}

func TestSessionJSONImportRoutesEnforceStation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		station Station
		marker  string
		status  int
	}{
		{"unmarked-mainland", Mainland, "", http.StatusBadRequest},
		{"unmarked-international", International, "", http.StatusOK},
		{"explicit-mainland", Mainland, `,"stationType":"mainland"`, http.StatusOK},
		{"mainland-alias", Mainland, `,"stationType":"cn"`, http.StatusOK},
		{"mainland-snake-case", Mainland, `,"station_type":"mainland"`, http.StatusOK},
		{"mainland-station", Mainland, `,"station":"mainland"`, http.StatusOK},
		{"blank-mainland", Mainland, `,"stationType":""`, http.StatusBadRequest},
		{"unknown-mainland", Mainland, `,"stationType":"unknown"`, http.StatusBadRequest},
		{"international-on-mainland", Mainland, `,"stationType":"international"`, http.StatusBadRequest},
		{"mainland-on-international", International, `,"stationType":"mainland"`, http.StatusBadRequest},
	} {
		for _, asString := range []bool{false, true} {
			format := "object"
			if asString {
				format = "string"
			}
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				var upstreamCalls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls.Add(1)
					w.WriteHeader(http.StatusBadGateway)
				}))
				t.Cleanup(server.Close)
				t.Setenv("WB_CODEARTS_STS_HOST", server.URL)
				t.Setenv("WB_CODEARTS_SNAP_HOST", server.URL)
				t.Setenv("WB_CODEARTS_SNAP_MANAGER", server.URL+"/snap-manager")
				t.Setenv("WB_CODEARTS_BENEFIT_HOST", server.URL)
				station := tc.station
				station.STS, station.SnapHost = server.URL, server.URL
				store := newTestStore(t)
				p, err := NewWithStation(store, station)
				if err != nil {
					t.Fatal(err)
				}
				mux := http.NewServeMux()
				api.RegisterProviderRoutes(mux, p)
				dir := filepath.Dir(store.Path)
				// Capture all persistence targets before import, including settings
				// and state. A rejected session must not even auto-enable a realm.
				paths := []string{store.Path, filepath.Join(dir, "codearts", "state.json"),
					filepath.Join(dir, "codearts-intl", "state.json")}
				before := make(map[string][]byte)
				for _, path := range paths {
					raw, err := os.ReadFile(path)
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
					before[path] = raw
				}
				rawSession := `{"account":{"id":9007199254740993,"label":"Numeric UID"},"accessKey":"AK-MARKER","secretKey":"SK-MARKER","securityToken":"ST-MARKER","refresh_token":"RT-MARKER","expiresAt":"2020-01-01T00:00:00Z"` + tc.marker + `}`
				var session any = json.RawMessage(rawSession)
				if asString {
					session = rawSession
				}
				body, err := json.Marshal(map[string]any{"session_json": session})
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, "/"+station.Realm+"/accounts/import", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != tc.status {
					t.Errorf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body)
				}
				if upstreamCalls.Load() != 0 {
					t.Fatal("import contacted upstream with a one-use refresh token")
				}
				assertNoSecrets(t, "session import response", rec.Body.String(), "AK-MARKER", "SK-MARKER", "ST-MARKER", "RT-MARKER")
				const uid = "9007199254740993"
				if tc.status == http.StatusBadRequest {
					if len(p.Pool().Accounts()) != 0 || LoadConfigForStation(store, station).Enabled {
						t.Error("rejected session was added or enabled its realm")
					}
					for _, path := range paths {
						after, err := os.ReadFile(path)
						if err != nil && !errors.Is(err, os.ErrNotExist) {
							t.Fatal(err)
						}
						if !bytes.Equal(after, before[path]) {
							t.Errorf("rejected session changed %s", path)
						}
					}
					for _, realm := range []string{"codearts", "codearts-intl"} {
						if _, err := os.Stat(filepath.Join(dir, realm, "codearts-"+uid+".json")); !errors.Is(err, os.ErrNotExist) {
							t.Errorf("rejected session persisted in %s: %v", realm, err)
						}
					}
					return
				}
				if got := routesText(decodeRoutesJSON(t, rec)["uid"]); got != uid {
					t.Fatalf("route rounded numeric uid to %q", got)
				}
				auth, err := LoadAuthForStation(filepath.Join(dir, station.AccountsFolder, "codearts-"+uid+".json"), station)
				if err != nil {
					t.Fatal(err)
				}
				if station.ID == International.ID {
					if auth.Station != International.ID || auth.SecurityToken != "ST-MARKER" || auth.CloudDragonToken != "" {
						t.Fatal("international session lost its station/token separation")
					}
				} else if auth.CloudDragonToken != "ST-MARKER" {
					t.Fatal("explicit mainland session no longer maps its signing token")
				}
			})
		}
	}
}

func TestMainlandLegacyAuthImportWithoutStation(t *testing.T) {
	for name, nested := range map[string]bool{"flat": false, "nested": true} {
		t.Run(name, func(t *testing.T) {
			f := newCodeArtsRoutesFixture(t, false)
			payload := routeAuthPayload(t, testAuth("legacy-mainland"))
			delete(payload, "station")
			if nested {
				payload = map[string]any{"auth": payload}
			}
			rec := f.do(http.MethodPost, "/codearts/accounts/import", payload, "")
			if rec.Code != http.StatusOK || f.p.Pool().Get("legacy-mainland") == nil {
				t.Fatalf("legacy auth import status=%d body=%s", rec.Code, rec.Body)
			}
		})
	}
}
