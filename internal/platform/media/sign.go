package media

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var unsafeTokenPattern = regexp.MustCompile(`[^0-9A-Za-z_-]+`)

func safeToken(value string) string {
	clean := unsafeTokenPattern.ReplaceAllString(strings.TrimSpace(value), "-")
	if len(clean) > 48 {
		clean = clean[:48]
	}
	if clean == "" {
		return "task"
	}
	return clean
}

// Sign returns exp.mac over safeTaskID|index|exp. The process secret means old
// links stop working after a restart, matching the task table lifetime.
func (s *TaskStore) Sign(id string, index int, ttl time.Duration) string {
	if s == nil {
		return ""
	}
	if ttl == 0 {
		ttl = DefaultSignTTL
	}
	exp := s.now().Add(ttl).Unix()
	message := safeToken(id) + "|" + strconv.Itoa(index) + "|" + strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(message))
	sum := hex.EncodeToString(mac.Sum(nil))
	return strconv.FormatInt(exp, 10) + "." + sum[:32]
}

// Verify checks signature and expiry with constant-time comparison.
func (s *TaskStore) Verify(id string, index int, token string) bool {
	if s == nil {
		return false
	}
	token = strings.TrimSpace(token)
	expText, suppliedMAC, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expText, 10, 64)
	if err != nil || exp < s.now().Unix() {
		return false
	}
	message := safeToken(id) + "|" + strconv.Itoa(index) + "|" + strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(message))
	expected := hex.EncodeToString(mac.Sum(nil))[:32]
	return hmac.Equal([]byte(expected), []byte(suppliedMAC))
}

// ContentPath returns an existing artifact for the task/index.
func (s *TaskStore) ContentPath(id string, index int) (string, bool) {
	task, ok := s.Get(id)
	if !ok || index < 0 || index >= len(task.Videos) {
		return "", false
	}
	path := task.Videos[index]
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	return path, true
}
