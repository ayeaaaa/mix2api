package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

	// keepaliveRefreshLead is v1 wb_scheduler's default 48-hour lead. The
	// scheduled keepalive runs daily at 22:00; Task 10 owns that cadence.
	// WB_KEEPALIVE_LEAD_HOURS may override it between 1 and 720 hours.
	// EnsureToken keeps the request-path 120-second skew.
	keepaliveRefreshLead  = 48 * time.Hour
	keepaliveLeadMinHours = 1.0
	keepaliveLeadMaxHours = 24.0 * 30

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

// tokenRefreshMode selects the expiry predicate for one refresh attempt.
type tokenRefreshMode uint8

const (
	// tokenRefreshRequestPath is EnsureToken's v1 120-second readiness skew.
	tokenRefreshRequestPath tokenRefreshMode = iota
	// tokenRefreshScheduled is Keepalive's v1 48-hour scheduled lead. An
	// unknown/opaque expiry is due.
	tokenRefreshScheduled
	// tokenRefreshForced is ForceRefresh's post-401 retry and ignores expiry.
	tokenRefreshForced
)

// tokenRefreshFlight is one in-progress refresh shared by concurrent refresh
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
	_, err := p.ensureToken(ctx, a, tokenRefreshRequestPath)
	return err
}

// ForceRefresh rotates a's credential regardless of its recorded expiry. Task 7
// calls it after a chat 401 so a token that is marked expired or still carries
// a future expiry is retried once with the same terminal-failure semantics as
// EnsureToken. Static API keys remain no-ops, and concurrent refreshes share
// one flight.
func (p *Provider) ForceRefresh(ctx context.Context, a *Account) error {
	_, err := p.ensureToken(ctx, a, tokenRefreshForced)
	return err
}

