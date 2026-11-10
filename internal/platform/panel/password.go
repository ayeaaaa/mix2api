// Package panel owns the web-panel password, in-memory sessions, login
// throttling, and the public status payload.
package panel

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

const (
	// DefaultPassword is the fresh-install panel password.
	DefaultPassword = "admin"
	// DefaultRounds is v1's PBKDF2-HMAC-SHA256 work factor.
	DefaultRounds = 120000
	// SessionTTL is seven days, matching v1.
	SessionTTL = 7 * 24 * time.Hour
)

// CryptoUnavailableReason is returned when the PBKDF2 primitive cannot run.
// v1 returned a hashlib.pbkdf2_hmac diagnostic rather than silently accepting a
// bad password; keep that wording recognizable for old clients.
const CryptoUnavailableReason = "module 'hashlib' has no attribute 'pbkdf2_hmac': PBKDF2-HMAC-SHA256 is unavailable; panel password hashing cannot run"

// Password stores and verifies the panel password in the shared settings file.
type Password struct {
	store *config.Store

	// Now is injectable for tests.
	Now func() time.Time
	// PBKDF2 is an injectable primitive; nil uses the in-repo implementation.
	// It returns an error when the crypto layer is unavailable.
	PBKDF2 func(password, salt []byte, rounds, keyLen int) ([]byte, error)
	// CryptoCheck lets composition report a platform-level crypto failure (for
	// example a FIPS build without SHA-256). Nil means available.
	CryptoCheck func() (bool, string)
}

// NewPassword returns a password service backed by store.
func NewPassword(store *config.Store) *Password {
	return &Password{store: store}
}

// CryptoAvailable reports whether password hashing can run and, when not, a
// safe diagnostic for /panel/status.
func (p *Password) CryptoAvailable() (bool, string) {
	if p != nil && p.CryptoCheck != nil {
		return p.CryptoCheck()
	}
	if p != nil && p.PBKDF2 != nil {
		if _, err := p.PBKDF2([]byte("smoke"), []byte("salt"), 1, 32); err != nil {
			return false, "pbkdf2: " + err.Error()
		}
		return true, ""
	}
	return true, ""
}

func (p *Password) cryptoError(reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = CryptoUnavailableReason
	}
	return errors.New(reason)
}

func (p *Password) digest(password, saltHex string, rounds int) (string, error) {
	ok, reason := p.CryptoAvailable()
	if !ok {
		return "", p.cryptoError(reason)
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return "", fmt.Errorf("invalid password salt hex: %w", err)
	}
	if rounds <= 0 {
		return "", errors.New("invalid password rounds")
	}
	fn := p.PBKDF2
	if fn == nil {
		fn = pbkdf2SHA256
	}
	key, err := fn([]byte(password), salt, rounds, sha256.Size)
	if err != nil {
		return "", p.cryptoError("pbkdf2 computation failed: " + err.Error())
	}
	return hex.EncodeToString(key), nil
}

// IsDefault reports whether the fresh-install password is still in force.
func (p *Password) IsDefault() bool {
	data, err := p.load()
	if err != nil {
		return true
	}
	hash := stringValue(data["panel_password_hash"])
	if hash == "" {
		return true
	}
	defaulted, _ := data["panel_password_default"].(bool)
	return defaulted
}

// Verify checks password against the stored digest. The default password opens
// a fresh install even when the crypto layer is unavailable, matching v1.
func (p *Password) Verify(password string) (bool, error) {
	data, err := p.load()
	if err != nil {
		return false, err
	}
	stored := stringValue(data["panel_password_hash"])
	if stored == "" {
		return password == DefaultPassword, nil
	}
	if defaulted, _ := data["panel_password_default"].(bool); defaulted {
		return password == DefaultPassword, nil
	}
	salt := stringValue(data["panel_password_salt"])
	if salt == "" {
		return false, nil
	}
	rounds := DefaultRounds
	if raw, present := data["panel_password_rounds"]; present {
		parsed, ok := integerValue(raw)
		if !ok {
			return false, nil
		}
		rounds = parsed
	}
	given, err := p.digest(password, salt, rounds)
	if err != nil {
		if strings.Contains(err.Error(), "invalid password rounds") || strings.Contains(err.Error(), "invalid password salt") {
			return false, nil
		}
		return false, err
	}
	storedBytes, err := hex.DecodeString(stored)
	if err != nil {
		return false, nil
	}
	givenBytes, err := hex.DecodeString(given)
	if err != nil {
		return false, nil
	}
	return hmac.Equal(givenBytes, storedBytes), nil
}

