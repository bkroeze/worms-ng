# Performance budgets and repeatable measurement

Budgets are release gates, not promises about arbitrary hardware. The baseline is
an Ubuntu 22.04/24.04 runner or developer laptop with four logical cores, 8 GiB
RAM, SSD storage, Go matching `go.mod`, and Chromium 120+ at 100% scale. Run three
warm repetitions and report the median and the slowest result. Do not compare a
cold build with a warm gameplay run.

## Budgets

| Area | Representative scenario | Budget (p95/limit) |
| --- | --- | --- |
| Engine | deterministic transition on the default board fixture | <= 2 ms/op, <= 64 KiB/op |
| Engine maximum | transition on the largest board fixture (named benchmark) | <= 20 ms/op, <= 512 KiB/op |
| Store | transactional create/append/resume against local SQLite | <= 50 ms/op; no partial event |
| Store replay | verify/replay 250 ordered events from a copied database | <= 250 ms total |
| WASM delivery | generated content-hashed `.wasm` asset | <= 32 MiB; loader <= 256 KiB |
| Rendering | default and maximum board at 1280x720 | 60 Hz target; frame work <= 16.7 ms p95 |
| Rendering allocation | representative steady-state frame | <= 1 MiB allocations/frame and no growth |

The maximum-board dimensions are the dimensions named by the engine's current
rules fixture. If that fixture changes, update this table and the release record
in the same change; never silently widen a budget. Measurements must include
scenario, commit, Go version, browser version, viewport, device scale, and whether
the run was cold or warm.

`make bench` invokes `scripts/benchmark.sh`. It runs engine, match, and store
microbenchmarks with `-benchmem`, requires numeric output for each package,
writes raw output and machine-readable summaries when `PERF_ARTIFACT_DIR` is
set, and enforces both time and bytes/op budgets. Engine results must include
the default and maximum-board benchmark names; a missing named scenario is a
release failure rather than an unmeasured pass. For a quick local run:

```sh
PERF_ARTIFACT_DIR=dist/perf BENCHTIME=1s make bench
```

A release run should use the default `BENCHTIME=1s`; CI uses a shorter warm run
only as a fast regression gate. Use explicit budget variables to document an
intentional hardware-specific override in the release record; do not disable
numeric or allocation checks. Store replay evidence is produced from a copied
database and must remain below the 250-event limit.

The SQLite replay budget is measured against a copy so the benchmark cannot
change production state. `make db-check DB=/path/to/copy.db` must pass before
recording that result.

## WASM and browser measurement

```sh
WASM_ARTIFACT_DIR=dist/perf-wasm make performance
```

This builds fresh content-hashed assets, checks their sizes/hashes, and records
native summaries. For a real browser run, install `agent-browser` and repeat
each release viewport:

```sh
BROWSER_PERF=1 WASM_ARTIFACT_DIR=dist/perf-wasm \
  BROWSER_VIEWPORT=1280x720 make performance
```

The browser procedure records request/console evidence and `frame-metrics.json`
with at least 120 animation frames. It fails when frame-work p95 exceeds
16.7ms, the canvas is blank/clipped, a request is HTTP 400+, or console/page
errors occur. Use the Chrome trace plus the machine-readable frame report to
record heap/allocation growth; native microbenchmarks do not substitute for
browser measurements.
