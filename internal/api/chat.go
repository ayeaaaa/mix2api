package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ayeaaaa/mix2api-v2/internal/api/protocol"
	"github.com/ayeaaaa/mix2api-v2/internal/providers"
)

const maxChatBodyBytes = 16 << 20

// chatHTTPRequest is the parsed gateway view of one /v1/chat/completions call.
type chatHTTPRequest struct {
	payload        map[string]json.RawMessage
	model          string // upstream model ID sent to the provider
	rawModel       string // client spelling before exit canonicalization
	requestedModel string // canonical when qualified, otherwise the bare ID
	exit           string // lower-case registered realm, or ""
	messages       []json.RawMessage
	stream         bool
	realm          string
	explicitRealm  bool
	sessionKey     string
	headers        http.Header
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeFailure(w, localFailure(http.StatusMethodNotAllowed, "method not allowed"))
		return
	}
	s.HandleChat(w, r, nil, "")
}

// HandleChat serves one chat completion. The normal router passes a nil
// provider and empty model so the request is resolved by realm or model. Tests
// and future embedders may pass an explicit provider/model pair; the model is
// still validated against that provider's Models catalog.
func (s *Server) HandleChat(w http.ResponseWriter, r *http.Request, provider providers.Provider, model string) {
	started := time.Now()
	request, failure := parseChatHTTPRequest(w, r)
	if failure != nil {
		s.observeChatFailure(r, "", "", false, failure)
		writeFailure(w, failure)
		return
	}

	override := strings.TrimSpace(model)
	if override != "" {
		request.model = override
		encoded, err := json.Marshal(override)
		if err != nil {
			failure := localFailure(http.StatusBadRequest, "model is required")
			s.observeChatFailure(r, "", "", false, failure)
			writeFailure(w, failure)
			return
		}
		request.payload["model"] = encoded
	}
	if strings.TrimSpace(request.model) == "" {
		failure := localFailure(http.StatusBadRequest, "model is required")
		s.observeChatFailure(r, "", "", request.stream, failure)
		writeFailure(w, failure)
		return
	}
	if failure := s.applyModelRef(request); failure != nil {
		s.observeChatFailure(r, request.model, "", request.stream, failure)
		writeFailure(w, failure)
		return
	}
	if provider != nil && request.exit != "" &&
		!strings.EqualFold(strings.TrimSpace(provider.Realm()), request.exit) {
		failure := localFailure(http.StatusBadRequest, "model exit does not match provider realm")
		s.observeChatFailure(r, request.requestedModel, provider.Realm(), request.stream, failure)
		writeFailure(w, failure)
		return
	}
	ref := modelRef{
		Raw:       request.rawModel,
		Exit:      request.exit,
		Upstream:  request.model,
		Canonical: request.requestedModel,
		Qualified: request.exit != "",
	}
	if failure := s.keyModelFailureForRef(r, ref); failure != nil {
		s.observeChatFailure(r, request.requestedModel, "", request.stream, failure)
		writeFailure(w, failure)
		return
	}

	boundRealm, boundFailure := s.apiKeyBoundRealm(r)
	if boundFailure != nil {
		s.observeChatFailure(r, request.requestedModel, "", request.stream, boundFailure)
		writeFailure(w, boundFailure)
		return
	}
	if request.exit != "" && boundRealm != "" && boundRealm != "gateway" && boundRealm != request.exit {
		failure := localFailure(http.StatusForbidden, "api key realm does not allow exit "+request.exit)
		s.observeChatFailure(r, request.requestedModel, request.exit, request.stream, failure)
		writeFailure(w, failure)
		return
	}

	if provider == nil {
		resolved, err := s.resolveChatProvider(r.Context(), request, boundRealm)
		if err != nil {
			realm := boundRealm
			if request.exit != "" {
				realm = request.exit
			}
			s.observeChatFailure(r, request.requestedModel, realm, request.stream, failureFromError(err))
			writeFailure(w, err)
			return
		}
		provider = resolved
	}

	var allowRealmFallback *bool
	suppressRealmFallback := request.exit != "" || request.explicitRealm || boundRealm != ""
	if suppressRealmFallback {
		disabled := false
		allowRealmFallback = &disabled
	}
	chatRequest := providers.ChatRequest{
		Raw:                request.payload,
		Headers:            request.headers,
		Model:              request.model,
		Messages:           request.messages,
		SessionKey:         request.sessionKey,
		AllowRealmFallback: allowRealmFallback,
		AllowedAccounts:    keyAllowedAccounts(r),
	}
	if s.opts.WebTools != nil && s.opts.WebTools.Active() {
		if request.stream {
			if handled := s.handleChatWithWebToolsStream(w, r, provider, chatRequest, request, started); handled {
				return
			}
		} else if handled := s.handleChatWithWebTools(w, r, provider, chatRequest, request, started); handled {
			return
		}
	}
	stream, err := provider.OpenChat(r.Context(), chatRequest)
	if err != nil {
		if stream != nil {
			_ = stream.Close()
		}
		failure := failureFromError(err)
		s.observeChatFailure(r, request.requestedModel, provider.Realm(), request.stream, failure)
		writeFailure(w, err)
		return
	}
	if stream == nil {
		failure := localFailure(http.StatusBadGateway, "upstream returned no chat stream")
		s.observeChatFailure(r, request.requestedModel, provider.Realm(), request.stream, failure)
		writeFailure(w, failure)
		return
	}
	tracked := newUsageTracker(withAttribution(stream, provider.Realm()), started)
	defer func() { _ = tracked.Close() }()

	result := chatStreamResult{Outcome: "completed", Status: http.StatusOK}
	printed := chatPrintedToolRecovery(request.payload)
	forceModel := request.exit != ""
	if request.stream {
		result = writeChatStreamFor(w, r, tracked, request.requestedModel, forceModel, printed)
	} else if forceModel {
		result = writeChatCompletionFor(w, tracked, request.requestedModel, true, printed)
	} else {
		result = writeChatCompletion(w, tracked, request.requestedModel, printed)
	}
	observation := ProtocolObservation{
		Protocol: "chat", Model: request.model, RequestedModel: request.requestedModel,
		Realm: provider.Realm(), Kind: "chat", Effort: chatReasoningEffort(request.payload),
		Stream: request.stream, Outcome: result.Outcome, Status: result.Status,
		Usage: result.Usage, Error: result.Error, Message: result.Message,
	}
	applyTracker(&observation, tracked)
	s.observeProtocol(r, observation)
}

