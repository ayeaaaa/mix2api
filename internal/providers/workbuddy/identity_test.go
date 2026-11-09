package workbuddy

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/core/config"
)

func TestIdentityRealmEndpoints(t *testing.T) {
	t.Setenv("WB_CN_CHAT_UPSTREAM", "")

	cases := []struct {
		realm   string
		product string
		want    string
		domain  string
	}{
		{"intl", "workbuddy", "https://www.workbuddy.ai", "www.workbuddy.ai"},
		{"intl", "vscode", "https://www.workbuddy.ai", "www.workbuddy.ai"},
		{"intl", "cli", "https://www.workbuddy.ai", "www.workbuddy.ai"},
		{"cn", "workbuddy", "https://copilot.tencent.com", "copilot.tencent.com"},
		{"cn", "vscode", "https://www.workbuddy.cn", "www.workbuddy.cn"},
		{"cn", "cli", "https://copilot.tencent.com", "copilot.tencent.com"},
		{"CN", "desktop", "https://copilot.tencent.com", "copilot.tencent.com"},
		{"nonsense", "wb", "https://www.workbuddy.ai", "www.workbuddy.ai"},
	}
	for _, tc := range cases {
		t.Run(tc.realm+"/"+tc.product, func(t *testing.T) {
			if got := endpointFor(tc.realm, tc.product); got != tc.want {
				t.Fatalf("endpointFor(%q, %q) = %q, want %q",
					tc.realm, tc.product, got, tc.want)
			}
			if got := domainFor(tc.realm, tc.product); got != tc.domain {
				t.Fatalf("domainFor(%q, %q) = %q, want %q",
					tc.realm, tc.product, got, tc.domain)
			}
		})
	}
}

func TestIdentityCNUpstreamOverride(t *testing.T) {
	t.Setenv("WB_CN_CHAT_UPSTREAM", " https://www.workbuddy.cn/ ")
	if got := endpointFor("cn", "workbuddy"); got != "https://www.workbuddy.cn" {
		t.Fatalf("cn desktop endpoint = %q", got)
	}
	if got := endpointFor("cn", "cli"); got != "https://www.workbuddy.cn" {
		t.Fatalf("cn cli endpoint = %q", got)
	}
	if got := endpointFor("cn", "vscode"); got != "https://www.workbuddy.cn" {
		t.Fatalf("cn vscode endpoint = %q", got)
	}
	if got := domainFor("cn", "workbuddy"); got != "copilot.tencent.com" {
		t.Fatalf("X-Domain followed the override: %q", got)
	}
	if got := endpointFor("intl", "workbuddy"); got != "https://www.workbuddy.ai" {
		t.Fatalf("intl endpoint changed: %q", got)
	}

	t.Setenv("WB_CN_CHAT_UPSTREAM", "http://127.0.0.1:8080/")
	if got := endpointFor("cn", "workbuddy"); got != "http://127.0.0.1:8080" {
		t.Fatalf("test upstream endpoint = %q", got)
	}

	for _, raw := range []string{
		"www.workbuddy.cn",
		"ftp://www.workbuddy.cn",
		"https://",
		"://bad",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("WB_CN_CHAT_UPSTREAM", raw)
			if got := endpointFor("cn", "workbuddy"); got != "https://copilot.tencent.com" {
				t.Fatalf("invalid override %q became %q", raw, got)
			}
		})
	}
}

