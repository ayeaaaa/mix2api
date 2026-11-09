package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

const (
	oauthStatePath       = "/v2/plugin/auth/state"
	oauthTokenPath       = "/v2/plugin/auth/token"
	oauthLoginPath       = "/v2/plugin/login/account"
	oauthTTLSeconds      = 600
	oauthMaxSessions     = 64
	oauthDefaultPlatform = "CLI"
	loginSlotAuto        = "auto"

	oauthDefaultHTTPTimeout    = 30 * time.Second
	oauthDefaultProfileTimeout = 15 * time.Second
)

// oauthDoer is the provider's contextual HTTP client. The shared core
// transport.Client implements it; tests can inject a fake upstream.
type oauthDoer interface {
	Do(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error)
}

type oauthSession struct {
	State     string
	AuthURL   string
	Platform  string
	Realm     string
	ProxySlot string
	ProxyURL  string
	AutoSlot  bool
	CreatedAt time.Time
}

// OAuthStart is the panel-facing browser-login start response. It carries no
// credential values.
type OAuthStart struct {
	OK        bool   `json:"ok"`
	State     string `json:"state"`
	AuthURL   string `json:"authUrl"`
	Realm     string `json:"realm"`
	Platform  string `json:"platform"`
	ProxySlot string `json:"proxySlot,omitempty"`
	Proxy     string `json:"proxy,omitempty"`
	ExpiresIn int    `json:"expires_in"`
}

// OAuthAccount is the identity published by a completed login. Tokens never
// appear here.
type OAuthAccount struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Realm    string `json:"realm"`
	Domain   string `json:"domain"`
	Source   string `json:"source"`
	APIKey   bool   `json:"apiKey"`
}

