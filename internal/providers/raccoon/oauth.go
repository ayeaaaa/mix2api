package raccoon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// oauthSession is one browser authorization attempt. State and code never
// enter the public poll payload; only the panel session id is exposed.
type oauthSession struct {
	ID        string
	State     string
	Port      int
	CreatedAt time.Time
	Status    string
	Message   string
	AuthURL   string
	Account   map[string]any
}

func authorizePageURL() string {
	if value := strings.TrimSpace(os.Getenv("WB_RACCOON_AUTHORIZE_URL")); value != "" {
		return value
	}
	return "https://xiaohuanxiong.com/authorize"
}

func oauthAuthURL(port int, state string) string {
	return fmt.Sprintf(
		"%s?login_source=desktop&appname=办公小浣熊客户端&desktop_callback=loopback&redirect=http://127.0.0.1:%d/auth/callback&state=%s",
		authorizePageURL(), port, state,
	)
}

func randomHex(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func stateMatches(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (p *Provider) gcOAuthLocked(now time.Time) {
	for id, session := range p.oauthSessions {
		if now.Sub(session.CreatedAt) > OAuthTTLSeconds*time.Second {
			delete(p.oauthSessions, id)
		}
	}
}

func (p *Provider) oauthStart(port int) (map[string]any, *adminError) {
	if port < 1024 || port > 65535 {
		return nil, badRequest("gateway port must be between 1024 and 65535")
	}

	now := p.nowTime()
	state, err := randomHex(oauthStateBytes)
	if err != nil {
		return nil, serverError("raccoon: cannot create OAuth session")
	}
	sessionID, err := randomHex(oauthIDBytes)
	if err != nil {
		return nil, serverError("raccoon: cannot create OAuth session")
	}

	p.oauthMu.Lock()
	defer p.oauthMu.Unlock()
	p.gcOAuthLocked(now)
	for len(p.oauthSessions) >= OAUTHMaxSessions {
		var oldest *oauthSession
		for _, candidate := range p.oauthSessions {
			if candidate.Status != "pending" {
				continue
			}
			if oldest == nil || candidate.CreatedAt.Before(oldest.CreatedAt) {
				oldest = candidate
			}
		}
		if oldest == nil {
			return nil, badRequest("OAuth 会话过多：请稍后重试或等待现有授权结束")
		}
		delete(p.oauthSessions, oldest.ID)
	}
	for {
		if _, exists := p.oauthSessions[sessionID]; !exists {
			break
		}
		sessionID, err = randomHex(oauthIDBytes)
		if err != nil {
			return nil, serverError("raccoon: cannot create OAuth session")
		}
	}
	for {
		collision := false
		for _, candidate := range p.oauthSessions {
			if stateMatches(candidate.State, state) {
				collision = true
				break
			}
		}
		if !collision {
			break
		}
		state, err = randomHex(oauthStateBytes)
		if err != nil {
			return nil, serverError("raccoon: cannot create OAuth session")
		}
	}

	authURL := oauthAuthURL(port, state)
	session := &oauthSession{
		ID:        sessionID,
		State:     state,
		Port:      port,
		CreatedAt: now,
		Status:    "pending",
		Message:   "等待浏览器完成授权…",
		AuthURL:   authURL,
	}
	p.oauthSessions[sessionID] = session
	return map[string]any{
		"ok":         true,
		"session_id": sessionID,
		"auth_url":   authURL,
		"expires_in": OAuthTTLSeconds,
		"message":    "请在浏览器打开授权链接，完成后等待自动检测",
	}, nil
}

func (p *Provider) oauthPublicLocked(session *oauthSession) map[string]any {
	status := session.Status
	if status == "exchanging" {
		status = "pending"
	}
	remaining := OAuthTTLSeconds*time.Second - p.nowTime().Sub(session.CreatedAt)
	expires := int(remaining / time.Second)
	if expires < 0 {
		expires = 0
	}
	out := map[string]any{
		"ok":         status != "error",
		"status":     status,
		"message":    session.Message,
		"session_id": session.ID,
		"auth_url":   session.AuthURL,
		"expires_in": expires,
	}
	if session.Account != nil {
		account := make(map[string]any, len(session.Account))
		for key, value := range session.Account {
			account[key] = value
		}
		out["account"] = account
	}
	return out
}

func (p *Provider) oauthPoll(sessionID string) (map[string]any, *adminError) {
	now := p.nowTime()
	p.oauthMu.Lock()
	defer p.oauthMu.Unlock()
	p.gcOAuthLocked(now)
	session := p.oauthSessions[strings.TrimSpace(sessionID)]
	if session == nil {
		return nil, &adminError{
			Status:  http.StatusConflict,
			Type:    "invalid_request_error",
			Message: "OAuth session not found or expired; start again",
		}
	}
	return p.oauthPublicLocked(session), nil
}

func (p *Provider) findOAuthSession(sessionID string) *oauthSession {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	now := p.nowTime()
	p.oauthMu.Lock()
	defer p.oauthMu.Unlock()
	p.gcOAuthLocked(now)
	return p.oauthSessions[sessionID]
}

func (p *Provider) findPendingOAuthState(state string) *oauthSession {
	state = strings.TrimSpace(state)
	if state == "" {
		return nil
	}
	now := p.nowTime()
	p.oauthMu.Lock()
	defer p.oauthMu.Unlock()
	p.gcOAuthLocked(now)
	for _, session := range p.oauthSessions {
		if session.Status == "pending" && stateMatches(session.State, state) {
			return session
		}
	}
	return nil
}

// claimOAuthSession atomically consumes one pending session. The state is
// single-use: a replay against a done/error/exchanging session returns nil.
func (p *Provider) claimOAuthSession(sessionID, state string) *oauthSession {
	now := p.nowTime()
	p.oauthMu.Lock()
	defer p.oauthMu.Unlock()
	p.gcOAuthLocked(now)

	var session *oauthSession
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		session = p.oauthSessions[sessionID]
	} else {
		state = strings.TrimSpace(state)
		if state == "" {
			return nil
		}
		for _, candidate := range p.oauthSessions {
			if candidate.Status == "pending" && stateMatches(candidate.State, state) {
				session = candidate
				break
			}
		}
	}
	if session == nil || session.Status != "pending" {
		return nil
	}
	if state != "" && !stateMatches(session.State, state) {
		return nil
	}
	session.Status = "exchanging"
	return session
}

func (p *Provider) oauthDone(session *oauthSession, uid, nickname string) {
	p.oauthMu.Lock()
	defer p.oauthMu.Unlock()
	session.Status = "done"
	session.Message = "登录成功：" + nickname
	session.Account = map[string]any{"uid": uid, "nickname": nickname}
}

func (p *Provider) oauthFailed(session *oauthSession, message string) {
	if message == "" {
		message = "OAuth exchange failed"
	}
	p.oauthMu.Lock()
	defer p.oauthMu.Unlock()
	session.Status = "error"
	session.Message = truncateRunes(message, 500)
}

func redactOAuthError(message string, secrets ...string) string {
	if strings.TrimSpace(message) == "" {
		return "OAuth exchange failed"
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "<redacted>")
	}
	return truncateRunes(redactText(message, secrets...), 500)
}

