# Load harness

The manual harness sends 200 concurrent streaming chat requests through the
real v2 HTTP server and a loopback fake WorkBuddy upstream. It records:

- p50/p95/p99 time to first SSE byte;
- request failures and `[DONE]` termination;
- process memory before/after, goroutine count, and cumulative CPU seconds;
- rows appended to `usage.jsonl`;
- rows written through the async `logs.sqlite3` store.

It is skipped from the normal suite:

```sh
MIX2API_LOAD=1 \
MIX2API_LOAD_EVIDENCE=/tmp/mix2api-load.json \
go test ./tests/load -run TestSSELoad -v -count=1 -timeout 5m
```

Configuration:

| Env | Default | Meaning |
| --- | --- | --- |
| `MIX2API_LOAD` | unset | `1` enables the test |
| `MIX2API_LOAD_CONCURRENCY` | `200` | concurrent requests |
| `MIX2API_LOAD_P95_MS` | `2000` | hard p95 first-byte threshold |
| `MIX2API_LOAD_EVIDENCE` | unset | write the JSON evidence file |

No real account or external network is used. The 8 accounts are generated in a
temporary directory against the loopback fake upstream.

## Recorded v2 run

Date: 2026-11-10
Platform: Linux amd64, Go 1.22.2, local loopback fake upstream
Command: 200 concurrent SSE, one 3-token completion per request

| Metric | Value |
| --- | --- |
| requests / failures | 200 / 0 |
| elapsed | 149 ms |
| p50 first byte | 120 ms |
| p95 first byte | 140 ms |
| p99 first byte | 145 ms |
| usage rows | 200 |
| SQLite log rows / append time | 200 / 42 ms |
| process memory (`Sys`) before/after | 13 MB / 67 MB |
| goroutines before/after | 11 / 615 |

These numbers are local fake-upstream evidence, not a promise for real provider
latency. The v1.6.5 baseline is **not recorded** in this tree; run the same
method against the v1 fake-upstream setup if a baseline comparison is required
for the Gate F report. The absolute p95 target for this harness is 2000 ms, and
the recorded run is well below it.