// chatStreamResult is the terminal outcome of one Chat request. Usage is the
// aggregate block when the non-stream path produced one.
type chatStreamResult struct {
	Outcome string
	Status  int
	Usage   map[string]any
	Error   bool
	Message string
}

// observeChatFailure records one failed Chat row before any upstream stream
// exists. It keeps v1's error/status/message fields so key/model/realm and
// parse failures are attributable and 429 rows remain usable by the remaining
// estimate.
func (s *Server) observeChatFailure(r *http.Request, model, realm string, stream bool, failure *apiFailure) {
	status := http.StatusBadRequest
	message := "request failed"
	if failure != nil {
		if failure.Status > 0 {
			status = failure.Status
		}
		if strings.TrimSpace(failure.Message) != "" {
			message = failure.Message
		}
	}
	s.observeProtocol(r, ProtocolObservation{
		Protocol: "chat", Model: model, RequestedModel: model, Realm: realm, Kind: "chat",
		Stream: stream, Outcome: "failed", Status: status, Error: true, Message: message,
	})
}

func failureFromError(err error) *apiFailure {
	var failure *apiFailure
	if errors.As(err, &failure) && failure != nil {
		return failure
	}
	return providerFailure(err)
}

func chatReasoningEffort(payload map[string]json.RawMessage) string {
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		if value, ok := decodeJSONString(payload[key]); ok && value != "" {
			return value
		}
	}
	return ""
}

func parseChatHTTPRequest(w http.ResponseWriter, r *http.Request) (*chatHTTPRequest, *apiFailure) {
	if r == nil || r.Body == nil {
		return nil, localFailure(http.StatusBadRequest, "invalid JSON body")
	}
	limitReader := http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	decoder := json.NewDecoder(limitReader)
	decoder.UseNumber()

	var payload map[string]json.RawMessage
	if err := decoder.Decode(&payload); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, localFailure(http.StatusRequestEntityTooLarge, "request body too large")
		}
		return nil, localFailure(http.StatusBadRequest, "invalid JSON body")
	}
	if payload == nil {
		return nil, localFailure(http.StatusBadRequest, "expected a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, localFailure(http.StatusBadRequest, "invalid JSON body")
	}

	// The body model is optional here so HandleChat's explicit model parameter
	// can supply it; the final "model is required" check happens after that
	// override is applied.
	model, _ := decodeJSONString(payload["model"])

	rawMessages, hasMessages := payload["messages"]
	var messages []json.RawMessage
	if !hasMessages || json.Unmarshal(rawMessages, &messages) != nil || len(messages) == 0 {
		return nil, localFailure(http.StatusBadRequest, "messages is empty")
	}

	stream := false
	if raw, ok := payload["stream"]; ok {
		var value bool
		if json.Unmarshal(raw, &value) == nil {
			stream = value
		}
	}

	// Realm precedence mirrors v1: X-Realm first, then ?realm, then the body
	// extension. realm is a gateway extension; never forward it upstream.
	realm, explicitRealm := realmFromRequest(r)
	if realm == "" {
		if bodyRealm, ok := decodeJSONString(payload["realm"]); ok && bodyRealm != "" {
			realm = bodyRealm
			explicitRealm = true
		}
	}
	delete(payload, "realm")

	return &chatHTTPRequest{
		payload:       payload,
		model:         model,
		messages:      messages,
		stream:        stream,
		realm:         realm,
		explicitRealm: explicitRealm,
		sessionKey:    chatSessionKey(r, payload),
		headers:       r.Header.Clone(),
	}, nil
}

