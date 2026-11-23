package providers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// ChatRequestMessages returns the request's decoded message list. Callers that
// only built Raw (older tests, provider probes) still get a usable list.
func ChatRequestMessages(req ChatRequest) []json.RawMessage {
	if raw, ok := req.Raw["messages"]; ok {
		var messages []json.RawMessage
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil
		}
		return messages
	}
	return req.Messages
}

// DerivePrefixSessionKey returns a stable conversation key from the request's
// first messages. It is the v2 counterpart of v1's prefix-based session
// affinity: without it, clients that send no conversation header rotate across
// the whole account pool every turn and lose the upstream account-scoped
// prefix cache. The explicit session key still wins; callers only use this
// helper when it is empty.
//
// Use the leading system/developer messages and first user, not an arbitrary
// first-two slice (which collides behind two system messages and changes a
// user-only key on its second turn). stable also includes the first assistant.
func DerivePrefixSessionKey(messages []json.RawMessage, stable bool) string {
	head := make([]any, 0, 4)
	haveUser := false
	for _, raw := range messages {
		if !json.Valid(raw) {
			return ""
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value map[string]any
		if err := decoder.Decode(&value); err != nil || value == nil {
			return ""
		}
		role, _ := value["role"].(string)
		role = strings.ToLower(role)
		if role == "developer" {
			role = "system"
		}
		projected := map[string]any{"role": role, "content": affinityContent(value["content"])}
		for _, key := range []string{"name", "tool_calls", "function_call"} {
			if v, ok := value[key]; ok {
				projected[key] = v
			}
		}
		if !haveUser {
			switch role {
			case "system", "developer":
				head = append(head, projected)
			case "user":
				head = append(head, projected)
				haveUser = true
				if !stable {
					return prefixHeadKey(head)
				}
			default:
				return ""
			}
		} else if role == "assistant" {
			head = append(head, projected)
			return prefixHeadKey(head)
		}
	}
	if !haveUser {
		return ""
	}
	return prefixHeadKey(head)
}

// Normalize equivalent text-block/string representations without mutating the
// wire request. Cache-control and transport metadata are not semantic text.
func affinityContent(value any) any {
	if value == nil {
		return ""
	}
	parts, ok := value.([]any)
	if !ok {
		return value
	}
	texts := []string{}
	normalized := []any{}
	textOnly := true
	for _, raw := range parts {
		if text, ok := raw.(string); ok {
			if text != "" {
				texts = append(texts, text)
			}
			normalized = append(normalized, map[string]any{"type": "text", "text": text})
			continue
		}
		part, ok := raw.(map[string]any)
		if !ok {
			textOnly = false
			normalized = append(normalized, raw)
			continue
		}
		typ, _ := part["type"].(string)
		switch typ {
		case "text", "input_text", "output_text", "summary_text":
			text, _ := part["text"].(string)
			if text != "" {
				texts = append(texts, text)
			}
			normalized = append(normalized, map[string]any{"type": "text", "text": text})
		case "image_url", "input_image", "image":
			textOnly = false
			url := part["image_url"]
			if url == nil {
				url = part["url"]
			}
			if obj, ok := url.(map[string]any); ok {
				url = obj["url"]
			}
			if url != nil {
				normalized = append(normalized, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			} else {
				normalized = append(normalized, part)
			}
		default:
			textOnly = false
			copy := map[string]any{}
			for key, v := range part {
				if key != "cache_control" {
					copy[key] = v
				}
			}
			normalized = append(normalized, copy)
		}
	}
	if textOnly {
		return strings.Join(texts, "\n")
	}
	return normalized
}

func prefixHeadKey(head []any) string {
	blob, err := json.Marshal(head)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(blob)
	return "pfx-" + hex.EncodeToString(digest[:])[:16]
}
