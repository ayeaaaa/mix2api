package doubao

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestChatStreamSetAndAppendBlocks(t *testing.T) {
	sse := strings.Join([]string{
		"id: 0\nevent: SSE_ACK\ndata: {\"ack_client_meta\":{\"conversation_id\":\"c1\",\"section_id\":\"s1\",\"local_conversation_id\":\"l1\"}}\n\n",
		"id: 0\nevent: STREAM_MSG_NOTIFY\ndata: {\"content\":{\"content_block\":[{\"block_id\":\"b1\",\"block_type\":10000,\"content\":{\"text_block\":{\"text\":\"O\"}}}]}}\n\n",
		"id: 0\nevent: STREAM_CHUNK\ndata: {\"patch_op\":[{\"patch_object\":1,\"patch_type\":1,\"patch_value\":{\"content_block\":[{\"block_id\":\"b1\",\"block_type\":10000,\"content\":{\"text_block\":{\"text\":\"K\"}}}]}}]}\n\n",
		"id: 0\nevent: STREAM_MSG_NOTIFY\ndata: {\"content\":{\"content_block\":[{\"block_id\":\"think\",\"block_type\":10040,\"content\":{\"text_block\":{\"text\":\"SECRET\"}}}]}}\n\n",
		"id: 0\nevent: SSE_REPLY_END\ndata: {}\n\n",
	}, "")
	conv := &conversation{}
	stream := newTestChatStream(sse, conv)
	defer stream.Close()
	body := readAllFrames(t, stream)
	if !strings.Contains(body, `"content":"O"`) || !strings.Contains(body, `"content":"K"`) {
		t.Fatalf("missing content frames: %s", body)
	}
	if strings.Contains(body, "SECRET") {
		t.Fatalf("thinking block leaked: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]: %s", body)
	}
	if conv.ConversationID != "c1" || conv.SectionID != "s1" {
		t.Fatalf("conversation not updated: %#v", conv)
	}
}

func TestChatStreamError(t *testing.T) {
	sse := "id: 0\nevent: STREAM_ERROR\ndata: {\"error_msg\":\"rate limited\",\"error_code\":710022004}\n\n"
	stream := newTestChatStream(sse, &conversation{})
	defer stream.Close()
	for {
		_, err := stream.Next()
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			t.Fatal("stream ended without error")
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Kind != KindRateLimited {
			t.Fatalf("error = %#v", err)
		}
		return
	}
}

func newTestChatStream(sse string, conv *conversation) *chatStream {
	model, _ := FindModel("doubao-fast")
	p := &Provider{pool: &Pool{cfg: Config{MaxConcurrent: 1}}, conversations: map[string]conversationEntry{}}
	account := &Account{UID: "u1", Enabled: true, maxConcurrent: 1}
	return newChatStream(context.Background(), io.NopCloser(strings.NewReader(sse)), p, account, model, conv, "session")
}

func readAllFrames(t *testing.T, stream *chatStream) string {
	t.Helper()
	var b strings.Builder
	for {
		frame, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("Next: %v", err)
		}
		b.Write(frame)
	}
	return b.String()
}
