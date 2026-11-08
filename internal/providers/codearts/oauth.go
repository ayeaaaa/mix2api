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
	oauthMaxResponseBytes   = 1 << 20
	oauthMessageLimit       = 500
	oauthTokenRejectBodyMax = 300
)

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
		if now.Sub(session.CreatedAt) > oauthTTL {
			delete(sessions, id)
		}
	}
}

func evictOldestLocked(sessions map[string]*oauthSession) *oauthSession {
	var oldest *oauthSession
	for _, session := range sessions {
		if session.Status == "exchanging" {
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
	session.Account = account
	saved := auth
	session.SavedAuth = &saved
	return account
}

// Poll returns one session's public state. A session armed by Continue or a
// code-less callback performs one ticket poll per transition; done and error
// sessions are terminal and never contact the upstream again.
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
	if session.Status != "pending" || !session.TicketFallback {
		poll := s.publicPollLocked(session)
		s.mu.Unlock()
		return poll, nil
	}
	// Serialize the ticket exchange with a possible code callback.
	session.Status = "exchanging"
	session.Message = "正在获取登录结果…"
	snapshot := *session
	s.mu.Unlock()

	token, ticketErr := s.pollTicket(snapshot.TicketID, snapshot.Secret)
	if ticketErr != nil {
		message := redactOAuthValues(ticketErr.Error(), snapshot.TicketID, snapshot.Secret)
		s.failSession(storeKey, sessionID, message)
		s.mu.Lock()
		defer s.mu.Unlock()
		current := s.findLocked(storeKey, sessionID)
		if current == nil {
			return OAuthPoll{}, ErrOAuthSession
		}
		return s.publicPollLocked(current), nil
	}
	if strings.TrimSpace(token.Credentials.SecurityToken) == "" {
		s.mu.Lock()
		if current := s.findLocked(storeKey, sessionID); current != nil && current.Status == "exchanging" {
			current.Status = "pending"
			current.Message = "等待浏览器完成登录…"
			poll := s.publicPollLocked(current)
			s.mu.Unlock()
			return poll, nil
		}
		s.mu.Unlock()
	} else {
		auth, saveErr := s.saveOAuthAuth(store, &snapshot, token)
		if saveErr != nil {
			s.failSession(storeKey, sessionID, saveErr.Error())
			s.mu.Lock()
			defer s.mu.Unlock()
			current := s.findLocked(storeKey, sessionID)
			if current == nil {
				return OAuthPoll{}, ErrOAuthSession
			}
			return s.publicPollLocked(current), nil
		}
		s.completeSession(storeKey, sessionID, auth)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.findLocked(storeKey, sessionID)
	if current == nil {
		return OAuthPoll{}, ErrOAuthSession
	}
	return s.publicPollLocked(current), nil
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
	snapshot := *session
	s.mu.Unlock()

	token, err := ExchangeCode(code, snapshot.Verifier, snapshot.Port, snapshot.DPoPKey)
	if err != nil {
		failMessage := redactOAuthValues(err.Error(), code, snapshot.Verifier,
			snapshot.Secret, snapshot.TicketID, string(snapshot.DPoPKey), dpopPrivateScalar(snapshot.DPoPKey))
		s.failSession(storeKey, sessionID, failMessage)
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
	session.Secret = secret
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
	var session *oauthSession
	if sessionID != "" {
		session = sessions[sessionID]
		if session == nil {
			s.mu.Unlock()
			return OAuthResult{}, ErrOAuthSession
		}
	} else {
		if state != "" {
			session = s.findByStateLocked(storeKey, state)
		}
		if session == nil && values.Secret != "" {
			session = s.findBySecretLocked(storeKey, values.Secret)
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
	if ctx == nil {
		ctx = context.Background()
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
	response, err := oauthHTTPClient.Do(request)
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
