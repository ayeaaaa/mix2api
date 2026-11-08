# Contract golden provenance

The JSON/SSE files in this directory are **regression goldens for the v2
foundation**, not outputs captured from a live v1.6.5 oracle run.

- They encode the small surface implemented by the Raccoon vertical slice
  (health, models, chat stream, chat non-stream) and lock down v2's intended
  normalized semantics.
- They have not been diffed against shipped v1.6.5 fake-upstream output in this
  branch. Gate A in the master plan is therefore **partial**: Raccoon
  vertical-slice contract/regression tests pass, but full v1.6.5 oracle parity
  remains open.
- When recording real oracle fixtures, follow `tests/oracle/README.md`, keep the
  v1.6.5 tree read-only and out of git, and replace these files only after
  reviewing semantic differences.

The `health.golden.json` fixture intentionally covers only the v2 foundation
fields; v1's panel/media/key readiness blocks are out of scope for this slice.

## CodeArts fixtures

The `codearts_*.golden.json` and `codearts_chat_stream.golden.json` files are
also **v2 regression fixtures**, generated from the local fake CodeArts upstream
through the real provider and API server. They are **not captured v1.6.5 oracle
output**. They cover the scoped CodeArts surface: provider health, the exact
`/v1/models?realm=codearts` union (including `DeepSeek-V4.1-Flash-1017`), chat
non-stream and SSE (text and tool calls), overview with no credential fields,
check-in claim/confirm, credits refresh, models refresh, disabled/error
envelopes, concurrency retry/failover, and credit-exhausted failover/readable
400.

Dynamic values are explicit placeholders: `base_url`, per-second
`token_remaining`, the 1017 peak/off-peak `credits` multiplier, timestamps, and
generated IDs. See `tests/oracle/codearts.md` for the read-only v1.6.5 oracle
procedure. Actual v1.6.5 parity remains **pending** for CodeArts as well; the
goldens lock down v2's scoped semantics and must be reviewed before being
replaced by recorded oracle assertions.