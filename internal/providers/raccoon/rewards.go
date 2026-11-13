package raccoon

import (
	"context"
	"errors"
	"net/http"
)

// GrantResult is the v1-compatible login-reward result. Points is zero for an
// idempotent already-claimed response, so callers cannot double-pay.
type GrantResult struct {
	Granted     bool   `json:"granted"`
	Points      int    `json:"points"`
	Source      string `json:"source"`
	DisplayName string `json:"display_name"`
	ExpireAt    string `json:"expire_at"`
}

// AutoClaimResult is the v1-compatible automatic login-reward result. Skipped
// explains a cache/toggle short-circuit; Error is the initial task-fetch
// failure, while Errors collects per-step redacted failures.
type AutoClaimResult struct {
	Desktop bool     `json:"desktop"`
	Mobile  bool     `json:"mobile"`
	Skipped string   `json:"skipped,omitempty"`
	Error   string   `json:"error,omitempty"`
	Errors  []string `json:"errors,omitempty"`
}

// GrantDesktopReward claims the one-time desktop login reward. The upstream
// remains the idempotency authority: a second claim returns granted=false and
// zero points without changing the balance.
func (p *Pool) GrantDesktopReward(ctx context.Context, a *Account) (*GrantResult, error) {
	return p.grantReward(ctx, a, PointsDesktopGrantPath, false)
}

// GrantMobileReward claims the one-time mobile-app login reward using the
// app-android / v1.1.0 client identity the endpoint requires.
func (p *Pool) GrantMobileReward(ctx context.Context, a *Account) (*GrantResult, error) {
	return p.grantReward(ctx, a, PointsMobileGrantPath, true)
}

func (p *Pool) grantReward(
	ctx context.Context,
	a *Account,
	path string,
	mobile bool,
) (*GrantResult, error) {
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
	client := p.HTTPClient()
	if client == nil {
		return nil, errors.New("raccoon: points http client is not configured")
	}
	tokens := p.TokenManager()
	if err := ensurePointsToken(ctx, tokens, a); err != nil {
		return nil, err
	}

	payload, err := p.pointsWithRefresh(
		ctx, a, client, tokens, http.MethodPost, path, nil, mobile,
	)
	if err != nil {
		return nil, err
	}
	data := pointsData(payload)
	if data == nil {
		if mobile {
			return nil, errors.New("raccoon: mobile grant returned no data")
		}
		return nil, errors.New("raccoon: grant returned no data")
	}
	return buildGrantResult(data), nil
}

func buildGrantResult(data map[string]any) *GrantResult {
	granted := pointsTruthy(data["granted"])
	popup, _ := data["popup"].(map[string]any)
	points := 0
	if granted {
		points = pointsInt(popup["points"])
	}
	return &GrantResult{
		Granted:     granted,
		Points:      points,
		Source:      pointsTruthyText(popup["source"]),
		DisplayName: pointsTruthyText(popup["display_name"]),
		ExpireAt:    pointsTruthyText(popup["expire_at"]),
	}
}

// AutoClaim checks unfinished desktop/mobile login tasks and claims each once
// per auto-claim TTL (five minutes), honoring the account's tasks toggle. One
// upstream failure is collected as a redacted error instead of failing the
// whole account row, matching v1. Successful grants refresh credits and tasks
// so the panel immediately reflects the new balance and completed states.
func (p *Pool) AutoClaim(ctx context.Context, a *Account) (AutoClaimResult, error) {
	return p.autoClaim(ctx, a, false)
}

func (p *Pool) autoClaim(ctx context.Context, a *Account, force bool) (AutoClaimResult, error) {
	result := AutoClaimResult{}
	if p == nil {
		return result, errors.New("raccoon: pool is nil")
	}
	if ctx == nil {
		return result, errors.New("raccoon: nil context")
	}
	if a == nil {
		// v1 returns the zero result rather than raising for a missing account.
		return result, nil
	}

	a.mu.Lock()
	tasksEnabled := a.State.TasksEnabled
	a.mu.Unlock()
	if !tasksEnabled {
		result.Skipped = "tasks disabled"
		return result, nil
	}

	a.autoClaimMu.Lock()
	defer a.autoClaimMu.Unlock()

	now := p.timeNow()
	if !force && !a.autoClaimAt.IsZero() && now.Sub(a.autoClaimAt) < autoClaimTTL {
		result.Skipped = "recently checked"
		return result, nil
	}

	tasks, err := p.FetchTasks(ctx, a, false)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		result.Error = redactedPointsError(err, a, maxPointsErrorRunes)
		return result, nil
	}

	states := make(map[string]string, len(tasks.NewUser))
	for _, item := range tasks.NewUser {
		if item.Key != "" {
			states[item.Key] = item.Status
		}
	}

	changed := false
	if states["desktop_login"] == "unfinished" {
		granted, grantErr := p.GrantDesktopReward(ctx, a)
		if grantErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			result.Errors = append(result.Errors,
				"desktop: "+redactedPointsError(grantErr, a, maxPointsStepErrorRunes))
		} else {
			result.Desktop = granted.Granted
			if granted.Granted {
				a.markPointTaskCompleted("desktop_login")
				changed = true
			}
		}
	}
	if states["mobile_login"] == "unfinished" {
		granted, grantErr := p.GrantMobileReward(ctx, a)
		if grantErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			result.Errors = append(result.Errors,
				"mobile: "+redactedPointsError(grantErr, a, maxPointsStepErrorRunes))
		} else {
			result.Mobile = granted.Granted
			if granted.Granted {
				a.markPointTaskCompleted("mobile_login")
				changed = true
			}
		}
	}

	// v1 stamps the check time after both attempts, even when a grant failed.
	a.autoClaimAt = now
	if changed {
		if _, err := p.FetchCredits(ctx, a, true); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			result.Errors = append(result.Errors,
				"credits: "+redactedPointsError(err, a, maxPointsStepErrorRunes))
		}
		if _, err := p.FetchTasks(ctx, a, true); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			result.Errors = append(result.Errors,
				"tasks: "+redactedPointsError(err, a, maxPointsStepErrorRunes))
		}
	}
	if len(result.Errors) == 0 {
		result.Errors = nil
	}
	return result, nil
}
