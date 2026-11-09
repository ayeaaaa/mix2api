package workbuddy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal JWT segment: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		encode(claims) + ".signature"
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func TestAuthFileRoundTripPreservesV1FieldsAndExtras(t *testing.T) {
	dir := t.TempDir()
	expires := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	added := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	token := makeJWT(t, map[string]any{
		"sub":           "uid-1",
		"iss":           "https://www.workbuddy.ai/auth/realms/copilot",
		"exp":           expires.Unix(),
		"enterprise_id": "ent-from-token",
	})
	auth := Auth{
		UID:                "uid-1",
		Nickname:           "  Nick  ",
		Realm:              realmIntl,
		Domain:             "www.workbuddy.ai",
		AccessToken:        token,
		RefreshToken:       "refresh-token",
		ExpiresAt:          expires,
		Platform:           "CLI",
		EnterpriseID:       "ent-explicit",
		Product:            "cli",
		Enabled:            true,
		TasksEnabled:       true,
		Weight:             3,
		Note:               "keep",
		Group:              "team",
		DisabledModels:     []string{"Model-A", "model-a", " x "},
		ProxySlot:          "slot-1",
		Source:             "oauth",
		AddedAt:            added,
		Tier:               json.RawMessage(`{"paid":true,"package":"Pro"}`),
		LastCheckin:        "2026-11-08 09:00:00",
		LastDailyChat:      "2026-11-08 10:00:00",
		LastActivityReport: "2026-11-08 11:00:00",
		Extra: map[string]json.RawMessage{
			"modelCaps":       json.RawMessage(`{"m":{"at":1,"reset":2}}`),
			"restrictedUntil": json.RawMessage(`123`),
			"futureField":     json.RawMessage(`{"nested":[1,2]}`),
			"lastError":       json.RawMessage(`"stale"`),
			"cooldownUntil":   json.RawMessage(`999`),
		},
	}

	saved, err := SaveAccount(dir, auth)
	if err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	if saved.Nickname != "Nick" {
		t.Fatalf("saved nickname = %q, want trimmed", saved.Nickname)
	}
	if saved.EnterpriseID != "ent-explicit" {
		t.Fatalf("saved enterprise id = %q", saved.EnterpriseID)
	}
	if got, want := saved.DisabledModels, []string{"model-a", "x"}; !equalStrings(got, want) {
		t.Fatalf("saved disabled models = %v, want %v", got, want)
	}

	path := filepath.Join(dir, "uid-1.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved account: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("account mode = %#o, want 0600", info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved account: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("saved account is not a JSON object: %v\n%s", err, data)
	}
	for _, key := range []string{
		"uid", "nickname", "domain", "realm", "platform", "product",
		"enterpriseId", "accessToken", "refreshToken", "expiresAt", "addedAt",
		"source", "apiKey", "proxySlot", "enabled", "tasksEnabled", "weight",
		"note", "group", "disabledModels", "tier", "lastCheckin",
		"lastDailyChat", "lastActivityReport", "modelCaps", "restrictedUntil",
		"futureField",
	} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("saved account is missing %q: %s", key, data)
		}
	}
	for _, key := range []string{"lastError", "cooldownUntil"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("runtime-only field %q was persisted: %s", key, data)
		}
	}
	var expiresNumber json.Number
	if err := json.Unmarshal(raw["expiresAt"], &expiresNumber); err != nil {
		t.Fatalf("expiresAt is not numeric: %s", raw["expiresAt"])
	}
	if got, _ := expiresNumber.Int64(); got != expires.Unix() {
		t.Fatalf("expiresAt = %d, want Unix seconds %d", got, expires.Unix())
	}
	var addedNumber json.Number
	if err := json.Unmarshal(raw["addedAt"], &addedNumber); err != nil {
		t.Fatalf("addedAt is not numeric: %s", raw["addedAt"])
	}
	if got, _ := addedNumber.Int64(); got != added.Unix() {
		t.Fatalf("addedAt = %d, want Unix seconds %d", got, added.Unix())
	}

	loaded, err := LoadAccounts(dir)
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("LoadAccounts returned %d accounts, want 1", len(loaded))
	}
	got := loaded[0]
	if got.UID != auth.UID || got.Nickname != "Nick" || got.AccessToken != token ||
		got.RefreshToken != "refresh-token" || got.Realm != realmIntl ||
		got.Domain != "www.workbuddy.ai" || got.Platform != "CLI" ||
		got.EnterpriseID != "ent-explicit" || got.Product != "cli" {
		t.Fatalf("loaded identity fields = %+v", got)
	}
	if !got.ExpiresAt.Equal(expires) || !got.AddedAt.Equal(added) {
		t.Fatalf("loaded times = %v / %v, want %v / %v",
			got.ExpiresAt, got.AddedAt, expires, added)
	}
	if !got.Enabled || !got.TasksEnabled || got.Weight != 3 ||
		got.Note != "keep" || got.Group != "team" || got.ProxySlot != "slot-1" ||
		got.Source != "oauth" {
		t.Fatalf("loaded state fields = %+v", got)
	}
	if got.LastCheckin != auth.LastCheckin ||
		got.LastDailyChat != auth.LastDailyChat ||
		got.LastActivityReport != auth.LastActivityReport {
		t.Fatalf("loaded activity fields = %+v", got)
	}
	if !jsonEqual(got.Tier, auth.Tier) {
		t.Fatalf("tier = %s, want %s", got.Tier, auth.Tier)
	}
	if got.Extra["modelCaps"] == nil || got.Extra["restrictedUntil"] == nil ||
		got.Extra["futureField"] == nil {
		t.Fatalf("known state/unknown extras were not preserved: %v", got.Extra)
	}
	if _, ok := got.Extra["lastError"]; ok {
		t.Fatal("runtime-only lastError was preserved in memory")
	}
	if _, ok := got.Extra["cooldownUntil"]; ok {
		t.Fatal("runtime-only cooldownUntil was preserved in memory")
	}
	if err := DeleteAccount(dir, auth.UID); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("account file still exists after delete: %v", err)
	}
}

