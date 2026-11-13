package codearts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

// CodeArts OAuth constants. Hosts and the client/plugin identity have
// WB_CODEARTS_* environment overrides for tests and private deployments; the
// defaults are v1.6.5's production values.
const (
	oauthDefaultClientID    = "codearts-agent"
	oauthDefaultPluginName  = "snap_AIIDE"
	oauthDefaultPluginVer   = "5.2.0"
	oauthRedirectPath       = "/oauth/callback"
	oauthTokenEndpoint      = "/v1/oauth2/tokens"
	oauthTicketEndpoint     = "/v1/login/ticket"
	oauthDefaultSTS         = "https://sts.cn-north-4.myhuaweicloud.com"
	oauthDefaultPortal      = "https://codearts.huaweicloud.com/portal"
	oauthDefaultSnapHost    = "https://snap-access.cn-north-4.myhuaweicloud.com"
	oauthTTL                = 600 * time.Second
	oauthMaxSessions        = 64
	oauthHTTPTimeout        = 30 * time.Second
	oauthBusyGrace          = 2 * oauthHTTPTimeout
	oauthMaxResponseBytes   = 1 << 20
	oauthMessageLimit       = 500
	oauthTokenRejectBodyMax = 300
	oauthIdentityTimeout    = 10 * time.Second
)

// oauthIdentityHTTP is the direct identity-enrichment client. It deliberately
// bypasses account proxy slots (matching v1's credential holder) and refuses
// redirects so the STS signature/security token is never replayed elsewhere.
var oauthIdentityHTTP = transport.NewClient(transport.ClientOptions{
	Timeout:         oauthIdentityTimeout,
	RefuseRedirects: true,
})

// OAuth flow errors. They are intentionally free of callback codes, state,
// verifier, ticket, and token values so they are safe to store and return.
var (
	ErrOAuthSession     = errors.New("codearts: OAuth session not found or expired")
	ErrOAuthState       = errors.New("codearts: OAuth state does not match the session")
	ErrOAuthReplay      = errors.New("codearts: OAuth session is no longer pending")
	ErrOAuthRedirect    = errors.New("codearts: OAuth redirect is not allowed")
	ErrOAuthCallback    = errors.New("codearts: OAuth callback is missing code or secret")
	ErrOAuthCallbackURL = errors.New("codearts: invalid loopback OAuth callback URL")
	ErrOAuthCode        = errors.New("codearts: authorization code is required")
	ErrOAuthPKCE        = errors.New("codearts: PKCE verifier is required")
	ErrTokenRejected    = errors.New("codearts: token request rejected")
	ErrTokenResponse    = errors.New("codearts: invalid token response")
	ErrTokenTransport   = errors.New("codearts: token transport failed")
)

// TokenEndpointError preserves the HTTP status and safe upstream detail for
// callers such as the refresh manager. Detail never contains request secrets.
type TokenEndpointError struct {
	Status int
	Detail string
}

func (e *TokenEndpointError) Error() string {
	if e == nil {
		return ErrTokenRejected.Error()
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s: HTTP %d: %s", ErrTokenRejected.Error(), e.Status, e.Detail)
	}
	return fmt.Sprintf("%s: HTTP %d", ErrTokenRejected.Error(), e.Status)
}

func (e *TokenEndpointError) Unwrap() error { return ErrTokenRejected }

var oauthHTTPClient = &http.Client{
	Timeout: oauthHTTPTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		// Never forward a DPoP proof, ticket secret, or callback value to a
		// redirect target; a redirect is an endpoint error.
		return http.ErrUseLastResponse
	},
}

// tokenEndpointDoer is the request sink for token endpoint calls. The login
// flow uses the package oauth client, while the refresh manager injects the
// shared core transport so an account proxy and configured timeout apply.
type tokenEndpointDoer interface {
	Do(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error)
}

type oauthEndpointDoer struct{}

func (oauthEndpointDoer) Do(_ context.Context, req *http.Request, _ string) (*http.Response, error) {
	return oauthHTTPClient.Do(req)
}

func envString(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func stsHost() string {
	return strings.TrimRight(envString("WB_CODEARTS_STS_HOST", oauthDefaultSTS), "/")
}

func portalHost() string {
	return strings.TrimRight(envString("WB_CODEARTS_PORTAL_HOST", oauthDefaultPortal), "/")
}

func snapHost() string {
	return strings.TrimRight(envString("WB_CODEARTS_SNAP_HOST", oauthDefaultSnapHost), "/")
}

func snapManagerBase() string {
	if value := strings.TrimSpace(os.Getenv("WB_CODEARTS_SNAP_MANAGER")); value != "" {
		return strings.TrimRight(value, "/")
	}
	return snapHost() + "/snap-manager"
}

func oauthClientID() string {
	return envString("WB_CODEARTS_CLIENT_ID", oauthDefaultClientID)
}

func oauthPluginName() string {
	return envString("WB_CODEARTS_PLUGIN_NAME", oauthDefaultPluginName)
}

func oauthPluginVersion() string {
	return envString("WB_CODEARTS_PLUGIN_VERSION", oauthDefaultPluginVer)
}

// Credentials is the STS temporary AK/SK returned by the token endpoint.
type Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	Expiration      string `json:"expiration"`
}

// Token is the normalized oauth2/tokens response. Legacy "credential" fields
// are converted into Credentials before this value is returned.
type Token struct {
	UserID       string      `json:"user_id"`
	UserName     string      `json:"user_name"`
	DomainID     string      `json:"domain_id"`
	RefreshToken string      `json:"refresh_token"`
	Credentials  Credentials `json:"credentials"`
}

// OAuthStart is the panel-facing start response. It never contains the code
// verifier, ticket secret, or DPoP private key.
type OAuthStart struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id"`
	AuthURL   string `json:"auth_url"`
	ExpiresIn int    `json:"expires_in"`
	Message   string `json:"message"`
}

// OAuthAccount is the identity published by a completed OAuth session.
type OAuthAccount struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
}

