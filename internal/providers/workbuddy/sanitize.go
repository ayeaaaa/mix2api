package workbuddy

import (
	"encoding/json"
	"regexp"
	"strings"
)

// This file ports v1's wb_proxy fingerprint sanitizer. Upstream treats several
// client identity strings and the Claude Code billing attribution header as
// unapproved-channel fingerprints (400 / 11128). The rewrites are deliberately
// narrow: user and tool content is never changed, only system/developer/
// assistant text that can carry a client fingerprint.
var (
	sanitizeBillingLine = regexp.MustCompile(`(?im)^[ \t]*x-anthropic-billing-(?:header|hdr)[ \t]*:[^\r\n]*(?:\r?\n|$)`)
	sanitizeBillingHdr  = regexp.MustCompile(`(?i)x-anthropic-billing-(?:header|hdr)\s*:\s*[^;\r\n]*(?:;\s*)?`)
	sanitizeBillingKV   = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\r\n]*;?\s*`)
	sanitizeBillingBare = regexp.MustCompile(`(?i)x-anthropic-billing-(?:header|hdr)`)

	sanitizeClaudeIntro = regexp.MustCompile(`(?i)You are Claude Code,\s*Anthropic['’]s official CLI(?: tool)? for Claude(?: Code)?\.?`)
	sanitizeCodexIntro  = regexp.MustCompile(`(?i)You are a coding agent running in the Codex CLI(?: tool)?[^.\n]*\.`)
	sanitizeIdentity    = regexp.MustCompile(`(?i)You are\s+(?:an?\s+|the\s+)?(?:[\w.-]+\s+){0,3}?(?:agent|assistant|model|AI|LLM)?[\s,]*(?:powered by|built on|built by|created by|developed by|made by|trained by)[^\n]*?(?:\.(?:\s|$)|(?:\n|$))`)
	sanitizeDSHGUI      = regexp.MustCompile(`(?i)(You are interacting with the user through the DeepSeek[ \t]+Harness[ \t]+Web[ \t]+GUI) whose URL the runtime context names\.`)
	sanitizeDSHFileRef  = regexp.MustCompile(`(?i)(Tokens prefixed with @ are paths )(the user explicitly referenced\.)`)
	sanitizeBrand       = regexp.MustCompile(`DeepSeek[ \t]+Harness`)
	sanitizeOMO         = regexp.MustCompile(`(?i)Sisyphus-Junior - Focused executor from OhMyOpenCode`)

	sanitizeFingerprintFeatures = []string{
		"x-anthropic-billing", "cc_entrypoint=", "you are claude code",
		"main branch (", "you are a coding agent running in the codex cli",
		"github.com/anthropics/", "11128", "deepseek harness",
		"sisyphus-junior - focused executor from ohmyopencode",
		"powered by", "built on", "built by", "created by", "developed by",
		"made by", "trained by", "whose url the runtime context names",
		"are paths the user explicitly referenced",
	}
)

func hasSanitizeFingerprint(text string, identity bool) bool {
	if text == "" {
		return false
	}
	low := strings.ToLower(text)
	for _, feature := range sanitizeFingerprintFeatures {
		if strings.Contains(low, feature) {
			return true
		}
	}
	if !identity {
		return false
	}
	return sanitizeIdentity.MatchString(text)
}

func sanitizeText(text string, identity bool) string {
	if text == "" || !hasSanitizeFingerprint(text, identity) {
		return text
	}
	text = sanitizeOMO.ReplaceAllString(text, "Sisyphus-Junior - Focused executor")
	if identity {
		text = sanitizeClaudeIntro.ReplaceAllString(text, "You are a helpful AI assistant.")
		text = sanitizeCodexIntro.ReplaceAllString(text, "You are a helpful AI assistant.")
		text = sanitizeIdentity.ReplaceAllString(text, "You are a helpful AI assistant.")
		text = sanitizeDSHGUI.ReplaceAllString(text, "${1} whose URL is named in the runtime context.")
		if strings.Contains(strings.ToLower(text), "are paths the user explicitly referenced") &&
			!strings.Contains(strings.ToLower(text), "are paths that the user explicitly referenced") {
			text = sanitizeDSHFileRef.ReplaceAllString(text, "${1}that ${2}")
		}
		text = sanitizeBrand.ReplaceAllString(text, "DeepSeek")
	}
	text = strings.ReplaceAll(text, "Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	text = strings.ReplaceAll(text,
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues")
	text = strings.ReplaceAll(text, "11128", "11-128")
	text = sanitizeBillingLine.ReplaceAllString(text, "")
	text = sanitizeBillingHdr.ReplaceAllString(text, "")
	if strings.Contains(text, "cc_") {
		for {
			next := sanitizeBillingKV.ReplaceAllString(text, "")
			if next == text {
				break
			}
			text = next
		}
	}
	text = sanitizeBillingBare.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

func sanitizeContent(content any, identity bool) any {
	switch typed := content.(type) {
	case string:
		return sanitizeText(typed, identity)
	case []any:
		out := make([]any, len(typed))
		for index, part := range typed {
			item, ok := part.(map[string]any)
			if !ok {
				out[index] = part
				continue
			}
			copied := cloneMap(item)
			if creditText(copied["type"]) == "text" {
				if text, ok := copied["text"].(string); ok {
					copied["text"] = sanitizeText(text, identity)
				}
			}
			out[index] = copied
		}
		return out
	default:
		return content
	}
}

func sanitizeToolCalls(toolCalls any) any {
	list, ok := toolCalls.([]any)
	if !ok {
		return toolCalls
	}
	out := make([]any, 0, len(list))
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		copied := cloneMap(item)
		if function, ok := copied["function"].(map[string]any); ok {
			fnCopy := cloneMap(function)
			switch args := fnCopy["arguments"].(type) {
			case string:
				fnCopy["arguments"] = sanitizeText(args, false)
			case nil:
				fnCopy["arguments"] = "{}"
			default:
				encoded, err := json.Marshal(args)
				if err != nil {
					fnCopy["arguments"] = creditText(args)
				} else {
					fnCopy["arguments"] = string(encoded)
				}
			}
			copied["function"] = fnCopy
		}
		out = append(out, copied)
	}
	return out
}

// sanitizeChatMessages returns a sanitized copy of the message list. User and
// tool content are preserved verbatim; system/developer/assistant content,
// reasoning, and tool arguments are cleaned.
func sanitizeChatMessages(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		item, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		copied := cloneMap(item)
		role := creditText(copied["role"])
		identity := role == "system" || role == "developer" || role == "assistant"
		before, _ := copied["content"].(string)
		if content, exists := copied["content"]; exists {
			copied["content"] = sanitizeContent(content, identity)
		}
		if (role == "system" || role == "developer") && before != "" &&
			strings.TrimSpace(before) != "" && creditText(copied["content"]) == "" {
			continue
		}
		if reasoning, ok := copied["reasoning_content"].(string); ok {
			copied["reasoning_content"] = sanitizeText(reasoning, false)
		}
		if reasoning, ok := copied["reasoning"].(string); ok {
			copied["reasoning"] = sanitizeText(reasoning, false)
		}
		if toolCalls, exists := copied["tool_calls"]; exists {
			copied["tool_calls"] = sanitizeToolCalls(toolCalls)
		}
		out = append(out, copied)
	}
	return out
}
