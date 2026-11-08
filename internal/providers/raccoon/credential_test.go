package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/accountstate"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

func waitChannel(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitError(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// blockingRefreshServer holds one refresh request on the wire until release.
func blockingRefreshServer(t *testing.T, token string) (url string, started <-chan struct{}, release func()) {
	t.Helper()
	startedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(startedCh) })
		<-releaseCh
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"access_token": token},
		})
	}))
	t.Cleanup(server.Close)

	release = func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	return server.URL, startedCh, release
}

func accountAuthSnapshot(a *Account) Auth {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Auth
}

func TestRefreshVsReimportAbortsStaleWrite(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-cred", "", now.Add(time.Minute), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	account := p.Get("u-cred")

	refreshedToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-cred",
		"exp": now.Add(time.Hour).Unix(),
	})
	url, started, release := blockingRefreshServer(t, refreshedToken)
	tm := p.NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
		BaseURL: url,
		Now:     func() time.Time { return now },
		Timeout: 2 * time.Second,
	})

	refreshErr := make(chan error, 1)
	go func() { refreshErr <- tm.Refresh(context.Background(), account) }()
	waitChannel(t, started, "refresh request")

	importedToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-cred",
		"exp": now.Add(2 * time.Hour).Unix(),
	})
	imported, err := p.Add(Auth{
		UID:           "u-cred",
		AccessToken:   importedToken,
		RefreshToken:  "imported-refresh",
		OfficeOrgName: "Imported Org",
	})
	if err != nil {
		release()
		t.Fatalf("Add during refresh: %v", err)
	}
	if imported != account {
		t.Fatal("re-import replaced the canonical account pointer")
	}

	release()
	err = waitError(t, refreshErr, "stale refresh result")
	if !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("stale refresh error = %v, want ErrCredentialsChanged", err)
	}

	auth := accountAuthSnapshot(account)
	if auth.AccessToken != importedToken || auth.RefreshToken != "imported-refresh" {
		t.Fatalf("stale refresh overwrote re-imported credentials: %+v", auth)
	}
	saved, err := LoadAuth(account.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != importedToken || saved.RefreshToken != "imported-refresh" {
		t.Fatalf("auth file has stale refresh credentials: %+v", saved)
	}
}

func TestRefreshVsRemoveDoesNotResurrectAuth(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	path := writePoolAuth(t, dir, "u-rm", "", now.Add(time.Minute), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	account := p.Get("u-rm")

	refreshedToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-rm",
		"exp": now.Add(time.Hour).Unix(),
	})
	url, started, release := blockingRefreshServer(t, refreshedToken)
	tm := p.NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
		BaseURL: url,
		Now:     func() time.Time { return now },
		Timeout: 2 * time.Second,
	})

	refreshErr := make(chan error, 1)
	go func() { refreshErr <- tm.Refresh(context.Background(), account) }()
	waitChannel(t, started, "refresh request")

	if err := p.Remove("u-rm"); err != nil {
		release()
		t.Fatalf("Remove during refresh: %v", err)
	}
	release()
	if err := waitError(t, refreshErr, "refresh after remove"); !errors.Is(err, ErrAccountDeleted) {
		t.Fatalf("refresh after remove error = %v, want ErrAccountDeleted", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("in-flight refresh recreated the deleted auth file: %v", err)
	}
	if p.Get("u-rm") != nil {
		t.Fatal("removed account is still in the pool")
	}
	if p.Acquire(account) {
		t.Fatal("stale account pointer still acquires concurrency")
	}
	if err := p.SetProxySlot("u-rm", "slot-1"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("SetProxySlot after remove = %v, want ErrAccountNotFound", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("SetProxySlot after remove recreated the auth file")
	}
}

func TestSetProxySlotAfterRemoveDoesNotRecreateAuth(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	path := writePoolAuth(t, dir, "u-gone", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	p.SetProxySlots([]transport.Slot{{ID: "slot-1", URL: "http://proxy.example:8080", Enabled: true}})
	account := p.Get("u-gone")

	if err := p.Remove("u-gone"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetProxySlot("u-gone", "slot-1"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("SetProxySlot after remove = %v, want ErrAccountNotFound", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("SetProxySlot recreated a deleted auth file: %v", err)
	}
	if p.Acquire(account) {
		t.Fatal("stale account pointer still acquires after remove")
	}
}

func TestRefreshVsProxySlotAbortsStaleWrite(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-proxy-race", "", now.Add(time.Minute), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	p.SetProxySlots([]transport.Slot{{ID: "slot-1", URL: "http://proxy.example:8080", Enabled: true}})
	account := p.Get("u-proxy-race")

	refreshedToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-proxy-race",
		"exp": now.Add(time.Hour).Unix(),
	})
	url, started, release := blockingRefreshServer(t, refreshedToken)
	tm := p.NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
		BaseURL: url,
		Now:     func() time.Time { return now },
		Timeout: 2 * time.Second,
	})

	refreshErr := make(chan error, 1)
	go func() { refreshErr <- tm.Refresh(context.Background(), account) }()
	waitChannel(t, started, "refresh request")

	if err := p.SetProxySlot("u-proxy-race", "slot-1"); err != nil {
		release()
		t.Fatalf("SetProxySlot during refresh: %v", err)
	}
	release()
	if err := waitError(t, refreshErr, "refresh after proxy change"); !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("refresh after proxy change error = %v, want ErrCredentialsChanged", err)
	}

	saved, err := LoadAuth(account.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ProxySlot != "slot-1" {
		t.Fatalf("stale refresh overwrote proxySlot: %+v", saved)
	}
	if auth := accountAuthSnapshot(account); auth.ProxySlot != "slot-1" {
		t.Fatalf("in-memory auth lost proxySlot: %+v", auth)
	}
}

