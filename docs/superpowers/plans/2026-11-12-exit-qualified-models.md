# Exit-Qualified Model Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let every registered exit serve models at the same time, with canonical `<exit>/<upstream-model>` IDs and a `realm=gateway` catalog, while keeping bare IDs and `realm=all` compatible.

**Architecture:** Parse the model reference once at the API boundary. Qualified references select an exact provider and are stripped before `ChatRequest`/payload reaches the provider. `realm=gateway` is a discovery/routing scope over all providers; `realm=all` remains the WorkBuddy-only legacy scope. Usage keeps the upstream `model` for pricing and adds the canonical `requested_model` for attribution.

**Tech Stack:** Go 1.26.0, `net/http`, existing provider contract, embedded `internal/web/dashboard.html`, Node assertion suites.

**Spec:** `docs/superpowers/specs/2026-11-12-exit-qualified-models-design.md`

## Global Constraints

- Only a registered provider realm as the first path segment is an exit. Unknown prefixes are bare upstream IDs; never return “unknown exit” for them.
- `realm=all` stays WorkBuddy `intl`/`cn` only for `/v1/models` and Chat. Do not add CodeArts/Raccoon to it.
- `gateway` is a scope, not a provider realm; `providerForRealm("gateway")` must stay nil.
- Prefixes are never forwarded upstream: update both `ChatRequest.Model` and the raw payload/chat map.
- API Key realm binding is policy and wins over request-level hints. A qualified prefix that conflicts with the binding is rejected.
- Qualified requests echo the canonical ID in client-visible structured `model` fields; bare requests keep current echo behavior.
- `gateway` bare-model resolution requires a complete catalog; any provider catalog failure is 503.
- Phase 1 is backend only. Do not modify the embedded dashboard, analytics grouping, or agent picker until Phase 2.
- Run `go test ./...` before each commit unless a task gives a narrower command.

## Review Focus

- `openai/gpt-4o` and other bare IDs containing `/` must pass through unchanged.
- A qualified request must not leak its prefix into `ChatRequest.Model`, `Raw["model"]`, or provider payloads.
- `realm=all` must continue to exclude CodeArts/Raccoon.
- A bound Key must not use `realm=gateway` or an exit prefix to see another realm.
- A gateway bare-model lookup must not route to the wrong exit when one provider catalog is unreadable.

---

## Task 1: Model Reference Parser

**Files:**
- Create: `internal/api/modelref.go`
- Test: `internal/api/modelref_test.go`

**Interfaces:**
- Produces:
  ```go
  type modelRef struct {
      Raw       string
      Exit      string
      Upstream  string
      Canonical string
      Qualified bool
  }
  func parseModelRef(raw string, isExit func(string) bool) (modelRef, error)
  func canonicalModelID(exit, upstream string) string
  func (r modelRef) Requested() string
  ```
- `Requested()` returns `Canonical` when `Qualified`, otherwise `Raw`.

- [ ] **Step 1: Write the failing table test**

Create `internal/api/modelref_test.go` with a table covering:

```go
func TestParseModelRef(t *testing.T) {
    isExit := func(exit string) bool {
        return exit == "intl" || exit == "cn" || exit == "codearts"
    }
    cases := []struct {
        raw       string
        exit      string
        upstream  string
        canonical string
        qualified bool
        wantErr   bool
    }{
        {"cn/m1", "cn", "m1", "cn/m1", true, false},
        {"CN/m1", "cn", "m1", "cn/m1", true, false},
        {" cn/foo/bar ", "cn", "foo/bar", "cn/foo/bar", true, false},
        {"m1", "", "m1", "m1", false, false},
        {"cn", "", "cn", "cn", false, false},
        {"", "", "", "", false, false},
        {"cn/", "cn", "", "cn/", true, true},
        {"openai/gpt-4o", "", "openai/gpt-4o", "openai/gpt-4o", false, false},
        {"gateway/foo", "", "gateway/foo", "gateway/foo", false, false},
    }
    // assert each field; error cases assert err != nil
}
```

Also assert `modelRef.Requested()` returns `cn/m1` for a qualified ref and `m1` for a bare ref.

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/api -run TestParseModelRef -v`
Expected: FAIL because `parseModelRef` is undefined.

- [ ] **Step 3: Implement the parser**

Use the first `/` only. Trim the input. Lowercase the candidate exit only for the `isExit` check and canonical output. Preserve the upstream segment verbatim. Return an error only when the candidate exit is registered and the upstream is empty.

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/api -run TestParseModelRef -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/modelref.go internal/api/modelref_test.go
git commit -m "feat(api): parse exit-qualified model ids"
```

