package codearts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
)

var unsafeUIDCharacters = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Auth is one CodeArts account's persisted credentials. The JSON shape matches
// v1's accounts/codearts/codearts-<uid>.json: all known fields keep their v1
// names, expiration stays an ISO-8601 string, and unknown fields survive a
// load/save cycle.
type Auth struct {
	UserID           string `json:"user_id"`
	UserName         string `json:"user_name"`
	DomainID         string `json:"domain_id"`
	CloudDragonToken string `json:"cloud_dragon_token"`
	AccessKeyID      string `json:"access_key_id"`
	SecretAccessKey  string `json:"secret_access_key"`
	RefreshToken     string `json:"refresh_token"`
	CodeVerifier     string `json:"code_verifier"`
	ClientID         string `json:"client_id"`
	Expiration       time.Time
	DPopPrivateKey   json.RawMessage
	ProxySlot        string `json:"proxySlot"`
	UpdatedAt        int64  `json:"updated_at"`

	// extraFields keeps unknown JSON fields byte-for-byte so a load/save cycle
	// cannot drop future v1 settings.
	extraFields map[string]json.RawMessage
}

var knownAuthJSONKeys = map[string]struct{}{
	"user_id":            {},
	"user_name":          {},
	"domain_id":          {},
	"cloud_dragon_token": {},
	"access_key_id":      {},
	"secret_access_key":  {},
	"refresh_token":      {},
	"code_verifier":      {},
	"client_id":          {},
	"expiration":         {},
	"dpop_private_key":   {},
	"proxySlot":          {},
	"updated_at":         {},
}

// MarshalJSON writes the v1 wire format in a fixed order followed by unknown
// fields sorted by key. Expiration is serialized as an ISO-8601 string (empty
// for the zero time) and dpop_private_key as the original JSON value.
func (a Auth) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	fields := []struct {
		name  string
		value any
	}{
		{"user_id", a.UserID},
		{"user_name", a.UserName},
		{"domain_id", a.DomainID},
		{"cloud_dragon_token", a.CloudDragonToken},
		{"access_key_id", a.AccessKeyID},
		{"secret_access_key", a.SecretAccessKey},
		{"refresh_token", a.RefreshToken},
		{"code_verifier", a.CodeVerifier},
		{"client_id", a.ClientID},
		{"expiration", formatExpiration(a.Expiration)},
		{"dpop_private_key", dpopJSONValue(a.DPopPrivateKey)},
		{"proxySlot", a.ProxySlot},
		{"updated_at", a.UpdatedAt},
	}
	for _, field := range fields {
		if err := appendAuthField(&buf, &first, field.name, field.value); err != nil {
			return nil, err
		}
	}

	extraKeys := make([]string, 0, len(a.extraFields))
	for key := range a.extraFields {
		if _, known := knownAuthJSONKeys[key]; !known {
			extraKeys = append(extraKeys, key)
		}
	}
	sort.Strings(extraKeys)
	for _, key := range extraKeys {
		encoded := json.RawMessage(bytes.TrimSpace(a.extraFields[key]))
		if len(encoded) == 0 {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		keyJSON, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buf.Write(keyJSON)
		buf.WriteByte(':')
		buf.Write(encoded)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func appendAuthField(buf *bytes.Buffer, first *bool, name string, value any) error {
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

func dpopJSONValue(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return raw
}

func formatExpiration(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	value = value.UTC()
	if value.Nanosecond() == 0 {
		return value.Format(time.RFC3339)
	}
	return value.Format(time.RFC3339Nano)
}

// UnmarshalJSON tolerates missing known fields, numeric spellings of string
// values, and empty or malformed expirations. Unknown fields are retained.
func (a *Auth) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("codearts: auth document is not a JSON object")
	}

	next := Auth{
		UserID:           decodeStringValue(raw["user_id"]),
		UserName:         decodeStringValue(raw["user_name"]),
		DomainID:         decodeStringValue(raw["domain_id"]),
		CloudDragonToken: decodeStringValue(raw["cloud_dragon_token"]),
		AccessKeyID:      decodeStringValue(raw["access_key_id"]),
		SecretAccessKey:  decodeStringValue(raw["secret_access_key"]),
		RefreshToken:     decodeStringValue(raw["refresh_token"]),
		CodeVerifier:     decodeStringValue(raw["code_verifier"]),
		ClientID:         decodeStringValue(raw["client_id"]),
		Expiration:       decodeExpiration(raw["expiration"]),
		DPopPrivateKey:   decodeRawValue(raw["dpop_private_key"]),
		ProxySlot:        decodeStringValue(raw["proxySlot"]),
		UpdatedAt:        decodeInt64Value(raw["updated_at"]),
	}

	for key, value := range raw {
		if _, known := knownAuthJSONKeys[key]; known {
			continue
		}
		if next.extraFields == nil {
			next.extraFields = make(map[string]json.RawMessage)
		}
		next.extraFields[key] = append(json.RawMessage(nil), value...)
	}

	*a = next
	return nil
}

func decodeRawValue(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// decodeStringValue accepts strings and numeric JSON values, which is what
// v1's str(value) handled for identity fields. Other shapes decode empty.
func decodeStringValue(raw json.RawMessage) string {
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
	return ""
}

func decodeInt64Value(raw json.RawMessage) int64 {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err == nil {
		if value, err := number.Int64(); err == nil {
			return value
		}
		if value, err := number.Float64(); err == nil {
			return int64(value)
		}
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err == nil {
			if value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
				return value
			}
			if value, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
				return int64(value)
			}
		}
		return 0
	}
	switch string(trimmed) {
	case "true":
		return 1
	case "false":
		return 0
	}
	return 0
}