// realmFromRequest returns the explicit realm and whether it was supplied by
// X-Realm, the query string, or the body extension. The boolean is the
// operator-intent signal that suppresses provider-internal realm fallback.
func realmFromRequest(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	realm := strings.TrimSpace(r.Header.Get("X-Realm"))
	if realm == "" {
		realm = strings.TrimSpace(r.URL.Query().Get("realm"))
	}
	return realm, realm != ""
}

func decodeJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return strings.TrimSpace(value), true
}

func chatSessionKey(r *http.Request, payload map[string]json.RawMessage) string {
	if r != nil {
		for _, name := range []string{
			"X-Conversation-Id", "Conversation-Id", "X-Session-Id", "Session-Id",
		} {
			if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
				return providers.NormalizeSessionKey(value)
			}
		}
	}
	for _, key := range []string{"conversation_id", "session_id"} {
		if value, ok := decodeJSONString(payload[key]); ok && value != "" {
			return providers.NormalizeSessionKey(value)
		}
	}
	if raw, ok := payload["metadata"]; ok {
		var metadata map[string]json.RawMessage
		if json.Unmarshal(raw, &metadata) == nil {
			if value, ok := decodeJSONString(metadata["conversation_id"]); ok && value != "" {
				return providers.NormalizeSessionKey(value)
			}
		}
	}
	return ""
}

// applyModelRef parses request.model into an exit and an upstream ID. Only a
// registered provider realm followed by "/" is qualified; every other prefix is
// part of a bare model ID. The payload model is rewritten to the upstream ID so
// the exit prefix is never forwarded upstream.
func (s *Server) applyModelRef(request *chatHTTPRequest) *apiFailure {
	if request == nil {
		return localFailure(http.StatusBadRequest, "model is required")
	}
	request.rawModel = strings.TrimSpace(request.model)
	ref, err := parseModelRef(request.model, func(exit string) bool {
		return s != nil && s.providerForRealm(exit) != nil
	})
	if err != nil {
		return localFailure(http.StatusBadRequest, err.Error())
	}
	request.requestedModel = ref.Requested()
	request.exit = ref.Exit
	request.model = ref.Upstream

	encoded, marshalErr := json.Marshal(ref.Upstream)
	if marshalErr != nil {
		return localFailure(http.StatusBadRequest, "invalid model")
	}
	if request.payload == nil {
		request.payload = make(map[string]json.RawMessage)
	}
	request.payload["model"] = encoded
	return nil
}

