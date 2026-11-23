package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/api"
	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

func TestPrefixAffinityAcrossProtocols(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"intl/deepseek-v4.1-flash","messages":[{"role":"system","content":"private rules"},{"role":"user","content":"private first"}]}`},
		{"responses", "/v1/responses", `{"model":"intl/deepseek-v4.1-flash","input":[{"type":"message","role":"system","content":"private rules"},{"type":"message","role":"user","content":"private first"}]}`},
		{"messages", "/v1/messages", `{"model":"intl/deepseek-v4.1-flash","system":"private rules","messages":[{"role":"user","content":"private first"}],"max_tokens":16}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WB_ENABLE_ANTHROPIC_MESSAGES", "1")
			dir := t.TempDir()
			for _, uid := range []string{"a", "b", "c"} {
				writeChatAccount(t, dir, uid, realmIntl, map[string]any{"product": productDesktop})
			}
			p := setupChatProvider(t, realmIntl, dir, map[string]any{})
			fake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
			p.chatBaseURL = newChatTestServer(t, fake).URL
			var records []usage.RecordInput
			s := api.New(api.ServerOptions{Providers: []providers.Provider{p}, APIKeyAuth: func(h http.Handler) http.Handler { return h }, RecordUsage: func(_ context.Context, in usage.RecordInput) error { records = append(records, in); return nil }})
			for i := 0; i < 2; i++ {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				s.Handler().ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
			}
			calls := fake.snapshot()
			if len(calls) != 2 || calls[0].Header.Get("Authorization") != calls[1].Header.Get("Authorization") {
				t.Fatalf("protocol lost affinity: %+v", calls)
			}
			if len(records) != 2 || records[0].PrefixSHA == "" || records[0].PrefixSHA != records[1].PrefixSHA || records[1].AffinityMode != "prefix" {
				t.Fatalf("routing diagnostics lost: %+v", records)
			}
			blob, _ := json.Marshal(records)
			if strings.Contains(string(blob), "private first") || strings.Contains(string(blob), "private rules") {
				t.Fatalf("prompt leaked: %s", blob)
			}
		})
	}
}

func TestAffinityPolicyPreservesAccountRestrictions(t *testing.T) {
	dir := t.TempDir()
	for _, uid := range []string{"a", "b", "c", "d"} {
		writeChatAccount(t, dir, uid, realmIntl, nil)
	}
	p := setupChatProvider(t, realmIntl, dir, nil).Pool()
	policy := providers.AffinityPolicy{Cohort: []string{"a", "b", "c"}, Rotate: true, Scope: "test"}
	// Excluded preferred members cannot be resurrected; fallback stays inside
	// existing account authorization and health checks.
	a, err := p.Pick("deepseek-v4.1-flash", "", map[string]bool{"a": true, "b": true, "c": true}, policy)
	if err != nil || a.UID() != "d" {
		t.Fatalf("fallback: %v %v", a, err)
	}
	_, err = p.Pick("deepseek-v4.1-flash", "", map[string]bool{"a": true, "b": true, "c": true, "d": true}, policy)
	if err == nil {
		t.Fatal("allow-list bypassed")
	}
	if err := p.NoteRestricted(p.Get("a"), "request illegal"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		a, err := p.Pick("deepseek-v4.1-flash", "", nil, policy)
		if err != nil || a.UID() == "a" {
			t.Fatalf("restricted account selected: %v %v", a, err)
		}
	}
}
