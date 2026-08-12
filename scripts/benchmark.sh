#!/bin/sh
# Run deterministic package benchmarks with numeric per-area budgets.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
: "${GOCMD:=go}"
: "${BENCHTIME:=1s}"
: "${BENCH_BUDGET:=60s}"
: "${ENGINE_BUDGET_NS:=2000000}"
: "${ENGINE_BUDGET_BYTES:=65536}"
: "${ENGINE_MAX_BUDGET_NS:=20000000}"
: "${ENGINE_MAX_BUDGET_BYTES:=524288}"
: "${ENGINE_MAX_BENCHMARK:=}"
: "${MATCH_BUDGET_NS:=20000000}"
: "${MATCH_BUDGET_BYTES:=0}"
: "${STORE_BUDGET_NS:=50000000}"
: "${STORE_BUDGET_BYTES:=0}"
: "${STORE_REPLAY_BUDGET_NS:=250000000}"
command -v timeout >/dev/null 2>&1 || { echo "timeout is required" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }
cd "$ROOT"

cleanup=
if [ -n "$PERF_ARTIFACT_DIR" ]; then
  artifact=$PERF_ARTIFACT_DIR
  mkdir -p "$artifact"
else
  artifact=$(mktemp -d "${TMPDIR:-/tmp}/worms-bench.XXXXXX")
  cleanup=1
fi
cleanup_artifacts() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "benchmark diagnostics: $artifact" >&2
  elif [ -n "$cleanup" ]; then
    rm -rf "$artifact"
  fi
  exit "$status"
}
run_package() {
  label=$1
  package=$2
  budget=$3
  byte_budget=$4
  log="$artifact/$label.txt"
  echo "benchmark: $label package=$package benchtime=$BENCHTIME budget_ns=$budget budget_bytes=$byte_budget"
  if ! timeout --signal=TERM "$BENCH_BUDGET" "$GOCMD" test "$package" -run '^$' -bench . -benchmem -benchtime="$BENCHTIME" >"$log" 2>&1; then
    cat "$log" >&2
    return 1
  fi
  cat "$log"
  python3 - "$label" "$budget" "$byte_budget" "$ENGINE_MAX_BUDGET_NS" "$ENGINE_MAX_BUDGET_BYTES" "$log" "$artifact/$label.json" <<'PY'
import json, re, sys
label, budget, byte_budget, max_budget, max_byte_budget, log, output = sys.argv[1], *map(float, sys.argv[2:6]), sys.argv[6], sys.argv[7]
rows = []
for line in open(log, encoding="utf-8"):
    m = re.search(r"^(Benchmark\S+)\s+\d+\s+([0-9.]+)\s+ns/op(?:\s+([0-9.]+)\s+B/op\s+([0-9.]+)\s+allocs/op)?", line)
    if m:
        rows.append({"name": m.group(1), "ns_per_op": float(m.group(2)),
                     "bytes_per_op": float(m.group(3) or 0), "allocs_per_op": float(m.group(4) or 0)})
if not rows:
    raise SystemExit(f"{label}: no numeric Benchmark output; add a stable benchmark")
if label == "engine" and not any(re.search(r"(?:max|maximum|18x18)", row["name"], re.I) for row in rows):
    raise SystemExit("engine: missing named maximum-board scenario benchmark")
for row in rows:
    limit_ns, limit_bytes = budget, byte_budget
    if label == "engine" and re.search(r"(?:max|maximum|18x18)", row["name"], re.I):
        limit_ns, limit_bytes = max_budget, max_byte_budget
    if row["ns_per_op"] > limit_ns:
        raise SystemExit(f"{label}: {row['name']} ns/op {row['ns_per_op']:g} exceeds budget {limit_ns:g}")
    if limit_bytes > 0 and row["bytes_per_op"] > limit_bytes:
        raise SystemExit(f"{label}: {row['name']} B/op {row['bytes_per_op']:g} exceeds budget {limit_bytes:g}")
worst = max(rows, key=lambda row: row["ns_per_op"])
worst_bytes = max(rows, key=lambda row: row["bytes_per_op"])
report = {"schema": 2, "area": label, "budget_ns_per_op": budget, "budget_bytes_per_op": byte_budget,
          "maximum_budget_ns_per_op": max_budget, "maximum_budget_bytes_per_op": max_byte_budget,
          "benchmarks": rows, "worst_ns_per_op": worst["ns_per_op"], "worst_bytes_per_op": worst_bytes["bytes_per_op"]}
json.dump(report, open(output, "w", encoding="utf-8"), indent=2, sort_keys=True)
open(output, "a", encoding="utf-8").write("\n")
PY
}

run_package engine ./internal/engine "$ENGINE_BUDGET_NS" "$ENGINE_BUDGET_BYTES"
run_package match ./internal/match "$MATCH_BUDGET_NS" "$MATCH_BUDGET_BYTES"
run_package store ./internal/store "$STORE_BUDGET_NS" "$STORE_BUDGET_BYTES"
echo "benchmark budgets passed (artifacts: $artifact)"