// resolveChatProvider applies the documented resolution order: the realm bound
// to the API key is policy and wins over every request-level realm hint; absent
// a binding, a qualified model exit routes exactly to that provider, an
// explicit realm (X-Realm, then query, then body), a unique model match across
// registered providers, then the only registered provider. A bound key
// therefore cannot be escaped with X-Realm, ?realm, a body realm, realm=all, or
// a conflicting exit prefix. realm=gateway is a routing scope, not a provider
// realm: a bare model must match exactly one provider catalog and every catalog
// must be readable (see resolveGatewayAmong).
//
// Explicit-realm and single-provider requests are deliberately not rejected by
// a catalog pre-check: a stale/empty cache must not hide a model the upstream
// supports. The provider classifies unknown models. Multi-provider resolution
// still needs a catalog match, but each lookup is bounded so discovery cannot
// stall chat.
func (s *Server) resolveChatProvider(ctx context.Context, request *chatHTTPRequest, boundRealm string) (providers.Provider, error) {
	boundRealm = strings.TrimSpace(boundRealm)
	if exit := strings.TrimSpace(request.exit); exit != "" {
		// A gateway-bound key may use every registered exit. A concrete key
		// binding remains a hard policy.
		if boundRealm != "" && boundRealm != "gateway" && boundRealm != exit {
			return nil, localFailure(http.StatusForbidden, "api key realm does not allow exit "+exit)
		}
		if realm := strings.TrimSpace(request.realm); realm != "" && realm != "all" && realm != "gateway" && realm != exit {
			return nil, localFailure(http.StatusBadRequest, "realm and model exit conflict")
		}
		provider := s.providerForRealm(exit)
		if provider == nil {
			return nil, localFailure(http.StatusNotFound, "unknown exit: "+exit)
		}
		return provider, nil
	}

	if boundRealm != "" && boundRealm != "gateway" {
		provider := s.providerForRealm(boundRealm)
		if provider == nil {
			return nil, localFailure(http.StatusBadRequest, "invalid api key realm: "+boundRealm)
		}
		return provider, nil
	}

	if realm := strings.TrimSpace(request.realm); realm != "" {
		if realm == "gateway" {
			return s.resolveGatewayAmong(ctx, request.model)
		}
		if realm == "all" {
			candidates := s.workBuddyProviders()
			if len(candidates) == 0 {
				return nil, localFailure(http.StatusBadRequest,
					"realm=all is only available for WorkBuddy providers (intl/cn); none is registered")
			}
			return s.resolveAmong(ctx, candidates, request.model, "all")
		}
		provider := s.providerForRealm(realm)
		if provider == nil {
			return nil, localFailure(http.StatusNotFound, "unknown realm: "+realm)
		}
		return provider, nil
	}

	if boundRealm == "gateway" {
		// The key is explicitly allowed to use every provider. Bare models
		// therefore use the same fail-closed unique-match rule as realm=gateway.
		return s.resolveGatewayAmong(ctx, request.model)
	}

	if len(s.providers) == 0 {
		return nil, localFailure(http.StatusBadRequest, "no provider is registered")
	}
	if len(s.providers) == 1 {
		return s.providers[0], nil
	}
	// When several providers expose the same model, registration order is not
	// an operator intent. Try the persisted active realm first, then fall back
	// to the remaining providers if that realm's catalog does not carry the
	// model. Without a realm service (tests/embedded callers) keep the legacy
	// provider order.
	candidates := s.providers
	if s.realm != nil {
		if preferred := strings.TrimSpace(s.realm.Active(ctx)); preferred != "" {
			if provider := s.providerForRealm(preferred); provider != nil {
				ordered := make([]providers.Provider, 0, len(s.providers))
				ordered = append(ordered, provider)
				for _, candidate := range s.providers {
					if candidate != provider {
						ordered = append(ordered, candidate)
					}
				}
				candidates = ordered
			}
		}
	}
	return s.resolveAmong(ctx, candidates, request.model, "")
}

// catalogResolutionTimeout bounds every catalog read used for model-to-realm
// resolution, including the /v1/models?realm=gateway fan-out. resolveAmong
// tolerates an unreadable or timed-out catalog when another candidate can
// still answer; it only returns the first error when every candidate failed.
// resolveGatewayAmong instead fails closed on any timeout, because a partial
// snapshot could hide a second match.
const catalogResolutionTimeout = 2 * time.Second

func (s *Server) resolveAmong(ctx context.Context, candidates []providers.Provider, model, realm string) (providers.Provider, error) {
	var firstErr error
	successes := 0
	for _, provider := range candidates {
		lookupCtx, cancel := context.WithTimeout(ctx, catalogResolutionTimeout)
		models, err := provider.Models(lookupCtx, false)
		cancel()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		successes++
		if modelMatches(models, model) {
			return provider, nil
		}
	}
	if successes == 0 && firstErr != nil {
		return nil, firstErr
	}
	if realm == "all" {
		return nil, localFailure(http.StatusNotFound,
			"model not found in WorkBuddy realms intl/cn: "+model)
	}
	return nil, localFailure(http.StatusNotFound, "model not found: "+model)
}