func jsonEqual(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return fmt.Sprint(leftValue) == fmt.Sprint(rightValue)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestAccountFilenameSanitizationAndSafety(t *testing.T) {
	dir := t.TempDir()
	saved, err := SaveAccount(dir, Auth{
		UID:         "a/b c",
		AccessToken: "opaque-token",
		Realm:       realmIntl,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	if want := "a_b_c.json"; saved.Path != filepath.Join(dir, want) {
		t.Fatalf("saved path = %q, want %q", saved.Path, filepath.Join(dir, want))
	}

	random, err := SaveAccount(dir, Auth{
		UID:         "____",
		AccessToken: "opaque-token",
		Realm:       realmIntl,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("SaveAccount random name: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}\.json$`).MatchString(filepath.Base(random.Path)) {
		t.Fatalf("random account filename = %q", filepath.Base(random.Path))
	}
	if filepath.Dir(random.Path) != dir {
		t.Fatalf("random account escaped dir: %q", random.Path)
	}
	if err := DeleteAccount(dir, random.UID); err != nil {
		t.Fatalf("DeleteAccount random filename: %v", err)
	}
	if _, err := os.Stat(random.Path); !os.IsNotExist(err) {
		t.Fatalf("random-named account still exists after delete: %v", err)
	}
}

func TestLoadAccountsSkipsNonAccountFilesAndMalformedRows(t *testing.T) {
	dir := t.TempDir()
	token := makeJWT(t, map[string]any{"sub": "uid-ok", "exp": 4102444800})
	writeJSON(t, filepath.Join(dir, "uid-ok.json"), map[string]any{
		"uid": "uid-ok", "accessToken": token, "realm": "intl", "enabled": true,
	})
	writeJSON(t, filepath.Join(dir, "uid-missing-token.json"), map[string]any{
		"uid": "uid-missing-token", "realm": "intl",
	})
	writeJSON(t, filepath.Join(dir, "uid-jwt-sub.json"), map[string]any{
		"accessToken": makeJWT(t, map[string]any{"sub": "uid-from-jwt"}),
		"enabled":     true,
	})
	for _, name := range []string{
		"settings.json", "active_realm.json", "model_windows.json", "quota_scan.json",
	} {
		writeJSON(t, filepath.Join(dir, name), map[string]any{"uid": name, "accessToken": "x"})
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	accounts, err := LoadAccounts(dir)
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("LoadAccounts returned %d accounts, want 2: %+v", len(accounts), accounts)
	}
	byUID := map[string]Auth{}
	for _, account := range accounts {
		byUID[account.UID] = account
	}
	if _, ok := byUID["uid-ok"]; !ok {
		t.Fatalf("valid account missing: %+v", accounts)
	}
	if _, ok := byUID["uid-from-jwt"]; !ok {
		t.Fatalf("JWT sub fallback uid missing: %+v", accounts)
	}
	for _, skipped := range []string{"settings.json", "active_realm.json", "model_windows.json", "quota_scan.json"} {
		if _, ok := byUID[skipped]; ok {
			t.Fatalf("non-account file %q was loaded", skipped)
		}
	}

	missingDir, err := LoadAccounts(filepath.Join(dir, "missing"))
	if err != nil || len(missingDir) != 0 {
		t.Fatalf("missing dir = %v, %v; want empty, nil", missingDir, err)
	}
}

func TestAPIKeyDetectionAndAccountShape(t *testing.T) {
	if !IsAPIKeyToken("ck_sampleid123.samplesecret456") ||
		!IsAPIKeyToken("pt_xyz.abc") {
		t.Fatal("ck_/pt_ tokens must be API keys")
	}
	for _, token := range []string{"", "sk_live", "ck", "CK_upper.case", "a.b.c"} {
		if IsAPIKeyToken(token) {
			t.Fatalf("%q was detected as an API key", token)
		}
	}
	if got := APIKeyID("ck_a/../b.c"); got != "a" {
		t.Fatalf("APIKeyID = %q, want %q", got, "a")
	}

	dir := t.TempDir()
	key := "ck_sampleid123.samplesecret456"
	saved, err := SaveAccount(dir, Auth{
		AccessToken: key,
		Realm:       realmIntl,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	if saved.UID != "apikey-sampleid123" || saved.Realm != realmCN ||
		!saved.APIKey || saved.Source != "api-key" || !saved.ExpiresAt.IsZero() {
		t.Fatalf("API key account = %+v", saved)
	}
	loaded, err := LoadAccounts(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadAccounts = %+v, %v", loaded, err)
	}
	if loaded[0].UID != saved.UID || !loaded[0].APIKey || loaded[0].Realm != realmCN {
		t.Fatalf("loaded API key account = %+v", loaded[0])
	}
}

func TestDetectRealmFromTokenAndDomainNormalization(t *testing.T) {
	cnToken := makeJWT(t, map[string]any{"iss": "https://www.workbuddy.cn/auth/realms/copilot"})
	cnLegacy := makeJWT(t, map[string]any{"iss": "https://www.codebuddy.cn/auth/realms/copilot"})
	cnTencent := makeJWT(t, map[string]any{"iss": "https://copilot.tencent.com/auth/realms/copilot"})
	intlToken := makeJWT(t, map[string]any{"iss": "https://www.workbuddy.ai/auth/realms/copilot"})
	opaque := makeJWT(t, map[string]any{"iss": "https://example.invalid/realms/x"})

	for _, token := range []string{cnToken, cnLegacy, cnTencent} {
		if got := DetectRealmFromToken(token, ""); got != realmCN {
			t.Fatalf("CN issuer realm = %q", got)
		}
	}
	if got := DetectRealmFromToken(intlToken, ""); got != realmIntl {
		t.Fatalf("intl issuer realm = %q", got)
	}
	for _, domain := range []string{"www.workbuddy.cn", "www.codebuddy.cn", "copilot.tencent.com"} {
		if got := DetectRealmFromToken(opaque, domain); got != realmCN {
			t.Fatalf("domain %q realm = %q", domain, got)
		}
	}
	if got := DetectRealmFromToken(opaque, "www.workbuddy.ai"); got != realmIntl {
		t.Fatalf("intl domain realm = %q", got)
	}
	if got := DetectRealmFromToken("opaque", ""); got != realmIntl {
		t.Fatalf("opaque default realm = %q", got)
	}
	if got := DetectRealmFromToken("ck_a.b", "www.workbuddy.ai"); got != realmCN {
		t.Fatalf("API key realm = %q", got)
	}

	if got := DomainForRealm(realmCN, "www.workbuddy.ai"); got != "copilot.tencent.com" {
		t.Fatalf("wrong-region domain = %q", got)
	}
	if got := DomainForRealm(realmIntl, "www.codebuddy.cn"); got != "www.workbuddy.ai" {
		t.Fatalf("wrong-region intl domain = %q", got)
	}
	if got := DomainForRealm(realmCN, "www.codebuddy.cn"); got != "www.codebuddy.cn" {
		t.Fatalf("matching domain was replaced: %q", got)
	}
	if got := DomainForRealm(realmIntl, "example.test"); got != "example.test" {
		t.Fatalf("unknown domain was replaced: %q", got)
	}
}

func TestLoadAccountUsesExplicitV1StateFields(t *testing.T) {
	dir := t.TempDir()
	token := makeJWT(t, map[string]any{"sub": "uid-state"})
	writeJSON(t, filepath.Join(dir, "uid-state.json"), map[string]any{
		"uid":                "uid-state",
		"accessToken":        token,
		"realm":              "cn",
		"domain":             "",
		"platform":           "",
		"product":            "vscode",
		"enabled":            false,
		"tasksEnabled":       true,
		"weight":             500,
		"note":               strings.Repeat("n", 250),
		"group":              strings.Repeat("g", 70),
		"disabledModels":     []any{" A ", "a", 7, ""},
		"source":             "",
		"expiresAt":          float64(4102444800) * 1000,
		"addedAt":            float64(1),
		"unknownState":       map[string]any{"keep": true},
		"lastError":          "must be dropped",
		"cooldownUntil":      123,
		"lastCheckin":        "2026-11-08 09:00:00",
		"lastActivityReport": "2026-11-08 10:00:00",
	})
	accounts, err := LoadAccounts(dir)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("LoadAccounts = %+v, %v", accounts, err)
	}
	got := accounts[0]
	if got.Enabled || !got.TasksEnabled {
		t.Fatalf("enabled/tasks = %v/%v, want false/true", got.Enabled, got.TasksEnabled)
	}
	if got.Weight != 100 {
		t.Fatalf("weight = %d, want clamped 100", got.Weight)
	}
	if len(got.Note) != 200 || len(got.Group) != 64 {
		t.Fatalf("note/group lengths = %d/%d", len(got.Note), len(got.Group))
	}
	if !equalStrings(got.DisabledModels, []string{"a"}) {
		t.Fatalf("disabled models = %v", got.DisabledModels)
	}
	if got.Product != productVSCode {
		t.Fatalf("product = %q", got.Product)
	}
	if got.Domain != "copilot.tencent.com" {
		t.Fatalf("domain = %q", got.Domain)
	}
	if got.Source != "oauth" {
		t.Fatalf("source = %q", got.Source)
	}
	if got.ExpiresAt.Unix() != 4102444800 {
		t.Fatalf("expiresAt = %v, want millisecond conversion", got.ExpiresAt)
	}
	if got.Extra["unknownState"] == nil {
		t.Fatalf("unknown state was not preserved: %v", got.Extra)
	}
	if _, ok := got.Extra["lastError"]; ok {
		t.Fatal("stale lastError was retained")
	}
}

func TestResolveDeviceTokenLiteralFileCacheAndLimit(t *testing.T) {
	dir := t.TempDir()
	if got, err := ResolveDeviceToken(dir); err != nil || got != "" {
		t.Fatalf("no config = %q, %v", got, err)
	}
	writeSettings(t, dir, map[string]any{
		"upstream": map[string]any{"device_token": "  tok-literal  "},
	})
	if got, err := ResolveDeviceToken(dir); err != nil || got != "tok-literal" {
		t.Fatalf("literal token = %q, %v", got, err)
	}

	tokenFile := filepath.Join(dir, "device_token")
	if err := os.WriteFile(tokenFile, []byte("  tok-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, dir, map[string]any{
		"upstream": map[string]any{"device_token_file": tokenFile},
	})
	resetDeviceTokenCache()
	oldClock := deviceTokenClock
	now := time.Unix(1000, 0)
	deviceTokenClock = func() time.Time { return now }
	defer func() { deviceTokenClock = oldClock; resetDeviceTokenCache() }()

	if got, _ := ResolveDeviceToken(dir); got != "tok-file" {
		t.Fatalf("file token = %q", got)
	}
	if err := os.WriteFile(tokenFile, []byte("tok-rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(1299, 0)
	if got, _ := ResolveDeviceToken(dir); got != "tok-file" {
		t.Fatalf("cache inside TTL = %q", got)
	}
	now = time.Unix(1301, 0)
	if got, _ := ResolveDeviceToken(dir); got != "tok-rotated" {
		t.Fatalf("cache after TTL = %q", got)
	}

	writeSettings(t, dir, map[string]any{
		"upstream": map[string]any{"device_token": "tok-literal", "device_token_file": tokenFile},
	})
	resetDeviceTokenCache()
	if got, _ := ResolveDeviceToken(dir); got != "tok-literal" {
		t.Fatalf("literal should win over file: %q", got)
	}

	oversized := filepath.Join(dir, "oversized")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", 1025)), 0600); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, dir, map[string]any{
		"upstream": map[string]any{"device_token_file": oversized},
	})
	resetDeviceTokenCache()
	if got, _ := ResolveDeviceToken(dir); got != "" {
		t.Fatalf("oversized token file = %q, want empty", got)
	}
	writeSettings(t, dir, map[string]any{
		"upstream": map[string]any{"device_token_file": filepath.Join(dir, "missing")},
	})
	resetDeviceTokenCache()
	if got, _ := ResolveDeviceToken(dir); got != "" {
		t.Fatalf("missing token file = %q, want empty", got)
	}
}

func writeSettings(t *testing.T, dir string, value map[string]any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func resetDeviceTokenCache() {
	deviceTokenCache.mu.Lock()
	deviceTokenCache.key = ""
	deviceTokenCache.token = ""
	deviceTokenCache.at = time.Time{}
	deviceTokenCache.mu.Unlock()
}

func TestFetchProfileMockHTTP(t *testing.T) {
	token := "tok-profile"
	var requestPath, authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		authorization = r.Header.Get("Authorization")
		writeHTTPJSON(w, map[string]any{
			"uid":         "uid-1",
			"nickname":    "  New Nick  ",
			"phoneNumber": "+86-13800000000",
			"email":       "secret@example.com",
		})
	}))
	defer server.Close()

	account := &Auth{UID: "uid-1", Realm: realmCN, AccessToken: token}
	if err := FetchProfileAt(context.Background(), server.Client(), server.URL, account); err != nil {
		t.Fatalf("FetchProfileAt: %v", err)
	}
	if requestPath != "/console/account" {
		t.Fatalf("profile path = %q", requestPath)
	}
	if authorization != "Bearer "+token {
		t.Fatalf("authorization = %q", authorization)
	}
	if account.Nickname != "New Nick" {
		t.Fatalf("nickname = %q", account.Nickname)
	}

	mismatch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHTTPJSON(w, map[string]any{"uid": "uid-other", "nickname": "X"})
	}))
	defer mismatch.Close()
	account = &Auth{UID: "uid-1", Realm: realmIntl, AccessToken: token}
	if err := FetchProfileAt(context.Background(), mismatch.Client(), mismatch.URL, account); err == nil {
		t.Fatal("uid mismatch was accepted")
	}
	if account.Nickname != "" {
		t.Fatalf("mismatch mutated nickname: %q", account.Nickname)
	}

	notObject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `["not","an","object"]`)
	}))
	defer notObject.Close()
	account = &Auth{UID: "uid-1", Realm: realmIntl, AccessToken: token}
	if err := FetchProfileAt(context.Background(), notObject.Client(), notObject.URL, account); err == nil {
		t.Fatal("non-object profile was accepted")
	}
}

func writeHTTPJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func TestScanDesktopCredentialsPlainAndEncrypted(t *testing.T) {
	dir := t.TempDir()
	intlToken := makeJWT(t, map[string]any{
		"sub": "uid-intl", "iss": "https://www.workbuddy.ai/auth/realms/copilot", "exp": 4102444800,
	})
	cnToken := makeJWT(t, map[string]any{
		"sub": "uid-cn", "iss": "https://www.workbuddy.cn/auth/realms/copilot", "exp": 4102444800,
	})
	writeJSON(t, filepath.Join(dir, "workbuddy-desktop-ai.info"), map[string]any{
		"auth": map[string]any{
			"accessToken": intlToken, "refreshToken": "refresh-intl",
			"domain": "www.workbuddy.ai", "expiresAt": 4102444800000,
		},
		"account": map[string]any{"uid": "uid-intl", "nickname": " Intl Desk ", "enterpriseId": "ent-1"},
	})
	// The filename hints intl, but the CN issuer is authoritative.
	writeJSON(t, filepath.Join(dir, "workbuddy-desktop.info"), map[string]any{
		"auth": map[string]any{
			"accessToken": cnToken, "refreshToken": "refresh-cn",
			"domain": "www.workbuddy.cn", "expiresAt": 4102444800000,
		},
		"account": map[string]any{"uid": "uid-cn", "nickname": " Cn Desk "},
	})
	encryptedDir := filepath.Join(dir, "encrypted")
	if err := os.MkdirAll(encryptedDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(encryptedDir, "workbuddy-desktop-ai.info"), map[string]any{
		"auth": map[string]any{
			"accessToken": map[string]any{"$wbEncrypted": 1, "envelope": "ciphertext"},
		},
		"account": map[string]any{"uid": "uid-secret"},
	})
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-desktop-broken.info"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}

	metadata, err := InspectDesktopCredentialsIn([]string{dir, encryptedDir})
	if err != nil {
		t.Fatalf("InspectDesktopCredentialsIn: %v", err)
	}
	if len(metadata) != 3 {
		t.Fatalf("metadata entries = %d, want 3 (known .info files only): %+v", len(metadata), metadata)
	}
	byPath := map[string]DesktopCredential{}
	for _, entry := range metadata {
		byPath[entry.Path] = entry
		encoded, _ := json.Marshal(entry)
		for _, secret := range []string{intlToken, cnToken, "ciphertext"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("metadata leaked a secret: %s", encoded)
			}
		}
	}
	if entry := byPath[filepath.Join(dir, "workbuddy-desktop-ai.info")]; !entry.Valid || entry.UID != "uid-intl" ||
		entry.Nickname != "Intl Desk" || entry.Realm != realmIntl || entry.Encrypted {
		t.Fatalf("intl metadata = %+v", entry)
	}
	if entry := byPath[filepath.Join(dir, "workbuddy-desktop.info")]; !entry.Valid || entry.Realm != realmCN ||
		entry.Domain != "www.workbuddy.cn" {
		t.Fatalf("realm override metadata = %+v", entry)
	}
	if entry := byPath[filepath.Join(encryptedDir, "workbuddy-desktop-ai.info")]; !entry.Encrypted ||
		!entry.NeedsKey || entry.Valid || entry.Error == "" {
		t.Fatalf("encrypted metadata = %+v", entry)
	}

	scanned, err := ScanDesktopCredentialsIn([]string{dir, encryptedDir})
	if err != nil {
		t.Fatalf("ScanDesktopCredentialsIn: %v", err)
	}
	if len(scanned) != 2 {
		t.Fatalf("scanned credentials = %d, want 2 plaintext entries: %+v", len(scanned), scanned)
	}
	for _, auth := range scanned {
		if auth.Source != "desktop-app" || auth.Platform != "CLI" || !auth.Enabled ||
			auth.AccessToken == "" || auth.UID == "" {
			t.Fatalf("scanned auth = %+v", auth)
		}
	}
	if scanned[0].UID == "uid-intl" {
		if scanned[0].Realm != realmIntl || scanned[0].Nickname != "Intl Desk" ||
			scanned[0].RefreshToken != "refresh-intl" || scanned[0].EnterpriseID != "ent-1" {
			t.Fatalf("intl scanned auth = %+v", scanned[0])
		}
	} else if scanned[0].UID != "uid-cn" || scanned[0].Realm != realmCN {
		t.Fatalf("unexpected scanned auth = %+v", scanned[0])
	}

	accountsDir := filepath.Join(dir, "accounts")
	imported, err := ImportDesktopCredential(accountsDir, filepath.Join(dir, "workbuddy-desktop.info"), "")
	if err != nil {
		t.Fatalf("ImportDesktopCredential: %v", err)
	}
	if imported.UID != "uid-cn" || imported.Realm != realmCN || imported.Source != "desktop-app" {
		t.Fatalf("imported desktop auth = %+v", imported)
	}
	if imported.Domain != "www.workbuddy.cn" {
		t.Fatalf("imported domain = %q", imported.Domain)
	}
	loaded, err := LoadAccounts(accountsDir)
	if err != nil || len(loaded) != 1 || loaded[0].AccessToken != cnToken {
		t.Fatalf("loaded imported desktop account = %+v, %v", loaded, err)
	}

	if _, err := ImportDesktopCredential(accountsDir,
		filepath.Join(encryptedDir, "workbuddy-desktop-ai.info"), ""); err == nil {
		t.Fatal("encrypted desktop credential was imported without a key")
	}
}

