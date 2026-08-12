#!/bin/sh
# Audit Go module distributions for license/notice files and emit a release report.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
: "${GOCMD:=go}"
: "${LICENSE_ARTIFACT_DIR:=$ROOT/dist/license-audit}"
strict=0
if [ "${1:-}" = "--strict" ]; then
  strict=1
elif [ "${1:-}" != "" ]; then
  echo "usage: $0 [--strict]" >&2
  exit 2
fi
command -v "$GOCMD" >/dev/null 2>&1 || { echo "go is required" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 2; }

mkdir -p "$LICENSE_ARTIFACT_DIR"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT INT TERM
cd "$ROOT"
"$GOCMD" list -m -json all >"$tmp"
python3 - "$tmp" "$LICENSE_ARTIFACT_DIR" "$strict" <<'PY'
import json, os, pathlib, re, sys
src, out_dir, strict = sys.argv[1], pathlib.Path(sys.argv[2]), int(sys.argv[3])
raw = pathlib.Path(src).read_text(encoding="utf-8")
decoder = json.JSONDecoder()
pos = 0
mods = []
while pos < len(raw):
    while pos < len(raw) and raw[pos].isspace():
        pos += 1
    if pos >= len(raw):
        break
    obj, end = decoder.raw_decode(raw, pos)
    mods.append(obj)
    pos = end
license_names = re.compile(r"^(?:copying|license|licence|notice)(?:[._ -].*)?$", re.I)
rows, missing = [], []
for mod in mods:
    if mod.get("Main"):
        continue
    path = mod.get("Dir")
    if not path:
        continue
    root = pathlib.Path(path)
    files = sorted(p.relative_to(root).as_posix() for p in root.iterdir() if p.is_file() and license_names.match(p.name))
    if files:
        rows.append({"path": mod.get("Path", ""), "version": mod.get("Version", ""), "licenses": files})
    else:
        missing.append({"path": mod.get("Path", ""), "version": mod.get("Version", ""), "dir": str(root)})
report = {"schema": 1, "modules": rows, "missing": missing}
out_dir.mkdir(parents=True, exist_ok=True)
(out_dir / "report.json").write_text(json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8")
with (out_dir / "report.txt").open("w", encoding="utf-8") as fh:
    for row in rows:
        fh.write(f"{row['path']} {row['version']}: {', '.join(row['licenses'])}\n")
    if missing:
        fh.write("\nMISSING LICENSE/NOTICE FILES:\n")
        for row in missing:
            fh.write(f"{row['path']} {row['version']} ({row['dir']})\n")
print(f"license audit: {len(rows)} modules with notices, {len(missing)} missing")
if missing and strict:
    raise SystemExit("license audit failed: collect missing notices before publishing")
PY
