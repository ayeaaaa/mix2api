package usage

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	cacheSchema       = 5
	legacyCacheSchema = 3
	cacheKindName     = "usage-aggregate-cache-%s.json"
	legacyCacheName   = "usage-aggregate-cache.json"
	cacheTailLength   = 64
)

var cacheKinds = map[string]bool{
	"snapshot": true, "by_account": true, "analytics": true, "series": true, "remaining": true,
}

// Cache persists aggregate checkpoints under the usage directory. A cache is
// an accelerator only: every validation failure falls back to a full scan.
type Cache struct {
	Dir string
	Now func() time.Time

	mu    sync.Mutex
	etags map[string]string
}

// NewCache returns a cache rooted at the usage directory.
func NewCache(dir string) *Cache {
	return &Cache{Dir: dir, etags: map[string]string{}}
}

// Enabled reports whether cache reads and writes are allowed. The environment
// is re-read on every call so an operator can disable it without a restart.
func (c *Cache) Enabled() bool {
	switch normalizeEnv(os.Getenv("WB_USAGE_CACHE")) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func normalizeEnv(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value))
}

// Path returns the kind-specific checkpoint path.
func (c *Cache) Path(kind string) string {
	if c == nil {
		return ""
	}
	return filepath.Join(c.Dir, fmt.Sprintf(cacheKindName, kind))
}

// Entries loads one kind's entries. Corrupt files, schema mismatches and kind
// mismatches are ignored; a corrupt kind file is removed so it cannot poison
// later reads. The legacy single-file layout is a read-only fallback.
func (c *Cache) Entries(kind string) []map[string]any {
	if c == nil || !c.Enabled() || !cacheKinds[kind] {
		return nil
	}
	entries, ok := c.readKind(kind)
	if ok {
		return entries
	}
	return c.readLegacy(kind)
}

