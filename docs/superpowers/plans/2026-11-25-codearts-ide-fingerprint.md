# CodeArts IDE Request Fingerprint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Align CodeArts chat requests with the observed CodeArts Agent 26.11.300 application-layer fingerprint.

**Architecture:** Add a focused `fingerprint.go` helper that derives IDE-shaped trace/session/message IDs and model display values. Wire it into `newChatRequest` before signing, remove the legacy `Chat-Id`/`Session-Id` outbound headers, add `user_prompt` to the body, and keep plaintext with `X-Encrypt-Capable: sm4-gcm` because the real client's key exchange currently fails and falls back to plaintext.

**Tech Stack:** Go standard library (`crypto/rand`, `crypto/sha256`, `net`), existing CodeArts `SignRequest` and SSE stream.

**Spec:** `docs/superpowers/specs/2026-11-25-codearts-ide-fingerprint-design.md`

## Global Constraints

- Preserve token refresh, account selection, signing validity, stream parsing, error classification, and OpenAI response shape.
- Do not implement SM4 encryption in this phase; only advertise `X-Encrypt-Capable: sm4-gcm`, matching the observed live fallback.
- Do not change TLS/HTTP2 transport or header order in this phase.
- Mainland keeps `x-auth-token` and benefit `maas_type` gating; international sends neither.
- Never include credentials in test output or error messages.
- No new dependencies.
- The `User-Agent` and client/plugin constants use environment overrides so a future IDE update does not require code changes.

---

### Task 1: Fingerprint identity helper

**Files:**
- Create: `internal/providers/codearts/fingerprint.go`
- Test: `internal/providers/codearts/fingerprint_test.go`

**Interfaces:**
- Produces: `type ideFingerprint struct` with `TraceID`, `SpanID`, `SessionID`, `MessageID`, `CreatedAt`, `ModelID`, `ModelName`, `ClientIP`, `UserAgent`; `func newIDEFingerprint(uid, model, chatID string, messages []any) ideFingerprint`; `func ideModelName(model string) string`; `func ideUserPrompt(messages []any) string`.
- Consumes: `randomHex`, `chatTruthyText`, existing message/content normalization helpers.

- [ ] Write failing tests for ID shape/stability, model display names, user prompt extraction, and env overrides.
- [ ] Run `go test ./internal/providers/codearts -run 'TestIDEFingerprint|TestIDEModelName|TestIDEUserPrompt' -count=1`; expect undefined symbols.
- [ ] Implement the helper with 26-char base62 `ses_`/`msg_` IDs, trace/span randomness, UTC RFC3339, known model display map, local IPv4/env default, and UA/env default.
- [ ] Run the focused tests; expect PASS.
- [ ] Commit `feat(codearts): derive ide request fingerprint ids`.

### Task 2: Wire headers and user_prompt into chat requests

**Files:**
- Modify: `internal/providers/codearts/chat.go`
- Test: `internal/providers/codearts/chat_test.go`
- Modify any existing tests that assert legacy `Chat-Id`/`Session-Id` outbound headers.

**Interfaces:**
- Consumes: `newIDEFingerprint` from Task 1.
- Produces: chat requests with the IDE header set and `user_prompt` body field.

- [ ] Add a failing fake-upstream test capturing the request headers/body: assert `User-Agent`, `X-Encrypt-Capable`, `x-snap-traceid`, `x-ot-*`, `x-session-affinity`, `user-session-id`, `user-msg-id`, `x-message-id`, `created-time`, `model-id`, `model-name`, client/plugin/IDE fields, `client-ip`; assert `Chat-Id` and `Session-Id` are absent; assert body `user_prompt` equals the last user text.
- [ ] Assert the Authorization `SignedHeaders` includes the new fingerprint headers.
- [ ] Run the focused chat test; expect FAIL.
- [ ] Wire the helper in `newChatRequest`, pass messages from the normalized body, set fingerprint headers before `SignRequest`, remove outbound `Chat-Id`/`Session-Id`, and add `user_prompt` in `buildChatBody`.
- [ ] Update existing tests that expect the removed headers/old trace format; keep `chat_id`/`prompt_cache_key` body fields.
- [ ] Run all codearts tests; expect PASS.

### Task 3: Regression and live smoke

**Files:**
- Test: existing suite
- Modify: design doc with observed live result.

- [ ] Run `go test ./internal/providers/codearts ./internal/api -count=1`, then `go test ./... -count=1` and `go vet ./...`.
- [ ] Build a temporary gateway with the saved international session credential.
- [ ] Send a real `codearts-intl/GLM-5.2` non-stream and `glm-5.3` stream request; confirm 200 and content, and confirm `/codearts-intl/overview` still shows the 5M package.
- [ ] Record in the design doc whether the upstream accepted the larger signed header set and the new User-Agent; if it rejects, revert only the offending header and note it.
- [ ] Commit verification notes.

## Self-Review

- Header values are copied from the live log; IDs match the observed prefixes/lengths.
- No SM4/TLS claim is made beyond the capability header and documented gaps.
- Tests pin header presence, stable session/message IDs, model metadata, and `user_prompt`.
- The existing body allowlist remains the source of truth; only `user_prompt` is added.