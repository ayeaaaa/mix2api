package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

const (
	// refreshSkew is v1's REFRESH_SKEW: an access token is refreshed this long
	// before it expires.
	refreshSkew = 30 * time.Minute

	// defaultTokenRefreshTimeout bounds one refresh when the caller does not
	// provide one. Production wires cfg.TimeoutSeconds through the Provider.
	defaultTokenRefreshTimeout = 30 * time.Second
)

// ErrRefreshTokenMissing means the account cannot be recovered without a new
// interactive login. Ensuring a token for an expired account returns it.
var ErrRefreshTokenMissing = errors.New("codearts: refresh_token is missing; account requires re-login")

// TokenManagerOptions configures a TokenManager.
type TokenManagerOptions struct {
	// Pool is the account pool Keepalive iterates. Ensure does not require it.
	Pool *Pool

	// Timeout bounds one refresh request. Zero uses 30 seconds.
	Timeout time.Duration

	// Now is the clock used for skew/expiry checks and updated_at. Nil uses
	// time.Now.
	Now func() time.Time

	// ProxyURL resolves the outbound proxy for an account. Nil uses the
	// account's currently resolved proxy slot.
	ProxyURL func(*Account) string
}

// TokenManager refreshes CodeArts access tokens. Refresh tokens are one-time
// credentials, so concurrent Ensure calls for one account coalesce into a
// single request while every caller observes the effective result. Rotated
// credentials are verified, persisted (0600), and only then published in
// memory.
type TokenManager struct {
	client   *transport.Client
	timeout  time.Duration
	now      func() time.Time
	proxyURL func(*Account) string
	pool     *Pool

	mu      sync.Mutex
	flights map[*Account]*tokenRefreshFlight

	// flightJoinHook is a test seam invoked after a caller joins an existing
	// refresh flight and before it waits for the result. Nil in production.
	flightJoinHook func()
}

type tokenRefreshFlight struct {
	done chan struct{}
	err  error
}

// tokenAccountSnapshot captures one account's credential and scheduler fields
// under its lock so a refresh never reads a torn Auth/State pair.
type tokenAccountSnapshot struct {
	uid            string
	path           string
	auth           Auth
	expiration     time.Time
	disabled       bool
	manualDisabled bool
	removed        bool
	generation     uint64
}

// NewTokenManager returns a manager using the shared core transport. The
// manager performs no network I/O until Ensure or Keepalive is called.
func NewTokenManager(client *transport.Client, opts TokenManagerOptions) *TokenManager {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTokenRefreshTimeout
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	proxyURL := opts.ProxyURL
	if proxyURL == nil {
		proxyURL = func(a *Account) string {
			if a == nil {
				return ""
			}
			return a.ProxyURL()
		}
	}
	return &TokenManager{
		client:   client,
		timeout:  timeout,
		now:      now,
		proxyURL: proxyURL,
		pool:     opts.Pool,
		flights:  make(map[*Account]*tokenRefreshFlight),
	}
}

// Ensure refreshes a when its access token is expired or expires within the
// 30-minute skew. An account with no recorded expiration is left untouched.
// Concurrent calls for one account share one request and recheck the expiry
// after winning the single-flight slot.
func (m *TokenManager) Ensure(ctx context.Context, a *Account) error {
	if err := m.validate(ctx, a); err != nil {
		return err
	}
	snapshot := a.tokenSnapshot()
	if snapshot.removed {
		return ErrAccountNotFound
	}
	if snapshot.manualDisabled {
		// A manual disable is an operator decision. Do not spend the
		// account's one-time refresh token until Enable clears it.
		return nil
	}
	if !tokenRefreshDue(snapshot.expiration, m.nowTime()) {
		return nil
	}

	m.mu.Lock()
	if flight := m.flights[a]; flight != nil {
		m.mu.Unlock()
		if m.flightJoinHook != nil {
			m.flightJoinHook()
		}
		if err := waitTokenRefresh(ctx, flight); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		return nil
	}
	flight := &tokenRefreshFlight{done: make(chan struct{})}
	if m.flights == nil {
		m.flights = make(map[*Account]*tokenRefreshFlight)
	}
	m.flights[a] = flight
	m.mu.Unlock()

	err := m.refresh(ctx, a)
	if err != nil {
		snapshot := a.tokenSnapshot()
		err = redactOAuthError(err, codeartsSecretValues(snapshot.auth)...)
	}

	m.mu.Lock()
	flight.err = err
	delete(m.flights, a)
	close(flight.done)
	m.mu.Unlock()
	return err
}

