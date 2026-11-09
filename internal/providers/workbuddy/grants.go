package workbuddy

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

const (
	grantStatusActive    = "active"
	grantStatusAvailable = "available"
	grantStatusUsedUp    = "used_up"
	grantStatusExpired   = "expired"
)

// GrantAction is the local checkin/daily-chat attempt associated with a grant.
// This slice has no activity history yet, so rows carry nil; the fields keep
// v1's payload shape for the task/activity slice that follows.
type GrantAction struct {
	Task         string `json:"task"`
	TS           string `json:"ts"`
	OK           bool   `json:"ok"`
	DeltaSeconds int    `json:"delta_seconds"`
}

// GrantRow is one cached credit package flattened into v1's grants table.
type GrantRow struct {
	UID         string       `json:"uid"`
	Nickname    string       `json:"nickname"`
	Realm       string       `json:"realm"`
	Name        string       `json:"name"`
	Product     string       `json:"product"`
	GrantReason string       `json:"grant_reason"`
	Size        float64      `json:"size"`
	Remain      float64      `json:"remain"`
	Used        float64      `json:"used"`
	Unit        string       `json:"unit"`
	CreateAt    *float64     `json:"create_at"`
	CreateISO   string       `json:"create_iso"`
	ExpireAt    *float64     `json:"expire_at"`
	ExpireISO   string       `json:"expire_iso"`
	NoExpiry    bool         `json:"no_expiry"`
	DaysLeft    *float64     `json:"days_left"`
	Status      string       `json:"status"`
	Action      *GrantAction `json:"action"`
}

// GrantSummary is the aggregate row directly under v1's grants payload.
type GrantSummary struct {
	Count    int     `json:"count"`
	Size     float64 `json:"size"`
	Remain   float64 `json:"remain"`
	Used     float64 `json:"used"`
	Accounts int     `json:"accounts"`
}

// GrantsResult is the read-only v1 grants payload. FetchedAt/FetchedISO
// describe the newest credit snapshot and are null when no snapshot exists.
type GrantsResult struct {
	OK           bool         `json:"ok"`
	GeneratedAt  float64      `json:"generated_at"`
	GeneratedISO string       `json:"generated_iso"`
	FetchedAt    *float64     `json:"fetched_at"`
	FetchedISO   *string      `json:"fetched_iso"`
	Rows         []GrantRow   `json:"rows"`
	Summary      GrantSummary `json:"summary"`
}

