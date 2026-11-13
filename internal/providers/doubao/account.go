package doubao

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const accountFolder = "doubao"

// AccountSummary is the provider-neutral panel inventory row.
type AccountSummary = providers.AccountSummary

// Account is one Doubao cookie/device pair. Cookies and device are credentials
// and never leave the provider through View or AccountSummary.
type Account struct {
	UID          string
	Nickname     string
	Cookies      map[string]string
	Device       map[string]string
	Enabled      bool
	Note         string
	ProxySlot    string
	AddedAt      string
	CheckinQuery string
	CheckinUA    string

	// Reserved for the next phase. They are persisted so the account model does
	// not need a migration when check-in/credits/tasks are added.
	Credits     map[string]any
	Tasks       map[string]any
	LastCheckin string

	Path string

	mu            sync.Mutex
	active        int
	maxConcurrent int
	lastUsed      time.Time
	coolingUntil  time.Time
	lastError     string
}

type accountFile struct {
	UID          string            `json:"uid"`
	Nickname     string            `json:"nickname"`
	Cookies      map[string]string `json:"cookies"`
	Device       map[string]string `json:"device"`
	Enabled      *bool             `json:"enabled,omitempty"`
	Note         string            `json:"note,omitempty"`
	ProxySlot    string            `json:"proxy_slot,omitempty"`
	AddedAt      string            `json:"added_at,omitempty"`
	CheckinQuery string            `json:"checkin_query,omitempty"`
	CheckinUA    string            `json:"checkin_ua,omitempty"`
	Credits      map[string]any    `json:"credits,omitempty"`
	Tasks        map[string]any    `json:"tasks,omitempty"`
	LastCheckin  string            `json:"last_checkin,omitempty"`
}

func (a *Account) fileValue() accountFile {
	enabled := a.Enabled
	return accountFile{
		UID: a.UID, Nickname: a.Nickname, Cookies: a.Cookies, Device: a.Device,
		Enabled: &enabled, Note: a.Note, ProxySlot: a.ProxySlot, AddedAt: a.AddedAt,
		CheckinQuery: a.CheckinQuery, CheckinUA: a.CheckinUA,
		Credits: a.Credits, Tasks: a.Tasks, LastCheckin: a.LastCheckin,
	}
}

func (a *Account) save() error {
	if a == nil || strings.TrimSpace(a.Path) == "" {
		return errors.New("doubao: account path is empty")
	}
	raw, err := json.MarshalIndent(a.fileValue(), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return err
	}
	tmp := a.Path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.Path)
}

func (a *Account) cookieHeader() string {
	if a == nil {
		return ""
	}
	parts := make([]string, 0, len(a.Cookies))
	for key, value := range a.Cookies {
		if strings.TrimSpace(key) == "" {
			continue
		}
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, "; ")
}

func (a *Account) cookie(name string) string {
	if a == nil {
		return ""
	}
	return a.Cookies[name]
}

func (a *Account) deviceValue(name string) string {
	if a == nil {
		return ""
	}
	return a.Device[name]
}

func (a *Account) ready(now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	max := a.maxConcurrent
	if max < 1 {
		max = 1
	}
	if !a.Enabled || a.active >= max {
		return false
	}
	return a.coolingUntil.IsZero() || !now.Before(a.coolingUntil)
}

func (a *Account) acquire() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	max := a.maxConcurrent
	if max < 1 {
		max = 1
	}
	if !a.Enabled || a.active >= max {
		return false
	}
	a.active++
	a.lastUsed = time.Now()
	return true
}

func (a *Account) release() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.active > 0 {
		a.active--
	}
	a.mu.Unlock()
}

func (a *Account) noteSuccess() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.lastError = ""
	a.coolingUntil = time.Time{}
	a.mu.Unlock()
}

func (a *Account) noteError(message string, cooldown time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.lastError = message
	if cooldown > 0 {
		a.coolingUntil = time.Now().Add(cooldown)
	}
	a.mu.Unlock()
}

