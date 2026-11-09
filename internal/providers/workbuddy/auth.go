package workbuddy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

const (
	sourceAPIKey   = "api-key"
	sourceOAuth    = "oauth"
	sourceImport   = "import"
	sourceDesktop  = "desktop-app"
	sourceCockpit  = "cockpit"
	profilePath    = "/console/account"
	loginPending   = 11217
	maxAuthBody    = 1 << 20
	maxDeviceToken = 1024
)

var unsafeAuthUID = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// reservedAccountFiles are shared settings/state/config files that live
// beside account JSON files. Account UIDs and account loading must never
// target these names; matching is case-insensitive because Windows and macOS
// filesystems commonly are.
var reservedAccountFiles = map[string]struct{}{
	"settings.json":      {},
	"active_realm.json":  {},
	"model_windows.json": {},
	"quota_scan.json":    {},
}

func isReservedAccountFile(name string) bool {
	_, reserved := reservedAccountFiles[strings.ToLower(strings.TrimSpace(name))]
	return reserved
}

// configInjectionExtraKeys are settings/state field names that must never be
// carried through account Extra into a credential file. Extra exists for
// provider account state, not for injecting settings/config keys.
var configInjectionExtraKeys = map[string]struct{}{
	"settings":          {},
	"workbuddy":         {},
	"proxy_slots":       {},
	"proxy_slot_seq":    {},
	"upstream":          {},
	"device_token":      {},
	"device_token_file": {},
	"active_realm":      {},
	"model_windows":     {},
	"quota_scan":        {},
}

// Auth is one WorkBuddy account's persisted credential and embedded state. Its
// JSON shape preserves v1.6.5's camelCase fields: expiration and added time are
// Unix seconds on disk, API keys are marked with apiKey, and unknown/state
// fields survive a load/save cycle.
type Auth struct {
	UID                string `json:"uid"`
	Nickname           string `json:"nickname"`
	Realm              string `json:"realm"`
	Domain             string `json:"domain"`
	AccessToken        string `json:"accessToken"`
	RefreshToken       string `json:"refreshToken"`
	ExpiresAt          time.Time
	Platform           string   `json:"platform"`
	EnterpriseID       string   `json:"enterpriseId"`
	Product            string   `json:"product"`
	APIKey             bool     `json:"apiKey"`
	Enabled            bool     `json:"enabled"`
	EnabledSet         bool     `json:"-"`
	TasksEnabled       bool     `json:"tasksEnabled"`
	TasksEnabledSet    bool     `json:"-"`
	Weight             int      `json:"weight"`
	Note               string   `json:"note"`
	Group              string   `json:"group"`
	DisabledModels     []string `json:"disabledModels"`
	ProxySlot          string   `json:"proxySlot"`
	Source             string   `json:"source"`
	AddedAt            time.Time
	Tier               json.RawMessage
	LastCheckin        string `json:"lastCheckin"`
	LastDailyChat      string `json:"lastDailyChat"`
	LastActivityReport string `json:"lastActivityReport"`

	// Path is the loaded/written account file. It is never part of the JSON.
	Path string `json:"-"`

	// ProxyURL is a runtime-resolved outbound proxy for profile/credential
	// helpers. It is never persisted; the pool fills it from ProxySlot.
	ProxyURL string `json:"-"`

	// Extra holds v1 state fields this slice does not model (credits,
	// modelCaps, restrictedUntil, legacy proxy, future fields). Runtime-only
	// fields are deliberately dropped on load and save.
	Extra map[string]json.RawMessage `json:"-"`
}

// SetEnabled records an explicit enabled/disabled choice. Without this call,
// SaveAccount defaults an unflagged account to enabled.
func (a *Auth) SetEnabled(enabled bool) {
	a.Enabled = enabled
	a.EnabledSet = true
}

// SetTasksEnabled records an explicit task-participation choice, independent
// of API dispatch.
func (a *Auth) SetTasksEnabled(enabled bool) {
	a.TasksEnabled = enabled
	a.TasksEnabledSet = true
}

var knownAuthJSONKeys = map[string]struct{}{
	"uid":                {},
	"nickname":           {},
	"domain":             {},
	"realm":              {},
	"platform":           {},
	"product":            {},
	"enterpriseId":       {},
	"accessToken":        {},
	"refreshToken":       {},
	"expiresAt":          {},
	"addedAt":            {},
	"source":             {},
	"apiKey":             {},
	"proxySlot":          {},
	"enabled":            {},
	"tasksEnabled":       {},
	"weight":             {},
	"note":               {},
	"group":              {},
	"disabledModels":     {},
	"tier":               {},
	"lastCheckin":        {},
	"lastDailyChat":      {},
	"lastActivityReport": {},
	"lastError":          {},
	"lastErrorDetail":    {},
	"cooldownUntil":      {},
}

var runtimeAuthJSONKeys = map[string]struct{}{
	"lastError":       {},
	"lastErrorDetail": {},
	"cooldownUntil":   {},
}

