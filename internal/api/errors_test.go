package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type hintTestError struct {
	hint string
}

func (e *hintTestError) Error() string       { return "upstream failed" }
func (e *hintTestError) HTTPStatus() int     { return http.StatusBadRequest }
func (e *hintTestError) ErrorType() string   { return "invalid_request_error" }
func (e *hintTestError) SafeMessage() string { return "insufficient credits; please top up" }
func (e *hintTestError) GatewayHint() string { return e.hint }

func TestWriteFailureAddsGatewayHintOnlyWhenSet(t *testing.T) {
	const want = "account credits exhausted at upstream; waiting for daily check-in to restore"
	rec := httptest.NewRecorder()
	writeFailure(rec, &hintTestError{hint: want})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error["gateway_hint"] != want {
		t.Fatalf("gateway_hint = %#v, want %q", body.Error["gateway_hint"], want)
	}

	rec = httptest.NewRecorder()
	writeFailure(rec, &hintTestError{})
	body = struct {
		Error map[string]any `json:"error"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := body.Error["gateway_hint"]; present {
		t.Fatalf("empty hint was written: %#v", body.Error)
	}
}
