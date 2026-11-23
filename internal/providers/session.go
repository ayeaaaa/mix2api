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
	if len(req.Messages) > 0 {
		return req.Messages
	}
	if req.Raw == nil {
		return nil
	}
	raw, ok := req.Raw["messages"]
	if !ok || len(raw) == 0 {
		return nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil
	}
	return messages
}

// DerivePrefixSessionKey returns a stable conversation key from the request's
// first messages. It is the v2 counterpart of v1's prefix-based session
// affinity: without it, clients that send no conversation header rotate across
// the whole account pool every turn and lose the upstream account-scoped
// prefix cache. The explicit session key still wins; callers only use this
// helper when it is empty.
//
// The default head is the first two messages (system + first user), matching
// v1's WB_AFFINITY_BY_PREFIX. stable=true also includes the first assistant
// reply, which separates two conversations that open with the same user line.
func DerivePrefixSessionKey(messages []json.RawMessage, stable bool) string {
	if len(messages) == 0 {
		return ""
	}
	limit := 2
	if stable && len(messages) > 2 {
		limit = 3
	}
	if len(messages) < limit {
		limit = len(messages)
	}
	head := make([]any, 0, limit)
	for _, raw := range messages[:limit] {
		if len(raw) == 0 {
			return ""
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return ""
		}
		head = append(head, value)
	}
	blob, err := json.Marshal(head)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(blob)
	return "pfx-" + hex.EncodeToString(digest[:])[:16]
}
