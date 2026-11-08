package raccoon

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
)

// Auth is one Raccoon account's persisted credentials. JSON field names match
// v1's auth documents.
type Auth struct {
	UID            string    `json:"uid"`
	AccessToken    string    `json:"access_token"`
	RefreshToken   string    `json:"refresh_token"`
	OfficeIdentity string    `json:"office_identity"`
	OfficeOrgName  string    `json:"office_org_name"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// MarshalJSON writes the six v1 auth keys. ExpiresAt is stored as Unix seconds
// to stay compatible with v1's numeric expires_at values.
func (a Auth) MarshalJSON() ([]byte, error) {
	type authDocument struct {
		UID            string `json:"uid"`
		AccessToken    string `json:"access_token"`
		RefreshToken   string `json:"refresh_token"`
		OfficeIdentity string `json:"office_identity"`
		OfficeOrgName  string `json:"office_org_name"`
		ExpiresAt      int64  `json:"expires_at"`
	}

	var expiresAt int64
	if !a.ExpiresAt.IsZero() {
		expiresAt = a.ExpiresAt.Unix()
	}
	return json.Marshal(authDocument{
		UID:            a.UID,
		AccessToken:    a.AccessToken,
		RefreshToken:   a.RefreshToken,
		OfficeIdentity: a.OfficeIdentity,
		OfficeOrgName:  a.OfficeOrgName,
		ExpiresAt:      expiresAt,
	})
}

// UnmarshalJSON accepts v1 numeric epoch seconds, epoch milliseconds, and
// RFC3339 strings for expires_at. Unknown keys are ignored.
func (a *Auth) UnmarshalJSON(data []byte) error {
	type authDocument struct {
		UID            string          `json:"uid"`
		AccessToken    string          `json:"access_token"`
		RefreshToken   string          `json:"refresh_token"`
		OfficeIdentity string          `json:"office_identity"`
		OfficeOrgName  string          `json:"office_org_name"`
		ExpiresAt      json.RawMessage `json:"expires_at"`
	}

	var doc authDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	*a = Auth{
		UID:            doc.UID,
		AccessToken:    doc.AccessToken,
		RefreshToken:   doc.RefreshToken,
		OfficeIdentity: doc.OfficeIdentity,
		OfficeOrgName:  doc.OfficeOrgName,
		ExpiresAt:      decodeExpiresAt(doc.ExpiresAt),
	}
	return nil
}

// LoadAuth reads one auth document. It rejects a document without a usable
// access token, strips a Bearer prefix, derives a missing UID from the JWT
// subject (with v1's opaque-token hash fallback), and falls back to the JWT
// exp claim when expires_at is absent. Errors never include token values.
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
	if auth.ExpiresAt.IsZero() {
		auth.ExpiresAt = expiresAtFromAccessToken(auth.AccessToken)
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
