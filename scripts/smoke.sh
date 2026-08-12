#!/bin/sh
# Build and exercise a disposable server/database without leaving release artifacts.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
: "${GOCMD:=go}"
: "${CURL:=curl}"
: "${SMOKE_ARTIFACT_DIR:=$ROOT/dist/smoke}"
: "${SMOKE_KEEP_TEMP:=0}"
command -v "$GOCMD" >/dev/null 2>&1 || { echo "go is required" >&2; exit 2; }
command -v "$CURL" >/dev/null 2>&1 || { echo "curl is required" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/worms-smoke.XXXXXX")
server_pid=
web_dir="$ROOT/cmd/worms-server/web"
mkdir -p "$tmp/original-web"
cp -R "$web_dir/." "$tmp/original-web/" 2>/dev/null || true
cleanup() {
  status=$?
  if [ -n "${server_pid:-}" ]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  if [ "$status" -ne 0 ] || [ "$SMOKE_KEEP_TEMP" = 1 ]; then
    mkdir -p "$SMOKE_ARTIFACT_DIR"
    cp -R "$tmp/." "$SMOKE_ARTIFACT_DIR/" 2>/dev/null || true
    echo "smoke diagnostics: $SMOKE_ARTIFACT_DIR" >&2
    if [ -f "$tmp/server.log" ]; then
      echo "smoke server log:" >&2
      cat "$tmp/server.log" >&2
    fi
  fi
  rm -rf "$web_dir"
  mkdir -p "$web_dir"
  cp -R "$tmp/original-web/." "$web_dir/" 2>/dev/null || true
  rm -rf "$tmp"
  exit "$status"
}
trap cleanup EXIT INT TERM

cd "$ROOT"
make build-wasm
"$GOCMD" build -trimpath -o "$tmp/worms-server" ./cmd/worms-server
"$GOCMD" build -trimpath -o "$tmp/wormsctl" ./cmd/wormsctl
if [ -n "${PORT:-}" ]; then
  port=$PORT
else
  port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
fi
base="http://127.0.0.1:$port"
"$tmp/worms-server" -addr "127.0.0.1:$port" -db "$tmp/smoke.db" >"$tmp/server.log" 2>&1 &
server_pid=$!

ready=0
for _ in $(seq 1 60); do
  if "$CURL" -fsS "$base/api/v1/health" >"$tmp/health.json" 2>/dev/null; then ready=1; break; fi
  sleep 0.1
done
[ "$ready" -eq 1 ] || { cat "$tmp/server.log" >&2; echo "server did not become ready" >&2; exit 1; }

check_asset() {
  path=$1
  expected_type=$2
  name=${path:-index.html}
  "$CURL" -fsS -D "$tmp/headers-$name.txt" "$base/${path}" -o "$tmp/$name"
  test -s "$tmp/$name"
  python3 - "$tmp/headers-$name.txt" "$expected_type" <<'PY'
import sys
headers = open(sys.argv[1], encoding="iso-8859-1").read().lower()
if sys.argv[2].lower() not in headers:
    raise SystemExit(f"missing content type {sys.argv[2]}: {headers!r}")
PY
}
check_asset "" "text/html"
wasm_asset=$(python3 - "$tmp/index.html" <<'PY'
import pathlib, re, sys
html = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
paths = re.findall(r'["\']([^"\']+\.(?:wasm|js))["\']', html)
for path in paths:
    name = pathlib.Path(path).name
    if name.startswith("main-") and name.endswith(".wasm"):
        print(name)
        break
else:
    raise SystemExit("index.html does not reference a content-hashed WASM asset")
PY
)
js_asset=$(python3 - "$tmp/index.html" <<'PY'
import pathlib, re, sys
html = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
for path in re.findall(r'["\']([^"\']+\.js)["\']', html):
    name = pathlib.Path(path).name
    if name.startswith("wasm-") and name.endswith(".js"):
        print(name)
        break
else:
    raise SystemExit("index.html does not reference a content-hashed loader")
PY
)
check_asset "$wasm_asset" "application/wasm"
check_asset "$js_asset" "javascript"

check_json() {
  file=$1
  shift
  python3 - "$file" "$@" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as fh:
    value = json.load(fh)
if value.get("version") != "v1":
    raise SystemExit(f"missing v1 response: {value!r}")
for key in sys.argv[2:]:
    if key not in value:
        raise SystemExit(f"missing {key}: {value!r}")
PY
}

check_json "$tmp/health.json" status
python3 - "$tmp/health.json" <<'PY'
import json, sys
if json.load(open(sys.argv[1], encoding="utf-8")).get("status") != "ok":
    raise SystemExit("health status is not ok")
PY
"$CURL" -fsS "$base/api/v1/demo" >"$tmp/demo.json"
check_json "$tmp/demo.json" demo
"$CURL" -fsS "$base/api/v1/build" >"$tmp/build.json"
check_json "$tmp/build.json" service_version schema_version

cat >"$tmp/create.json" <<'JSON'
{"version":"v1","id":"smoke-game","status":"running","participants":[{"id":"w1","name":"Smoke Worm","kind":"human"}]}
JSON
"$CURL" -fsS -X POST "$base/api/v1/games" -H 'content-type: application/json' --data-binary @"$tmp/create.json" >"$tmp/game.json"
check_json "$tmp/game.json" game
python3 - "$tmp/game.json" <<'PY'
import json, sys
x = json.load(open(sys.argv[1], encoding="utf-8"))
g = x["game"]
if g.get("id") != "smoke-game" or g.get("cursor") != 0:
    raise SystemExit(f"unexpected game: {g!r}")
PY
"$CURL" -fsS "$base/api/v1/games/smoke-game/resume" >"$tmp/resume.json"
check_json "$tmp/resume.json" game state

cat >"$tmp/act.json" <<'JSON'
{"version":"v1","cursor":0,"event_hash":"","worm_id":"w1","direction":0}
JSON
"$CURL" -fsS -X POST "$base/api/v1/games/smoke-game/act" -H 'content-type: application/json' --data-binary @"$tmp/act.json" >"$tmp/act-response.json"
check_json "$tmp/act-response.json" game events state
python3 - "$tmp/act-response.json" <<'PY'
import json, sys
x = json.load(open(sys.argv[1], encoding="utf-8"))
if x["game"].get("cursor") != 1 or not x["events"]:
    raise SystemExit(f"move was not persisted: {x!r}")
PY

"$tmp/wormsctl" --db "$tmp/smoke.db" game list --json >"$tmp/cli-games.json"
"$tmp/wormsctl" --db "$tmp/smoke.db" game verify smoke-game --json >"$tmp/cli-verify.json"
python3 - "$tmp/cli-games.json" "$tmp/cli-verify.json" <<'PY'
import json, sys
listed = json.load(open(sys.argv[1], encoding="utf-8"))
verified = json.load(open(sys.argv[2], encoding="utf-8"))
if listed.get("version") != 1 or not any(g.get("id") == "smoke-game" for g in listed.get("data", [])):
    raise SystemExit(f"CLI game list did not expose smoke game: {listed!r}")
if verified.get("version") != 1 or not verified.get("data", {}).get("valid"):
    raise SystemExit(f"CLI verification failed: {verified!r}")
PY

echo "smoke: WASM assets, health, metadata, SQLite create/resume/move, and CLI verification passed"