// OAuthPoll is the public state of one browser-login session.
type OAuthPoll struct {
	OK        bool          `json:"ok"`
	Status    string        `json:"status"`
	Message   string        `json:"message,omitempty"`
	State     string        `json:"state,omitempty"`
	AuthURL   string        `json:"authUrl,omitempty"`
	ProxySlot string        `json:"proxySlot,omitempty"`
	Account   *OAuthAccount `json:"account,omitempty"`
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (p *Provider) oauthStartBudget() time.Duration {
	if p == nil {
		return oauthDefaultHTTPTimeout
	}
	return durationOrDefault(p.oauthStartTimeout, oauthDefaultHTTPTimeout)
}

func (p *Provider) oauthTokenBudget() time.Duration {
	if p == nil {
		return oauthDefaultHTTPTimeout
	}
	return durationOrDefault(p.oauthTokenTimeout, oauthDefaultHTTPTimeout)
}

func (p *Provider) oauthProfileBudget() time.Duration {
	if p == nil {
		return oauthDefaultProfileTimeout
	}
	return durationOrDefault(p.oauthProfileTimeout, oauthDefaultProfileTimeout)
}

func (p *Provider) oauthNowTime() time.Time {
	if p != nil && p.oauthNow != nil {
		return p.oauthNow()
	}
	return time.Now()
}

func (p *Provider) oauthBase() string {
	if p == nil {
		return ""
	}
	if base := strings.TrimRight(strings.TrimSpace(p.oauthBaseURL), "/"); base != "" {
		return base
	}
	return strings.TrimRight(endpointFor(p.realm, productDesktop), "/")
}

func (p *Provider) oauthHTTP() (oauthDoer, error) {
	if p == nil {
		return nil, errors.New("workbuddy: provider is nil")
	}
	if p.oauthClient != nil {
		return p.oauthClient, nil
	}
	if p.transport == nil {
		return nil, errors.New("workbuddy: OAuth transport is unavailable")
	}
	return p.transport, nil
}

func oauthOriginForRealm(realm string) string {
	if realmOrDefault(realm) == realmCN {
		return "https://www.codebuddy.cn"
	}
	return "https://www.workbuddy.ai"
}

func (p *Provider) oauthHeaders() http.Header {
	origin := oauthOriginForRealm(p.realm)
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json, text/plain, */*")
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("User-Agent", "WorkBuddy/"+p.DesktopVersion())
	headers.Set("Origin", origin)
	headers.Set("Referer", origin+"/")
	return headers
}

// BeginOAuth asks the realm endpoint for a browser-login state/auth URL,
// records the pending session for this provider, and returns the public start
// descriptor. An explicit proxy slot must exist and be enabled; "auto" picks a
// random enabled slot that is not bound to an account or another pending login.
func (p *Provider) BeginOAuth(ctx context.Context, platform, proxySlot string) (OAuthStart, error) {
	if p == nil {
		return OAuthStart{}, errors.New("workbuddy: provider is nil")
	}
	platform = strings.TrimSpace(platform)
	if platform == "" {
		platform = oauthDefaultPlatform
	}
	slot, proxyURL, reserved, err := p.reserveLoginProxy(proxySlot)
	if err != nil {
		return OAuthStart{}, err
	}
	if reserved {
		defer p.releaseLoginProxy(slot)
	}

	base := p.oauthBase()
	client, err := p.oauthHTTP()
	if err != nil {
		return OAuthStart{}, err
	}
	query := url.Values{}
	query.Set("platform", platform)
	endpoint := base + oauthStatePath + "?" + query.Encode()
	status, payload, err := p.oauthJSON(ctx, client, http.MethodPost, endpoint, proxyURL, []byte("{}"), p.oauthStartBudget())
	if err != nil {
		return OAuthStart{}, err
	}
	if status < 200 || status >= 300 {
		return OAuthStart{}, fmt.Errorf("workbuddy: OAuth state HTTP %d", status)
	}
	data := decodeJSONObjectRaw(payload["data"])
	state := strings.TrimSpace(decodeAuthString(data["state"]))
	authURL := strings.TrimSpace(decodeAuthString(data["authUrl"]))
	if state == "" || authURL == "" {
		return OAuthStart{}, errors.New("workbuddy: auth/state returned no state/authUrl")
	}

	now := p.oauthNowTime()
	p.oauthMu.Lock()
	if p.oauthSessions == nil {
		p.oauthSessions = map[string]*oauthSession{}
	}
	p.gcOAuthLocked(now)
	if p.oauthSessions[state] != nil {
		p.oauthMu.Unlock()
		return OAuthStart{}, errors.New("workbuddy: upstream returned a duplicate login state")
	}
	for len(p.oauthSessions) >= oauthMaxSessions {
		oldest := ""
		var oldestAt time.Time
		for key, session := range p.oauthSessions {
			if oldest == "" || session.CreatedAt.Before(oldestAt) {
				oldest, oldestAt = key, session.CreatedAt
			}
		}
		if oldest == "" {
			break
		}
		delete(p.oauthSessions, oldest)
	}
	p.oauthSessions[state] = &oauthSession{
		State:     state,
		AuthURL:   authURL,
		Platform:  platform,
		Realm:     p.realm,
		ProxySlot: slot,
		ProxyURL:  proxyURL,
		AutoSlot:  strings.TrimSpace(proxySlot) == loginSlotAuto,
		CreatedAt: now,
	}
	p.oauthMu.Unlock()

	return OAuthStart{
		OK:        true,
		State:     state,
		AuthURL:   authURL,
		Realm:     p.realm,
		Platform:  platform,
		ProxySlot: slot,
		Proxy:     proxyDisplay(proxyURL),
		ExpiresIn: oauthTTLSeconds,
	}, nil
}

// BeginBrowserOAuth is the v1-named alias for BeginOAuth.
func (p *Provider) BeginBrowserOAuth(ctx context.Context, platform, proxySlot string) (OAuthStart, error) {
	return p.BeginOAuth(ctx, platform, proxySlot)
}

// PollOAuth checks one pending browser login. Pending responses stay pollable;
// unknown and expired sessions are terminal; a successful response saves the
// account (preserving duplicate-login operator state) and consumes the
// session. No token is copied into the public result.
func (p *Provider) PollOAuth(ctx context.Context, state string) (OAuthPoll, error) {
	if p == nil {
		return OAuthPoll{}, errors.New("workbuddy: provider is nil")
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return OAuthPoll{OK: false, Status: "unknown", Message: "state not recognised - start the login again"}, nil
	}
	now := p.oauthNowTime()
	p.oauthMu.Lock()
	session := p.oauthSessions[state]
	if session == nil {
		p.oauthMu.Unlock()
		return OAuthPoll{OK: false, Status: "unknown", Message: "state not recognised - start the login again"}, nil
	}
	if now.Sub(session.CreatedAt) > oauthTTLSeconds*time.Second {
		delete(p.oauthSessions, state)
		p.oauthMu.Unlock()
		return OAuthPoll{OK: false, Status: "expired", Message: "login window expired - start again"}, nil
	}
	snapshot := *session
	p.oauthMu.Unlock()

	client, err := p.oauthHTTP()
	if err != nil {
		return OAuthPoll{}, err
	}
	query := url.Values{}
	query.Set("state", state)
	endpoint := p.oauthBase() + oauthTokenPath + "?" + query.Encode()
	status, payload, err := p.oauthJSON(ctx, client, http.MethodGet, endpoint, snapshot.ProxyURL, nil, p.oauthTokenBudget())
	if err != nil {
		return OAuthPoll{
			OK: true, Status: "pending", State: state, AuthURL: snapshot.AuthURL,
			ProxySlot: snapshot.ProxySlot, Message: "poll error: upstream request failed",
		}, nil
	}
	if status < 200 || status >= 300 {
		return OAuthPoll{
			OK: true, Status: "pending", State: state, AuthURL: snapshot.AuthURL,
			ProxySlot: snapshot.ProxySlot, Message: fmt.Sprintf("poll error: HTTP %d", status),
		}, nil
	}
	data := decodeJSONObjectRaw(payload["data"])
	code, _ := decodeJSONInt(payload["code"])
	message := strings.TrimSpace(decodeAuthString(payload["msg"]))
	message = redactOAuthPayloadMessage(message, payload, data)
	message = truncateRunes(message, 500)
	if code == loginPending {
		if message == "" {
			message = "waiting for browser login"
		}
		return OAuthPoll{
			OK: true, Status: "pending", State: state, AuthURL: snapshot.AuthURL,
			ProxySlot: snapshot.ProxySlot, Message: message,
		}, nil
	}
	if code != 0 {
		if message == "" {
			message = "login rejected"
		}
		return OAuthPoll{
			OK: false, Status: "error", State: state, AuthURL: snapshot.AuthURL,
			ProxySlot: snapshot.ProxySlot, Message: fmt.Sprintf("code=%d msg=%s", code, message),
		}, nil
	}

	token := strings.TrimSpace(decodeAuthString(data["accessToken"]))
	if token == "" {
		return OAuthPoll{
			OK: true, Status: "pending", State: state, AuthURL: snapshot.AuthURL,
			ProxySlot: snapshot.ProxySlot, Message: "waiting for token",
		}, nil
	}
	apiKey := IsAPIKeyToken(token)
	claims := parseJWTClaims(token)
	uid := strings.TrimSpace(claims.Subject)
	if uid == "" && apiKey {
		uid = apiKeyUID(token)
	}
	if uid == "" {
		return OAuthPoll{
			OK: false, Status: "error", State: state, AuthURL: snapshot.AuthURL,
			ProxySlot: snapshot.ProxySlot, Message: "token response has no usable identity",
		}, nil
	}
	realm := realmOrDefault(snapshot.Realm)
	domain := strings.TrimSpace(decodeAuthString(data["domain"]))
	if domain == "" {
		domain = defaultDomain(realm)
	} else {
		domain = alignedDomainForRealm(realm, domain)
	}
	expires := decodeEpochTime(data["expiresAt"])
	if expires.IsZero() && !apiKey {
		expires = epochTime(claims.ExpiresAt)
	}
	nickname := p.fetchLoginAccount(ctx, client, token, state, snapshot.ProxyURL)
	if nickname == "" {
		nickname = truncateRunes(uid, 8)
	}
	next := Auth{
		UID:          uid,
		Nickname:     nickname,
		Realm:        realm,
		Domain:       domain,
		AccessToken:  token,
		RefreshToken: strings.TrimSpace(decodeAuthString(data["refreshToken"])),
		ExpiresAt:    expires,
		Platform:     snapshot.Platform,
		EnterpriseID: claims.EnterpriseID,
		APIKey:       apiKey,
		Enabled:      true,
		TasksEnabled: true,
		ProxySlot:    snapshot.ProxySlot,
		Source:       sourceOAuth,
	}
	if existing, ok := findAccount(p.AccountsDir(), uid); ok {
		if snapshot.AutoSlot && existing.ProxySlot != "" {
			next.ProxySlot = ""
		}
		next = mergeReloginAuth(existing, next)
	}
	next, err = normalizeAuth(next)
	if err != nil {
		return OAuthPoll{}, err
	}
	saved, err := SaveAccount(p.AccountsDir(), next)
	if err != nil {
		return OAuthPoll{}, err
	}
	p.oauthMu.Lock()
	delete(p.oauthSessions, state)
	p.oauthMu.Unlock()
	return OAuthPoll{
		OK:        true,
		Status:    "ok",
		Message:   "login successful",
		State:     state,
		AuthURL:   snapshot.AuthURL,
		ProxySlot: snapshot.ProxySlot,
		Account: &OAuthAccount{
			UID:      saved.UID,
			Nickname: saved.Nickname,
			Realm:    saved.Realm,
			Domain:   saved.Domain,
			Source:   saved.Source,
			APIKey:   saved.APIKey,
		},
	}, nil
}

// CancelOAuth forgets a pending login. It is safe to call twice.
func (p *Provider) CancelOAuth(state string) bool {
	if p == nil {
		return false
	}
	state = strings.TrimSpace(state)
	if state == "" {
		return false
	}
	p.oauthMu.Lock()
	defer p.oauthMu.Unlock()
	if p.oauthSessions[state] == nil {
		return false
	}
	delete(p.oauthSessions, state)
	return true
}

func (p *Provider) gcOAuthLocked(now time.Time) {
	for state, session := range p.oauthSessions {
		if now.Sub(session.CreatedAt) > oauthTTLSeconds*time.Second {
			delete(p.oauthSessions, state)
		}
	}
}

func (p *Provider) fetchLoginAccount(ctx context.Context, client oauthDoer, token, state, proxyURL string) string {
	query := url.Values{}
	query.Set("state", state)
	endpoint := p.oauthBase() + oauthLoginPath + "?" + query.Encode()
	headers := p.oauthHeaders()
	headers.Set("Authorization", "Bearer "+token)
	status, payload, err := p.oauthJSONWithHeaders(ctx, client, http.MethodGet, endpoint, proxyURL, nil, headers, p.oauthProfileBudget())
	if err != nil || status < 200 || status >= 300 {
		return ""
	}
	data := decodeJSONObjectRaw(payload["data"])
	nickname := strings.TrimSpace(decodeAuthString(data["nickname"]))
	if nickname == "" {
		nickname = strings.TrimSpace(decodeAuthString(payload["nickname"]))
	}
	return nickname
}

func (p *Provider) oauthJSON(ctx context.Context, client oauthDoer, method, endpoint, proxyURL string, body []byte, timeout time.Duration) (int, map[string]json.RawMessage, error) {
	return p.oauthJSONWithHeaders(ctx, client, method, endpoint, proxyURL, body, p.oauthHeaders(), timeout)
}

func (p *Provider) oauthJSONWithHeaders(ctx context.Context, client oauthDoer, method, endpoint, proxyURL string, body []byte, headers http.Header, timeout time.Duration) (int, map[string]json.RawMessage, error) {
	if client == nil {
		return 0, nil, errors.New("workbuddy: OAuth HTTP client is nil")
	}
	ctx = nonNilContext(ctx)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, errors.New("workbuddy: OAuth request is invalid")
	}
	request.Header = headers.Clone()
	response, err := client.Do(ctx, request, proxyURL)
	if err != nil {
		return 0, nil, errors.New("workbuddy: OAuth request failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxAuthBody))
	if err != nil {
		return 0, nil, errors.New("workbuddy: OAuth response read failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, nil, nil
	}
	payload := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return response.StatusCode, nil, errors.New("workbuddy: OAuth response is not a JSON object")
	}
	return response.StatusCode, payload, nil
}

func decodeJSONObjectRaw(raw json.RawMessage) map[string]json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]json.RawMessage{}
	}
	object := map[string]json.RawMessage{}
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return map[string]json.RawMessage{}
	}
	return object
}

func decodeJSONInt(raw json.RawMessage) (int64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err == nil {
		if value, err := number.Int64(); err == nil {
			return value, true
		}
		if value, err := number.Float64(); err == nil {
			return int64(value), true
		}
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		if value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
			return value, true
		}
		if value, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
			return int64(value), true
		}
	}
	return 0, false
}

func redactOAuthPayloadMessage(message string, payload, data map[string]json.RawMessage) string {
	values := make([]string, 0, 8)
	add := func(object map[string]json.RawMessage) {
		for _, key := range []string{
			"accessToken", "access_token", "refreshToken", "refresh_token", "token",
		} {
			value := strings.TrimSpace(decodeAuthString(object[key]))
			if value != "" {
				values = append(values, value)
			}
		}
	}
	add(payload)
	add(data)
	return secrets.Redact(message, values...)
}

func proxyDisplay(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Host != "" {
		return parsed.Host
	}
	// Already host:port without a scheme.
	if index := strings.LastIndex(raw, "@"); index >= 0 {
		raw = raw[index+1:]
	}
	return raw
}

func (p *Provider) loginSlots() transport.Slots {
	if p == nil || p.store == nil {
		return nil
	}
	settings, err := p.store.Load()
	if err != nil {
		return nil
	}
	return transport.SlotsFromSettings(settings)
}

// SetLoginTakenSlots wires the cross-provider proxy-slot exclusion used by
// browser-login "auto" selection. The callback is invoked on demand, so the
// sibling provider may be constructed after WorkBuddy.
func (p *Provider) SetLoginTakenSlots(taken func() []string) {
	if p == nil {
		return
	}
	p.oauthMu.Lock()
	p.loginTakenSlots = taken
	p.oauthMu.Unlock()
}

func (p *Provider) takenLoginSlots() []string {
	if p == nil {
		return nil
	}
	p.oauthMu.Lock()
	callback := p.loginTakenSlots
	p.oauthMu.Unlock()
	if callback == nil {
		return nil
	}
	return callback()
}

func (p *Provider) reserveLoginProxy(proxySlot string) (string, string, bool, error) {
	wanted := strings.TrimSpace(proxySlot)
	if wanted == "" {
		return "", "", false, nil
	}
	slots := p.loginSlots()
	for _, slot := range slots {
		if slot.ID != wanted {
			continue
		}
		if !slot.Enabled {
			return "", "", false, fmt.Errorf("workbuddy: proxy slot %s is disabled", wanted)
		}
		return slot.ID, slot.URL, false, nil
	}
	if wanted != loginSlotAuto {
		return "", "", false, fmt.Errorf("workbuddy: proxy slot %s does not exist", wanted)
	}

	external := map[string]bool{}
	for _, slot := range p.takenLoginSlots() {
		if slot = strings.TrimSpace(slot); slot != "" {
			external[slot] = true
		}
	}

	p.oauthMu.Lock()
	defer p.oauthMu.Unlock()
	now := p.oauthNowTime()
	p.gcOAuthLocked(now)
	used := map[string]bool{}
	for slot := range external {
		used[slot] = true
	}
	if accounts, err := LoadAccounts(p.AccountsDir()); err == nil {
		for _, account := range accounts {
			if account.ProxySlot != "" {
				used[account.ProxySlot] = true
			}
		}
	}
	for _, session := range p.oauthSessions {
		if session.ProxySlot != "" {
			used[session.ProxySlot] = true
		}
	}
	for slot := range p.oauthReserved {
		used[slot] = true
	}
	free := make([]transport.Slot, 0, len(slots))
	for _, slot := range slots {
		if slot.Enabled && !used[slot.ID] {
			free = append(free, slot)
		}
	}
	if len(free) == 0 {
		return "", "", false, nil
	}
	sort.Slice(free, func(i, j int) bool { return free[i].ID < free[j].ID })
	pick := free[int(randomBytes(1)[0])%len(free)]
	if p.oauthReserved == nil {
		p.oauthReserved = map[string]bool{}
	}
	p.oauthReserved[pick.ID] = true
	return pick.ID, pick.URL, true, nil
}

func (p *Provider) releaseLoginProxy(slot string) {
	if p == nil || slot == "" {
		return
	}
	p.oauthMu.Lock()
	delete(p.oauthReserved, slot)
	p.oauthMu.Unlock()
}
