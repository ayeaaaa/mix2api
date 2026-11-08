package raccoon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// FetchTasks returns one account's point-task snapshot, cached for 300 seconds
// unless force is set. The invite code and marketing text are deliberately not
// copied; only task key/status pairs and the invite counters are public.
func (p *Pool) FetchTasks(ctx context.Context, a *Account, force bool) (*PointsTasks, error) {
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
		if cached, ok := a.freshTasks(p.timeNow()); ok {
			return cached, nil
		}
	}

	client := p.HTTPClient()
	if client == nil {
		return nil, errors.New("raccoon: points http client is not configured")
	}
	tokens := p.TokenManager()

	a.tasksMu.Lock()
	defer a.tasksMu.Unlock()

	now := p.timeNow()
	if !force {
		if cached, ok := a.freshTasks(now); ok {
			return cached, nil
		}
	}
	if err := ensurePointsToken(ctx, tokens, a); err != nil {
		return nil, err
	}

	payload, err := p.pointsWithRefresh(
		ctx, a, client, tokens, http.MethodGet, PointsTasksPath, nil, false,
	)
	if err != nil {
		return nil, err
	}
	data := pointsData(payload)
	if data == nil {
		return nil, errors.New("raccoon: tasks returned no data")
	}

	snapshot := buildPointsTasks(data, now)
	a.mu.Lock()
	a.PointsTasks = snapshot
	a.tasksFetchedAt = now
	result := publicPointsTasks(snapshot)
	a.mu.Unlock()
	return result, nil
}

// buildPointsTasks normalizes the two task-state lists and the invite
// counters. The upstream invite code never enters the snapshot type.
func buildPointsTasks(data map[string]any, now time.Time) *PointsTasks {
	invite, _ := data["invite"].(map[string]any)
	return &PointsTasks{
		NewUser: pointsTaskStates(data["new_user_task_states"]),
		Invitee: pointsTaskStates(data["invitee_task_states"]),
		Invite: PointsInvite{
			GrantedPoints:           pointsInt(invite["granted_points"]),
			RegisterRewardPoints:    pointsInt(invite["register_reward_points"]),
			RegisteredInviteeCount:  pointsInt(invite["registered_invitee_count"]),
			RemainingRewardInvitees: pointsInt(invite["remaining_reward_invitees"]),
		},
		FetchedAt: unixFloat(now),
	}
}

// pointsTaskStates mirrors v1 _point_task_states: only objects with both a
// non-empty key and status survive, trimmed and capped at 80/40 characters.
func pointsTaskStates(value any) []PointsTaskState {
	states := make([]PointsTaskState, 0)
	rawItems, _ := value.([]any)
	for _, rawItem := range rawItems {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		key := truncateRunes(strings.TrimSpace(pointsTruthyText(item["key"])), 80)
		status := truncateRunes(strings.TrimSpace(pointsTruthyText(item["status"])), 40)
		if key == "" || status == "" {
			continue
		}
		states = append(states, PointsTaskState{Key: key, Status: status})
	}
	return states
}

// freshTasks returns a public copy of the cached, unexpired task snapshot.
func (a *Account) freshTasks(now time.Time) (*PointsTasks, bool) {
	if a == nil {
		return nil, false
	}
	a.mu.Lock()
	raw := a.PointsTasks
	if raw == nil {
		a.mu.Unlock()
		return nil, false
	}
	stamp := a.tasksFetchedAt
	if stamp.IsZero() && raw.FetchedAt > 0 {
		stamp = epochSecondsToTime(raw.FetchedAt)
	}
	cached := publicPointsTasks(raw)
	a.mu.Unlock()
	if stamp.IsZero() || now.Sub(stamp) >= pointsTasksTTL {
		return nil, false
	}
	return cached, true
}

// markPointTaskCompleted marks a just-granted login task in the local snapshot
// so the panel sees the result even if the follow-up forced fetch fails.
func (a *Account) markPointTaskCompleted(key string) bool {
	if a == nil || key == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tasks := a.PointsTasks
	if tasks == nil {
		return false
	}
	changed := false
	for index := range tasks.NewUser {
		if tasks.NewUser[index].Key == key && tasks.NewUser[index].Status != "completed" {
			tasks.NewUser[index].Status = "completed"
			changed = true
		}
	}
	for index := range tasks.Invitee {
		if tasks.Invitee[index].Key == key && tasks.Invitee[index].Status != "completed" {
			tasks.Invitee[index].Status = "completed"
			changed = true
		}
	}
	return changed
}
