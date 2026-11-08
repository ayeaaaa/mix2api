package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// PointsBalancePath is the v1 points balance endpoint.
	PointsBalancePath = "/api/web/points/v1/balance"
	// PointsBillsPath is the v1 points bill endpoint.
	PointsBillsPath = "/api/web/points/v2/bills"
	// PointsTasksPath is the v1 points-task endpoint.
	PointsTasksPath = "/api/web/points/v1/tasks"
	// PointsDesktopGrantPath claims the one-time desktop login reward.
	PointsDesktopGrantPath = "/api/web/desktop/v1/login/points/grant"
	// PointsMobileGrantPath claims the one-time mobile-app login reward.
	PointsMobileGrantPath = "/api/web/mobile/v1/login/points/grant"

	// MobileClientPlatform and MobileClientVersion gate the mobile reward
	// endpoint. Desktop calls keep the catalog identity.
	MobileClientPlatform = "app-android"
	MobileClientVersion  = "v1.1.0"

	// creditsTTL, pointsTasksTTL, and autoClaimTTL mirror v1's cache windows.
	creditsTTL     = 60 * time.Second
	pointsTasksTTL = 300 * time.Second
	autoClaimTTL   = pointsTasksTTL

	// creditsBillLimit and creditsMaxPages mirror v1's bounded bill scan.
	creditsBillLimit = 100
	creditsMaxPages  = 3

	// maxPointsErrorRunes and maxPointsStepErrorRunes mirror v1's redacted
	// account-level and per-step error caps.
	maxPointsErrorRunes     = 300
	maxPointsStepErrorRunes = 200
)

// CreditsResult is one row of v1's POST /raccoon/credits/refresh response.
// A failed row carries a redacted message and no credits snapshot.
type CreditsResult struct {
	OK      bool             `json:"ok"`
	UID     string           `json:"uid"`
	Credits *CreditsSnapshot `json:"credits,omitempty"`
	Message string           `json:"message,omitempty"`
}

// FetchCredits returns one account's balance plus recent bills, cached for 60
// seconds unless force is set. It normalizes v1's public credit view and never
// exposes upstream IDs. A 401/403 triggers exactly one forced refresh for the
// credential generation that produced the rejected token, then the retry's
// failure is terminal for this call.
func (p *Pool) FetchCredits(ctx context.Context, a *Account, force bool) (*CreditsSnapshot, error) {
	if p == nil {
		return nil, errors.New("raccoon: pool is nil")
	}
	if ctx == nil {
		return nil, errors.New("raccoon: nil context")
	}
	if a == nil {
		return nil, errors.New("raccoon: account is nil")
	}
	if a.isDeleted() {
		return nil, ErrAccountDeleted
	}

	if !force {
		if cached, ok := a.freshCredits(p.timeNow()); ok {
			return cached, nil
		}
	}

	client := p.HTTPClient()
	if client == nil {
		return nil, errors.New("raccoon: points http client is not configured")
	}
	tokens := p.TokenManager()

	a.creditsMu.Lock()
	defer a.creditsMu.Unlock()

	now := p.timeNow()
	if !force {
		// Another request may have filled the cache while this one waited.
		if cached, ok := a.freshCredits(now); ok {
			return cached, nil
		}
	}
	if err := ensurePointsToken(ctx, tokens, a); err != nil {
		return nil, err
	}

	balance, err := p.pointsWithRefresh(
		ctx, a, client, tokens, http.MethodGet, PointsBalancePath, nil, false,
	)
	if err != nil {
		return nil, err
	}
	data := pointsData(balance)
	if data == nil {
		return nil, errors.New("raccoon: balance returned no data")
	}

	items, err := p.fetchBills(ctx, a, client, tokens)
	if err != nil {
		return nil, err
	}

	snapshot := buildCredits(data, items, now)
	a.mu.Lock()
	a.Credits = snapshot
	a.creditsFetchedAt = now
	result := publicCredits(snapshot)
	a.mu.Unlock()
	return result, nil
}

// RefreshCredits force-refreshes one account (when uid is set) or every loaded
// account. Per-account upstream failures are aggregated as redacted rows so a
// single bad account cannot hide the rest of the pool, matching v1.
func (p *Pool) RefreshCredits(ctx context.Context, uid string) ([]CreditsResult, error) {
	if p == nil {
		return nil, errors.New("raccoon: pool is nil")
	}
	if ctx == nil {
		return nil, errors.New("raccoon: nil context")
	}

	uid = strings.TrimSpace(uid)
	var targets []*Account
	if uid != "" {
		account := p.Get(uid)
		if account == nil {
			return nil, ErrAccountNotFound
		}
		targets = []*Account{account}
	} else {
		targets = p.Accounts()
	}

	results := make([]CreditsResult, 0, len(targets))
	for _, account := range targets {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		snapshot, err := p.FetchCredits(ctx, account, true)
		// A successful refresh may rotate the JWT subject; report the live UID
		// exactly as v1 appends acct.uid after the fetch.
		currentUID, _ := account.currentUID()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return results, ctxErr
			}
			results = append(results, CreditsResult{
				OK:      false,
				UID:     currentUID,
				Message: redactedPointsError(err, account, maxPointsErrorRunes),
			})
			continue
		}
		results = append(results, CreditsResult{
			OK:      true,
			UID:     currentUID,
			Credits: snapshot,
		})
	}
	return results, nil
}

