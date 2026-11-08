package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

// apiFailure is the API layer's provider-neutral error envelope.
type apiFailure struct {
	Status  int
	Type    string
	Message string
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
		return &apiFailure{Status: status, Type: errType, Message: message}
	}

	return localFailure(http.StatusBadGateway, "upstream request failed")
}

// writeFailure writes err as a v1 error envelope. *apiFailure values are used
// directly; any other error is mapped through providerFailure.
func writeFailure(w http.ResponseWriter, err error) {
	var failure *apiFailure
	if errors.As(err, &failure) && failure != nil {
		writeV1Error(w, failure.Status, failure.Type, failure.Message)
		return
	}
	mapped := providerFailure(err)
	writeV1Error(w, mapped.Status, mapped.Type, mapped.Message)
}

// writeV1Error is the single v1 error envelope writer used by the generic API
// and the provider-route adapter.
func writeV1Error(w http.ResponseWriter, status int, errType, message string) {
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	if strings.TrimSpace(errType) == "" {
		errType = "invalid_request_error"
	}
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(status)
	}
	body, err := marshalJSON(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    status,
		},
	})
	if err != nil {
		body = []byte(`{"error":{"message":"response encoding failed","type":"server_error","code":500}}`)
		status = http.StatusInternalServerError
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
