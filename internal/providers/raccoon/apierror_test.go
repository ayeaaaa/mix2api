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
			name:    "not found maps upstream",
			err:     &APIError{Kind: KindNotFound, Status: http.StatusNotFound},
			status:  http.StatusBadGateway,
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
