package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

// refreshWindow is the v1 proactive-refresh window: refresh when the access
// token expires within two minutes.
const refreshWindow = 120 * time.Second

// TokenManagerOptions configures a TokenManager. BaseURL overrides the Raccoon
// API host for tests or private deployments; Timeout defaults to
// DefaultHTTPTimeout; Now defaults to time.Now.
type TokenManagerOptions struct {
	BaseURL  string
	Timeout  time.Duration
	Now      func() time.Time
	ProxyURL func(*Account) string
}

// TokenManager refreshes Raccoon access tokens with per-account coalescing.
// Only one refresh request is in flight per account, and the credentials are
// persisted through SaveAuth before they are published in memory.
type TokenManager struct {
	http     *HTTPClient
	now      func() time.Time
	timeout  time.Duration
	proxyURL func(*Account) string

	mu     sync.Mutex
	states map[accountRefreshKey]*accountRefreshState

	// OnStateChange is an optional hook invoked after a successful refresh
	// clears a non-manual disable, or after an auth failure marks the account
	// terminal. It is called without TokenManager locks held, so a pool can
	// persist state. Set it before serving requests.
	OnStateChange func(*Account)
}

type accountRefreshKey struct {
	account *Account
	epoch   uint64
}

type accountRefreshState struct {
	mu       sync.Mutex
	epoch    uint64
	inflight *refreshCall
}

type refreshCall struct {
	done chan struct{}
	err  error
}

// NewTokenManager returns a token manager using the shared core transport.
func NewTokenManager(client *transport.Client, opts TokenManagerOptions) *TokenManager {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultHTTPTimeout
	}
	return &TokenManager{
		http: NewHTTPClient(client, HTTPClientOptions{
			BaseURL: opts.BaseURL,
			Timeout: timeout,
		}),
		now:      now,
		timeout:  timeout,
		proxyURL: opts.ProxyURL,
		states:   make(map[accountRefreshKey]*accountRefreshState),
	}
}

// Ensure refreshes the account when its access token expires within the v1
// two-minute window. Concurrent calls for one account share a single refresh.
// A transient failure is ignored while the current token is still valid,
// matching v1; an expired token reports the failure to the caller.
func (m *TokenManager) Ensure(ctx context.Context, a *Account) error {
	if err := m.validateCall(ctx, a); err != nil {
		return err
	}

	now := m.now()
	for {
		state := m.stateFor(a)
		state.mu.Lock()
		if state.epoch != a.flightEpochValue() {
			state.mu.Unlock()
			continue
		}
		if !a.refreshDue(now) {
			state.mu.Unlock()
			return nil
		}
		if state.inflight != nil {
			call := state.inflight
			state.mu.Unlock()

			err := waitRefresh(ctx, call)
			if err == nil {
				return nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return m.ensureFailure(a, state, err)
		}

		snapshot := a.refreshCredentials()
		if snapshot.Epoch != state.epoch {
			state.mu.Unlock()
			continue
		}
		if snapshot.Deleted {
			state.mu.Unlock()
			return ErrAccountDeleted
		}
		call := &refreshCall{done: make(chan struct{})}
		state.inflight = call
		state.mu.Unlock()

		err := m.performRefresh(ctx, a, state, call, snapshot)
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return m.ensureFailure(a, state, err)
	}
}

// ensureFailure applies v1's swallow rule only to transient upstream failures:
// a still-valid token keeps working, while an expired token reports the error.
// Local failures (for example a failed SaveAuth) and terminal auth failures are
// always returned.
func (m *TokenManager) ensureFailure(a *Account, state *accountRefreshState, err error) error {
	if IsAuthError(err) {
		return err
	}
	state.mu.Lock()
	expired := a.tokenIsExpired(m.now())
	state.mu.Unlock()
	if IsTransientError(err) && !expired {
		return nil
	}
	return err
}

// Refresh forces a refresh regardless of expiry. Concurrent forced refreshes
// for the same account coalesce: the first request rotates the credentials and
// the waiters return without consuming the one-time refresh token again.
func (m *TokenManager) Refresh(ctx context.Context, a *Account) error {
	return m.refresh(ctx, a, nil)
}

// RefreshIfGeneration forces a refresh only while the account is still at the
// caller's observed credential generation. A 401 handler should capture
// Generation before sending its upstream request and pass it here so a
// concurrent refresh that already rotated the token is not repeated.
func (m *TokenManager) RefreshIfGeneration(ctx context.Context, a *Account, generation uint64) error {
	return m.refresh(ctx, a, &generation)
}

// Generation returns the credential generation for an account. It increases
// once per successful refresh and on every credential rewrite (re-import or
// removal). Proxy-slot bindings deliberately do not advance it.
func (m *TokenManager) Generation(a *Account) uint64 {
	if m == nil || a == nil {
		return 0
	}
	return a.credentialGeneration()
}

func (m *TokenManager) refresh(ctx context.Context, a *Account, expected *uint64) error {
	if err := m.validateCall(ctx, a); err != nil {
		return err
	}

	for {
		state := m.stateFor(a)
		state.mu.Lock()
		if state.epoch != a.flightEpochValue() {
			state.mu.Unlock()
			continue
		}
		if expected != nil && a.credentialGeneration() != *expected {
			state.mu.Unlock()
			return nil
		}
		if state.inflight != nil {
			call := state.inflight
			state.mu.Unlock()
			return waitRefresh(ctx, call)
		}

		snapshot := a.refreshCredentials()
		if snapshot.Epoch != state.epoch {
			state.mu.Unlock()
			continue
		}
		if snapshot.Deleted {
			state.mu.Unlock()
			return ErrAccountDeleted
		}
		if expected != nil && snapshot.Generation != *expected {
			state.mu.Unlock()
			return nil
		}
		call := &refreshCall{done: make(chan struct{})}
		state.inflight = call
		state.mu.Unlock()

		return m.performRefresh(ctx, a, state, call, snapshot)
	}
}

func (m *TokenManager) validateCall(ctx context.Context, a *Account) error {
	if m == nil {
		return errors.New("raccoon: token manager is nil")
	}
	if ctx == nil {
		return errors.New("raccoon: nil context")
	}
	if a == nil {
		return errors.New("raccoon: account is nil")
	}
	if m.http == nil {
		return errors.New("raccoon: http client is nil")
	}
	return nil
}

func (m *TokenManager) stateFor(a *Account) *accountRefreshState {
	epoch := a.flightEpochValue()
	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-read under the manager lock: a stale pre-lock capture must never
	// evict a newer epoch's state or create a competing same-epoch flight.
	if current := a.flightEpochValue(); current != epoch {
		epoch = current
	}
	return m.stateForEpochLocked(a, epoch)
}

// stateForEpoch returns flight state for an explicit epoch. Eviction is
// monotonic: only older epochs for this account are forgotten, never a newer
// epoch that may already have an in-flight refresh.
func (m *TokenManager) stateForEpoch(a *Account, epoch uint64) *accountRefreshState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateForEpochLocked(a, epoch)
}

