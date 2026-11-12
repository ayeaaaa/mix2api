package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const printedDSMLFixture = `<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="run_shell"><｜｜DSML｜｜ parameter name="command" string="true">ls -l</｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>`
const printedHybridFixture = `<run_code><parameter name="code">print(1)</parameter>
<parameter name="description">say hi</｜｜DSML｜｜ parameter>
</run_code></invoke></｜｜DSML｜｜ parameter></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>`

// These tests exercise the HTTP boundary, not a replacement for the parser:
// without recovery the client receives markup and cannot execute the tool.
func TestChatPrintedToolsRecovery(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, tc := range []struct {
			name, text, tool string
			args             map[string]string
		}{
			{"dsml", printedDSMLFixture, "run_shell", map[string]string{"command": "ls -l"}},
			{"hybrid", printedHybridFixture, "run_code", map[string]string{"code": "print(1)", "description": "say hi"}},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, streaming), func(t *testing.T) {
				// Fragment through tag names, full-width delimiters and parameter values.
				pieces := []string{"before "}
				for _, r := range tc.text {
					pieces = append(pieces, string(r))
				}
				pieces = append(pieces, " after")
				out := runPrintedChat(t, streaming, pieces, true, "stop", false)
				choice := out["choices"].([]any)[0].(map[string]any)
				message := choice["message"].(map[string]any)
				calls, _ := message["tool_calls"].([]any)
				if len(calls) != 1 {
					t.Fatalf("client got %d calls instead of 1: %v", len(calls), message)
				}
				fn := calls[0].(map[string]any)["function"].(map[string]any)
				var args map[string]string
				if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
					t.Fatal(err)
				}
				if fn["name"] != tc.tool || !reflect.DeepEqual(args, tc.args) {
					t.Fatalf("tool=%v args=%v", fn["name"], args)
				}
				if strings.TrimSpace(message["content"].(string)) != "before  after" {
					t.Fatalf("markup leaked or prose lost: %q", message["content"])
				}
				if choice["finish_reason"] != "tool_calls" {
					t.Fatalf("finish=%v", choice["finish_reason"])
				}
				if message["reasoning_content"] != "think" {
					t.Fatalf("reasoning lost: %v", message)
				}
				if total, _ := jsonNumberValue(out["usage"].(map[string]any)["total_tokens"]); total != 7 {
					t.Fatalf("usage lost: %v", out["usage"])
				}
			})
		}
	}
}

func TestChatPrintedToolsPreservesNonCalls(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, tc := range []struct {
			name, text string
			tools      bool
		}{
			{"plain", "The final. The final.", true},
			{"html", "hello <div>world</div>", true},
			{"no-tools", printedDSMLFixture, false},
			{"unknown", `<invoke name="delete_everything"><parameter name="x">1</parameter></invoke>`, true},
			{"parameter-only", `use <parameter>less</parameter> approach`, true},
			{"truncated", `<run_code><parameter name="code">print(1)`, true},
			{"missing-owner-close", `<run_code><parameter name="code">print(1)</parameter>`, true},
			{"code-example", "```xml\n" + printedDSMLFixture + "\n```", true},
			{"nested-fence", "````markdown\n```xml\n" + printedDSMLFixture + "\n```\n````", true},
			{"invalid-fence-close", "```xml\n```not_a_closing_fence\n" + printedDSMLFixture + "\n```", true},
			{"inline-code", "example: `" + printedDSMLFixture + "`", true},
			{"data-name", `<invoke data-name="run_shell"><parameter data-name="command">pwd</parameter></invoke>`, true},
			{"duplicate-attribute", `<invoke name="run_shell" name="unknown"><parameter name="command">pwd</parameter></invoke>`, true},
			{"mismatched-quote", `<invoke name="run_shell'><parameter name="command'>pwd</parameter></invoke>`, true},
			{"nested-owner", `<run_code><run_shell><parameter name="command">pwd</parameter></run_shell></run_code>`, true},
			{"nested-unknown", `<invoke name="unknown"><parameter name="example"><run_shell><parameter name="command">pwd</parameter></run_shell></parameter></invoke>`, true},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, streaming), func(t *testing.T) {
				out := runPrintedChat(t, streaming, []string{tc.text}, tc.tools, "length", false)
				choice := out["choices"].([]any)[0].(map[string]any)
				message := choice["message"].(map[string]any)
				if message["content"] != tc.text || message["tool_calls"] != nil || choice["finish_reason"] != "length" {
					t.Fatalf("ordinary/incomplete text changed: %v", choice)
				}
			})
		}
	}
}