// fetchBills reads at most creditsMaxPages pages with v1's limit and type.
// A malformed page terminates the scan with the pages already collected; a
// 401/403 here is terminal because the balance call already had its refresh.
func (p *Pool) fetchBills(
	ctx context.Context,
	a *Account,
	client *HTTPClient,
	tokens *TokenManager,
) ([]map[string]any, error) {
	items := make([]map[string]any, 0)
	cursor := ""
	for page := 0; page < creditsMaxPages; page++ {
		payload, _, err := p.requestPoints(
			ctx, a, client, tokens, http.MethodGet, pointsBillsPath(cursor), nil, false,
		)
		if err != nil {
			return nil, err
		}
		data := pointsData(payload)
		if data == nil {
			break
		}
		if rawItems, ok := data["items"].([]any); ok {
			for _, rawItem := range rawItems {
				if item, ok := rawItem.(map[string]any); ok {
					items = append(items, item)
				}
			}
		}
		nextCursor := pointsTruthyText(data["next_cursor"])
		if nextCursor == "" {
			if paging, ok := data["paging"].(map[string]any); ok {
				nextCursor = pointsTruthyText(paging["next_cursor"])
			}
		}
		hasMore, hasMoreSet := data["has_more"]
		if !hasMoreSet || hasMore == nil {
			hasMore = nextCursor != ""
		}
		if !pointsTruthy(hasMore) || nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return items, nil
}

func pointsBillsPath(cursor string) string {
	// Keep v1's literal parameter order: paging.limit, type, then cursor.
	path := PointsBillsPath + "?paging.limit=" + strconv.Itoa(creditsBillLimit) + "&type=all"
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	return path
}

// buildCredits reproduces v1's normalized credit view. Only enumerated balance
// fields and five bill display fields survive; today/week totals count negative
// bill points in the local calendar day and the trailing seven days.
func buildCredits(data map[string]any, items []map[string]any, now time.Time) *CreditsSnapshot {
	available := pointsInt(data["available_points"])
	consumed := 0
	todayUsed := 0
	weekUsed := 0
	today := now.In(time.Local).Format("2006-01-02")
	weekFloor := now.Add(-7 * 24 * time.Hour).Unix()
	recent := make([]map[string]any, 0, 10)

	for _, item := range items {
		points := pointsInt(item["points"])
		when, parsed := parsePointsTime(item["created_at"])
		if points < 0 {
			used := -points
			consumed += used
			if parsed {
				if when.In(time.Local).Format("2006-01-02") == today {
					todayUsed += used
				}
				if when.Unix() >= weekFloor {
					weekUsed += used
				}
			}
		}
		if len(recent) < 10 {
			recent = append(recent, map[string]any{
				"event":       pointsTruthyText(item["event_name"]),
				"points":      points,
				"created_at":  pointsTruthyText(item["created_at"]),
				"biz_type":    pointsTruthyText(item["biz_type"]),
				"client_type": pointsTruthyText(item["client_type"]),
			})
		}
	}

	return &CreditsSnapshot{
		Available:       available,
		Remain:          available,
		Amount:          available + consumed,
		Used:            consumed,
		Daily:           pointsInt(data["daily_points"]),
		Monthly:         pointsInt(data["monthly_points"]),
		Reward:          pointsInt(data["reward_points"]),
		Topup:           pointsInt(data["topup_points"]),
		EnterpriseTopup: pointsInt(data["enterprise_topup_points"]),
		Total:           pointsInt(data["total_chargeable_points"]),
		TodayUsed:       todayUsed,
		WeekUsed:        weekUsed,
		Unit:            "points",
		FetchedAt:       unixFloat(now),
		Recent:          recent,
	}
}

// pointsWithRefresh sends one points request and retries once after a forced
// refresh when the first response is 401/403. The generation pins the token
// that was actually rejected so an already-completed concurrent refresh is not
// repeated. No tokens are ever included in returned errors.
func (p *Pool) pointsWithRefresh(
	ctx context.Context,
	a *Account,
	client *HTTPClient,
	tokens *TokenManager,
	method, path string,
	body []byte,
	mobile bool,
) (map[string]any, error) {
	payload, generation, err := p.requestPoints(ctx, a, client, tokens, method, path, body, mobile)
	if err == nil || !IsAuthError(err) || tokens == nil {
		return payload, err
	}
	if refreshErr := tokens.RefreshIfGeneration(ctx, a, generation); refreshErr != nil {
		return nil, refreshErr
	}
	payload, _, err = p.requestPoints(ctx, a, client, tokens, method, path, body, mobile)
	return payload, err
}

// requestPoints sends one points request and returns the credential generation
// that produced its bearer header.
func (p *Pool) requestPoints(
	ctx context.Context,
	a *Account,
	client *HTTPClient,
	tokens *TokenManager,
	method, path string,
	body []byte,
	mobile bool,
) (map[string]any, uint64, error) {
	generation := uint64(0)
	if tokens != nil {
		generation = tokens.Generation(a)
	}
	snapshot := a.refreshCredentials()
	if snapshot.Deleted {
		return nil, 0, ErrAccountDeleted
	}
	// Pin the generation that produced the actual header token, not a stale
	// pre-refresh capture.
	if snapshot.Generation != generation {
		generation = snapshot.Generation
	}

	headers := pointsHeaders(snapshot.Auth)
	if mobile {
		headers.Set("X-Client-Platform", MobileClientPlatform)
		headers.Set("X-Client-Version", MobileClientVersion)
	}
	payload, err := client.DoJSON(
		ctx, method, path, headers, body, p.ProxyURL(a),
		authSecretValues(snapshot.Auth)...,
	)
	if err != nil {
		return nil, generation, err
	}
	return payload, generation, nil
}

func ensurePointsToken(ctx context.Context, tokens *TokenManager, a *Account) error {
	if tokens == nil {
		return nil
	}
	return tokens.Ensure(ctx, a)
}

// pointsHeaders reuses the desktop client identity used by catalog and chat.
// Mobile reward calls override the platform and version pair.
func pointsHeaders(auth Auth) http.Header {
	return catalogHeaders(auth)
}

// pointsData returns the object under "data", or nil when the upstream shape
// is unusable. It never copies upstream IDs.
func pointsData(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	data, _ := payload["data"].(map[string]any)
	return data
}

// pointsInt mirrors v1 _as_points / int(float(value)): invalid, NaN, and
// infinite values are zero, numeric strings are accepted, and fractions are
// truncated toward zero.
func pointsInt(value any) int {
	switch typed := value.(type) {
	case bool:
		if typed {
			return 1
		}
		return 0
	}
	number, ok := numericValue(value)
	if !ok {
		return 0
	}
	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1
	if number >= float64(maxInt) {
		return maxInt
	}
	if number <= float64(minInt) {
		return minInt
	}
	return int(number)
}

// pointsText mirrors Python str() for the scalar fields v1 keeps.
func pointsText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	default:
		return fmt.Sprint(typed)
	}
}

