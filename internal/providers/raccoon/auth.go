package raccoon

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
)

var unsafeUIDCharacters = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Auth is one Raccoon account's persisted credentials. The first six fields
// are the required v1 keys; the remaining known v1 fields and any unknown
// future fields are preserved additively.
type Auth struct {
	UID            string    `json:"uid"`
	AccessToken    string    `json:"access_token"`
	RefreshToken   string    `json:"refresh_token"`
	OfficeIdentity string    `json:"office_identity"`
	OfficeOrgName  string    `json:"office_org_name"`
	ExpiresAt      time.Time `json:"expires_at"`

	TokenType     string `json:"token_type,omitempty"`
	OfficeOrgRole string `json:"office_org_role,omitempty"`
	ProxySlot     string `json:"proxySlot,omitempty"`
	UpdatedAt     int64  `json:"updated_at,omitempty"`

	// extraFields keeps unknown JSON fields byte-for-byte so a load/save cycle
	// cannot drop future v1 settings. Empty known extras are also kept here so
	// their presence survives a round trip.
	extraFields map[string]json.RawMessage
}

// MarshalJSON writes the six required v1 auth keys plus any present known
// extras and unknown fields loaded from disk. ExpiresAt is stored as Unix
// seconds to stay compatible with v1's numeric expires_at values.
func (a Auth) MarshalJSON() ([]byte, error) {
	fields := make(map[string]json.RawMessage, 10)
	put := func(key string, value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		fields[key] = encoded
		return nil
	}

	var expiresAt int64
	if !a.ExpiresAt.IsZero() {
		expiresAt = a.ExpiresAt.Unix()
	}
	values := []struct {
		key   string
		value any
	}{
		{"uid", a.UID},
		{"access_token", a.AccessToken},
		{"refresh_token", a.RefreshToken},
		{"office_identity", a.OfficeIdentity},
		{"office_org_name", a.OfficeOrgName},
		{"expires_at", expiresAt},
	}
	for _, field := range values {
		if err := put(field.key, field.value); err != nil {
			return nil, err
		}
	}

	if a.TokenType != "" {
		if err := put("token_type", a.TokenType); err != nil {
			return nil, err
		}
	}
	if a.OfficeOrgRole != "" {
		if err := put("office_org_role", a.OfficeOrgRole); err != nil {
			return nil, err
		}
	}
	if a.ProxySlot != "" {
		if err := put("proxySlot", a.ProxySlot); err != nil {
			return nil, err
		}
	}
	if a.UpdatedAt != 0 {
		if err := put("updated_at", a.UpdatedAt); err != nil {
			return nil, err
		}
	}

	for key, raw := range a.extraFields {
		if _, present := fields[key]; present {
			continue
		}
		fields[key] = append(json.RawMessage(nil), raw...)
	}
	return json.Marshal(fields)
}

// UnmarshalJSON accepts v1 numeric epoch seconds, epoch milliseconds, and
// RFC3339 strings for expires_at. Known v1 extras and unknown keys are kept;
// unknown values remain raw so numeric precision and shape are not lost.
func (a *Auth) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	next := Auth{}
	for key, destination := range map[string]*string{
		"uid":             &next.UID,
		"access_token":    &next.AccessToken,
		"refresh_token":   &next.RefreshToken,
		"office_identity": &next.OfficeIdentity,
		"office_org_name": &next.OfficeOrgName,
	} {
		if raw, ok := fields[key]; ok {
			if err := json.Unmarshal(raw, destination); err != nil {
				return err
			}
		}
	}
	next.ExpiresAt = decodeExpiresAt(fields["expires_at"])

	if raw, ok := fields["token_type"]; ok {
		if err := json.Unmarshal(raw, &next.TokenType); err != nil || next.TokenType == "" {
			next.addExtra("token_type", raw)
		}
	}
	if raw, ok := fields["office_org_role"]; ok {
		if err := json.Unmarshal(raw, &next.OfficeOrgRole); err != nil || next.OfficeOrgRole == "" {
			next.addExtra("office_org_role", raw)
		}
	}
	if raw, ok := fields["proxySlot"]; ok {
		if err := json.Unmarshal(raw, &next.ProxySlot); err != nil || next.ProxySlot == "" {
			next.addExtra("proxySlot", raw)
		}
	}
	if raw, ok := fields["updated_at"]; ok {
		if value, err := decodeInt64JSON(raw); err != nil || value == 0 {
			next.addExtra("updated_at", raw)
		} else {
			next.UpdatedAt = value
		}
	}

	known := map[string]bool{
		"uid":             true,
		"access_token":    true,
		"refresh_token":   true,
		"office_identity": true,
		"office_org_name": true,
		"expires_at":      true,
		"token_type":      true,
		"office_org_role": true,
		"proxySlot":       true,
		"updated_at":      true,
	}
	for key, raw := range fields {
		if !known[key] {
			next.addExtra(key, raw)
		}
	}

	*a = next
	return nil
}

func (a *Auth) addExtra(key string, raw json.RawMessage) {
	if a.extraFields == nil {
		a.extraFields = make(map[string]json.RawMessage)
	}
	a.extraFields[key] = append(json.RawMessage(nil), raw...)
}

func decodeInt64JSON(raw json.RawMessage) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return 0, err
	}
	return number.Int64()
}