func (m *TokenManager) stateForEpochLocked(a *Account, epoch uint64) *accountRefreshState {
	key := accountRefreshKey{account: a, epoch: epoch}
	if m.states == nil {
		m.states = make(map[accountRefreshKey]*accountRefreshState)
	}
	for existing := range m.states {
		if existing.account == a && existing.epoch < epoch {
			delete(m.states, existing)
		}
	}
	state := m.states[key]
	if state == nil {
		state = &accountRefreshState{epoch: epoch}
		m.states[key] = state
	}
	return state
}

// ForgetAccount drops all flight bookkeeping for a removed account. A flight
// already in progress keeps its state pointer and completes; future lookups
// cannot join it.
func (m *TokenManager) ForgetAccount(a *Account) {
	if m == nil || a == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.states {
		if key.account == a {
			delete(m.states, key)
		}
	}
}

// ForgetAccountBefore drops older-epoch bookkeeping after a re-import while
// preserving any state a fresh request has already created for the new epoch.
func (m *TokenManager) ForgetAccountBefore(a *Account, epoch uint64) {
	if m == nil || a == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.states {
		if key.account == a && key.epoch < epoch {
			delete(m.states, key)
		}
	}
}

func waitRefresh(ctx context.Context, call *refreshCall) error {
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *TokenManager) performRefresh(
	ctx context.Context,
	a *Account,
	state *accountRefreshState,
	call *refreshCall,
	captured refreshCredentialSnapshot,
) error {
	// The refresh is shared by every waiter, so it must not inherit any one
	// caller's cancellation. WithoutCancel preserves values, while the HTTP
	// client bounds the request with the configured timeout.
	requestCtx := context.WithoutCancel(ctx)
	if m.timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(requestCtx, m.timeout)
		defer cancel()
	}

	var updated Auth
	var err error
	if strings.TrimSpace(captured.Path) == "" {
		err = errors.New("raccoon: token refresh requires an auth path")
	} else {
		updated, err = m.requestRefresh(requestCtx, a, captured.Auth)
	}

	var (
		stateChanged bool
		oldUID       string
		newUID       string
	)
	state.mu.Lock()
	a.credentialMu.Lock()
	if captured.Deleted || a.Deleted || a.authGeneration != captured.Generation {
		if a.Deleted {
			err = ErrAccountDeleted
		} else {
			err = ErrCredentialsChanged
		}
		call.err = err
	} else if err == nil {
		// A proxy-only change is serialized by credentialMu but must not
		// invalidate the rotated credentials. Merge the latest binding so the
		// new tokens and the new proxy slot both survive.
		updated.ProxySlot = a.latestProxySlot()
		if saveErr := SaveAuth(captured.Path, updated); saveErr != nil {
			call.err = fmt.Errorf("raccoon: persist refreshed auth: %w", saveErr)
			err = call.err
		} else {
			oldUID, newUID, stateChanged = a.publishRefreshedAuthLocked(updated)
			a.authGeneration++
			call.err = nil
		}
	} else {
		// A manual disable is a panel decision. A failed refresh must not
		// replace its reason or otherwise alter that state.
		if IsAuthError(err) {
			stateChanged = a.markAuthFailureLocked(err.Error())
		}
		call.err = err
	}
	a.credentialMu.Unlock()
	state.inflight = nil
	close(call.done)
	state.mu.Unlock()

	if oldUID != "" && newUID != "" && oldUID != newUID && a.pool != nil {
		a.pool.renameAffinity(oldUID, newUID)
	}
	if stateChanged && m.OnStateChange != nil {
		m.OnStateChange(a)
	}
	if call.err != nil {
		return call.err
	}
	return err
}