func TestImportAccountsDocumentFormsDuplicatesAndRealmRules(t *testing.T) {
	dir := t.TempDir()
	cnToken := makeJWT(t, map[string]any{
		"sub": "uid-import-cn", "iss": "https://www.workbuddy.cn/auth/realms/copilot", "exp": 4102444800,
	})
	intlToken := makeJWT(t, map[string]any{
		"sub": "uid-import-intl", "iss": "https://www.workbuddy.ai/auth/realms/copilot", "exp": 4102444800,
	})
	doc := map[string]any{
		"format": "workbuddy-accounts",
		"accounts": []any{
			map[string]any{
				"uid": "uid-import-cn", "accessToken": cnToken,
				"realm": "intl", "domain": "www.workbuddy.ai",
				"note": "imported note", "group": "Team C", "weight": 5,
				"disabledModels": []any{"Model-A"}, "tasksEnabled": false,
				"source": "import", "unknownImportState": map[string]any{"keep": true},
			},
			map[string]any{
				"accessToken": "ck_importid.secret",
			},
			map[string]any{
				"uid": "uid-import-cn", "accessToken": intlToken,
			},
			map[string]any{"uid": "broken", "accessToken": "not-a-jwt"},
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ImportAccounts(dir, data, "", false)
	if err != nil {
		t.Fatalf("ImportAccounts: %v", err)
	}
	if !equalStrings(result.Added, []string{"uid-import-cn", "apikey-importid"}) {
		t.Fatalf("added = %v", result.Added)
	}
	if len(result.Updated) != 0 || len(result.Skipped) != 1 || len(result.Invalid) != 1 {
		t.Fatalf("import result = %+v", result)
	}
	if result.Skipped[0].UID != "uid-import-cn" || !strings.Contains(result.Skipped[0].Reason, "duplicate") {
		t.Fatalf("duplicate skip = %+v", result.Skipped[0])
	}
	if result.Invalid[0].Index != 4 {
		t.Fatalf("invalid index = %d, want 4", result.Invalid[0].Index)
	}

	accounts, err := LoadAccounts(dir)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("LoadAccounts = %+v, %v", accounts, err)
	}
	byUID := map[string]Auth{}
	for _, account := range accounts {
		byUID[account.UID] = account
	}
	cn := byUID["uid-import-cn"]
	if cn.Realm != realmCN || cn.Domain != "copilot.tencent.com" {
		t.Fatalf("token evidence did not correct the stale realm: %+v", cn)
	}
	if cn.Note != "imported note" || cn.Group != "Team C" || cn.Weight != 5 ||
		cn.TasksEnabled || !equalStrings(cn.DisabledModels, []string{"model-a"}) ||
		cn.Extra["unknownImportState"] == nil {
		t.Fatalf("imported operator state = %+v", cn)
	}
	key := byUID["apikey-importid"]
	if !key.APIKey || key.Realm != realmCN || key.Source != "api-key" ||
		!key.ExpiresAt.IsZero() || !key.Enabled {
		t.Fatalf("imported API key = %+v", key)
	}

	second, err := ImportAccounts(dir, data, "", false)
	if err != nil {
		t.Fatalf("second ImportAccounts: %v", err)
	}
	if len(second.Added) != 0 || len(second.Updated) != 0 || len(second.Skipped) != 3 {
		t.Fatalf("second import = %+v", second)
	}

	update := map[string]any{"accounts": []any{map[string]any{
		"uid": "uid-import-cn", "accessToken": cnToken,
		"realm": "cn", "note": "updated note", "tasksEnabled": true,
	}}}
	updateData, _ := json.Marshal(update)
	overwritten, err := ImportAccounts(dir, updateData, "", true)
	if err != nil {
		t.Fatalf("overwrite ImportAccounts: %v", err)
	}
	if !equalStrings(overwritten.Updated, []string{"uid-import-cn"}) {
		t.Fatalf("overwrite result = %+v", overwritten)
	}
	accounts, _ = LoadAccounts(dir)
	for _, account := range accounts {
		if account.UID == "uid-import-cn" {
			if account.Note != "updated note" || !account.TasksEnabled {
				t.Fatalf("overwritten state = %+v", account)
			}
			if account.Extra["unknownImportState"] == nil {
				t.Fatalf("overwrite dropped preserved extras: %v", account.Extra)
			}
		}
	}

	listData, _ := json.Marshal([]any{map[string]any{
		"uid": "uid-list", "access_token": intlToken, "expires_at": 4102444800000,
	}})
	listResult, err := ImportAccounts(dir, listData, "", false)
	if err != nil || len(listResult.Added) != 1 {
		t.Fatalf("bare list import = %+v, %v", listResult, err)
	}
	if len(listResult.Invalid) != 0 || len(listResult.Skipped) != 0 {
		t.Fatalf("bare list import issues: %+v", listResult)
	}

	if _, err := ImportAccounts(dir, []byte("{"), "", false); err == nil {
		t.Fatal("malformed import document was accepted")
	}
	if _, err := ImportAccounts(dir, []byte(`{"nope":1}`), "", false); err == nil {
		t.Fatal("wrong-shape import document was accepted")
	}
}

func TestAPIKeyExpiryForcedZeroOnLoadAndSave(t *testing.T) {
	dir := t.TempDir()
	key := "ck_expiryid.secret"
	future := time.Now().Add(24 * time.Hour)
	saved, err := SaveAccount(dir, Auth{
		AccessToken: key,
		ExpiresAt:   future,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("SaveAccount API key: %v", err)
	}
	if !saved.ExpiresAt.IsZero() {
		t.Fatalf("saved API-key expiry = %v, want zero", saved.ExpiresAt)
	}
	data, err := os.ReadFile(saved.Path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var expiry int64
	if err := json.Unmarshal(raw["expiresAt"], &expiry); err != nil || expiry != 0 {
		t.Fatalf("stored API-key expiresAt = %s, want 0", raw["expiresAt"])
	}

	// A hand-edited/imported file with a stale non-zero expiry must still load
	// as a never-expiring API-key account.
	writeJSON(t, filepath.Join(dir, "apikey-manual.json"), map[string]any{
		"uid":         "apikey-expiryid",
		"accessToken": key,
		"realm":       "cn",
		"expiresAt":   future.Unix(),
		"enabled":     true,
	})
	loaded, err := LoadAccount(filepath.Join(dir, "apikey-manual.json"))
	if err != nil {
		t.Fatalf("LoadAccount manual API key: %v", err)
	}
	if !loaded.APIKey || !loaded.ExpiresAt.IsZero() {
		t.Fatalf("loaded API-key expiry = %+v", loaded)
	}
}

func TestLoadAccountsAllowsRefreshOnlyAccount(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "uid-refresh.json"), map[string]any{
		"uid":          "uid-refresh",
		"refreshToken": "refresh-only-token",
		"realm":        "cn",
		"domain":       "copilot.tencent.com",
		"enabled":      false,
		"tasksEnabled": true,
	})
	accounts, err := LoadAccounts(dir)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("LoadAccounts = %+v, %v", accounts, err)
	}
	got := accounts[0]
	if got.AccessToken != "" || got.RefreshToken != "refresh-only-token" ||
		got.UID != "uid-refresh" || got.Realm != realmCN || got.Enabled {
		t.Fatalf("refresh-only account = %+v", got)
	}
	saved, err := SaveAccount(dir, got)
	if err != nil {
		t.Fatalf("SaveAccount refresh-only: %v", err)
	}
	if saved.AccessToken != "" || saved.RefreshToken != "refresh-only-token" {
		t.Fatalf("saved refresh-only account = %+v", saved)
	}
}