func TestIdentityDesktopUAVersions(t *testing.T) {
	t.Setenv("WB_VSCODE_IDE_VERSION", "")
	t.Setenv("WB_IDENTITY_NODE_VERSION", "")

	if got, want := desktopUA("intl"), "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0"; got != want {
		t.Fatalf("desktopUA(intl) = %q, want %q", got, want)
	}
	if got, want := desktopUA("cn"), "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"; got != want {
		t.Fatalf("desktopUA(cn) = %q, want %q", got, want)
	}
	if got := desktopVersion("intl"); got != "5.8.4" {
		t.Fatalf("desktopVersion(intl) = %q", got)
	}
	if got := desktopVersion("cn"); got != "5.8.3" {
		t.Fatalf("desktopVersion(cn) = %q", got)
	}
	if got := desktopVersion("nonsense"); got != "5.8.4" {
		t.Fatalf("desktopVersion(nonsense) = %q", got)
	}

	override := Versions{DesktopIntl: "9.9.9", DesktopCLI: "9.0.0"}
	if got, want := desktopUAWithVersions("intl", override),
		"WorkBuddy/9.9.9 workbuddy-ai/9.9.9 CLI/9.0.0"; got != want {
		t.Fatalf("desktopUAWithVersions() = %q, want %q", got, want)
	}
	if got := desktopVersionWithVersions("intl", override); got != "9.9.9" {
		t.Fatalf("desktopVersionWithVersions() = %q", got)
	}

	if got, want := vscodeUA(), "VSCode/1.119.0 CodeBuddy/4.12.39997873"; got != want {
		t.Fatalf("vscodeUA() = %q, want %q", got, want)
	}
	if got, want := vscodeUAWithVersions(Versions{VSCodeIDE: "2.3.4", VSCodeProduct: "1.2.3"}),
		"VSCode/2.3.4 CodeBuddy/1.2.3"; got != want {
		t.Fatalf("vscodeUAWithVersions() = %q, want %q", got, want)
	}
	t.Setenv("WB_VSCODE_IDE_VERSION", "1.120.0")
	if got, want := vscodeUA(), "VSCode/1.120.0 CodeBuddy/4.12.39997873"; got != want {
		t.Fatalf("env vscode UA = %q, want %q", got, want)
	}
	if got, want := vscodeUAWithVersions(Versions{VSCodeIDE: "2.3.4"}),
		"VSCode/2.3.4 CodeBuddy/4.12.39997873"; got != want {
		t.Fatalf("settings override did not beat env: %q", got)
	}

	if got, want := cliUA(), "CLI/2.168.2 CodeBuddy/2.168.2"; got != want {
		t.Fatalf("cliUA() = %q, want %q", got, want)
	}
	if got, want := cliUAWithVersions(Versions{CLI: "9.8.7"}),
		"CLI/9.8.7 CodeBuddy/9.8.7"; got != want {
		t.Fatalf("cliUAWithVersions() = %q, want %q", got, want)
	}
}

