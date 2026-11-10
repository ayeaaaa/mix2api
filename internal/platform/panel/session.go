package panel

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sessions is an in-memory bearer-token table. Tokens are deliberately not
// persisted: a restart logs every browser out.
type Sessions struct {
	TTL time.Duration
	Now func() time.Time

	mu     sync.Mutex
	tokens map[string]time.Time
}

// NewSessions returns a session table with ttl. A non-positive ttl uses the v1
// seven-day default.
func NewSessions(ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = SessionTTL
	}
	return &Sessions{TTL: ttl}
}

func (s *Sessions) ttl() time.Duration {
	if s != nil && s.TTL > 0 {
		return s.TTL
	}
	return SessionTTL
}

func (s *Sessions) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Create mints a 24-byte URL-safe token.
func (s *Sessions) Create() string {
	if s == nil {
		return ""
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	if s.tokens == nil {
		s.tokens = map[string]time.Time{}
	}
	s.tokens[token] = s.now().Add(s.ttl())
	s.mu.Unlock()
	return token
}

// Valid reports whether token is a live session. Expired tokens are pruned.
func (s *Sessions) Valid(token string) bool {
	token = strings.TrimSpace(token)
	if s == nil || token == "" {
		return false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.tokens[token]
	if !ok {
		return false
	}
	if !now.Before(expiry) {
		delete(s.tokens, token)
		return false
	}
	return true
}

// Revoke removes one token.
func (s *Sessions) Revoke(token string) {
	if s == nil {
		return
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

// RevokeAll clears every session.
func (s *Sessions) RevokeAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.tokens = map[string]time.Time{}
	s.mu.Unlock()
}

// TokenFromRequest returns the panel session token from X-Panel-Token only.
// Query and cookie tokens are deliberately ignored so a token cannot leak via
// history, Referer, or access logs.
func TokenFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Panel-Token"))
}

// RequireSession returns middleware that accepts exactly one panel session
// token and rejects unsafe cross-site mutations. A nil table fails closed.
func RequireSession(sessions *Sessions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if next == nil {
			next = http.NotFoundHandler()
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !safeOrigin(r) {
				writePanelError(w, http.StatusForbidden, "cross-origin panel request rejected")
				return
			}
			if !sessions.Valid(TokenFromRequest(r)) {
				writePanelError(w, http.StatusUnauthorized, "panel password required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func safeOrigin(r *http.Request) bool {
	if r == nil {
		return true
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := parseOrigin(origin)
	if err != nil || parsed == "" {
		return false
	}
	return strings.EqualFold(parsed, strings.TrimSpace(r.Host))
}

func parseOrigin(origin string) (string, error) {
	scheme, rest, found := strings.Cut(origin, "://")
	if !found || scheme == "" || rest == "" {
		return "", errInvalidOrigin
	}
	host := rest
	if slash := strings.IndexByte(host, '/'); slash >= 0 {
		host = host[:slash]
	}
	if host == "" {
		return "", errInvalidOrigin
	}
	return host, nil
}

var errInvalidOrigin = &originError{}

type originError struct{}

func (*originError) Error() string { return "invalid origin" }

func writePanelError(w http.ResponseWriter, status int, message string) {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"code":    status,
		},
	})
	if err != nil {
		body = []byte(`{"error":{"message":"response encoding failed","type":"server_error","code":500}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