// FetchGrants flattens the cached credit packages into v1's grants rows and
// summary. It performs no upstream request; call RefreshCredits first when the
// numbers must be current.
func (p *Provider) FetchGrants(ctx context.Context) (GrantsResult, error) {
	now := time.Now()
	result := GrantsResult{
		OK:           true,
		GeneratedAt:  float64(now.UnixNano()) / float64(time.Second),
		GeneratedISO: now.Format(creditsTimeLayout),
		Rows:         []GrantRow{},
	}
	if p == nil {
		return result, errors.New("workbuddy: provider is nil")
	}
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	now = p.creditsNowTime()
	result.GeneratedAt = float64(now.UnixNano()) / float64(time.Second)
	result.GeneratedISO = now.Format(creditsTimeLayout)

	pool := p.Pool()
	if pool == nil {
		return result, errors.New("workbuddy: account pool is nil")
	}

	fetched := 0.0
	hasFetched := false
	accountsWithRows := make(map[string]bool)
	for _, account := range pool.Accounts() {
		snapshot := creditSnapshotOf(account)
		if snapshot == nil {
			continue
		}
		if snapshot.UpdatedAt > fetched {
			fetched = snapshot.UpdatedAt
			hasFetched = true
		}
		account.mu.RLock()
		uid := strings.TrimSpace(account.Auth.UID)
		nickname := strings.TrimSpace(account.Auth.Nickname)
		realm := strings.TrimSpace(account.Auth.Realm)
		account.mu.RUnlock()
		if nickname == "" {
			nickname = shortUID(uid)
		}

		for _, pkg := range snapshot.Packages {
			name := strings.TrimSpace(pkg.Name)
			if name == "" {
				name = strings.TrimSpace(pkg.PackageCode)
			}
			if name == "" {
				name = "Package"
			}
			product := strings.TrimSpace(pkg.SubProductName)
			if product == "" {
				product = strings.TrimSpace(pkg.ProductName)
			}
			unit := strings.TrimSpace(pkg.Unit)
			if unit == "" {
				unit = "credit"
			}
			expireISO := strings.TrimSpace(pkg.ExpireTime)
			if expireISO == "" {
				expireISO = strings.TrimSpace(pkg.CycleEndTime)
			}
			createAt := creditStampEpoch(pkg.CreateTime)
			expireAt := creditStampEpoch(expireISO)
			result.Rows = append(result.Rows, GrantRow{
				UID:         uid,
				Nickname:    nickname,
				Realm:       realm,
				Name:        name,
				Product:     product,
				GrantReason: pkg.GrantReason,
				Size:        pkg.Size,
				Remain:      pkg.Remain,
				Used:        pkg.Used,
				Unit:        unit,
				CreateAt:    createAt,
				CreateISO:   strings.TrimSpace(pkg.CreateTime),
				ExpireAt:    expireAt,
				ExpireISO:   expireISO,
				NoExpiry:    pkg.NoExpiry,
				DaysLeft:    cloneFloat64Pointer(pkg.DaysLeft),
				Status:      creditGrantStatus(pkg, expireAt, now),
				Action:      nil,
			})
			accountsWithRows[uid] = true
		}
	}

	// Newest first; within one timestamp, nickname then name keep the order
	// stable across refreshes (v1 credit_grants).
	sort.SliceStable(result.Rows, func(i, j int) bool {
		left, right := result.Rows[i], result.Rows[j]
		leftAt, rightAt := 0.0, 0.0
		if left.CreateAt != nil {
			leftAt = *left.CreateAt
		}
		if right.CreateAt != nil {
			rightAt = *right.CreateAt
		}
		if leftAt != rightAt {
			return leftAt > rightAt
		}
		if left.Nickname != right.Nickname {
			return left.Nickname < right.Nickname
		}
		return left.Name < right.Name
	})

	size, remain, used := 0.0, 0.0, 0.0
	for _, row := range result.Rows {
		size += row.Size
		remain += row.Remain
		used += row.Used
	}
	result.Summary = GrantSummary{
		Count:    len(result.Rows),
		Size:     roundCredit(size),
		Remain:   roundCredit(remain),
		Used:     roundCredit(used),
		Accounts: len(accountsWithRows),
	}
	if hasFetched {
		value := fetched
		result.FetchedAt = &value
		iso := time.Unix(int64(fetched), 0).In(time.Local).Format(creditsTimeLayout)
		result.FetchedISO = &iso
	}
	return result, nil
}

func creditGrantStatus(pkg CreditPackage, expireAt *float64, now time.Time) string {
	expired := pkg.IsExpired
	if expireAt != nil && *expireAt <= float64(now.Unix()) {
		expired = true
	}
	if expired {
		return grantStatusExpired
	}
	if pkg.Remain <= 0 {
		return grantStatusUsedUp
	}
	if pkg.InUsage {
		return grantStatusActive
	}
	return grantStatusAvailable
}

// creditStampEpoch converts a package ISO timestamp to local epoch seconds.
// Unrecognised values return nil, matching v1's _parse_stamp_epoch.
func creditStampEpoch(text string) *float64 {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	clean := strings.ReplaceAll(text, "T", " ")
	if len(clean) > 19 {
		clean = clean[:19]
	}
	parsed, err := time.ParseInLocation(creditsTimeLayout, clean, time.Local)
	if err != nil {
		return nil
	}
	value := float64(parsed.Unix())
	return &value
}