---

## Task 2: Key Allow-List Accepts Canonical and Upstream IDs

**Files:**
- Modify: `internal/platform/keys/binding.go`
- Test: `internal/platform/keys/binding_test.go`

**Interfaces:**
- Produces:
  ```go
  func AllowsModelAny(entry Entry, models ...string) bool
  ```
- Empty patterns return true. Otherwise return true when any non-empty model matches an existing pattern; return false when none match.

- [ ] **Step 1: Write the failing test**

Add to `binding_test.go`:

```go
func TestAllowsModelAny(t *testing.T) {
    bare := Entry{Models: NormalizeModelPatterns("deepseek/*")}
    if !AllowsModelAny(bare, "cn/deepseek-v4.1-flash", "deepseek-v4.1-flash") {
        t.Fatal("upstream fallback should allow canonical request")
    }
    qualified := Entry{Models: NormalizeModelPatterns("cn/*")}
    if !AllowsModelAny(qualified, "cn/deepseek-v4.1-flash", "deepseek-v4.1-flash") {
        t.Fatal("canonical pattern rejected")
    }
    if AllowsModelAny(qualified, "codearts/deepseek-v4.1-flash", "deepseek-v4.1-flash") {
        t.Fatal("wrong exit accepted")
    }
    if !AllowsModelAny(Entry{}, "cn/anything") {
        t.Fatal("empty allow-list should remain unrestricted")
    }
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/platform/keys -run TestAllowsModelAny -v`
Expected: FAIL because `AllowsModelAny` is undefined.

- [ ] **Step 3: Implement the helper**

Normalize the patterns once. If empty, return true. Loop over the provided names, skipping blanks, and return true on the first `AllowsModel` match. Return false after the loop.

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/platform/keys -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/keys/binding.go internal/platform/keys/binding_test.go
git commit -m "feat(keys): match canonical and upstream model ids"
```

---

## Task 3: Gateway Model Catalog

**Files:**
- Modify: `internal/api/models.go`
- Test: `internal/api/models_test.go`

**Interfaces:**
- Produces:
  ```go
  type modelsResponse struct {
      Object   string            `json:"object"`
      Data     []providers.Model `json:"data"`
      Realm    string            `json:"realm"`
      Catalog  map[string]any    `json:"catalog,omitempty"`
      Catalogs map[string]any    `json:"catalogs,omitempty"`
  }
  func (s *Server) gatewayModels(ctx context.Context, candidates []providers.Provider, force bool) ([]providers.Model, map[string]any, error)
  func decorateModel(model providers.Model, provider providers.Provider, canonicalID bool) providers.Model
  ```
- `decorateModel` clones the map, sets `upstream_id`, `qualified_id`, `provider`, and `realm`; when `canonicalID` is true it also sets `id` to the qualified ID.
- `gatewayModels` dedupes by lowercased `qualified_id`, preserves provider order, and returns all successful catalogs plus a per-realm `catalogs` map. If every candidate fails, return the first error.

- [ ] **Step 1: Write the failing tests**

Add to `models_test.go`:

```go
func TestModelsGatewayUnionsAllProviders(t *testing.T) {
    intl := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "shared", "name": "Intl"}}}
    cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "shared", "name": "CN"}}}
    codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared", "name": "CodeArts"}}}
    raccoon := &stubProvider{id: "raccoon", realm: "raccoon", models: []providers.Model{{"id": "r1"}}}
    srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{intl, cn, codearts, raccoon}})

    rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
    // assert 200, realm == "gateway", 4 entries
    // assert IDs: intl/shared, cn/shared, codearts/shared, raccoon/r1
    // assert upstream_id and realm/provider on the shared entries
}

func TestModelsGatewayPartialCatalogFailure(t *testing.T) {
    good := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "m1"}}}
    broken := &stubProvider{id: "codearts", realm: "codearts", modelsErr: errors.New("offline")}
    srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{good, broken}})

    rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
    // assert 200 and only cn/m1
}

func TestModelsGatewayBoundKeyFiltersRealm(t *testing.T) {
    cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "m1"}}}
    codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "m1"}}}
    srv := newTestHTTPServer(ServerOptions{
        Providers: []providers.Provider{cn, codearts},
        APIKeyRealm: func(*http.Request) string { return "cn" },
    })

    rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=gateway", "", nil)
    // assert 200, only cn/m1, no codearts/m1
}

