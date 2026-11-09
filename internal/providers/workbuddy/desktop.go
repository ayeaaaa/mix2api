package workbuddy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// DesktopCredential is the read-only metadata for one desktop-client
// credential file. It deliberately has no token fields, so routes can expose
// it to the panel without leaking credentials.
type DesktopCredential struct {
	Path      string `json:"path"`
	File      string `json:"file"`
	Realm     string `json:"realm"`
	Domain    string `json:"domain"`
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	ExpiresAt int64  `json:"expiresAt"`
	Readable  bool   `json:"readable"`
	Valid     bool   `json:"valid"`
	Encrypted bool   `json:"encrypted"`
	NeedsKey  bool   `json:"needsKey"`
	Error     string `json:"error"`
}

type desktopFileShape struct {
	Auth    map[string]json.RawMessage `json:"auth"`
	Account map[string]json.RawMessage `json:"account"`
}

// DesktopAuthDirs lists the known desktop-client credential directories.
func DesktopAuthDirs() []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}

	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		local := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
		if local == "" && home != "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		if local != "" {
			add(filepath.Join(local, "CodeBuddyExtension", "Data", "Public", "auth"))
		}
		return out
	}

	base := filepath.Join(home, ".local", "share")
	if runtime.GOOS == "darwin" && home != "" {
		base = filepath.Join(home, "Library", "Application Support")
	}
	if home == "" {
		return out
	}
	for _, app := range []string{"CodeBuddyExtension", "WorkBuddy", "CodeBuddy"} {
		add(filepath.Join(base, app, "Data", "Public", "auth"))
		add(filepath.Join(base, app, "auth"))
	}
	return out
}

type desktopCandidate struct {
	path string
	hint string
	file string
}

func desktopCredentialCandidates(dirs []string) []desktopCandidate {
	if dirs == nil {
		dirs = DesktopAuthDirs()
	}
	seen := map[string]bool{}
	out := []desktopCandidate{}
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		for _, name := range []struct {
			file string
			hint string
		}{
			{"workbuddy-desktop-ai.info", realmIntl},
			{"workbuddy-desktop.info", realmCN},
		} {
			path := filepath.Join(dir, name.file)
			if seen[path] {
				continue
			}
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			seen[path] = true
			out = append(out, desktopCandidate{path: path, hint: name.hint, file: name.file})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

func isEncryptedEnvelope(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '{' {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return false
		}
		_, marker := object["$wbEncrypted"]
		var envelope string
		if err := json.Unmarshal(object["envelope"], &envelope); err == nil && envelope != "" {
			return marker
		}
		return false
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return false
		}
		text = strings.TrimLeft(text, " \t\r\n")
		return strings.HasPrefix(text, "{") && strings.Contains(truncateRunes(text, 64), "$wbEncrypted")
	}
	return false
}

func effectiveDesktopRealm(hint, token, domain string) string {
	if evidence := realmEvidence(token, domain); evidence != "" {
		return evidence
	}
	if normalized, ok := normalizeRealm(hint); ok {
		return normalized
	}
	return realmIntl
}

func desktopHintForPath(path, fallback string) string {
	if normalized, ok := normalizeRealm(fallback); ok {
		return normalized
	}
	switch strings.ToLower(filepath.Base(path)) {
	case "workbuddy-desktop-ai.info":
		return realmIntl
	case "workbuddy-desktop.info":
		return realmCN
	default:
		return realmIntl
	}
}