// MarshalJSON writes the v1 field order followed by preserved state/unknown
// fields sorted by key. Expiration and added time stay Unix seconds.
func (a Auth) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	models := a.DisabledModels
	if models == nil {
		models = []string{}
	}
	tier := a.Tier
	if len(bytes.TrimSpace(tier)) == 0 {
		tier = json.RawMessage("null")
	}
	fields := []struct {
		name  string
		value any
	}{
		{"uid", a.UID},
		{"nickname", a.Nickname},
		{"domain", a.Domain},
		{"realm", a.Realm},
		{"platform", a.Platform},
		{"product", a.Product},
		{"enterpriseId", a.EnterpriseID},
		{"accessToken", a.AccessToken},
		{"refreshToken", a.RefreshToken},
		{"expiresAt", authExpiresAtValue(a)},
		{"addedAt", timeUnixSeconds(a.AddedAt)},
		{"source", a.Source},
		{"apiKey", a.APIKey},
		{"proxySlot", a.ProxySlot},
		{"enabled", a.Enabled},
		{"tasksEnabled", a.TasksEnabled},
		{"weight", a.Weight},
		{"note", a.Note},
		{"group", a.Group},
		{"disabledModels", models},
		{"tier", tier},
		{"lastCheckin", a.LastCheckin},
		{"lastDailyChat", a.LastDailyChat},
		{"lastActivityReport", a.LastActivityReport},
	}
	for _, field := range fields {
		if err := appendAuthJSONField(&buf, &first, field.name, field.value); err != nil {
			return nil, err
		}
	}

	keys := make([]string, 0, len(a.Extra))
	for key := range a.Extra {
		if _, known := knownAuthJSONKeys[key]; known {
			continue
		}
		if _, injectable := configInjectionExtraKeys[strings.ToLower(key)]; injectable {
			continue
		}
		encoded := bytes.TrimSpace(a.Extra[key])
		if len(encoded) == 0 {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := appendAuthJSONRawField(&buf, &first, key, a.Extra[key]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func appendAuthJSONField(buf *bytes.Buffer, first *bool, name string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	key, err := json.Marshal(name)
	if err != nil {
		return err
	}
	if !*first {
		buf.WriteByte(',')
	}
	*first = false
	buf.Write(key)
	buf.WriteByte(':')
	buf.Write(encoded)
	return nil
}

func appendAuthJSONRawField(buf *bytes.Buffer, first *bool, name string, value json.RawMessage) error {
	key, err := json.Marshal(name)
	if err != nil {
		return err
	}
	if !*first {
		buf.WriteByte(',')
	}
	*first = false
	buf.Write(key)
	buf.WriteByte(':')
	buf.Write(bytes.TrimSpace(value))
	return nil
}

// UnmarshalJSON tolerates v1's missing fields, millisecond timestamps, and
// malformed operator state while preserving unknown/state fields. Runtime-only
// error/cooldown fields are intentionally discarded.
func (a *Auth) UnmarshalJSON(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("workbuddy: account document is not a JSON object")
	}

	nickname := strings.TrimSpace(decodeAuthString(raw["nickname"]))
	if strings.Contains(nickname, "$wbEncrypted") {
		nickname = ""
	}
	next := Auth{
		UID:                strings.TrimSpace(decodeAuthString(raw["uid"])),
		Nickname:           nickname,
		AccessToken:        decodeAuthString(raw["accessToken"]),
		RefreshToken:       decodeAuthString(raw["refreshToken"]),
		Platform:           strings.TrimSpace(decodeAuthString(raw["platform"])),
		EnterpriseID:       strings.TrimSpace(decodeAuthString(raw["enterpriseId"])),
		Source:             strings.TrimSpace(decodeAuthString(raw["source"])),
		Weight:             normalizeAuthWeight(raw["weight"]),
		Note:               truncateRunes(strings.TrimSpace(decodeAuthString(raw["note"])), 200),
		Group:              truncateRunes(strings.TrimSpace(decodeAuthString(raw["group"])), 64),
		DisabledModels:     decodeDisabledModels(raw["disabledModels"]),
		ProxySlot:          strings.TrimSpace(decodeAuthString(raw["proxySlot"])),
		Product:            normalizeProduct(decodeAuthString(raw["product"])),
		Tier:               copyRawJSON(raw["tier"]),
		LastCheckin:        decodeAuthString(raw["lastCheckin"]),
		LastDailyChat:      decodeAuthString(raw["lastDailyChat"]),
		LastActivityReport: decodeAuthString(raw["lastActivityReport"]),
	}
	next.Enabled = true
	if authJSONFieldPresent(raw, "enabled") {
		next.Enabled = decodeAuthBool(raw["enabled"], true)
		next.EnabledSet = true
	}
	next.TasksEnabled = next.Enabled
	if authJSONFieldPresent(raw, "tasksEnabled") {
		next.TasksEnabled = decodeAuthBool(raw["tasksEnabled"], next.Enabled)
		next.TasksEnabledSet = true
	}

	next.APIKey = IsAPIKeyToken(next.AccessToken)
	claims := parseJWTClaims(next.AccessToken)
	if next.UID == "" {
		if next.APIKey {
			next.UID = apiKeyUID(next.AccessToken)
		} else {
			next.UID = strings.TrimSpace(claims.Subject)
		}
	}
	if next.EnterpriseID == "" {
		next.EnterpriseID = claims.EnterpriseID
	}
	next.Realm = normalizeAuthRealm(decodeAuthString(raw["realm"]), next.AccessToken,
		decodeAuthString(raw["domain"]), next.APIKey)
	if next.APIKey {
		next.Realm = realmCN
	}
	next.Domain = strings.TrimSpace(decodeAuthString(raw["domain"]))
	if next.Domain == "" {
		next.Domain = alignedDomainForRealm(next.Realm, "")
	}
	if next.Platform == "" {
		next.Platform = "CLI"
	}
	if next.Product == "" {
		next.Product = productDesktop
	}
	next.ExpiresAt = decodeEpochTime(raw["expiresAt"])
	if next.ExpiresAt.IsZero() && !next.APIKey {
		next.ExpiresAt = epochTime(claims.ExpiresAt)
	}
	if next.APIKey {
		// Static API keys never expire; a stale expiresAt in an imported or
		// hand-edited file must not evict a valid key.
		next.ExpiresAt = time.Time{}
	}
	next.AddedAt = decodeEpochTime(raw["addedAt"])
	if next.AddedAt.IsZero() {
		next.AddedAt = time.Now()
	}
	if next.Source == "" {
		if next.APIKey {
			next.Source = sourceAPIKey
		} else {
			next.Source = sourceOAuth
		}
	}

	for key, value := range raw {
		if _, known := knownAuthJSONKeys[key]; known {
			continue
		}
		if next.Extra == nil {
			next.Extra = map[string]json.RawMessage{}
		}
		next.Extra[key] = append(json.RawMessage(nil), value...)
	}
	*a = next
	return nil
}

func decodeAuthString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return ""
		}
		return text
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err == nil {
		return number.String()
	}
	switch string(trimmed) {
	case "true":
		return "true"
	case "false":
		return "false"
	default:
		return ""
	}
}