func TestAuthEnabledTaskDefaultsAndExplicitFalse(t *testing.T) {
	dir := t.TempDir()

	defaulted, err := SaveAccount(dir, Auth{
		UID: "uid-default-flags", AccessToken: "opaque-token", Realm: realmIntl,
	})
	if err != nil {
		t.Fatalf("SaveAccount default flags: %v", err)
	}
	if !defaulted.Enabled || !defaulted.TasksEnabled {
		t.Fatalf("unflagged defaults = %+v, want enabled/tasks true", defaulted)
	}

	explicit, err := SaveAccount(dir, Auth{
		UID: "uid-explicit-off", AccessToken: "opaque-token", Realm: realmIntl,
	})
	if err != nil {
		t.Fatalf("SaveAccount setup: %v", err)
	}
	explicit.SetEnabled(false)
	explicit.SetTasksEnabled(false)
	explicit, err = SaveAccount(dir, explicit)
	if err != nil {
		t.Fatalf("SaveAccount explicit off: %v", err)
	}
	if explicit.Enabled || explicit.TasksEnabled || !explicit.EnabledSet || !explicit.TasksEnabledSet {
		t.Fatalf("explicit false flags = %+v", explicit)
	}
	data, err := os.ReadFile(explicit.Path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["enabled"]) != "false" || string(raw["tasksEnabled"]) != "false" {
		t.Fatalf("explicit false not persisted: enabled=%s tasks=%s",
			raw["enabled"], raw["tasksEnabled"])
	}

	loaded, err := LoadAccount(explicit.Path)
	if err != nil {
		t.Fatalf("LoadAccount explicit off: %v", err)
	}
	if loaded.Enabled || loaded.TasksEnabled ||
		!loaded.EnabledSet || !loaded.TasksEnabledSet {
		t.Fatalf("loaded explicit false = %+v", loaded)
	}

	// v1 files with enabled=false but no tasksEnabled follow enabled.
	writeJSON(t, filepath.Join(dir, "uid-v1-off.json"), map[string]any{
		"uid": "uid-v1-off", "accessToken": "opaque-token",
		"realm": "intl", "enabled": false,
	})
	v1, err := LoadAccount(filepath.Join(dir, "uid-v1-off.json"))
	if err != nil {
		t.Fatalf("LoadAccount v1 off: %v", err)
	}
	if v1.Enabled || v1.TasksEnabled || !v1.EnabledSet || v1.TasksEnabledSet {
		t.Fatalf("v1 explicit disabled = %+v", v1)
	}
	v1, err = SaveAccount(dir, v1)
	if err != nil {
		t.Fatalf("SaveAccount v1 off: %v", err)
	}
	if v1.Enabled || v1.TasksEnabled {
		t.Fatalf("v1 explicit disabled became enabled: %+v", v1)
	}

	// Missing fields default to enabled/tasks on both load and save.
	writeJSON(t, filepath.Join(dir, "uid-missing-flags.json"), map[string]any{
		"uid": "uid-missing-flags", "accessToken": "opaque-token", "realm": "intl",
	})
	missing, err := LoadAccount(filepath.Join(dir, "uid-missing-flags.json"))
	if err != nil {
		t.Fatalf("LoadAccount missing flags: %v", err)
	}
	if !missing.Enabled || !missing.TasksEnabled ||
		missing.EnabledSet || missing.TasksEnabledSet {
		t.Fatalf("missing flags = %+v", missing)
	}
	missing, err = SaveAccount(dir, missing)
	if err != nil {
		t.Fatalf("SaveAccount missing flags: %v", err)
	}
	if !missing.Enabled || !missing.TasksEnabled {
		t.Fatalf("saved missing flags = %+v", missing)
	}

	// Tasks can be independent of API dispatch.
	tasksOnly, err := SaveAccount(dir, Auth{
		UID: "uid-tasks-only", AccessToken: "opaque-token", Realm: realmIntl,
	})
	if err != nil {
		t.Fatal(err)
	}
	tasksOnly.SetEnabled(false)
	tasksOnly.SetTasksEnabled(true)
	tasksOnly, err = SaveAccount(dir, tasksOnly)
	if err != nil {
		t.Fatal(err)
	}
	if tasksOnly.Enabled || !tasksOnly.TasksEnabled {
		t.Fatalf("tasks-only flags = %+v", tasksOnly)
	}
}

func TestDeleteAccountRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "uid-dir.json")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAccount(dir, "uid-dir"); err == nil {
		t.Fatal("DeleteAccount removed or ignored a directory path")
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory disappeared after refused delete: %v / %v", info, err)
	}
}

type recordingProxyDoer struct {
	client HTTPDoer
	proxy  string
}

func (d *recordingProxyDoer) Do(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
	d.proxy = proxyURL
	return d.client.Do(req)
}

func TestFetchProfileProxySeamAndEmptyNicknamePreserve(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHTTPJSON(w, map[string]any{"uid": "uid-1", "nickname": ""})
	}))
	defer server.Close()

	proxyURL := "http://user:secret@127.0.0.1:8080"
	doer := &recordingProxyDoer{client: server.Client()}
	account := &Auth{UID: "uid-1", Nickname: "Keep Me", Realm: realmIntl, AccessToken: "tok"}
	if err := FetchProfileWithProxy(context.Background(), doer, server.URL, account, proxyURL); err != nil {
		t.Fatalf("FetchProfileWithProxy: %v", err)
	}
	if doer.proxy != proxyURL {
		t.Fatalf("profile proxy = %q, want %q", doer.proxy, proxyURL)
	}
	if account.Nickname != "Keep Me" {
		t.Fatalf("empty profile nickname cleared the stored nickname: %q", account.Nickname)
	}

	// FetchProfile uses the runtime Auth.ProxyURL seam resolved by the pool.
	oldTransport := defaultProfileTransport
	oldOrigin := profileOriginFor
	defaultProfileTransport = doer
	profileOriginFor = func(string) string { return server.URL }
	defer func() {
		defaultProfileTransport = oldTransport
		profileOriginFor = oldOrigin
	}()
	doer.proxy = ""
	account = &Auth{
		UID: "uid-1", Nickname: "Keep Me", Realm: realmIntl,
		AccessToken: "tok", ProxyURL: proxyURL,
	}
	if err := FetchProfile(context.Background(), account); err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	if doer.proxy != proxyURL {
		t.Fatalf("FetchProfile ignored Auth.ProxyURL: %q", doer.proxy)
	}
	if account.Nickname != "Keep Me" {
		t.Fatalf("FetchProfile cleared nickname: %q", account.Nickname)
	}
}