// Set stores a new password. Setting it back to "admin" restores the default
// sentinel and clears the digest material.
func (p *Password) Set(password string) error {
	if p == nil || p.store == nil {
		return errors.New("panel: settings store is required")
	}
	if password == DefaultPassword {
		_, err := p.store.Update(func(data map[string]any) error {
			delete(data, "panel_password_salt")
			delete(data, "panel_password_rounds")
			data["panel_password_hash"] = ""
			data["panel_password_default"] = true
			return nil
		})
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return p.cryptoError("random salt generation failed: " + err.Error())
	}
	saltHex := hex.EncodeToString(salt)
	hash, err := p.digest(password, saltHex, DefaultRounds)
	if err != nil {
		return err
	}
	_, err = p.store.Merge(map[string]any{
		"panel_password_salt":    saltHex,
		"panel_password_rounds":  DefaultRounds,
		"panel_password_hash":    hash,
		"panel_password_default": false,
	})
	return err
}

func (p *Password) load() (map[string]any, error) {
	if p == nil || p.store == nil {
		return map[string]any{}, nil
	}
	return p.store.Load()
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

func integerValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		return parsed, err == nil
	default:
		return 0, false
	}
}

// pbkdf2SHA256 is a small in-repo PBKDF2-HMAC-SHA256 implementation. It keeps
// the module dependency-free and is pinned by tests against published vectors.
func pbkdf2SHA256(password, salt []byte, rounds, keyLen int) ([]byte, error) {
	if rounds <= 0 || keyLen <= 0 {
		return nil, errors.New("pbkdf2: invalid parameters")
	}
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	var counter [4]byte
	for block := 1; block <= blocks; block++ {
		counter[0] = byte(block >> 24)
		counter[1] = byte(block >> 16)
		counter[2] = byte(block >> 8)
		counter[3] = byte(block)
		prf.Reset()
		_, _ = prf.Write(salt)
		_, _ = prf.Write(counter[:])
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for round := 1; round < rounds; round++ {
			prf.Reset()
			_, _ = prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen], nil
}

// LoginLimiter bounds failed panel logins per client key.
type LoginLimiter struct {
	Max    int
	Window time.Duration
	Now    func() time.Time

	mu       sync.Mutex
	attempts map[string][]time.Time
}

// NewLoginLimiter returns v1's five-failures-per-minute limiter.
func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{Max: 5, Window: time.Minute}
}

func (l *LoginLimiter) now() time.Time {
	if l != nil && l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Check reports whether key may attempt a login. When blocked, reset is the
// moment the oldest counted failure leaves the window.
func (l *LoginLimiter) Check(key string) (bool, time.Time) {
	if l == nil {
		return true, time.Time{}
	}
	max := l.Max
	if max <= 0 {
		max = 5
	}
	window := l.Window
	if window <= 0 {
		window = time.Minute
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.attempts == nil {
		l.attempts = map[string][]time.Time{}
	}
	l.pruneLocked(now, window)
	if len(l.attempts[key]) >= max {
		return false, l.attempts[key][0].Add(window)
	}
	return true, time.Time{}
}

func (l *LoginLimiter) pruneLocked(now time.Time, window time.Duration) {
	for candidate, attempts := range l.attempts {
		kept := attempts[:0]
		for _, at := range attempts {
			if now.Sub(at) < window {
				kept = append(kept, at)
			}
		}
		if len(kept) == 0 {
			delete(l.attempts, candidate)
			continue
		}
		l.attempts[candidate] = kept
	}
}

// Fail records one failed login.
func (l *LoginLimiter) Fail(key string) {
	if l == nil {
		return
	}
	window := l.Window
	if window <= 0 {
		window = time.Minute
	}
	now := l.now()
	l.mu.Lock()
	if l.attempts == nil {
		l.attempts = map[string][]time.Time{}
	}
	l.pruneLocked(now, window)
	l.attempts[key] = append(l.attempts[key], now)
	l.mu.Unlock()
}

// Clear forgets failures after a successful login.
func (l *LoginLimiter) Clear(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.attempts, key)
	l.mu.Unlock()
}

// LoginClientKey implements v1 login_rate_limit_key: the peer is authoritative
// unless it is a loopback reverse proxy, in which case X-Real-IP then the first
// X-Forwarded-For hop supplies the real client.
func LoginClientKey(peerIP string, headers http.Header) string {
	peerIP = strings.TrimSpace(peerIP)
	ip := net.ParseIP(peerIP)
	if ip == nil || !ip.IsLoopback() {
		return peerIP
	}
	if real := proxyHeaderIP(headers.Get("X-Real-IP")); real != "" {
		return real
	}
	forwarded := headers.Get("X-Forwarded-For")
	first, _, _ := strings.Cut(forwarded, ",")
	if via := proxyHeaderIP(first); via != "" {
		return via
	}
	return peerIP
}

func proxyHeaderIP(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 || strings.Contains(value, ",") {
		return ""
	}
	if strings.HasPrefix(value, "[") {
		if end := strings.Index(value, "]"); end > 0 {
			value = value[1:end]
		} else {
			return ""
		}
	} else if strings.Count(value, ":") == 1 {
		host, port, found := strings.Cut(value, ":")
		if found {
			if _, err := strconv.Atoi(port); err == nil {
				value = host
			}
		}
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return ""
	}
	return ip.String()
}