// inspectDesktopFile reads one desktop credential without network access and
// without attempting encrypted-envelope recovery.
func inspectDesktopFile(candidate desktopCandidate, hintOverride string) (Auth, DesktopCredential) {
	hint := candidate.hint
	if strings.TrimSpace(hintOverride) != "" {
		hint = desktopHintForPath(candidate.path, hintOverride)
	}
	entry := DesktopCredential{
		Path:   candidate.path,
		File:   candidate.file,
		Realm:  hint,
		Domain: defaultDomain(hint),
	}

	data, err := os.ReadFile(candidate.path)
	if err != nil {
		entry.Error = "credential file is unreadable"
		return Auth{}, entry
	}
	var shape desktopFileShape
	if err := json.Unmarshal(data, &shape); err != nil || shape.Auth == nil {
		entry.Error = "credential file is not a valid desktop credential"
		return Auth{}, entry
	}
	entry.Readable = true

	rawToken := shape.Auth["accessToken"]
	encrypted := isEncryptedEnvelope(rawToken)
	if encrypted {
		entry.Encrypted = true
		entry.NeedsKey = true
		entry.Error = "credential is encrypted; recover the desktop key first"
		return Auth{}, entry
	}
	token := decodeAuthString(rawToken)
	if token == "" {
		entry.Error = "no accessToken inside the file"
		return Auth{}, entry
	}

	uid := strings.TrimSpace(decodeAuthString(shape.Account["uid"]))
	if uid == "" {
		uid = strings.TrimSpace(parseJWTClaims(token).Subject)
	}
	nickname := strings.TrimSpace(decodeAuthString(shape.Account["nickname"]))
	if strings.Contains(nickname, "$wbEncrypted") {
		nickname = ""
	}
	realm := effectiveDesktopRealm(hint, token, decodeAuthString(shape.Auth["domain"]))
	domain := alignedDomainForRealm(realm, decodeAuthString(shape.Auth["domain"]))
	expires := decodeEpochTime(shape.Auth["expiresAt"])
	if expires.IsZero() {
		expires = epochTime(parseJWTClaims(token).ExpiresAt)
	}
	entry.Realm = realm
	entry.Domain = domain
	entry.UID = uid
	entry.Nickname = nickname
	entry.ExpiresAt = timeUnixSeconds(expires)
	entry.Valid = uid != ""

	refresh := ""
	if rawRefresh := shape.Auth["refreshToken"]; !isEncryptedEnvelope(rawRefresh) {
		refresh = decodeAuthString(rawRefresh)
	}
	auth := Auth{
		UID:          uid,
		Nickname:     nickname,
		Realm:        realm,
		Domain:       domain,
		AccessToken:  token,
		RefreshToken: refresh,
		ExpiresAt:    expires,
		Platform:     "CLI",
		EnterpriseID: strings.TrimSpace(decodeAuthString(shape.Account["enterpriseId"])),
		Source:       sourceDesktop,
		Enabled:      true,
		TasksEnabled: true,
	}
	return auth, entry
}

// InspectDesktopCredentials returns metadata for every known desktop
// credential file in the default locations. Encrypted entries are marked
// needsKey instead of being decrypted.
func InspectDesktopCredentials() ([]DesktopCredential, error) {
	return InspectDesktopCredentialsIn(nil)
}

// InspectDesktopCredentialsIn is InspectDesktopCredentials with explicit
// candidate directories (meaningful mainly for tests and packaging).
func InspectDesktopCredentialsIn(dirs []string) ([]DesktopCredential, error) {
	candidates := desktopCredentialCandidates(dirs)
	entries := make([]DesktopCredential, 0, len(candidates))
	for _, candidate := range candidates {
		_, entry := inspectDesktopFile(candidate, "")
		entries = append(entries, entry)
	}
	return entries, nil
}

// ScanDesktopCredentials reads the known desktop-client credential files.
// It is read-only, performs no network I/O, and skips encrypted envelopes.
func ScanDesktopCredentials() ([]Auth, error) {
	return ScanDesktopCredentialsIn(nil)
}

// ScanDesktopCredentialsIn is ScanDesktopCredentials with explicit candidate
// directories.
func ScanDesktopCredentialsIn(dirs []string) ([]Auth, error) {
	candidates := desktopCredentialCandidates(dirs)
	accounts := make([]Auth, 0, len(candidates))
	for _, candidate := range candidates {
		auth, entry := inspectDesktopFile(candidate, "")
		if !entry.Valid || auth.AccessToken == "" {
			continue
		}
		normalized, err := normalizeAuth(auth)
		if err != nil {
			continue
		}
		accounts = append(accounts, normalized)
	}
	return accounts, nil
}