// ensureToken is EnsureToken plus whether an upstream rotation actually
// happened. Keepalive uses the count to report successful refreshes without
// treating a transient no-op or a superseded response as a refreshed account.
func (p *Provider) ensureToken(ctx context.Context, a *Account, mode tokenRefreshMode) (bool, error) {
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
	if mode != tokenRefreshForced && !tokenRefreshNeeded(snapshot.auth, p.tokenNowTime(), mode) {
		return false, nil
	}

	p.tokenMu.Lock()
	if p.tokenFlights == nil {
		p.tokenFlights = make(map[*Account]*tokenRefreshFlight)
	}
	if flight := p.tokenFlights[a]; flight != nil {
		p.tokenMu.Unlock()
		if p.tokenJoinHook != nil {
			p.tokenJoinHook()
		}
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

	refreshed, err := p.refreshToken(ctx, a, mode)
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

func tokenCredentialValid(auth Auth, tokenExpired bool, now time.Time) bool {
	if tokenExpired || strings.TrimSpace(auth.AccessToken) == "" {
		return false
	}
	expiry := auth.ExpiresAt
	if expiry.IsZero() {
		expiry = epochTime(parseJWTClaims(auth.AccessToken).ExpiresAt)
	}
	return expiry.IsZero() || expiry.After(now)
}

// tokenKeepaliveDue is v1 wb_scheduler.keepalive_due: scheduled keepalive uses
// a 48-hour lead and treats an unknown/opaque expiry as due. EnsureToken keeps
// the request-path 120-second skew instead.
func tokenKeepaliveDue(auth Auth, now time.Time) bool {
	if tokenAuthIsAPIKey(auth) {
		return false
	}
	if strings.TrimSpace(auth.AccessToken) == "" {
		return true
	}
	expiry := auth.ExpiresAt
	if expiry.IsZero() {
		expiry = epochTime(parseJWTClaims(auth.AccessToken).ExpiresAt)
	}
	if expiry.IsZero() {
		return true
	}
	return expiry.Sub(now) < keepaliveLead()
}

// keepaliveLead mirrors v1 wb_scheduler.keepalive_lead_seconds: the
// WB_KEEPALIVE_LEAD_HOURS override is honoured only for finite values in
// [1, 720]; missing or invalid values fall back to 48 hours.
func keepaliveLead() time.Duration {
	raw := strings.TrimSpace(os.Getenv("WB_KEEPALIVE_LEAD_HOURS"))
	if raw == "" {
		return keepaliveRefreshLead
	}
	hours, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(hours) || math.IsInf(hours, 0) ||
		hours < keepaliveLeadMinHours || hours > keepaliveLeadMaxHours {
		return keepaliveRefreshLead
	}
	return time.Duration(hours * float64(time.Hour))
}

func tokenRefreshNeeded(auth Auth, now time.Time, mode tokenRefreshMode) bool {
	switch mode {
	case tokenRefreshScheduled:
		return tokenKeepaliveDue(auth, now)
	case tokenRefreshForced:
		return true
	default:
		return tokenRefreshDue(auth, now)
	}
}

// tokenCredentialsCurrentLocked verifies that the credential snapshot that
// produced a refresh response is still installed. Callers hold Account.mu.
func tokenCredentialsCurrentLocked(a *Account, snapshot tokenAccountSnapshot) bool {
	if a == nil || a.removed || snapshot.removed {
		return false
	}
	return strings.TrimSpace(a.Auth.UID) == strings.TrimSpace(snapshot.auth.UID) &&
		a.Auth.AccessToken == snapshot.auth.AccessToken &&
		strings.TrimSpace(a.Auth.RefreshToken) == strings.TrimSpace(snapshot.auth.RefreshToken)
}

// refreshToken performs one refresh for a. It re-checks the mode's due state
// after the single-flight lock so a caller that joins after a re-import or a
// successful rotation does not spend the one-time refresh token again.
func (p *Provider) refreshToken(ctx context.Context, a *Account, mode tokenRefreshMode) (bool, error) {
	snapshot := tokenAccountSnapshotOf(a)
	if snapshot.removed {
		return false, ErrAccountNotFound
	}
	if tokenAuthIsAPIKey(snapshot.auth) {
		return false, nil
	}
	if mode != tokenRefreshForced && !tokenRefreshNeeded(snapshot.auth, p.tokenNowTime(), mode) {
		return false, nil
	}
	auth := snapshot.auth
	refreshToken := strings.TrimSpace(auth.RefreshToken)
	if refreshToken == "" {
		message := tokenTerminalMessage(auth.UID, "no refresh token; sign in again")
		if p.noteTokenTerminal(a, snapshot, message) {
			return false, errors.New(message)
		}
		return false, nil
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
		return p.noteTokenTransient(a, snapshot, err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		drainTokenResponse(response.Body)
		message := tokenTerminalMessage(auth.UID,
			fmt.Sprintf("token refresh rejected (HTTP %d); sign in again", response.StatusCode))
		if p.noteTokenTerminal(a, snapshot, message) {
			return false, errors.New(message)
		}
		return false, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		drainTokenResponse(response.Body)
		return p.noteTokenTransient(a, snapshot,
			fmt.Errorf("token refresh HTTP %d", response.StatusCode))
	}

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxAuthBody))
	if err != nil {
		return p.noteTokenTransient(a, snapshot, errors.New("token refresh response read failed"))
	}
	payload := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		// A 2xx body that is HTML, empty, null, or malformed is a transient
		// upstream/proxy failure, not proof that the refresh token is dead.
		return p.noteTokenTransient(a, snapshot,
			errors.New("token refresh returned an invalid response"))
	}

	data := tokenRefreshData(payload)
	accessToken := strings.TrimSpace(decodeAuthString(data["accessToken"]))
	if accessToken == "" {
		message := tokenTerminalMessage(auth.UID,
			"token refresh returned no access token; sign in again")
		if p.noteTokenTerminal(a, snapshot, message) {
			return false, errors.New(message)
		}
		return false, nil
	}
	rotatedRefresh := strings.TrimSpace(decodeAuthString(data["refreshToken"]))
	expiry := decodeEpochTime(data["expiresAt"])
	if expiry.IsZero() {
		expiry = epochTime(parseJWTClaims(accessToken).ExpiresAt)
	}
	if expiry.IsZero() {
		expiry = auth.ExpiresAt
	}

	applied, err := p.applyRefreshedToken(a, snapshot, accessToken, rotatedRefresh, expiry)
	if err != nil {
		return false, err
	}
	return applied, nil
}

