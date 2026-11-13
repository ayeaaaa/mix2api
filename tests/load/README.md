# Load harness

Two manual harnesses run the same workload: 200 concurrent streaming chat
requests through the real gateway and a loopback fake WorkBuddy upstream. Each
records p50/p95/p99 time to first SSE byte, failures, `[DONE]` termination,
elapsed time, process RSS/CPU, and rows appended to `usage.jsonl`.

Both are skipped from the normal suite. The v1 harness additionally requires a
private network namespace so the ignored v1.6.5 copy cannot reach a real
provider.

## v2 absolute gate

`TestSSELoad` is the v2 method. It starts the real v2 HTTP server in-process and,
by default, an in-process Go fake upstream. The earlier in-process-fake sanity
run recorded p95 162 ms; the baseline below uses the external oracle fake so
both targets see the same upstream bytes and framing.

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
| `MIX2API_LOAD_UPSTREAM` | unset | use an external loopback fake instead of the in-process one |
| `MIX2API_LOAD_TARGET_VERSION` | `2.0.0-load` | version reported by the test server and evidence |
| `MIX2API_LOAD_TARGET_COMMIT` | unset | commit recorded in evidence |

## v1.6.5 baseline

The v1 harness starts the ignored copy under `tests/oracle/v1.6.5`, the shared
oracle fake upstream (`tests/oracle/fakes/fake_upstream.py`), and synthetic
WorkBuddy accounts. It never starts or edits `/root/opencode/mix2api`.

```sh
# Requires unshare and a loopback interface. Writes one run per invocation.
tests/load/run-v1-baseline.sh /tmp/v1-baseline-run.json
```

The wrapper runs the test inside `unshare -n`. The test is also gated on
`MIX2API_LOAD_V1=1` and `MIX2API_LOAD_NETNS=1`; running it directly without the
wrapper is refused. The v1 tree must be present from the documented archive
copy (pin `e40c4e8994f9cc6ace3457e350466cdd5d422a37`); the original tree must
stay clean:

```sh
git -C /root/opencode/mix2api status --short   # must stay empty
```

The baseline profile matches the v2 load profile:

- 8 synthetic intl WorkBuddy accounts in a temporary directory;
- one API key (`load-api-key`) and one 3-token completion per request;
- model `hy4-preview`, `stream: true`, no warm-up;
- v1 `WB_MAX_CONCURRENT_CHAT=232` matches v2 `max_concurrent=232`;
- all upstream origins point at the loopback fake.

## v2 baseline with the same oracle fake

`TestSSELoad` normally uses its in-process fake. For the baseline comparison,
the wrapper starts the same Python oracle fake used by v1 and points the v2 test
at it. This keeps the upstream response bytes and framing identical across the
two targets:

```sh
tests/load/run-v2-baseline.sh /tmp/v2-baseline-run.json
```

The wrapper also runs inside `unshare -n` and records `2.0.0-rc.1` plus the
current v2 commit.

`fake_upstream.py` raises the stdlib listen backlog to 256. Without it, 200
simultaneous loopback connects are dominated by TCP SYN retransmits (the v2
external-fake p95 was ~6.9 s); the change alters no response bytes.

## Recorded comparison (2026-11-11)

Environment: Linux amd64, 32 logical CPUs, Go 1.22.2, Python 3.12.3, private
network namespace, loopback-only fake upstream. Each target was run three times;
the table shows the three rounds and the summary.

### v1.6.5 (`e40c4e8`)

| Round | p50 (ms) | p95 (ms) | p99 (ms) | elapsed (ms) | failures | `[DONE]` | usage rows | RSS peak (KB) | CPU (s) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 889 | 1520 | 1567 | 1588 | 0 | 200 | 200 | 86608 | 2.07 |
| 2 | 999 | 1571 | 1622 | 1643 | 0 | 200 | 200 | 89072 | 2.11 |
| 3 | 953 | 1546 | 1582 | 1619 | 0 | 200 | 200 | 88064 | 2.11 |
| mean | 947.0 | 1545.7 | 1590.3 | 1616.7 | 0 | 600 | 600 | 87914.7 | 2.10 |

p95 range 1520-1571 ms (51 ms, 3.3%); elapsed range 1588-1643 ms (55 ms).

### v2.0.0-rc.1 (`49fa5b4`)

| Round | p50 (ms) | p95 (ms) | p99 (ms) | elapsed (ms) | failures | `[DONE]` | usage rows | RSS peak (KB) | CPU (s) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 206 | 322 | 333 | 337 | 0 | 200 | 200 | 66660 | 1.69 |
| 2 | 215 | 328 | 338 | 343 | 0 | 200 | 200 | 65364 | 1.61 |
| 3 | 196 | 313 | 324 | 328 | 0 | 200 | 200 | 69532 | 1.80 |
| mean | 205.7 | 321.0 | 331.7 | 336.0 | 0 | 600 | 600 | 67185.3 | 1.70 |

p95 range 313-328 ms (15 ms, 4.7%); elapsed range 328-343 ms (15 ms). The
v2 SQLite log-store check appended 200 additional rows after the measured
workload; `sqlite_append_ms` was 30-39 ms and is not included in the latency.

### Target result

| Metric | Value |
| --- | ---: |
| v1 p95 mean | 1545.7 ms |
| v2 p95 mean | 321.0 ms |
| **p95 ratio v2 / v1** | **0.208** |
| Target | <= 1.20 |
| Result | **PASS** (v2 is ~4.8x lower) |
| Absolute p95 target | 2000 ms |
| Absolute result | **PASS** (max 328 ms) |

The committed raw rounds and summaries are in `v1-baseline.json` and
`v2-baseline.json`.

## Interpretation and limits

- This is local fake-upstream evidence, not a prediction of real provider
  latency. The fake emits a fixed 3-token completion immediately.
- v1's first-byte latency is dominated by Python thread/GIL scheduling under a
  200-way burst. That is a real v1 runtime characteristic, not an artificial
  delay; v1 was given the same 232 chat slots as v2.
- v1's default `WB_MAX_CONCURRENT_CHAT=32` would be slower. The baseline
  deliberately raises it so the comparison measures the runtimes, not the
  default queue limit.
- RSS is the whole process: Python runtime + v1 gateway for v1; Go test binary +
  in-process v2 server + SQLite for v2. It is not a pure server RSS.
- The v2 target commit is the server code under test (`49fa5b4`); the harness
  changes in this tree only add the external-fake switch and metrics.
- No real account or external network is used. The v1 source tree is never
  modified; only the ignored `tests/oracle/v1.6.5` copy is executed.