// OAuthPoll is the panel-facing session state. Callback codes, state, ticket
// secrets, tokens, and JWKs are never copied into it.
type OAuthPoll struct {
	OK        bool          `json:"ok"`
	Status    string        `json:"status"`
	Message   string        `json:"message"`
	SessionID string        `json:"session_id"`
	AuthURL   string        `json:"auth_url"`
	NextURL   string        `json:"next_url,omitempty"`
	Account   *OAuthAccount `json:"account,omitempty"`
}

// OAuthResult is returned by a callback that completed (or armed the ticket
// fallback for) one session. Auth is excluded from JSON so routes can hand it
// to the pool without exposing credentials.
type OAuthResult struct {
	OK        bool   `json:"ok"`
	Status    string `json:"status,omitempty"`
	Message   string `json:"message,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	AuthURL   string `json:"auth_url,omitempty"`
	NextURL   string `json:"next_url,omitempty"`
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	Auth      *Auth  `json:"-"`
}

type oauthSession struct {
	ID             string
	TicketID       string
	Secret         string
	State          string
	Verifier       string
	DPoPKey        json.RawMessage
	Port           int
	CreatedAt      time.Time
	Status         string
	Message        string
	AuthURL        string
	TicketFallback bool
	TicketPolling  bool
	SecretSet      bool
	PendingTicket  *Token
	Account        *OAuthAccount
	SavedAuth      *Auth
}

// OAuthSessions owns browser authorization attempts, keyed by settings-file
// path. A Provider may keep its own instance; the package-level BeginOAuth,
// PollOAuth, and HandleOAuthCallback use DefaultOAuthSessions.
type OAuthSessions struct {
	mu      sync.Mutex
	byStore map[string]map[string]*oauthSession
	now     func() time.Time
	client  *http.Client
}

// NewOAuthSessions returns an empty session table.
func NewOAuthSessions() *OAuthSessions {
	return &OAuthSessions{
		byStore: make(map[string]map[string]*oauthSession),
		now:     time.Now,
		client:  oauthHTTPClient,
	}
}

// DefaultOAuthSessions backs the package-level OAuth functions.
var DefaultOAuthSessions = NewOAuthSessions()

func (s *OAuthSessions) nowTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *OAuthSessions) httpClient() *http.Client {
	if s != nil && s.client != nil {
		return s.client
	}
	return oauthHTTPClient
}

func oauthStoreKey(store *config.Store) (string, error) {
	if store == nil || strings.TrimSpace(store.Path) == "" {
		return "", errors.New("codearts: OAuth requires a config store")
	}
	absolute, err := filepath.Abs(store.Path)
	if err != nil {
		absolute = filepath.Clean(store.Path)
	}
	return absolute, nil
}

func (s *OAuthSessions) sessionsFor(storeKey string) map[string]*oauthSession {
	if s.byStore == nil {
		s.byStore = make(map[string]map[string]*oauthSession)
	}
	sessions := s.byStore[storeKey]
	if sessions == nil {
		sessions = make(map[string]*oauthSession)
		s.byStore[storeKey] = sessions
	}
	return sessions
}

func (s *OAuthSessions) gcLocked(sessions map[string]*oauthSession, now time.Time) {
	for id, session := range sessions {
		age := now.Sub(session.CreatedAt)
		if age <= oauthTTL {
			continue
		}
		// Do not delete an exchange or ticket request out from under its
		// network call (or a retained ticket result awaiting persistence) at
		// the exact TTL boundary. The grace is longer than the HTTP timeout;
		// a stuck busy session is still collected after it.
		if oauthSessionBusy(session) && age <= oauthTTL+oauthBusyGrace {
			continue
		}
		delete(sessions, id)
	}
}

// oauthSessionBusy reports state that owns network work or a ticket result.
// Both cap eviction and TTL collection avoid destroying such sessions.
func oauthSessionBusy(session *oauthSession) bool {
	return session != nil &&
		(session.Status == "exchanging" || session.TicketPolling || session.PendingTicket != nil)
}

func evictOldestLocked(sessions map[string]*oauthSession) *oauthSession {
	var oldest *oauthSession
	for _, session := range sessions {
		// A session with an exchange or ticket request in flight, or a retained
		// ticket result, is busy: the cap must evict a settled session instead
		// of orphaning that work.
		if oauthSessionBusy(session) {
			continue
		}
		if oldest == nil || session.CreatedAt.Before(oldest.CreatedAt) {
			oldest = session
		}
	}
	return oldest
}

func constantTimeEqual(left, right string) bool {
	if left == "" || right == "" || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func oauthStateMatches(session *oauthSession, state string) bool {
	if session == nil {
		return false
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return false
	}
	candidates := []string{state}
	if index := strings.Index(state, "."); index > 0 {
		candidates = append(candidates, state[:index])
	}
	for _, candidate := range candidates {
		if constantTimeEqual(candidate, session.State) ||
			constantTimeEqual(candidate, session.TicketID) {
			return true
		}
	}
	return false
}

func oauthAuthorizeURL(port int, challenge, ticketID string) string {
	clientID := oauthClientID()
	pairs := []struct {
		key   string
		value string
	}{
		{"theme", "2"},
		{"locale", "zh-cn"},
		{"uri_scheme", clientID},
		{"client_id", clientID},
		{"port", strconv.Itoa(port)},
		{"code_challenge", challenge},
		{"code_challenge_method", "SHA-256"},
		{"ticket_id", ticketID},
		{"plugin-name", oauthPluginName()},
		{"plugin-version", oauthPluginVersion()},
	}
	var builder strings.Builder
	builder.WriteString(portalHost())
	builder.WriteString("/authorize?")
	for i, pair := range pairs {
		if i > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(url.QueryEscape(pair.key))
		builder.WriteByte('=')
		builder.WriteString(url.QueryEscape(pair.value))
	}
	return builder.String()
}

// Begin creates one pending OAuth session and returns the official authorize
// URL. Sessions expire after 600 seconds; at most 64 coexist per store.
func (s *OAuthSessions) Begin(store *config.Store, port int) (OAuthStart, error) {
	storeKey, err := oauthStoreKey(store)
	if err != nil {
		return OAuthStart{}, err
	}
	if port < 1 || port > 65535 {
		return OAuthStart{}, errors.New("codearts: OAuth port is out of range")
	}

	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return OAuthStart{}, err
	}
	privateJWK, err := NewDPoPKey()
	if err != nil {
		return OAuthStart{}, err
	}
	sessionID, err := randomHex(8)
	if err != nil {
		return OAuthStart{}, err
	}
	ticketID, err := randomHex(16)
	if err != nil {
		return OAuthStart{}, err
	}
	secret, err := randomHex(16)
	if err != nil {
		return OAuthStart{}, err
	}

	now := s.nowTime()
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := s.sessionsFor(storeKey)
	s.gcLocked(sessions, now)
	for len(sessions) >= oauthMaxSessions {
		oldest := evictOldestLocked(sessions)
		if oldest == nil {
			return OAuthStart{}, errors.New("codearts: too many pending OAuth sessions")
		}
		delete(sessions, oldest.ID)
	}
	for {
		collision := sessions[sessionID] != nil
		for _, existing := range sessions {
			if constantTimeEqual(existing.TicketID, ticketID) ||
				constantTimeEqual(existing.Secret, secret) {
				collision = true
				break
			}
		}
		if !collision {
			break
		}
		if sessionID, err = randomHex(8); err != nil {
			return OAuthStart{}, err
		}
		if ticketID, err = randomHex(16); err != nil {
			return OAuthStart{}, err
		}
		if secret, err = randomHex(16); err != nil {
			return OAuthStart{}, err
		}
	}

	authURL := oauthAuthorizeURL(port, challenge, ticketID)
	session := &oauthSession{
		ID:        sessionID,
		TicketID:  ticketID,
		Secret:    secret,
		State:     ticketID,
		Verifier:  verifier,
		DPoPKey:   append(json.RawMessage(nil), privateJWK...),
		Port:      port,
		CreatedAt: now,
		Status:    "pending",
		Message:   "等待浏览器完成授权…",
		AuthURL:   authURL,
	}
	sessions[sessionID] = session
	return OAuthStart{
		OK:        true,
		SessionID: sessionID,
		AuthURL:   authURL,
		ExpiresIn: int(oauthTTL / time.Second),
		Message:   "请在浏览器打开授权链接，完成后等待自动检测",
	}, nil
}

func (s *OAuthSessions) publicPollLocked(session *oauthSession) OAuthPoll {
	status := session.Status
	if status == "exchanging" {
		status = "pending"
	}
	poll := OAuthPoll{
		OK:        status != "error",
		Status:    status,
		Message:   session.Message,
		SessionID: session.ID,
		AuthURL:   session.AuthURL,
	}
	if session.Account != nil {
		account := *session.Account
		poll.Account = &account
	}
	return poll
}

func (s *OAuthSessions) findLocked(storeKey, sessionID string) *oauthSession {
	if sessionID == "" {
		return nil
	}
	return s.sessionsFor(storeKey)[sessionID]
}

func (s *OAuthSessions) findByStateLocked(storeKey, state string) *oauthSession {
	if strings.TrimSpace(state) == "" {
		return nil
	}
	for _, session := range s.sessionsFor(storeKey) {
		if oauthStateMatches(session, state) {
			return session
		}
	}
	return nil
}

func (s *OAuthSessions) findBySecretLocked(storeKey, secret string) *oauthSession {
	if strings.TrimSpace(secret) == "" {
		return nil
	}
	for _, session := range s.sessionsFor(storeKey) {
		if session.Status == "pending" && constantTimeEqual(session.Secret, secret) {
			return session
		}
	}
	return nil
}

func (s *OAuthSessions) failSession(storeKey, sessionID, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.findLocked(storeKey, sessionID)
	if session == nil {
		return
	}
	session.Status = "error"
	session.Message = truncateOAuthMessage(message, oauthMessageLimit)
	session.TicketPolling = false
	session.PendingTicket = nil
	session.Account = nil
}

// codeExchangeFailed records a failed authorization-code exchange. When the
// session also has a ticket fallback (armed or in flight), the session returns
// to pending so the valid ticket result can still be used; the already-issued
// error is still returned to the caller.
func (s *OAuthSessions) codeExchangeFailed(storeKey, sessionID, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.findLocked(storeKey, sessionID)
	if session == nil || session.Status != "exchanging" {
		return
	}
	if session.TicketFallback || session.PendingTicket != nil {
		session.Status = "pending"
		session.Message = "授权码兑换失败，继续等待票据登录…"
		session.Account = nil
		return
	}
	session.Status = "error"
	session.Message = truncateOAuthMessage(message, oauthMessageLimit)
	session.TicketPolling = false
	session.PendingTicket = nil
	session.Account = nil
}

func (s *OAuthSessions) completeSession(storeKey, sessionID string, auth Auth) *OAuthAccount {
	nickname := auth.UserName
	if strings.TrimSpace(nickname) == "" {
		nickname = auth.UserID
	}
	account := &OAuthAccount{UID: auth.UserID, Nickname: nickname}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.findLocked(storeKey, sessionID)
	if session == nil {
		return account
	}
	session.Status = "done"
	session.Message = "登录成功：" + nickname
	session.TicketPolling = false
	session.PendingTicket = nil
	session.Account = account
	saved := auth
	session.SavedAuth = &saved
	return account
}

// Poll returns one session's public state. A pending fallback session runs at
// most one ticket GET at a time; concurrent polls return pending while that
// request is in flight. A valid code callback can claim the session during the
// request and takes priority; a valid ticket result is retained until the code
// outcome is known. Done and error sessions are terminal.
func (s *OAuthSessions) Poll(store *config.Store, sessionID string) (OAuthPoll, error) {
	storeKey, err := oauthStoreKey(store)
	if err != nil {
		return OAuthPoll{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return OAuthPoll{}, ErrOAuthSession
	}

	now := s.nowTime()
	s.mu.Lock()
	sessions := s.sessionsFor(storeKey)
	s.gcLocked(sessions, now)
	session := sessions[sessionID]
	if session == nil {
		s.mu.Unlock()
		return OAuthPoll{}, ErrOAuthSession
	}
	if session.Status != "pending" ||
		(!session.TicketFallback && session.PendingTicket == nil) {
		poll := s.publicPollLocked(session)
		s.mu.Unlock()
		return poll, nil
	}

	// A code callback may have failed after a valid ticket result arrived while
	// it owned the session. Persist the retained result without another GET.
	if session.PendingTicket != nil {
		token := *session.PendingTicket
		session.PendingTicket = nil
		session.Status = "exchanging"
		session.Message = "正在保存登录结果…"
		session.TicketPolling = false
		snapshot := *session
		s.mu.Unlock()
		return s.finishTicketPoll(store, storeKey, sessionID, snapshot, token)
	}
	if !session.TicketFallback {
		poll := s.publicPollLocked(session)
		s.mu.Unlock()
		return poll, nil
	}
	if session.TicketPolling {
		// Another poll already has a ticket request in flight.
		poll := s.publicPollLocked(session)
		s.mu.Unlock()
		return poll, nil
	}
	// Do not change Status: a valid code callback must be able to claim the
	// pending session while the ticket request is blocked in the network.
	session.TicketPolling = true
	snapshot := *session
	snapshotSecret := snapshot.Secret
	s.mu.Unlock()

	token, ticketErr := s.pollTicket(snapshot.TicketID, snapshot.Secret)

	s.mu.Lock()
	current := s.findLocked(storeKey, sessionID)
	if current == nil {
		s.mu.Unlock()
		return OAuthPoll{}, ErrOAuthSession
	}
	if current.Status != "pending" {
		current.TicketPolling = false
		// A code callback owns the session. Retain a valid ticket result so a
		// later code-exchange failure can fall back to it.
		if current.Status == "exchanging" && current.TicketFallback &&
			ticketErr == nil && ticketTokenUsable(token) &&
			constantTimeEqual(current.Secret, snapshotSecret) {
			retained := token
			current.PendingTicket = &retained
			current.Message = "正在完成登录…"
		}
		poll := s.publicPollLocked(current)
		s.mu.Unlock()
		return poll, nil
	}
	if !current.TicketPolling {
		poll := s.publicPollLocked(current)
		s.mu.Unlock()
		return poll, nil
	}
	// A new callback secret arrived during the request; retry with the new
	// secret on the next poll instead of persisting a stale result.
	if !constantTimeEqual(current.Secret, snapshotSecret) {
		current.TicketPolling = false
		current.Message = "等待浏览器完成登录…"
		poll := s.publicPollLocked(current)
		s.mu.Unlock()
		return poll, nil
	}
	current.TicketPolling = false
	if ticketErr != nil {
		current.Status = "error"
		current.Message = truncateOAuthMessage(
			redactOAuthValues(ticketErr.Error(), snapshot.TicketID, snapshot.Secret),
			oauthMessageLimit,
		)
		current.Account = nil
		poll := s.publicPollLocked(current)
		s.mu.Unlock()
		return poll, nil
	}
	// v1's ticket fallback only persists a response that carries a user_id.
	// The code path keeps the token/JWT-sub/refresh-hash fallback instead.
	if !ticketTokenUsable(token) {
		current.Message = "等待浏览器完成登录…"
		poll := s.publicPollLocked(current)
		s.mu.Unlock()
		return poll, nil
	}

	// The ticket token is valid and the session is still pending: reserve it
	// for persistence so a racing code callback cannot consume the same slot.
	current.Status = "exchanging"
	current.Message = "正在保存登录结果…"
	saveSnapshot := *current
	s.mu.Unlock()
	return s.finishTicketPoll(store, storeKey, sessionID, saveSnapshot, token)
}

func ticketTokenUsable(token Token) bool {
	return strings.TrimSpace(token.Credentials.SecurityToken) != "" &&
		strings.TrimSpace(token.UserID) != ""
}

func (s *OAuthSessions) finishTicketPoll(
	store *config.Store, storeKey, sessionID string, snapshot oauthSession, token Token,
) (OAuthPoll, error) {
	auth, saveErr := s.saveOAuthAuth(store, &snapshot, token)
	if saveErr != nil {
		s.failSession(storeKey, sessionID, saveErr.Error())
		s.mu.Lock()
		current := s.findLocked(storeKey, sessionID)
		if current == nil {
			s.mu.Unlock()
			return OAuthPoll{}, ErrOAuthSession
		}
		poll := s.publicPollLocked(current)
		s.mu.Unlock()
		return poll, nil
	}
	s.completeSession(storeKey, sessionID, auth)

	s.mu.Lock()
	current := s.findLocked(storeKey, sessionID)
	if current == nil {
		s.mu.Unlock()
		return OAuthPoll{}, ErrOAuthSession
	}
	poll := s.publicPollLocked(current)
	s.mu.Unlock()
	return poll, nil
}

func (s *OAuthSessions) pollTicket(ticketID, secret string) (Token, error) {
	endpoint := snapManagerBase() + oauthTicketEndpoint
	query := url.Values{}
	query.Set("ticket_id", ticketID)
	query.Set("secret", secret)
	request, err := http.NewRequest(http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return Token{}, fmt.Errorf("%w: ticket request is invalid", ErrTokenTransport)
	}
	request.Header.Set("plugin-name", oauthPluginName())
	request.Header.Set("plugin-version", oauthPluginVersion())

	response, err := s.httpClient().Do(request)
	if err != nil {
		return Token{}, oauthTransportError("ticket request failed", err, ticketID, secret)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, oauthMaxResponseBytes))
	if err != nil {
		return Token{}, oauthTransportError("read ticket response", err, ticketID, secret)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Token{}, &TokenEndpointError{
			Status: response.StatusCode,
			Detail: tokenEndpointDetail(raw, ticketID, secret),
		}
	}
	return normalizeTokenPayload(raw)
}

// Callback redeems an authorization code. state may be omitted for the panel
// API's session-id form; otherwise it must match the session's state/ticket
// (including Huawei's "<ticket>.pkg" alias). A code-less callback arms the
// ticket fallback instead of spending the one-time claim.
func (s *OAuthSessions) Callback(store *config.Store, sessionID, code, state string) (OAuthResult, error) {
	storeKey, err := oauthStoreKey(store)
	if err != nil {
		return OAuthResult{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	code = strings.TrimSpace(code)
	state = strings.TrimSpace(state)

	now := s.nowTime()
	s.mu.Lock()
	sessions := s.sessionsFor(storeKey)
	s.gcLocked(sessions, now)
	var session *oauthSession
	if sessionID != "" {
		session = sessions[sessionID]
		if session == nil {
			s.mu.Unlock()
			return OAuthResult{}, ErrOAuthSession
		}
	} else {
		if state == "" {
			s.mu.Unlock()
			return OAuthResult{}, ErrOAuthState
		}
		session = s.findByStateLocked(storeKey, state)
		if session == nil {
			s.mu.Unlock()
			return OAuthResult{}, ErrOAuthSession
		}
		sessionID = session.ID
	}
	if state != "" && !oauthStateMatches(session, state) {
		s.mu.Unlock()
		return OAuthResult{}, ErrOAuthState
	}

	if code == "" {
		if session.Status != "pending" {
			s.mu.Unlock()
			return OAuthResult{}, ErrOAuthReplay
		}
		session.TicketFallback = true
		session.Message = "回调已接收，继续等待授权结果…"
		result := OAuthResult{
			OK:        true,
			Status:    "continue",
			Message:   session.Message,
			SessionID: session.ID,
			AuthURL:   session.AuthURL,
		}
		s.mu.Unlock()
		return result, nil
	}

	if session.Status != "pending" {
		s.mu.Unlock()
		return OAuthResult{}, ErrOAuthReplay
	}
	session.Status = "exchanging"
	session.Message = "正在兑换授权码…"
	// Keep TicketPolling as-is: an in-flight ticket request must be allowed to
	// finish and its result retained if this code exchange fails.
	snapshot := *session
	s.mu.Unlock()

	token, err := ExchangeCode(code, snapshot.Verifier, snapshot.Port, snapshot.DPoPKey)
	if err != nil {
		failMessage := redactOAuthValues(err.Error(), code, snapshot.Verifier,
			snapshot.Secret, snapshot.TicketID, string(snapshot.DPoPKey), dpopPrivateScalar(snapshot.DPoPKey))
		s.codeExchangeFailed(storeKey, sessionID, failMessage)
		return OAuthResult{}, redactOAuthError(err, code, snapshot.Verifier,
			snapshot.Secret, snapshot.TicketID, string(snapshot.DPoPKey), dpopPrivateScalar(snapshot.DPoPKey))
	}
	auth, err := s.saveOAuthAuth(store, &snapshot, token)
	if err != nil {
		s.failSession(storeKey, sessionID, err.Error())
		return OAuthResult{}, err
	}
	account := s.completeSession(storeKey, sessionID, auth)
	result := OAuthResult{
		OK:        true,
		Status:    "done",
		Message:   "登录成功：" + account.Nickname,
		SessionID: sessionID,
		AuthURL:   snapshot.AuthURL,
		UID:       auth.UserID,
		Nickname:  account.Nickname,
		Auth:      &auth,
	}
	return result, nil
}

func (s *OAuthSessions) saveOAuthAuth(store *config.Store, session *oauthSession, token Token) (Auth, error) {
	uid, name, domain := oauthIdentity(token)
	if strings.TrimSpace(uid) == "" {
		return Auth{}, errors.New("codearts: token response has no usable identity")
	}
	if strings.TrimSpace(name) == "" {
		name = uid
	}
	accountsDir := filepath.Dir(store.Path)
	path, err := AuthPath(accountsDir, uid)
	if err != nil {
		return Auth{}, err
	}
	if err := verifyAuthOwnership(path, uid); err != nil {
		return Auth{}, err
	}
	auth := Auth{
		UserID:           uid,
		UserName:         name,
		DomainID:         domain,
		CloudDragonToken: token.Credentials.SecurityToken,
		AccessKeyID:      token.Credentials.AccessKeyID,
		SecretAccessKey:  token.Credentials.SecretAccessKey,
		RefreshToken:     token.RefreshToken,
		CodeVerifier:     session.Verifier,
		ClientID:         oauthClientID(),
		Expiration:       parseExpirationText(token.Credentials.Expiration),
		DPopPrivateKey:   append(json.RawMessage(nil), session.DPoPKey...),
		UpdatedAt:        s.nowTime().Unix(),
	}
	if err := SaveAuth(path, auth); err != nil {
		return Auth{}, err
	}
	saved, err := LoadAuth(path)
	if err != nil {
		return Auth{}, err
	}
	return saved, nil
}

// Continue handles the portal's secret+redirect callback. The redirect must
// belong to the configured portal; embedded credentials are rejected. It arms
// the ticket fallback and returns a "continue" poll without spending the
// one-time code claim.
func (s *OAuthSessions) Continue(store *config.Store, sessionID, secret, redirect string) (OAuthPoll, error) {
	storeKey, err := oauthStoreKey(store)
	if err != nil {
		return OAuthPoll{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	secret = strings.TrimSpace(secret)
	redirect = strings.TrimSpace(redirect)
	if secret == "" {
		return OAuthPoll{}, ErrOAuthCallback
	}
	if redirect == "" {
		return OAuthPoll{}, ErrOAuthRedirect
	}

	now := s.nowTime()
	s.mu.Lock()
	sessions := s.sessionsFor(storeKey)
	s.gcLocked(sessions, now)
	var session *oauthSession
	if sessionID != "" {
		session = sessions[sessionID]
	} else {
		session = s.findBySecretLocked(storeKey, secret)
	}
	if session == nil {
		s.mu.Unlock()
		return OAuthPoll{}, ErrOAuthSession
	}
	if session.Status != "pending" {
		s.mu.Unlock()
		return OAuthPoll{}, ErrOAuthReplay
	}
	if session.SecretSet && !constantTimeEqual(session.Secret, secret) {
		s.mu.Unlock()
		return OAuthPoll{}, ErrOAuthState
	}
	s.mu.Unlock()

	if !safeOAuthRedirect(redirect) {
		return OAuthPoll{}, ErrOAuthRedirect
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	session = s.findLocked(storeKey, session.ID)
	if session == nil {
		return OAuthPoll{}, ErrOAuthSession
	}
	if session.Status != "pending" {
		return OAuthPoll{}, ErrOAuthReplay
	}
	if session.SecretSet && !constantTimeEqual(session.Secret, secret) {
		return OAuthPoll{}, ErrOAuthState
	}
	session.Secret = secret
	session.SecretSet = true
	session.TicketFallback = true
	session.Message = "回调已接收，继续等待授权结果…"
	poll := s.publicPollLocked(session)
	poll.Status = "continue"
	poll.NextURL = redirect
	return poll, nil
}

func safeOAuthRedirect(raw string) bool {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Hostname() == "" || target.User != nil {
		return false
	}
	portal, err := url.Parse(portalHost())
	if err != nil || portal.Hostname() == "" {
		return false
	}
	if !strings.EqualFold(target.Scheme, portal.Scheme) ||
		!strings.EqualFold(target.Hostname(), portal.Hostname()) {
		return false
	}
	if effectivePort(target) != effectivePort(portal) {
		return false
	}
	return true
}

type oauthCallbackURL struct {
	Code     string
	State    string
	TicketID string
	Secret   string
	Redirect string
	Port     int
}

// parseOAuthCallbackURL accepts only v1's loopback callback shape: http,
// no userinfo, /oauth/callback, and a loopback host. Errors never include the
// URL because it may carry a code, state, ticket, or secret.
func parseOAuthCallbackURL(raw string) (oauthCallbackURL, error) {
	var values oauthCallbackURL
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Scheme, "http") ||
		parsed.User != nil || parsed.Path != oauthRedirectPath {
		return values, ErrOAuthCallbackURL
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
	default:
		return values, ErrOAuthCallbackURL
	}
	if text := parsed.Port(); text != "" {
		port, err := strconv.Atoi(text)
		if err != nil || port < 1 || port > 65535 {
			return values, ErrOAuthCallbackURL
		}
		values.Port = port
	}
	query := parsed.Query()
	values.Code = strings.TrimSpace(query.Get("code"))
	values.State = strings.TrimSpace(query.Get("state"))
	values.TicketID = strings.TrimSpace(query.Get("ticket_id"))
	values.Secret = strings.TrimSpace(query.Get("secret"))
	values.Redirect = strings.TrimSpace(query.Get("redirect"))
	return values, nil
}

// CallbackURL validates and handles a raw loopback callback URL. It is the
// v1-compatible entry point for the panel's callback_url field and the browser
// GET route. A code is redeemed; secret+redirect arms the ticket fallback.
func (s *OAuthSessions) CallbackURL(store *config.Store, sessionID, rawURL string) (OAuthResult, error) {
	values, err := parseOAuthCallbackURL(rawURL)
	if err != nil {
		return OAuthResult{}, err
	}
	storeKey, err := oauthStoreKey(store)
	if err != nil {
		return OAuthResult{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	state := values.State
	if state == "" {
		state = values.TicketID
	}

	now := s.nowTime()
	s.mu.Lock()
	sessions := s.sessionsFor(storeKey)
	s.gcLocked(sessions, now)
	matchedBySecret := false
	matchedByTicket := false
	var session *oauthSession
	if sessionID != "" {
		session = sessions[sessionID]
		if session == nil {
			s.mu.Unlock()
			return OAuthResult{}, ErrOAuthSession
		}
		// The panel already selected the session by its authenticated
		// session_id. Huawei rewrites ticket_id into an opaque state (for
		// example "<ticket>.pkg"); v1 deliberately does not require that state
		// on this path. Browser GET callbacks (sessionID == "") still use the
		// exact secret/ticket/state matching below.
		state = ""
	} else if values.Secret != "" {
		// v1's _find_session treats a supplied secret as authoritative: an
		// unknown secret is rejected instead of falling back to a ticket or
		// state that may belong to another session.
		session = s.findBySecretLocked(storeKey, values.Secret)
		matchedBySecret = session != nil
		if session == nil {
			s.mu.Unlock()
			return OAuthResult{}, ErrOAuthSession
		}
	} else {
		// Only use ticket/state lookup when the callback supplied no secret.
		if values.TicketID != "" {
			session = s.findByStateLocked(storeKey, values.TicketID)
			matchedByTicket = session != nil
		}
		if session == nil && state != "" {
			session = s.findByStateLocked(storeKey, state)
		}
	}
	if session == nil {
		s.mu.Unlock()
		return OAuthResult{}, ErrOAuthSession
	}
	if values.Port != 0 && values.Port != session.Port {
		s.mu.Unlock()
		return OAuthResult{}, ErrOAuthCallbackURL
	}
	if matchedBySecret || matchedByTicket {
		// A supplied secret/ticket is authoritative; the separate state value
		// may be stale (for example another session's ticket id).
		state = ""
	}
	sessionID = session.ID
	s.mu.Unlock()

	if values.Code != "" {
		return s.Callback(store, sessionID, values.Code, state)
	}
	if values.Secret != "" && values.Redirect != "" {
		poll, err := s.Continue(store, sessionID, values.Secret, values.Redirect)
		if err != nil {
			return OAuthResult{}, err
		}
		return OAuthResult{
			OK:        true,
			Status:    "continue",
			Message:   poll.Message,
			SessionID: poll.SessionID,
			AuthURL:   poll.AuthURL,
			NextURL:   poll.NextURL,
		}, nil
	}
	return OAuthResult{}, ErrOAuthCallback
}

func effectivePort(target *url.URL) string {
	if target.Port() != "" {
		return target.Port()
	}
	switch strings.ToLower(target.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func oauthIdentity(token Token) (uid, name, domain string) {
	uid = strings.TrimSpace(token.UserID)
	name = strings.TrimSpace(token.UserName)
	domain = strings.TrimSpace(token.DomainID)
	if uid == "" || name == "" {
		lookedUID, lookedName, lookedDomain := lookupOAuthIdentity(token.Credentials)
		if uid == "" {
			uid = lookedUID
		}
		if name == "" {
			name = lookedName
		}
		if domain == "" {
			domain = lookedDomain
		}
	}
	if uid == "" {
		uid = identityFromRefreshToken(token.RefreshToken)
	}
	if uid == "" {
		sum := sha256.Sum256([]byte(token.RefreshToken))
		uid = "codearts-" + hex.EncodeToString(sum[:])[:12]
	}
	if name == "" {
		name = uid
	}
	return uid, name, domain
}

// lookupOAuthIdentity mirrors v1's STS enrichment: caller-identity first, then
// current/user for the display name when caller-identity only has ids. Both
// requests are best effort with a 10s bound and never use a proxy slot. The
// credential values are used only for request signing and are never returned
// or included in an error.
func lookupOAuthIdentity(creds Credentials) (uid, name, domain string) {
	holder := Auth{
		AccessKeyID:      creds.AccessKeyID,
		SecretAccessKey:  creds.SecretAccessKey,
		CloudDragonToken: creds.SecurityToken,
	}
	firstUID, firstName, firstDomain := "", "", ""
	if data, err := oauthSignedIdentityGet(holder, stsHost()+"/v5/caller-identity"); err == nil {
		firstUID = oauthFirstNonEmpty(modelText(data["principal_id"]), modelText(data["account_id"]))
		urn := strings.TrimSpace(modelText(data["principal_urn"]))
		if index := strings.LastIndex(urn, "/"); index >= 0 {
			firstName = strings.TrimSpace(urn[index+1:])
		}
		firstDomain = strings.TrimSpace(modelText(data["account_id"]))
		if firstUID != "" && firstName != "" && firstName != firstUID {
			return firstUID, firstName, firstDomain
		}
	}
	if data, err := oauthSignedIdentityGet(holder, snapManagerBase()+"/v1/current/user"); err == nil {
		currentUID := strings.TrimSpace(modelText(data["user_id"]))
		currentName := strings.TrimSpace(modelText(data["user_name"]))
		currentDomain := strings.TrimSpace(modelText(data["domain_id"]))
		if currentUID != "" || firstUID != "" {
			return oauthFirstNonEmpty(firstUID, currentUID),
				oauthFirstNonEmpty(currentName, firstName),
				oauthFirstNonEmpty(currentDomain, firstDomain)
		}
	}
	return firstUID, firstName, firstDomain
}

func oauthFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// oauthSignedIdentityGet performs one signed GET with the STS temporary
// credentials. The response must be a JSON object; any transport/HTTP/decode
// failure is returned to the best-effort caller without credential text.
func oauthSignedIdentityGet(holder Auth, endpoint string) (map[string]any, error) {
	if strings.TrimSpace(holder.AccessKeyID) == "" ||
		strings.TrimSpace(holder.SecretAccessKey) == "" ||
		strings.TrimSpace(holder.CloudDragonToken) == "" {
		return nil, errors.New("codearts: identity lookup requires STS credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), oauthIdentityTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("codearts: identity lookup request is invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Language", "zh-cn")
	if err := SignRequest(request, nil, SignCredentials{
		AccessKeyID:     holder.AccessKeyID,
		SecretAccessKey: holder.SecretAccessKey,
		SecurityToken:   holder.CloudDragonToken,
	}); err != nil {
		return nil, errors.New("codearts: identity lookup signing failed")
	}
	response, err := oauthIdentityHTTP.Do(ctx, request, "")
	if err != nil {
		return nil, errors.New("codearts: identity lookup transport failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, oauthMaxResponseBytes))
	if err != nil {
		return nil, errors.New("codearts: identity lookup response read failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("codearts: identity lookup HTTP %d", response.StatusCode)
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, errors.New("codearts: identity lookup response is not a JSON object")
	}
	return payload, nil
}

func identityFromRefreshToken(refreshToken string) string {
	parts := strings.Split(strings.TrimSpace(refreshToken), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := decodeBase64URL(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(decodeStringValue(claims["sub"]))
}

func dpopPrivateScalar(jwk json.RawMessage) string {
	var fields dpopJWKFields
	if err := json.Unmarshal(jwk, &fields); err != nil {
		return ""
	}
	return fields.D
}

func decodeJSONObject(raw json.RawMessage) map[string]json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return nil
	}
	return object
}

func normalizeTokenPayload(raw []byte) (Token, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return Token{}, fmt.Errorf("%w: response is not a JSON object", ErrTokenResponse)
	}
	token := Token{
		UserID:       strings.TrimSpace(decodeStringValue(root["user_id"])),
		UserName:     strings.TrimSpace(decodeStringValue(root["user_name"])),
		DomainID:     strings.TrimSpace(decodeStringValue(root["domain_id"])),
		RefreshToken: strings.TrimSpace(decodeStringValue(root["refresh_token"])),
	}

	credentials := decodeJSONObject(root["credentials"])
	if credentials != nil {
		token.Credentials = Credentials{
			AccessKeyID:     strings.TrimSpace(decodeStringValue(credentials["access_key_id"])),
			SecretAccessKey: strings.TrimSpace(decodeStringValue(credentials["secret_access_key"])),
			SecurityToken:   strings.TrimSpace(decodeStringValue(credentials["security_token"])),
			Expiration:      strings.TrimSpace(decodeStringValue(credentials["expiration"])),
		}
	}
	legacy := decodeJSONObject(root["credential"])
	if token.Credentials.SecurityToken == "" && legacy != nil {
		legacyToken := strings.TrimSpace(decodeStringValue(legacy["securitytoken"]))
		if legacyToken != "" {
			token.Credentials = Credentials{
				AccessKeyID:     strings.TrimSpace(decodeStringValue(legacy["access"])),
				SecretAccessKey: strings.TrimSpace(decodeStringValue(legacy["secret"])),
				SecurityToken:   legacyToken,
				Expiration:      strings.TrimSpace(decodeStringValue(legacy["expires_at"])),
			}
		}
	}
	return token, nil
}

// ExchangeCode redeems one authorization code with PKCE and DPoP. The form and
// redirect URI match v1.6.5 exactly.
func ExchangeCode(code, verifier string, port int, dpopJWK json.RawMessage) (Token, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return Token{}, ErrOAuthCode
	}
	if strings.TrimSpace(verifier) == "" {
		return Token{}, ErrOAuthPKCE
	}
	if port < 1 || port > 65535 {
		return Token{}, errors.New("codearts: OAuth redirect port is out of range")
	}
	form := url.Values{}
	form.Set("client_id", oauthClientID())
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", fmt.Sprintf("http://127.0.0.1:%d%s", port, oauthRedirectPath))
	return RequestToken(form, dpopJWK)
}

// RequestToken POSTs a form to the STS token endpoint with a DPoP proof and
// returns the normalized response. It is also the building block for refresh.
func RequestToken(form url.Values, dpopJWK json.RawMessage) (Token, error) {
	return RequestTokenContext(context.Background(), form, dpopJWK)
}

// RequestTokenContext is RequestToken with a caller context.
func RequestTokenContext(ctx context.Context, form url.Values, dpopJWK json.RawMessage) (Token, error) {
	return requestTokenContextWith(ctx, oauthEndpointDoer{}, "", form, dpopJWK)
}

// requestTokenContextWith is the shared token request implementation. The
// refresh manager injects the core transport and the account proxy; the login
// flow keeps its package client and no proxy override.
func requestTokenContextWith(
	ctx context.Context,
	doer tokenEndpointDoer,
	proxyURL string,
	form url.Values,
	dpopJWK json.RawMessage,
) (Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if doer == nil {
		return Token{}, fmt.Errorf("%w: token request transport is missing", ErrTokenTransport)
	}
	endpoint := stsHost() + oauthTokenEndpoint
	proof, err := DPoPProof(dpopJWK, http.MethodPost, endpoint, "")
	if err != nil {
		return Token{}, err
	}
	body := form.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return Token{}, fmt.Errorf("%w: token request is invalid", ErrTokenTransport)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("DPoP", proof)

	sensitive := tokenRequestSecretValues(form, dpopJWK)
	response, err := doer.Do(ctx, request, proxyURL)
	if err != nil {
		return Token{}, oauthTransportError("token request failed", err, sensitive...)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, oauthMaxResponseBytes))
	if err != nil {
		return Token{}, oauthTransportError("read token response", err, sensitive...)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Token{}, &TokenEndpointError{
			Status: response.StatusCode,
			Detail: tokenEndpointDetail(raw, sensitive...),
		}
	}
	token, err := normalizeTokenPayload(raw)
	if err != nil {
		return Token{}, err
	}
	if strings.TrimSpace(token.Credentials.SecurityToken) == "" {
		return Token{}, fmt.Errorf("%w: response is missing security_token", ErrTokenResponse)
	}
	return token, nil
}

func tokenRequestSecretValues(form url.Values, dpopJWK json.RawMessage) []string {
	values := make([]string, 0, len(form)*2+2)
	for _, entries := range form {
		values = append(values, entries...)
	}
	values = append(values, string(dpopJWK), dpopPrivateScalar(dpopJWK))
	return values
}

func tokenEndpointDetail(raw []byte, secrets ...string) string {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err == nil && root != nil {
		parts := make([]string, 0, 5)
		seen := make(map[string]bool)
		for _, key := range []string{"error", "error_msg", "message", "error_description", "code"} {
			value := strings.TrimSpace(decodeStringValue(root[key]))
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			parts = append(parts, value)
		}
		if len(parts) > 0 {
			return truncateOAuthMessage(redactOAuthValues(strings.Join(parts, ": "), secrets...), oauthMessageLimit)
		}
	}
	text := redactOAuthValues(strings.TrimSpace(string(raw)), secrets...)
	return truncateOAuthMessage(text, oauthTokenRejectBodyMax)
}

func oauthTransportError(prefix string, err error, secrets ...string) error {
	message := prefix
	if err != nil {
		message += ": " + err.Error()
	}
	message = redactOAuthValues(message, secrets...)
	return fmt.Errorf("%w: %s", ErrTokenTransport, message)
}

type redactedOAuthError struct {
	cause   error
	message string
}

func (e *redactedOAuthError) Error() string { return e.message }
func (e *redactedOAuthError) Unwrap() error { return e.cause }

func redactOAuthError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := redactOAuthValues(err.Error(), secrets...)
	if message == err.Error() {
		return err
	}
	return &redactedOAuthError{cause: err, message: message}
}

func redactOAuthValues(text string, values ...string) string {
	ordered := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			ordered = append(ordered, value)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, value := range ordered {
		text = strings.ReplaceAll(text, value, "<redacted>")
	}
	return text
}

func truncateOAuthMessage(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// BeginOAuth starts an authorization using the package session table.
func BeginOAuth(store *config.Store, port int) (OAuthStart, error) {
	return DefaultOAuthSessions.Begin(store, port)
}

// PollOAuth polls an authorization using the package session table.
func PollOAuth(store *config.Store, sessionID string) (OAuthPoll, error) {
	return DefaultOAuthSessions.Poll(store, sessionID)
}

// HandleOAuthCallback completes an authorization using the package session
// table.
func HandleOAuthCallback(store *config.Store, sessionID, code, state string) (OAuthResult, error) {
	return DefaultOAuthSessions.Callback(store, sessionID, code, state)
}

// ContinueOAuth handles a secret+redirect callback using the package session
// table.
func ContinueOAuth(store *config.Store, sessionID, secret, redirect string) (OAuthPoll, error) {
	return DefaultOAuthSessions.Continue(store, sessionID, secret, redirect)
}

// HandleOAuthCallbackURL handles a raw loopback callback URL using the package
// session table.
func HandleOAuthCallbackURL(store *config.Store, sessionID, callbackURL string) (OAuthResult, error) {
	return DefaultOAuthSessions.CallbackURL(store, sessionID, callbackURL)
}
