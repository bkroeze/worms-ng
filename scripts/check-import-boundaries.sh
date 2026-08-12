#!/bin/sh
# Ensure deterministic/pure packages do not import UI, HTTP handlers, or SQLite.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
: "${GOCMD:=go}"
command -v "$GOCMD" >/dev/null 2>&1 || { echo "go is required" >&2; exit 2; }
cd "$ROOT"

# Keep this list explicit: adding a new pure package requires deciding its
# boundary rather than silently inheriting a transitive UI or persistence import.
# match and agent are integration packages by design (they own persistence and
# external HTTP adapters respectively), so they are not asserted as pure here.
packages="./internal/engine ./internal/protocol"
for pkg in $packages; do
  deps=$($GOCMD list -deps "$pkg")
  bad=
  while IFS= read -r dep; do
    case "$dep" in
      gioui.org/*|net/http|modernc.org/sqlite|database/sql)
        bad="${bad}${dep}
"
        ;;
    esac
  done <<EOF
$deps
EOF
  if [ -n "$bad" ]; then
    echo "import boundary violation in $pkg:" >&2
    printf '%s' "$bad" >&2
    exit 1
  fi
  echo "import boundary: $pkg ok"
done
