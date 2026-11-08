package codearts

import (
	"fmt"
	"net/http"
	"strings"
)

// ErrorKind classifies a CodeArts chat failure. The kind drives failover and
// account bookkeeping; HTTPStatus maps it to the client-visible v1 status.
type ErrorKind int

const (
	// KindBadRequest is a request validation failure. It is never retried on
	// another account because the request, not the account, is at fault.
	KindBadRequest ErrorKind = iota

	// KindAuth means the upstream rejected the account credentials. The
	// account is disabled (terminal) and the request may fail over.
	KindAuth

	// KindRateLimited is an upstream HTTP 429 or an equivalent throttling
	// event. The account is cooled softly and the request may fail over.
	KindRateLimited

	// KindConcurrency is CodeArts' 400 "too many concurrent sessions" answer.
	// It is retried once on the same account, then cooled and failed over.
	KindConcurrency

	// KindCreditExhausted is the account-level "credits are empty" 400. The
	// account is parked and the request fails over to another account.
	KindCreditExhausted

	// KindUnavailable means no local account could serve the request, or the
	// upstream could not be reached.
	KindUnavailable

	// KindUpstream is any other upstream failure.
	KindUpstream
)

// APIError is a classified CodeArts failure. Message is already redacted and
// bounded, so it is safe to persist as a state reason or return to a client.
type APIError struct {
	Kind    ErrorKind
	Status  int
	Message string
}

// Error is the internal logging form. The API layer uses SafeMessage for the
// wire response and never writes this string to a client.
func (e *APIError) Error() string {
	if e == nil {
		return "codearts: upstream error"
	}
	switch {
	case e.Status > 0 && e.Message != "":
		return fmt.Sprintf("codearts: HTTP %d: %s", e.Status, e.Message)
	case e.Status > 0:
		return fmt.Sprintf("codearts: HTTP %d", e.Status)
	case e.Message != "":
		return "codearts: " + e.Message
	default:
		return "codearts: upstream error"
	}
}

// HTTPStatus maps the kind to the v1 chat error status. Authentication
// failures are 502 because the client's API key is not the rejected
// credential. Concurrency is a 429 like v1's CodeArts mapping.
func (e *APIError) HTTPStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	switch e.Kind {
	case KindBadRequest, KindCreditExhausted:
		return http.StatusBadRequest
	case KindRateLimited, KindConcurrency:
		return http.StatusTooManyRequests
	case KindUnavailable:
		return http.StatusServiceUnavailable
	case KindAuth:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}

// ErrorType returns the OpenAI error type for the kind.
func (e *APIError) ErrorType() string {
	if e == nil {
		return "invalid_request_error"
	}
	switch e.Kind {
	case KindRateLimited, KindConcurrency:
		return "rate_limit_error"
	default:
		return "invalid_request_error"
	}
}

// SafeMessage returns the redacted client-facing message without the internal
// prefix or status label.
func (e *APIError) SafeMessage() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.Message)
}

// newChatError builds a redacted, bounded APIError for one account.
func newChatError(kind ErrorKind, status int, message string, auth Auth) *APIError {
	return &APIError{
		Kind:    kind,
		Status:  status,
		Message: redactChatMessage(message, auth),
	}
}

// redactChatMessage removes every known credential, masks proxy userinfo, and
// caps the text at v1's 500-rune error budget.
func redactChatMessage(message string, auth Auth) string {
	values := codeartsSecretValues(auth)
	if strings.TrimSpace(auth.AccessKeyID) != "" {
		values = append(values, auth.AccessKeyID)
	}
	message = redactText(message, values)
	message = maskProxyURL(message)
	message = truncateRunes(message, 500)
	return strings.TrimSpace(message)
}
