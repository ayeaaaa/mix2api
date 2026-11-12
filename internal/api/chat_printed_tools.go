package api

import (
	"encoding/json"
	"sort"
	"strings"
)

// Recovery is local to Chat requests with declared function tools. It is not
// an executor and does not alter Responses translation or arbitrary markup.
type printedToolRecovery struct{ names map[string]bool }

func chatPrintedToolRecovery(payload map[string]json.RawMessage) *printedToolRecovery {
	if raw, exists := payload["n"]; exists {
		var n int
		if json.Unmarshal(raw, &n) != nil || n != 1 {
			return nil
		}
	}
	names := map[string]bool{}
	var tools []map[string]any
	_ = json.Unmarshal(payload["tools"], &tools)
	for _, tool := range tools {
		if kind, _ := tool["type"].(string); kind != "" && kind != "function" {
			continue
		}
		function, _ := tool["function"].(map[string]any)
		if function == nil {
			function = tool
		}
		if name, _ := function["name"].(string); name != "" {
			names[name] = true
		}
	}
	if _, hasTools := payload["tools"]; !hasTools {
		var functions []map[string]any
		_ = json.Unmarshal(payload["functions"], &functions)
		for _, function := range functions {
			if name, _ := function["name"].(string); name != "" {
				names[name] = true
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	return &printedToolRecovery{names: names}
}

const printedToolMaxHold = 256 * 1024

type printedTag struct {
	start, end              int
	name                    string
	attrs                   map[string]string
	close, valid, selfClose bool
}
type printedSpan struct{ start, end int }

func printedWrapper(name string) bool {
	switch strings.ToLower(name) {
	case "calls", "tool_calls", "function_calls", "function_call":
		return true
	}
	return false
}
func printedParameter(name string) bool {
	switch strings.ToLower(name) {
	case "parameter", "param", "arg", "argument":
		return true
	}
	return false
}
func printedSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func printedNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == ':'
}

// Every attribute must have one exact name and matching quotes. Substrings
// such as data-name, duplicate attributes and malformed quotes never supply a
// function/parameter name. Unknown well-formed attributes are harmless.
func printedAttributes(raw string) (map[string]string, bool) {
	var attrs map[string]string
	for len(raw) > 0 {
		if !printedSpace(raw[0]) {
			return nil, false
		}
		raw = strings.TrimLeft(raw, " \t\r\n")
		if raw == "" {
			break
		}
		n := 0
		for n < len(raw) && printedNameByte(raw[n]) {
			n++
		}
		if n == 0 {
			return nil, false
		}
		name := strings.ToLower(raw[:n])
		raw = strings.TrimLeft(raw[n:], " \t\r\n")
		if len(raw) == 0 || raw[0] != '=' {
			return nil, false
		}
		raw = strings.TrimLeft(raw[1:], " \t\r\n")
		if len(raw) == 0 || raw[0] != '\'' && raw[0] != '"' {
			return nil, false
		}
		end := strings.IndexByte(raw[1:], raw[0])
		if end < 0 {
			return nil, false
		}
		if attrs == nil {
			attrs = map[string]string{}
		}
		if _, duplicate := attrs[name]; duplicate {
			return nil, false
		}
		attrs[name] = raw[1 : end+1]
		raw = raw[end+2:]
	}
	return attrs, true
}

// Tokenize once, preserving byte offsets in the original UTF-8 text. Fences
// track their exact delimiter length, and only a bare matching fence closes.
func printedTags(text string) []printedTag {
	var tags []printedTag
	var fence byte
	fenceLen := 0
	inlineLen := 0
	for i := 0; i < len(text); {
		if inlineLen == 0 && (i == 0 || text[i-1] == '\n') {
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				end = len(text) - i
			}
			line := strings.TrimLeft(text[i:i+end], " \t")
			if len(line) >= 3 && (line[0] == '`' || line[0] == '~') {
				n := 1
				for n < len(line) && line[n] == line[0] {
					n++
				}
				if fence == 0 && n >= 3 {
					fence, fenceLen = line[0], n
					i += end
					if i < len(text) {
						i++
					}
					continue
				}
				if line[0] == fence && n >= fenceLen && strings.TrimSpace(line[n:]) == "" {
					fence = 0
					i += end
					if i < len(text) {
						i++
					}
					continue
				}
			}
			if fence != 0 {
				i += end
				if i < len(text) {
					i++
				}
				continue
			}
		}
		if text[i] == '`' {
			start := i
			for i < len(text) && text[i] == '`' {
				i++
			}
			if inlineLen == 0 {
				inlineLen = i - start
			} else if inlineLen == i-start {
				inlineLen = 0
			}
			continue
		}
		if inlineLen > 0 {
			i++
			continue
		}
		if text[i] != '<' {
			i++
			continue
		}
		start := i
		quote := byte(0)
		i++
		for i < len(text) {
			c := text[i]
			if quote != 0 {
				if c == quote {
					quote = 0
				}
			} else if c == '\'' || c == '"' {
				quote = c
			} else if c == '>' || c == '<' {
				break
			}
			i++
		}
		if i >= len(text) {
			break
		}
		if text[i] != '>' {
			continue
		}
		i++
		raw := strings.TrimSpace(text[start+1 : i-1])
		tag := printedTag{start: start, end: i}
		if strings.HasPrefix(raw, "/") {
			tag.close = true
			raw = strings.TrimSpace(raw[1:])
		}
		if strings.HasSuffix(raw, "/") {
			tag.selfClose = true
			raw = strings.TrimRight(raw[:len(raw)-1], " \t\r\n")
		}
		if strings.HasPrefix(raw, "|") || strings.HasPrefix(raw, "｜") {
			raw = strings.TrimLeft(raw, "|｜ \t\r\n")
			if len(raw) < 4 || !strings.EqualFold(raw[:4], "DSML") {
				continue
			}
			raw = strings.TrimLeft(raw[4:], " \t\r\n")
			if !strings.HasPrefix(raw, "|") && !strings.HasPrefix(raw, "｜") {
				continue
			}
			raw = strings.TrimLeft(raw, "|｜ \t\r\n")
		}
		n := 0
		for n < len(raw) && printedNameByte(raw[n]) {
			n++
		}
		if n == 0 {
			continue
		}
		tag.name = raw[:n]
		tag.attrs, tag.valid = printedAttributes(raw[n:])
		if tag.close && len(tag.attrs) != 0 {
			tag.valid = false
		}
		tags = append(tags, tag)
	}
	return tags
}

// Recover only top-level, complete owners under optional calls wrappers. A
// rejected/unknown owner or parameter is never reparsed for nested calls.
func (p *printedToolRecovery) parse(text string) ([]dsmlCall, string) {
	if p == nil || len(text) > printedToolMaxHold {
		return nil, text
	}
	type context struct {
		tag       printedTag
		callCount int
		wrapper   bool
	}
	type candidate struct {
		tag   printedTag
		depth int
		name  string
		args  map[string]string
		last  int
		valid bool
	}
	var stack []context
	nonWrapperDepth := 0
	var call *candidate
	var param *printedTag
	var spans []printedSpan
	var calls []dsmlCall
	lastCallEnd := -1
	for _, tag := range printedTags(text) {
		name := strings.ToLower(tag.name)
		if param != nil {
			if tag.close && name == strings.ToLower(param.name) {
				if !tag.valid {
					call.valid = false
				}
				call.args[param.attrs["name"]] = strings.TrimSpace(text[param.end:tag.start])
				call.last = tag.end
				param = nil
			} else if !tag.close && printedParameter(name) {
				call.valid = false
			}
			continue
		}
		if !tag.close {
			if call != nil {
				if len(stack) == call.depth && printedParameter(name) {
					if !tag.valid || tag.attrs["name"] == "" || strings.TrimSpace(text[call.last:tag.start]) != "" {
						call.valid = false
					}
					if _, dup := call.args[tag.attrs["name"]]; dup {
						call.valid = false
					}
					copy := tag
					param = &copy
					continue
				}
				call.valid = false
			}
			allowedContext := nonWrapperDepth == 0
			if !tag.selfClose {
				wrapper := printedWrapper(tag.name) && tag.valid
				stack = append(stack, context{tag: tag, callCount: len(calls), wrapper: wrapper})
				if !wrapper {
					nonWrapperDepth++
				}
			}
			if call == nil && allowedContext && !tag.selfClose {
				tool := tag.name
				if name == "invoke" {
					tool = tag.attrs["name"]
				}
				if p.names[tool] {
					call = &candidate{tag: tag, depth: len(stack), name: tool, args: map[string]string{}, last: tag.end, valid: tag.valid}
				}
			}
			continue
		}
		if len(stack) == 0 || !strings.EqualFold(stack[len(stack)-1].tag.name, tag.name) {
			if call != nil {
				call.valid = false
			}
			// Hybrid responses can add redundant closes after a complete call.
			if lastCallEnd >= 0 && strings.TrimSpace(text[lastCallEnd:tag.start]) == "" && tag.valid && (printedWrapper(name) || printedParameter(name) || name == "invoke") {
				spans = append(spans, printedSpan{tag.start, tag.end})
				lastCallEnd = tag.end
			}
			continue
		}
		entry := stack[len(stack)-1]
		if call != nil && len(stack) == call.depth {
			if call.valid && tag.valid && strings.TrimSpace(text[call.last:tag.start]) == "" {
				args, _ := json.Marshal(call.args)
				calls = append(calls, dsmlCall{id: randomChatID("call_"), name: call.name, arguments: string(args)})
				spans = append(spans, printedSpan{call.tag.start, tag.end})
				lastCallEnd = tag.end
			}
			call = nil
		}
		if entry.wrapper && len(calls) > entry.callCount {
			spans = append(spans, printedSpan{entry.tag.start, entry.tag.end}, printedSpan{tag.start, tag.end})
			lastCallEnd = tag.end
		}
		stack = stack[:len(stack)-1]
		if !entry.wrapper {
			nonWrapperDepth--
		}
	}
	if len(calls) == 0 {
		return nil, text
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var clean strings.Builder
	last := 0
	for _, span := range spans {
		if span.end <= last {
			continue
		}
		if span.start > last {
			clean.WriteString(text[last:span.start])
		}
		last = span.end
	}
	clean.WriteString(text[last:])
	return calls, clean.String()
}

// Bounded candidate-prefix checks and a builder keep fragmented streams and
// ordinary HTML linear in input size. Tool frames remain deferred so a later
// structured call can win without executing both forms.
type printedTextBuffer struct {
	recovery *printedToolRecovery
	buffer   strings.Builder
	disabled bool
}

func (f *printedTextBuffer) feed(piece string) string {
	if f.recovery == nil || f.disabled {
		return piece
	}
	if f.buffer.Len() > 0 {
		f.buffer.WriteString(piece)
		if f.buffer.Len() <= printedToolMaxHold {
			return ""
		}
		out := f.buffer.String()
		f.buffer.Reset()
		f.disabled = true
		return out
	}
	for i := 0; i < len(piece); i++ {
		if piece[i] != '<' {
			continue
		}
		end := i + 96
		if end > len(piece) {
			end = len(piece)
		}
		tail := "<" + strings.ToLower(strings.TrimLeft(piece[i+1:end], " \t\r\n"))
		candidate := tail == "<" || strings.HasPrefix(tail, "<|") || strings.HasPrefix(tail, "<｜")
		for _, name := range []string{"invoke", "calls", "tool_calls", "function_calls", "function_call"} {
			prefix := "<" + name
			if strings.HasPrefix(prefix, tail) || strings.HasPrefix(tail, prefix) {
				candidate = true
			}
		}
		if !candidate {
			for name := range f.recovery.names {
				prefix := "<" + strings.ToLower(name)
				if strings.HasPrefix(prefix, tail) || strings.HasPrefix(tail, prefix) {
					candidate = true
					break
				}
			}
		}
		if !candidate {
			continue
		}
		if len(piece[i:]) > printedToolMaxHold {
			f.disabled = true
			return piece
		}
		f.buffer.WriteString(piece[i:])
		return piece[:i]
	}
	return piece
}
func (f *printedTextBuffer) flush(structured bool, prefix string) (string, []dsmlCall) {
	text := f.buffer.String()
	f.buffer.Reset()
	if structured || f.disabled || f.recovery == nil {
		return text, nil
	}
	calls, clean := f.recovery.parse(prefix + text)
	if !strings.HasPrefix(clean, prefix) {
		return text, nil
	}
	return strings.TrimPrefix(clean, prefix), calls
}