// authJSONFieldPresent distinguishes an absent/null v1 field from an explicit
// false. Defaults only apply when the field is truly absent.
func authJSONFieldPresent(raw map[string]json.RawMessage, key string) bool {
	value, exists := raw[key]
	if !exists {
		return false
	}
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func decodeAuthBool(raw json.RawMessage, fallback bool) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fallback
	}
	switch string(trimmed) {
	case "true":
		return true
	case "false":
		return false
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err == nil {
		if value, err := number.Float64(); err == nil {
			return value != 0
		}
		return false
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err == nil {
			return text != ""
		}
	}
	return true
}

func normalizeAuthWeight(raw json.RawMessage) int {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 1
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err == nil {
		if value, err := number.Int64(); err == nil {
			return clampAuthWeight(int(value))
		}
		if value, err := number.Float64(); err == nil {
			return clampAuthWeight(int(value))
		}
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		if value, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
			return clampAuthWeight(value)
		}
		if value, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
			return clampAuthWeight(int(value))
		}
	}
	return 1
}

func clampAuthWeight(value int) int {
	if value < 1 {
		return 1
	}
	if value > 100 {
		return 100
	}
	return value
}

func decodeDisabledModels(raw json.RawMessage) []string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var values []any
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return nil
	}
	return normalizeDisabledModels(values)
}

func normalizeDisabledModels(values []any) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			continue
		}
		text = strings.ToLower(strings.TrimSpace(text))
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, text)
	}
	return out
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit])
}