func TestIdentityNormalizeProduct(t *testing.T) {
	cases := map[string]string{
		"":           "workbuddy",
		"workbuddy":  "workbuddy",
		"WorkBuddy":  "workbuddy",
		"wb":         "workbuddy",
		"desktop":    "workbuddy",
		"vscode":     "vscode",
		"vsc":        "vscode",
		"ide":        "vscode",
		"cli":        "cli",
		"codebuddy":  "cli",
		"nonsense":   "workbuddy",
		"  VSCODE  ": "vscode",
	}
	for input, want := range cases {
		if got := normalizeProduct(input); got != want {
			t.Fatalf("normalizeProduct(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIdentityUUIDShapes(t *testing.T) {
	first := newMessageID()
	second := newMessageID()
	if !isHex(first, 32) || !isHex(second, 32) {
		t.Fatalf("message ids are not 32 hex: %q %q", first, second)
	}
	if first == second {
		t.Fatal("two message ids are identical")
	}
	if first[12] != '7' {
		t.Fatalf("message id is not UUIDv7: %q", first)
	}
	if !isHexByte(first[16], "89ab") {
		t.Fatalf("message id variant = %q", first[16])
	}
	if got := newTurnRequestID(); !isHex(got, 32) {
		t.Fatalf("turn request id = %q", got)
	}

	conv := newConversationID()
	if !isDashedUUID(conv) {
		t.Fatalf("conversation id = %q", conv)
	}
	if conv[14] != '7' || !isHexByte(conv[19], "89ab") {
		t.Fatalf("conversation id version/variant = %q", conv)
	}

	trace := newRequestTraceID()
	if !isDashedUUID(trace) || trace[14] != '4' || !isHexByte(trace[19], "89ab") {
		t.Fatalf("request trace id = %q", trace)
	}

	stable := conversationIDFor("uid-1", "session-1")
	if !isDashedUUID(stable) || stable[14] != '4' || stable[19] != 'a' {
		t.Fatalf("stable conversation id = %q", stable)
	}
	if got := conversationIDFor("uid-1", "session-1"); got != stable {
		t.Fatalf("stable conversation id changed: %q != %q", got, stable)
	}
	if got := conversationIDFor("uid-2", "session-1"); got == stable {
		t.Fatalf("different uid produced the same id: %q", got)
	}
	if got := conversationIDFor("uid-1", "session-2"); got == stable {
		t.Fatalf("different session produced the same id: %q", got)
	}
	if got := conversationIDFor("uid-1", "   "); got != "" {
		t.Fatalf("empty session key produced %q", got)
	}
}

func TestIdentityDesktopHeaders(t *testing.T) {
	t.Setenv("WB_DESKTOP_CHAT_COMPAT", "")

	h := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "workbuddy", UID: "uid-1", Token: "tok-1",
	})

	for name, want := range map[string]string{
		"X-Agent-Intent":    "craft",
		"X-Agent-Purpose":   "conversation",
		"X-Agent-Type":      "main",
		"X-IDE-Name":        "WorkBuddy",
		"X-IDE-Type":        "WorkBuddy",
		"X-IDE-Version":     "5.8.4",
		"X-Private-Data":    "true",
		"X-Product":         "SaaS",
		"X-Domain":          "www.workbuddy.ai",
		"User-Agent":        "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0",
		"X-User-Id":         "uid-1",
		"Authorization":     "Bearer tok-1",
		"X-Root-Request-ID": h.Get("X-Conversation-Request-ID"),
	} {
		if got := h.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if h.Get("X-Request-ID") != h.Get("X-Conversation-Message-ID") {
		t.Fatal("desktop X-Request-ID must equal X-Conversation-Message-ID")
	}
	if !isDashedUUID(h.Get("X-Conversation-ID")) {
		t.Fatalf("desktop conversation id = %q", h.Get("X-Conversation-ID"))
	}
	if !isHex(h.Get("X-Conversation-Request-ID"), 32) ||
		!isHex(h.Get("X-Conversation-Message-ID"), 32) {
		t.Fatalf("desktop request ids = %q/%q",
			h.Get("X-Conversation-Request-ID"), h.Get("X-Conversation-Message-ID"))
	}
	for _, absent := range []string{
		"X-Product-Version", "X-Request-Trace-Id", "X-Env-ID", "X-Model-ID",
		"X-B3-ParentSpanId",
	} {
		if got := h.Get(absent); got != "" {
			t.Fatalf("desktop unexpectedly sent %s = %q", absent, got)
		}
	}
	if b3 := strings.Split(h.Get("b3"), "-"); len(b3) != 3 {
		t.Fatalf("desktop b3 = %q, want three segments", h.Get("b3"))
	}
	if got := h.Get("traceparent"); !strings.HasPrefix(got, "00-") || !strings.HasSuffix(got, "-01") {
		t.Fatalf("desktop traceparent = %q", got)
	}
	if got := h.Get("X-Trace-ID"); !isHex(got, 32) {
		t.Fatalf("X-Trace-ID = %q", got)
	}

	cn := buildIdentityHeaders(IdentityOptions{
		Realm: "cn", Product: "workbuddy", UID: "uid-1", Token: "tok-1",
	})
	if cn.Get("X-IDE-Version") != "5.8.3" ||
		cn.Get("User-Agent") != "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0" ||
		cn.Get("X-Domain") != "copilot.tencent.com" {
		t.Fatalf("cn desktop identity = %#v", cn)
	}

	key := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "workbuddy", UID: "uid-1", Token: "ck_abc.secret",
	})
	if key.Get("X-API-Key") != "ck_abc.secret" {
		t.Fatalf("static key did not add X-API-Key: %#v", key)
	}
}

