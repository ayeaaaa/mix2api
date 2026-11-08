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