func copyRawJSON(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func timeUnixSeconds(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.Unix()
}

func authExpiresAtValue(auth Auth) int64 {
	if auth.APIKey {
		return 0
	}
	return timeUnixSeconds(auth.ExpiresAt)
}

func epochTime(seconds int64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

// decodeEpochTime follows v1's normalize_epoch: numeric values and numeric
// strings are seconds unless they exceed 1e11, in which case they are
// milliseconds. Malformed values become the zero time.
func decodeEpochTime(raw json.RawMessage) time.Time {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return time.Time{}
	}
	var number float64
	if err := json.Unmarshal(trimmed, &number); err != nil {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return time.Time{}
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil || parsed != parsed {
			return time.Time{}
		}
		number = parsed
	}
	if number != number {
		return time.Time{}
	}
	if number > 1e11 {
		number /= 1000
	}
	return epochTime(int64(number))
}

func normalizeAuthRealm(field, token, domain string, apiKey bool) string {
	if apiKey {
		return realmCN
	}
	if normalized, ok := normalizeRealm(field); ok {
		return normalized
	}
	if evidence := realmEvidence(token, domain); evidence != "" {
		return evidence
	}
	return realmIntl
}

func alignedDomainForRealm(realm, domain string) string {
	realm = realmOrDefault(realm)
	domain = strings.TrimSpace(domain)
	other := realmIntl
	if realm == realmCN {
		other = realmIntl
	} else {
		other = realmCN
	}
	if domain == "" || hasAnyMarker(strings.ToLower(domain), realmMarkers[other]) {
		return defaultDomain(realm)
	}
	return domain
}

var realmMarkers = map[string][]string{
	realmCN:   {"copilot.tencent.com", "codebuddy.cn", "workbuddy.cn"},
	realmIntl: {"workbuddy.ai", "codebuddy.ai"},
}

func defaultDomain(realm string) string {
	if realmOrDefault(realm) == realmCN {
		return cnDesktopDomain
	}
	return intlDomain
}

func hasAnyMarker(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func realmEvidence(token, domain string) string {
	issuer := strings.ToLower(strings.TrimSpace(parseJWTClaims(token).Issuer))
	if hasAnyMarker(issuer, realmMarkers[realmCN]) {
		return realmCN
	}
	if hasAnyMarker(issuer, realmMarkers[realmIntl]) {
		return realmIntl
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if hasAnyMarker(domain, realmMarkers[realmCN]) {
		return realmCN
	}
	if hasAnyMarker(domain, realmMarkers[realmIntl]) {
		return realmIntl
	}
	return ""
}

// DetectRealmFromToken returns the realm proved by the token issuer or domain,
// defaulting to intl when neither carries evidence. Static API keys always
// belong to the CN realm.
func DetectRealmFromToken(token, domain string) string {
	if IsAPIKeyToken(token) {
		return realmCN
	}
	if evidence := realmEvidence(token, domain); evidence != "" {
		return evidence
	}
	return realmIntl
}

// DomainForRealm aligns a recorded domain with realm. A domain that clearly
// names the other realm is replaced by this realm's canonical fallback.
func DomainForRealm(realm, domain string) string {
	return alignedDomainForRealm(realm, domain)
}

// IsAPIKeyToken reports whether token is one of the static CodeBuddy API keys.
func IsAPIKeyToken(token string) bool {
	return strings.HasPrefix(token, "ck_") || strings.HasPrefix(token, "pt_")
}

// APIKeyID extracts the ck_<id>.<secret> identity segment. Characters that
// would be unsafe in a filename are removed.
func APIKeyID(token string) string {
	text := token
	for _, prefix := range []string{"ck_", "pt_"} {
		if strings.HasPrefix(text, prefix) {
			text = text[len(prefix):]
			break
		}
	}
	if index := strings.Index(text, "."); index >= 0 {
		text = text[:index]
	}
	return unsafeAuthUID.ReplaceAllString(text, "")
}

func apiKeyUID(token string) string {
	id := APIKeyID(token)
	if id == "" {
		return ""
	}
	return "apikey-" + id
}

type jwtClaimSet struct {
	Subject      string
	Issuer       string
	EnterpriseID string
	ExpiresAt    int64
}

func parseJWTClaims(token string) jwtClaimSet {
	var claims jwtClaimSet
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return claims
	}
	payload, err := decodeJWTBase64(parts[1])
	if err != nil {
		return claims
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return claims
	}
	claims.Subject = strings.TrimSpace(decodeAuthString(raw["sub"]))
	claims.Issuer = strings.TrimSpace(decodeAuthString(raw["iss"]))
	claims.EnterpriseID = strings.TrimSpace(decodeAuthString(raw["enterprise_id"]))
	if claims.EnterpriseID == "" {
		claims.EnterpriseID = strings.TrimSpace(decodeAuthString(raw["enterpriseId"]))
	}
	if value := decodeEpochTime(raw["exp"]); !value.IsZero() {
		claims.ExpiresAt = value.Unix()
	}
	return claims
}

func decodeJWTBase64(segment string) ([]byte, error) {
	segment = strings.TrimSpace(segment)
	if decoded, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(segment)
}

// normalizeAuth fills v1 defaults, derives identity from a JWT/API key, and
// rejects documents without a usable uid or a refreshable credential (access
// token or refresh token). It never logs or includes credential values in
// errors.
func normalizeAuth(a Auth) (Auth, error) {
	a.UID = strings.TrimSpace(a.UID)
	if !a.EnabledSet {
		a.Enabled = true
	}
	if !a.TasksEnabledSet {
		a.TasksEnabled = a.Enabled
	}
	a.APIKey = IsAPIKeyToken(a.AccessToken)
	claims := parseJWTClaims(a.AccessToken)
	if a.UID == "" {
		if a.APIKey {
			a.UID = apiKeyUID(a.AccessToken)
		} else {
			a.UID = strings.TrimSpace(claims.Subject)
		}
	}
	if a.UID == "" {
		return Auth{}, errors.New("workbuddy: account has no uid")
	}
	if a.AccessToken == "" && a.RefreshToken == "" {
		return Auth{}, errors.New("workbuddy: account has no access or refresh token")
	}
	if a.EnterpriseID == "" {
		a.EnterpriseID = claims.EnterpriseID
	}
	if a.Realm == "" || !validRealm(a.Realm) {
		a.Realm = DetectRealmFromToken(a.AccessToken, a.Domain)
	}
	a.Realm = realmOrDefault(a.Realm)
	if a.APIKey {
		a.Realm = realmCN
	}
	a.Domain = alignedDomainForRealm(a.Realm, a.Domain)
	if a.Platform == "" {
		a.Platform = "CLI"
	}
	a.Product = normalizeProduct(a.Product)
	if a.Source == "" {
		if a.APIKey {
			a.Source = sourceAPIKey
		} else {
			a.Source = sourceOAuth
		}
	}
	if a.APIKey {
		a.ExpiresAt = time.Time{}
	} else if a.ExpiresAt.IsZero() {
		a.ExpiresAt = epochTime(claims.ExpiresAt)
	}
	if a.AddedAt.IsZero() {
		a.AddedAt = time.Now()
	}
	a.Weight = clampAuthWeight(a.Weight)
	a.Nickname = strings.TrimSpace(a.Nickname)
	if strings.Contains(a.Nickname, "$wbEncrypted") {
		a.Nickname = ""
	}
	a.Note = truncateRunes(strings.TrimSpace(a.Note), 200)
	a.Group = truncateRunes(strings.TrimSpace(a.Group), 64)
	if a.DisabledModels == nil {
		a.DisabledModels = []string{}
	}
	a.DisabledModels = normalizeDisabledModels(stringSliceToAny(a.DisabledModels))
	a.Extra = sanitizeAuthExtra(a.Extra)
	return a, nil
}

func validRealm(value string) bool {
	_, ok := normalizeRealm(value)
	return ok
}

func stringSliceToAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func sanitizeAuthExtra(extra map[string]json.RawMessage) map[string]json.RawMessage {
	if len(extra) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for key, value := range extra {
		if _, known := knownAuthJSONKeys[key]; known {
			continue
		}
		if _, runtime := runtimeAuthJSONKeys[key]; runtime {
			continue
		}
		if _, injectable := configInjectionExtraKeys[strings.ToLower(key)]; injectable {
			continue
		}
		trimmed := bytes.TrimSpace(value)
		if len(trimmed) == 0 {
			continue
		}
		out[key] = append(json.RawMessage(nil), value...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// LoadAccount reads and normalizes one v1 credential file.
func LoadAccount(path string) (Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Auth{}, fmt.Errorf("workbuddy: read account file %s: %w", path, err)
	}
	var auth Auth
	if err := json.Unmarshal(data, &auth); err != nil {
		return Auth{}, fmt.Errorf("workbuddy: decode account file %s: %w", path, err)
	}
	auth, err = normalizeAuth(auth)
	if err != nil {
		return Auth{}, fmt.Errorf("workbuddy: account file %s: %w", path, err)
	}
	auth.Path = path
	return auth, nil
}

// LoadAccounts reads every account JSON file in dir. Unknown files and
// malformed/missing-credential rows are skipped so one bad file cannot stop
// the pool.
func LoadAccounts(dir string) ([]Auth, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Auth{}, nil
		}
		return nil, fmt.Errorf("workbuddy: read accounts dir: %w", err)
	}
	accounts := make([]Auth, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if isReservedAccountFile(name) {
			continue
		}
		account, err := LoadAccount(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

// AccountPath returns the v1 filename for uid. Unsafe characters are replaced
// with underscores; an empty sanitized value gets a random filename.
func AccountPath(dir, uid string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", errors.New("workbuddy: accounts dir is empty")
	}
	safe := regexpReplaceAuthUID(uid)
	if safe == "" {
		safe = randomHex(16)
	}
	if isReservedAccountFile(safe + ".json") {
		return "", fmt.Errorf("workbuddy: account uid %q collides with a reserved config file", uid)
	}
	return filepath.Join(dir, safe+".json"), nil
}

func regexpReplaceAuthUID(uid string) string {
	safe := unsafeAuthUID.ReplaceAllString(uid, "_")
	return strings.Trim(safe, "_ ")
}

// SaveAccount writes one credential file atomically with mode 0600. It strips
// runtime-only error/cooldown fields and preserves all other unknown/state
// fields carried on Auth.
func SaveAccount(dir string, auth Auth) (Auth, error) {
	auth, err := normalizeAuth(auth)
	if err != nil {
		return Auth{}, err
	}
	path, err := AccountPath(dir, auth.UID)
	if err != nil {
		return Auth{}, err
	}
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.IsDir() {
			return Auth{}, fmt.Errorf("workbuddy: refusing to overwrite account directory %s", filepath.Base(path))
		}
		existing, err := LoadAccount(path)
		if err != nil {
			return Auth{}, fmt.Errorf("workbuddy: refusing to overwrite non-account file %s", filepath.Base(path))
		}
		if existing.UID != auth.UID {
			return Auth{}, fmt.Errorf("workbuddy: account filename collision at %s", filepath.Base(path))
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Auth{}, fmt.Errorf("workbuddy: inspect account path: %w", statErr)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Auth{}, fmt.Errorf("workbuddy: create accounts dir: %w", err)
	}
	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return Auth{}, fmt.Errorf("workbuddy: encode account: %w", err)
	}
	data = append(data, '\n')
	if err := secrets.WritePrivate(path, data); err != nil {
		return Auth{}, fmt.Errorf("workbuddy: save account: %w", err)
	}
	auth.Path = path
	return auth, nil
}

// DeleteAccount removes uid's credential file. Missing files are not an error,
// matching v1's idempotent delete. Account files saved under a random fallback
// name (when the sanitized uid is empty) are found by scanning the directory.
func DeleteAccount(dir, uid string) error {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return errors.New("workbuddy: account uid is empty")
	}
	if accounts, err := LoadAccounts(dir); err == nil {
		for _, account := range accounts {
			if account.UID != uid {
				continue
			}
			return removeAccountFile(account.Path)
		}
	}
	path, err := AccountPath(dir, uid)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("workbuddy: delete account: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("workbuddy: refusing to delete account directory %s", path)
	}
	return fmt.Errorf("workbuddy: refusing to delete non-account file %s", filepath.Base(path))
}

// removeAccountFile deletes one credential file. It refuses directories even
// when their name ends in .json, so a crafted/accidental directory cannot be
// removed by an account delete.
func removeAccountFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("workbuddy: delete account: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("workbuddy: refusing to delete account directory %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workbuddy: delete account: %w", err)
	}
	return nil
}

func readSettingsRaw(dir string) map[string]any {
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return map[string]any{}
	}
	settings := map[string]any{}
	if err := json.Unmarshal(data, &settings); err != nil || settings == nil {
		return map[string]any{}
	}
	return settings
}