func (m *TokenManager) requestRefresh(ctx context.Context, a *Account, oldAuth Auth) (Auth, error) {
	refreshToken := cleanToken(oldAuth.RefreshToken)
	if refreshToken == "" {
		return Auth{}, &APIError{
			Kind:    KindAuth,
			Message: "refresh_token is missing; import an auth.json with a refresh token",
		}
	}
	accessToken := cleanToken(oldAuth.AccessToken)

	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return Auth{}, errors.New("raccoon: encode refresh request failed")
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	if accessToken != "" {
		headers.Set("Authorization", "Bearer "+accessToken)
	}

	proxyURL := ""
	if m.proxyURL != nil {
		proxyURL = strings.TrimSpace(m.proxyURL(a))
	}

	payload, err := m.http.DoJSON(
		ctx, http.MethodPost, RefreshPath, headers, body, proxyURL,
		accessToken, refreshToken,
	)
	if err != nil {
		// v1 maps a refresh endpoint 400 (invalid grant) to an auth failure:
		// re-import is required, so the account is terminal.
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Kind == KindBadRequest {
			return Auth{}, &APIError{
				Kind:    KindAuth,
				Status:  apiErr.Status,
				Message: apiErr.Message,
			}
		}
		return Auth{}, err
	}

	data := payload
	if nested, ok := payload["data"].(map[string]any); ok && nested != nil {
		data = nested
	}
	newAccess := cleanToken(responseString(data, "access_token"))
	if newAccess == "" {
		// v1 treats an invalid refresh success body as an auth failure: only
		// re-importing the account can recover.
		return Auth{}, &APIError{
			Kind:    KindAuth,
			Message: "refresh response did not include access_token",
		}
	}

	updated := oldAuth
	updated.AccessToken = newAccess
	if newRefresh := cleanToken(responseString(data, "refresh_token")); newRefresh != "" {
		updated.RefreshToken = newRefresh
	}
	for key, destination := range map[string]*string{
		"token_type":      &updated.TokenType,
		"office_identity": &updated.OfficeIdentity,
		"office_org_name": &updated.OfficeOrgName,
		"office_org_role": &updated.OfficeOrgRole,
	} {
		if value := strings.TrimSpace(responseString(data, key)); value != "" {
			*destination = value
		}
	}

	expiry := responseExpiry(data["expires_at"])
	if expiry.IsZero() {
		// v1 drops a missing expires_at and falls back to the JWT exp claim.
		expiry = expiresAtFromAccessToken(newAccess)
	}
	updated.ExpiresAt = expiry

	uid := UIDFromAccessToken(newAccess)
	if uid == "" {
		uid = strings.TrimSpace(oldAuth.UID)
	}
	if uid == "" {
		uid = updated.uidFromCredentials()
	}
	updated.UID = uid
	updated.UpdatedAt = m.now().Unix()
	return updated, nil
}

func responseString(payload map[string]any, key string) string {
	if value, ok := payload[key].(string); ok {
		return value
	}
	return ""
}

func responseExpiry(value any) time.Time {
	if value == nil {
		return time.Time{}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return time.Time{}
	}
	return decodeExpiresAt(raw)
}

// tokenExpiringSoon applies v1's fixed two-minute window. An unknown expiry
// (zero) is not proactively refreshed because there is nothing to compare.
func tokenExpiringSoon(auth Auth, now time.Time) bool {
	expiry := effectiveExpiry(auth)
	if expiry.IsZero() {
		return false
	}
	return !expiry.After(now.Add(refreshWindow))
}

func tokenExpired(auth Auth, now time.Time) bool {
	expiry := effectiveExpiry(auth)
	return !expiry.IsZero() && !expiry.After(now)
}

// effectiveExpiry mirrors v1: the JWT exp claim wins over the stored field.
// LoadAuth already applies this preference, but in-memory accounts created by
// import or tests may carry only the token.
func effectiveExpiry(auth Auth) time.Time {
	if expiry := expiresAtFromAccessToken(auth.AccessToken); !expiry.IsZero() {
		return expiry
	}
	return auth.ExpiresAt
}
