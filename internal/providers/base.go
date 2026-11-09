// Package providers defines the compile-time provider contract and the
// realm-keyed registry used by the core to route requests. Concrete providers
// live in subpackages and are registered once in cmd.
package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/core/scheduler"
)

// Health is a provider health payload. Field names and shapes must stay
// compatible with the v1 wire format.
type Health = map[string]any

// Model is one entry in a provider's model catalog. Field names and shapes
// must stay compatible with the v1 wire format.
type Model = map[string]any

// ChatRequest carries one chat completion request to a provider. Raw and
// Headers preserve the original request so providers can apply their own
// conversation, compression, identity, or reasoning behavior.
type ChatRequest struct {
	Raw        map[string]json.RawMessage
	Headers    http.Header
	Model      string
	Messages   []json.RawMessage
	SessionKey string

	// AllowRealmFallback suppresses provider-internal cross-realm fallback
	// when false. A nil value leaves the provider's configuration in control.
	// The API layer sets false when a realm was explicitly requested or bound
	// to an API key, because that is the operator's written intent.
	AllowRealmFallback *bool
}

// ChatStream is a provider-owned stream of response bytes.
type ChatStream interface {
	// Next returns the next chunk of the stream. It returns io.EOF when the
	// stream is complete.
	Next() ([]byte, error)

	// Close releases the stream's resources. It is safe to call after Next
	// has returned io.EOF.
	Close() error
}

// HTTPError is an optional provider error contract. Providers use it to
// declare the HTTP status, OpenAI-style error type, and the safe client-facing
// message the generic API should return. The API layer checks for this
// interface with errors.As, so it never imports a concrete provider package.
//
// Error() remains the internal/logging representation and may include a
// provider prefix or status label. SafeMessage must not: the API writes it to
// /v1 clients verbatim.
type HTTPError interface {
	error

	// HTTPStatus returns the HTTP status to send to the client.
	HTTPStatus() int

	// ErrorType returns the OpenAI error type, such as "rate_limit_error" or
	// "invalid_request_error".
	ErrorType() string

	// SafeMessage returns the redacted message for the wire response without
	// provider prefixes such as "raccoon: HTTP 400:".
	SafeMessage() string
}

// RetryMetadata is an optional extension for rate-limit errors. Providers that
// implement it let the generic API send v1-compatible Retry-After and absolute
// reset headers without importing a concrete provider package.
type RetryMetadata interface {
	error

	// RetryDelay is the recommended wait before retrying. Values <= 0 mean
	// no usable relative delay.
	RetryDelay() time.Duration

	// RetryReset is the absolute recovery time. The zero time means unknown.
	RetryReset() time.Time
}

// AuthClass tells the API layer what authentication a route requires.
type AuthClass int

const (
	// AuthNone requires no authentication. It is reserved for routes such as
	// login and OAuth callbacks that validate their own input.
	AuthNone AuthClass = iota

	// AuthAPIKey requires a valid API key.
	AuthAPIKey

	// AuthPanel requires a valid dashboard (panel) session.
	AuthPanel
)

// Route is a provider-specific HTTP route, such as an admin, OAuth, check-in,
// or media endpoint. The API layer mounts it under the provider's realm.
type Route struct {
	Method  string
	Path    string
	Auth    AuthClass
	Handler http.HandlerFunc
}

// Provider is one upstream channel instance. One instance maps to exactly one
// realm; channels that serve multiple regions (WorkBuddy intl and cn) register
// separate instances of the same implementation.
//
// Provider configuration and account pools are internal to the implementation
// and are not part of this contract.
type Provider interface {
	// ID returns the stable provider implementation identifier, such as
	// "raccoon" or "workbuddy". Several instances may share an ID.
	ID() string

	// Realm returns the unique realm this instance serves, such as "intl" or
	// "cn". The registry keys providers by realm.
	Realm() string

	Overview(ctx context.Context, force bool) (any, error)
	Health(ctx context.Context, force bool) (Health, error)
	Models(ctx context.Context, force bool) ([]Model, error)

	// OpenChat starts a chat completion stream. The caller owns the returned
	// stream and must close it.
	OpenChat(ctx context.Context, req ChatRequest) (ChatStream, error)

	// Routes returns provider-specific HTTP routes. The core mounts them.
	Routes() []Route

	// Jobs returns background jobs the core should schedule. Implementations
	// may return nil.
	Jobs() []scheduler.Job
}
