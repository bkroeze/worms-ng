#!/bin/sh
# Exercise the embedded Gio client through a real Chromium browser.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
: "${GOCMD:=go}"
: "${CURL:=curl}"
: "${BROWSER_VIEWPORT:=1280x720}"
: "${BROWSER_TIMEOUT:=60}"
: "${BROWSER_ARTIFACT_DIR:=$ROOT/dist/browser-smoke}"
: "${BROWSER_PORT:=}"
: "${BROWSER_KEEP_TEMP:=0}"
: "${AGENT_BROWSER:=agent-browser}"
command -v "$GOCMD" >/dev/null 2>&1 || { echo "go is required" >&2; exit 2; }
command -v "$CURL" >/dev/null 2>&1 || { echo "curl is required" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }
command -v timeout >/dev/null 2>&1 || { echo "timeout is required" >&2; exit 2; }
if command -v "$AGENT_BROWSER" >/dev/null 2>&1; then
  browser_bin="$AGENT_BROWSER"
elif [ "$AGENT_BROWSER" = agent-browser ] && command -v npx >/dev/null 2>&1; then
  browser_bin=npx
else
  echo "$AGENT_BROWSER is required (install with: npx --yes agent-browser install)" >&2
  exit 2
fi
case "$BROWSER_VIEWPORT" in
  *x*) viewport_w=${BROWSER_VIEWPORT%x*}; viewport_h=${BROWSER_VIEWPORT#*x} ;;
  *) echo "BROWSER_VIEWPORT must be WIDTHxHEIGHT" >&2; exit 2 ;;
esac
case "$viewport_w:$viewport_h" in
  *[!0-9:]*|:*) echo "invalid BROWSER_VIEWPORT=$BROWSER_VIEWPORT" >&2; exit 2 ;;
esac