func TestChatPrintedToolsStructuredCallsWin(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		out := runPrintedChat(t, streaming, []string{printedDSMLFixture}, true, "tool_calls", true)
		choice := out["choices"].([]any)[0].(map[string]any)
		message := choice["message"].(map[string]any)
		calls, _ := message["tool_calls"].([]any)
		if len(calls) != 1 || calls[0].(map[string]any)["id"] != "call_real" || message["content"] != printedDSMLFixture {
			t.Fatalf("structured tool duplicated/replaced or original text lost: %v", message)
		}
	}
}

func TestChatPrintedToolsCompleteCallAtLengthLimit(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		out := runPrintedChat(t, streaming, []string{printedDSMLFixture}, true, "length", false)
		choice := out["choices"].([]any)[0].(map[string]any)
		if choice["finish_reason"] != "tool_calls" {
			t.Fatalf("a complete recovered call must be executable: %v", choice)
		}
	}
}

func TestChatPrintedToolsPreservesBlockedOutput(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		out := runPrintedChat(t, streaming, []string{printedDSMLFixture}, true, "content_filter", false)
		choice := out["choices"].([]any)[0].(map[string]any)
		message := choice["message"].(map[string]any)
		if message["tool_calls"] != nil || choice["finish_reason"] != "content_filter" || message["content"] != printedDSMLFixture {
			t.Fatalf("blocked output must not become an executable call: %v", choice)
		}
	}
}

func TestChatPrintedToolBufferBoundAndProse(t *testing.T) {
	p := &printedToolRecovery{names: map[string]bool{"run_shell": true}}
	f := printedTextBuffer{recovery: p}
	if got := f.feed("hello <div>world</div>"); got != "hello <div>world</div>" {
		t.Fatalf("ordinary text delayed: %q", got)
	}
	long := "<run_shell>" + strings.Repeat("x", printedToolMaxHold)
	if got := f.feed(long); got != long || f.buffer.Len() != 0 {
		t.Fatal("oversize candidate must flush as text")
	}
	if got := f.feed(printedDSMLFixture); got != printedDSMLFixture {
		t.Fatal("overflow must disable recovery for the turn")
	}
	if text, calls := f.flush(false, ""); text != "" || len(calls) != 0 {
		t.Fatal("oversize buffer fabricated a call")
	}
}

func TestChatPrintedToolsMultipleAndParamSafety(t *testing.T) {
	p := &printedToolRecovery{names: map[string]bool{"run_shell": true, "run_code": true}}
	for _, text := range []string{
		`<run_shell><parameter name="command">pwd</parameter><parameter name="command">rm</parameter></run_shell>`,
		`<run_shell><parameter>pwd</parameter></run_shell>`,
		`<run_shell><parameter name="command">pwd</parameter> garbage </run_shell>`,
	} {
		if calls, clean := p.parse(text); len(calls) != 0 || clean != text {
			t.Fatalf("invalid block recovered: %v %q", calls, clean)
		}
	}
	if calls, clean := p.parse(printedDSMLFixture + " between " + printedDSMLFixture); len(calls) != 2 || clean != " between " {
		t.Fatalf("parallel recovery failed: %v %q", calls, clean)
	}
	text := `<run_shell><parameter name="command">echo '</run_shell> 你好 &amp;'</parameter></run_shell>`
	calls, _ := p.parse(text)
	if len(calls) != 1 {
		t.Fatalf("literal XML in parameter truncated the call: %v", calls)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].arguments), &args); err != nil || args["command"] != "echo '</run_shell> 你好 &amp;'" {
		t.Fatalf("parameter content modified: %v %v", args, err)
	}
}

