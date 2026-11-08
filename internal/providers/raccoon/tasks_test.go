package raccoon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTasksStripInviteCode(t *testing.T) {
	fixture := newPointsFixture(t, "u-tasks")
	account := fixture.account(t)

	tasks, err := fixture.p.FetchTasks(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchTasks: %v", err)
	}
	if len(tasks.NewUser) != 3 {
		t.Fatalf("new_user = %+v, want 3 states", tasks.NewUser)
	}
	wantKeys := []string{"register_pack", "desktop_login", "mobile_login"}
	for index, key := range wantKeys {
		if tasks.NewUser[index].Key != key {
			t.Fatalf("new_user[%d].key = %q, want %q", index, tasks.NewUser[index].Key, key)
		}
	}
	if tasks.NewUser[0].Status != "completed" || tasks.NewUser[1].Status != "unfinished" || tasks.NewUser[2].Status != "unfinished" {
		t.Fatalf("new_user statuses = %+v", tasks.NewUser)
	}
	if len(tasks.Invitee) != 0 {
		t.Fatalf("invitee = %+v, want empty", tasks.Invitee)
	}
	if got := testInt(tasks.Invite.RegisterRewardPoints); got != 1500 {
		t.Fatalf("register_reward_points = %v, want 1500", tasks.Invite.RegisterRewardPoints)
	}
	if got := testInt(tasks.Invite.RemainingRewardInvitees); got != 20 {
		t.Fatalf("remaining_reward_invitees = %v, want 20", tasks.Invite.RemainingRewardInvitees)
	}
	if tasks.FetchedAt <= 0 || int64(tasks.FetchedAt) != fixture.now.Unix() {
		t.Fatalf("fetched_at = %v, want %d", tasks.FetchedAt, fixture.now.Unix())
	}

	encoded, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"fake-invite-code", "invite_code", "marketing_text", "access_token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("tasks leaked %q: %s", forbidden, text)
		}
	}

	viewJSON, err := json.Marshal(account.View())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(viewJSON), "fake-invite-code") || strings.Contains(string(viewJSON), "invite_code") {
		t.Fatalf("account view leaked the invite code: %s", viewJSON)
	}
	if !strings.Contains(string(viewJSON), `"points_tasks":`) {
		t.Fatalf("account view is missing points_tasks: %s", viewJSON)
	}

	if _, _, tasksCalls, _, _, _ := fixture.fake.counts(); tasksCalls != 1 {
		t.Fatalf("tasks calls = %d, want 1", tasksCalls)
	}
	cached, err := fixture.p.FetchTasks(context.Background(), account, false)
	if err != nil {
		t.Fatalf("cached FetchTasks: %v", err)
	}
	if len(cached.NewUser) != 3 {
		t.Fatalf("cached new_user = %+v", cached.NewUser)
	}
	if _, _, tasksCalls, _, _, _ := fixture.fake.counts(); tasksCalls != 1 {
		t.Fatalf("tasks cache made an upstream call: %d", tasksCalls)
	}

	fixture.now = fixture.now.Add(300 * time.Second)
	if _, err := fixture.p.FetchTasks(context.Background(), account, false); err != nil {
		t.Fatalf("expired FetchTasks: %v", err)
	}
	if _, _, tasksCalls, _, _, _ := fixture.fake.counts(); tasksCalls != 2 {
		t.Fatalf("expired tasks calls = %d, want 2", tasksCalls)
	}
}

func TestTasks401RefreshesOnce(t *testing.T) {
	fixture := newPointsFixture(t, "u-tasks-401")
	account := fixture.account(t)
	fixture.fake.update(func(fake *pointsFake) {
		fake.allowed = make(map[string]bool)
	})

	tasks, err := fixture.p.FetchTasks(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchTasks after one 401: %v", err)
	}
	if len(tasks.NewUser) != 3 {
		t.Fatalf("new_user = %+v", tasks.NewUser)
	}
	if _, _, tasksCalls, _, _, refreshCalls := fixture.fake.counts(); tasksCalls != 2 || refreshCalls != 1 {
		t.Fatalf("tasks/refresh calls = %d/%d, want 2/1", tasksCalls, refreshCalls)
	}
}