func TestIdentityDesktopChatCompatProfile(t *testing.T) {
	t.Setenv("WB_DESKTOP_CHAT_COMPAT", "true")
	legacy := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "workbuddy", UID: "uid-1", Token: "tok",
	})
	if legacy.Get("X-Stainless-Runtime") != "" {
		t.Fatal("only the literal value 1 may enable the compat profile")
	}

	t.Setenv("WB_DESKTOP_CHAT_COMPAT", "1")
	t.Setenv("WB_IDENTITY_OS", "Linux")
	t.Setenv("WB_IDENTITY_ARCH", "arm64")
	h := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "workbuddy", UID: "uid-1", Token: "tok",
		Versions: Versions{SDK: "9.1.0", Node: "v23.0.0"},
	})
	for name, want := range map[string]string{
		"x-stainless-lang":            "js",
		"x-stainless-package-version": "9.1.0",
		"x-stainless-os":              "Linux",
		"x-stainless-arch":            "arm64",
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": "v23.0.0",
		"x-stainless-retry-count":     "0",
	} {
		if got := h.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if h.Get("x-stainless-timeout") != "" {
		t.Fatal("desktop compat profile must not copy the CLI timeout budget")
	}
	b3 := strings.Split(h.Get("b3"), "-")
	if len(b3) != 4 {
		t.Fatalf("compat b3 = %q, want four segments", h.Get("b3"))
	}
	if h.Get("X-B3-ParentSpanId") != b3[3] {
		t.Fatalf("b3 parent %q != X-B3-ParentSpanId %q", b3[3], h.Get("X-B3-ParentSpanId"))
	}
	if !strings.HasPrefix(h.Get("traceparent"), "00-") {
		t.Fatalf("compat traceparent = %q", h.Get("traceparent"))
	}
}

func TestIdentityCLIHeaders(t *testing.T) {
	t.Setenv("WB_IDENTITY_OS", "Linux")
	t.Setenv("WB_IDENTITY_ARCH", "x64")
	t.Setenv("WB_IDENTITY_NODE_VERSION", "")

	h := buildIdentityHeaders(IdentityOptions{
		Realm: "cn", Product: "cli", UID: "uid-1", Token: "tok-1",
		SessionKey: "session-1", TurnRequestID: "turn-1",
	})
	for name, want := range map[string]string{
		"X-Agent-Intent":            "craft",
		"X-Agent-Purpose":           "conversation",
		"X-Agent-Runtime":           "other",
		"X-Agent-Type":              "main",
		"X-IDE-Type":                "CLI",
		"X-IDE-Name":                "CLI",
		"X-IDE-Version":             "2.168.2",
		"X-Product":                 "SaaS",
		"X-Domain":                  "copilot.tencent.com",
		"User-Agent":                "CLI/2.168.2 CodeBuddy/2.168.2",
		"X-Conversation-ID":         conversationIDFor("uid-1", "session-1"),
		"X-Conversation-Request-ID": "turn-1",
		"X-Root-Request-ID":         "turn-1",
		"X-Private-Data":            "false",
		"X-User-Id":                 "uid-1",
		"Authorization":             "Bearer tok-1",
	} {
		if got := h.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if h.Get("X-Request-ID") != h.Get("X-Conversation-Message-ID") {
		t.Fatal("CLI X-Request-ID must equal X-Conversation-Message-ID")
	}
	if !isHex(h.Get("X-Request-ID"), 32) {
		t.Fatalf("CLI message id = %q", h.Get("X-Request-ID"))
	}
	if h.Get("X-Conversation-ID") == h.Get("X-Conversation-Request-ID") {
		t.Fatal("CLI conversation and turn request ids must be distinct")
	}
	if got := h.Get("b3"); len(strings.Split(got, "-")) != 4 {
		t.Fatalf("CLI b3 = %q", got)
	}
	if !strings.HasPrefix(h.Get("traceparent"), "00-") {
		t.Fatalf("CLI traceparent = %q", h.Get("traceparent"))
	}
	for name, want := range map[string]string{
		"x-stainless-lang":            "js",
		"x-stainless-package-version": "6.25.0",
		"x-stainless-os":              "Linux",
		"x-stainless-arch":            "x64",
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": "v22.21.1",
		"x-stainless-retry-count":     "0",
		"x-stainless-timeout":         "1200",
	} {
		if got := h.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}

	explicit := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "cli", UID: "uid-1", Token: "ck_secret.value",
		SessionKey: "session-1", ConversationID: "explicit-conv",
		TurnRequestID: "turn-explicit", EnterpriseID: "ent-1",
		Department: "dept-1", PrivateData: "false",
	})
	if explicit.Get("X-Conversation-ID") != "explicit-conv" {
		t.Fatalf("explicit conversation id = %q", explicit.Get("X-Conversation-ID"))
	}
	if explicit.Get("X-Conversation-Request-ID") != "turn-explicit" ||
		explicit.Get("X-Root-Request-ID") != "turn-explicit" {
		t.Fatalf("turn request id was not reused: %#v", explicit)
	}
	if explicit.Get("X-Private-Data") != "false" {
		t.Fatalf("private data override = %q", explicit.Get("X-Private-Data"))
	}
	if explicit.Get("X-Enterprise-Id") != "ent-1" ||
		explicit.Get("X-Tenant-Id") != "ent-1" ||
		explicit.Get("X-Department-Info") != "dept-1" {
		t.Fatalf("enterprise headers = %#v", explicit)
	}
	if explicit.Get("X-API-Key") != "ck_secret.value" {
		t.Fatalf("static key did not add X-API-Key: %#v", explicit)
	}
	if got := explicit.Get("X-Private-Data"); got != "false" {
		t.Fatalf("explicit private data = %q", got)
	}
}

