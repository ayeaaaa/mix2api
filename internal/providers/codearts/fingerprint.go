package codearts

import (
	"crypto/sha256"
	"math/big"
	"net"
	"strings"
	"time"
)

const (
	defaultIDEUserAgent      = "opencode/126.11.3-20261124194420"
	defaultIDEClientVersion  = "26.11.300"
	defaultIDEPluginName     = "codemate_vscode"
	defaultIDEPluginVersion  = "26.11.300"
	defaultIDEIDEName        = "VSCode"
	defaultIDEIDEVersion     = "26.11.300"
	defaultIDEClientType     = "IDE"
	defaultIDEFunction       = "agent-general"
	defaultIDEEncryptCapable = "sm4-gcm"
)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ideFingerprint is the per-request identity and metadata emitted by CodeArts
// Agent 26.11.300. Values use the same prefixes and lengths as the observed
// client so downstream logs and fingerprints see the same shape.
type ideFingerprint struct {
	TraceID       string
	SpanID        string
	SessionID     string
	MessageID     string
	CreatedAt     string
	ModelID       string
	ModelName     string
	ClientIP      string
	UserAgent     string
	ClientType    string
	ClientVersion string
	PluginName    string
	PluginVersion string
	IDEName       string
	IDEVersion    string
	Function      string
	EncryptCap    string
}

func newIDEFingerprint(uid, model, chatID string, messages []any) (ideFingerprint, error) {
	trace, err := randomHex(16)
	if err != nil {
		return ideFingerprint{}, err
	}
	span, err := randomHex(8)
	if err != nil {
		return ideFingerprint{}, err
	}
	prompt := ideUserPrompt(messages)
	return ideFingerprint{
		TraceID:       trace,
		SpanID:        span,
		SessionID:     "ses_" + ideStableID(uid+"|"+chatID, 26),
		MessageID:     "msg_" + ideStableID(chatID+"|"+prompt, 26),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		ModelID:       model,
		ModelName:     ideModelName(model),
		ClientIP:      ideClientIP(),
		UserAgent:     envString("WB_CODEARTS_IDE_USER_AGENT", defaultIDEUserAgent),
		ClientType:    defaultIDEClientType,
		ClientVersion: envString("WB_CODEARTS_IDE_CLIENT_VERSION", defaultIDEClientVersion),
		PluginName:    envString("WB_CODEARTS_IDE_PLUGIN_NAME", defaultIDEPluginName),
		PluginVersion: envString("WB_CODEARTS_IDE_PLUGIN_VERSION", defaultIDEPluginVersion),
		IDEName:       envString("WB_CODEARTS_IDE_NAME", defaultIDEIDEName),
		IDEVersion:    envString("WB_CODEARTS_IDE_IDE_VERSION", defaultIDEIDEVersion),
		Function:      envString("WB_CODEARTS_IDE_FUNCTION", defaultIDEFunction),
		EncryptCap:    defaultIDEEncryptCapable,
	}, nil
}

// ideStableID returns length characters of a deterministic base62 digest.
func ideStableID(value string, length int) string {
	if length <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	number := new(big.Int).SetBytes(sum[:])
	base := big.NewInt(int64(len(base62Alphabet)))
	out := make([]byte, 0, length)
	mod := new(big.Int)
	for len(out) < length {
		number.DivMod(number, base, mod)
		out = append(out, base62Alphabet[mod.Int64()])
	}
	// The division produces least-significant digits first; the client IDs are
	// opaque, so a reversed encoding is just as stable and reads naturally.
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return string(out)
}

func ideModelName(model string) string {
	model = strings.TrimSpace(model)
	switch strings.ToLower(model) {
	case "glm-5.3":
		return "GLM-5.3"
	case "glm-5.2":
		return "GLM-5.2"
	case "glm-5.1":
		return "GLM-5.1"
	case "qwen3-vl-235b":
		return "Qwen3-VL-235B"
	default:
		return model
	}
}

// ideUserPrompt returns the last user message's text, matching the user_prompt
// field the CodeArts Agent sends for quota/metadata accounting.
func ideUserPrompt(messages []any) string {
	for index := len(messages) - 1; index >= 0; index-- {
		message, _ := messages[index].(map[string]any)
		if message == nil || !strings.EqualFold(ideText(message["role"]), "user") {
			continue
		}
		if text := ideContentText(message["content"]); text != "" {
			return text
		}
	}
	return ""
}

func ideContentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var builder strings.Builder
		for _, part := range value {
			object, _ := part.(map[string]any)
			if object == nil {
				continue
			}
			if kind := strings.ToLower(ideText(object["type"])); kind != "" && kind != "text" && kind != "input_text" {
				continue
			}
			builder.WriteString(ideText(object["text"]))
		}
		return builder.String()
	case map[string]any:
		if kind := strings.ToLower(ideText(value["type"])); kind == "" || kind == "text" || kind == "input_text" {
			return ideText(value["text"])
		}
	}
	return ""
}

func ideText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return ""
	}
}

func ideClientIP() string {
	if value := strings.TrimSpace(envString("WB_CODEARTS_CLIENT_IP", "")); value != "" {
		return value
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		network, ok := addr.(*net.IPNet)
		if !ok || network.IP == nil || network.IP.IsLoopback() || network.IP.IsLinkLocalUnicast() {
			continue
		}
		if ipv4 := network.IP.To4(); ipv4 != nil {
			return ipv4.String()
		}
	}
	return ""
}
