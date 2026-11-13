package raccoon

import (
	"net/http"
	"testing"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

var _ providers.HTTPError = (*APIError)(nil)

func TestAPIErrorHTTPMapping(t *testing.T) {
	cases := []struct {
		name    string
		err     *APIError
		status  int
		errType string
	}{
		{
			name:    "bad request",
			err:     &APIError{Kind: KindBadRequest, Status: http.StatusBadRequest},
			status:  http.StatusBadRequest,
			errType: "invalid_request_error",
		},
		{
			name:    "auth",
			err:     &APIError{Kind: KindAuth, Status: http.StatusUnauthorized},
			status:  http.StatusBadGateway,
			errType: "invalid_request_error",
		},
		{
			name:    "rate limited",
			err:     &APIError{Kind: KindRateLimited, Status: http.StatusTooManyRequests},
			status:  http.StatusTooManyRequests,
			errType: "rate_limit_error",
		},
		{
			name:    "unavailable",
			err:     &APIError{Kind: KindUnavailable, Status: http.StatusServiceUnavailable},
			status:  http.StatusServiceUnavailable,
			errType: "invalid_request_error",
		},
		{
			name:    "upstream",
			err:     &APIError{Kind: KindUpstream, Status: http.StatusBadGateway},
			status:  http.StatusBadGateway,
			errType: "invalid_request_error",
		},
		{
			name:    "network transient",
			err:     &APIError{Kind: KindTransient},
			status:  http.StatusServiceUnavailable,
			errType: "invalid_request_error",
		},
		{
			name:    "status transient",
			err:     &APIError{Kind: KindTransient, Status: http.StatusInternalServerError},
			status:  http.StatusBadGateway,
			errType: "invalid_request_error",
		},
		{
			name:    "not found maps conflict",
			err:     &APIError{Kind: KindNotFound, Status: http.StatusNotFound},
			status:  http.StatusConflict,
			errType: "invalid_request_error",
		},
		{
			name:    "nil",
			err:     nil,
			status:  http.StatusBadGateway,
			errType: "invalid_request_error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.HTTPStatus(); got != tc.status {
				t.Fatalf("HTTPStatus() = %d, want %d", got, tc.status)
			}
			if got := tc.err.ErrorType(); got != tc.errType {
				t.Fatalf("ErrorType() = %q, want %q", got, tc.errType)
			}
		})
	}
}

func TestAPIErrorSafeMessageIsUnprefixed(t *testing.T) {
	err := &APIError{Kind: KindBadRequest, Status: http.StatusBadRequest, Message: "bad input"}
	if got := err.SafeMessage(); got != "bad input" {
		t.Fatalf("SafeMessage = %q, want %q", got, "bad input")
	}
	if got := err.Error(); got == err.SafeMessage() || got != "raccoon: HTTP 400: bad input" {
		t.Fatalf("internal Error() = %q, want prefixed log form", got)
	}
	if got := (*APIError)(nil).SafeMessage(); got != "" {
		t.Fatalf("nil SafeMessage = %q", got)
	}
}

var _ providers.GatewayHint = (*APIError)(nil)

func TestAPIErrorGatewayHintMatchesV1CreditShape(t *testing.T) {
	credit := &APIError{
		Kind: KindBadRequest, Status: http.StatusBadRequest,
		Message: "insufficient credits; please top up",
	}
	want := "account credits exhausted at upstream; waiting for daily check-in to restore"
	if got := credit.GatewayHint(); got != want {
		t.Fatalf("GatewayHint = %q, want %q", got, want)
	}
	payment := &APIError{Kind: KindBadRequest, Status: http.StatusPaymentRequired}
	if got := payment.GatewayHint(); got != want {
		t.Fatalf("402 GatewayHint = %q, want %q", got, want)
	}
	for _, err := range []*APIError{
		nil,
		{Kind: KindBadRequest, Status: http.StatusBadRequest, Message: "concurrent session limit"},
		{Kind: KindRateLimited, Status: http.StatusTooManyRequests, Message: "too many requests"},
		{Kind: KindBadRequest, Status: http.StatusBadRequest, Message: "insufficient quota"},
	} {
		if got := err.GatewayHint(); got != "" {
			t.Fatalf("unexpected hint for %#v: %q", err, got)
		}
	}
}
