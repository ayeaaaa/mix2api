package api

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/platform/usage"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// usageCapture keeps every RecordInput the API hands to the recorder and also
// persists it through the real recorder, so a test can pin both the attribution
// contract and the row shape written to the ledger.
type usageCapture struct {
	store  *usage.Store
	inputs []usage.RecordInput
}

func newUsageCapture(t *testing.T) *usageCapture {
	t.Helper()
	dir := t.TempDir()
	return &usageCapture{store: usage.New(usage.Paths{Dir: dir, Log: filepath.Join(dir, "usage.jsonl")})}
}

func (c *usageCapture) record(ctx context.Context, in usage.RecordInput) error {
	c.inputs = append(c.inputs, in)
	return (&usage.Recorder{Store: c.store}).Record(ctx, in)
}

func (c *usageCapture) single(t *testing.T) usage.RecordInput {
	t.Helper()
	if len(c.inputs) != 1 {
		t.Fatalf("usage records = %d, want 1: %+v", len(c.inputs), c.inputs)
	}
	return c.inputs[0]
}

func (c *usageCapture) rows(t *testing.T) []usage.Row {
	t.Helper()
	rows, err := c.store.ReadRows()
	if err != nil {
		t.Fatalf("read usage rows: %v", err)
	}
	return rows
}

// TestUsageQualifiedModelAttribution pins the exit-qualified recording contract
// for every protocol: Model is the upstream ID billing sees, RequestedModel is
// the canonical client ID, and Realm is the exit that actually served it. The
// persisted row keeps both IDs so the ledger can explain the charge without
// re-parsing the exit prefix.
func TestUsageQualifiedModelAttribution(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		body    string
		stream  func() providers.ChatStream
		options func(*ServerOptions)
	}{
		{
			name:   "chat",
			path:   "/v1/chat/completions",
			body:   `{"model":"codearts/shared","messages":[{"role":"user","content":"hi"}]}`,
			stream: chatSuccessStream,
		},
		{
			name:   "responses",
			path:   "/v1/responses",
			body:   `{"model":"codearts/shared","input":"hi"}`,
			stream: func() providers.ChatStream { return responsesStreamWithModel("shared") },
		},
		{
			name:   "messages",
			path:   "/v1/messages",
			body:   `{"model":"codearts/shared","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			stream: func() providers.ChatStream { return messagesChatStream("shared") },
			options: func(opts *ServerOptions) {
				opts.AnthropicMessagesEnabled = func() bool { return true }
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			capture := newUsageCapture(t)
			codearts := &stubProvider{id: "codearts", realm: "codearts",
				models: []providers.Model{{"id": "shared"}}, openStream: testCase.stream()}
			opts := ServerOptions{
				Providers:   []providers.Provider{codearts},
				RecordUsage: capture.record,
			}
			if testCase.options != nil {
				testCase.options(&opts)
			}
			srv := newTestHTTPServer(opts)

			rec := serveRequest(srv.Handler, http.MethodPost, testCase.path, testCase.body, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}

			got := capture.single(t)
			if got.Model != "shared" {
				t.Fatalf("billing model = %q", got.Model)
			}
			if got.RequestedModel != "codearts/shared" {
				t.Fatalf("requested model = %q", got.RequestedModel)
			}
			if got.Realm != "codearts" {
				t.Fatalf("realm = %q", got.Realm)
			}

			rows := capture.rows(t)
			if len(rows) != 1 {
				t.Fatalf("persisted rows = %d, want 1: %#v", len(rows), rows)
			}
			if rows[0]["model"] != "shared" {
				t.Fatalf("persisted model = %#v", rows[0]["model"])
			}
			if rows[0]["requested_model"] != "codearts/shared" {
				t.Fatalf("persisted requested_model = %#v", rows[0]["requested_model"])
			}
		})
	}
}

// TestUsageBareModelRequestedModelOmitted pins the bare-request shape: the
// requested ID equals the upstream ID, so the recorder must not persist a
// redundant requested_model field.
func TestUsageBareModelRequestedModelOmitted(t *testing.T) {
	capture := newUsageCapture(t)
	codearts := &stubProvider{id: "codearts", realm: "codearts",
		models: []providers.Model{{"id": "shared"}}, openStream: chatSuccessStream()}
	srv := newTestHTTPServer(ServerOptions{
		Providers:   []providers.Provider{codearts},
		RecordUsage: capture.record,
	})

	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions?realm=codearts",
		`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	got := capture.single(t)
	if got.Model != "shared" || got.RequestedModel != "shared" {
		t.Fatalf("bare attribution = model %q requested %q, want shared/shared", got.Model, got.RequestedModel)
	}
	if got.Realm != "codearts" {
		t.Fatalf("realm = %q", got.Realm)
	}

	rows := capture.rows(t)
	if len(rows) != 1 {
		t.Fatalf("persisted rows = %d, want 1: %#v", len(rows), rows)
	}
	if rows[0]["model"] != "shared" {
		t.Fatalf("persisted model = %#v", rows[0]["model"])
	}
	if value, exists := rows[0]["requested_model"]; exists {
		t.Fatalf("bare request persisted requested_model = %#v", value)
	}
}