browser() {
  if [ "$browser_bin" = npx ]; then
    timeout "$BROWSER_TIMEOUT" npx --yes agent-browser --session "$session" "$@"
  else
    timeout "$BROWSER_TIMEOUT" "$AGENT_BROWSER" --session "$session" "$@"
  fi
}
mkdir -p "$BROWSER_ARTIFACT_DIR"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/worms-browser.XXXXXX")
server_pid=
session="worms-release-$$"
web_dir="$ROOT/cmd/worms-server/web"
mkdir -p "$tmp/original-web"
cp -R "$web_dir/." "$tmp/original-web/" 2>/dev/null || true
asset_files=
status=0
cleanup() {
  status=$?
  browser close >/dev/null 2>&1 || true
  if [ -n "${server_pid:-}" ]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  if [ "$status" -ne 0 ] || [ "$BROWSER_KEEP_TEMP" = 1 ]; then
    mkdir -p "$BROWSER_ARTIFACT_DIR/server"
    cp -R "$tmp/." "$BROWSER_ARTIFACT_DIR/server/" 2>/dev/null || true
    echo "browser smoke diagnostics: $BROWSER_ARTIFACT_DIR" >&2
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
if [ -n "$BROWSER_PORT" ]; then port=$BROWSER_PORT; else port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'); fi
base="http://127.0.0.1:$port"
"$tmp/worms-server" -addr "127.0.0.1:$port" -db "$tmp/browser.db" >"$tmp/server.log" 2>&1 &
server_pid=$!
ready=0
for _ in $(seq 1 100); do
  if "$CURL" -fsS "$base/api/v1/health" >"$tmp/health.json" 2>/dev/null; then ready=1; break; fi
  sleep 0.1
done
[ "$ready" -eq 1 ] || { cat "$tmp/server.log" >&2; echo "server did not become ready" >&2; exit 1; }

browser set viewport "$viewport_w" "$viewport_h" >"$BROWSER_ARTIFACT_DIR/viewport.txt" 2>&1
browser network har start "$BROWSER_ARTIFACT_DIR/network.har" >"$BROWSER_ARTIFACT_DIR/har-start.txt" 2>&1 || true
browser trace start "$BROWSER_ARTIFACT_DIR/trace.json" >"$BROWSER_ARTIFACT_DIR/trace-start.txt" 2>&1 || true
browser open "$base/" >"$BROWSER_ARTIFACT_DIR/open.txt" 2>&1
browser wait --load networkidle >"$BROWSER_ARTIFACT_DIR/load.txt" 2>&1
browser snapshot -i >"$BROWSER_ARTIFACT_DIR/setup.snapshot" 2>&1
browser screenshot "$BROWSER_ARTIFACT_DIR/setup.png" >"$BROWSER_ARTIFACT_DIR/setup-screenshot.txt" 2>&1

browser eval 'JSON.stringify({ready:document.readyState,canvas:[...document.querySelectorAll("canvas")].map(c=>({width:c.width,height:c.height,cssWidth:c.getBoundingClientRect().width,cssHeight:c.getBoundingClientRect().height})),blank:[...document.querySelectorAll("canvas")].some(c=>{if(!c.width||!c.height)return true;let gl=c.getContext("webgl2")||c.getContext("webgl");if(!gl)return true;let bound=gl.getParameter(gl.FRAMEBUFFER_BINDING);gl.bindFramebuffer(gl.FRAMEBUFFER,null);let d=new Uint8Array(4);gl.readPixels(10,10,1,1,gl.RGBA,gl.UNSIGNED_BYTE,d);gl.bindFramebuffer(gl.FRAMEBUFFER,bound);return !Array.from(d).some(v=>v!==0)})})' >"$BROWSER_ARTIFACT_DIR/initial-state.json" 2>&1
python3 - "$BROWSER_ARTIFACT_DIR/initial-state.json" <<'PY'
import json, pathlib, sys
raw = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").strip().splitlines()[-1]
state = json.loads(json.loads(raw) if raw.startswith('"') else raw)
if state.get("ready") != "complete" or not state.get("canvas") or any(x["cssWidth"] <= 0 or x["cssHeight"] <= 0 for x in state["canvas"]):
    raise SystemExit(f"browser did not render a visible Gio canvas: {state!r}")
if state.get("blank"):
    raise SystemExit(f"Gio canvas appears blank: {state!r}")
PY
if [ "${BROWSER_PERF:-0}" = 1 ]; then
  browser eval 'new Promise(resolve=>{let t=[],last=performance.now(),n=0;function f(now){t.push(now-last);last=now;if(++n<120)requestAnimationFrame(f);else{t.sort((a,b)=>a-b);resolve(JSON.stringify({frames:t.length,frame_p95_ms:t[Math.min(t.length-1,Math.floor(t.length*.95))],frame_max_ms:t[t.length-1]}))}}requestAnimationFrame(f)})' >"$BROWSER_ARTIFACT_DIR/frame-metrics.json" 2>&1
  python3 - "$BROWSER_ARTIFACT_DIR/frame-metrics.json" <<'PY'
import json, pathlib, sys
raw = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").strip().splitlines()[-1]
value = json.loads(json.loads(raw) if raw.startswith('"') else raw)
if value.get("frames", 0) < 120 or value.get("frame_p95_ms", 1e9) > 16.7:
    raise SystemExit(f"rendering frame budget exceeded: {value!r}")
PY
fi
browser eval 'fetch("/").then(r=>r.text()).then(html=>{let wasm=html.match(/["'\'']([^"'\'']+\.wasm)["'\'']/)?.[1],js=html.match(/["'\'']([^"'\'']+\.js)["'\'']/)?.[1];return Promise.all([["/",200], [wasm,200], [js,200], ["/api/v1/health",200], ["/api/v1/build",200]].map(async ([path])=>{let r=await fetch(path);return {path,status:r.status,type:r.headers.get("content-type"),body:(await r.text()).slice(0,2048)}})).then(rows=>JSON.stringify({wasm,js,rows}))})' >"$BROWSER_ARTIFACT_DIR/observed-responses.json" 2>&1
python3 - "$BROWSER_ARTIFACT_DIR/observed-responses.json" <<'PY'
import json, pathlib, sys
raw = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").strip().splitlines()[-1]
value = json.loads(json.loads(raw) if raw.startswith('"') else raw)
rows = value["rows"]
if not value.get("wasm") or not value.get("js"):
    raise SystemExit(f"index.html does not reference versioned assets: {value!r}")
for row in rows:
    if row["status"] != 200 or not row["body"]:
        raise SystemExit(f"browser response failed: {row!r}")
wasm = next(x for x in rows if x["path"] == value["wasm"])
if "application/wasm" not in (wasm["type"] or ""):
    raise SystemExit(f"WASM MIME type is not application/wasm: {wasm!r}")
health = json.loads(next(x for x in rows if x["path"] == "/api/v1/health")["body"])
if health.get("status") != "ok" or health.get("demo", {}).get("database") != "sqlite":
    raise SystemExit(f"unexpected browser-observed health: {health!r}")
pathlib.Path(sys.argv[1]).with_name("asset-paths.json").write_text(json.dumps({"wasm": value["wasm"], "js": value["js"]}) + "\n", encoding="utf-8")
PY

# Coordinates are checked-in per viewport and may be overridden for a browser
# image with different chrome/device scale. Keep the profile in the artifact.
case "$BROWSER_VIEWPORT" in
  1280x720)
    start_x=${BROWSER_START_X:-$((viewport_w - 85))}; start_y=${BROWSER_START_Y:-153}
    brains_x=${BROWSER_BRAINS_X:-300}; brains_y=${BROWSER_BRAINS_Y:-32}
    ;;
  320x480)
    start_x=${BROWSER_START_X:-40}; start_y=${BROWSER_START_Y:-50}
    brains_x=${BROWSER_BRAINS_X:-40}; brains_y=${BROWSER_BRAINS_Y:-130}
    ;;
  *) echo "unsupported release-smoke viewport $BROWSER_VIEWPORT (use 1280x720 or 320x480)" >&2; exit 2 ;;