func TestConcurrentDisableVsRefreshSaveNoLostState(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-a", "", now.Add(time.Hour), nil)
	writePoolAuth(t, dir, "u-b", "", now.Add(time.Minute), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})

	b := p.Get("u-b")
	b.mu.Lock()
	b.State.Disabled = true
	b.State.DisabledBy = "auth"
	b.State.Reason = "old auth failure"
	b.State.LastError = "old auth failure"
	b.State.ErrCount = 2
	b.State.CoolUntil = now.Add(10 * time.Minute)
	b.mu.Unlock()

	refreshedToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-b",
		"exp": now.Add(time.Hour).Unix(),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"access_token": refreshedToken},
		})
	}))
	t.Cleanup(server.Close)
	tm := p.NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
		BaseURL: server.URL,
		Now:     func() time.Time { return now },
		Timeout: 2 * time.Second,
	})

	snapshotReached := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	var snapshotOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSnapshot) }) }
	t.Cleanup(release)
	p.stateWriteHook = func() {
		snapshotOnce.Do(func() {
			close(snapshotReached)
			<-releaseSnapshot
		})
	}

	bMutated := make(chan struct{})
	tm.OnStateChange = func(*Account) {
		close(bMutated)
		_ = p.SaveState()
	}

	disableErr := make(chan error, 1)
	go func() { disableErr <- p.Disable("u-a", "manual disable") }()
	waitChannel(t, snapshotReached, "state snapshot")

	refreshErr := make(chan error, 1)
	go func() { refreshErr <- tm.Refresh(context.Background(), b) }()
	waitChannel(t, bMutated, "refresh state mutation")
	release()

	if err := waitError(t, disableErr, "concurrent disable"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := waitError(t, refreshErr, "refresh save"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	data, err := os.ReadFile(p.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]accountstate.State
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	aState := saved["u-a"]
	if !aState.Disabled || !aState.ManualDisabled || aState.Reason != "manual disable" {
		t.Fatalf("manual disable was lost from the state file: %+v\n%s", aState, data)
	}
	bState := saved["u-b"]
	if bState.Disabled || bState.DisabledBy != "" || bState.LastError != "" || bState.ErrCount != 0 || !bState.CoolUntil.IsZero() {
		t.Fatalf("stale refresh snapshot overwrote recovered state: %+v\n%s", bState, data)
	}
}

func TestRefreshClearsV1NonManualState(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	expiry := now.Add(time.Hour)
	newToken := makeUnsignedJWT(t, map[string]any{"sub": "pu-clear", "exp": expiry.Unix()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"access_token": newToken},
		})
	}))
	t.Cleanup(server.Close)

	account := &Account{
		Auth: Auth{
			UID:          "pu-clear",
			AccessToken:  makeUnsignedJWT(t, map[string]any{"sub": "pu-clear", "exp": now.Add(time.Minute).Unix()}),
			RefreshToken: "refresh",
			ExpiresAt:    now.Add(time.Minute),
		},
		State: accountstate.State{
			UID:          "pu-clear",
			Disabled:     true,
			DisabledBy:   "auth",
			Reason:       "old failure",
			LastError:    "old failure",
			ErrCount:     3,
			CoolUntil:    now.Add(5 * time.Minute),
			TasksEnabled: true,
		},
		Path: filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-clear.json"),
	}
	tm := NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
		BaseURL: server.URL,
		Now:     func() time.Time { return now },
		Timeout: 2 * time.Second,
	})
	if err := tm.Refresh(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if account.State.Disabled || account.State.ManualDisabled || account.State.DisabledBy != "" ||
		account.State.Reason != "" || account.State.LastError != "" ||
		account.State.ErrCount != 0 || !account.State.CoolUntil.IsZero() {
		t.Fatalf("successful refresh did not clear v1 non-manual state: %+v", account.State)
	}
}

