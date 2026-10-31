package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/pool"
)

func adminTestHandler(t *testing.T, apiKey string) (*Handler, string, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()
	a := auth.New("u1", "user", "domain", "token", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	if err := auth.SaveNew(dir, a); err != nil {
		t.Fatal(err)
	}
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{
		Pool:          p,
		APIKey:        apiKey,
		AuthDir:       dir,
		ConvStateFile: filepath.Join(dir, "chats.json"),
	})
	return h, dir, p
}

func TestAdminDeleteAccount(t *testing.T) {
	h, dir, p := adminTestHandler(t, "test-key")
	h.chats["u1"] = "chat-1"
	h.convAcct["conv-1"] = "u1"
	h.convAcct["conv-2"] = "u2"

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/delete",
		strings.NewReader(`{"uid":"u1"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(p.List()) != 0 {
		t.Fatal("account still in pool")
	}
	if auth.Exists(dir, "u1") {
		t.Fatal("credential file still exists")
	}
	if _, ok := h.chats["u1"]; ok {
		t.Fatal("default chat mapping not cleaned")
	}
	if _, ok := h.convAcct["conv-1"]; ok {
		t.Fatal("conversation affinity not cleaned")
	}
	if _, ok := h.convAcct["conv-2"]; !ok {
		t.Fatal("unrelated conversation affinity was removed")
	}
}

func TestAdminDeleteUnknownAccount(t *testing.T) {
	h, _, _ := adminTestHandler(t, "test-key")
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/delete",
		strings.NewReader(`{"uid":"nope"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestAdminDeleteRequiresAPIKey(t *testing.T) {
	h, _, _ := adminTestHandler(t, "")
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/delete",
		strings.NewReader(`{"uid":"u1"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestAdminDeleteRejectsWrongKey(t *testing.T) {
	h, _, _ := adminTestHandler(t, "test-key")
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/delete",
		strings.NewReader(`{"uid":"u1"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}