func TestImportCockpitSourceAndNonObjectRowAborts(t *testing.T) {
	dir := t.TempDir()
	token := makeJWT(t, map[string]any{"sub": "uid-cockpit", "exp": 4102444800})
	doc := []any{
		map[string]any{
			"uid": "uid-cockpit", "access_token": token,
			"refresh_token": "r.r.r", "expires_at": 4102444800,
			"nickname": "Cockpit User",
		},
	}
	data, _ := json.Marshal(doc)
	result, err := ImportAccounts(dir, data, "", false)
	if err != nil || len(result.Added) != 1 {
		t.Fatalf("cockpit import = %+v, %v", result, err)
	}
	accounts, err := LoadAccounts(dir)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("LoadAccounts = %+v, %v", accounts, err)
	}
	if accounts[0].Source != sourceCockpit || accounts[0].RefreshToken != "r.r.r" {
		t.Fatalf("cockpit row = %+v", accounts[0])
	}

	bad, _ := json.Marshal([]any{
		map[string]any{"uid": "uid-ok", "accessToken": token},
		"not-an-object",
	})
	if _, err := ImportAccounts(dir, bad, "", false); err == nil {
		t.Fatal("non-object import row did not abort the document")
	}
}

func TestReservedAndArbitraryFilesCannotBeOverwrittenOrDeleted(t *testing.T) {
	dir := t.TempDir()
	reserved := []string{"settings", "active_realm", "model_windows", "quota_scan"}
	sentinel := []byte(`{"sentinel":"keep","upstream":{"device_token":"keep"}}`)
	for _, base := range reserved {
		if err := os.WriteFile(filepath.Join(dir, base+".json"), sentinel, 0600); err != nil {
			t.Fatal(err)
		}
	}

	token := makeJWT(t, map[string]any{"sub": "ignored", "exp": 4102444800})
	for _, base := range reserved {
		path := filepath.Join(dir, base+".json")
		_, err := SaveAccount(dir, Auth{
			UID:         base,
			AccessToken: token,
			Enabled:     true,
			Extra: map[string]json.RawMessage{
				"proxy_slots": json.RawMessage(`[{"id":"evil"}]`),
			},
		})
		if err == nil {
			t.Fatalf("SaveAccount accepted reserved uid %q", base)
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, sentinel) {
			t.Fatalf("SaveAccount modified reserved file %q: %s", base, got)
		}

		if err := DeleteAccount(dir, base); err == nil {
			t.Fatalf("DeleteAccount accepted reserved uid %q", base)
		}
		got, _ = os.ReadFile(path)
		if !bytes.Equal(got, sentinel) {
			t.Fatalf("DeleteAccount modified reserved file %q: %s", base, got)
		}

		doc, _ := json.Marshal(map[string]any{"accounts": []any{map[string]any{
			"uid": base, "accessToken": token, "realm": "intl",
		}}})
		result, err := ImportAccounts(dir, doc, "", false)
		if err != nil {
			t.Fatalf("ImportAccounts reserved %q document error: %v", base, err)
		}
		if len(result.Invalid) != 1 || len(result.Added) != 0 || len(result.Updated) != 0 {
			t.Fatalf("ImportAccounts reserved %q result = %+v", base, result)
		}
		got, _ = os.ReadFile(path)
		if !bytes.Equal(got, sentinel) {
			t.Fatalf("ImportAccounts modified reserved file %q: %s", base, got)
		}
	}

	// Case-insensitive reserved names (Windows/macOS semantics).
	if _, err := SaveAccount(dir, Auth{
		UID: "Settings", AccessToken: token, Enabled: true,
	}); err == nil {
		t.Fatal("SaveAccount accepted case-variant reserved uid")
	}
	if err := DeleteAccount(dir, "SETTINGS"); err == nil {
		t.Fatal("DeleteAccount accepted case-variant reserved uid")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("case-variant reserved operations modified settings.json: %s", got)
	}
	// Reserved files must also be excluded case-insensitively on load.
	writeJSON(t, filepath.Join(dir, "Settings.json"), map[string]any{
		"uid": "uid-case-reserved", "accessToken": token, "enabled": true,
	})
	if accounts, err := LoadAccounts(dir); err != nil {
		t.Fatal(err)
	} else {
		for _, account := range accounts {
			if account.UID == "uid-case-reserved" {
				t.Fatal("case-variant reserved file was loaded as an account")
			}
		}
	}

	// An arbitrary non-account .json must never be overwritten or deleted.
	arbitrary := filepath.Join(dir, "uid-arbitrary.json")
	arbitraryBody := []byte(`{"not":"an account"}`)
	if err := os.WriteFile(arbitrary, arbitraryBody, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveAccount(dir, Auth{
		UID: "uid-arbitrary", AccessToken: token, Enabled: true,
	}); err == nil {
		t.Fatal("SaveAccount overwrote an arbitrary non-account JSON file")
	}
	if err := DeleteAccount(dir, "uid-arbitrary"); err == nil {
		t.Fatal("DeleteAccount deleted/ignored an arbitrary non-account JSON file")
	}
	got, _ = os.ReadFile(arbitrary)
	if !bytes.Equal(got, arbitraryBody) {
		t.Fatalf("arbitrary JSON changed: %s", got)
	}

	// SaveAccount may update only an existing valid account with the same uid.
	updated, err := SaveAccount(dir, Auth{
		UID: "uid-update", AccessToken: token, Realm: realmIntl,
		Nickname: "old", Enabled: true,
	})
	if err != nil {
		t.Fatalf("seed update account: %v", err)
	}
	updated.Nickname = "new"
	if _, err := SaveAccount(dir, updated); err != nil {
		t.Fatalf("update same-uid account: %v", err)
	}
	loadedUpdate, err := LoadAccount(updated.Path)
	if err != nil || loadedUpdate.Nickname != "new" {
		t.Fatalf("updated account = %+v, %v", loadedUpdate, err)
	}
	writeJSON(t, filepath.Join(dir, "uid-collision.json"), map[string]any{
		"uid": "uid-other", "accessToken": token, "enabled": true,
	})
	if _, err := SaveAccount(dir, Auth{
		UID: "uid-collision", AccessToken: token, Enabled: true,
	}); err == nil {
		t.Fatal("SaveAccount overwrote a valid account with a different uid")
	}

	// Extra cannot inject settings/config keys into a credential file.
	extra, err := SaveAccount(dir, Auth{
		UID: "uid-extra-block", AccessToken: token, Enabled: true,
		Extra: map[string]json.RawMessage{
			"proxy_slots":        json.RawMessage(`[{"id":"evil"}]`),
			"upstream":           json.RawMessage(`{"device_token":"evil"}`),
			"workbuddy":          json.RawMessage(`{"intl":{"enabled":false}}`),
			"device_token":       json.RawMessage(`"evil"`),
			"active_realm":       json.RawMessage(`"cn"`),
			"model_windows":      json.RawMessage(`[]`),
			"quota_scan":         json.RawMessage(`[]`),
			"futureAccountState": json.RawMessage(`{"keep":true}`),
		},
	})
	if err != nil {
		t.Fatalf("SaveAccount extra fixture: %v", err)
	}
	rawData, err := os.ReadFile(extra.Path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rawData, &raw); err != nil {
		t.Fatal(err)
	}
	for _, blocked := range []string{
		"proxy_slots", "upstream", "workbuddy", "device_token",
		"active_realm", "model_windows", "quota_scan",
	} {
		if _, exists := raw[blocked]; exists {
			t.Fatalf("Extra injected config key %q: %s", blocked, rawData)
		}
	}
	if _, exists := raw["futureAccountState"]; !exists {
		t.Fatalf("Extra dropped a legitimate future account field: %s", rawData)
	}
	got, _ = os.ReadFile(filepath.Join(dir, "settings.json"))
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("account operations modified settings.json: %s", got)
	}
}

func TestImportDesktopCredentialRejectsReservedUID(t *testing.T) {
	accountsDir := t.TempDir()
	settingsPath := filepath.Join(accountsDir, "settings.json")
	sentinel := []byte(`{"sentinel":"keep"}`)
	if err := os.WriteFile(settingsPath, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	token := makeJWT(t, map[string]any{"sub": "settings", "exp": 4102444800})
	credentialPath := filepath.Join(sourceDir, "workbuddy-desktop-ai.info")
	writeJSON(t, credentialPath, map[string]any{
		"auth":    map[string]any{"accessToken": token},
		"account": map[string]any{"uid": "settings", "nickname": "Evil"},
	})
	if _, err := ImportDesktopCredential(accountsDir, credentialPath, ""); err == nil {
		t.Fatal("ImportDesktopCredential accepted a reserved account uid")
	}
	got, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("desktop import modified settings.json: %s", got)
	}
}