func TestChatPrintedToolsStreamAbortFlushesText(t *testing.T) {
	stream := newStubStream(`data: {"id":"up","choices":[{"index":0,"delta":{"role":"assistant","content":"<run_code><parameter name=\"code\">print(1)"}}]}` + "\n\n")
	stream.terminal = errors.New("read interrupted")
	provider := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "m1"}}, openStream: stream}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true,"tools":[{"type":"function","function":{"name":"run_code"}}]}`, nil)
	if !strings.Contains(rec.Body.String(), "print(1)") || !strings.Contains(rec.Body.String(), "upstream_stream_error") || strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), `"tool_calls"`) {
		t.Fatalf("abort lost buffered text or fabricated successful tool completion: %s", rec.Body)
	}
}

func TestChatPrintedToolDeclarations(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"functions":[{"name":"run_shell"}],"tool_choice":"none"}`, true},
		{`{"tools":[],"functions":[{"name":"run_shell"}]}`, false},
		{`{"tools":[{"type":"function","name":"run_shell"}]}`, true},
		{`{"tools":[{"type":"web_search","name":"run_shell"}]}`, false},
		{`{"n":2,"tools":[{"type":"function","name":"run_shell"}]}`, false},
		{`{"n":2.0,"tools":[{"type":"function","name":"run_shell"}]}`, false},
		{`{"n":2e0,"tools":[{"type":"function","name":"run_shell"}]}`, false},
		{`{"tools":[{"type":"web_search","function":{"name":"run_shell"}}]}`, false},
	} {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.body), &payload); err != nil {
			t.Fatal(err)
		}
		got := chatPrintedToolRecovery(payload)
		if (got != nil) != tc.want {
			t.Fatalf("declarations=%s got=%v want recovery=%v", tc.body, got, tc.want)
		}
	}
}

func TestChatPrintedToolsOrdinaryHTMLAllocation(t *testing.T) {
	f := printedTextBuffer{recovery: &printedToolRecovery{names: map[string]bool{"run_shell": true}}}
	text := strings.Repeat("<div>x", 3000)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := f.feed(text)
	runtime.ReadMemStats(&after)
	if got != text {
		t.Fatal("HTML changed")
	}
	if after.TotalAlloc-before.TotalAlloc > 4*1024*1024 {
		t.Fatalf("18KiB HTML allocated %d bytes", after.TotalAlloc-before.TotalAlloc)
	}
}

func TestChatPrintedToolsMessageFormStructuredWins(t *testing.T) {
	frame := "data: " + `{"choices":[{"index":0,"message":{"tool_calls":[{"index":0,"id":"call_real","type":"function","function":{"name":"run_shell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n"
	n := newStreamNormalizer("m1", &printedToolRecovery{names: map[string]bool{"run_shell": true}})
	delta, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": printedDSMLFixture}}}})
	n.absorb([]byte("data: " + string(delta) + "\n\n"))
	out := n.absorb([]byte(frame))
	out = append(out, n.finalize()...)
	var calls []string
	var content strings.Builder
	for _, data := range out {
		var chunk map[string]any
		if err := json.Unmarshal([]byte(ssePayload(data)), &chunk); err != nil {
			t.Fatal(err)
		}
		for _, raw := range chunk["choices"].([]any) {
			choice := raw.(map[string]any)
			for _, key := range []string{"delta", "message"} {
				body, _ := choice[key].(map[string]any)
				if text, ok := body["content"].(string); ok {
					content.WriteString(text)
				}
				for _, c := range func() []any { v, _ := body["tool_calls"].([]any); return v }() {
					calls = append(calls, c.(map[string]any)["id"].(string))
				}
			}
		}
	}
	if !reflect.DeepEqual(calls, []string{"call_real"}) || content.String() != printedDSMLFixture {
		t.Fatalf("calls=%v content=%q", calls, content.String())
	}
}

func TestChatPrintedToolsErrorFrameDoesNotFinalize(t *testing.T) {
	delta, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": printedDSMLFixture}}}})
	stream := newStubStream("data: "+string(delta)+"\n\n", `data: {"error":{"type":"upstream_error","message":"generation aborted"}}`+"\n\n")
	provider := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "m1"}}, openStream: stream}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true,"tools":[{"type":"function","function":{"name":"run_shell"}}]}`, nil)
	if !strings.Contains(rec.Body.String(), "generation aborted") || strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), `"tool_calls"`) {
		t.Fatalf("error became successful completion: %s", rec.Body)
	}
}