func TestRefreshTerminalSetsV1State(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid credentials"}`))
	}))
	t.Cleanup(server.Close)

	newAccount := func(state accountstate.State) *Account {
		return &Account{
			Auth: Auth{
				UID:          "pu-terminal",
				AccessToken:  makeUnsignedJWT(t, map[string]any{"sub": "pu-terminal", "exp": now.Add(time.Minute).Unix()}),
				RefreshToken: "refresh",
				ExpiresAt:    now.Add(time.Minute),
			},
			State: state,
			Path:  filepath.Join(t.TempDir(), "raccoon", "raccoon-pu-terminal.json"),
		}
	}
	manager := func() *TokenManager {
		return NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
			BaseURL: server.URL,
			Now:     func() time.Time { return now },
			Timeout: 2 * time.Second,
		})
	}

	t.Run("non-manual", func(t *testing.T) {
		account := newAccount(accountstate.State{
			UID:       "pu-terminal",
			LastError: "stale error",
			ErrCount:  2,
		})
		err := manager().Refresh(context.Background(), account)
		if err == nil || !IsAuthError(err) {
			t.Fatalf("Refresh error = %v, want auth error", err)
		}
		if !account.State.Disabled || account.State.ManualDisabled || account.State.DisabledBy != "auth" {
			t.Fatalf("terminal failure did not apply v1 disable metadata: %+v", account.State)
		}
		if account.State.LastError == "" || account.State.Reason != account.State.LastError {
			t.Fatalf("terminal failure LastError/Reason mismatch: %+v", account.State)
		}
	})

	t.Run("manual preserved", func(t *testing.T) {
		coolUntil := now.Add(10 * time.Minute)
		account := newAccount(accountstate.State{
			UID:            "pu-terminal",
			Disabled:       true,
			ManualDisabled: true,
			DisabledBy:     "manual",
			Reason:         "disabled from the panel",
			LastError:      "old",
			CoolUntil:      coolUntil,
		})
		err := manager().Refresh(context.Background(), account)
		if err == nil || !IsAuthError(err) {
			t.Fatalf("Refresh error = %v, want auth error", err)
		}
		if !account.State.ManualDisabled || !account.State.Disabled || account.State.DisabledBy != "manual" {
			t.Fatalf("terminal failure cleared manual disable: %+v", account.State)
		}
		if account.State.Reason != "disabled from the panel" || !account.State.CoolUntil.Equal(coolUntil) {
			t.Fatalf("terminal failure changed manual reason/cooldown: %+v", account.State)
		}
		if account.State.LastError == "" || account.State.LastError == "old" {
			t.Fatalf("terminal failure did not update LastError: %+v", account.State)
		}
	})
}

