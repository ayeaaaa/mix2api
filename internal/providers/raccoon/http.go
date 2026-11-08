package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/secrets"
	"github.com/ayeaaaa/mix2api-v2/internal/core/transport"
)

const (
	// DefaultBaseURL is the production Raccoon API host used by v1.
	DefaultBaseURL = "https://xiaohuanxiong.com"

	// RefreshPath is the v1 token refresh endpoint.
	RefreshPath = "/api/web/auth/v1/refresh"

	// DefaultHTTPTimeout matches v1's 30-second token-refresh timeout.
	DefaultHTTPTimeout = 30 * time.Second

	// maxHTTPBodyBytes bounds response bodies so a broken upstream cannot
	// grow memory without limit.
	maxHTTPBodyBytes = 256 * 1024
)

// ErrorKind classifies an upstream Raccoon failure.
type ErrorKind int

const (
	// KindTransient transient failures may be retried or failed over: network
	// errors, timeouts, malformed responses, and 5xx.
	KindTransient ErrorKind = iota
	// KindAuth means the account credentials were rejected and need re-login.
	KindAuth
	// KindBadRequest means the request itself was rejected.
	KindBadRequest
	// KindNotFound means the requested resource does not exist.
	KindNotFound
	// KindRateLimited means the upstream returned HTTP 429.
	KindRateLimited
	// KindUpstream means the upstream returned a 5xx response.
	KindUpstream
)

// APIError is a classified Raccoon HTTP failure. Message is always redacted:
// callers may surface it in logs or state reason strings.
type APIError struct {
	Kind    ErrorKind
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e == nil {
		return "raccoon: upstream error"
	}
	switch {
	case e.Status > 0 && e.Message != "":
		return fmt.Sprintf("raccoon: HTTP %d: %s", e.Status, e.Message)
	case e.Status > 0:
		return fmt.Sprintf("raccoon: HTTP %d", e.Status)
	case e.Message != "":
		return "raccoon: " + e.Message
	default:
		return "raccoon: upstream error"
	}
}

// IsAuthError reports whether err is an authentication failure. A non-API
// error, such as a network or filesystem error, is never an auth failure.
func IsAuthError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Kind == KindAuth
}

// IsTransientError reports whether err is a retryable upstream failure.
func IsTransientError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Kind {
	case KindTransient, KindRateLimited, KindUpstream:
		return true
	default:
		return false
	}
}

// HTTPClientOptions configures a Raccoon HTTP client. A zero timeout uses
// DefaultHTTPTimeout; an empty BaseURL uses WB_RACCOON_BASE_URL when set and
// DefaultBaseURL otherwise.
type HTTPClientOptions struct {
	BaseURL string
	Timeout time.Duration
}

// HTTPClient sends Raccoon JSON requests through the shared core transport.
// Each request selects its proxy slot explicitly; it never falls back to a
// second transport or the process proxy environment.
type HTTPClient struct {
	client  *transport.Client
	baseURL string
	timeout time.Duration
}

// NewHTTPClient returns a Raccoon API client. A nil transport client is
// replaced with a default core transport so tests can inject one and
// production code cannot accidentally create a different HTTP stack.
func NewHTTPClient(client *transport.Client, opts HTTPClientOptions) *HTTPClient {
	if client == nil {
		client = transport.NewClient(transport.ClientOptions{})
	}
	baseURL := strings.TrimSpace(opts.BaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL()
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultHTTPTimeout
	}
	return &HTTPClient{
		client:  client,
		baseURL: strings.TrimRight(baseURL, "/"),
		timeout: timeout,
	}
}

func defaultBaseURL() string {
	if value := strings.TrimSpace(os.Getenv("WB_RACCOON_BASE_URL")); value != "" {
		return value
	}
	return DefaultBaseURL
}