// drainTokenResponse consumes a bounded non-2xx body so net/http can reuse the
// connection. Body text is never surfaced or logged.
func drainTokenResponse(body io.Reader) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxAuthBody))
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
// concurrent Pool.Add/login/import that replaced the UID or either token wins;
// this stale response is then ignored and reported as not refreshed.
func (p *Provider) applyRefreshedToken(
	a *Account,
	snapshot tokenAccountSnapshot,
	accessToken, rotatedRefresh string,
	expiry time.Time,
) (bool, error) {
	if a == nil {
		return false, errors.New("workbuddy: token refresh requires an account")
	}
	a.mu.Lock()
	if a.removed {
		a.mu.Unlock()
		return false, ErrAccountNotFound
	}
	if !tokenCredentialsCurrentLocked(a, snapshot) {
		// A login/import replaced the credential while the request was in
		// flight. Do not clobber it with the now-stale response.
		a.mu.Unlock()
		return false, nil
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
		return true, fmt.Errorf("workbuddy: persist refreshed token: %w", err)
	}
	return true, nil
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
// It applies only while the credential snapshot that produced the failure is
// still installed, so a concurrent re-import is never disabled by a stale 401.
// It marks the runtime token expired and last error, and auto-disables the
// account unless the operator already disabled it. Auth.Enabled and task
// participation are never changed here.
func (p *Provider) noteTokenTerminal(a *Account, snapshot tokenAccountSnapshot, message string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !tokenCredentialsCurrentLocked(a, snapshot) {
		return false
	}
	message = truncateText(redactAccountText(message, accountSecretValuesLocked(a)), tokenErrorLimit)
	a.tokenExpired = true
	a.State.LastError = message
	if !a.State.ManualDisabled && !tokenAuthIsAPIKey(a.Auth) {
		a.State.Disabled = true
		a.State.Reason = message
		a.State.DisabledBy = "auth"
	}
	return true
}

// noteTokenTransient records a network/upstream failure without replacing the
// credential. A stale response for credentials replaced by a re-import is
// ignored. Otherwise v1 keeps serving while the old access token remains valid
// and reports the error once it has expired.
func (p *Provider) noteTokenTransient(a *Account, snapshot tokenAccountSnapshot, cause error) (bool, error) {
	if a == nil {
		return false, nil
	}
	a.mu.Lock()
	if !tokenCredentialsCurrentLocked(a, snapshot) {
		a.mu.Unlock()
		return false, nil
	}
	message := tokenTerminalMessage(snapshot.auth.UID, "refresh failed: "+cause.Error())
	message = truncateText(redactAccountText(message, accountSecretValuesLocked(a)), tokenErrorLimit)
	a.State.LastError = message
	valid := tokenCredentialValid(a.Auth, a.tokenExpired, p.tokenNowTime())
	a.mu.Unlock()

	if valid {
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
// Due uses v1's scheduled 48-hour lead, not EnsureToken's 120-second request
// skew, and an opaque/unknown expiry counts as due. Disabled, manual-disabled,
// removed, API-key, and not-yet-due accounts are skipped. One account's failure
// never aborts the sweep; failures and mid-sweep cancellation are returned as
// redacted per-account messages alongside the result. It performs no check-in
// side effect.
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
	canceled := func() bool {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			return true
		}
		return false
	}
	for _, account := range accounts {
		if canceled() {
			break
		}
		snapshot := tokenAccountSnapshotOf(account)
		result.Accounts++

		if snapshot.removed || tokenAuthIsAPIKey(snapshot.auth) ||
			!snapshot.auth.Enabled || snapshot.stateDisabled || snapshot.manualDisabled {
			result.Skipped++
			if canceled() {
				break
			}
			continue
		}
		if !tokenKeepaliveDue(snapshot.auth, p.tokenNowTime()) {
			result.Skipped++
			if canceled() {
				break
			}
			continue
		}

		// Keepalive applies the 48-hour scheduled lead and re-checks it under
		// the same single flight so a concurrent re-import is not refreshed
		// again with a stale due decision.
		refreshed, err := p.ensureToken(ctx, account, tokenRefreshScheduled)
		if err != nil {
			result.Failed++
			uid := strings.TrimSpace(snapshot.auth.UID)
			if uid == "" {
				uid = "unknown"
			}
			message := uid + ": " + p.redactTokenErrorText(account, err.Error())
			result.Errors = append(result.Errors, message)
			failures = append(failures, errors.New(message))
			if canceled() {
				break
			}
			continue
		}
		if refreshed {
			result.Refreshed++
		} else {
			result.Skipped++
		}
		if canceled() {
			break
		}
	}
	if len(failures) > 0 {
		return result, errors.Join(failures...)
	}
	return result, nil
}