func TestModelsConcreteRealmAddsQualifiedID(t *testing.T) {
    cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "m1"}}}
    srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{cn}})

    rec := serveRequest(srv.Handler, http.MethodGet, "/v1/models?realm=cn", "", nil)
    // assert id == "m1", qualified_id == "cn/m1", upstream_id == "m1"
}
```

Keep the existing `TestModelsRealmAllUnion` and `TestModelsRealmAllLockedWithoutWorkBuddy` assertions unchanged; they must still pass.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/api -run 'TestModelsGateway|TestModelsConcreteRealmAddsQualifiedID' -v`
Expected: FAIL because `gateway` is treated as an unknown realm and metadata fields are absent.

- [ ] **Step 3: Implement the gateway catalog**

In `handleModels`, resolve the bound realm first. If the request scope is `gateway`, keep the gateway scope but restrict candidates to the bound provider when a binding exists. Add the `gateway` branch before the `realm=all` branch. Use `writeModelsResponse` for legacy scopes and a new response writer for gateway that includes `catalogs`.

For concrete realms, decorate every model with `canonicalID=false`; for gateway, use `canonicalID=true`.

- [ ] **Step 4: Run the API model tests**

Run: `go test ./internal/api -run 'TestModels' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/models.go internal/api/models_test.go
git commit -m "feat(api): add gateway model catalog"
```

---

## Task 4: Chat Ref Plumbing and Exact Exit Routing

**Files:**
- Modify: `internal/api/chat.go`
- Modify: `internal/api/auth.go`
- Test: `internal/api/chat_test.go`
- Test: `internal/api/auth_platform_test.go` if the key fixture is reused

**Interfaces:**
- Extend `chatHTTPRequest`:
  ```go
  requestedModel string // canonical when qualified, otherwise the bare ID
  exit           string // lower-case registered realm, or ""
  ```
- `model` remains the upstream ID sent to the provider.
- Produces:
  ```go
  func (s *Server) applyModelRef(request *chatHTTPRequest) *apiFailure
  func (s *Server) keyModelFailureForRef(r *http.Request, ref modelRef) *apiFailure
  ```
- `applyModelRef` parses `request.model`, sets `requestedModel`, `exit`, and `model` (upstream), and rewrites `request.payload["model"]` to the upstream ID.
- `resolveChatProvider` uses `request.exit` before any realm/model matching.

- [ ] **Step 1: Write the failing routing tests**

Add tests using three stub providers with the same model `shared`:

```go
func TestChatQualifiedModelRoutesExactExit(t *testing.T) {
    intl := &stubProvider{id: "workbuddy", realm: "intl", models: []providers.Model{{"id": "shared"}}, openStream: chatSuccessStream()}
    cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "shared"}}, openStream: chatSuccessStream()}
    codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "shared"}}, openStream: chatSuccessStream()}
    srv := newTestHTTPServer(ServerOptions{Providers: []providers.Provider{intl, cn, codearts}})

    rec := serveRequest(srv.Handler, http.MethodPost, "/v1/chat/completions",
        `{"model":"codearts/shared","messages":[{"role":"user","content":"hi"}]}`, nil)
    // assert 200
    // assert codearts.openRequests[0].Model == "shared"
    // assert Raw["model"] decodes to "shared"
    // assert intl/cn have no open requests
    // assert response JSON model == "codearts/shared"
}
```

Add failure tests:

- `?realm=codearts` + `cn/shared` → 400.
- `APIKeyRealm` returns `cn` + `codearts/shared` → 403.
- `?realm=all` + `cn/shared` → 200 and routes to cn.
- Bare `shared` with `?realm=cn` still routes to cn and response `model` is `shared`.
- Qualified request sets `AllowRealmFallback` to a non-nil false.
- A scenario with `ServerOptions.ProtocolObserver` asserts `Model == "shared"` and `RequestedModel == "codearts/shared"` for the successful request.
- `HandleChat(w, r, cnProvider, "codearts/shared")` returns 400 because the explicit provider realm conflicts with the prefix.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/api -run 'TestChatQualifiedModel|TestChatBareModelCompatibility' -v`
Expected: FAIL because `codearts/shared` is treated as a literal model.

- [ ] **Step 3: Implement the plumbing**

In `HandleChat`, apply the explicit `HandleChat(provider, model)` override before parsing the reference. After the model-required check, call `s.applyModelRef(request)`. If an explicit provider was supplied and `request.exit` is non-empty but does not match `provider.Realm()`, return 400. Then use `keyModelFailureForRef`, keep the bound-realm check, and resolve. Use `request.requestedModel` for failure observations, response model arguments, and `ProtocolObservation.RequestedModel`; use `request.model` for `ChatRequest.Model` and `ProtocolObservation.Model`.

In `resolveChatProvider`, when `request.exit` is non-empty:

```go
if boundRealm != "" && boundRealm != request.exit {
    return nil, localFailure(http.StatusForbidden, "api key realm does not allow exit "+request.exit)
}
if realm := strings.TrimSpace(request.realm); realm != "" && realm != "all" && realm != "gateway" && realm != request.exit {
    return nil, localFailure(http.StatusBadRequest, "realm and model exit conflict")
}
provider := s.providerForRealm(request.exit)
if provider == nil {
    return nil, localFailure(http.StatusNotFound, "unknown exit: "+request.exit)
}
return provider, nil
```

Do not add the gateway bare-model branch yet; Task 5 owns it.

- [ ] **Step 4: Run the targeted tests**

Run: `go test ./internal/api -run 'TestChatQualifiedModel|TestChatBareModelCompatibility|TestChatKeyRealm' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/chat.go internal/api/auth.go internal/api/chat_test.go internal/api/auth_platform_test.go
git commit -m "feat(api): route exit-qualified chat models"
```

---

## Task 5: Gateway Bare-Model Resolution

**Files:**
- Modify: `internal/api/chat.go`
- Test: `internal/api/chat_test.go`

**Interfaces:**
- Produces:
  ```go
  func (s *Server) resolveGatewayAmong(ctx context.Context, model string) (providers.Provider, error)
  ```
- Behavior:
  - Call `Models(ctx, false)` for every registered provider with `catalogResolutionTimeout`.
  - Any error → 503 `gateway model catalog incomplete; use exit/model or a concrete realm`.
  - Zero matches → 404 `model not found in gateway exits: <model>`.
  - One match → that provider.
  - Multiple matches → 400 whose message lists the candidate canonical IDs.

- [ ] **Step 1: Write the failing tests**

Add:

```go
func TestChatGatewayBareModelUnique(t *testing.T) {
    cn := &stubProvider{id: "workbuddy", realm: "cn", models: []providers.Model{{"id": "only-cn"}}, openStream: chatSuccessStream()}
    codearts := &stubProvider{id: "codearts", realm: "codearts", models: []providers.Model{{"id": "only-codearts"}}, openStream: chatSuccessStream()}
    // request ?realm=gateway with model only-cn -> 200, cn receives it
}

func TestChatGatewayBareModelAmbiguous(t *testing.T) {
    // intl and cn both expose shared; ?realm=gateway + shared -> 400
    // response message contains both "intl/shared" and "cn/shared"
}

func TestChatGatewayBareModelCatalogFailure(t *testing.T) {
    // one provider modelsErr; ?realm=gateway + unique model -> 503
}
```

Also assert a qualified `codearts/shared` with `?realm=gateway` still routes directly and does not call the other catalogs.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/api -run TestChatGatewayBareModel -v`
Expected: FAIL because `realm=gateway` is treated as an unknown realm.

- [ ] **Step 3: Implement `resolveGatewayAmong` and the branch**

Add the branch in `resolveChatProvider` before the `realm=all` branch:

```go
if realm == "gateway" {
    return s.resolveGatewayAmong(ctx, request.model)
}
```

`resolveGatewayAmong` must not use `resolveAmong`, because `resolveAmong` tolerates catalog failures and returns on the first match. Collect all matches and only route when exactly one is found.

- [ ] **Step 4: Run the gateway tests**

Run: `go test ./internal/api -run 'TestChatGateway' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/chat.go internal/api/chat_test.go
git commit -m "feat(api): resolve bare models across gateway exits"
```

---

## Task 6: Canonical Model Echo for Chat and Web Tools

**Files:**
- Modify: `internal/api/sse.go`
- Modify: `internal/api/chat.go`
- Modify: `internal/api/chat_webtools.go`
- Test: `internal/api/chat_test.go`
- Test: `internal/api/chat_webtools_test.go` or the existing web-tool test file

**Interfaces:**
- Add `forceModel bool` to `streamNormalizer`.
- Produces:
  ```go
  func newStreamNormalizerForModel(model string, forceModel bool, printed ...*printedToolRecovery) *streamNormalizer
  func writeChatStreamFor(w http.ResponseWriter, r *http.Request, stream providers.ChatStream, model string, forceModel bool, printed ...*printedToolRecovery) chatStreamResult
  ```
- `newStreamNormalizer` and `writeChatStream` remain as compatibility wrappers with `forceModel=false`.
- In `absorb`, only overwrite `n.model` from an upstream chunk when `!n.forceModel`.
- Web tools use `request.model` for provider rounds and `request.requestedModel` for the final client-visible completion/stream.