// resolveGatewayAmong resolves one bare model across every registered exit.
// realm=gateway is a routing scope, not a provider realm, so the lookup must
// read the whole catalog set. Unlike resolveAmong, which tolerates an unreadable
// catalog and returns on the first match, this fails closed: a partial snapshot
// could hide a second match and silently pin the request to one exit. A client
// that needs a guaranteed route can use exit/model or a concrete realm.
func (s *Server) resolveGatewayAmong(ctx context.Context, model string) (providers.Provider, error) {
	var matches []providers.Provider
	for _, provider := range s.providers {
		lookupCtx, cancel := context.WithTimeout(ctx, catalogResolutionTimeout)
		models, err := provider.Models(lookupCtx, false)
		cancel()
		if err != nil {
			return nil, &apiFailure{
				Status:  http.StatusServiceUnavailable,
				Type:    "server_error",
				Message: "gateway model catalog incomplete; use exit/model or a concrete realm",
			}
		}
		if modelMatches(models, model) {
			matches = append(matches, provider)
		}
	}

	switch len(matches) {
	case 0:
		return nil, localFailure(http.StatusNotFound, "model not found in gateway exits: "+model)
	case 1:
		return matches[0], nil
	}

	// Ambiguity is reported with canonical IDs so the client can retry with the
	// exact exit prefix. Duplicate realm registrations collapse to one ID.
	candidates := make([]string, 0, len(matches))
	seen := make(map[string]bool, len(matches))
	for _, provider := range matches {
		candidate := canonicalModelID(providerRealm(provider), model)
		key := strings.ToLower(candidate)
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, candidate)
	}
	return nil, localFailure(http.StatusBadRequest,
		"model is available in multiple gateway exits; use exit/model: "+strings.Join(candidates, ", "))
}

func modelMatches(models []providers.Model, model string) bool {
	want := strings.ToLower(strings.TrimSpace(model))
	if want == "" {
		return false
	}
	for _, item := range models {
		id, _ := item["id"].(string)
		if strings.ToLower(strings.TrimSpace(id)) == want {
			return true
		}
	}
	return false
}

func (s *Server) providerForRealm(realm string) providers.Provider {
	realm = strings.TrimSpace(realm)
	for _, provider := range s.providers {
		if providerRealm(provider) == realm {
			return provider
		}
	}
	return nil
}

func (s *Server) workBuddyProviders() []providers.Provider {
	candidates := make([]providers.Provider, 0, 2)
	for _, realm := range []string{"intl", "cn"} {
		provider := s.providerForRealm(realm)
		if provider != nil && isWorkBuddyProvider(provider) {
			candidates = append(candidates, provider)
		}
	}
	return candidates
}

// isWorkBuddyProvider recognizes the WorkBuddy implementation identity. Realm
// names alone are not enough: another provider could claim intl/cn, while
// realm=all is a WorkBuddy-only discovery/chat contract.
func isWorkBuddyProvider(provider providers.Provider) bool {
	if provider == nil {
		return false
	}
	id := strings.ToLower(strings.TrimSpace(provider.ID()))
	return id == "workbuddy" ||
		strings.HasPrefix(id, "workbuddy-") ||
		strings.HasPrefix(id, "workbuddy_")
}

// writeChatStream normalizes provider frames into one OpenAI-style stream. SSE
// headers are written only after the first normalized frame, a heartbeat keeps
// silent upstreams alive, and the normalizer hoists usage/finish fields. After
// [DONE] the writer keeps reading long enough to forward a provider's post-DONE
// credit accounting frame; clients that stop at [DONE] never wait for it.
func writeChatStream(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, model string, printed ...*printedToolRecovery) chatStreamResult {
	return writeChatStreamFor(w, r, stream, model, false, printed...)
}

