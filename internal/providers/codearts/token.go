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
	if !tokenRefreshDue(snapshot.expiration, m.nowTime()) {
		return nil
	}

	m.mu.Lock()
	if flight := m.flights[a]; flight != nil {
		m.mu.Unlock()
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
	if !tokenRefreshDue(snapshot.expiration, m.nowTime()) {
		return nil
	}

	auth := snapshot.auth
	if !usableDPoPKey(auth.DPopPrivateKey) {
		key, err := NewDPoPKey()
		if err != nil {
			return err
		}
		auth.DPopPrivateKey = key
		wrote, err := m.writeAuth(a, snapshot, auth)
		if err != nil {
			return err
		}
		if !wrote {
			// A login/refresh replaced the credentials while the key was being
			// generated. Refresh only the latest view if it is still due.
			snapshot = a.tokenSnapshot()
			if snapshot.removed {
				return ErrAccountNotFound
			}
			if !tokenRefreshDue(snapshot.expiration, m.nowTime()) {
				return nil
			}
			auth = snapshot.auth
			if !usableDPoPKey(auth.DPopPrivateKey) {
				return errors.New("codearts: generated DPoP key was not applied")
			}
		} else {
			snapshot.auth = auth
		}
	}

	refreshToken := strings.TrimSpace(auth.RefreshToken)
	if refreshToken == "" {
		m.noteTerminal(a, ErrRefreshTokenMissing.Error())
		return ErrRefreshTokenMissing
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
	wrote, err := m.writeAuth(a, snapshot, updated)
	if err != nil {
		return err
	}
	if !wrote {
		// A newer credential document won the race; do not overwrite it.
		return nil
	}
	m.noteSuccess(a)
	return nil
}

func (m *TokenManager) handleRefreshFailure(a *Account, snapshot tokenAccountSnapshot, auth Auth, err error) error {
	if !m.stillCurrent(a, snapshot) {
		return nil
	}

	if isTerminalRefreshError(err) {
		message := fmt.Sprintf("refresh_token 已失效或被拒，需要重新登录：%s", err.Error())
		m.noteTerminal(a, message)
		return err
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.removed || a.uidLocked() != snapshot.uid {
		return false
	}
	return sameRefreshCredentials(a.Auth, snapshot.auth)
}

func sameRefreshCredentials(current, previous Auth) bool {
	return strings.TrimSpace(current.RefreshToken) == strings.TrimSpace(previous.RefreshToken) &&
		bytes.Equal(current.DPopPrivateKey, previous.DPopPrivateKey) &&
		current.CloudDragonToken == previous.CloudDragonToken
}

func usableDPoPKey(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	_, err := parseDPoPPrivateKey(raw)
	return err == nil
}

// writeAuth verifies path ownership, persists candidate atomically, and only
// then publishes it on the account. It returns false without writing when a
// newer credential document replaced the snapshot.
func (m *TokenManager) writeAuth(a *Account, snapshot tokenAccountSnapshot, candidate Auth) (bool, error) {
	if a == nil {
		return false, ErrAccountNotFound
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
		return false, ErrAccountNotFound
	}
	uid := a.uidLocked()
	if strings.TrimSpace(uid) == "" {
		return false, errors.New("codearts: token refresh requires a user_id")
	}
	if uid != snapshot.uid || !sameRefreshCredentials(a.Auth, snapshot.auth) {
		return false, nil
	}

	path := strings.TrimSpace(a.Path)
	if path == "" {
		path = strings.TrimSpace(snapshot.path)
	}
	if path == "" {
		return false, errors.New("codearts: token refresh requires an auth path")
	}
	if err := verifyAuthOwnership(path, uid); err != nil {
		return false, err
	}

	// Preserve a proxy binding that changed while the request was in flight.
	candidate.ProxySlot = a.Auth.ProxySlot
	if err := SaveAuth(path, candidate); err != nil {
		return false, fmt.Errorf("codearts: save refreshed auth: %w", err)
	}
	a.Auth = candidate
	a.Path = path
	return true, nil
}

// authFromRefreshToken merges a successful token response exactly like v1
// update_auth: identity fields only when returned, all STS credentials, the
// rotated refresh token, and updated_at. PKCE/client identity, the DPoP key,
// proxy slot, and unknown fields are preserved.
func authFromRefreshToken(old Auth, token Token, now time.Time) Auth {
	updated := old
	if value := strings.TrimSpace(token.UserID); value != "" {
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

func (m *TokenManager) noteTerminal(a *Account, message string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	secrets := codeartsSecretValues(a.Auth)
	a.mu.Unlock()
	message = redactText(message, secrets)

	if pool := a.pool; pool != nil {
		_ = pool.NoteError(a, message, false, true)
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.State.LastError = truncateRunes(message, maxStateNoteRunes)
	if !a.State.ManualDisabled {
		a.State.Disabled = true
		a.State.Reason = a.State.LastError
	}
}

// noteTokenSuccess mirrors v1 update_auth's state cleanup: transient failures
// are cleared, but a manual disable and its reason remain untouched.
func (m *TokenManager) noteSuccess(a *Account) {
	if a == nil {
		return
	}
	if pool := a.pool; pool != nil {
		_ = pool.noteTokenSuccess(a)
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.State.ManualDisabled {
		return
	}
	a.State.ErrCount = 0
	a.State.CoolUntil = 0
	a.State.LastError = ""
	a.State.Disabled = false
	a.State.Reason = ""
}

// noteTokenSuccess clears non-manual failure state after a successful token
// rotation and persists one state snapshot.
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
		a.State.ErrCount = 0
		a.State.CoolUntil = 0
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
			continue
		}
		refreshed++
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
	}
}