func TestGrantDesktopAndMobileIdempotent(t *testing.T) {
	fixture := newPointsFixture(t, "u-rewards")
	account := fixture.account(t)

	desktop, err := fixture.p.GrantDesktopReward(context.Background(), account)
	if err != nil {
		t.Fatalf("GrantDesktopReward: %v", err)
	}
	if !desktop.Granted || desktop.Points != 3000 {
		t.Fatalf("desktop grant = %+v, want granted 3000", desktop)
	}
	if desktop.Source != "desktop_login" || desktop.DisplayName != "桌面端登录奖励" || desktop.ExpireAt == "" {
		t.Fatalf("desktop popup = %+v", desktop)
	}
	if got := fixture.fake.header("X-Client-Platform", "desktop"); got != "desktop-linux-x64" {
		t.Fatalf("desktop grant platform = %q", got)
	}
	if got := fixture.fake.header("X-Client-Version", "desktop"); got != "v1.1.5" {
		t.Fatalf("desktop grant version = %q", got)
	}

	credits, err := fixture.p.FetchCredits(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchCredits after desktop grant: %v", err)
	}
	if got := testInt(credits.Available); got != 4234 {
		t.Fatalf("available after desktop grant = %d, want 4234", got)
	}

	again, err := fixture.p.GrantDesktopReward(context.Background(), account)
	if err != nil {
		t.Fatalf("second GrantDesktopReward: %v", err)
	}
	if again.Granted || again.Points != 0 {
		t.Fatalf("second desktop grant = %+v, want granted=false points=0", again)
	}
	credits, err = fixture.p.FetchCredits(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchCredits after second desktop grant: %v", err)
	}
	if got := testInt(credits.Available); got != 4234 {
		t.Fatalf("desktop grant double-paid: available %d, want 4234", got)
	}

	mobile, err := fixture.p.GrantMobileReward(context.Background(), account)
	if err != nil {
		t.Fatalf("GrantMobileReward: %v", err)
	}
	if !mobile.Granted || mobile.Points != 3000 {
		t.Fatalf("mobile grant = %+v, want granted 3000", mobile)
	}
	if mobile.Source != "mobile_login" || mobile.DisplayName != "移动端登录奖励" || mobile.ExpireAt == "" {
		t.Fatalf("mobile popup = %+v", mobile)
	}
	if got := fixture.fake.header("X-Client-Platform", "mobile"); got != "app-android" {
		t.Fatalf("mobile grant platform = %q, want app-android", got)
	}
	if got := fixture.fake.header("X-Client-Version", "mobile"); got != "v1.1.0" {
		t.Fatalf("mobile grant version = %q, want v1.1.0", got)
	}

	credits, err = fixture.p.FetchCredits(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchCredits after mobile grant: %v", err)
	}
	if got := testInt(credits.Available); got != 7234 {
		t.Fatalf("available after mobile grant = %d, want 7234", got)
	}

	again, err = fixture.p.GrantMobileReward(context.Background(), account)
	if err != nil {
		t.Fatalf("second GrantMobileReward: %v", err)
	}
	if again.Granted || again.Points != 0 {
		t.Fatalf("second mobile grant = %+v, want granted=false points=0", again)
	}
	credits, err = fixture.p.FetchCredits(context.Background(), account, true)
	if err != nil {
		t.Fatalf("FetchCredits after second mobile grant: %v", err)
	}
	if got := testInt(credits.Available); got != 7234 {
		t.Fatalf("mobile grant double-paid: available %d, want 7234", got)
	}

	encoded, err := json.Marshal(desktop)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, key := range []string{`"granted":`, `"points":`, `"source":`, `"display_name":`, `"expire_at":`} {
		if !strings.Contains(text, key) {
			t.Fatalf("grant result is missing %s: %s", key, text)
		}
	}
	account.mu.Lock()
	token := account.Auth.AccessToken
	account.mu.Unlock()
	if strings.Contains(text, token) {
		t.Fatalf("grant result leaked the access token: %s", text)
	}
}

