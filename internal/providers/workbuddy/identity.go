package workbuddy

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Verified default versions. These are the values pinned by v1's captures and
// unpacked official packages; panel overrides change only the emitted strings.
const (
	defaultDesktopVersionIntl = "5.8.4"
	defaultDesktopVersionCN   = "5.8.3"
	defaultDesktopCLI         = "2.161.0"
	defaultVSCodeProduct      = "4.12.39997873"
	defaultVSCodeIDE          = "1.119.0"
	defaultCLI                = "2.168.2"
	defaultSDK                = "6.25.0"
	defaultNode               = "v22.21.1"

	cnChatBaseDefault = "https://copilot.tencent.com"
	intlChatBase      = "https://www.workbuddy.ai"
	cnVSCodeBase      = "https://www.workbuddy.cn"
	intlDomain        = "www.workbuddy.ai"
	cnDesktopDomain   = "copilot.tencent.com"
	cnVSCodeDomain    = "www.workbuddy.cn"

	productDesktop = "workbuddy"
	productVSCode  = "vscode"
	productCLI     = "cli"
)

// Versions is a set of identity version overrides. Empty fields fall back to
// the verified defaults, so an empty Versions means "use the shipped values".
type Versions struct {
	DesktopIntl   string `json:"desktop_intl,omitempty"`
	DesktopCN     string `json:"desktop_cn,omitempty"`
	DesktopCLI    string `json:"desktop_cli,omitempty"`
	VSCodeProduct string `json:"vscode_product,omitempty"`
	VSCodeIDE     string `json:"vscode_ide,omitempty"`
	CLI           string `json:"cli,omitempty"`
	SDK           string `json:"sdk,omitempty"`
	Node          string `json:"node,omitempty"`
}

// DefaultVersions returns the verified default identity versions.
func DefaultVersions() Versions {
	return Versions{
		DesktopIntl:   defaultDesktopVersionIntl,
		DesktopCN:     defaultDesktopVersionCN,
		DesktopCLI:    defaultDesktopCLI,
		VSCodeProduct: defaultVSCodeProduct,
		VSCodeIDE:     defaultVSCodeIDE,
		CLI:           defaultCLI,
		SDK:           defaultSDK,
		Node:          defaultNode,
	}
}

func (v Versions) withDefaults() Versions {
	out := Versions{
		DesktopIntl:   firstNonEmpty(v.DesktopIntl, defaultDesktopVersionIntl),
		DesktopCN:     firstNonEmpty(v.DesktopCN, defaultDesktopVersionCN),
		DesktopCLI:    firstNonEmpty(v.DesktopCLI, defaultDesktopCLI),
		VSCodeProduct: firstNonEmpty(v.VSCodeProduct, defaultVSCodeProduct),
		VSCodeIDE:     firstNonEmpty(v.VSCodeIDE, defaultVSCodeIDE),
		CLI:           firstNonEmpty(v.CLI, defaultCLI),
		SDK:           firstNonEmpty(v.SDK, defaultSDK),
		Node:          firstNonEmpty(v.Node, defaultNode),
	}
	return out
}