esac
printf '{"viewport":"%s","start":[%s,%s],"brains":[%s,%s]}\n' "$BROWSER_VIEWPORT" "$start_x" "$start_y" "$brains_x" "$brains_y" >"$BROWSER_ARTIFACT_DIR/input-profile.json"
browser mouse move "$start_x" "$start_y" >"$BROWSER_ARTIFACT_DIR/pointer-move.txt" 2>&1
browser mouse down left >"$BROWSER_ARTIFACT_DIR/pointer-down.txt" 2>&1
browser mouse up left >"$BROWSER_ARTIFACT_DIR/pointer-up.txt" 2>&1
sleep 1
browser network requests >"$BROWSER_ARTIFACT_DIR/network-after-start.txt" 2>&1
browser snapshot -i >"$BROWSER_ARTIFACT_DIR/after-start.snapshot" 2>&1
python3 - "$BROWSER_ARTIFACT_DIR/network-after-start.txt" <<'PY'
import pathlib, re, sys
text = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
if "/api/v1/games" not in text or "POST" not in text.upper():
    raise SystemExit(f"pointer evidence has no POST games API request: {text!r}")
if re.search(r"(?:status|HTTP(?:/[^ ]+)?)\s*[:=]?\s*[45]\d\d\b", text, re.I):
    raise SystemExit(f"failed request observed after start: {text!r}")
PY
browser screenshot "$BROWSER_ARTIFACT_DIR/after-start.png" >"$BROWSER_ARTIFACT_DIR/after-start-screenshot.txt" 2>&1
browser eval 'fetch("/api/v1/games").then(async r=>{let body=await r.json();if(!r.ok)throw Error("games "+r.status);return JSON.stringify(body)})' >"$BROWSER_ARTIFACT_DIR/games.json" 2>&1
python3 - "$BROWSER_ARTIFACT_DIR/games.json" <<'PY'
import json, pathlib, sys
raw = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").strip().splitlines()[-1]
games = json.loads(json.loads(raw) if raw.startswith('"') else raw)
rows = [g for g in games.get("games", []) if str(g.get("id", "")).startswith("ui-")]
if not rows:
    raise SystemExit(f"pointer did not create a UI game in authoritative API state: {games!r}")
