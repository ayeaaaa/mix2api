package workbuddy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrorKind classifies one WorkBuddy chat failure. The kind controls retry,
// failover, and account-state behavior; Status keeps the upstream HTTP status
// when there was one.
type ErrorKind int

const (
	// KindInvalid is a request the upstream rejected (HTTP 400). It is never
	// retried on another account.
	KindInvalid ErrorKind = iota

	// KindAuth is an authentication failure. The client's gateway key is not
	// the rejected credential, so the wire status is 502.
	KindAuth

	// KindRateLimited is an upstream HTTP 429. A model or account cooldown is
	// recorded before another candidate is tried.
	KindRateLimited

	// KindRestricted is an account-level upstream restriction (typically a 403
	// with code 11140). The account is parked; the request may rotate.
	KindRestricted

	// KindContentRejected is a content-review 403. Another credential gets the
	// same answer for the same content, so it is passed straight through.
	KindContentRejected

	// KindUpstream is a 5xx or another upstream-side failure. Another account
	// may still work.
	KindUpstream

	// KindNetwork is a transport/DNS/TLS failure before a usable response.
	KindNetwork

	// KindUnavailable means the local pool has no usable account. The v1 wire
	// status is 503 so the caller knows waiting or adding an account helps.
	KindUnavailable
)

// APIError is one redacted WorkBuddy chat failure. Message is safe for the
// client: it never contains credentials, proxy userinfo, or account files.
type APIError struct {
	Kind    ErrorKind
	Status  int
	Message string

	// Terminal marks an auth failure that cannot be recovered by refreshing
	// this credential (a second 401 or a static API key 401).
	Terminal bool

	// ModelScoped is set for a 429 that names an upstream model window. The
	// account stays usable for sibling models.
	ModelScoped bool

	// RetryAfter and ResetAt are set for rate limits when the upstream names a
	// recovery clock. They are advisory and not part of providers.HTTPError.
	RetryAfter time.Duration
	ResetAt    time.Time
}

func (e *APIError) Error() string {
	if e == nil {
		return "workbuddy: upstream error"
	}
	message := strings.TrimSpace(e.Message)
	switch {
	case e.Status > 0 && message != "":
		return fmt.Sprintf("workbuddy: HTTP %d: %s", e.Status, message)
	case e.Status > 0:
		return fmt.Sprintf("workbuddy: HTTP %d", e.Status)
	case message != "":
		return "workbuddy: " + message
	default:
		return "workbuddy: upstream error"
	}
}

// HTTPStatus maps the classified failure to the v1 client-facing status.
func (e *APIError) HTTPStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	switch e.Kind {
	case KindInvalid:
		return http.StatusBadRequest
	case KindAuth:
		// The gateway API key was valid; the rejected credential is upstream's.
		return http.StatusBadGateway
	case KindRateLimited:
		return http.StatusTooManyRequests
	case KindRestricted, KindContentRejected:
		return http.StatusForbidden
	case KindNetwork:
		return http.StatusServiceUnavailable
	case KindUnavailable:
		return http.StatusServiceUnavailable
	case KindUpstream:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}

// SafeMessage returns the client-facing message without the internal prefix.
func (e *APIError) SafeMessage() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.Message)
}

// ErrorType returns the OpenAI-style error type for the classified failure.
func (e *APIError) ErrorType() string {
	if e == nil {
		return "invalid_request_error"
	}
	switch e.Kind {
	case KindRateLimited:
		return "rate_limit_error"
	case KindUpstream, KindNetwork, KindUnavailable:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// IsAuthError reports whether err is a WorkBuddy auth failure.
func IsAuthError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Kind == KindAuth
}

// IsTransientError reports whether err is a retryable upstream/pool failure.
func IsTransientError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Kind {
	case KindRateLimited, KindUpstream, KindNetwork, KindUnavailable:
		return true
	default:
		return false
	}
}

func newAPIError(kind ErrorKind, status int, message string) *APIError {
	return &APIError{
		Kind:    kind,
		Status:  status,
		Message: strings.TrimSpace(message),
	}
}
