package protocol

import (
	"context"
	"errors"
	"io"
	"testing"
)

type testStream struct {
	frames []string
	index  int
	err    error
}

func (s *testStream) Next() ([]byte, error) {
	if s.index < len(s.frames) {
		frame := []byte(s.frames[s.index])
		s.index++
		return frame, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}

func (s *testStream) Close() error { return nil }

func TestParseFrameAndReader(t *testing.T) {
	frame := []byte("data: {\"id\":\"c1\",\"model\":\"m\",\"created\":10,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\",\"reasoning_content\":\"think\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"run\",\"arguments\":\"{\"}}]},\"finish_reason\":null}],\"usage\":{\"total_tokens\":3}}\n\n")
	chunk := ParseFrame(frame)
	if !chunk.Valid || chunk.Done {
		t.Fatalf("chunk valid=%v done=%v", chunk.Valid, chunk.Done)
	}
	if chunk.ID != "c1" || chunk.Model != "m" || chunk.Created != 10 {
		t.Fatalf("header = %#v", chunk)
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].Delta.Content != "hi" ||
		chunk.Choices[0].Delta.ReasoningContent != "think" {
		t.Fatalf("choices = %#v", chunk.Choices)
	}
	if len(chunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", chunk.Choices[0].Delta.ToolCalls)
	}
	call := chunk.Choices[0].Delta.ToolCalls[0]
	if call.ID != "call_1" || call.Name != "run" || call.Arguments != "{" {
		t.Fatalf("call = %#v", call)
	}
	if total, _ := Int64(chunk.Usage["total_tokens"]); total != 3 {
		t.Fatalf("usage = %#v", chunk.Usage)
	}

	done := ParseFrame([]byte("data: [DONE]\n\n"))
	if !done.Done || !done.Valid {
		t.Fatalf("done chunk = %#v", done)
	}
	if invalid := ParseFrame([]byte("data: not-json\n\n")); invalid.Valid {
		t.Fatalf("invalid chunk valid = %#v", invalid)
	}
	if empty := ParseFrame([]byte(": keepalive\n\n")); empty.Data != "" {
		t.Fatalf("comment data = %q", empty.Data)
	}
}

func TestReaderSkipsNoiseAndContinuesAfterDone(t *testing.T) {
	stream := &testStream{frames: []string{
		": keepalive\n\n",
		"data: not-json\n\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
		"data: [DONE]\n\n",
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"total_tokens\":9}}\n\n",
	}}
	reader := NewReader(stream)
	first, err := reader.Next()
	if err != nil || first.Choices[0].Delta.Content != "a" {
		t.Fatalf("first = %#v err=%v", first, err)
	}
	done, err := reader.Next()
	if err != nil || !done.Done {
		t.Fatalf("done = %#v err=%v", done, err)
	}
	usage, err := reader.Next()
	if err != nil {
		t.Fatalf("usage err = %v", err)
	}
	if total, _ := Int64(usage.Usage["total_tokens"]); total != 9 {
		t.Fatalf("usage = %#v", usage.Usage)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("final err = %v, want EOF", err)
	}
}

func TestReaderEmptyAndCanceled(t *testing.T) {
	reader := NewReader(nil)
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("nil stream err = %v", err)
	}
	reader = NewReader(&testStream{err: context.Canceled})
	if _, err := reader.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err = %v", err)
	}
}
