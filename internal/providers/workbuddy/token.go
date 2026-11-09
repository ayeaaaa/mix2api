package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

const (
	// tokenRefreshPath is v1's REFRESH_PATH. The refresh endpoint only accepts
	// OAuth device-flow credentials; static API keys are skipped.
	tokenRefreshPath = "/v2/plugin/auth/token/refresh"

	// tokenRefreshSkew is v1's 120-second readiness window. A token is
	// refreshed when it expires inside this window, including exactly at the
	// boundary.
	tokenRefreshSkew = 120 * time.Second

	// defaultTokenRefreshTimeout matches v1's 30-second refresh budget. The
	// provider's tokenTimeout test seam may override it.
	defaultTokenRefreshTimeout = 30 * time.Second

	// tokenErrorLimit bounds runtime error text while keeping enough context
	// for an operator to understand the failure.
	tokenErrorLimit = 500
)

// KeepaliveResult reports one token keepalive sweep. Accounts counts every
// account in the realm pool snapshot; Refreshed counts successful rotations;
// Skipped counts disabled, API-key, removed, non-due, and no-op accounts; and
// Failed counts accounts whose refresh returned an error. Errors holds one
// redacted, UID-prefixed message per failed account.
type KeepaliveResult struct {
	Accounts  int      `json:"accounts"`
	Refreshed int      `json:"refreshed"`
	Skipped   int      `json:"skipped"`
	Failed    int      `json:"failed"`
	Errors    []string `json:"errors,omitempty"`
}

// tokenRefreshFlight is one in-progress refresh shared by concurrent Ensure
// callers. done is closed after refreshed and err are published.
type tokenRefreshFlight struct {
	done      chan struct{}
	refreshed bool
	err       error
}

// tokenAccountSnapshot captures the credential and scheduler fields used by a
// refresh without holding Account.mu across I/O.
type tokenAccountSnapshot struct {
	auth           Auth
	removed        bool
	tokenExpired   bool
	stateDisabled  bool
	manualDisabled bool
}

