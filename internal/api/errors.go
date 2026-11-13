package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// apiFailure is the API layer's provider-neutral error envelope.
type apiFailure struct {
	Status  int
	Type    string
	Message string
	Hint    string
}

func (e *apiFailure) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// localFailure builds an invalid_request_error for a locally rejected request.
func localFailure(status int, message string) *apiFailure {
	return &apiFailure{Status: status, Type: "invalid_request_error", Message: message}
}

// providerFailure converts a provider error to the v1 wire envelope. Errors that
// implement providers.HTTPError declare their own status, type, and safe
// message. Every other error becomes a generic 502: raw local/provider error
// text is never echoed because it can contain auth-file paths or account UIDs.
func providerFailure(err error) *apiFailure {
	if err == nil {
		return localFailure(http.StatusBadGateway, "upstream request failed")
	}

	var httpErr providers.HTTPError
	if errors.As(err, &httpErr) {
		message := strings.TrimSpace(httpErr.SafeMessage())
		if message == "" {
			message = "upstream request failed"
		}
		status := httpErr.HTTPStatus()
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		errType := strings.TrimSpace(httpErr.ErrorType())
		if errType == "" {
			errType = "invalid_request_error"
		}
		failure := &apiFailure{Status: status, Type: errType, Message: message}
		var hintErr providers.GatewayHint
		if errors.As(err, &hintErr) {
			failure.Hint = strings.TrimSpace(hintErr.GatewayHint())
		}
		return failure
	}

	return localFailure(http.StatusBadGateway, "upstream request failed")
}

// writeFailure writes err as a v1 error envelope. *apiFailure values are used
// directly; any other error is mapped through providerFailure.
func writeFailure(w http.ResponseWriter, err error) {
	var failure *apiFailure
	if errors.As(err, &failure) && failure != nil {
		writeV1ErrorWithHint(w, failure.Status, failure.Type, failure.Message, failure.Hint)
		return
	}
	mapped := providerFailure(err)
	if mapped.Status == http.StatusTooManyRequests {
		var retry providers.RetryMetadata
		if errors.As(err, &retry) {
			writeV1ErrorWithRetryAndHint(w, mapped.Status, mapped.Type, mapped.Message, retry.RetryDelay(), retry.RetryReset(), mapped.Hint)
			return
		}
	}
	writeV1ErrorWithHint(w, mapped.Status, mapped.Type, mapped.Message, mapped.Hint)
}

// writeV1Error is the single v1 error envelope writer used by the generic API
// and the provider-route adapter.
func writeV1Error(w http.ResponseWriter, status int, errType, message string) {
	writeV1ErrorWithHint(w, status, errType, message, "")
}

// writeV1ErrorWithHint adds v1's optional gateway_hint beside the error. An
// empty hint omits the key entirely; no hint is ever synthesized here.
func writeV1ErrorWithHint(w http.ResponseWriter, status int, errType, message, hint string) {
	writeV1ErrorWithRetryAndHint(w, status, errType, message, 0, time.Time{}, hint)
}

// writeV1ErrorWithRetry writes the v1 envelope plus the rate-limit metadata v1
// clients consume: Retry-After, absolute reset headers, and the same fields in
// the JSON body.
func writeV1ErrorWithRetry(w http.ResponseWriter, status int, errType, message string, wait time.Duration, reset time.Time) {
	writeV1ErrorWithRetryAndHint(w, status, errType, message, wait, reset, "")
}

func writeV1ErrorWithRetryAndHint(w http.ResponseWriter, status int, errType, message string, wait time.Duration, reset time.Time, hint string) {
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	if strings.TrimSpace(errType) == "" {
		errType = "invalid_request_error"
	}
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(status)
	}
	seconds := int64(0)
	if wait > 0 {
		seconds = int64(math.Ceil(wait.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
	}
	validReset := time.Time{}
	if !reset.IsZero() && reset.After(time.Now()) {
		validReset = reset.UTC()
	}
	payload := map[string]any{
		"message": message,
		"type":    errType,
		"code":    status,
	}
	if seconds > 0 {
		payload["retry_after"] = seconds
	}
	if !validReset.IsZero() {
		payload["reset_at"] = validReset.Unix()
	}
	if hint = strings.TrimSpace(hint); hint != "" {
		payload["gateway_hint"] = hint
	}
	body, err := marshalJSON(map[string]any{"error": payload})
	if err != nil {
		body = []byte(`{"error":{"message":"response encoding failed","type":"server_error","code":500}}`)
		status = http.StatusInternalServerError
	}
	if seconds > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	if !validReset.IsZero() {
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(validReset.Unix(), 10))
		w.Header().Set("X-RateLimit-Reset-At", validReset.Format(time.RFC3339))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeJSONResponse writes a compact JSON response without HTML escaping.
func writeJSONResponse(w http.ResponseWriter, status int, value any) {
	body, err := marshalJSON(value)
	if err != nil {
		writeV1Error(w, http.StatusInternalServerError, "server_error", "response encoding failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func marshalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// queryBool accepts the v1 force flag spellings.
func queryBool(r *http.Request, name string) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