// DoJSON sends one JSON request and returns the decoded object. RedactValues
// are the account's secrets; they and any encoded bearer/named token fields
// are removed from every returned error. Response bodies are bounded.
func (c *HTTPClient) DoJSON(
	ctx context.Context,
	method, path string,
	headers http.Header,
	body []byte,
	proxyURL string,
	redactValues ...string,
) (map[string]any, error) {
	if c == nil {
		return nil, errors.New("raccoon: http client is nil")
	}
	if ctx == nil {
		return nil, errors.New("raccoon: nil context")
	}
	if c.client == nil {
		return nil, errors.New("raccoon: http transport is nil")
	}

	requestCtx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	request, err := http.NewRequestWithContext(requestCtx, method, c.resolve(path), bytes.NewReader(body))
	if err != nil {
		return nil, &APIError{Kind: KindTransient, Message: "raccoon: build request failed"}
	}
	if headers != nil {
		request.Header = headers.Clone()
	}
	if request.Header.Get("Accept") == "" {
		request.Header.Set("Accept", "application/json")
	}
	if len(body) > 0 && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.client.Do(requestCtx, request, proxyURL)
	if err != nil {
		// Caller cancellation is not an upstream condition.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &APIError{
			Kind:    KindTransient,
			Message: redactText("raccoon: cannot reach upstream: "+err.Error(), redactValues...),
		}
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPBodyBytes))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &APIError{
			Kind:    KindTransient,
			Status:  response.StatusCode,
			Message: redactText("raccoon: read response failed: "+err.Error(), redactValues...),
		}
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, newAPIError(response.StatusCode, raw, redactValues...)
	}

	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, &APIError{
			Kind:    KindTransient,
			Status:  response.StatusCode,
			Message: fmt.Sprintf("raccoon: non-JSON response (HTTP %d)", response.StatusCode),
		}
	}
	return payload, nil
}

func (c *HTTPClient) resolve(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return c.baseURL + "/" + strings.TrimLeft(path, "/")
}

func newAPIError(status int, body []byte, redactValues ...string) *APIError {
	kind := KindTransient
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = KindAuth
	case status == http.StatusBadRequest:
		kind = KindBadRequest
	case status == http.StatusNotFound:
		kind = KindNotFound
	case status == http.StatusTooManyRequests:
		kind = KindRateLimited
	case status >= 500:
		kind = KindUpstream
	}
	return &APIError{
		Kind:    kind,
		Status:  status,
		Message: errorMessage(body, redactValues...),
	}
}

// errorMessage extracts the most useful upstream message and redacts it. It
// never returns the raw body unless no structured message exists, and even
// then the body is bounded and redacted.
func errorMessage(body []byte, redactValues ...string) string {
	var payload any
	_ = json.Unmarshal(body, &payload)

	message := ""
	if object, ok := payload.(map[string]any); ok {
		for _, key := range []string{"message", "error_msg", "error_description", "error"} {
			switch value := object[key].(type) {
			case string:
				message = strings.TrimSpace(value)
			case map[string]any:
				for _, nestedKey := range []string{"message", "error_msg"} {
					if nested, ok := value[nestedKey].(string); ok {
						message = strings.TrimSpace(nested)
						if message != "" {
							break
						}
					}
				}
			}
			if message != "" {
				break
			}
		}
		if message == "" {
			if data, ok := object["data"].(map[string]any); ok {
				if nested, ok := data["message"].(string); ok {
					message = strings.TrimSpace(nested)
				}
			}
		}
		if message == "" {
			if code, ok := object["code"]; ok && code != nil && code != "" && code != json.Number("0") && code != float64(0) {
				message = fmt.Sprintf("error_code=%v", code)
			}
		}
	}

	if message == "" {
		message = strings.TrimSpace(string(body))
		if len(message) > 500 {
			message = message[:500]
		}
	}
	return redactText(message, redactValues...)
}

var (
	bearerSecretPattern = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;"']+`)
	namedSecretPattern  = regexp.MustCompile(`(?i)(["']?(?:access_token|refresh_token|authorization_code|authorization)["']?\s*[:=]\s*)("(?:[^"\\]|\\.)*"|[^\s,;"'&}]+)`)
	jwtSecretPattern    = regexp.MustCompile(`[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)
)

// redactText removes known secret values plus common token-shaped fields and
// bearer/JWT forms. It is deliberately conservative: over-redacting an error
// is preferable to leaking a credential into a log or state reason.
func redactText(text string, values ...string) string {
	text = secrets.Redact(text, values...)
	text = bearerSecretPattern.ReplaceAllString(text, "Bearer <redacted>")
	text = namedSecretPattern.ReplaceAllString(text, "$1<redacted>")
	text = jwtSecretPattern.ReplaceAllString(text, "<redacted-jwt>")
	return text
}