func TestIdentityVSCodeHeaders(t *testing.T) {
	h := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "vscode", UID: "uid-1", Token: "tok-1",
		Model: "deepseek-v4.1-flash",
	})
	for name, want := range map[string]string{
		"X-Agent-Intent":    "craft",
		"X-Agent-Type":      "main",
		"X-IDE-Type":        "VSCode",
		"X-IDE-Name":        "VSCode",
		"X-IDE-Version":     "1.119.0",
		"X-Product-Version": "4.12.39997873",
		"X-Env-ID":          "production",
		"X-Product":         "SaaS",
		"X-Domain":          "www.workbuddy.ai",
		"User-Agent":        "VSCode/1.119.0 CodeBuddy/4.12.39997873",
		"X-Model-ID":        "deepseek-v4.1-flash",
		"X-Private-Data":    "true",
		"X-User-Id":         "uid-1",
		"Authorization":     "Bearer tok-1",
	} {
		if got := h.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if h.Get("X-Request-ID") != h.Get("X-Conversation-Request-ID") {
		t.Fatal("VSCode X-Request-ID must equal X-Conversation-Request-ID")
	}
	if h.Get("X-Conversation-Message-ID") == h.Get("X-Request-ID") {
		t.Fatal("VSCode message id must be independent")
	}
	if !isHex(h.Get("X-Conversation-ID"), 32) ||
		!isHex(h.Get("X-Conversation-Message-ID"), 32) {
		t.Fatalf("VSCode ids = %q/%q",
			h.Get("X-Conversation-ID"), h.Get("X-Conversation-Message-ID"))
	}
	if got := h.Get("X-Root-Request-ID"); got != "" {
		t.Fatalf("VSCode main chat sent X-Root-Request-ID = %q", got)
	}
	for _, absent := range []string{
		"X-Agent-Purpose", "X-Agent-Runtime", "traceparent", "X-Device-Token",
	} {
		if got := h.Get(absent); got != "" {
			t.Fatalf("VSCode unexpectedly sent %s = %q", absent, got)
		}
	}
	if !isDashedUUID(h.Get("X-Request-Trace-Id")) ||
		h.Get("X-Request-Trace-Id")[14] != '4' {
		t.Fatalf("X-Request-Trace-Id = %q", h.Get("X-Request-Trace-Id"))
	}
	if b3 := strings.Split(h.Get("b3"), "-"); len(b3) != 4 ||
		b3[3] != h.Get("X-B3-ParentSpanId") {
		t.Fatalf("VSCode b3 = %q parent = %q", h.Get("b3"), h.Get("X-B3-ParentSpanId"))
	}
	if got := h.Get("monitor_promptPrepareStartTime"); !isDigits(got) {
		t.Fatalf("monitor prompt timestamp = %q", got)
	}
	if got := h.Get("monitor_httpSendTime"); !isDigits(got) {
		t.Fatalf("monitor send timestamp = %q", got)
	}

	cn := buildIdentityHeaders(IdentityOptions{
		Realm: "cn", Product: "vscode", UID: "uid-1", Token: "tok-1",
	})
	if cn.Get("X-Private-Data") != "" || cn.Get("X-Domain") != "www.workbuddy.cn" {
		t.Fatalf("cn VSCode identity = %#v", cn)
	}
	cnPrivate := buildIdentityHeaders(IdentityOptions{
		Realm: "cn", Product: "vscode", UID: "uid-1", Token: "tok-1",
		PrivateData: "true",
	})
	if cnPrivate.Get("X-Private-Data") != "true" {
		t.Fatal("explicit private-data override was ignored on CN VSCode")
	}
	intlPrivate := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "vscode", UID: "uid-1", Token: "tok-1",
		PrivateData: "false",
	})
	if intlPrivate.Get("X-Private-Data") != "false" {
		t.Fatal("explicit private-data override was ignored on intl VSCode")
	}
	explicit := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "vscode", UID: "uid-1", Token: "tok-1",
		ConversationID: "conv-explicit",
	})
	if explicit.Get("X-Conversation-ID") != "conv-explicit" {
		t.Fatalf("explicit conversation id = %q", explicit.Get("X-Conversation-ID"))
	}
	noModel := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "vscode", UID: "uid-1", Token: "tok-1",
	})
	if noModel.Get("X-Model-ID") != "" ||
		noModel.Get("monitor_promptPrepareStartTime") != "" {
		t.Fatal("VSCode model headers appeared without a model")
	}
}

