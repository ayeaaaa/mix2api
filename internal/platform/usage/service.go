package usage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/pricing"
)

// SnapshotOptions selects the usage summary view.
type SnapshotOptions struct {
	Realm string
	Range string
	Since *time.Time
	Until *time.Time
}

// AnalyticsOptions selects the analytics halves.
type AnalyticsOptions struct {
	Realm string
	Range string
	Since *time.Time
	Until *time.Time
}

// PerfOptions selects the latency sample.
type PerfOptions struct {
	Realm  string
	Range  string
	Since  *time.Time
	Until  *time.Time
	Sample int
}

// SeriesOptions selects a bucketed token/credit series.
type SeriesOptions struct {
	Realm         string
	Range         string
	Since         *time.Time
	Until         *time.Time
	BucketSeconds int
}

// KeyInfo is the narrow key-store view analytics needs. Keys == nil lets the
// service treat every non-empty key in the log as a named row, which keeps the
// package usable before the key store is wired.
type KeyInfo struct {
	ID      string
	Name    string
	Realm   string
	Enabled bool
}

// AccountInfo is the narrow account-pool view the summary needs.
type AccountInfo struct {
	UID            string
	Nickname       string
	Realm          string
	Domain         string
	Issuer         string
	CredentialFile string
	ExpiresAt      float64
	Accounts       int
	AccountsReady  int
	Credits        map[string]any
}

// CapAnchor is one persisted model-cap reset instant.
type CapAnchor struct {
	At    float64
	Reset float64
}

// RemainingAccount is the narrow account view the remaining estimate needs.
// Nil RemainingAccounts makes the service derive accounts from the log.
type RemainingAccount struct {
	UID       string
	Nickname  string
	Realm     string
	Enabled   bool
	ModelCaps map[string]CapAnchor
}

// Service is the v1 usage/analytics application service.
type Service struct {
	Store   *Store
	Pricing *pricing.Snapshot
	Cache   *Cache
	Now     func() time.Time

	// KeyName resolves a key id to its display name. Unknown ids should be
	// returned unchanged; an empty id returns "".
	KeyName func(string) string
	Keys    []KeyInfo

	Accounts       map[string]AccountInfo
	CurrentAccount AccountInfo
	// AccountsFunc and CurrentAccountFunc make the panel summary follow the
	// live provider pools instead of a startup snapshot. The function forms
	// take precedence when set; CurrentAccountFunc receives the query realm
	// ("" or "all" means the whole pool).
	AccountsFunc       func() map[string]AccountInfo
	CurrentAccountFunc func(realm string) AccountInfo
	DefaultRealm       string
	// DefaultRealmFunc makes the panel's active realm live: usage views read
	// the current realm without rebuilding the service.
	DefaultRealmFunc func() string
	StartedAt        time.Time

	RemainingAccounts []RemainingAccount

	// AfterScan is a test/benchmark seam invoked between a full scan and the
	// checkpoint write. Production leaves it nil; a test can append a row here
	// to prove the checkpoint offset is the scanned prefix, not a later size.
	AfterScan func()

	resultMu    sync.Mutex
	resultCache map[string]cachedResult
	queryETags  map[string]string

	remainingMu   sync.Mutex
	remainingData map[string]any
	remainingAt   time.Time
	remainingDay  string
}

type cachedResult struct {
	data    any
	builtAt float64
	etag    string
	expires time.Time
}

// NewService builds a service with v1 default TTLs and a started clock.
func NewService(store *Store, price *pricing.Snapshot, cache *Cache) *Service {
	return &Service{
		Store:       store,
		Pricing:     price,
		Cache:       cache,
		StartedAt:   time.Now(),
		resultCache: map[string]cachedResult{},
	}
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) started() time.Time {
	if s != nil && !s.StartedAt.IsZero() {
		return s.StartedAt
	}
	return s.now()
}

func (s *Service) statsTTL() time.Duration {
	return envSeconds("WB_STATS_TTL", 15)
}

func (s *Service) remainingTTL() time.Duration {
	return envSeconds("WB_REMAINING_TTL", 15)
}

func envSeconds(name string, fallback float64) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return time.Duration(fallback * float64(time.Second))
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 {
		return time.Duration(fallback * float64(time.Second))
	}
	return time.Duration(value * float64(time.Second))
}