func (a *Account) view() map[string]any {
	if a == nil {
		return map[string]any{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{
		"uid": a.UID, "nickname": a.Nickname, "realm": ProviderRealm,
		"enabled": a.Enabled, "note": a.Note, "proxy_slot": a.ProxySlot,
		"checkin_probe":     a.CheckinQuery != "",
		"active_concurrent": a.active, "max_concurrent": a.maxConcurrent,
		"last_error": a.lastError,
		"cooling":    !a.coolingUntil.IsZero() && time.Now().Before(a.coolingUntil),
		"credits":    a.Credits, "tasks": a.Tasks, "last_checkin": a.LastCheckin,
	}
}

func (a *Account) summary() providers.AccountSummary {
	if a == nil {
		return providers.AccountSummary{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return providers.AccountSummary{
		UID: a.UID, Nickname: a.Nickname, Realm: ProviderRealm,
		Enabled: a.Enabled, Ready: a.Enabled,
	}
}

// LoadAccounts loads every valid account JSON under dir/doubao. Malformed
// files are skipped; a malformed account is never rewritten.
func LoadAccounts(dir string) ([]*Account, error) {
	folder := filepath.Join(dir, accountFolder)
	entries, err := os.ReadDir(folder)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Account, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(folder, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var file accountFile
		if json.Unmarshal(raw, &file) != nil || len(file.Cookies) == 0 {
			continue
		}
		account := &Account{
			UID: file.UID, Nickname: file.Nickname, Cookies: cloneStringMap(file.Cookies),
			Device: cloneStringMap(file.Device), Enabled: true, Note: file.Note,
			ProxySlot: file.ProxySlot, AddedAt: file.AddedAt,
			CheckinQuery: file.CheckinQuery, CheckinUA: file.CheckinUA,
			Credits: file.Credits,
			Tasks:   file.Tasks, LastCheckin: file.LastCheckin, Path: path,
		}
		if file.Enabled != nil {
			account.Enabled = *file.Enabled
		}
		if account.UID == "" {
			account.UID = deriveUID(file.Cookies)
		}
		if account.Nickname == "" {
			account.Nickname = account.UID
		}
		if account.Device == nil {
			account.Device = map[string]string{}
		}
		out = append(out, account)
	}
	return out, nil
}

func newImportedAccount(dir string, cookies, device map[string]string, uid, nickname string, enabled bool) (*Account, error) {
	if len(cookies) == 0 {
		return nil, errors.New("doubao: cookies are required")
	}
	if uid == "" {
		uid = deriveUID(cookies)
	}
	if nickname == "" {
		nickname = uid
	}
	if device == nil {
		device = map[string]string{}
	}
	if device["web_tab_id"] == "" {
		device["web_tab_id"] = randomID()
	}
	account := &Account{
		UID: uid, Nickname: nickname, Cookies: cloneStringMap(cookies),
		Device: cloneStringMap(device), Enabled: enabled,
		AddedAt: time.Now().Format("2006-01-02 15:04:05"),
		Path:    filepath.Join(dir, accountFolder, safeFilename(uid)+".json"),
	}
	if err := account.save(); err != nil {
		return nil, err
	}
	return account, nil
}

func deriveUID(cookies map[string]string) string {
	seed := strings.TrimSpace(cookies["sessionid"])
	if seed == "" {
		seed = strings.TrimSpace(cookies["ttwid"])
	}
	if seed == "" {
		seed = strings.TrimSpace(cookies["s_v_web_id"])
	}
	if seed == "" {
		seed = randomID()
	}
	sum := sha256.Sum256([]byte(seed))
	return "db_" + hex.EncodeToString(sum[:8])
}

func safeFilename(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "account"
	}
	return b.String()
}

func randomID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	// Doubao expects UUID-shaped ids for block_id/local_message_id/unique_key.
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func cloneStringMap(value map[string]string) map[string]string {
	if len(value) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

func decodeCookieValue(value string) string {
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}