func TestIdentityDeviceTokenRealmRules(t *testing.T) {
	cn := buildIdentityHeaders(IdentityOptions{
		Realm: "cn", Product: "workbuddy", UID: "u", Token: "t",
		DeviceToken: "device-1",
	})
	if cn.Get("X-Device-Token") != "device-1" {
		t.Fatalf("cn device token = %q", cn.Get("X-Device-Token"))
	}
	intl := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "workbuddy", UID: "u", Token: "t",
		DeviceToken: "device-1",
	})
	if got := intl.Get("X-Device-Token"); got != "" {
		t.Fatalf("intl device token = %q, want omitted", got)
	}
	cli := buildIdentityHeaders(IdentityOptions{
		Realm: "cn", Product: "cli", UID: "u", Token: "t",
		DeviceToken: "device-1",
	})
	if got := cli.Get("X-Device-Token"); got != "" {
		t.Fatalf("CLI device token = %q, want omitted", got)
	}
	vsc := buildIdentityHeaders(IdentityOptions{
		Realm: "cn", Product: "vscode", UID: "u", Token: "t",
		DeviceTokenError: "sdk_not_initialized",
	})
	if vsc.Get("X-Device-Token-Error") != "sdk_not_initialized" {
		t.Fatalf("vscode device-token error = %q", vsc.Get("X-Device-Token-Error"))
	}
}

func TestIdentityHeadersKeepTokenOutOfNonAuthHeaders(t *testing.T) {
	token := "sk-test-SECRET-123"
	for _, product := range []string{"workbuddy", "vscode", "cli"} {
		t.Run(product, func(t *testing.T) {
			h := buildIdentityHeaders(IdentityOptions{
				Realm: "intl", Product: product, UID: "uid-1", Token: token,
				Model: "model-1", SessionKey: "session-1",
				TurnRequestID: "turn-1",
			})
			if got := h.Get("Authorization"); got != "Bearer "+token {
				t.Fatalf("Authorization = %q", got)
			}
			for name, values := range h {
				if name == "Authorization" || name == "X-Api-Key" {
					continue
				}
				for _, value := range values {
					if strings.Contains(value, token) {
						t.Fatalf("%s leaked the token: %q", name, value)
					}
				}
			}
		})
	}
}