// exchangeOAuthSession redeems code, saves and enables the account, and
// publishes the result on the session. Failures are stored redacted.
func (p *Provider) exchangeOAuthSession(ctx context.Context, session *oauthSession, code string) *adminError {
	account, failure := p.exchangeCode(ctx, code)
	if failure != nil {
		message := redactOAuthError(failure.Message, code, session.State)
		p.oauthFailed(session, message)
		failure.Message = message
		return failure
	}
	view := account.View()
	nickname := view.Nickname
	if nickname == "" {
		nickname = view.UID
	}
	p.oauthDone(session, view.UID, nickname)
	return nil
}

func (p *Provider) exchangeCode(ctx context.Context, code string) (*Account, *adminError) {
	if ctx == nil {
		ctx = context.Background()
	}
	code = cleanToken(code)
	if code == "" {
		return nil, badRequest("authorization code is required")
	}
	client := p.currentHTTPClient()
	if client == nil {
		return nil, serverError("raccoon: login http client is not configured")
	}
	body, err := json.Marshal(map[string]string{"authorization_code": code})
	if err != nil {
		return nil, serverError("raccoon: cannot build exchange request")
	}
	payload, err := client.DoJSON(
		ctx, http.MethodPost, LoginPath, http.Header{"Accept": {"application/json"}},
		body, "", code,
	)
	if err != nil {
		failure := adminErrorFrom(err)
		failure.Message = redactOAuthError(failure.Message, code)
		return nil, failure
	}
	auth, failure := authFromPayload(payload, p.nowTime())
	if failure != nil {
		failure.Message = redactOAuthError(failure.Message, code)
		return nil, failure
	}
	account, err := p.saveAndEnable(auth)
	if err != nil {
		return nil, &adminError{
			Status:  http.StatusBadGateway,
			Type:    "invalid_request_error",
			Message: redactOAuthError(err.Error(), code),
		}
	}
	return account, nil
}