func firstNonEmpty(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

// versionsFromAny parses a v1 identity_versions object. Following v1, a single
// invalid known value discards the whole override set rather than mixing valid
// and invalid strings.
func versionsFromAny(value any) Versions {
	switch direct := value.(type) {
	case Versions:
		return sanitizeVersions(direct)
	case nil:
		return Versions{}
	}
	raw, ok := value.(map[string]any)
	if !ok {
		return Versions{}
	}
	out := Versions{}
	valid := true
	set := func(key string, target *string) {
		item, exists := raw[key]
		if !exists {
			return
		}
		text, ok := versionText(item)
		if !ok {
			valid = false
			return
		}
		if text == "" {
			return
		}
		if !validIdentityVersion(text) {
			valid = false
			return
		}
		*target = text
	}
	set("desktop_intl", &out.DesktopIntl)
	set("desktop_cn", &out.DesktopCN)
	set("desktop_cli", &out.DesktopCLI)
	set("vscode_product", &out.VSCodeProduct)
	set("vscode_ide", &out.VSCodeIDE)
	set("cli", &out.CLI)
	set("sdk", &out.SDK)
	set("node", &out.Node)
	if !valid {
		return Versions{}
	}
	return out
}

func sanitizeVersions(value Versions) Versions {
	out := Versions{}
	clean := func(text string) string {
		text = strings.TrimSpace(text)
		if !validIdentityVersion(text) {
			return ""
		}
		return text
	}
	out.DesktopIntl = clean(value.DesktopIntl)
	out.DesktopCN = clean(value.DesktopCN)
	out.DesktopCLI = clean(value.DesktopCLI)
	out.VSCodeProduct = clean(value.VSCodeProduct)
	out.VSCodeIDE = clean(value.VSCodeIDE)
	out.CLI = clean(value.CLI)
	out.SDK = clean(value.SDK)
	out.Node = clean(value.Node)
	return out
}

func versionText(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v), true
	case json.Number:
		return strings.TrimSpace(string(v)), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), true
	case int:
		return strconv.Itoa(v), true
	case int8:
		return strconv.FormatInt(int64(v), 10), true
	case int16:
		return strconv.FormatInt(int64(v), 10), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case uint:
		return strconv.FormatUint(uint64(v), 10), true
	case uint8:
		return strconv.FormatUint(uint64(v), 10), true
	case uint16:
		return strconv.FormatUint(uint64(v), 10), true
	case uint32:
		return strconv.FormatUint(uint64(v), 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	case nil:
		return "", true
	default:
		return "", false
	}
}

func validIdentityVersion(text string) bool {
	if len(text) == 0 || len(text) > 64 {
		return false
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			continue
		}
		if i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return false
	}
	return true
}

func versionsValues(v Versions) any {
	raw := map[string]any{}
	if v.DesktopIntl != "" {
		raw["desktop_intl"] = v.DesktopIntl
	}
	if v.DesktopCN != "" {
		raw["desktop_cn"] = v.DesktopCN
	}
	if v.DesktopCLI != "" {
		raw["desktop_cli"] = v.DesktopCLI
	}
	if v.VSCodeProduct != "" {
		raw["vscode_product"] = v.VSCodeProduct
	}
	if v.VSCodeIDE != "" {
		raw["vscode_ide"] = v.VSCodeIDE
	}
	if v.CLI != "" {
		raw["cli"] = v.CLI
	}
	if v.SDK != "" {
		raw["sdk"] = v.SDK
	}
	if v.Node != "" {
		raw["node"] = v.Node
	}
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func normalizeProduct(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case productDesktop, "wb", "desktop":
		return productDesktop
	case productVSCode, "vsc", "ide":
		return productVSCode
	case productCLI, "codebuddy":
		return productCLI
	default:
		return productDesktop
	}
}

func cnChatBase() string {
	raw := strings.TrimSpace(os.Getenv("WB_CN_CHAT_UPSTREAM"))
	if raw == "" {
		return cnChatBaseDefault
	}
	base := strings.TrimRight(raw, "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return cnChatBaseDefault
	}
	return base
}

// endpointFor returns the exact v1 chat base URL for a realm and product.
func endpointFor(realm, product string) string {
	realm = realmOrDefault(realm)
	switch normalizeProduct(product) {
	case productVSCode:
		if realm == realmCN {
			return cnVSCodeBase
		}
		return intlChatBase
	default:
		if realm == realmCN {
			return cnChatBase()
		}
		return intlChatBase
	}
}

// domainFor returns the X-Domain fallback value. It deliberately does not
// follow WB_CN_CHAT_UPSTREAM: the override changes where requests are sent, not
// the domain recorded by the account/login flow.
func domainFor(realm, product string) string {
	realm = realmOrDefault(realm)
	switch normalizeProduct(product) {
	case productVSCode:
		if realm == realmCN {
			return cnVSCodeDomain
		}
		return intlDomain
	default:
		if realm == realmCN {
			return cnDesktopDomain
		}
		return intlDomain
	}
}