func TestIdentityGzipBodyIsDeterministic(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	first, err := gzipBody(payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := gzipBody(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("gzipBody is not deterministic")
	}
	// gzip header: magic, method, flags, mtime (4 bytes), xfl, os.
	if len(first) < 10 || first[0] != 0x1f || first[1] != 0x8b {
		t.Fatalf("not a gzip stream: %x", first)
	}
	if !bytes.Equal(first[4:8], []byte{0, 0, 0, 0}) {
		t.Fatalf("gzip mtime = %x, want zero", first[4:8])
	}

	zr, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if !zr.Header.ModTime.IsZero() {
		t.Fatalf("gzip header mtime = %v, want zero", zr.Header.ModTime)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("gzip body round-trip = %q, want %q", got, payload)
	}
}

func TestIdentityStainlessEnvironmentOverrides(t *testing.T) {
	t.Setenv("WB_IDENTITY_OS", "Windows")
	t.Setenv("WB_IDENTITY_ARCH", "ia32")
	t.Setenv("WB_IDENTITY_NODE_VERSION", "v20.0.0")
	t.Setenv("WB_DESKTOP_CHAT_COMPAT", "1")

	cli := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "cli", UID: "u", Token: "t",
	})
	if cli.Get("x-stainless-os") != "Windows" ||
		cli.Get("x-stainless-arch") != "ia32" ||
		cli.Get("x-stainless-runtime-version") != "v20.0.0" {
		t.Fatalf("CLI stainless env overrides = %#v", cli)
	}
	desktop := buildIdentityHeaders(IdentityOptions{
		Realm: "intl", Product: "workbuddy", UID: "u", Token: "t",
		Versions: Versions{Node: "v23.0.0"},
	})
	if got := desktop.Get("x-stainless-runtime-version"); got != "v23.0.0" {
		t.Fatalf("settings node override = %q, want v23.0.0", got)
	}
	if got := desktop.Get("x-stainless-os"); got != "Windows" {
		t.Fatalf("desktop stainless os = %q", got)
	}
}