- [ ] **Step 1: Write the failing tests**

Add a stream fixture whose chunks carry `"model":"upstream-alias"`, then:

```go
func TestChatQualifiedStreamEchoesCanonicalModel(t *testing.T) {
    // POST codearts/shared with stream:true; collect SSE body
    // every data chunk that has "model" must contain "codearts/shared"
    // no chunk may contain "upstream-alias"
}

func TestChatBareStreamKeepsUpstreamModel(t *testing.T) {
    // same fixture with bare "shared" and ?realm=codearts
    // chunks keep "upstream-alias" (current behavior)
}
```

Add a direct test for `writeChatStreamFor` with `forceModel=true` if the HTTP fixture is too indirect.

For web tools, assert the final non-stream payload has `model == "codearts/shared"` and the provider round observed `Model == "shared"`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/api -run 'TestChatQualifiedStreamEchoesCanonicalModel|TestChatBareStreamKeepsUpstreamModel|TestChatWebTools.*Canonical' -v`
Expected: FAIL because the normalizer overwrites `model` from chunks.

- [ ] **Step 3: Implement the force flag and web-tool plumbing**

In `HandleChat`, choose `writeChatStreamFor(..., request.requestedModel, request.exit != "", printed)` and `writeChatCompletion(..., request.requestedModel, printed)`. In `runChatWebRounds`, keep `request.model` for `aggregateChatStream`; set `final["model"] = request.requestedModel` before returning when `request.requestedModel` is non-empty. In the stream web-tool handler, call `writeChatStreamFor` with the same requested/force values.

- [ ] **Step 4: Run the Chat tests**

Run: `go test ./internal/api -run 'TestChat' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/sse.go internal/api/chat.go internal/api/chat_webtools.go internal/api/chat_test.go internal/api/chat_webtools_test.go
git commit -m "fix(api): echo canonical model ids in chat streams"
```

---

## Task 7: Responses Protocol Integration

**Files:**
- Modify: `internal/api/responses.go`
- Modify: `internal/api/responses_stream.go`
- Modify: `internal/api/responses_compact.go`
- Modify: `internal/api/responses_webtools.go`
- Test: `internal/api/responses_test.go`

**Interfaces:**
- Reuse `applyModelRef` and `keyModelFailureForRef` by constructing the `chatHTTPRequest` before translation.
- `serveResponsesStream(..., model string, ...)` receives the client-visible requested model.
- `chatToResponse(chat, requestedModel, meta)` receives the requested model.
- `ProtocolObservation.Model` receives the upstream ID; `RequestedModel` receives the canonical/bare requested ID.

- [ ] **Step 1: Write the failing tests**

Add:

- `responses` with `codearts/shared` routes to codearts and the provider sees `Model == "shared"` / `Raw["model"] == "shared"`.
- Non-stream response body `model == "codearts/shared"`.
- Stream events `response.created` / `response.completed` have `model == "codearts/shared"` even when the stub chunks carry `"model":"upstream-alias"`.
- Bound Key `cn` + `codearts/shared` → 403.
- `?realm=gateway` + unique bare model → routes; ambiguous → 400.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/api -run 'TestResponses.*Qualified|TestResponses.*Gateway' -v`
Expected: FAIL because the prefix is passed through as the model.

- [ ] **Step 3: Implement the integration**

Parse the reference before `translateResponsesRequest`. Write the upstream model back into `payload["model"]` so the translated chat map and `ChatRequest.Model` receive the bare ID. Use the requested ID for `chatToResponse`, `serveResponsesStream`, failure observations, and `ProtocolObservation.RequestedModel`. Set `AllowRealmFallback=false` when `request.exit != ""`.

Update `responses_webtools.go` so provider rounds use the upstream model and final output uses the requested model. Update `responses_compact.go` so `writeResponsesCompaction` receives the requested model for `responsesCompactionResponse` and observations, while its aggregate uses the upstream stream/chat data.

- [ ] **Step 4: Run the Responses tests**

Run: `go test ./internal/api -run 'TestResponses' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/responses.go internal/api/responses_stream.go internal/api/responses_webtools.go internal/api/responses_test.go
git commit -m "feat(api): support exit-qualified responses models"
```

---

## Task 8: Anthropic Messages Integration

**Files:**
- Modify: `internal/api/messages.go`
- Modify: `internal/api/messages_stream.go`
- Test: `internal/api/messages_test.go`
- Test: `internal/api/messages_stream_test.go`