func domainForRealm(realm string) string {
	return domainFor(realm, productDesktop)
}

func desktopVersion(realm string) string {
	return desktopVersionWithVersions(realm, Versions{})
}

func desktopVersionWithVersions(realm string, versions Versions) string {
	versions = versions.withDefaults()
	if realmOrDefault(realm) == realmCN {
		return versions.DesktopCN
	}
	return versions.DesktopIntl
}

func desktopUA(realm string) string {
	return desktopUAWithVersions(realm, Versions{})
}

func desktopUAWithVersions(realm string, versions Versions) string {
	versions = versions.withDefaults()
	appVersion := versions.DesktopIntl
	product := "workbuddy-ai"
	if realmOrDefault(realm) == realmCN {
		appVersion = versions.DesktopCN
		product = "WorkBuddy"
	}
	return fmt.Sprintf("WorkBuddy/%s %s/%s CLI/%s",
		appVersion, product, appVersion, versions.DesktopCLI)
}

func vscodeIDEVersion() string {
	return vscodeIDEVersionWithVersions(Versions{})
}

func vscodeIDEVersionWithVersions(versions Versions) string {
	if strings.TrimSpace(versions.VSCodeIDE) != "" {
		return strings.TrimSpace(versions.VSCodeIDE)
	}
	if override := strings.TrimSpace(os.Getenv("WB_VSCODE_IDE_VERSION")); override != "" {
		return override
	}
	return defaultVSCodeIDE
}

func vscodeProductVersion() string {
	return vscodeProductVersionWithVersions(Versions{})
}

func vscodeProductVersionWithVersions(versions Versions) string {
	return firstNonEmpty(versions.VSCodeProduct, defaultVSCodeProduct)
}

func vscodeUA() string {
	return vscodeUAWithVersions(Versions{})
}

func vscodeUAWithVersions(versions Versions) string {
	return fmt.Sprintf("VSCode/%s CodeBuddy/%s",
		vscodeIDEVersionWithVersions(versions),
		vscodeProductVersionWithVersions(versions))
}

func cliVersion() string {
	return cliVersionWithVersions(Versions{})
}

func cliVersionWithVersions(versions Versions) string {
	return firstNonEmpty(versions.CLI, defaultCLI)
}

func cliUA() string {
	return cliUAWithVersions(Versions{})
}

func cliUAWithVersions(versions Versions) string {
	version := cliVersionWithVersions(versions)
	return fmt.Sprintf("CLI/%s CodeBuddy/%s", version, version)
}

func sdkVersion() string {
	return sdkVersionWithVersions(Versions{})
}

func sdkVersionWithVersions(versions Versions) string {
	return firstNonEmpty(versions.SDK, defaultSDK)
}

func nodeVersion() string {
	return nodeVersionWithVersions(Versions{})
}

func nodeVersionWithVersions(versions Versions) string {
	if override := strings.TrimSpace(versions.Node); override != "" {
		return override
	}
	if override := strings.TrimSpace(os.Getenv("WB_IDENTITY_NODE_VERSION")); override != "" {
		return override
	}
	return defaultNode
}

func identityOS() string {
	if override := strings.TrimSpace(os.Getenv("WB_IDENTITY_OS")); override != "" {
		return override
	}
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "MacOS"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}