func TestAddReimportPreservesProxySlot(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-import-proxy", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	p.SetProxySlots([]transport.Slot{{ID: "slot-1", URL: "http://u:p@proxy.example:8080", Enabled: true}})
	if err := p.SetProxySlot("u-import-proxy", "slot-1"); err != nil {
		t.Fatal(err)
	}

	token := makeUnsignedJWT(t, map[string]any{"sub": "u-import-proxy", "exp": poolExpiry(now).Unix()})
	account, err := p.Add(Auth{
		UID:           "u-import-proxy",
		AccessToken:   token,
		RefreshToken:  "refresh-new",
		OfficeOrgName: "Reimported",
	})
	if err != nil {
		t.Fatal(err)
	}
	if account.ProxySlot() != "slot-1" {
		t.Fatalf("re-import dropped proxy slot: %q", account.ProxySlot())
	}
	saved, err := LoadAuth(account.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ProxySlot != "slot-1" {
		t.Fatalf("saved re-import auth lacks preserved proxySlot: %+v", saved)
	}
	if view := account.View(); view.ProxySlot != "slot-1" || view.Proxy != "http://<redacted>@proxy.example:8080" {
		t.Fatalf("view after re-import = %+v", view)
	}
}

func TestPoolTokenManagerWiresTimeoutAndProxy(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-tm", "", poolExpiry(now), nil)
	p := mustPool(t, dir, Config{MaxConcurrent: 1, TimeoutSeconds: 2.5})
	p.SetProxySlots([]transport.Slot{{ID: "slot-1", URL: "http://proxy.example:8080", Enabled: true}})
	if err := p.SetProxySlot("u-tm", "slot-1"); err != nil {
		t.Fatal(err)
	}
	account := p.Get("u-tm")

	manager := p.NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{})
	if manager.timeout != 2500*time.Millisecond {
		t.Fatalf("manager timeout = %v, want cfg timeout 2.5s", manager.timeout)
	}
	if manager.proxyURL == nil || manager.proxyURL(account) != "http://proxy.example:8080" {
		t.Fatal("pool token manager did not wire the account proxy")
	}

	explicit := p.NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
		Timeout: 7 * time.Second,
	})
	if explicit.timeout != 7*time.Second {
		t.Fatalf("explicit timeout = %v, want 7s", explicit.timeout)
	}
}

func TestAccountsSortsAfterUIDRotationAndMigratesOldState(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	dir := t.TempDir()
	writePoolAuth(t, dir, "u-z", "", now.Add(time.Minute), nil)
	writePoolAuth(t, dir, "u-m", "", now.Add(time.Hour), nil)
	writePoolState(t, dir, `{
	  "u-z": {
	    "disabled": true,
	    "manual_disabled": true,
	    "disabled_by": "manual",
	    "reason": "manual disable",
	    "note": "keep me",
	    "tasks_enabled": false,
	    "future_state": {"keep": true}
	  }
	}`)

	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	rotated := p.Get("u-z")
	rotatedToken := makeUnsignedJWT(t, map[string]any{
		"sub": "u-a",
		"exp": now.Add(2 * time.Hour).Unix(),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"access_token": rotatedToken},
		})
	}))
	t.Cleanup(server.Close)
	tm := p.NewTokenManager(transport.NewClient(transport.ClientOptions{}), TokenManagerOptions{
		BaseURL: server.URL,
		Now:     func() time.Time { return now },
		Timeout: 2 * time.Second,
	})
	if err := tm.Refresh(context.Background(), rotated); err != nil {
		t.Fatal(err)
	}

	accounts := p.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("account count = %d", len(accounts))
	}
	firstUID, _ := accounts[0].currentUID()
	secondUID, _ := accounts[1].currentUID()
	if firstUID != "u-a" || secondUID != "u-m" {
		t.Fatalf("Accounts() order = %q, %q; want u-a, u-m", firstUID, secondUID)
	}
	if p.Get("u-z") != nil || p.Get("u-a") != rotated {
		t.Fatal("UID rotation was not applied to the pool")
	}

	data, err := os.ReadFile(p.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]accountstate.State
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved["u-z"]; ok {
		t.Fatalf("old UID state entry survived rotation: %s", data)
	}
	migrated := saved["u-a"]
	if migrated.Note != "keep me" || !migrated.ManualDisabled || migrated.TasksEnabled {
		t.Fatalf("pre-existing state was not migrated to the rotated UID: %+v\n%s", migrated, data)
	}
	if _, ok := migrated.Extra["future_state"]; !ok {
		t.Fatalf("unknown state field was lost during UID rotation: %s", data)
	}
}

func TestMalformedAuthFileDoesNotWipePreservedState(t *testing.T) {
	dir := t.TempDir()
	statePath := writePoolState(t, dir, `{
	  "u-bad": {"note": "keep state", "tasks_enabled": false, "future_state": {"keep": 1}}
	}`)
	folder := filepath.Join(dir, "raccoon")
	broken := filepath.Join(folder, "raccoon-u-bad.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := mustPool(t, dir, Config{MaxConcurrent: 1})
	if p.Get("u-bad") != nil {
		t.Fatal("malformed auth file produced an account")
	}
	if err := p.SaveState(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]accountstate.State
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["u-bad"].Note != "keep state" || saved["u-bad"].TasksEnabled {
		t.Fatalf("malformed auth file caused its state to be wiped: %s", data)
	}
	if _, ok := saved["u-bad"].Extra["future_state"]; !ok {
		t.Fatalf("unknown state field was wiped: %s", data)
	}
}