type deviceTokenCacheEntry struct {
	mu    sync.Mutex
	key   string
	token string
	at    time.Time
}

var (
	deviceTokenCache deviceTokenCacheEntry
	deviceTokenClock = time.Now
)

const deviceTokenTTL = 5 * time.Minute

// ResolveDeviceToken returns the settings-provided X-Device-Token source: a
// literal value first, otherwise a desktop token file (max 1KB). Results are
// cached for five minutes. Any read failure degrades to an empty token.
func ResolveDeviceToken(dir string) (string, error) {
	settings := readSettingsRaw(dir)
	upstream, _ := settings["upstream"].(map[string]any)
	literal, _ := upstream["device_token"].(string)
	literal = strings.TrimSpace(literal)
	path, _ := upstream["device_token_file"].(string)
	path = strings.TrimSpace(path)
	key := literal
	if key == "" {
		key = path
	}
	if key == "" {
		return "", nil
	}

	now := deviceTokenClock()
	deviceTokenCache.mu.Lock()
	if deviceTokenCache.key == key && !deviceTokenCache.at.IsZero() &&
		now.Sub(deviceTokenCache.at) < deviceTokenTTL {
		token := deviceTokenCache.token
		deviceTokenCache.mu.Unlock()
		return token, nil
	}
	deviceTokenCache.mu.Unlock()

	token := literal
	if token == "" && path != "" {
		if info, err := os.Stat(path); err == nil && info.Size() <= maxDeviceToken {
			if data, err := os.ReadFile(path); err == nil {
				token = strings.TrimSpace(string(data))
			}
		}
	}
	deviceTokenCache.mu.Lock()
	deviceTokenCache.key = key
	deviceTokenCache.token = token
	deviceTokenCache.at = now
	deviceTokenCache.mu.Unlock()
	return token, nil
}

// HTTPDoer is the minimal HTTP client used by profile fetches. It exists so
// tests and callers can use httptest clients without network access.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ContextHTTPDoer is the context/proxy-aware client shape used by the shared
// transport. Profile fetches use it so an account's resolved proxy can be
// honoured without changing the plain HTTPDoer test seam.
type ContextHTTPDoer interface {
	Do(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error)
}

const profileHTTPTimeout = 15 * time.Second

var defaultProfileTransport ContextHTTPDoer = transport.NewClient(transport.ClientOptions{
	Timeout:         profileHTTPTimeout,
	RefuseRedirects: true,
})

const webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0"

func webOriginForRealm(realm string) string {
	if realmOrDefault(realm) == realmCN {
		return "https://www.workbuddy.cn"
	}
	return "https://www.workbuddy.ai"
}

// profileOriginFor is a test seam for the fixed web-console origin.
var profileOriginFor = webOriginForRealm

