package providers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
		if !haveUser {
			switch role {
			case "system", "developer":
				head = append(head, value)
			case "user":
				head = append(head, value)
				haveUser = true
				if !stable {
					return prefixHeadKey(head)
				}
			default:
				return ""
			}
		} else if role == "assistant" {
			head = append(head, value)
			return prefixHeadKey(head)
		}
	}
	if !haveUser {
		return ""
	}
	return prefixHeadKey(head)
}

func prefixHeadKey(head []any) string {
	blob, err := json.Marshal(head)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(blob)
	return "pfx-" + hex.EncodeToString(digest[:])[:16]
}