func (c *Cache) readKind(kind string) ([]map[string]any, bool) {
	path := c.Path(kind)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var envelope struct {
		Schema  *int              `json:"schema"`
		Kind    *string           `json:"kind"`
		Entries *[]map[string]any `json:"entries"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		_ = os.Remove(path)
		return nil, false
	}
	if envelope.Schema == nil || *envelope.Schema != cacheSchema ||
		envelope.Kind == nil || *envelope.Kind != kind || envelope.Entries == nil {
		return nil, false
	}
	return *envelope.Entries, true
}

func (c *Cache) readLegacy(kind string) []map[string]any {
	path := filepath.Join(c.Dir, legacyCacheName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var envelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil
	}
	schema, ok := cacheInt(envelope["schema"])
	if !ok || schema != legacyCacheSchema {
		return nil
	}
	// v3 stored each kind as a top-level section ({"snapshot":[...]});
	// some tooling used the v4 envelope shape instead. Accept both.
	if section, ok := envelope[kind].([]any); ok {
		out := make([]map[string]any, 0, len(section))
		for _, raw := range section {
			if entry, ok := raw.(map[string]any); ok && entry != nil {
				out = append(out, entry)
			}
		}
		return out
	}
	rawEntries, ok := envelope["entries"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(rawEntries))
	for _, raw := range rawEntries {
		entry, ok := raw.(map[string]any)
		if !ok || entry == nil {
			continue
		}
		if label, _ := entry["kind"].(string); label != "" {
			if label == kind {
				out = append(out, entry)
			}
			continue
		}
		if legacyEntryMatches(kind, entry) {
			out = append(out, entry)
		}
	}
	return out
}

func legacyEntryMatches(kind string, entry map[string]any) bool {
	switch kind {
	case "snapshot":
		_, ok := entry["snap"]
		return ok
	case "by_account":
		_, ok := entry["buckets"]
		return ok
	case "analytics":
		_, ok := entry["maps"]
		return ok
	case "series":
		_, days := entry["days"]
		_, credits := entry["credits"]
		return days && credits
	case "remaining":
		_, events := entry["events"]
		_, usage := entry["usage"]
		return events && usage
	default:
		return false
	}
}

// Save merges one entry into a kind's checkpoint. Entries for other realms are
// preserved untouched. The write is atomic (temp file + rename).
func (c *Cache) Save(kind string, entry map[string]any) error {
	if c == nil || !c.Enabled() || !cacheKinds[kind] || entry == nil {
		return nil
	}
	lock := cacheLock(c.Path(kind))
	lock.Lock()
	defer lock.Unlock()

	existing := c.Entries(kind)
	realm, _ := entry["realm"].(string)
	merged := make([]map[string]any, 0, len(existing)+1)
	for _, old := range existing {
		oldRealm, _ := old["realm"].(string)
		if kind == "by_account" || kind == "remaining" || oldRealm == realm {
			continue
		}
		merged = append(merged, old)
	}
	merged = append(merged, entry)
	envelope := map[string]any{
		"schema":     cacheSchema,
		"kind":       kind,
		"written_at": float64(c.now().UnixNano()) / 1e9,
		"entries":    merged,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(c.Dir, "."+fmt.Sprintf(cacheKindName, kind)+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, c.Path(kind)); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Delete removes one kind's checkpoint (best effort).
func (c *Cache) Delete(kind string) {
	if c == nil {
		return
	}
	_ = os.Remove(c.Path(kind))
}

// SetETag records the ETag of the result currently served for a kind/realm.
func (c *Cache) SetETag(kind, realm string, builtAt float64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.etags == nil {
		c.etags = map[string]string{}
	}
	c.etags[kind+"\x1f"+realm] = CacheETag(kind, realm, builtAt)
	c.mu.Unlock()
}

// ETag returns the last recorded ETag, or "" when there is none.
func (c *Cache) ETag(kind, realm string) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.etags[kind+"\x1f"+realm]
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// CacheETag derives the strong validator v1 uses from (kind, key, builtAt).
func CacheETag(kind, realm string, builtAt float64) string {
	stamp := fmt.Sprintf("%x", int64(builtAt*1_000_000))
	sum := sha1.Sum([]byte(kind + "\x1f" + realm))
	digest := hex.EncodeToString(sum[:])[:16]
	return `"` + stamp + "-" + digest + `"`
}

var cacheLocks sync.Map

func cacheLock(path string) *sync.Mutex {
	value, _ := cacheLocks.LoadOrStore(path, &sync.Mutex{})
	return value.(*sync.Mutex)
}

// ValidateCachePosition checks the stored offset against the current log:
// the file is not shorter than the offset, the mtime shape is sane, and the
// 64 bytes before the offset are unchanged. It returns the offset.
//
// Device, inode and path are deliberately advisory: a migration copies the
// usage log to a new file (and often a new path), so the identity fields no
// longer match even though the bytes before the checkpoint offset are
// unchanged. The tail signature is the content authority; rejecting on a
// copied inode would force every migrated deployment into a full cold scan.
func ValidateCachePosition(entry map[string]any, path string) (int64, bool) {
	if entry == nil {
		return 0, false
	}
	offset, ok := cacheInt(entry["offset"])
	if !ok || offset <= 0 {
		return 0, false
	}
	key, ok := entry["key"].([]any)
	if !ok || len(key) != 3 {
		// JSON decoded arrays are []any; accept []int64 from in-process entries.
		if values, ok := entry["key"].([]int64); ok {
			key = make([]any, len(values))
			for index, value := range values {
				key[index] = value
			}
		} else {
			return 0, false
		}
	}
	size, okSize := cacheInt(key[0])
	if !okSize {
		return 0, false
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() < offset {
		return 0, false
	}
	if currentSize, _, _, ok := logIdentity(info); ok {
		if currentSize != size && currentSize < offset {
			return 0, false
		}
	}
	// v1/v3 checkpoints predate the explicit mtime field; when it is present
	// it must not claim a future write, but its absence is not invalid. A
	// legacy entry without mtime is accepted on device/inode/size/tail alone;
	// any mismatch there is a cold rescan, never a fatal error. Store whole
	// seconds: nanosecond values do not round-trip through json.Number
	// (float64) without occasionally rounding up and looking "future".
	if storedMTime, okMTime := cacheInt(entry["mtime"]); okMTime {
		if info.ModTime().Unix() < storedMTime {
			return 0, false
		}
	}
	tailHex, _ := entry["tail"].(string)
	tail, err := hex.DecodeString(tailHex)
	if err != nil {
		return 0, false
	}
	currentTail, err := tailAt(path, offset)
	if err != nil || !bytes.Equal(currentTail, tail) {
		return 0, false
	}
	return offset, true
}

// CachePosition builds the position fields an entry must carry.
func CachePosition(path string, offset int64) (map[string]any, bool) {
	if offset <= 0 {
		return nil, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	tail, err := tailAt(path, offset)
	if err != nil {
		return nil, false
	}
	_, device, inode, _ := logIdentity(info)
	size := info.Size()
	return map[string]any{
		"log":    path,
		"offset": offset,
		"size":   size,
		"device": device,
		"inode":  inode,
		"mtime":  info.ModTime().Unix(),
		"key":    []int64{size, int64(device), int64(inode)},
		"tail":   hex.EncodeToString(tail),
	}, true
}

func logIdentity(info os.FileInfo) (size int64, device uint64, inode uint64, ok bool) {
	if info == nil {
		return 0, 0, 0, false
	}
	dev, ino, identified := fileIdentity(info)
	return info.Size(), dev, ino, identified
}

func tailAt(path string, offset int64) ([]byte, error) {
	if offset <= 0 {
		return []byte{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	start := offset - cacheTailLength
	if start < 0 {
		start = 0
	}
	tail := make([]byte, offset-start)
	if _, err := file.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return tail, nil
}

func cacheInt(value any) (int64, bool) {
	number, ok := numericValue(value)
	if !ok {
		return 0, false
	}
	return int64(number), true
}

// CacheFingerprint is a small stable digest for a fingerprint string.
func CacheFingerprint(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

// cacheFingerprintOK accepts both our compact fingerprint strings and the
// richer v1 records (a pricing dependency object plus realm_inputs). A string
// mismatch rejects; a v1 object is accepted after shape validation.
func (s *Service) cacheFingerprintOK(entry map[string]any) bool {
	v1Record := false
	if value, ok := entry["pricing"]; ok {
		switch typed := value.(type) {
		case nil:
			// An explicit null dependency record means v1 could not describe
			// the prices its fold used; do not adopt it.
			return false
		case string:
			if typed != CacheFingerprint(s.pricingFingerprint()) {
				return false
			}
		case map[string]any:
			v1Record = true
			if !validV1PricingRecord(typed) {
				return false
			}
		case []any:
			v1Record = true
			if !validV1PricingList(typed) {
				return false
			}
		default:
			return false
		}
	}
	if v1Record {
		inputs, ok := entry["realm_inputs"]
		if !ok || inputs == nil || !validV1RealmInputs(inputs) {
			return false
		}
	}
	if value, ok := entry["realm_fp"]; ok {
		text, ok := value.(string)
		if !ok || text != CacheFingerprint(s.realmFingerprint()) {
			return false
		}
	}
	if value, ok := entry["realm_inputs"]; ok && value != nil {
		return validV1RealmInputs(value)
	}
	return true
}

func validV1RealmInputs(value any) bool {
	parts, ok := value.([]any)
	if !ok || len(parts) != 2 {
		return false
	}
	if parts[0] != nil {
		if _, ok := parts[0].(string); !ok {
			return false
		}
	}
	pairs, ok := parts[1].([]any)
	if !ok {
		return false
	}
	for _, raw := range pairs {
		pair, ok := raw.([]any)
		if !ok || len(pair) != 2 {
			return false
		}
		if _, ok := pair[0].(string); !ok {
			return false
		}
		if _, ok := pair[1].(string); !ok {
			return false
		}
	}
	return true
}

func validV1PricingList(value []any) bool {
	if len(value) == 1 {
		flag, ok := value[0].(string)
		return ok && flag == "off"
	}
	if len(value) != 4 {
		return false
	}
	if flag, ok := value[0].(string); !ok || flag != "on" {
		return false
	}
	for _, raw := range value[1:] {
		if raw == nil {
			continue
		}
		if _, ok := raw.(string); !ok {
			return false
		}
	}
	return true
}

func validV1PricingRecord(record map[string]any) bool {
	for _, field := range []string{"refs", "timeline", "timeline_at", "timeline_models", "unresolved", "backfilled", "builtin"} {
		if _, ok := record[field]; !ok {
			return false
		}
	}
	refs, ok := record["refs"].(map[string]any)
	if !ok {
		return false
	}
	for id, digest := range refs {
		if id == "" {
			return false
		}
		if digest != nil {
			if _, ok := digest.(string); !ok {
				return false
			}
		}
	}
	if builtin, ok := record["builtin"]; ok && builtin != nil {
		if _, ok := builtin.(string); !ok {
			return false
		}
	}
	for _, field := range []string{"timeline", "timeline_at"} {
		if value, ok := record[field]; ok && value != nil {
			if field == "timeline" {
				if _, ok := value.(string); !ok {
					return false
				}
			} else if _, ok := numericValue(value); !ok {
				return false
			}
		}
	}
	for _, field := range []string{"timeline_models", "unresolved"} {
		if value, ok := record[field]; ok && value != nil {
			list, ok := value.([]any)
			if !ok {
				return false
			}
			for _, item := range list {
				if _, ok := item.(string); !ok {
					return false
				}
			}
		}
	}
	if backfilled, ok := record["backfilled"]; ok {
		if _, ok := backfilled.(bool); !ok {
			return false
		}
	}
	return true
}