// FetchProfile reads only uid and nickname from the web-console profile,
// updating auth. It never returns phone/email or any credential value. The
// account's resolved ProxyURL, when set by the pool, pins the request to the
// account's exit IP.
func FetchProfile(ctx context.Context, auth *Auth) error {
	if auth == nil {
		return errors.New("workbuddy: profile account is nil")
	}
	return FetchProfileWithProxy(ctx, defaultProfileTransport,
		profileOriginFor(auth.Realm), auth, auth.ProxyURL)
}

// FetchProfileWithProxy is FetchProfile with an injectable contextual client,
// origin, and explicit proxy URL. An empty proxy URL means direct.
func FetchProfileWithProxy(ctx context.Context, client ContextHTTPDoer, origin string, auth *Auth, proxyURL string) error {
	if client == nil {
		return errors.New("workbuddy: profile HTTP client is nil")
	}
	return fetchProfile(ctx, origin, auth, func(ctx context.Context, req *http.Request) (*http.Response, error) {
		return client.Do(ctx, req, proxyURL)
	})
}

// FetchProfileAt is FetchProfile with an injectable plain HTTP client and
// origin (used by mock HTTP tests; it cannot carry a proxy).
func FetchProfileAt(ctx context.Context, client HTTPDoer, origin string, auth *Auth) error {
	if client == nil {
		return errors.New("workbuddy: profile HTTP client is nil")
	}
	return fetchProfile(ctx, origin, auth, func(_ context.Context, req *http.Request) (*http.Response, error) {
		return client.Do(req)
	})
}

type profileDo func(ctx context.Context, req *http.Request) (*http.Response, error)