// pointsTruthyText mirrors Python `str(value or "")`: falsy scalars become the
// empty string, while truthy values keep pointsText's rendering.
func pointsTruthyText(value any) string {
	if !pointsTruthy(value) {
		return ""
	}
	return pointsText(value)
}

// pointsTruthy mirrors Python truthiness for has_more values.
func pointsTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		number, err := strconv.ParseFloat(typed.String(), 64)
		return err == nil && number != 0
	case float64:
		return typed != 0
	case float32:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	default:
		return false
	}
}

// parsePointsTime accepts the ISO-8601 shapes v1 records: RFC3339 with an
// offset or Z, naive timestamps (interpreted as UTC), and date-only values.
func parsePointsTime(value any) (time.Time, bool) {
	text := strings.TrimSpace(pointsText(value))
	if text == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return parsed, true
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-0700",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if parsed, err := time.ParseInLocation(layout, text, time.UTC); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func unixFloat(value time.Time) float64 {
	return float64(value.Unix()) + float64(value.Nanosecond())/float64(time.Second)
}

// freshCredits returns a public copy of the cached, unexpired credit snapshot.
// The exact account stamp wins; manually built snapshots fall back to their
// public fetched_at value.
func (a *Account) freshCredits(now time.Time) (*CreditsSnapshot, bool) {
	if a == nil {
		return nil, false
	}
	a.mu.Lock()
	raw := a.Credits
	if raw == nil {
		a.mu.Unlock()
		return nil, false
	}
	stamp := a.creditsFetchedAt
	if stamp.IsZero() {
		if value, ok := numericValue(raw.FetchedAt); ok && value > 0 {
			stamp = epochSecondsToTime(value)
		}
	}
	cached := publicCredits(raw)
	a.mu.Unlock()
	if stamp.IsZero() || now.Sub(stamp) >= creditsTTL {
		return nil, false
	}
	return cached, true
}

// redactedPointsError renders an upstream failure for a panel/result message.
// Known credentials are removed, plus token-shaped text, and the result is
// capped like v1.
func redactedPointsError(err error, a *Account, limit int) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if a != nil {
		text = redactText(text, authSecretValues(a.authSnapshot())...)
	}
	return truncateRunes(text, limit)
}

// authSnapshot returns a copy of the current credentials without exposing any
// lock to callers.
func (a *Account) authSnapshot() Auth {
	if a == nil {
		return Auth{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Auth
}
