package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateACPLinkRejectsSSRFTargets(t *testing.T) {
	rejected := []string{
		"http://sandbox.workbuddy.ai/acp",
		"https://127.0.0.1/acp",
		"https://[::1]/acp",
		"https://10.1.2.3/acp",
		"https://192.168.1.5/acp",
		"https://169.254.169.254/latest/meta-data",
		"https://localhost/acp",
		"https://metadata.google.internal/acp",
		"https://sandbox.local/acp",
		"https://user:pass@sandbox.workbuddy.ai/acp",
	}
	for _, link := range rejected {
		if err := validateACPLink(link); err == nil {
			t.Fatalf("validateACPLink(%q) accepted an SSRF target", link)
		}
	}
	for _, link := range []string{
		"https://sandbox.workbuddy.ai/acp",
		"https://abc.sandbox.workbuddy.cn/acp",
		"https://sandbox.tencent.com/acp",
	} {
		if err := validateACPLink(link); err != nil {
			t.Fatalf("validateACPLink(%q) = %v, want allowlisted", link, err)
		}
	}
}

func TestACPTokenNeverSentToRejectedLink(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Acp-Connection-Id", "should-not-happen")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result := runACPTurn(context.Background(),
		catalogDoerFunc(func(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
			return server.Client().Do(req)
		}),
		server.URL+"/acp", "sandbox-secret-token", "session-1", "/workspace", "Hi", "test-agent", "model",
		"", 50*time.Millisecond, time.Millisecond, nil, validateACPLink)
	if calls.Load() != 0 {
		t.Fatalf("rejected ACP link was contacted %d times", calls.Load())
	}
	if result.OK || !result.Terminal || !strings.Contains(result.Error, "https") {
		t.Fatalf("rejected ACP result = %+v", result)
	}
	if strings.Contains(result.Error, "sandbox-secret-token") {
		t.Fatal("ACP rejection leaked the sandbox token")
	}
}