// authFromPayload normalizes a flat auth.json or a login/refresh response.
// The nested "data" object wins, token names accept access_token/token/
// auth_token, Bearer prefixes are stripped, and an access token is required.
func authFromPayload(payload map[string]any, now time.Time) (Auth, *adminError) {
	if payload == nil {
		return Auth{}, badRequest("auth payload must be an object")
	}
	source := make(map[string]any, len(payload))
	for key, value := range payload {
		source[key] = value
	}
	if data, ok := payload["data"].(map[string]any); ok {
		for key, value := range data {
			source[key] = value
		}
	}

	access := cleanToken(pointsText(source["access_token"]))
	if access == "" {
		access = cleanToken(pointsText(source["token"]))
	}
	if access == "" {
		access = cleanToken(pointsText(source["auth_token"]))
	}
	if access == "" {
		return Auth{}, badRequest("auth payload requires access_token")
	}

	tokenType := strings.TrimSpace(pointsText(source["token_type"]))
	if tokenType == "" {
		tokenType = "Bearer"
	}
	auth := Auth{
		AccessToken:    access,
		RefreshToken:   cleanToken(pointsText(source["refresh_token"])),
		TokenType:      tokenType,
		OfficeIdentity: strings.TrimSpace(pointsText(source["office_identity"])),
		OfficeOrgName:  strings.TrimSpace(pointsText(source["office_org_name"])),
		OfficeOrgRole:  strings.TrimSpace(pointsText(source["office_org_role"])),
		UpdatedAt:      now.Unix(),
		ExpiresAt:      expiresAtFromAccessToken(access),
	}
	proxySlot := strings.TrimSpace(pointsText(source["proxySlot"]))
	if proxySlot == "" {
		proxySlot = strings.TrimSpace(pointsText(source["proxy_slot"]))
	}
	if proxySlot != "" {
		auth.ProxySlot = proxySlot
	}
	auth.UID = auth.uidFromCredentials()
	if auth.UID == "" {
		return Auth{}, badRequest("auth payload requires a uid")
	}
	return auth, nil
}

// parseLoopbackCallback validates a manually pasted callback URL. Only the
// official GET path is accepted; the legacy /raccoon/oauth/callback path is
// rejected rather than reintroduced.
func parseLoopbackCallback(raw string) (*url.URL, int, *adminError) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, 0, badRequest("invalid callback URL")
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return nil, 0, badRequest("callback URL must use http")
	}
	if parsed.User != nil {
		return nil, 0, badRequest("callback URL must not contain credentials")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return nil, 0, badRequest("callback URL must point at loopback")
	}
	if parsed.Path != "/auth/callback" {
		return nil, 0, badRequest("callback URL path must be /auth/callback")
	}
	portText := parsed.Port()
	if portText == "" {
		return nil, 0, badRequest("callback URL must include the gateway port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, 0, badRequest("callback URL has an invalid port")
	}
	return parsed, port, nil
}