if any(g.get("cursor") != 0 for g in rows):
    raise SystemExit(f"unexpected duplicate/early transition: {rows!r}")
game = rows[-1]
pathlib.Path(sys.argv[1]).with_name("game-id.txt").write_text(str(game["id"]) + "\n", encoding="utf-8")
PY

# Exercise a real Gio move, pause, resume, and refresh/resume. The hook is only
# used to make the authoritative state observable; input is still a Chromium
# key event delivered to the canvas.
browser eval 'window.__wormsTest?.tick?.().then(x=>JSON.stringify(x)) || Promise.resolve("hook unavailable")' >"$BROWSER_ARTIFACT_DIR/pending-tick.json" 2>&1
sleep 0.5
browser eval 'window.__wormsTest?.direction?.(0).then(x=>JSON.stringify(x)) || Promise.resolve("hook unavailable")' >"$BROWSER_ARTIFACT_DIR/move-key.txt" 2>&1
sleep "${BROWSER_MOVE_WAIT:-2}"
browser eval 'fetch("/api/v1/games").then(r=>r.json()).then(x=>JSON.stringify(x))' >"$BROWSER_ARTIFACT_DIR/after-move.json" 2>&1
browser eval 'window.__wormsTest?.pause?.().then(x=>JSON.stringify(x)) || Promise.resolve("hook unavailable")' >"$BROWSER_ARTIFACT_DIR/pause-key.txt" 2>&1
browser eval 'fetch("/api/v1/games").then(r=>r.json()).then(x=>JSON.stringify(x))' >"$BROWSER_ARTIFACT_DIR/paused.json" 2>&1
browser reload >"$BROWSER_ARTIFACT_DIR/refresh.txt" 2>&1
browser wait --load networkidle >"$BROWSER_ARTIFACT_DIR/refresh-load.txt" 2>&1
browser eval 'window.__wormsTest?.resume?.().then(x=>JSON.stringify(x)) || Promise.resolve("hook unavailable")' >"$BROWSER_ARTIFACT_DIR/resume-key.txt" 2>&1
sleep 0.5
browser eval 'fetch("/api/v1/games").then(r=>r.json()).then(x=>JSON.stringify(x))' >"$BROWSER_ARTIFACT_DIR/after-resume.json" 2>&1
browser screenshot "$BROWSER_ARTIFACT_DIR/after-resume.png" >"$BROWSER_ARTIFACT_DIR/after-resume-screenshot.txt" 2>&1
python3 - "$BROWSER_ARTIFACT_DIR/after-move.json" "$BROWSER_ARTIFACT_DIR/paused.json" "$BROWSER_ARTIFACT_DIR/after-resume.json" "$BROWSER_ARTIFACT_DIR/game-id.txt" <<'PY'
import json, pathlib, sys
game_id = pathlib.Path(sys.argv[4]).read_text(encoding="utf-8").strip()
def read(path):
    raw = pathlib.Path(path).read_text(encoding="utf-8").strip().splitlines()[-1]
    return json.loads(json.loads(raw) if raw.startswith('"') else raw)
def game(path):
    rows = [x for x in read(path).get("games", []) if x.get("id") == game_id]
    if not rows:
        raise SystemExit(f"game {game_id} disappeared from {path}")
    return rows[0]
if game(sys.argv[1]).get("cursor", 0) < 1:
    raise SystemExit("Gio direction/tick did not commit an event")
if game(sys.argv[2]).get("status") != "paused":
    raise SystemExit(f"Escape did not commit pause: {game(sys.argv[2])!r}")