// writeChatStreamFor normalizes provider frames into one OpenAI-style stream.
// forceModel pins every client-visible chunk model to model (the canonical
// requested ID); bare requests keep the upstream echo.
func writeChatStreamFor(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, model string, forceModel bool, printed ...*printedToolRecovery) chatStreamResult {
	result := chatStreamResult{Outcome: "completed", Status: http.StatusOK}
	writer := newSSEWriter(w)
	defer writer.Close()
	normalizer := newStreamNormalizerForModel(model, forceModel, printed...)

	for {
		frame, err := stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				final := normalizer.finalize()
				for _, out := range final {
					if writeErr := writer.Write(out); writeErr != nil {
						result.Outcome = "client_aborted"
						return result
					}
				}
				if !writer.WroteHeaders() && len(final) == 0 && !normalizer.sawAny {
					result.Outcome = "failed"
					result.Status = http.StatusBadGateway
					result.Error = true
					result.Message = "upstream returned an empty stream"
					writeFailure(w, localFailure(http.StatusBadGateway, result.Message))
					return result
				}
				if err := writer.Write([]byte("data: [DONE]\n\n")); err != nil {
					result.Outcome = "client_aborted"
				}
				return result
			}
			if r != nil && r.Context().Err() != nil {
				result.Outcome = "client_aborted"
				return result
			}
			if errors.Is(err, context.Canceled) {
				result.Outcome = "client_aborted"
				return result
			}
			// A read failure is not a completed tool call. Release buffered
			// candidate text verbatim before the error, without finish/[DONE].
			if text, _ := normalizer.printed.flush(true, ""); text != "" {
				if writeErr := writer.Write(normalizer.chunk(map[string]any{"content": text}, nil)); writeErr != nil {
					result.Outcome = "client_aborted"
					return result
				}
			}
			failure := providerFailure(err)
			result.Outcome = "upstream_aborted"
			result.Status = failure.Status
			result.Error = true
			result.Message = failure.Message
			if result.Message == "" {
				result.Message = "upstream stream error"
			}
			if !writer.WroteHeaders() {
				result.Outcome = "failed"
				writeFailure(w, err)
				return result
			}
			writeSSEError(writer, err)
			return result
		}
		if len(frame) == 0 {
			continue
		}
		// A provider may signal failure inside an SSE data frame instead of
		// returning a Next error. Do not turn that failed turn into recovered
		// calls or a successful [DONE]. Forward the actual error once.
		parsed := protocol.ParseFrame(frame)
		if parsed.Valid && parsed.Fields["error"] != nil {
			if text, _ := normalizer.printed.flush(true, ""); text != "" {
				if writeErr := writer.Write(normalizer.chunk(map[string]any{"content": text}, nil)); writeErr != nil {
					result.Outcome = "client_aborted"
					return result
				}
			}
			if writeErr := writer.Write(frame); writeErr != nil {
				result.Outcome = "client_aborted"
				return result
			}
			result.Outcome = "upstream_aborted"
			result.Status = http.StatusBadGateway
			result.Error = true
			result.Message = "upstream stream error"
			return result
		}
		if sseFrameDone(frame) {
			for _, out := range normalizer.finalize() {
				if writeErr := writer.Write(out); writeErr != nil {
					result.Outcome = "client_aborted"
					return result
				}
			}
			if writeErr := writer.Write([]byte("data: [DONE]\n\n")); writeErr != nil {
				result.Outcome = "client_aborted"
				return result
			}
			forwardPostDone(writer, r, stream, normalizer)
			return result
		}
		for _, out := range normalizer.absorb(frame) {
			if len(out) == 0 {
				continue
			}
			if writeErr := writer.Write(out); writeErr != nil {
				result.Outcome = "client_aborted"
				return result
			}
		}
	}
}

type postDoneResult struct {
	frame []byte
	err   error
}