func fetchProfile(ctx context.Context, origin string, auth *Auth, do profileDo) error {
	if auth == nil {
		return errors.New("workbuddy: profile account is nil")
	}
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return errors.New("workbuddy: profile origin is empty")
	}
	if do == nil {
		return errors.New("workbuddy: profile HTTP client is nil")
	}
	ctx, cancel := context.WithTimeout(nonNilContext(ctx), profileHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+profilePath, nil)
	if err != nil {
		return errors.New("workbuddy: profile request is invalid")
	}
	req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("Origin", "https://www.workbuddy.cn")
	req.Header.Set("Referer", "https://www.workbuddy.cn/profile/account-settings")
	req.Header.Set("User-Agent", webUserAgent)

	response, err := do(ctx, req)
	if err != nil {
		return errors.New("workbuddy: profile request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAuthBody))
	if err != nil {
		return errors.New("workbuddy: profile response read failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("workbuddy: profile HTTP %d", response.StatusCode)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return errors.New("workbuddy: profile response is not a JSON object")
	}
	if data, ok := root["data"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(data, &nested); err == nil && nested != nil {
			for key, value := range nested {
				if _, exists := root[key]; !exists {
					root[key] = value
				}
			}
		}
	}
	uid := strings.TrimSpace(decodeAuthString(root["uid"]))
	var nickname string
	if rawNickname, exists := root["nickname"]; exists {
		var text string
		if err := json.Unmarshal(rawNickname, &text); err == nil {
			nickname = strings.TrimSpace(text)
		}
	}
	if uid != "" && auth.UID != "" && uid != auth.UID {
		return errors.New("workbuddy: profile uid mismatch")
	}
	if auth.UID == "" {
		auth.UID = uid
	}
	if nickname != "" {
		auth.Nickname = nickname
	}
	return nil
}

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// ImportIssue describes one skipped or invalid import row.
type ImportIssue struct {
	Index  int    `json:"index,omitempty"`
	UID    string `json:"uid,omitempty"`
	Reason string `json:"reason"`
}

// ImportResult reports an account-document import.
type ImportResult struct {
	Added   []string      `json:"added"`
	Updated []string      `json:"updated"`
	Skipped []ImportIssue `json:"skipped"`
	Invalid []ImportIssue `json:"invalid"`
}

var importKnownKeys = map[string]struct{}{
	"uid": {}, "nickname": {}, "email": {}, "domain": {}, "realm": {},
	"platform": {}, "product": {}, "enterpriseId": {}, "enterprise_id": {},
	"accessToken": {}, "access_token": {}, "refreshToken": {},
	"refresh_token": {}, "expiresAt": {}, "expires_at": {}, "addedAt": {},
	"added_at": {}, "source": {}, "apiKey": {}, "api_key": {},
	"proxySlot": {}, "proxy_slot": {}, "proxy": {}, "enabled": {},
	"tasksEnabled": {}, "tasks_enabled": {}, "weight": {}, "note": {},
	"group": {}, "disabledModels": {}, "disabled_models": {}, "tier": {},
	"lastCheckin": {}, "last_checkin": {}, "lastDailyChat": {},
	"last_daily_chat": {}, "lastActivityReport": {},
	"last_activity_report": {}, "lastError": {}, "last_error": {},
	"cooldownUntil": {}, "cooldown_until": {}, "credits": {},
	"modelCaps": {}, "model_caps": {}, "restrictedUntil": {},
	"restricted_until": {}, "auth": {}, "account": {}, "format": {},
	"version": {}, "exportedAt": {}, "count": {}, "accounts": {},
}

var importVolatileKeys = map[string]struct{}{
	"lastError": {}, "last_error": {}, "cooldownUntil": {}, "cooldown_until": {},
	"credits": {}, "modelCaps": {}, "model_caps": {}, "restrictedUntil": {},
	"restricted_until": {}, "tier": {}, "lastCheckin": {}, "last_checkin": {},
	"lastDailyChat": {}, "last_daily_chat": {}, "lastActivityReport": {},
	"last_activity_report": {},
}

// ImportAccounts imports a v1 export document, a bare account list, one
// account object, or a desktop credential envelope. Existing accounts are
// skipped unless overwrite is true. Invalid account objects are reported per
// row; a non-object row aborts the whole document because its shape cannot be
// trusted. Sources come from the row (cockpit rows default to "cockpit"; API
// keys to "api-key"); there is no function-level source override.
func ImportAccounts(dir string, data []byte, realm string, overwrite bool) (ImportResult, error) {
	rows, err := decodeImportRows(data)
	if err != nil {
		return ImportResult{}, err
	}
	forced, _ := normalizeRealm(strings.TrimSpace(realm))
	known := map[string]Auth{}
	if existing, err := LoadAccounts(dir); err == nil {
		for _, account := range existing {
			known[account.UID] = account
		}
	}

	result := ImportResult{
		Added: []string{}, Updated: []string{},
		Skipped: []ImportIssue{}, Invalid: []ImportIssue{},
	}
	seen := map[string]bool{}
	for index, row := range rows {
		next, presence, err := normalizeImportRow(row, forced)
		if err != nil {
			result.Invalid = append(result.Invalid, ImportIssue{
				Index: index + 1, Reason: err.Error(),
			})
			continue
		}
		uid := next.UID
		if seen[uid] {
			result.Skipped = append(result.Skipped, ImportIssue{
				UID: uid, Reason: "duplicate inside the document",
			})
			continue
		}
		seen[uid] = true

		existing, exists := known[uid]
		if exists && !overwrite {
			result.Skipped = append(result.Skipped, ImportIssue{
				UID: uid, Reason: "already exists",
			})
			continue
		}
		if exists {
			explicit := next
			next = mergeReloginAuth(existing, next)
			reapplyImportPresence(&next, explicit, presence)
		}
		saved, err := SaveAccount(dir, next)
		if err != nil {
			result.Invalid = append(result.Invalid, ImportIssue{
				UID: uid, Reason: "account could not be saved",
			})
			continue
		}
		known[uid] = saved
		if exists {
			result.Updated = append(result.Updated, uid)
		} else {
			result.Added = append(result.Added, uid)
		}
	}
	return result, nil
}

func reapplyImportPresence(auth *Auth, explicit Auth, presence map[string]bool) {
	if presence["proxySlot"] {
		auth.ProxySlot = explicit.ProxySlot
	}
	if presence["group"] {
		auth.Group = explicit.Group
	}
	if presence["note"] {
		auth.Note = explicit.Note
	}
	if presence["weight"] {
		auth.Weight = explicit.Weight
	}
	if presence["tasksEnabled"] {
		auth.TasksEnabled = explicit.TasksEnabled
		auth.TasksEnabledSet = true
	}
	if presence["disabledModels"] {
		auth.DisabledModels = append([]string(nil), explicit.DisabledModels...)
	}
}

func decodeImportRows(data []byte) ([]map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, errors.New("workbuddy: import document is not valid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("workbuddy: import document has trailing data")
	}

	var rows []any
	switch value := root.(type) {
	case []any:
		rows = value
	case map[string]any:
		if accounts, ok := value["accounts"].([]any); ok {
			rows = accounts
		} else if looksLikeImportAccount(value) {
			rows = []any{value}
		} else {
			return nil, errors.New("workbuddy: import document is not an account document")
		}
	default:
		return nil, errors.New("workbuddy: import document is not an account document")
	}
	if len(rows) == 0 {
		return nil, errors.New("workbuddy: import document has no accounts")
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		object, ok := row.(map[string]any)
		if !ok {
			return nil, errors.New("workbuddy: import row is not an object")
		}
		out = append(out, object)
	}
	return out, nil
}

func looksLikeImportAccount(row map[string]any) bool {
	if _, ok := row["accessToken"]; ok {
		return true
	}
	if _, ok := row["access_token"]; ok {
		return true
	}
	if _, ok := row["auth"].(map[string]any); ok {
		return true
	}
	if _, ok := row["account"].(map[string]any); ok {
		return true
	}
	return false
}

func importLayers(row map[string]any) []map[string]any {
	layers := []map[string]any{row}
	for _, key := range []string{"auth", "account"} {
		if layer, ok := row[key].(map[string]any); ok {
			layers = append(layers, layer)
		}
	}
	return layers
}

func pickImportValue(layers []map[string]any, keys ...string) (any, bool) {
	for _, layer := range layers {
		for _, key := range keys {
			value, exists := layer[key]
			if !exists || value == nil || value == "" {
				continue
			}
			return value, true
		}
	}
	return nil, false
}

func pickImportString(layers []map[string]any, keys ...string) (string, bool) {
	value, ok := pickImportValue(layers, keys...)
	if !ok {
		return "", false
	}
	return importStringValue(value), true
}

func importStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func importBoolValue(value any, fallback bool) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case json.Number:
		number, err := typed.Float64()
		return err == nil && number != 0
	case float64:
		return typed != 0
	case string:
		return typed != ""
	default:
		return fallback
	}
}

func importWeightValue(value any) int {
	switch typed := value.(type) {
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return clampAuthWeight(int(number))
		}
		if number, err := typed.Float64(); err == nil {
			return clampAuthWeight(int(number))
		}
	case float64:
		return clampAuthWeight(int(typed))
	case string:
		if number, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return clampAuthWeight(number)
		}
	}
	return 1
}

func importEpochValue(value any) time.Time {
	if value == nil {
		return time.Time{}
	}
	var text string
	switch typed := value.(type) {
	case json.Number:
		text = typed.String()
	case float64:
		text = strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		text = strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case int:
		text = strconv.Itoa(typed)
	case int64:
		text = strconv.FormatInt(typed, 10)
	case string:
		text = typed
	default:
		return time.Time{}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil || number != number {
		return time.Time{}
	}
	if number > 1e11 {
		number /= 1000
	}
	return epochTime(int64(number))
}

func importDisabledValues(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case []string:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = item
		}
		return out, true
	default:
		return nil, false
	}
}