func (m *TokenManager) validate(ctx context.Context, a *Account) error {
	if m == nil {
		return errors.New("codearts: token manager is nil")
	}
	if ctx == nil {
		return errors.New("codearts: token refresh requires a context")
	}
	if a == nil {
		return errors.New("codearts: token refresh requires an account")
	}
	if m.client == nil {
		return errors.New("codearts: token refresh transport is nil")
	}
	return nil
}

// tokenRefreshDue applies the fast path and the post-single-flight recheck.
func tokenRefreshDue(expiration, now time.Time) bool {
	if expiration.IsZero() {
		return false
	}
	return !now.Before(expiration.Add(-refreshSkew))
}

func (m *TokenManager) nowTime() time.Time {
	if m == nil || m.now == nil {
		return time.Now()
	}
	return m.now()
}

func tokenExpired(expiration, now time.Time) bool {
	if expiration.IsZero() {
		return false
	}
	return !now.Before(expiration)
}

func waitTokenRefresh(ctx context.Context, flight *tokenRefreshFlight) error {
	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// refresh performs one account refresh. The request context deliberately does
// not inherit the caller's cancellation: every waiter shares the result, so a
// canceled caller must not abort a rotation the others are waiting for. The
// configured timeout still bounds the request.
func (m *TokenManager) refresh(ctx context.Context, a *Account) error {
	snapshot := a.tokenSnapshot()
	if snapshot.removed {
		return ErrAccountNotFound
	}
	if snapshot.manualDisabled {
		return nil
	}
	if !tokenRefreshDue(snapshot.expiration, m.nowTime()) {
		return nil
	}

	auth := snapshot.auth
	for attempt := 0; attempt < 2 && !usableDPoPKey(auth.DPopPrivateKey); attempt++ {
		key, err := NewDPoPKey()
		if err != nil {
			return err
		}
		auth.DPopPrivateKey = key
		wrote, generation, err := m.writeAuth(a, snapshot, auth)
		if err != nil {
			return err
		}
		if wrote {
			snapshot.auth = auth
			snapshot.generation = generation
			break
		}
		// A login/refresh replaced or proxy-updated the credentials while the
		// key was being generated. Re-read and retry once for the latest view.
		snapshot = a.tokenSnapshot()
		if snapshot.removed {
			return ErrAccountNotFound
		}
		if snapshot.manualDisabled {
			return nil
		}
		if !tokenRefreshDue(snapshot.expiration, m.nowTime()) {
			return nil
		}
		auth = snapshot.auth
	}
	if !usableDPoPKey(auth.DPopPrivateKey) {
		return errors.New("codearts: generated DPoP key was not applied")
	}

	refreshToken := strings.TrimSpace(auth.RefreshToken)
	if refreshToken == "" {
		applied, noteErr := m.noteTerminalIfCurrent(a, snapshot, ErrRefreshTokenMissing.Error())
		if !applied {
			return nil
		}
		return errors.Join(ErrRefreshTokenMissing, noteErr)
	}

	clientID := strings.TrimSpace(auth.ClientID)
	if clientID == "" {
		clientID = oauthClientID()
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("code_verifier", auth.CodeVerifier)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", auth.RefreshToken)

	requestCtx := context.WithoutCancel(ctx)
	if m.timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(requestCtx, m.timeout)
		defer cancel()
	}
	proxyURL := ""
	if m.proxyURL != nil {
		proxyURL = strings.TrimSpace(m.proxyURL(a))
	}

	token, err := requestTokenContextWith(requestCtx, m.client, proxyURL, form, auth.DPopPrivateKey)
	if err != nil {
		return m.handleRefreshFailure(a, snapshot, auth, err)
	}

	updated := authFromRefreshToken(auth, token, m.nowTime())
	wrote, _, err := m.writeAuth(a, snapshot, updated)
	if err != nil {
		return err
	}
	if !wrote {
		// A newer credential document won the race; do not overwrite it.
		return nil
	}
	return m.noteSuccess(a)
}

func (m *TokenManager) handleRefreshFailure(a *Account, snapshot tokenAccountSnapshot, auth Auth, err error) error {
	if isTerminalRefreshError(err) {
		message := fmt.Sprintf("refresh_token 已失效或被拒，需要重新登录：%s", err.Error())
		applied, noteErr := m.noteTerminalIfCurrent(a, snapshot, message)
		if !applied {
			return nil
		}
		return errors.Join(err, noteErr)
	}

	if !m.stillCurrent(a, snapshot) {
		return nil
	}
	// v1 swallows transient failures while the current access token is still
	// valid, and reports them once it has expired.
	if tokenExpired(auth.Expiration, m.nowTime()) {
		return err
	}
	return nil
}

// stillCurrent reports whether the credentials that produced a failed refresh
// are still installed. A concurrent login/import supersedes the failure.
func (m *TokenManager) stillCurrent(a *Account, snapshot tokenAccountSnapshot) bool {
	if a == nil {
		return false
	}
	if pool := a.pool; pool != nil {
		pool.authOpsMu.Lock()
		defer pool.authOpsMu.Unlock()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return stillCurrentLocked(a, snapshot)
}

func stillCurrentLocked(a *Account, snapshot tokenAccountSnapshot) bool {
	if a == nil || a.removed {
		return false
	}
	if a.uidLocked() != snapshot.uid ||
		a.authGeneration != snapshot.generation {
		return false
	}
	return sameCredentialIdentity(a.Auth, snapshot.auth)
}

// noteTerminalIfCurrent verifies the refresh snapshot and applies the terminal
// disable while authOpsMu is held, so a concurrent re-import/proxy save cannot
// intercept the check between verification and the state mutation. NoteError
// takes stateMu and Account.mu in the documented authOpsMu -> stateMu ->
// Account.mu order.
func (m *TokenManager) noteTerminalIfCurrent(
	a *Account,
	snapshot tokenAccountSnapshot,
	message string,
) (bool, error) {
	if a == nil {
		return false, nil
	}
	if pool := a.pool; pool != nil {
		pool.authOpsMu.Lock()
		defer pool.authOpsMu.Unlock()

		a.mu.Lock()
		current := stillCurrentLocked(a, snapshot)
		secrets := codeartsSecretValues(a.Auth)
		a.mu.Unlock()
		if !current {
			return false, nil
		}
		return true, pool.NoteError(a, redactText(message, secrets), false, true)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if !stillCurrentLocked(a, snapshot) {
		return false, nil
	}
	message = redactText(message, codeartsSecretValues(a.Auth))
	a.State.LastError = truncateRunes(message, maxStateNoteRunes)
	if !a.State.ManualDisabled {
		a.State.Disabled = true
		a.State.Reason = a.State.LastError
	}
	return true, nil
}

// sameCredentialIdentity compares the credential material that identifies an
// account across auth-document rewrites. UpdatedAt, proxy binding, unknown
// extras, and display fields are intentionally excluded so a semantically
// identical re-import does not invalidate an in-flight refresh.
func sameCredentialIdentity(current, previous Auth) bool {
	return strings.TrimSpace(current.UserID) == strings.TrimSpace(previous.UserID) &&
		strings.TrimSpace(current.RefreshToken) == strings.TrimSpace(previous.RefreshToken) &&
		current.CloudDragonToken == previous.CloudDragonToken &&
		current.AccessKeyID == previous.AccessKeyID &&
		current.SecretAccessKey == previous.SecretAccessKey &&
		current.ClientID == previous.ClientID &&
		current.CodeVerifier == previous.CodeVerifier &&
		sameDPoPKey(current.DPopPrivateKey, previous.DPopPrivateKey)
}

func sameDPoPKey(left, right json.RawMessage) bool {
	compact := func(raw json.RawMessage) []byte {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			return nil
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, trimmed); err != nil {
			return trimmed
		}
		return buf.Bytes()
	}
	leftJSON, rightJSON := compact(left), compact(right)
	if leftJSON == nil || rightJSON == nil {
		return leftJSON == nil && rightJSON == nil
	}
	if bytes.Equal(leftJSON, rightJSON) {
		return true
	}
	leftKey, leftErr := parseDPoPPrivateKey(leftJSON)
	rightKey, rightErr := parseDPoPPrivateKey(rightJSON)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return leftKey.D.Cmp(rightKey.D) == 0 &&
		leftKey.X.Cmp(rightKey.X) == 0 &&
		leftKey.Y.Cmp(rightKey.Y) == 0
}

func usableDPoPKey(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	_, err := parseDPoPPrivateKey(raw)
	return err == nil
}

// writeAuth verifies generation, path ownership, and credential identity,
// persists candidate atomically, and only then publishes it on the account. It
// returns false without writing when a newer credential document replaced the
// snapshot. A concurrent proxy-only save keeps the same generation and
// semantic identity, so the refreshed token is merged with the latest
// ProxySlot rather than discarded.
func (m *TokenManager) writeAuth(a *Account, snapshot tokenAccountSnapshot, candidate Auth) (bool, uint64, error) {
	if a == nil {
		return false, 0, ErrAccountNotFound
	}

	var pool *Pool
	pool = a.pool
	if pool != nil {
		pool.authOpsMu.Lock()
		defer pool.authOpsMu.Unlock()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.removed {
		return false, a.authGeneration, ErrAccountNotFound
	}
	uid := a.uidLocked()
	if strings.TrimSpace(uid) == "" {
		return false, a.authGeneration, errors.New("codearts: token refresh requires a user_id")
	}
	if uid != snapshot.uid || !sameCredentialIdentity(a.Auth, snapshot.auth) {
		return false, a.authGeneration, nil
	}
	// A generation bump with identical credential material is an identical
	// re-import (for example a panel re-save that changed only updated_at,
	// proxy fields, or extras). The rotated one-time token is still valid for
	// the current credentials, so merge the latest metadata and publish it.
	candidate = mergeTokenAuthMetadata(candidate, a.Auth, snapshot.auth)

	path := strings.TrimSpace(a.Path)
	if path == "" {
		path = strings.TrimSpace(snapshot.path)
	}
	if path == "" {
		return false, a.authGeneration, errors.New("codearts: token refresh requires an auth path")
	}
	if err := verifyAuthOwnership(path, uid); err != nil {
		return false, a.authGeneration, err
	}

	if err := SaveAuth(path, candidate); err != nil {
		return false, a.authGeneration, fmt.Errorf("codearts: save refreshed auth: %w", err)
	}
	a.Auth = candidate
	a.Path = path
	a.authGeneration++
	return true, a.authGeneration, nil
}

// mergeTokenAuthMetadata keeps the latest proxy binding, extras, and a
// re-imported updated_at while preserving the refreshed credential payload.
func mergeTokenAuthMetadata(candidate, current, snapshot Auth) Auth {
	candidate.ProxySlot = current.ProxySlot
	if candidate.UpdatedAt == snapshot.UpdatedAt {
		candidate.UpdatedAt = current.UpdatedAt
	}
	if len(current.extraFields) > 0 {
		merged := make(map[string]json.RawMessage, len(candidate.extraFields)+len(current.extraFields))
		for key, value := range candidate.extraFields {
			merged[key] = value
		}
		for key, value := range current.extraFields {
			merged[key] = value
		}
		candidate.extraFields = merged
	}
	return candidate
}

// authFromRefreshToken merges a successful token response like v1
// update_auth: all STS credentials, the rotated refresh token, normalized
// identity fields when returned, and updated_at. PKCE/client identity, the
// DPoP key, proxy slot, unknown fields, and the established UID are preserved.
func authFromRefreshToken(old Auth, token Token, now time.Time) Auth {
	updated := old
	// Keep the established UID. Accepting a changed identity would leave the
	// credential document under its old canonical path while changing the
	// pool's authoritative UID, risking collisions with another account.
	if value := strings.TrimSpace(token.UserID); value != "" &&
		strings.TrimSpace(updated.UserID) == "" {
		updated.UserID = value
	}
	if value := strings.TrimSpace(token.UserName); value != "" {
		updated.UserName = value
	}
	if value := strings.TrimSpace(token.DomainID); value != "" {
		updated.DomainID = value
	}
	updated.CloudDragonToken = token.Credentials.SecurityToken
	updated.AccessKeyID = token.Credentials.AccessKeyID
	updated.SecretAccessKey = token.Credentials.SecretAccessKey
	updated.Expiration = parseExpirationText(token.Credentials.Expiration)
	if value := strings.TrimSpace(token.RefreshToken); value != "" {
		updated.RefreshToken = value
	}
	updated.UpdatedAt = now.Unix()
	return updated
}

func isTerminalRefreshError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRefreshTokenMissing) {
		return true
	}
	var endpoint *TokenEndpointError
	if !errors.As(err, &endpoint) {
		return false
	}
	if endpoint.Status == http.StatusUnauthorized || endpoint.Status == http.StatusForbidden {
		return true
	}
	text := strings.ToLower(endpoint.Error())
	for _, marker := range []string{
		"invalid_grant",
		"expiredrefreshtoken",
		"expired_refresh_token",
		"expired refresh token",
		"invaliddpopheader",
		"invalid dpop",
		"invalid client id",
		"invalid_client",
		"has been used",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// noteTokenSuccess mirrors v1 update_auth's state cleanup: only disabled,
// reason, and last_error are cleared for a non-manual failure. Error counters
// and cooldowns are intentionally left to the scheduler.
func (m *TokenManager) noteSuccess(a *Account) error {
	if a == nil {
		return nil
	}
	if pool := a.pool; pool != nil {
		return pool.noteTokenSuccess(a)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.State.ManualDisabled {
		return nil
	}
	a.State.LastError = ""
	a.State.Disabled = false
	a.State.Reason = ""
	return nil
}

// noteTokenSuccess clears v1's post-login failure fields after a successful
// token rotation and persists one state snapshot. err_count and cool_until are
// scheduler history and are not touched by v1 update_auth.
func (p *Pool) noteTokenSuccess(a *Account) error {
	if a == nil {
		return nil
	}
	if p == nil {
		return errors.New("codearts: pool is nil")
	}
	return p.mutateState(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.State.ManualDisabled {
			return
		}
		a.State.LastError = ""
		a.State.Disabled = false
		a.State.Reason = ""
	})
}

// Keepalive refreshes every account in a pool snapshot whose token is expiring
// or expired. Disabled, manual-disabled, and removed accounts are skipped.
// Failures do not abort the sweep; the returned error joins redacted
// per-account errors and the count reports successful refreshes.
func (m *TokenManager) Keepalive(ctx context.Context) (int, error) {
	if m == nil {
		return 0, errors.New("codearts: token manager is nil")
	}
	if ctx == nil {
		return 0, errors.New("codearts: keepalive requires a context")
	}
	if m.pool == nil {
		return 0, errors.New("codearts: keepalive requires an account pool")
	}

	accounts := m.pool.Accounts()
	refreshed := 0
	var errs []error
	for _, account := range accounts {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		snapshot := account.tokenSnapshot()
		if snapshot.removed || snapshot.disabled || snapshot.manualDisabled {
			continue
		}
		if !tokenRefreshDue(snapshot.expiration, m.nowTime()) {
			continue
		}

		if err := m.Ensure(ctx, account); err != nil {
			uid := snapshot.uid
			if uid == "" {
				uid = "unknown"
			}
			redacted := redactOAuthError(err, codeartsSecretValues(snapshot.auth)...)
			errs = append(errs, fmt.Errorf("codearts: refresh %s: %w", uid, redacted))
			if ctxErr := ctx.Err(); ctxErr != nil {
				errs = append(errs, ctxErr)
				break
			}
			continue
		}
		refreshed++
		if ctxErr := ctx.Err(); ctxErr != nil {
			errs = append(errs, ctxErr)
			break
		}
	}

	if len(errs) > 0 {
		return refreshed, errors.Join(errs...)
	}
	return refreshed, nil
}

func (a *Account) tokenSnapshot() tokenAccountSnapshot {
	if a == nil {
		return tokenAccountSnapshot{removed: true}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return tokenAccountSnapshot{
		uid:            a.uidLocked(),
		path:           a.Path,
		auth:           a.Auth,
		expiration:     a.Auth.Expiration,
		disabled:       a.State.Disabled,
		manualDisabled: a.State.ManualDisabled,
		removed:        a.removed,
		generation:     a.authGeneration,
	}
}