// decodeExpiration tolerates v1's ISO-8601 strings, empty values, and
// malformed values. Numeric and other non-string shapes are malformed by v1's
// _parse_expiration rules. Anything unparseable becomes the zero time rather
// than blocking the account load.
func decodeExpiration(raw json.RawMessage) time.Time {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return time.Time{}
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return time.Time{}
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return time.Time{}
		}
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed
		}
		// v1's fromisoformat also accepted a naive local timestamp.
		for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
			if parsed, err := time.ParseInLocation(layout, text, time.Local); err == nil {
				return parsed
			}
		}
		return time.Time{}
	}

	// v1's _parse_expiration only accepted ISO-8601 text; numeric or other
	// shapes are malformed and become the zero time.
	return time.Time{}
}

// LoadAuth reads one auth document. It rejects a document without a usable
// cloud_dragon_token or user_id, the two fields v1 requires before an account
// enters the pool. Errors never include token values.
func LoadAuth(path string) (Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Auth{}, fmt.Errorf("codearts: read auth file %s: %w", path, err)
	}

	var auth Auth
	if err := json.Unmarshal(data, &auth); err != nil {
		return Auth{}, fmt.Errorf("codearts: decode auth file %s: %w", path, err)
	}
	if strings.TrimSpace(auth.UserID) == "" {
		return Auth{}, fmt.Errorf("codearts: auth file %s: user_id is empty", path)
	}
	if strings.TrimSpace(auth.CloudDragonToken) == "" {
		return Auth{}, fmt.Errorf("codearts: auth file %s: cloud_dragon_token is empty", path)
	}
	return auth, nil
}

// SaveAuth writes one auth document atomically with mode 0600, creating its
// parent directory if needed. Missing required identity fields are rejected.
// Errors never include token values.
func SaveAuth(path string, auth Auth) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("codearts: auth path is empty")
	}
	if strings.TrimSpace(auth.UserID) == "" {
		return errors.New("codearts: auth requires user_id")
	}
	if strings.TrimSpace(auth.CloudDragonToken) == "" {
		return errors.New("codearts: auth requires cloud_dragon_token")
	}

	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return fmt.Errorf("codearts: encode auth: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codearts: create auth directory: %w", err)
	}
	if err := secrets.WritePrivate(path, data); err != nil {
		return fmt.Errorf("codearts: save auth: %w", err)
	}
	return nil
}

// AuthPath returns the v1 auth path for uid under accountsDir:
// accountsDir/codearts/codearts-<safe-uid>.json. UID is untrusted, so path
// separators and non [A-Za-z0-9._-] characters are replaced with underscores.
// An empty UID keeps v1's "unknown" filename fallback.
func AuthPath(accountsDir, uid string) (string, error) {
	text := strings.TrimSpace(uid)
	if text == "" {
		text = "unknown"
	}
	safe := unsafeUIDCharacters.ReplaceAllString(text, "_")
	if safe == "" {
		safe = "unknown"
	}
	return filepath.Join(accountsDir, "codearts", "codearts-"+safe+".json"), nil
}