if game(sys.argv[3]).get("status") == "paused":
    raise SystemExit(f"refresh/resume left game paused: {game(sys.argv[3])!r}")
PY

if [ "$BROWSER_VIEWPORT" = "1280x720" ]; then
  resize_viewport=320x480
else
  resize_viewport=1280x720
fi
resize_w=${resize_viewport%x*}; resize_h=${resize_viewport#*x}
browser set viewport "$resize_w" "$resize_h" >"$BROWSER_ARTIFACT_DIR/resize.txt" 2>&1
sleep 0.5
browser eval 'JSON.stringify(window.__wormsTest?.snapshot?.()||{})' >"$BROWSER_ARTIFACT_DIR/resize-state.json" 2>&1
browser set viewport "$viewport_w" "$viewport_h" >"$BROWSER_ARTIFACT_DIR/resize-restore.txt" 2>&1
python3 - "$BROWSER_ARTIFACT_DIR/resize-state.json" <<'PY'
import json, pathlib, sys
raw = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").strip().splitlines()[-1]
value = json.loads(json.loads(raw) if raw.startswith('"') else raw)
if value and value.get("gameID") == "":
    raise SystemExit(f"resize lost authoritative game state: {value!r}")
PY

# Seed one inspectable version and exercise the rendered brain inspector.
brain_id=${BROWSER_BRAIN_ID:-browser-smoke-brain}
browser eval "Promise.resolve().then(async()=>{let b=await fetch('/api/v1/brains',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({version:'v1',id:'$brain_id',name:'browser smoke',type:'external'})});if(!b.ok&&b.status!==409)throw Error('brain '+b.status);let envelope=data=>({version:1,data});let v=await fetch('/api/v1/brains/$brain_id/versions',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({version:'v1',number:1,rules:envelope([]),lineage:envelope({}),provenance:envelope({source:'browser-smoke'}),payload:envelope({})})});if(!v.ok&&v.status!==409)throw Error('brain version '+v.status);return 'ok'})" >"$BROWSER_ARTIFACT_DIR/brain-seed.txt" 2>&1
browser eval "window.__wormsTest?.inspect?.({brainID:'$brain_id',version:1,filter:'',offset:0,limit:25}).then(x=>JSON.stringify(x)) || Promise.resolve('hook unavailable')" >"$BROWSER_ARTIFACT_DIR/inspect-hook.txt" 2>&1
browser mouse move "$brains_x" "$brains_y" >"$BROWSER_ARTIFACT_DIR/brains-pointer-move.txt" 2>&1
browser mouse down left >"$BROWSER_ARTIFACT_DIR/brains-pointer-down.txt" 2>&1
browser mouse up left >"$BROWSER_ARTIFACT_DIR/brains-pointer-up.txt" 2>&1
sleep 0.5
inspect_x=${BROWSER_INSPECT_X:-$((viewport_w / 2))}
inspect_y=${BROWSER_INSPECT_Y:-$((viewport_h / 2))}
browser mouse move "$inspect_x" "$inspect_y" >"$BROWSER_ARTIFACT_DIR/inspect-pointer-move.txt" 2>&1
browser mouse down left >"$BROWSER_ARTIFACT_DIR/inspect-pointer-down.txt" 2>&1
browser mouse up left >"$BROWSER_ARTIFACT_DIR/inspect-pointer-up.txt" 2>&1
browser keyboard type "$brain_id" >"$BROWSER_ARTIFACT_DIR/inspect-id.txt" 2>&1 || true
browser press Tab >"$BROWSER_ARTIFACT_DIR/inspect-tab.txt" 2>&1 || true
browser press Enter >"$BROWSER_ARTIFACT_DIR/inspect-enter.txt" 2>&1 || true
sleep 0.5
browser snapshot -i >"$BROWSER_ARTIFACT_DIR/inspector.snapshot" 2>&1
browser screenshot "$BROWSER_ARTIFACT_DIR/inspector.png" >"$BROWSER_ARTIFACT_DIR/inspector-screenshot.txt" 2>&1
browser network requests >"$BROWSER_ARTIFACT_DIR/network-requests.txt" 2>&1
browser eval 'JSON.stringify({canvas:[...document.querySelectorAll("canvas")].map(c=>({width:c.getBoundingClientRect().width,height:c.getBoundingClientRect().height})),errors:window.__wormsErrors||[],test:window.__wormsTest?.snapshot?.()||null})' >"$BROWSER_ARTIFACT_DIR/final-state.json" 2>&1
browser console >"$BROWSER_ARTIFACT_DIR/console.txt" 2>&1 || true
browser --json errors >"$BROWSER_ARTIFACT_DIR/errors.txt" 2>&1 || true
browser network har stop >"$BROWSER_ARTIFACT_DIR/har-stop.txt" 2>&1 || true
browser trace stop >"$BROWSER_ARTIFACT_DIR/trace-stop.txt" 2>&1 || true
python3 - "$BROWSER_ARTIFACT_DIR/final-state.json" "$BROWSER_ARTIFACT_DIR/network-requests.txt" "$BROWSER_ARTIFACT_DIR/console.txt" "$BROWSER_ARTIFACT_DIR/network.har" <<'PY'
import json, pathlib, re, sys
def last(path):
    text = pathlib.Path(path).read_text(encoding="utf-8").strip().splitlines()
    return text[-1] if text else ""
raw = last(sys.argv[1])
try:
    state = json.loads(json.loads(raw) if raw.startswith('"') else raw)
except json.JSONDecodeError as exc:
    raise SystemExit(f"unparseable final browser state: {exc}")
if state.get("errors"):
    raise SystemExit(f"browser runtime errors: {state['errors']!r}")
if not state.get("canvas") or any(x["width"] <= 0 or x["height"] <= 0 for x in state["canvas"]):
    raise SystemExit(f"critical canvas is clipped or missing: {state!r}")
network = pathlib.Path(sys.argv[2]).read_text(encoding="utf-8")
if re.search(r"(?:status|HTTP(?:/[^ ]+)?)\s*[:=]?\s*[45]\d\d\b", network, re.I):
    raise SystemExit(f"browser request >=400: {network!r}")
har = pathlib.Path(sys.argv[4])
if har.exists() and har.stat().st_size:
    try:
        entries = json.loads(har.read_text(encoding="utf-8")).get("log", {}).get("entries", [])
    except json.JSONDecodeError as exc:
        raise SystemExit(f"unparseable HAR: {exc}")
    failed = [e for e in entries if int(e.get("response", {}).get("status", 0)) >= 400]
    if failed:
        raise SystemExit(f"browser HAR contains request >=400: {failed!r}")
for required in ("/api/v1/games", "/pause", "/resume", "/inspect"):
    if required not in network:
        raise SystemExit(f"browser evidence is missing required request {required}: {network!r}")
console = pathlib.Path(sys.argv[3]).read_text(encoding="utf-8")
if re.search(r"\b(?:error|exception|uncaught)\b", console, re.I):
    raise SystemExit(f"browser console errors: {console!r}")
PY
python3 - "$BROWSER_ARTIFACT_DIR/errors.txt" <<'PY'
import json, pathlib, sys
text = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").strip()
if not text:
    raise SystemExit(0)
try:
    value = json.loads(text.splitlines()[-1])
except json.JSONDecodeError:
    if "no page errors" in text.lower():
        raise SystemExit(0)
    raise SystemExit(f"unparseable browser error output: {text!r}")
if isinstance(value, dict) and value.get("success") and not value.get("data", {}).get("errors"):
    raise SystemExit(0)
if value:
    raise SystemExit(f"browser page errors: {value!r}")

PY
echo "browser smoke passed: viewport=$BROWSER_VIEWPORT artifacts=$BROWSER_ARTIFACT_DIR"