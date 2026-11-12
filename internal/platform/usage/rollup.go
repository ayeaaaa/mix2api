package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/pricing"
)

const (
	rollupName   = "usage-rollup.sqlite3"
	rollupSchema = 1
	rollupTable  = "analytics_daily"
	rollupMeta   = "rollup_meta"
	rollupPosKey = "position"
	rollupVerKey = "schema"
)

// Rollup is a persistent daily/account/model/key aggregate for analytics. It
// is intentionally an accelerator, not a source of truth: a corrupt, missing
// or stale table falls back to the JSONL scan.
type Rollup struct {
	dir     string
	pricing *pricing.Snapshot

	mu sync.Mutex
	db *sql.DB
}

func NewRollup(dir string, price *pricing.Snapshot) *Rollup {
	return &Rollup{dir: dir, pricing: price}
}

func (r *Rollup) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db != nil {
		err := r.db.Close()
		r.db = nil
		return err
	}
	return nil
}

func (r *Rollup) openLocked() (*sql.DB, error) {
	if r.db != nil {
		return r.db, nil
	}
	if r.dir == "" {
		return nil, errors.New("usage: empty rollup directory")
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(r.dir, rollupName)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=DELETE",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	schema := []string{
		`CREATE TABLE IF NOT EXISTS ` + rollupMeta + ` (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ` + rollupTable + ` (
			day TEXT NOT NULL,
			realm TEXT NOT NULL,
			account TEXT NOT NULL,
			model TEXT NOT NULL,
			key_bucket TEXT NOT NULL,
			requests INTEGER NOT NULL DEFAULT 0,
			errors INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			credit REAL NOT NULL DEFAULT 0,
			cost_cny REAL NOT NULL DEFAULT 0,
			ttft_sum REAL NOT NULL DEFAULT 0,
			ttft_n INTEGER NOT NULL DEFAULT 0,
			speed_sum REAL NOT NULL DEFAULT 0,
			speed_n INTEGER NOT NULL DEFAULT 0,
			elapsed_sum REAL NOT NULL DEFAULT 0,
			elapsed_n INTEGER NOT NULL DEFAULT 0,
			max_at REAL NOT NULL DEFAULT 0,
			PRIMARY KEY(day, realm, account, model, key_bucket)
		)`,
	}
	for _, statement := range schema {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	var version string
	err = db.QueryRow(`SELECT value FROM `+rollupMeta+` WHERE key = ?`, rollupVerKey).Scan(&version)
	if err == nil && version != "1" {
		if _, dropErr := db.Exec(`DROP TABLE IF EXISTS ` + rollupTable); dropErr != nil {
			_ = db.Close()
			return nil, dropErr
		}
		if _, schemaErr := db.Exec(schema[1]); schemaErr != nil {
			_ = db.Close()
			return nil, schemaErr
		}
		_, _ = db.Exec(`DELETE FROM `+rollupMeta+` WHERE key = ?`, rollupPosKey)
	}
	if _, err := db.Exec(`INSERT INTO `+rollupMeta+`(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, rollupVerKey, fmt.Sprint(rollupSchema)); err != nil {
		_ = db.Close()
		return nil, err
	}
	r.db = db
	return db, nil
}

// Analytics builds the v1 analytics maps from the persistent table. The
// returned offset is the log offset covered by the table.
func (r *Rollup) Analytics(ctx context.Context, service *Service, realm string) (analyticsMaps, int64, bool) {
	if r == nil || service == nil || service.Store == nil {
		return analyticsMaps{}, 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	db, err := r.openLocked()
	if err != nil {
		return analyticsMaps{}, 0, false
	}
	offset, ok := r.ensureLocked(ctx, service, db)
	if !ok {
		return analyticsMaps{}, 0, false
	}
	query := `SELECT day, realm, account, model, key_bucket,
		requests, errors, prompt_tokens, completion_tokens, reasoning_tokens,
		cached_tokens, total_tokens, credit, cost_cny,
		ttft_sum, ttft_n, speed_sum, speed_n, elapsed_sum, elapsed_n, max_at
		FROM ` + rollupTable + ` WHERE (? = '' OR realm = ?)`
	rows, err := db.QueryContext(ctx, query, realm, realm)
	if err != nil {
		return analyticsMaps{}, 0, false
	}
	defer rows.Close()

	all := newAnalyticsMaps()
	for rows.Next() {
		var (
			day, rowRealm, account, model, keyBucket string
			stat                                     AnalyticsStat
			maxAt                                    float64
		)
		if err := rows.Scan(
			&day, &rowRealm, &account, &model, &keyBucket,
			&stat.Requests, &stat.Errors, &stat.PromptTokens,
			&stat.CompletionTokens, &stat.ReasoningTokens, &stat.CachedTokens,
			&stat.TotalTokens, &stat.Credit, &stat.CostCNY,
			&stat.TTFTSum, &stat.TTFTN, &stat.SpeedSum, &stat.SpeedN,
			&stat.ElapsedSum, &stat.ElapsedN, &maxAt,
		); err != nil {
			return analyticsMaps{}, 0, false
		}
		addRollupAggregate(&all, day, rowRealm, account, model, keyBucket, stat, maxAt)
	}
	if err := rows.Err(); err != nil {
		return analyticsMaps{}, 0, false
	}
	return all, offset, true
}

// ensureLocked advances the table to the current log tail. The caller holds mu.
func (r *Rollup) ensureLocked(ctx context.Context, service *Service, db *sql.DB) (int64, bool) {
	logPath := service.Store.Paths.Log
	offset := r.positionLocked(db, logPath)
	info, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, true
		}
		return 0, false
	}
	if info.Size() <= offset {
		return info.Size(), offset <= info.Size()
	}

	pricer := service.newRowPricer()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO `+rollupTable+` (
		day, realm, account, model, key_bucket,
		requests, errors, prompt_tokens, completion_tokens, reasoning_tokens,
		cached_tokens, total_tokens, credit, cost_cny,
		ttft_sum, ttft_n, speed_sum, speed_n, elapsed_sum, elapsed_n, max_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(day, realm, account, model, key_bucket) DO UPDATE SET
		requests = requests + excluded.requests,
		errors = errors + excluded.errors,
		prompt_tokens = prompt_tokens + excluded.prompt_tokens,
		completion_tokens = completion_tokens + excluded.completion_tokens,
		reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
		cached_tokens = cached_tokens + excluded.cached_tokens,
		total_tokens = total_tokens + excluded.total_tokens,
		credit = credit + excluded.credit,
		cost_cny = cost_cny + excluded.cost_cny,
		ttft_sum = ttft_sum + excluded.ttft_sum,
		ttft_n = ttft_n + excluded.ttft_n,
		speed_sum = speed_sum + excluded.speed_sum,
		speed_n = speed_n + excluded.speed_n,
		elapsed_sum = elapsed_sum + excluded.elapsed_sum,
		elapsed_n = elapsed_n + excluded.elapsed_n,
		max_at = MAX(max_at, excluded.max_at)`)
	if err != nil {
		return 0, false
	}
	defer statement.Close()

	type key struct {
		day, realm, account, model, keyBucket string
	}
	batch := make(map[key]rollupValues)
	newOffset, err := service.scan(ctx, logPath, offset, func(row Row) error {
		values, ok := r.rowValues(service, pricer, row)
		if !ok {
			return nil
		}
		k := key{values.day, values.realm, values.account, values.model, values.keyBucket}
		existing, ok := batch[k]
		if !ok {
			existing = values
		} else {
			existing.stat = addAnalyticsStat(existing.stat, values.stat)
			if values.maxAt > existing.maxAt {
				existing.maxAt = values.maxAt
			}
		}
		batch[k] = existing
		return nil
	})
	if err != nil {
		return 0, false
	}
	for _, values := range batch {
		stat := values.stat
		if _, err := statement.ExecContext(ctx,
			values.day, values.realm, values.account, values.model, values.keyBucket,
			stat.Requests, stat.Errors, stat.PromptTokens, stat.CompletionTokens,
			stat.ReasoningTokens, stat.CachedTokens, stat.TotalTokens,
			stat.Credit, stat.CostCNY, stat.TTFTSum, stat.TTFTN,
			stat.SpeedSum, stat.SpeedN, stat.ElapsedSum, stat.ElapsedN, values.maxAt,
		); err != nil {
			return 0, false
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false
	}
	if position, ok := CachePosition(logPath, newOffset); ok {
		if encoded, err := json.Marshal(position); err == nil {
			_, _ = db.ExecContext(ctx, `INSERT INTO `+rollupMeta+`(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, rollupPosKey, string(encoded))
		}
	}
	return newOffset, true
}

type rollupValues struct {
	day, realm, account, model, keyBucket string
	stat                                  AnalyticsStat
	maxAt                                 float64
}

func (r *Rollup) rowValues(service *Service, pricer *pricing.RowPricer, row Row) (rollupValues, bool) {
	outcome := RowOutcome(row)
	if outcome == "client_aborted" {
		return rollupValues{}, false
	}
	at, ok := RowAt(row)
	if !ok || at <= 0 {
		return rollupValues{}, false
	}
	account := rowString(row, "account")
	if account == "" {
		account = "(unattributed)"
	}
	model := rowModelKey(row)
	if model == "" {
		model = "(unknown)"
	}
	keyBucket := KeyBucketBefore
	if _, exists := row["key"]; exists {
		keyBucket = rowString(row, "key")
		if keyBucket == "" {
			keyBucket = KeyBucketAnon
		}
	}
	stat := AnalyticsStat{}
	if outcome == "completed" {
		stat.Requests = 1
	} else {
		stat.Errors = 1
	}
	stat.PromptTokens = intNumber(row, "prompt_tokens")
	stat.CompletionTokens = intNumber(row, "completion_tokens")
	stat.ReasoningTokens = intNumber(row, "reasoning_tokens")
	stat.CachedTokens = intNumber(row, "cached_tokens")
	stat.TotalTokens = intNumber(row, "total_tokens")
	stat.Credit = number(row, "credit")
	if cost := service.costForWith(pricer, row); cost.Known {
		stat.CostCNY = cost.CNY
	}
	if value := number(row, "ttft_ms"); value != 0 {
		stat.TTFTSum = value
		stat.TTFTN = 1
	}
	if value := number(row, "tokens_per_sec"); value != 0 {
		stat.SpeedSum = value
		stat.SpeedN = 1
	}
	if value := number(row, "elapsed_ms"); value != 0 {
		stat.ElapsedSum = value
		stat.ElapsedN = 1
	}
	return rollupValues{
		day:       rollupDayKey(at),
		realm:     service.rowRealm(row),
		account:   account,
		model:     model,
		keyBucket: keyBucket,
		stat:      stat,
		maxAt:     at,
	}, true
}

func rollupDayKey(at float64) string {
	return analyticsDayKey(time.Unix(int64(at), 0))
}

func (r *Rollup) positionLocked(db *sql.DB, logPath string) int64 {
	var raw string
	if err := db.QueryRow(`SELECT value FROM `+rollupMeta+` WHERE key = ?`, rollupPosKey).Scan(&raw); err != nil {
		return 0
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return 0
	}
	offset, ok := ValidateCachePosition(entry, logPath)
	if !ok {
		_, _ = db.Exec(`DELETE FROM ` + rollupTable)
		_, _ = db.Exec(`DELETE FROM `+rollupMeta+` WHERE key = ?`, rollupPosKey)
		return 0
	}
	return offset
}

func addRollupAggregate(all *analyticsMaps, day, realm, account, model, keyBucket string, stat AnalyticsStat, maxAt float64) {
	dayMaps, ok := all.Days[day]
	if !ok {
		dayMaps = newAnalyticsMaps()
	}
	all.Summary = addAnalyticsStat(all.Summary, stat)
	dayMaps.Summary = addAnalyticsStat(dayMaps.Summary, stat)

	allAccount := all.Accts[account]
	if allAccount == nil {
		allAccount = &AnalyticsAccount{UID: account, Nickname: account, WindowModels: map[string]AnalyticsModel{}, AllModels: map[string]AnalyticsModel{}}
		all.Accts[account] = allAccount
	}
	if allAccount.Realm == "" {
		allAccount.Realm = realm
	}
	allAccount.AllTime = addAnalyticsStat(allAccount.AllTime, stat)
	modelValue := AnalyticsModel{Requests: stat.Requests, Tokens: stat.TotalTokens, Reasoning: stat.ReasoningTokens, CostCNY: stat.CostCNY}
	if stat.Requests > 0 {
		existing := allAccount.AllModels[model]
		existing.Requests += modelValue.Requests
		existing.Tokens += modelValue.Tokens
		existing.Reasoning += modelValue.Reasoning
		existing.CostCNY += modelValue.CostCNY
		allAccount.AllModels[model] = existing
	}

	dayAccount := dayMaps.Accts[account]
	if dayAccount == nil {
		dayAccount = &AnalyticsAccount{UID: account, Nickname: account, Realm: realm, WindowModels: map[string]AnalyticsModel{}, AllModels: map[string]AnalyticsModel{}}
		dayMaps.Accts[account] = dayAccount
	}
	dayAccount.Window = addAnalyticsStat(dayAccount.Window, stat)
	if stat.Requests > 0 {
		dayModel := dayAccount.WindowModels[model]
		dayModel.Requests += modelValue.Requests
		dayModel.Tokens += modelValue.Tokens
		dayModel.Reasoning += modelValue.Reasoning
		dayModel.CostCNY += modelValue.CostCNY
		dayAccount.WindowModels[model] = dayModel
	}

	allModel := all.Models[model]
	if allModel == nil {
		allModel = &AnalyticsModelRow{Model: model}
		all.Models[model] = allModel
	}
	allModel.AllTime = addAnalyticsStat(allModel.AllTime, stat)
	dayModelRow := dayMaps.Models[model]
	if dayModelRow == nil {
		dayModelRow = &AnalyticsModelRow{Model: model}
		dayMaps.Models[model] = dayModelRow
	}
	dayModelRow.Window = addAnalyticsStat(dayModelRow.Window, stat)

	allKey := all.Keys[keyBucket]
	if allKey == nil {
		allKey = &AnalyticsKey{Key: keyBucket, WindowModels: map[string]AnalyticsModel{}, AllModels: map[string]AnalyticsModel{}, Realms: map[string]int64{}}
		all.Keys[keyBucket] = allKey
	}
	allKey.AllTime = addAnalyticsStat(allKey.AllTime, stat)
	allKey.Realms[realm]++
	if maxAt > allKey.LastAt {
		allKey.LastAt = maxAt
	}
	if stat.Requests > 0 {
		keyModel := allKey.AllModels[model]
		keyModel.Requests += modelValue.Requests
		keyModel.Tokens += modelValue.Tokens
		keyModel.Reasoning += modelValue.Reasoning
		keyModel.CostCNY += modelValue.CostCNY
		allKey.AllModels[model] = keyModel
	}

	dayKey := dayMaps.Keys[keyBucket]
	if dayKey == nil {
		dayKey = &AnalyticsKey{Key: keyBucket, WindowModels: map[string]AnalyticsModel{}, AllModels: map[string]AnalyticsModel{}, Realms: map[string]int64{}}
		dayMaps.Keys[keyBucket] = dayKey
	}
	dayKey.Window = addAnalyticsStat(dayKey.Window, stat)
	if stat.Requests > 0 {
		dayKeyModel := dayKey.WindowModels[model]
		dayKeyModel.Requests += modelValue.Requests
		dayKeyModel.Tokens += modelValue.Tokens
		dayKeyModel.Reasoning += modelValue.Reasoning
		dayKeyModel.CostCNY += modelValue.CostCNY
		dayKey.WindowModels[model] = dayKeyModel
	}

	if maxAt > all.MaxAt {
		all.MaxAt = maxAt
	}
	if all.DaysFloor == "" || day < all.DaysFloor {
		all.DaysFloor = day
	}
	dayMaps.Days = nil
	all.Days[day] = dayMaps
}