// LoadAuth reads one auth document. It rejects a document without a usable
// access token, strips a Bearer prefix, derives a missing UID from the JWT
// subject (with v1's opaque-token hash fallback), and prefers the JWT exp
// claim over a stored expires_at when the token carries exp. Errors never
// include token values.
func LoadAuth(path string) (Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Auth{}, fmt.Errorf("raccoon: read auth file %s: %w", path, err)
	}

	var auth Auth
	if err := json.Unmarshal(data, &auth); err != nil {
		return Auth{}, fmt.Errorf("raccoon: decode auth file %s: %w", path, err)
	}
	cleanAuth(&auth)
	if auth.AccessToken == "" {
		return Auth{}, fmt.Errorf("raccoon: auth file %s: access_token is empty", path)
	}
	if auth.UID == "" {
		auth.UID = auth.uidFromCredentials()
	}
	if auth.UID == "" {
		return Auth{}, fmt.Errorf("raccoon: auth file %s: uid is empty", path)
	}
	if exp := expiresAtFromAccessToken(auth.AccessToken); !exp.IsZero() {
		auth.ExpiresAt = exp
	}
	return auth, nil
}

// SaveAuth writes one auth document atomically with mode 0600, creating its
// parent directory if needed. A missing UID is derived from the access token;
// a missing access token is rejected. Errors never include token values.
func SaveAuth(path string, auth Auth) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("raccoon: auth path is empty")
	}
	cleanAuth(&auth)
	if auth.AccessToken == "" {
		return errors.New("raccoon: auth requires access_token")
	}
	if auth.UID == "" {
		auth.UID = auth.uidFromCredentials()
	}
	if auth.UID == "" {
		return errors.New("raccoon: auth requires uid")
	}

	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return fmt.Errorf("raccoon: encode auth: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("raccoon: create auth directory: %w", err)
	}
	if err := secrets.WritePrivate(path, data); err != nil {
		return fmt.Errorf("raccoon: save auth: %w", err)
	}
	return nil
}

// AuthPath returns the v1 auth path for UID under accountsDir:
// accountsDir/raccoon/raccoon-<safe-uid>.json. UID is untrusted (it may come
// from an unverified JWT sub), so non [A-Za-z0-9._-] runs are replaced with
// underscores and empty UIDs are rejected rather than used raw.
func AuthPath(accountsDir, uid string) (string, error) {
	text := strings.TrimSpace(uid)
	if text == "" {
		return "", errors.New("raccoon: uid is empty")
	}
	safe := unsafeUIDCharacters.ReplaceAllString(text, "_")
	if safe == "" || safe == "." || safe == ".." {
		safe = "_"
	}
	return filepath.Join(accountsDir, "raccoon", "raccoon-"+safe+".json"), nil
}

// UIDFromAccessToken returns the JWT "sub" claim without verifying the
// signature. A malformed or non-JWT token yields an empty string.
func UIDFromAccessToken(token string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	switch sub := claims["sub"].(type) {
	case string:
		return strings.TrimSpace(sub)
	case float64:
		return strconv.FormatFloat(sub, 'f', -1, 64)
	}
	return ""
}

func cleanAuth(auth *Auth) {
	auth.UID = strings.TrimSpace(auth.UID)
	auth.AccessToken = cleanToken(auth.AccessToken)
	auth.RefreshToken = cleanToken(auth.RefreshToken)
	auth.OfficeIdentity = strings.TrimSpace(auth.OfficeIdentity)
	auth.OfficeOrgName = strings.TrimSpace(auth.OfficeOrgName)
}

func cleanToken(value string) string {
	text := strings.TrimSpace(value)
	if strings.EqualFold(text, "bearer") {
		return ""
	}
	if len(text) >= 7 && strings.EqualFold(text[:7], "bearer ") {
		return strings.TrimSpace(text[7:])
	}
	return text
}

// uidFromCredentials mirrors v1: the JWT subject when available, otherwise a
// deterministic hash of the refresh token (or access token).
func (a Auth) uidFromCredentials() string {
	if sub := UIDFromAccessToken(a.AccessToken); sub != "" {
		return sub
	}
	seed := a.RefreshToken
	if seed == "" {
		seed = a.AccessToken
	}
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return "raccoon-" + hex.EncodeToString(sum[:])[:12]
}

// jwtClaims decodes the payload segment without verifying the signature. Any
// malformed input yields nil so callers can treat it as "no claims".
func jwtClaims(token string) map[string]any {
	parts := strings.Split(cleanToken(token), ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}

	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	if claims == nil {
		return nil
	}
	return claims
}

func expiresAtFromAccessToken(token string) time.Time {
	claims := jwtClaims(token)
	if claims == nil {
		return time.Time{}
	}
	exp, ok := numericValue(claims["exp"])
	if !ok || exp <= 0 {
		return time.Time{}
	}
	return epochSecondsToTime(exp)
}

// decodeExpiresAt tolerates the value shapes seen in v1 data: epoch seconds,
// epoch milliseconds, RFC3339 strings, null, and malformed values (zero time).
func decodeExpiresAt(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return time.Time{}
	}
	switch v := value.(type) {
	case float64:
		return epochSecondsToTime(v)
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return time.Time{}
		}
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UTC()
		}
		number, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return time.Time{}
		}
		return epochSecondsToTime(number)
	default:
		return time.Time{}
	}
}

func epochSecondsToTime(seconds float64) time.Time {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return time.Time{}
	}
	// Values this large are epoch milliseconds from imported v1 data.
	if seconds >= 1e12 {
		seconds /= 1000
	}
	// Reject values that cannot be a sensible calendar time (after year 9999).
	if seconds > 253402300799 {
		return time.Time{}
	}
	whole := int64(seconds)
	nanos := int64(math.Round((seconds - float64(whole)) * float64(time.Second)))
	if nanos >= int64(time.Second) {
		whole++
		nanos -= int64(time.Second)
	}
	return time.Unix(whole, nanos).UTC()
}