**Interfaces:**
- Parse the reference in `handleMessages` before `messagesToChatWithDefault` so default max-output lookup uses the upstream ID.
- Rewrite `payload["model"]` to the upstream ID before calling `messagesToChatWithDefault`; pass the requested model and exit separately into `serveMessagesRequest`.
- Pass the requested model into `serveMessagesRequest` and `serveMessagesStream`.
- `chat["model"]` must be the upstream ID while building `rawChat` and the `ChatRequest`; after `chatMapToRaw`, set `chat["model"]` back to the requested ID for output helpers.
- `ProtocolObservation.Model` receives upstream; `RequestedModel` receives requested.

- [ ] **Step 1: Write the failing tests**

Add:

- `messages` with `codearts/shared` routes to codearts; provider sees `Model == "shared"`.
- Non-stream response body `model == "codearts/shared"`.
- Stream `message_start.message.model` and the aggregate message model are `codearts/shared`, even if the stub chunk says `upstream-alias`.
- Bound Key `cn` + `codearts/shared` → 403.
- Bare model + `?realm=cn` unchanged.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/api -run 'TestMessages.*Qualified|TestMessages.*Canonical' -v`
Expected: FAIL because the prefix is used as the model.

- [ ] **Step 3: Implement the integration**

Remove the duplicate key check from `handleMessages`; do it once in `serveMessagesRequest` after parsing the reference. Set `chat["model"]` to upstream before `chatMapToRaw`, build `ChatRequest.Model` from upstream, then restore `chat["model"]` to requested before `chatToAnthropic`/stream output. Use requested for failure observations and response models.

- [ ] **Step 4: Run the Messages tests**

Run: `go test ./internal/api -run 'TestMessages' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/messages.go internal/api/messages_stream.go internal/api/messages_test.go internal/api/messages_stream_test.go
git commit -m "feat(api): support exit-qualified messages models"
```

---

## Task 9: Usage Attribution Regression Tests

**Files:**
- Modify: `internal/api/usage_hook_test.go` or add `internal/api/usage_modelref_test.go`
- Test: `internal/platform/usage/recorder_test.go` only if a recorder change is needed

**Interfaces:**
- The recording contract is already present: `RecordInput.Model` is the upstream ID and `RequestedModel` is the canonical ID.
- This task proves all three protocols set them correctly; it should not change pricing or row schema.

- [ ] **Step 1: Write the failing assertions**

Use `ServerOptions.RecordUsage` or `ProtocolObserver` to capture observations for Chat, Responses, and Messages qualified requests. Assert:

```go
if got.Model != "shared" {
    t.Fatalf("billing model = %q", got.Model)
}
if got.RequestedModel != "codearts/shared" {
    t.Fatalf("requested model = %q", got.RequestedModel)
}
if got.Realm != "codearts" {
    t.Fatalf("realm = %q", got.Realm)
}
```

Add a bare-request assertion that `Model == RequestedModel == "shared"` and `RequestedModel` is omitted from the persisted row when equal.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/api -run 'TestUsage.*Qualified' -v`
Expected: FAIL if any protocol still records the canonical ID as `Model` or the upstream ID as `RequestedModel`.

- [ ] **Step 3: Fix only the attribution wiring**

Keep `model` upstream for pricing. Use the requested/canonical value only for `RequestedModel` and client-visible fields. Do not change `recorder.go` unless the assertion shows `requested_model` is dropped.

- [ ] **Step 4: Run the usage tests**

Run: `go test ./internal/api -run 'TestUsage' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/usage_hook_test.go internal/platform/usage/recorder_test.go
git commit -m "test(api): pin exit-qualified usage attribution"
```

---

## Task 10: Dashboard Key Model Picker (Phase 2)

**Files:**
- Modify: `internal/web/dashboard.html`
- Modify: `internal/web/assets.go`
- Modify: `tests/oracle/dashboard.md`
- Create: `tests/dashboard/node/_test_key_model_catalog.js`
- Modify: `tests/dashboard/node/run.js`

**Interfaces:**
- `loadKeyModelCatalog()` first requests `/v1/models?realm=gateway`; only if that fails or returns no data does it use the existing unscoped/intl/cn fallback.
- Add:
  ```js
  function filterKeyModelsForRealm(models, realm){
    const want = String(realm || '').trim().toLowerCase();
    if(!want) return models || [];
    return (models || []).filter(m => !m || !m.realm || String(m.realm).toLowerCase() === want);
  }
  ```
- `renderKeyPickers(i)` calls `filterKeyModelsForRealm(models, row.realm)` before rendering.

- [ ] **Step 1: Write the failing dashboard suite**

Create `_test_key_model_catalog.js` using `loadDashboard` and a fake `fetch`:

- gateway response returns:
  ```js
  [{id:'cn/m1', realm:'cn'}, {id:'codearts/m1', realm:'codearts'}, {id:'intl/m1', realm:'intl'}]
  ```
- assert `await page.loadKeyModelCatalog()` calls the gateway URL first and returns the three IDs.
- assert `page.filterKeyModelsForRealm(models, 'cn')` returns only `cn/m1`.
- assert `filterKeyModelsForRealm(models, '')` returns all.
- assert gateway failure falls back to `/v1/models` and does not leave the picker empty.
- Add the suite name to `SUITES` in `run.js`.

- [ ] **Step 2: Run the suite and confirm it fails**

Run: `go test ./tests/dashboard -run TestDashboardJSHarness -v`
Expected: FAIL because the gateway-first function/filter does not exist.

- [ ] **Step 3: Implement the picker changes**

Update `loadKeyModelCatalog` to try the gateway URL first, cache the result, and retain the old fallbacks. Update `renderKeyPickers` to filter by `row.realm` and render `m.id`. Do not change the model page, account-disabled-model picker, or analytics matrix in this task.

- [ ] **Step 4: Update the asset hash and oracle**

```bash
sha256sum internal/web/dashboard.html
```

Replace `web.DashboardSHA256` in `internal/web/assets.go` and the v2 SHA/size rows in `tests/oracle/dashboard.md` with the new values.

- [ ] **Step 5: Run the dashboard tests**

Run: `go test ./tests/dashboard -v`
Expected: PASS, including the new suite.

- [ ] **Step 6: Commit**

```bash
git add internal/web/dashboard.html internal/web/assets.go tests/oracle/dashboard.md tests/dashboard/node/_test_key_model_catalog.js tests/dashboard/node/run.js
git commit -m "feat(web): let key picker use gateway model ids"
```

---

## Task 11: Analytics Model Keys and Matrix Whitelist (Phase 2)

**Files:**
- Modify: `internal/platform/usage/row.go`
- Modify: `internal/platform/usage/analytics.go`
- Modify: `internal/platform/usage/snapshot.go`
- Modify: `internal/platform/usage/perf.go`
- Modify: `internal/web/dashboard.html`
- Modify: `tests/oracle/dashboard.md`
- Test: `internal/platform/usage/analytics_test.go`
- Test: `internal/platform/usage/snapshot_test.go`
- Test: `internal/platform/usage/perf_test.go`
- Test: `tests/dashboard/node/_test_analytics_realm_scope.js` or a new matrix suite

**Interfaces:**
- Add to `row.go`:
  ```go
  func rowModelKey(row Row) string {
      if requested := rowString(row, "requested_model"); requested != "" {
          return requested
      }
      return rowString(row, "model")
  }
  ```
- Replace the model extraction in analytics, snapshot, and perf with `rowModelKey`.
- The existing `(unknown)`/`unknown` fallbacks stay in the callers.
- Dashboard matrix switches its whitelist request from `/v1/models?realm=all` to `/v1/models?realm=gateway`.

- [ ] **Step 1: Write the failing usage tests**

Add rows with:

```go
Row{"model": "deepseek", "requested_model": "cn/deepseek", "realm": "cn", "outcome": "completed"}
Row{"model": "deepseek", "requested_model": "codearts/deepseek", "realm": "codearts", "outcome": "completed"}
Row{"model": "deepseek", "realm": "intl", "outcome": "completed"}
```

Assert:

- analytics `models` keys are `cn/deepseek`, `codearts/deepseek`, and `deepseek`.
- snapshot `by_model` has the same three keys.
- perf `by_model` has the same three keys.
- rows without `requested_model` still group by `model`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/platform/usage -run 'Test.*RequestedModel|Test.*ModelKey' -v`
Expected: FAIL because all three rows group under `deepseek`.

- [ ] **Step 3: Implement the shared key helper**

Replace `rowString(row, "model")` in the three aggregation paths with `rowModelKey(row)`. Keep pricing and per-row display unchanged.

- [ ] **Step 4: Update the matrix whitelist**

In `loadAnalyticsMatrix`, change the catalog request to `/v1/models?realm=gateway`. `MATRIX_MODELS` continues to use `m.id`, which is canonical in gateway scope. Update the JS suite to assert the gateway URL and canonical IDs.

- [ ] **Step 5: Update the asset hash and oracle**

Recalculate `DashboardSHA256` and the oracle v2 SHA/size after the dashboard edit.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/platform/usage -v && go test ./tests/dashboard -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/platform/usage/row.go internal/platform/usage/analytics.go internal/platform/usage/snapshot.go internal/platform/usage/perf.go internal/web/dashboard.html internal/web/assets.go tests/oracle/dashboard.md tests/dashboard/node/_test_analytics_realm_scope.js
git commit -m "feat(usage): group model analytics by exit-qualified id"
```