func isCockpitImportRow(row map[string]any) bool {
	_, snake := row["access_token"]
	_, camel := row["accessToken"]
	return snake && !camel
}

func normalizeImportRow(row map[string]any, forcedRealm string) (Auth, map[string]bool, error) {
	layers := importLayers(row)
	cockpit := isCockpitImportRow(row)
	token, _ := pickImportString(layers, "accessToken", "access_token")
	token = strings.TrimSpace(token)
	if token == "" {
		return Auth{}, nil, errors.New("no accessToken")
	}
	apiKey := IsAPIKeyToken(token)
	if !apiKey && strings.Count(token, ".") != 2 {
		return Auth{}, nil, errors.New("accessToken is not a JWT")
	}

	domain, _ := pickImportString(layers, "domain")
	rowRealm, _ := pickImportString(layers, "realm")
	detected := ""
	if normalized, ok := normalizeRealm(forcedRealm); ok {
		detected = normalized
	} else if evidence := realmEvidence(token, domain); evidence != "" {
		detected = evidence
	} else if normalized, ok := normalizeRealm(rowRealm); ok {
		detected = normalized
	} else {
		detected = realmIntl
	}
	if apiKey {
		detected = realmCN
	}

	uid, _ := pickImportString(layers, "uid")
	uid = strings.TrimSpace(uid)
	if uid == "" {
		if apiKey {
			uid = apiKeyUID(token)
		} else {
			uid = strings.TrimSpace(parseJWTClaims(token).Subject)
		}
	}
	uid = regexpReplaceAuthUID(uid)
	if uid == "" {
		return Auth{}, nil, errors.New("cannot determine uid (no uid field and no sub claim)")
	}

	nickname, _ := pickImportString(layers, "nickname")
	if nickname == "" {
		nickname, _ = pickImportString(layers, "email")
	}
	nickname = strings.TrimSpace(nickname)
	if strings.Contains(nickname, "$wbEncrypted") {
		nickname = ""
	}
	platform, _ := pickImportString(layers, "platform")
	enterprise, _ := pickImportString(layers, "enterpriseId", "enterprise_id")
	product, _ := pickImportString(layers, "product")
	source, _ := pickImportString(layers, "source")
	if source == "" {
		switch {
		case apiKey:
			source = sourceAPIKey
		case cockpit:
			source = sourceCockpit
		default:
			source = sourceImport
		}
	}
	proxySlot, proxySet := pickImportString(layers, "proxySlot", "proxy_slot")
	proxySlot = strings.TrimSpace(proxySlot)
	group, groupSet := pickImportString(layers, "group")
	note, noteSet := pickImportString(layers, "note")
	weightRaw, weightSet := pickImportValue(layers, "weight")
	tasksRaw, tasksSet := pickImportValue(layers, "tasksEnabled", "tasks_enabled")
	disabledRaw, disabledSet := pickImportValue(layers, "disabledModels", "disabled_models")
	addedRaw, _ := pickImportValue(layers, "addedAt", "added_at")
	expiresRaw, _ := pickImportValue(layers, "expiresAt", "expires_at")

	presence := map[string]bool{
		"proxySlot":      proxySet,
		"group":          groupSet,
		"note":           noteSet,
		"weight":         weightSet,
		"tasksEnabled":   tasksSet,
		"disabledModels": disabledSet,
	}
	expires := time.Time{}
	if !apiKey {
		expires = importEpochValue(expiresRaw)
		if expires.IsZero() {
			expires = epochTime(parseJWTClaims(token).ExpiresAt)
		}
	}
	added := importEpochValue(addedRaw)
	if added.IsZero() {
		added = time.Now()
	}
	weight := 1
	if weightSet {
		weight = importWeightValue(weightRaw)
	}
	tasksEnabled := true
	if tasksSet {
		tasksEnabled = importBoolValue(tasksRaw, true)
	}
	var disabled []string
	if disabledSet {
		if values, ok := importDisabledValues(disabledRaw); ok {
			disabled = normalizeDisabledModels(values)
		}
	}
	if disabled == nil {
		disabled = []string{}
	}
	claims := parseJWTClaims(token)
	if enterprise == "" {
		enterprise = claims.EnterpriseID
	}
	auth := Auth{
		UID:             uid,
		Nickname:        nickname,
		Realm:           detected,
		Domain:          alignedDomainForRealm(detected, domain),
		AccessToken:     token,
		RefreshToken:    mustImportString(layers, "refreshToken", "refresh_token"),
		ExpiresAt:       expires,
		Platform:        platform,
		EnterpriseID:    enterprise,
		Product:         product,
		APIKey:          apiKey,
		Enabled:         true,
		TasksEnabled:    tasksEnabled,
		TasksEnabledSet: tasksSet,
		Weight:          weight,
		Note:            truncateRunes(strings.TrimSpace(note), 200),
		Group:           truncateRunes(strings.TrimSpace(group), 64),
		DisabledModels:  disabled,
		ProxySlot:       proxySlot,
		Source:          source,
		AddedAt:         added,
		Extra:           importExtras(layers),
	}
	auth, err := normalizeAuth(auth)
	if err != nil {
		return Auth{}, nil, err
	}
	return auth, presence, nil
}

func mustImportString(layers []map[string]any, keys ...string) string {
	value, _ := pickImportString(layers, keys...)
	return strings.TrimSpace(value)
}

func importExtras(layers []map[string]any) map[string]json.RawMessage {
	var extra map[string]json.RawMessage
	for _, layer := range layers {
		for key, value := range layer {
			if _, known := importKnownKeys[key]; known {
				continue
			}
			if _, volatile := importVolatileKeys[key]; volatile {
				continue
			}
			if _, runtime := runtimeAuthJSONKeys[key]; runtime {
				continue
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				continue
			}
			if extra == nil {
				extra = map[string]json.RawMessage{}
			}
			extra[key] = encoded
		}
	}
	return sanitizeAuthExtra(extra)
}