// ImportDesktopCredential imports one desktop credential file into the
// accounts directory. It rejects encrypted envelopes because v2 does not
// recover desktop process-memory keys.
func ImportDesktopCredential(accountsDir, path, hint string) (Auth, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Auth{}, errors.New("workbuddy: desktop credential path is empty")
	}
	file := filepath.Base(path)
	candidate := desktopCandidate{path: path, hint: desktopHintForPath(path, hint), file: file}
	auth, entry := inspectDesktopFile(candidate, hint)
	if !entry.Valid || auth.AccessToken == "" {
		if entry.Encrypted {
			return Auth{}, errors.New("workbuddy: desktop credential is encrypted; recover the desktop key first")
		}
		return Auth{}, errors.New("workbuddy: desktop credential has no usable access token")
	}
	auth.Source = sourceDesktop
	auth.Enabled = true
	auth.TasksEnabled = true
	normalized, err := normalizeAuth(auth)
	if err != nil {
		return Auth{}, err
	}
	if existing, ok := findAccount(accountsDir, normalized.UID); ok {
		normalized = mergeReloginAuth(existing, normalized)
	}
	return SaveAccount(accountsDir, normalized)
}

// ImportDesktopCredentials imports every readable desktop credential under the
// known default locations, filtered to realm when realm is intl/cn.
func ImportDesktopCredentials(accountsDir, realm string) ([]Auth, error) {
	accounts := []Auth{}
	for _, candidate := range desktopCredentialCandidates(nil) {
		auth, entry := inspectDesktopFile(candidate, "")
		if !entry.Valid || auth.AccessToken == "" {
			continue
		}
		if normalized, ok := normalizeRealm(realm); ok && auth.Realm != normalized {
			continue
		}
		imported, err := ImportDesktopCredential(accountsDir, candidate.path, candidate.hint)
		if err != nil {
			continue
		}
		accounts = append(accounts, imported)
	}
	return accounts, nil
}

func findAccount(dir, uid string) (Auth, bool) {
	accounts, err := LoadAccounts(dir)
	if err != nil {
		return Auth{}, false
	}
	for _, account := range accounts {
		if account.UID == uid {
			return account, true
		}
	}
	return Auth{}, false
}

// mergeReloginAuth preserves v1 operator state across a duplicate login or
// import: the existing added time, proxy binding, note/group/weight/model
// bans, task participation, task timestamps, tier, and unknown state survive
// unless the caller supplied a non-zero replacement.
func mergeReloginAuth(existing, next Auth) Auth {
	if !existing.AddedAt.IsZero() {
		next.AddedAt = existing.AddedAt
	}
	if next.ProxySlot == "" {
		next.ProxySlot = existing.ProxySlot
	}
	if existing.Note != "" {
		next.Note = existing.Note
	}
	if existing.Group != "" {
		next.Group = existing.Group
	}
	if existing.Weight > 0 {
		next.Weight = existing.Weight
	}
	if len(existing.DisabledModels) > 0 {
		next.DisabledModels = append([]string(nil), existing.DisabledModels...)
	}
	next.TasksEnabled = existing.TasksEnabled
	next.TasksEnabledSet = true
	if existing.Tier != nil {
		next.Tier = copyRawJSON(existing.Tier)
	}
	if existing.LastCheckin != "" {
		next.LastCheckin = existing.LastCheckin
	}
	if existing.LastDailyChat != "" {
		next.LastDailyChat = existing.LastDailyChat
	}
	if existing.LastActivityReport != "" {
		next.LastActivityReport = existing.LastActivityReport
	}
	merged := map[string]json.RawMessage{}
	for key, value := range existing.Extra {
		merged[key] = append(json.RawMessage(nil), value...)
	}
	for key, value := range next.Extra {
		merged[key] = append(json.RawMessage(nil), value...)
	}
	next.Extra = sanitizeAuthExtra(merged)
	return next
}