func (s *Service) resultGet(kind, key string) (any, string, bool) {
	if s == nil {
		return nil, "", false
	}
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	entry, ok := s.resultCache[kind+"\x1f"+key]
	if !ok || !entry.expires.After(s.now()) {
		return nil, "", false
	}
	return entry.data, entry.etag, true
}

func (s *Service) resultPut(kind, key string, value any, ttl time.Duration) string {
	return s.resultPutQuery(kind, key, "", value, ttl)
}

func (s *Service) resultPutQuery(kind, key, signature string, value any, ttl time.Duration) string {
	if s == nil {
		return ""
	}
	now := s.now()
	builtAt := float64(now.UnixNano()) / 1e9
	etag := CacheETag(kind, key, builtAt)
	s.resultMu.Lock()
	if s.resultCache == nil {
		s.resultCache = map[string]cachedResult{}
	}
	s.resultCache[kind+"\x1f"+key] = cachedResult{
		data: value, builtAt: builtAt, etag: etag, expires: now.Add(ttl),
	}
	if signature != "" {
		if s.queryETags == nil {
			s.queryETags = map[string]string{}
		}
		s.queryETags[kind+"\x1f"+signature] = etag
	}
	s.resultMu.Unlock()
	if s.Cache != nil {
		s.Cache.SetETag(kind, key, builtAt)
	}
	return etag
}

// QueryETag returns the validator recorded for one exact query signature.
func (s *Service) QueryETag(kind, signature string) string {
	if s == nil {
		return ""
	}
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	return s.queryETags[kind+"\x1f"+signature]
}

// SnapshotETag returns the validator for one snapshot query.
func (s *Service) SnapshotETag(opts SnapshotOptions) string {
	if s == nil {
		return ""
	}
	window := ParseRange(opts.Range, opts.Since, opts.Until)
	return s.QueryETag("snapshot", SnapshotETagKey(opts.Realm, window))
}

// AnalyticsETag returns the validator for one analytics query.
func (s *Service) AnalyticsETag(opts AnalyticsOptions) string {
	if s == nil {
		return ""
	}
	window := ParseRange(opts.Range, opts.Since, opts.Until)
	return s.QueryETag("analytics", AnalyticsETagKey(opts.Realm, window))
}

// PerfETag returns the validator for one perf query.
func (s *Service) PerfETag(opts PerfOptions) string {
	if s == nil {
		return ""
	}
	sample := opts.Sample
	if sample <= 0 {
		sample = 5000
	}
	if sample < 10 {
		sample = 10
	}
	if sample > 20000 {
		sample = 20000
	}
	window := ParseRange(opts.Range, opts.Since, opts.Until)
	return s.QueryETag("perf", PerfETagKey(opts.Realm, window, sample))
}

// SeriesETag returns the validator for one timeseries query, keyed by the
// resolved bucket step so bucket aliases share the same tag.
func (s *Service) SeriesETag(opts SeriesOptions) string {
	if s == nil {
		return ""
	}
	window, _, _, step := s.seriesPlan(opts)
	return s.QueryETag("series", SeriesETagKeyResolved(opts.Realm, window, step))
}

// ByAccountETag returns the validator for the constant by-account view.
func (s *Service) ByAccountETag() string {
	if s == nil {
		return ""
	}
	return s.QueryETag("by_account", ByAccountETagKey())
}

// RemainingETag returns the validator for the constant remaining view.
func (s *Service) RemainingETag() string {
	if s == nil {
		return ""
	}
	return s.QueryETag("remaining", RemainingETagKey())
}

// ETag signatures deliberately contain every query dimension that changes the
// payload. They are built by the same helpers on the service and HTTP sides so
// one query can never be served another query's validator.

// SnapshotETagKey identifies a snapshot query.
func SnapshotETagKey(realm string, window Range) string {
	return strings.Join([]string{"snapshot", scopeRealm(realm), boundKey(window.Since), boundKey(window.Until)}, "\x1f")
}

// AnalyticsETagKey identifies an analytics query.
func AnalyticsETagKey(realm string, window Range) string {
	return strings.Join([]string{"analytics", scopeRealm(realm), boundKey(window.Since), boundKey(window.Until)}, "\x1f")
}

