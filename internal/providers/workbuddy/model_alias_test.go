package workbuddy

import (
	"context"
	"net/http"
	"testing"
)

func TestIntlModelAliasScopeAndEscapeHatch(t *testing.T) {
	t.Setenv("WB_INTL_MODEL_ALIAS", "1")
	if got := intlModelAlias("deepseek-v4.1-flash", realmIntl); got != "deepseek-v4.1-flash-sg" {
		t.Fatalf("intl alias = %q", got)
	}
	if got := intlModelAlias("deepseek-v4.1-flash", realmCN); got != "deepseek-v4.1-flash" {
		t.Fatalf("cn alias = %q", got)
	}
	if got := intlModelAlias("glm-5.2", realmIntl); got != "glm-5.2" {
		t.Fatalf("unrelated model = %q", got)
	}
	t.Setenv("WB_INTL_MODEL_ALIAS", "0")
	if got := intlModelAlias("deepseek-v4.1-flash", realmIntl); got != "deepseek-v4.1-flash" {
		t.Fatalf("disabled alias = %q", got)
	}
}

func TestChatUsesIntlAliasOutboundOnly(t *testing.T) {
	t.Setenv("WB_INTL_MODEL_ALIAS", "1")
	dir := t.TempDir()
	writeChatAccount(t, dir, "intl-alias", realmIntl, map[string]any{
		"product": productDesktop, "platform": "CLI",
	})
	provider := setupChatProvider(t, realmIntl, dir, map[string]any{})
	fake := &chatTestFake{body: ssePayload(chatChunk("hello", "stop"), "[DONE]")}
	server := newChatTestServer(t, fake)
	provider.chatBaseURL = server.URL

	raw := map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	}
	stream, err := provider.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", raw))
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()
	calls := fake.snapshot()
	if len(calls) != 1 || calls[0].Method != http.MethodPost {
		t.Fatalf("calls = %#v", calls)
	}
	body := decodeChatBody(t, calls[0])
	if body["model"] != "deepseek-v4.1-flash-sg" {
		t.Fatalf("outbound model = %#v, want -sg", body["model"])
	}
	usage := chatStreamUsage(t, stream)
	if usage.Model != "deepseek-v4.1-flash-sg" {
		t.Fatalf("usage billed model = %q", usage.Model)
	}

	// CN keeps the logical id even when the alias switch is on.
	cnDir := t.TempDir()
	writeChatAccount(t, cnDir, "cn-alias", realmCN, map[string]any{
		"product": productDesktop, "platform": "CLI",
	})
	cn := setupChatProvider(t, realmCN, cnDir, map[string]any{})
	cnFake := &chatTestFake{body: ssePayload(chatChunk("ok", "stop"), "[DONE]")}
	cn.chatBaseURL = newChatTestServer(t, cnFake).URL
	cnStream, err := cn.OpenChat(context.Background(), chatRequest(t, "deepseek-v4.1-flash", map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	}))
	if err != nil {
		t.Fatalf("CN OpenChat: %v", err)
	}
	defer cnStream.Close()
	cnBody := decodeChatBody(t, cnFake.snapshot()[0])
	if cnBody["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("CN outbound model = %#v", cnBody["model"])
	}
}