func TestAutoClaimRespectsTasksToggle(t *testing.T) {
	fixture := newPointsFixture(t, "u-auto-toggle")
	account := fixture.account(t)

	if err := fixture.p.SetTasksEnabled("u-auto-toggle", false); err != nil {
		t.Fatalf("SetTasksEnabled(false): %v", err)
	}
	disabled, err := fixture.p.AutoClaim(context.Background(), account)
	if err != nil {
		t.Fatalf("AutoClaim while disabled: %v", err)
	}
	if disabled.Skipped != "tasks disabled" || disabled.Desktop || disabled.Mobile {
		t.Fatalf("disabled auto claim = %+v", disabled)
	}
	if balance, bills, tasks, desktop, mobile, refresh := fixture.fake.counts(); balance != 0 || bills != 0 || tasks != 0 || desktop != 0 || mobile != 0 || refresh != 0 {
		t.Fatalf("disabled auto claim made upstream calls: b=%d bills=%d t=%d d=%d m=%d r=%d",
			balance, bills, tasks, desktop, mobile, refresh)
	}

	if err := fixture.p.SetTasksEnabled("u-auto-toggle", true); err != nil {
		t.Fatalf("SetTasksEnabled(true): %v", err)
	}
	result, err := fixture.p.AutoClaim(context.Background(), account)
	if err != nil {
		t.Fatalf("AutoClaim: %v", err)
	}
	if !result.Desktop || !result.Mobile || result.Skipped != "" || len(result.Errors) != 0 {
		t.Fatalf("auto claim result = %+v", result)
	}
	if _, _, tasksCalls, desktop, mobile, _ := fixture.fake.counts(); tasksCalls != 2 || desktop != 1 || mobile != 1 {
		t.Fatalf("auto claim calls = tasks %d desktop %d mobile %d, want 2/1/1", tasksCalls, desktop, mobile)
	}
	credits, err := fixture.p.FetchCredits(context.Background(), account, false)
	if err != nil {
		t.Fatalf("cached credits after auto claim: %v", err)
	}
	if got := testInt(credits.Available); got != 7234 {
		t.Fatalf("available after auto claim = %d, want 7234", got)
	}
	tasks, err := fixture.p.FetchTasks(context.Background(), account, false)
	if err != nil {
		t.Fatalf("cached tasks after auto claim: %v", err)
	}
	states := make(map[string]string, len(tasks.NewUser))
	for _, item := range tasks.NewUser {
		states[item.Key] = item.Status
	}
	if states["desktop_login"] != "completed" || states["mobile_login"] != "completed" {
		t.Fatalf("auto claim task states = %v", states)
	}

	again, err := fixture.p.AutoClaim(context.Background(), account)
	if err != nil {
		t.Fatalf("immediate second AutoClaim: %v", err)
	}
	if again.Skipped != "recently checked" || again.Desktop || again.Mobile {
		t.Fatalf("second auto claim = %+v", again)
	}
	if _, _, tasksCalls, desktop, mobile, _ := fixture.fake.counts(); tasksCalls != 2 || desktop != 1 || mobile != 1 {
		t.Fatalf("TTL did not stop upstream calls: tasks %d desktop %d mobile %d", tasksCalls, desktop, mobile)
	}

	fixture.now = fixture.now.Add(5 * time.Minute)
	afterTTL, err := fixture.p.AutoClaim(context.Background(), account)
	if err != nil {
		t.Fatalf("AutoClaim after TTL: %v", err)
	}
	if afterTTL.Skipped == "recently checked" {
		t.Fatalf("auto claim stayed cached after its TTL: %+v", afterTTL)
	}
	if afterTTL.Desktop || afterTTL.Mobile {
		t.Fatalf("auto claim double-paid: %+v", afterTTL)
	}
	if _, _, tasksCalls, desktop, mobile, _ := fixture.fake.counts(); tasksCalls != 3 || desktop != 1 || mobile != 1 {
		t.Fatalf("post-TTL calls = tasks %d desktop %d mobile %d, want 3/1/1", tasksCalls, desktop, mobile)
	}
}