// PerfETagKey identifies a perf query.
func PerfETagKey(realm string, window Range, sample int) string {
	return strings.Join([]string{"perf", scopeRealm(realm), boundKey(window.Since), boundKey(window.Until), fmt.Sprint(sample)}, "\x1f")
}

// SeriesETagKey identifies a timeseries query by an explicit bucket size.
// Values below the minimum 60s resolve to the same step and therefore the same
// validator; the service itself always uses the fully resolved step.
func SeriesETagKey(realm string, window Range, bucketSeconds int) string {
	step := bucketSeconds
	if step > 0 && step < 60 {
		step = 60
	}
	return SeriesETagKeyResolved(realm, window, step)
}

// ByAccountETagKey is constant: this view has no query dimensions.
func ByAccountETagKey() string { return "by_account" }

// RemainingETagKey is constant: this view has no query dimensions.
func RemainingETagKey() string { return "remaining" }

// ETag returns the validator for the last result of kind/key.
func (s *Service) ETag(kind, key string) string {
	if s == nil {
		return ""
	}
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	return s.resultCache[kind+"\x1f"+key].etag
}

func (s *Service) pricingFingerprint() string {
	if s == nil || s.Pricing == nil {
		return "nil"
	}
	return s.Pricing.Fingerprint()
}

// DefaultRealmValue returns the live default realm, falling back to the
// startup value when no callback is configured.
func (s *Service) DefaultRealmValue() string {
	if s == nil {
		return ""
	}
	if s.DefaultRealmFunc != nil {
		if realm := strings.TrimSpace(s.DefaultRealmFunc()); realm != "" {
			return realm
		}
	}
	return s.DefaultRealm
}

// accountMap returns the current pool view, preferring the live function.
func (s *Service) accountMap() map[string]AccountInfo {
	if s == nil {
		return nil
	}
	if s.AccountsFunc != nil {
		return s.AccountsFunc()
	}
	return s.Accounts
}

// currentAccount returns the representative account for one query realm.
func (s *Service) currentAccount(realm string) AccountInfo {
	if s == nil {
		return AccountInfo{}
	}
	if s.CurrentAccountFunc != nil {
		return s.CurrentAccountFunc(realm)
	}
	return s.CurrentAccount
}

func (s *Service) realmFingerprint() string {
	if s == nil {
		return ""
	}
	accounts := s.accountMap()
	uids := make([]string, 0, len(accounts))
	for uid := range accounts {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	var builder strings.Builder
	builder.WriteString("default=")
	builder.WriteString(s.DefaultRealmValue())
	for _, uid := range uids {
		builder.WriteString("|")
		builder.WriteString(uid)
		builder.WriteString("=")
		builder.WriteString(accounts[uid].Realm)
	}
	return builder.String()
}

func (s *Service) usdCNY() float64 {
	if s == nil || s.Pricing == nil {
		return 0
	}
	return s.Pricing.USDCNY()
}

func (s *Service) costFor(row map[string]any) pricing.Cost {
	if s == nil || s.Pricing == nil {
		return pricing.Cost{}
	}
	return s.Pricing.CostForRow(row)
}

// newRowPricer loads the pricing tables once for a batch/scan. A nil result
// means pricing is not configured; callers fall back to costFor.
func (s *Service) newRowPricer() *pricing.RowPricer {
	if s == nil || s.Pricing == nil {
		return nil
	}
	return s.Pricing.NewRowPricer()
}

func (s *Service) costForWith(pricer *pricing.RowPricer, row map[string]any) pricing.Cost {
	if pricer != nil {
		return pricer.CostForRow(row)
	}
	return s.costFor(row)
}

func (s *Service) rowRealm(row Row) string {
	if s == nil || s.Store == nil {
		return RowRealm(row)
	}
	return s.Store.rowRealm(row)
}

func scopeRealm(realm string) string {
	realm = strings.TrimSpace(realm)
	if strings.EqualFold(realm, "all") {
		return ""
	}
	return realm
}

func (s *Service) matchesRealm(row Row, realm string) bool {
	realm = scopeRealm(realm)
	return realm == "" || s.rowRealm(row) == realm
}

// scan reads complete newline-terminated rows beginning at offset. A trailing
// fragment is left for the next append, matching v1's scan discipline.
func (s *Service) scan(ctx context.Context, path string, offset int64, fold func(Row) error) (int64, error) {
	if s == nil || s.Store == nil {
		return offset, errors.New("usage: nil service or store")
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return offset, nil
		}
		return offset, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return offset, err
	}
	if info.Size() < offset {
		return 0, errors.New("usage: ledger truncated")
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	reader := bufio.NewReaderSize(file, 256*1024)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return offset, nil
			}
			return offset, readErr
		}
		if !bytes.HasSuffix(line, []byte("\n")) {
			// Trailing fragment: only counted after the newline arrives.
			return offset, nil
		}
		next := offset + int64(len(line))
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(trimmed))
			decoder.UseNumber()
			var row Row
			if err := decoder.Decode(&row); err == nil && row != nil {
				if fold != nil {
					if err := fold(row); err != nil {
						return next, err
					}
				}
			}
		}
		offset = next
		select {
		case <-ctx.Done():
			return offset, ctx.Err()
		default:
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return offset, nil
			}
			return offset, readErr
		}
	}
}