---

## Task 12: Agent Model Picker (Phase 2)

**Files:**
- Modify: `internal/platform/agents/models.go`
- Test: `internal/platform/agents/models_test.go`
- Test: `internal/platform/agents/apply_test.go` if generated configs are asserted

**Interfaces:**
- `GatewayModels(ctx, list)` includes every provider, not only the active WorkBuddy realm plus CodeArts official IDs.
- Every emitted `ModelSpec.ID` is `provider.Realm() + "/" + upstreamID`.
- Prefer `AgentPickerModels` when a provider implements it; otherwise use `Models`.
- Append `AgentOfficialModels` entries that are not already present, also prefixed by the provider realm.
- `MergeModels` dedupes canonical IDs and keeps the 80-model cap.

- [ ] **Step 1: Write the failing tests**

Update `TestGatewayModelsUsesPickerOrderAndOfficialIds`:

- intl, cn, codearts, raccoon all contribute.
- Expected IDs include `intl/zeta`, `intl/alpha`, `cn/cn-only`, `codearts/deepseek-v4.1-flash-1017`, `raccoon/raccoon-090dd3`.
- No bare IDs appear.
- The CodeArts official ID that duplicates `alpha` must not duplicate `intl/alpha` under a second ID.

Rename/update `TestGatewayModelsFollowsActiveWorkBuddyRealm` to `TestGatewayModelsIncludesEveryWorkBuddyRealm` and assert both `intl/intl-model` and `cn/cn-model` are present; active realm no longer hides the other WorkBuddy exit.

Add an apply test asserting a generated provider config contains a canonical ID when that model is selected.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/platform/agents -run 'TestGatewayModels|Test.*Canonical' -v`
Expected: FAIL because only the active WorkBuddy realm and bare CodeArts IDs are returned.

- [ ] **Step 3: Implement the all-exit picker**

Replace `pickerModels`’ single-realm selection with a loop over all providers. Keep the richer `AgentPickerModels` metadata when available, prefix the ID, and append official IDs with the same prefix. Preserve sorting and the existing cap.

- [ ] **Step 4: Run the agent tests**

Run: `go test ./internal/platform/agents -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/platform/agents/models.go internal/platform/agents/models_test.go internal/platform/agents/apply_test.go
git commit -m "feat(agents): emit exit-qualified model ids"
```

---

## Task 13: Documentation and Release Note

**Files:**
- Modify: `README.md`
- Modify: `README.en.md` if it carries the same API example
- Modify: `docs/superpowers/specs/2026-11-12-exit-qualified-models-design.md` status to `已实施` only after all tasks pass

**Interfaces:**
- Document `GET /v1/models?realm=gateway`.
- Document the four canonical prefixes.
- Document that `realm=all` remains WorkBuddy-only.
- Document that bare IDs remain accepted.
- Document Key binding precedence.

- [ ] **Step 1: Add a short “全出口模型 ID” section**

Include one curl example:

```bash
curl -fsS http://127.0.0.1:58675/v1/models?realm=gateway
```

and one chat example:

```json
{"model":"codearts/deepseek-v4.1-flash-1017","messages":[{"role":"user","content":"hi"}]}
```

- [ ] **Step 2: Verify the docs mention both compatibility rules**

Run:

```bash
grep -n "realm=gateway\|realm=all\|codearts/" README.md README.en.md
```

Expected: gateway and `realm=all` compatibility are both mentioned.

- [ ] **Step 3: Commit**

```bash
git add README.md README.en.md docs/superpowers/specs/2026-11-12-exit-qualified-models-design.md
git commit -m "docs: document gateway model ids"
```

---

## Self-Review Checklist

- Every spec section maps to a task:
  - parser rules → Task 1
  - gateway catalog → Task 3
  - exact exit routing → Task 4
  - gateway bare resolution and 503 → Task 5
  - canonical echo → Tasks 6–8
  - Key allow-list → Task 2 and Task 4
  - usage attribution → Task 9
  - dashboard Key picker → Task 10
  - analytics grouping/matrix → Task 11
  - agent picker → Task 12
  - docs → Task 13
- No task changes `realm=all` model/chat semantics.
- No task forwards a prefix to a provider.
- No task adds cross-provider fallback.
- Phase 2 tasks are not started until Phase 1 is merged; Phase 1 is independently shippable.
