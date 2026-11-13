package workbuddy

import (
	"fmt"
	"strings"
	"testing"
)

func TestWebBrowserProfileIsStablePerAccount(t *testing.T) {
	first := webProfile("uid-1")
	again := webProfile("uid-1")
	if first != again {
		t.Fatalf("profile for one uid changed: %+v vs %+v", first, again)
	}
	if first.UA == "" || first.Platform == "" || first.Brand == "" {
		t.Fatalf("incomplete profile: %+v", first)
	}
	seen := map[string]bool{}
	for index := 0; index < 16; index++ {
		seen[webProfile(fmt.Sprintf("uid-%d", index)).UA] = true
	}
	if len(seen) < 2 {
		t.Fatal("all uids selected the same browser profile")
	}
}

func TestWebBrowserHeadersMatchProfileAndRealm(t *testing.T) {
	headers := webBrowserHeaders("uid-1", realmIntl)
	profile := webProfile("uid-1")
	if got := headers.Get("User-Agent"); got != profile.UA {
		t.Fatalf("User-Agent = %q, want %q", got, profile.UA)
	}
	if got := headers.Get("Accept-Language"); got != "en-US,en;q=0.9" {
		t.Fatalf("intl Accept-Language = %q", got)
	}
	if !strings.Contains(headers.Get("sec-ch-ua"), profile.Brand) ||
		headers.Get("sec-ch-ua-platform") != `"`+profile.Platform+`"` {
		t.Fatalf("sec-ch headers = %+v", headers)
	}
	cn := webBrowserHeaders("uid-1", realmCN)
	if got := cn.Get("Accept-Language"); !strings.Contains(got, "zh-CN") {
		t.Fatalf("cn Accept-Language = %q", got)
	}
}

func TestACPPromptMetaMatchesOfficialShape(t *testing.T) {
	meta := acpPromptMeta("conv-1", "Hi", "GLM-5.2")
	buddy, ok := meta["codebuddy.ai"].(map[string]any)
	if !ok {
		t.Fatalf("codebuddy.ai meta = %#v", meta["codebuddy.ai"])
	}
	for _, key := range []string{"traceparent", "traceId", "requestId", "promptRequestId",
		"clientSendTime", "conversationId", "model", "mode", "expertId", "extra"} {
		if _, exists := buddy[key]; !exists {
			t.Fatalf("prompt meta missing %q: %#v", key, buddy)
		}
	}
	if buddy["conversationId"] != "conv-1" || buddy["mode"] != "craft" ||
		buddy["model"] != "GLM-5.2" {
		t.Fatalf("prompt meta values = %#v", buddy)
	}
	if meta["baggage"] != "buddyId=,buddyName=" {
		t.Fatalf("baggage = %#v", meta["baggage"])
	}
	if _, exists := meta["codebuddy.ai/conversationRequestId"]; !exists {
		t.Fatal("missing conversationRequestId")
	}
}