func (s *Service) scanAll(ctx context.Context, fold func(Row) error) (int64, error) {
	offset, err := s.scan(ctx, s.Store.Paths.Log, 0, fold)
	if err == nil && s.AfterScan != nil {
		s.AfterScan()
	}
	return offset, err
}

func number(row Row, key string) float64 {
	value, ok := numericValue(row[key])
	if !ok {
		return 0
	}
	return value
}

func intNumber(row Row, key string) int64 { return int64(number(row, key)) }

func rowString(row Row, key string) string { return stringValue(row, key) }

func truthyValue(value any) bool { return truthy(value) }

func formatISO(epoch float64) string {
	return time.Unix(int64(epoch), 0).Local().Format("2006-01-02 15:04:05")
}

func isoAt(epoch float64) string {
	if epoch <= 0 {
		return ""
	}
	return time.Unix(int64(epoch), 0).Local().Format("2006-01-02 15:04:05")
}

// tailLines returns up to the last n non-empty lines, oldest first.
func tailLines(path string, n int) ([][]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	const chunk = 256 * 1024
	position := info.Size()
	var buffer []byte
	lines := make([][]byte, 0, n)
	for position > 0 && len(lines) < n {
		step := int64(chunk)
		if position < step {
			step = position
		}
		position -= step
		piece := make([]byte, int(step))
		if _, err := file.ReadAt(piece, position); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		buffer = append(piece, buffer...)
		parts := bytes.Split(buffer, []byte("\n"))
		buffer = append([]byte(nil), parts[0]...)
		for index := len(parts) - 1; index >= 1 && len(lines) < n; index-- {
			trimmed := bytes.TrimSpace(parts[index])
			if len(trimmed) > 0 {
				lines = append(lines, trimmed)
			}
		}
	}
	if len(lines) < n {
		trimmed := bytes.TrimSpace(buffer)
		if len(trimmed) > 0 {
			lines = append(lines, trimmed)
		}
	}
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	return lines, nil
}

func decodeUsageLine(line []byte) (Row, bool) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var row Row
	if err := decoder.Decode(&row); err != nil || row == nil {
		return nil, false
	}
	return row, true
}

func (s *Service) keyDisplay(id string) string {
	if id == "" {
		return ""
	}
	if s.KeyName != nil {
		if name := strings.TrimSpace(s.KeyName(id)); name != "" {
			return name
		}
	}
	for _, key := range s.Keys {
		if key.ID == id {
			if strings.TrimSpace(key.Name) != "" {
				return key.Name
			}
			return id
		}
	}
	return id
}

func (s *Service) keyKnown(id string) bool {
	if s.Keys == nil {
		return id != ""
	}
	for _, key := range s.Keys {
		if key.ID == id {
			return true
		}
	}
	return false
}

func (s *Service) keyInfo(id string) (KeyInfo, bool) {
	for _, key := range s.Keys {
		if key.ID == id {
			return key, true
		}
	}
	return KeyInfo{}, false
}

func sortedKeys(source map[string]int64) []string {
	out := make([]string, 0, len(source))
	for key := range source {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func copyRow(row Row) map[string]any {
	out := make(map[string]any, len(row))
	for key, value := range row {
		out[key] = value
	}
	return out
}

func (s *Service) recentKeyName(id string) string { return s.keyDisplay(id) }

func fmtAny(value any) string { return fmt.Sprint(value) }