func identityArch() string {
	if override := strings.TrimSpace(os.Getenv("WB_IDENTITY_ARCH")); override != "" {
		return override
	}
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	case "arm64":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

func desktopChatCompatEnabled() bool {
	return strings.TrimSpace(os.Getenv("WB_DESKTOP_CHAT_COMPAT")) == "1"
}

// cliStainlessHeaders mirrors the official OpenAI Node SDK 6.25.0 fingerprint
// headers sent by the CodeBuddy CLI.
func cliStainlessHeaders(versions Versions) map[string]string {
	return map[string]string{
		"x-stainless-lang":            "js",
		"x-stainless-package-version": sdkVersionWithVersions(versions),
		"x-stainless-os":              identityOS(),
		"x-stainless-arch":            identityArch(),
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": nodeVersionWithVersions(versions),
		"x-stainless-retry-count":     "0",
		"x-stainless-timeout":         "1200",
	}
}

var (
	fallbackRandomMu      sync.Mutex
	fallbackRandomCounter uint64
)

func randomBytes(n int) []byte {
	out := make([]byte, n)
	if _, err := rand.Read(out); err == nil {
		return out
	}
	// crypto/rand should never fail; keep IDs unique if it does.
	fallbackRandomMu.Lock()
	fallbackRandomCounter++
	seed := uint64(time.Now().UnixNano()) ^ fallbackRandomCounter
	fallbackRandomMu.Unlock()
	sha := sha256.Sum256([]byte(strconv.FormatUint(seed, 10)))
	for i := range out {
		out[i] = sha[i%len(sha)]
		if i >= len(sha) {
			sha = sha256.Sum256(sha[:])
		}
	}
	return out
}

func uuid7Bytes() [16]byte {
	var out [16]byte
	ms := uint64(time.Now().UnixMilli()) & ((uint64(1) << 48) - 1)
	binary.BigEndian.PutUint64(out[0:8], ms<<16)
	random := randomBytes(10)
	out[6] = 0x70 | (random[0] & 0x0f)
	out[7] = random[1]
	out[8] = 0x80 | (random[2] & 0x3f)
	copy(out[9:16], random[3:10])
	return out
}

func formatUUID(value [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func uuid7() string {
	return formatUUID(uuid7Bytes())
}

func newConversationID() string {
	return uuid7()
}

// conversationIDFor derives a stable dashed UUID-shaped conversation id from
// the account uid and session key, matching v1's sha256("uid|key") scheme.
// Empty session keys return "" so the caller can fall back to a fresh id.
func conversationIDFor(uid, sessionKey string) string {
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%s", uid, key)))
	text := hex.EncodeToString(digest[:])[:32]
	raw := []byte(text)
	raw[12] = '4'
	raw[16] = 'a'
	text = string(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		text[0:8], text[8:12], text[12:16], text[16:20], text[20:32])
}

func newMessageID() string {
	value := uuid7Bytes()
	return hex.EncodeToString(value[:])
}

func newTurnRequestID() string {
	return newMessageID()
}

func newRequestTraceID() string {
	random := randomBytes(16)
	random[6] = (random[6] & 0x0f) | 0x40
	random[8] = (random[8] & 0x3f) | 0x80
	var value [16]byte
	copy(value[:], random)
	return formatUUID(value)
}

func randomHex(bytesCount int) string {
	return hex.EncodeToString(randomBytes(bytesCount))
}

func traceHeaders(withParent bool) http.Header {
	trace := newMessageID()
	span := randomHex(8)
	headers := http.Header{}
	setHeader(headers, "X-Trace-ID", trace)
	setHeader(headers, "traceparent", "00-"+trace+"-"+span+"-01")
	setHeader(headers, "b3", trace+"-"+span+"-1")
	setHeader(headers, "X-B3-TraceId", trace)
	setHeader(headers, "X-B3-SpanId", span)
	setHeader(headers, "X-B3-Sampled", "1")
	if withParent {
		parent := randomHex(8)
		setHeader(headers, "X-B3-ParentSpanId", parent)
		setHeader(headers, "b3", trace+"-"+span+"-1-"+parent)
	}
	return headers
}

func vscodeTraceHeaders() http.Header {
	trace := newMessageID()
	span := randomHex(8)
	parent := randomHex(8)
	headers := http.Header{}
	setHeader(headers, "X-Trace-ID", trace)
	setHeader(headers, "b3", trace+"-"+span+"-1-"+parent)
	setHeader(headers, "X-B3-TraceId", trace)
	setHeader(headers, "X-B3-ParentSpanId", parent)
	setHeader(headers, "X-B3-SpanId", span)
	setHeader(headers, "X-B3-Sampled", "1")
	return headers
}

func setHeader(headers http.Header, name, value string) {
	headers.Set(name, value)
}

func setHeaders(headers http.Header, values map[string]string) {
	for name, value := range values {
		headers.Set(name, value)
	}
}

func mergeHeaders(headers, values http.Header) {
	for name, items := range values {
		for _, value := range items {
			headers.Set(name, value)
		}
	}
}

// IdentityOptions carries all per-request inputs for buildIdentityHeaders.
// Versions holds only panel overrides; empty fields use the verified defaults.
type IdentityOptions struct {
	Realm       string
	Product     string
	UID         string
	Token       string
	Versions    Versions
	PrivateData string

	ConversationID string
	SessionKey     string
	TurnRequestID  string
	Model          string

	EnterpriseID string
	TenantID     string
	Department   string

	DeviceToken      string
	DeviceTokenError string
}

// buildIdentityHeaders returns the v1-compatible outbound identity header set
// for one chat request. It contains no credentials beyond the Authorization /
// X-API-Key headers the upstream requires.
func buildIdentityHeaders(options IdentityOptions) http.Header {
	product := normalizeProduct(options.Product)
	realm := realmOrDefault(options.Realm)
	versions := options.Versions
	isCN := realm == realmCN

	explicitConversation := strings.TrimSpace(options.ConversationID)
	conversation := explicitConversation
	if conversation == "" {
		if product == productVSCode {
			conversation = newMessageID()
		} else {
			conversation = newConversationID()
		}
	}
	messageID := newMessageID()
	conversationRequestID := newMessageID()

	if product == productCLI {
		if explicitConversation == "" {
			if derived := conversationIDFor(options.UID, options.SessionKey); derived != "" {
				conversation = derived
			}
		}
		conversationRequestID = strings.TrimSpace(options.TurnRequestID)
		if conversationRequestID == "" {
			conversationRequestID = newTurnRequestID()
		}
	}

	uid := strings.TrimSpace(options.UID)
	if uid == "" {
		uid = "anonymous"
	}

	headers := http.Header{}
	switch product {
	case productVSCode:
		setHeaders(headers, map[string]string{
			"X-Agent-Intent":            "craft",
			"X-Agent-Type":              "main",
			"X-IDE-Type":                "VSCode",
			"X-IDE-Name":                "VSCode",
			"X-IDE-Version":             vscodeIDEVersionWithVersions(versions),
			"X-Product-Version":         vscodeProductVersionWithVersions(versions),
			"X-Env-ID":                  "production",
			"X-Product":                 "SaaS",
			"X-Domain":                  domainFor(realm, product),
			"User-Agent":                vscodeUAWithVersions(versions),
			"X-User-Id":                 uid,
			"Authorization":             "Bearer " + options.Token,
			"X-Request-Trace-Id":        newRequestTraceID(),
			"X-Conversation-ID":         conversation,
			"X-Conversation-Request-ID": conversationRequestID,
			"X-Conversation-Message-ID": messageID,
			"X-Request-ID":              conversationRequestID,
		})
		if options.Model != "" {
			now := strconv.FormatInt(time.Now().UnixMilli(), 10)
			setHeader(headers, "X-Model-ID", options.Model)
			setHeader(headers, "monitor_promptPrepareStartTime", now)
			setHeader(headers, "monitor_httpSendTime", now)
		}
		if !isCN {
			setHeader(headers, "X-Private-Data", "true")
		}
		mergeHeaders(headers, vscodeTraceHeaders())
	case productCLI:
		version := cliVersionWithVersions(versions)
		setHeaders(headers, map[string]string{
			"X-Agent-Intent":            "craft",
			"X-Agent-Purpose":           "conversation",
			"X-Agent-Runtime":           "other",
			"X-Agent-Type":              "main",
			"X-IDE-Type":                "CLI",
			"X-IDE-Name":                "CLI",
			"X-IDE-Version":             version,
			"X-Product":                 "SaaS",
			"X-Domain":                  domainFor(realm, product),
			"User-Agent":                fmt.Sprintf("CLI/%s CodeBuddy/%s", version, version),
			"X-User-Id":                 uid,
			"Authorization":             "Bearer " + options.Token,
			"X-Conversation-ID":         conversation,
			"X-Conversation-Request-ID": conversationRequestID,
			"X-Root-Request-ID":         conversationRequestID,
			"X-Conversation-Message-ID": messageID,
			"X-Request-ID":              messageID,
		})
		if options.EnterpriseID != "" {
			setHeader(headers, "X-Private-Data", "true")
		} else {
			setHeader(headers, "X-Private-Data", "false")
		}
		setHeaders(headers, cliStainlessHeaders(versions))
		mergeHeaders(headers, traceHeaders(true))
	default:
		setHeaders(headers, map[string]string{
			"X-Agent-Intent":            "craft",
			"X-Agent-Purpose":           "conversation",
			"X-Agent-Type":              "main",
			"X-IDE-Name":                "WorkBuddy",
			"X-IDE-Type":                "WorkBuddy",
			"X-IDE-Version":             desktopVersionWithVersions(realm, versions),
			"X-Private-Data":            "true",
			"X-Product":                 "SaaS",
			"X-Domain":                  domainFor(realm, product),
			"User-Agent":                desktopUAWithVersions(realm, versions),
			"X-User-Id":                 uid,
			"Authorization":             "Bearer " + options.Token,
			"X-Conversation-ID":         conversation,
			"X-Conversation-Request-ID": conversationRequestID,
			"X-Root-Request-ID":         conversationRequestID,
			"X-Conversation-Message-ID": messageID,
			"X-Request-ID":              messageID,
		})
		if desktopChatCompatEnabled() {
			mergeHeaders(headers, traceHeaders(true))
			sdkHeaders := cliStainlessHeaders(versions)
			delete(sdkHeaders, "x-stainless-timeout")
			setHeaders(headers, sdkHeaders)
		} else {
			mergeHeaders(headers, traceHeaders(false))
		}
	}

	// Static API keys authenticate with either header; v1 sends both because the
	// /v3/config model list is shorter when X-API-Key is missing.
	if strings.HasPrefix(options.Token, "ck_") || strings.HasPrefix(options.Token, "pt_") {
		setHeader(headers, "X-API-Key", options.Token)
	}

	if options.EnterpriseID != "" {
		setHeader(headers, "X-Enterprise-Id", options.EnterpriseID)
		tenant := options.TenantID
		if tenant == "" {
			tenant = options.EnterpriseID
		}
		setHeader(headers, "X-Tenant-Id", tenant)
	}
	if options.Department != "" {
		setHeader(headers, "X-Department-Info", options.Department)
	}

	if options.PrivateData != "" {
		switch strings.ToLower(strings.TrimSpace(options.PrivateData)) {
		case "true", "false":
			setHeader(headers, "X-Private-Data", strings.ToLower(strings.TrimSpace(options.PrivateData)))
		}
	}

	// v1 only sends the device token for CN desktop/VSCode identities. CLI has
	// no device-token logic at all.
	if isCN && product != productCLI && options.DeviceToken != "" {
		setHeader(headers, "X-Device-Token", options.DeviceToken)
	} else if product == productVSCode && options.EnterpriseID == "" && options.DeviceTokenError != "" {
		setHeader(headers, "X-Device-Token-Error", options.DeviceTokenError)
	}

	return headers
}

// gzipBody compresses an already-serialized chat body. The gzip mtime is zero
// so the same input always produces the same bytes.
func gzipBody(data []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.DefaultCompression)
	if err != nil {
		return nil, err
	}
	writer.Header.ModTime = time.Time{}
	writer.Header.OS = 255
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
