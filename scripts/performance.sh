#!/bin/sh
# Build measured release assets and run native budget checks.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
: "${WASM_ARTIFACT_DIR:=${PERF_ARTIFACT_DIR:-$ROOT/dist/performance}}"
: "${WASM_BUDGET_BYTES:=33554432}"
: "${WASM_JS_BUDGET_BYTES:=262144}"
: "${GOCMD:=go}"
command -v "$GOCMD" >/dev/null 2>&1 || { echo "go is required" >&2; exit 2; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 2; }
cd "$ROOT"
mkdir -p "$WASM_ARTIFACT_DIR"
make build-wasm
wasm=$(printf '%s\n' cmd/worms-server/web/main-*.wasm)
loader=$(printf '%s\n' cmd/worms-server/web/wasm-*.js)
[ -s "$wasm" ] || { echo "versioned main.wasm asset missing" >&2; exit 1; }
[ -s "$loader" ] || { echo "versioned wasm.js asset missing" >&2; exit 1; }
wasm_size=$(wc -c <"$wasm")
loader_size=$(wc -c <"$loader")
[ "$wasm_size" -le "$WASM_BUDGET_BYTES" ] || { echo "main.wasm is $wasm_size bytes (budget $WASM_BUDGET_BYTES)" >&2; exit 1; }
[ "$loader_size" -le "$WASM_JS_BUDGET_BYTES" ] || { echo "wasm.js is $loader_size bytes (budget $WASM_JS_BUDGET_BYTES)" >&2; exit 1; }
{
  printf '{\n  "schema": 2,\n  "scenario": "wasm-delivery",\n  "wasm_bytes": %s,\n  "wasm_budget_bytes": %s,\n  "loader_bytes": %s,\n  "loader_budget_bytes": %s,\n  "sha256": {\n' "$wasm_size" "$WASM_BUDGET_BYTES" "$loader_size" "$WASM_JS_BUDGET_BYTES"
  sha256sum "$wasm" "$loader" | awk '{printf "    \"%s\": \"%s\"%s\n", $2, $1, (NR == 2 ? "" : ",")}'
  printf '  }\n}\n'
} >"$WASM_ARTIFACT_DIR/assets.json"
PERF_ARTIFACT_DIR="$WASM_ARTIFACT_DIR/native" BENCHTIME="${BENCHTIME:-1s}" ENGINE_BUDGET_NS="${ENGINE_BUDGET_NS:-2000000}" ENGINE_BUDGET_BYTES="${ENGINE_BUDGET_BYTES:-65536}" ENGINE_MAX_BUDGET_NS="${ENGINE_MAX_BUDGET_NS:-20000000}" ENGINE_MAX_BUDGET_BYTES="${ENGINE_MAX_BUDGET_BYTES:-524288}" MATCH_BUDGET_NS="${MATCH_BUDGET_NS:-20000000}" MATCH_BUDGET_BYTES="${MATCH_BUDGET_BYTES:-0}" STORE_BUDGET_NS="${STORE_BUDGET_NS:-50000000}" STORE_BUDGET_BYTES="${STORE_BUDGET_BYTES:-0}" STORE_REPLAY_BUDGET_NS="${STORE_REPLAY_BUDGET_NS:-250000000}" make bench
if [ "${BROWSER_PERF:-0}" = 1 ]; then
  BROWSER_PERF=1 BROWSER_ARTIFACT_DIR="$WASM_ARTIFACT_DIR/browser" BROWSER_VIEWPORT="${BROWSER_VIEWPORT:-1280x720}" make browser-smoke
fi
printf 'performance budgets passed; assets report: %s\n' "$WASM_ARTIFACT_DIR/assets.json"