// forwardPostDone reads after [DONE] with a bounded idle timeout. The upstream
// account slot stays held until the stream closes, matching v1's stream scope.
func forwardPostDone(writer *sseWriter, r *http.Request, stream providers.ChatStream, normalizer *streamNormalizer) {
	// Do not use r.Context() here. Raccoon emits its credit usage frame after
	// [DONE]; clients commonly close the stream at [DONE], which cancels the
	// request context. Keep draining with a detached bounded timer so the
	// gateway can still record the post-DONE credit.
	timer := time.NewTimer(postDoneReadIdleTimeout)
	defer timer.Stop()
	for {
		results := make(chan postDoneResult, 1)
		go func() {
			frame, err := stream.Next()
			results <- postDoneResult{frame: frame, err: err}
		}()

		select {
		case <-timer.C:
			return
		case result := <-results:
			if result.err != nil {
				return
			}
			if len(result.frame) > 0 {
				if out := normalizer.rewritePostDoneFrame(result.frame); len(out) > 0 {
					if err := writer.Write(out); err != nil {
						return
					}
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(postDoneReadIdleTimeout)
		}
	}
}

func writeChatCompletion(w http.ResponseWriter, stream providers.ChatStream, model string, printed ...*printedToolRecovery) chatStreamResult {
	return writeChatCompletionFor(w, stream, model, false, printed...)
}

// writeChatCompletionFor aggregates one non-stream response. forceModel makes
// the client-visible model the canonical requested ID even when the upstream
// reports an alias; bare requests keep the existing upstream echo.
func writeChatCompletionFor(w http.ResponseWriter, stream providers.ChatStream, model string, forceModel bool, printed ...*printedToolRecovery) chatStreamResult {
	completion, err := aggregateChatStream(stream, model, printed...)
	if err != nil {
		status := providerFailure(err).Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		writeFailure(w, err)
		message := providerFailure(err).Message
		if message == "" {
			message = "upstream request failed"
		}
		return chatStreamResult{Outcome: "failed", Status: status, Error: true, Message: message}
	}
	if forceModel {
		completion["model"] = model
	}
	usage, _ := completion["usage"].(map[string]any)
	if tracker, ok := stream.(*usageTracker); ok {
		// v1 adds these only to the non-stream chat envelope; SSE frames keep
		// their existing shape.
		completion["elapsed_ms"] = tracker.ElapsedMS()
		if first := tracker.FirstChunkAt(); !first.IsZero() {
			completion["first_chunk_at"] = float64(first.UnixNano()) / float64(time.Second)
		}
	}
	writeJSONResponse(w, http.StatusOK, completion)
	return chatStreamResult{Outcome: "completed", Status: http.StatusOK, Usage: usage}
}

func aggregateChatStream(stream providers.ChatStream, model string, printed ...*printedToolRecovery) (map[string]any, error) {
	if stream == nil {
		return nil, localFailure(http.StatusBadGateway, "upstream returned no chat stream")
	}
	aggregate := &chatAggregate{tools: make(map[int]*chatToolCall)}
	if len(printed) > 0 {
		aggregate.printed = printed[0]
	}
	reader := protocol.NewReader(stream)
	for {
		chunk, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if chunk.Done {
			// Keep reading: Raccoon emits its credit usage frame after [DONE].
			aggregate.sawDone = true
			continue
		}
		if chunk.Valid && chunk.Fields["error"] != nil {
			return nil, localFailure(http.StatusBadGateway, "upstream stream error")
		}
		if aggregate.printed != nil {
			if chunk.ChoiceCount > 1 {
				aggregate.printed = nil
			}
			for _, choice := range chunk.Choices {
				if choice.Index != 0 {
					aggregate.printed = nil
				}
			}
		}
		aggregate.addChunk(chunk)
	}
	completion, ok := aggregate.completion(model)
	if !ok {
		return nil, localFailure(http.StatusBadGateway, "upstream returned no chat completion")
	}
	return completion, nil
}

func writeSSEError(writer *sseWriter, err error) {
	failure := providerFailure(err)
	message := failure.Message
	if message == "" {
		message = "upstream stream error"
	}
	payload, encodeErr := marshalJSON(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"param":   nil,
			"code":    "upstream_stream_error",
		},
	})
	if encodeErr != nil {
		payload = []byte(`{"error":{"message":"upstream stream error","type":"upstream_error","code":"upstream_stream_error"}}`)
	}
	frame := make([]byte, 0, len(payload)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	_ = writer.Write(frame)
}

func sseFrameDone(frame []byte) bool {
	return strings.TrimSpace(ssePayload(frame)) == "[DONE]"
}

// ssePayload joins the data lines of one complete SSE frame. Non-data fields
// and comments are ignored, matching the v1 data-line pass-through.
func ssePayload(frame []byte) string {
	var lines []string
	for _, rawLine := range bytes.Split(frame, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		if strings.HasPrefix(data, " ") {
			data = data[1:]
		}
		lines = append(lines, data)
	}
	if len(lines) == 0 {
		trimmed := strings.TrimSpace(string(frame))
		if trimmed == "[DONE]" {
			return trimmed
		}
		return ""
	}
	return strings.Join(lines, "\n")
}

// chatAggregate folds OpenAI-style SSE chunks into one chat.completion. It
// intentionally understands only the generic wire shape; no Raccoon fields.
type chatAggregate struct {
	printed     *printedToolRecovery
	id          string
	model       string
	content     []string
	reasoning   []string
	finish      string
	tools       map[int]*chatToolCall
	sawChoice   bool
	sawDone     bool
	usage       map[string]any
	lastCreated int64
}

type chatToolCall struct {
	id        string
	kind      string
	name      string
	arguments string
}

func (a *chatAggregate) addChunk(chunk protocol.Chunk) {
	if a == nil {
		return
	}
	if strings.TrimSpace(chunk.ID) != "" {
		a.id = chunk.ID
	}
	if strings.TrimSpace(chunk.Model) != "" {
		a.model = chunk.Model
	}
	if chunk.Created > 0 && a.lastCreated == 0 {
		a.lastCreated = chunk.Created
	}
	if len(chunk.Usage) > 0 {
		a.usage = chunk.Usage
	}
	if chunk.ChoiceCount == 0 {
		return
	}
	a.sawChoice = true
	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if len(delta.Raw) == 0 {
			delta = choice.Message
		}
		if delta.Content != "" {
			a.content = append(a.content, delta.Content)
		}
		if delta.ReasoningContent != "" {
			a.reasoning = append(a.reasoning, delta.ReasoningContent)
		}
		a.addProtocolToolCalls(delta.ToolCalls)
		if delta.FunctionCall != nil {
			a.addLegacyFunctionCall(map[string]any{
				"name": delta.FunctionCall.Name, "arguments": delta.FunctionCall.Arguments,
			})
		}
		if strings.TrimSpace(choice.FinishReason) != "" {
			a.finish = choice.FinishReason
		}
	}
}