func TestProviderNewRealmAndIdentityHelpers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	body := `{"workbuddy":{"intl":{"identity_versions":{"desktop_intl":"9.9.9","sdk":"7.0.0"}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store := config.New(path)

	p, err := New("intl", store)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID() != "workbuddy-intl" || p.Realm() != "intl" {
		t.Fatalf("provider identity = %q/%q", p.ID(), p.Realm())
	}
	if got, want := p.DesktopUA(), "WorkBuddy/9.9.9 workbuddy-ai/9.9.9 CLI/2.161.0"; got != want {
		t.Fatalf("Provider.DesktopUA() = %q, want %q", got, want)
	}
	if got := p.DesktopVersion(); got != "9.9.9" {
		t.Fatalf("Provider.DesktopVersion() = %q", got)
	}
	if got := p.EndpointFor("workbuddy"); got != "https://www.workbuddy.ai" {
		t.Fatalf("Provider.EndpointFor() = %q", got)
	}
	if got := p.DomainFor("workbuddy"); got != "www.workbuddy.ai" {
		t.Fatalf("Provider.DomainFor() = %q", got)
	}
	h := p.IdentityHeaders(IdentityOptions{
		Product: "cli", UID: "u", Token: "t", SessionKey: "s",
	})
	if got := h.Get("x-stainless-package-version"); got != "7.0.0" {
		t.Fatalf("Provider.IdentityHeaders() SDK = %q", got)
	}
	if got := h.Get("X-Conversation-ID"); got != conversationIDFor("u", "s") {
		t.Fatalf("Provider.IdentityHeaders() conversation = %q", got)
	}

	cn, err := New("cn", store)
	if err != nil {
		t.Fatal(err)
	}
	if cn.ID() != "workbuddy-cn" || cn.Realm() != "cn" {
		t.Fatalf("cn provider identity = %q/%q", cn.ID(), cn.Realm())
	}
	if got := cn.EndpointFor("workbuddy"); got != "https://copilot.tencent.com" {
		t.Fatalf("cn Provider.EndpointFor() = %q", got)
	}
	if got := cn.DomainFor("vscode"); got != "www.workbuddy.cn" {
		t.Fatalf("cn Provider.DomainFor(vscode) = %q", got)
	}

	if _, err := New("eu", store); err == nil {
		t.Fatal("New accepted an invalid realm")
	}
	if _, err := New("intl", nil); err == nil {
		t.Fatal("New accepted a nil store")
	}
}

func TestProviderPrivateDataDefaultFlowsIntoHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	body := `{"workbuddy":{"intl":{"private_data":"false"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New("intl", config.New(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, product := range []string{"workbuddy", "vscode"} {
		h := p.IdentityHeaders(IdentityOptions{
			Product: product, UID: "u", Token: "t",
		})
		if got := h.Get("X-Private-Data"); got != "false" {
			t.Fatalf("%s private data = %q, want false", product, got)
		}
	}
}

func TestIdentityVersionsFromSettingsAreValidated(t *testing.T) {
	valid := Versions{
		DesktopIntl: "9.9.9", DesktopCN: "9.9.8", DesktopCLI: "3.0.0",
		VSCodeProduct: "1.2.3", VSCodeIDE: "2.3.4", CLI: "4.5.6",
		SDK: "7.8.9", Node: "v9.9.9",
	}
	got := versionsFromAny(map[string]any{
		"desktop_intl": "9.9.9", "desktop_cn": "9.9.8", "desktop_cli": "3.0.0",
		"vscode_product": "1.2.3", "vscode_ide": "2.3.4", "cli": "4.5.6",
		"sdk": "7.8.9", "node": "v9.9.9", "unknown": "ignored",
	})
	if got != valid {
		t.Fatalf("versionsFromAny() = %#v, want %#v", got, valid)
	}
	if got := versionsFromAny(map[string]any{"desktop_intl": "9 9"}); got != (Versions{}) {
		t.Fatalf("invalid version set = %#v, want empty overrides", got)
	}
	if got := versionsFromAny(map[string]any{"desktop_intl": "x" + strings.Repeat("x", 64)}); got != (Versions{}) {
		t.Fatalf("overlong version set = %#v, want empty overrides", got)
	}
}

func TestIdentityVersionSettingsRoundTrip(t *testing.T) {
	store := config.New(filepath.Join(t.TempDir(), "settings.json"))
	if _, err := SaveConfig(store, "cn", map[string]any{
		"identity_versions": map[string]any{"desktop_cn": "9.9.9", "node": "v23.0.0"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := LoadConfig(store, "cn")
	resolved := cfg.Versions()
	if resolved.DesktopCN != "9.9.9" || resolved.Node != "v23.0.0" {
		t.Fatalf("resolved versions = %#v", resolved)
	}
	if resolved.DesktopIntl != "5.8.4" || resolved.CLI != "2.168.2" {
		t.Fatalf("defaults were not filled: %#v", resolved)
	}
	// The stored config must contain only overrides, not a frozen copy of every
	// verified default.
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	section := settings["workbuddy"].(map[string]any)
	cn := section["cn"].(map[string]any)
	versions := cn["identity_versions"].(map[string]any)
	if len(versions) != 2 || versions["desktop_cn"] != "9.9.9" || versions["node"] != "v23.0.0" {
		t.Fatalf("stored identity_versions = %#v", versions)
	}
}

func isHex(value string, n int) bool {
	if len(value) != n {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isDashedUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' ||
		value[18] != '-' || value[23] != '-' {
		return false
	}
	return isHex(strings.ReplaceAll(value, "-", ""), 32)
}

func isHexByte(value byte, allowed string) bool {
	for i := 0; i < len(allowed); i++ {
		if value == allowed[i] {
			return true
		}
	}
	return false
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