func tokenAccountSnapshotOf(a *Account) tokenAccountSnapshot {
	if a == nil {
		return tokenAccountSnapshot{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return tokenAccountSnapshot{
		auth:           cloneAuth(a.Auth),
		removed:        a.removed,
		tokenExpired:   a.tokenExpired,
		stateDisabled:  a.State.Disabled,
		manualDisabled: a.State.ManualDisabled,
	}
}

// tokenAuthIsAPIKey recognizes both the persisted marker and a hand-built
// account whose token has a static-key prefix.
func tokenAuthIsAPIKey(auth Auth) bool {
	return auth.APIKey || IsAPIKeyToken(strings.TrimSpace(auth.AccessToken))
}

func (p *Provider) tokenNowTime() time.Time {
	if p != nil && p.tokenNow != nil {
		return p.tokenNow()
	}
	return time.Now()
}

func (p *Provider) tokenRefreshBase() string {
	if p == nil {
		return ""
	}
	if base := strings.TrimRight(strings.TrimSpace(p.tokenBaseURL), "/"); base != "" {
		return base
	}
	return strings.TrimRight(p.EndpointFor(productDesktop), "/")
}

func (p *Provider) tokenRefreshBudget() time.Duration {
	if p != nil && p.tokenTimeout > 0 {
		return p.tokenTimeout
	}
	return defaultTokenRefreshTimeout
}

// EnsureToken refreshes a's access token when it is expired or expires within
// v1's 120-second skew. API-key accounts, valid tokens, and tokens with no
// known expiry are no-ops. Concurrent calls for the same account share one
// upstream request; every waiter observes the same result.
func (p *Provider) EnsureToken(ctx context.Context, a *Account) error {
	_, err := p.ensureToken(ctx, a)
	return err
}

// ensureToken is EnsureToken plus whether an upstream rotation actually
// happened. Keepalive uses the count to report successful refreshes without
// treating a transient no-op as a refreshed account.
func (p *Provider) ensureToken(ctx context.Context, a *Account) (bool, error) {
	if p == nil {
		return false, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return false, errors.New("workbuddy: token refresh requires a context")
	}
	if a == nil {
		return false, errors.New("workbuddy: token refresh requires an account")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	snapshot := tokenAccountSnapshotOf(a)
	if snapshot.removed {
		return false, ErrAccountNotFound
	}
	if tokenAuthIsAPIKey(snapshot.auth) {
		return false, nil
	}
	if !tokenRefreshDue(snapshot.auth, p.tokenNowTime()) {
		return false, nil
	}

	p.tokenMu.Lock()
	if p.tokenFlights == nil {
		p.tokenFlights = make(map[*Account]*tokenRefreshFlight)
	}
	if flight := p.tokenFlights[a]; flight != nil {
		p.tokenMu.Unlock()
		select {
		case <-flight.done:
			if flight.err != nil {
				return false, flight.err
			}
			return flight.refreshed, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	flight := &tokenRefreshFlight{done: make(chan struct{})}
	p.tokenFlights[a] = flight
	p.tokenMu.Unlock()

	refreshed, err := p.refreshToken(ctx, a)
	if err != nil {
		err = p.redactTokenError(a, err)
	}

	p.tokenMu.Lock()
	flight.refreshed = refreshed
	flight.err = err
	delete(p.tokenFlights, a)
	close(flight.done)
	p.tokenMu.Unlock()
	return refreshed, err
}

// tokenRefreshDue mirrors v1 Account.ready: an account with no access token is
// due, a missing/unknown expiry is not refreshed proactively, and a known
// expiry is due once it is 120 seconds away or closer.
func tokenRefreshDue(auth Auth, now time.Time) bool {
	if tokenAuthIsAPIKey(auth) {
		return false
	}
	accessToken := strings.TrimSpace(auth.AccessToken)
	if accessToken == "" {
		return true
	}
	expiry := auth.ExpiresAt
	if expiry.IsZero() {
		expiry = epochTime(parseJWTClaims(accessToken).ExpiresAt)
	}
	if expiry.IsZero() {
		return false
	}
	return !now.Before(expiry.Add(-tokenRefreshSkew))
}

func tokenRefreshValid(snapshot tokenAccountSnapshot, now time.Time) bool {
	if snapshot.tokenExpired || strings.TrimSpace(snapshot.auth.AccessToken) == "" {
		return false
	}
	expiry := snapshot.auth.ExpiresAt
	if expiry.IsZero() {
		expiry = epochTime(parseJWTClaims(snapshot.auth.AccessToken).ExpiresAt)
	}
	return expiry.IsZero() || expiry.After(now)
}

// refreshToken performs one refresh for a. It re-checks due state after the
// single-flight lock so a caller that joins after a successful rotation does
// not spend the one-time refresh token again.
func (p *Provider) refreshToken(ctx context.Context, a *Account) (bool, error) {
	snapshot := tokenAccountSnapshotOf(a)
	if snapshot.removed {
		return false, ErrAccountNotFound
	}
	if tokenAuthIsAPIKey(snapshot.auth) {
		return false, nil
	}
	if !tokenRefreshDue(snapshot.auth, p.tokenNowTime()) {
		return false, nil
	}
	auth := snapshot.auth
	refreshToken := strings.TrimSpace(auth.RefreshToken)
	if refreshToken == "" {
		message := tokenTerminalMessage(auth.UID, "no refresh token; sign in again")
		p.noteTokenTerminal(a, message)
		return false, errors.New(message)
	}

	if p.transport == nil {
		return false, errors.New("workbuddy: token refresh transport is unavailable")
	}
	base := p.tokenRefreshBase()
	if base == "" {
		return false, errors.New("workbuddy: token refresh endpoint is unavailable")
	}

	requestCtx := nonNilContext(ctx)
	if budget := p.tokenRefreshBudget(); budget > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(requestCtx, budget)
		defer cancel()
	}

	request, err := http.NewRequestWithContext(
		requestCtx, http.MethodPost, base+tokenRefreshPath, bytes.NewReader([]byte("{}")),
	)
	if err != nil {
		return false, errors.New("workbuddy: token refresh request is invalid")
	}
	request.Header = p.tokenRefreshHeaders(auth)

	response, err := p.transport.Do(requestCtx, request, strings.TrimSpace(auth.ProxyURL))
	if err != nil {
		return p.noteTokenTransient(a, err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		message := tokenTerminalMessage(auth.UID,
			fmt.Sprintf("token refresh rejected (HTTP %d); sign in again", response.StatusCode))
		p.noteTokenTerminal(a, message)
		return false, errors.New(message)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return p.noteTokenTransient(a,
			fmt.Errorf("token refresh HTTP %d", response.StatusCode))
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxAuthBody))
	if err != nil {
		return p.noteTokenTransient(a, errors.New("token refresh response read failed"))
	}
	payload := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		message := tokenTerminalMessage(auth.UID,
			"token refresh returned an invalid response; sign in again")
		p.noteTokenTerminal(a, message)
		return false, errors.New(message)
	}

	data := tokenRefreshData(payload)
	accessToken := strings.TrimSpace(decodeAuthString(data["accessToken"]))
	if accessToken == "" {
		message := tokenTerminalMessage(auth.UID,
			"token refresh returned no access token; sign in again")
		p.noteTokenTerminal(a, message)
		return false, errors.New(message)
	}
	rotatedRefresh := strings.TrimSpace(decodeAuthString(data["refreshToken"]))
	expiry := decodeEpochTime(data["expiresAt"])
	if expiry.IsZero() {
		expiry = epochTime(parseJWTClaims(accessToken).ExpiresAt)
	}
	if expiry.IsZero() {
		expiry = auth.ExpiresAt
	}

	if err := p.applyRefreshedToken(a, refreshToken, accessToken, rotatedRefresh, expiry); err != nil {
		return false, err
	}
	return true, nil
}

// tokenRefreshData accepts the one- and two-level data envelopes used by the
// refresh endpoint ({"data": ...} and {"data":{"data": ...}}). Like v1, token
// fields outside data are not trusted.
func tokenRefreshData(payload map[string]json.RawMessage) map[string]json.RawMessage {
	data := decodeJSONObjectRaw(payload["data"])
	if nested := decodeJSONObjectRaw(data["data"]); len(nested) > 0 {
		return nested
	}
	return data
}

// tokenRefreshHeaders is v1 Account._refresh_locked's billing header set.
func (p *Provider) tokenRefreshHeaders(auth Auth) http.Header {
	origin := oauthOriginForRealm(p.realm)
	domain := strings.TrimSpace(auth.Domain)
	if domain == "" {
		domain = domainForRealm(p.realm)
	}
	source := "plugin"
	language := "en-US"
	if p.realm == realmCN {
		source = "workbuddy"
		language = "zh-CN"
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("User-Agent", "WorkBuddy/"+p.DesktopVersion())
	headers.Set("Origin", origin)
	headers.Set("Referer", origin+"/")
	headers.Set("X-Refresh-Token", strings.TrimSpace(auth.RefreshToken))
	headers.Set("X-Auth-Refresh-Source", source)
	headers.Set("X-User-Id", strings.TrimSpace(auth.UID))
	headers.Set("X-Domain", domain)
	headers.Set("X-CodeBuddy-Request", "1")
	headers.Set("Accept-Language", language)
	if enterpriseID := strings.TrimSpace(auth.EnterpriseID); enterpriseID != "" {
		headers.Set("X-Enterprise-Id", enterpriseID)
	}
	return headers
}

// applyRefreshedToken publishes a rotated credential and persists it. A
// concurrent login that replaced the refresh token wins; this refresh then
// does not overwrite the newer document.
func (p *Provider) applyRefreshedToken(
	a *Account,
	expectedRefresh, accessToken, rotatedRefresh string,
	expiry time.Time,
) error {
	if a == nil {
		return errors.New("workbuddy: token refresh requires an account")
	}
	a.mu.Lock()
	if a.removed {
		a.mu.Unlock()
		return ErrAccountNotFound
	}
	if strings.TrimSpace(a.Auth.RefreshToken) != expectedRefresh {
		// A login/import rotated the credential while the request was in
		// flight. Do not clobber it with the now-stale response.
		a.mu.Unlock()
		return nil
	}
	a.Auth.AccessToken = accessToken
	if rotatedRefresh != "" {
		a.Auth.RefreshToken = rotatedRefresh
	}
	if !expiry.IsZero() {
		a.Auth.ExpiresAt = expiry
	}
	a.tokenExpired = false
	a.State.LastError = ""
	a.State.ErrCount = 0
	a.State.CoolUntil = time.Time{}
	if !a.State.ManualDisabled {
		a.State.Disabled = false
		a.State.Reason = ""
		a.State.DisabledBy = ""
	}
	a.mu.Unlock()

	if err := p.persistRefreshedToken(a); err != nil {
		return fmt.Errorf("workbuddy: persist refreshed token: %w", err)
	}
	return nil
}

func (p *Provider) persistRefreshedToken(a *Account) error {
	if a == nil {
		return nil
	}
	if a.pool != nil {
		return a.pool.SaveAccount(a)
	}

	// Standalone accounts have no pool to own the write path. Persist through
	// the same v1-shaped helper when a path was supplied.
	a.mu.RLock()
	path := strings.TrimSpace(a.Path)
	auth := cloneAuth(a.Auth)
	runtimeProxy := a.Auth.ProxyURL
	a.mu.RUnlock()
	if path == "" {
		return nil
	}
	saved, err := SaveAccount(filepath.Dir(path), auth)
	if err != nil {
		return err
	}
	saved.ProxyURL = runtimeProxy
	a.mu.Lock()
	a.Auth = saved
	a.Path = saved.Path
	a.mu.Unlock()
	return nil
}

// noteTokenTerminal records a failure that cannot be recovered by refreshing.
// It marks the runtime token expired and last error, and auto-disables the
// account unless the operator already disabled it. Auth.Enabled and task
// participation are never changed here.
func (p *Provider) noteTokenTerminal(a *Account, message string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	message = truncateText(redactAccountText(message, accountSecretValuesLocked(a)), tokenErrorLimit)
	a.tokenExpired = true
	a.State.LastError = message
	if !a.State.ManualDisabled && !tokenAuthIsAPIKey(a.Auth) {
		a.State.Disabled = true
		a.State.Reason = message
		a.State.DisabledBy = "auth"
	}
	a.mu.Unlock()
}

// noteTokenTransient records a network/upstream failure without replacing the
// credential. v1 keeps serving while the old access token remains valid and
// reports the error once it has expired.
func (p *Provider) noteTokenTransient(a *Account, cause error) (bool, error) {
	snapshot := tokenAccountSnapshotOf(a)
	message := tokenTerminalMessage(snapshot.auth.UID, "refresh failed: "+cause.Error())
	message = p.redactTokenErrorText(a, message)
	a.mu.Lock()
	a.State.LastError = truncateText(message, tokenErrorLimit)
	a.mu.Unlock()

	if tokenRefreshValid(tokenAccountSnapshotOf(a), p.tokenNowTime()) {
		return false, nil
	}
	return false, errors.New(message)
}

func tokenTerminalMessage(uid, detail string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		uid = "unknown"
	}
	return fmt.Sprintf("workbuddy: token refresh for %s: %s", uid, detail)
}

func (p *Provider) redactTokenError(a *Account, err error) error {
	if err == nil {
		return nil
	}
	text := p.redactTokenErrorText(a, err.Error())
	if text == err.Error() {
		return err
	}
	return errors.New(text)
}

func (p *Provider) redactTokenErrorText(a *Account, text string) string {
	if a == nil {
		return redactAccountText(text, nil)
	}
	a.mu.RLock()
	values := accountSecretValuesLocked(a)
	a.mu.RUnlock()
	return redactAccountText(text, values)
}

// Keepalive refreshes every due, enabled OAuth account in this realm's pool.
// Disabled, manual-disabled, removed, API-key, and not-yet-due accounts are
// skipped. One account's failure never aborts the sweep; failures are returned
// as redacted per-account messages alongside the result. It performs no
// check-in side effect.
func (p *Provider) Keepalive(ctx context.Context) (KeepaliveResult, error) {
	result := KeepaliveResult{}
	if p == nil {
		return result, errors.New("workbuddy: provider is nil")
	}
	if ctx == nil {
		return result, errors.New("workbuddy: keepalive requires a context")
	}
	pool := p.Pool()
	if pool == nil {
		return result, errors.New("workbuddy: keepalive requires an account pool")
	}

	accounts := pool.Accounts()
	var failures []error
	for _, account := range accounts {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		snapshot := tokenAccountSnapshotOf(account)
		result.Accounts++

		if snapshot.removed || tokenAuthIsAPIKey(snapshot.auth) ||
			!snapshot.auth.Enabled || snapshot.stateDisabled || snapshot.manualDisabled {
			result.Skipped++
			continue
		}
		if !tokenRefreshDue(snapshot.auth, p.tokenNowTime()) {
			result.Skipped++
			continue
		}

		refreshed, err := p.ensureToken(ctx, account)
		if err != nil {
			result.Failed++
			uid := strings.TrimSpace(snapshot.auth.UID)
			if uid == "" {
				uid = "unknown"
			}
			message := uid + ": " + p.redactTokenErrorText(account, err.Error())
			result.Errors = append(result.Errors, message)
			failures = append(failures, errors.New(message))
			if ctxErr := ctx.Err(); ctxErr != nil {
				failures = append(failures, ctxErr)
				break
			}
			continue
		}
		if refreshed {
			result.Refreshed++
		} else {
			result.Skipped++
		}
	}
	if len(failures) > 0 {
		return result, errors.Join(failures...)
	}
	return result, nil
}