func (a *chatAggregate) addProtocolToolCalls(calls []protocol.ToolCall) {
	for _, call := range calls {
		index := call.Index
		if !call.HasIndex {
			index = len(a.tools)
		}
		entry := a.tools[index]
		if entry == nil {
			entry = &chatToolCall{}
			a.tools[index] = entry
		}
		if call.ID != "" {
			entry.id = call.ID
		}
		if call.Type != "" {
			entry.kind = call.Type
		}
		entry.name += call.Name
		entry.arguments += call.Arguments
	}
}

func (a *chatAggregate) addToolCalls(value any) {
	calls, ok := value.([]any)
	if !ok {
		return
	}
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		index, ok := intValue(call["index"])
		if !ok {
			index = len(a.tools)
		}
		entry := a.tools[index]
		if entry == nil {
			entry = &chatToolCall{}
			a.tools[index] = entry
		}
		if id, ok := call["id"].(string); ok && id != "" {
			entry.id = id
		}
		if kind, ok := call["type"].(string); ok && kind != "" {
			entry.kind = kind
		}
		if function, ok := call["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok && name != "" {
				entry.name += name
			}
			if arguments, ok := function["arguments"].(string); ok && arguments != "" {
				entry.arguments += arguments
			}
		}
	}
}

func (a *chatAggregate) addLegacyFunctionCall(function map[string]any) {
	name, _ := function["name"].(string)
	if strings.TrimSpace(name) == "" {
		return
	}
	entry := a.tools[0]
	if entry == nil {
		entry = &chatToolCall{}
		a.tools[0] = entry
	}
	if entry.name == "" {
		entry.name = name
	}
	if arguments, ok := function["arguments"].(string); ok {
		entry.arguments += arguments
	}
}

func (a *chatAggregate) completion(requestedModel string) (map[string]any, bool) {
	if a == nil {
		return nil, false
	}
	if !a.sawChoice && len(a.tools) == 0 && len(a.content) == 0 && len(a.reasoning) == 0 {
		return nil, false
	}

	message := map[string]any{
		"role":    "assistant",
		"content": strings.Join(a.content, ""),
	}
	if reasoning := strings.Join(a.reasoning, ""); reasoning != "" {
		message["reasoning_content"] = reasoning
	}

	finish := a.finish
	if finish == "" {
		finish = "stop"
	}
	if len(a.tools) > 0 {
		indexes := make([]int, 0, len(a.tools))
		for index := range a.tools {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		calls := make([]any, 0, len(indexes))
		for _, index := range indexes {
			call := a.tools[index]
			if call == nil || strings.TrimSpace(call.name) == "" {
				continue
			}
			// A stream cut off by max_tokens or a dropped connection can leave
			// the last call with half-written JSON. Never hand the client
			// unparsable arguments; empty arguments are a legal no-arg call.
			if !validToolArguments(call.arguments) {
				continue
			}
			id := call.id
			if id == "" {
				id = randomChatID("call_")
			}
			kind := call.kind
			if kind == "" {
				kind = "function"
			}
			calls = append(calls, map[string]any{
				"id":   id,
				"type": kind,
				"function": map[string]any{
					"name":      call.name,
					"arguments": call.arguments,
				},
			})
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
			if finish == "stop" {
				finish = "tool_calls"
			}
		} else if finish == "tool_calls" {
			finish = "stop"
		}
	} else if finish == "tool_calls" {
		finish = "stop"
	}
	if len(a.tools) == 0 && a.printed != nil && (finish == "stop" || finish == "length") {
		if calls, clean := a.printed.parse(strings.Join(a.content, "")); len(calls) > 0 {
			message["content"] = clean
			toolCalls := make([]any, 0, len(calls))
			for _, call := range calls {
				toolCalls = append(toolCalls, map[string]any{"id": call.id, "type": "function", "function": map[string]any{"name": call.name, "arguments": call.arguments}})
			}
			message["tool_calls"] = toolCalls
			finish = "tool_calls"
		}
	}

	created := a.lastCreated
	if created == 0 {
		created = time.Now().Unix()
	}
	id := a.id
	if id == "" {
		id = randomChatID("chatcmpl-")
	}
	model := a.model
	if model == "" {
		model = requestedModel
	}

	completion := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
	}
	if a.usage != nil && usageTotal(a.usage) > 0 {
		completion["usage"] = a.usage
	} else if estimated := estimateChatUsage(strings.Join(a.content, ""), strings.Join(a.reasoning, "")); estimated != nil {
		completion["usage"] = estimated
	} else if a.usage != nil {
		completion["usage"] = a.usage
	}
	return completion, true
}

func intValue(value any) (int, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(number), true
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, false
		}
		return number, true
	default:
		return 0, false
	}
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return number, true
	case float64:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}

func randomChatID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return prefix + hex.EncodeToString(raw[:])
}