func TestChatPrintedToolsNonStreamErrorFrame(t *testing.T) {
	delta, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": printedDSMLFixture}}}})
	stream := newStubStream("data: "+string(delta)+"\n\n", `data: {"error":{"type":"upstream_error","message":"generation aborted"}}`+"\n\n")
	out, err := aggregateChatStream(stream, "m1", &printedToolRecovery{names: map[string]bool{"run_shell": true}})
	if err == nil || out != nil {
		t.Fatalf("nonstream error became executable completion: %v %v", out, err)
	}
}

func BenchmarkChatPrintedOrdinaryHTML(b *testing.B) {
	text := strings.Repeat("<div>x", 16000)
	p := &printedToolRecovery{names: map[string]bool{"run_shell": true}}
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f := printedTextBuffer{recovery: p}
		if f.feed(text) != text {
			b.Fatal("HTML changed")
		}
	}
}

func BenchmarkChatPrintedManyEmptyCalls(b *testing.B) {
	text := strings.Repeat("<run_shell></run_shell>", 6000)
	p := &printedToolRecovery{names: map[string]bool{"run_shell": true}}
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		calls, clean := p.parse(text)
		if len(calls) != 6000 || clean != "" {
			b.Fatal("calls lost")
		}
	}
}

func TestChatPrintedUnexpectedMultipleChoices(t *testing.T) {
	frame := `data: {"choices":[{"index":0,"delta":{"content":"<run_shell><parameter name=\"command\">"}},{"index":1,"delta":{"content":"pwd</parameter></run_shell>"}}]}` + "\n\n"
	p := &printedToolRecovery{names: map[string]bool{"run_shell": true}}
	completion, err := aggregateChatStream(newStubStream(frame), "m1", p)
	if err != nil {
		t.Fatal(err)
	}
	message := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["tool_calls"] != nil {
		t.Fatalf("two partial choices synthesized a call: %v", message)
	}
	n := newStreamNormalizer("m1", p)
	out := n.absorb([]byte(frame))
	out = append(out, n.finalize()...)
	for _, piece := range out {
		if strings.Contains(string(piece), `"tool_calls"`) {
			t.Fatalf("two streamed choices synthesized a call: %s", piece)
		}
	}
}

func runPrintedChat(t *testing.T, streaming bool, pieces []string, withTools bool, finish string, structured bool) map[string]any {
	t.Helper()
	frame := func(delta map[string]any, finish any) string {
		data, err := json.Marshal(map[string]any{"id": "up", "model": "m1", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		if err != nil {
			t.Fatal(err)
		}
		return "data: " + string(data) + "\n\n"
	}
	frames := []string{frame(map[string]any{"role": "assistant", "reasoning_content": "think"}, nil)}
	for _, piece := range pieces {
		frames = append(frames, frame(map[string]any{"content": piece}, nil))
	}
	if structured {
		frames = append(frames, frame(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_real", "type": "function", "function": map[string]any{"name": "run_shell", "arguments": `{"command":"pwd"}`}}}}, nil))
	}
	frames = append(frames, frame(map[string]any{}, finish), `data: {"id":"up","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`+"\n\n", "data: [DONE]\n\n")
	provider := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "m1"}}, openStream: newStubStream(frames...)}
	srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{provider}})
	request := map[string]any{"model": "m1", "messages": []any{map[string]any{"role": "user", "content": "do the thing"}}, "stream": streaming, "tool_choice": "none"}
	if withTools {
		request["tools"] = []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "run_shell", "parameters": map[string]any{"type": "object"}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "run_code", "parameters": map[string]any{"type": "object"}}},
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions", string(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if streaming {
		if strings.Count(rec.Body.String(), "data: [DONE]") != 1 {
			t.Fatalf("bad DONE contract: %s", rec.Body)
		}
		// Consume the actual client-visible SSE using the existing protocol aggregator.
		clientFrames := strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
		for index := range clientFrames {
			clientFrames[index] += "\n\n"
		}
		out, err = aggregateChatStream(newStubStream(clientFrames...), "m1")
	} else {
		err = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return out
}