func TestAutoClaimConcurrentDoesNotDoublePay(t *testing.T) {
	fixture := newPointsFixture(t, "u-auto-flight")
	account := fixture.account(t)

	var wg sync.WaitGroup
	results := make(chan AutoClaimResult, 8)
	errs := make(chan error, 8)
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := fixture.p.AutoClaim(context.Background(), account)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent AutoClaim: %v", err)
		}
	}

	desktopClaimed := 0
	for result := range results {
		if result.Desktop {
			desktopClaimed++
		}
	}
	if desktopClaimed > 1 {
		t.Fatalf("desktop reward reported granted %d times", desktopClaimed)
	}
	if _, _, _, desktop, mobile, _ := fixture.fake.counts(); desktop != 1 || mobile != 1 {
		t.Fatalf("concurrent auto claim grants = desktop %d mobile %d, want 1/1", desktop, mobile)
	}
	credits, err := fixture.p.FetchCredits(context.Background(), account, false)
	if err != nil {
		t.Fatalf("cached credits: %v", err)
	}
	if got := testInt(credits.Available); got != 7234 {
		t.Fatalf("concurrent auto claim balance = %d, want 7234", got)
	}
}

func TestAutoClaimCollectsRedactedErrors(t *testing.T) {
	fixture := newPointsFixture(t, "u-auto-errors")
	account := fixture.account(t)
	fixture.fake.update(func(fake *pointsFake) {
		fake.desktopStatus = http.StatusInternalServerError
	})

	result, err := fixture.p.AutoClaim(context.Background(), account)
	if err != nil {
		t.Fatalf("AutoClaim should not fail for one upstream error: %v", err)
	}
	if result.Desktop || !result.Mobile {
		t.Fatalf("auto claim result = %+v, want desktop=false mobile=true", result)
	}
	if len(result.Errors) == 0 || !strings.Contains(result.Errors[0], "desktop:") {
		t.Fatalf("auto claim errors = %v, want a desktop error", result.Errors)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	account.mu.Lock()
	token := account.Auth.AccessToken
	account.mu.Unlock()
	if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), "refresh-u-auto-errors") {
		t.Fatalf("auto claim errors leaked a token: %s", encoded)
	}
}

func TestAutoClaimTaskFetchFailureIsRedacted(t *testing.T) {
	fixture := newPointsFixture(t, "u-auto-fail")
	account := fixture.account(t)
	fixture.fake.update(func(fake *pointsFake) {
		fake.tasksStatus = http.StatusInternalServerError
	})

	result, err := fixture.p.AutoClaim(context.Background(), account)
	if err != nil {
		t.Fatalf("AutoClaim: %v", err)
	}
	if result.Error == "" || result.Desktop || result.Mobile {
		t.Fatalf("auto claim task failure result = %+v", result)
	}
	if _, _, tasksCalls, desktop, mobile, _ := fixture.fake.counts(); tasksCalls != 1 || desktop != 0 || mobile != 0 {
		t.Fatalf("task failure calls = tasks %d desktop %d mobile %d", tasksCalls, desktop, mobile)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	account.mu.Lock()
	token := account.Auth.AccessToken
	account.mu.Unlock()
	if strings.Contains(string(encoded), token) {
		t.Fatalf("task failure leaked a token: %s", encoded)
	}
}