func callbackHTML(message string) string {
	return `<!doctype html><meta charset="utf-8"><body style="font-family:sans-serif;padding:32px;line-height:1.7">` +
		"<h3>" + html.EscapeString(message) + "</h3></body>"
}

func (p *Provider) oauthCallbackGET(ctx context.Context, query url.Values) (int, string) {
	state := strings.TrimSpace(query.Get("state"))
	code := cleanToken(query.Get("code"))
	if state == "" {
		return http.StatusBadRequest, callbackHTML("回调缺少 state，请回到看板重新发起授权。")
	}
	session := p.findPendingOAuthState(state)
	if session == nil {
		return http.StatusBadRequest, callbackHTML("登录会话不存在或已过期，请回到看板重新发起。")
	}
	if code == "" {
		return http.StatusBadRequest, callbackHTML("登录失败：回调缺少 code。")
	}
	claimed := p.claimOAuthSession("", state)
	if claimed == nil {
		return http.StatusBadRequest, callbackHTML("登录会话不存在或已过期，请回到看板重新发起。")
	}
	if failure := p.exchangeOAuthSession(ctx, claimed, code); failure != nil {
		return http.StatusBadRequest, callbackHTML("登录失败：" + failure.Message)
	}
	return http.StatusOK, callbackHTML("Raccoon 登录成功，可以关闭此页面。")
}

func (p *Provider) oauthCallback(
	ctx context.Context,
	sessionID, callbackURL, code, state string,
) (map[string]any, *adminError) {
	sessionID = strings.TrimSpace(sessionID)
	callbackURL = strings.TrimSpace(callbackURL)
	code = cleanToken(code)
	state = strings.TrimSpace(state)

	var parsed *url.URL
	callbackPort := 0
	urlState := ""
	if callbackURL != "" {
		var failure *adminError
		parsed, callbackPort, failure = parseLoopbackCallback(callbackURL)
		if failure != nil {
			return nil, failure
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			return nil, badRequest("invalid callback URL")
		}
		urlState = strings.TrimSpace(query.Get("state"))
		urlCode := cleanToken(query.Get("code"))
		if urlState == "" {
			return nil, badRequest("callback URL is missing state")
		}
		if state != "" && !stateMatches(state, urlState) {
			return nil, badRequest("callback state does not match the session")
		}
		state = urlState
		if code == "" {
			code = urlCode
		}
	}

	var session *oauthSession
	switch {
	case sessionID != "":
		session = p.findOAuthSession(sessionID)
		if session == nil {
			return nil, &adminError{
				Status: http.StatusConflict, Type: "invalid_request_error",
				Message: "OAuth session not found or expired; start again",
			}
		}
	case state != "":
		session = p.findPendingOAuthState(state)
		if session == nil {
			return nil, &adminError{
				Status: http.StatusConflict, Type: "invalid_request_error",
				Message: "OAuth session not found or expired; start again",
			}
		}
		sessionID = session.ID
	default:
		return nil, badRequest("session_id or state is required")
	}

	if parsed != nil {
		if callbackPort != session.Port {
			return nil, badRequest("callback port does not match the authorization session")
		}
		if !stateMatches(urlState, session.State) {
			return nil, badRequest("callback state does not match the session")
		}
	}
	if state != "" && !stateMatches(state, session.State) {
		return nil, badRequest("callback state does not match the session")
	}
	if code == "" {
		return nil, badRequest("callback is missing code")
	}

	claimed := p.claimOAuthSession(sessionID, session.State)
	if claimed == nil {
		return nil, &adminError{
			Status: http.StatusConflict, Type: "invalid_request_error",
			Message: "OAuth session is no longer pending; start a new authorization",
		}
	}
	if failure := p.exchangeOAuthSession(ctx, claimed, code); failure != nil {
		return nil, failure
	}
	p.oauthMu.Lock()
	result := p.oauthPublicLocked(claimed)
	p.oauthMu.Unlock()
	return result, nil
}
