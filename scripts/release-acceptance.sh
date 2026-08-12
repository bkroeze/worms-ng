#!/bin/sh
# Run the operator-facing first-release acceptance path against packaged binaries.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
: "${CURL:=curl}"
: "${ARTIFACT_DIR:=$ROOT/dist/release-acceptance}"
: "${VERSION:=acceptance}"
command -v "$CURL" >/dev/null 2>&1 || { echo "curl is required" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }
mkdir -p "$ARTIFACT_DIR"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/worms-release-acceptance.XXXXXX")
server_pid=
cleanup() {
  status=$?
  if [ -n "${server_pid:-}" ]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  if [ "$status" -ne 0 ]; then cp -R "$tmp/." "$ARTIFACT_DIR/" 2>/dev/null || true; fi
  rm -rf "$tmp"
  exit "$status"
}
trap cleanup EXIT INT TERM

cd "$ROOT"
make clean build release VERSION="$VERSION" DIST_DIR="$tmp/dist" BIN_DIR="$tmp/bin"
archive=$(printf '%s\n' "$tmp/dist"/*.tar.gz)
tar -tzf "$archive" | grep -qx worms-server
tar -tzf "$archive" | grep -qx wormsctl
tar -tzf "$archive" | grep -qx worms-agent
cp "$tmp/dist/build-metadata.json" "$ARTIFACT_DIR/"

port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
base="http://127.0.0.1:$port"
"$tmp/bin/worms-server" -addr "127.0.0.1:$port" -db "$tmp/release.db" >"$tmp/server.log" 2>&1 &
server_pid=$!
for _ in $(seq 1 100); do "$CURL" -fsS "$base/api/v1/health" >"$tmp/health.json" 2>/dev/null && break; sleep 0.1; done
(
  cd "$tmp/dist"
  sha256sum -c SHA256SUMS
) >"$ARTIFACT_DIR/checksums.txt"

cat >"$tmp/game.json" <<'JSON'
{"version":"v1","id":"release-game","status":"running","participants":[{"id":"w1","name":"Release Worm","kind":"human"}]}
JSON
"$CURL" -fsS -X POST "$base/api/v1/games" -H 'content-type: application/json' --data-binary @"$tmp/game.json" >"$ARTIFACT_DIR/game-create.json"
"$CURL" -fsS "$base/api/v1/games/release-game/resume" >"$ARTIFACT_DIR/game-resume.json"
cat >"$tmp/act.json" <<'JSON'
{"version":"v1","cursor":0,"event_hash":"","worm_id":"w1","direction":0}
JSON
"$CURL" -fsS -X POST "$base/api/v1/games/release-game/act" -H 'content-type: application/json' --data-binary @"$tmp/act.json" >"$ARTIFACT_DIR/game-act.json"
"$tmp/bin/wormsctl" --api "$base" game verify release-game --json >"$ARTIFACT_DIR/game-verify.json"
"$tmp/bin/wormsctl" --api "$base" game replay release-game --json >"$ARTIFACT_DIR/game-replay.json"

cat >"$tmp/brain.json" <<'JSON'
{"version":"v1","id":"release-brain","name":"Release brain","type":"external"}
JSON
"$CURL" -fsS -X POST "$base/api/v1/brains" -H 'content-type: application/json' --data-binary @"$tmp/brain.json" >"$tmp/brain-create.json"
cat >"$tmp/version.json" <<'JSON'
{"version":"v1","number":1,"rules":{"version":1,"data":[]},"lineage":{"version":1,"data":{}},"provenance":{"version":1,"data":{"source":"release-acceptance","learned_event":1}},"payload":{"version":1,"data":{}}}
JSON
"$CURL" -fsS -X POST "$base/api/v1/brains/release-brain/versions" -H 'content-type: application/json' --data-binary @"$tmp/version.json" >"$tmp/brain-version.json"
"$tmp/bin/wormsctl" --api "$base" brain show release-brain --version 1 --json >"$ARTIFACT_DIR/brain-show.json"
"$tmp/bin/wormsctl" --db "$tmp/release.db" diagnostic export release-brain release-game --redact >"$ARTIFACT_DIR/diagnostic.json"

cat >"$tmp/tournament.json" <<'JSON'
{"version":"v1","id":"release-tournament","name":"Release tournament","status":"completed","rules_payload":{"version":1,"data":{}}}
JSON
"$CURL" -fsS -X POST "$base/api/v1/tournaments" -H 'content-type: application/json' --data-binary @"$tmp/tournament.json" >"$ARTIFACT_DIR/tournament.json"
cat >"$tmp/match.json" <<'JSON'
{"version":"v1","id":"release-match","game_id":"release-game","status":"completed","round":1,"payload":{"version":1,"data":{}}}
JSON
"$CURL" -fsS -X POST "$base/api/v1/tournaments/release-tournament/matches" -H 'content-type: application/json' --data-binary @"$tmp/match.json" >"$ARTIFACT_DIR/tournament-match.json"
"$CURL" -fsS "$base/api/v1/tournaments/release-tournament/matches" >"$ARTIFACT_DIR/tournament-list.json"

kill "$server_pid"; wait "$server_pid" 2>/dev/null || true; server_pid=
cp "$tmp/release.db" "$tmp/backup.db"
DB="$tmp/backup.db" make db-check >"$ARTIFACT_DIR/backup-check.txt"
"$tmp/bin/wormsctl" diagnostic import "$ARTIFACT_DIR/diagnostic.json" --db "$tmp/restored.db" --authorize-import >"$ARTIFACT_DIR/diagnostic-restore.txt"
DB="$tmp/restored.db" make db-check >"$ARTIFACT_DIR/restore-check.txt"

BROWSER_ARTIFACT_DIR="$ARTIFACT_DIR/browser-1280x720" BROWSER_VIEWPORT=1280x720 make browser-smoke
BROWSER_ARTIFACT_DIR="$ARTIFACT_DIR/browser-320x480" BROWSER_VIEWPORT=320x480 make browser-smoke
printf 'release acceptance passed; evidence: %s\n' "$ARTIFACT_DIR"
