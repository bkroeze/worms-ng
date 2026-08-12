# Release engineering

Releases are built from a clean checkout with a pinned Go toolchain matching
`go.mod`. The build embeds the generated browser client into the server; generated
WASM, binaries, and databases are build products and must never be committed.

## Local release workflow

```sh
make clean
make build
make metadata VERSION=v0.1.0
make checksums VERSION=v0.1.0
make package-native VERSION=v0.1.0 GOOS=linux GOARCH=amd64
make smoke
```

`make metadata` writes `dist/build-metadata.json` containing release version,
commit, normalized build time, Go/Gio versions, target, API/schema/protocol
versions, dependency hash, and the content hash plus filenames of every browser
asset. `make checksums` writes `dist/SHA256SUMS` for release files.
`make package-native` writes a reproducible archive containing
`worms-server`, `worms-client`, `wormsctl`, and the reference `worms-agent`
executable under `dist/`; set `GOOS` and `GOARCH` explicitly for each supported
package. Inspect metadata and checksums before publishing.

Linux native builds require the platform development packages used by Gio
(`pkg-config`, X11/XKB common, Wayland, and EGL headers). Install the equivalent
packages for the target runner before `make build-native` or
`make package-native`; cross-compiling still needs these host build dependencies.
Release builds default to Gio's `novulkan` build tag. Install the target Vulkan
development headers and set `CLIENT_BUILD_TAGS=` only for packages that should
include Gio's Vulkan renderer.
Browser assets are content-addressed (`main-<sha256-prefix>.wasm` and
`wasm-<sha256-prefix>.js`) and `index.html` references the exact names from the
same build. The server may cache those immutable names for a year without
allowing an upgraded client to reuse old bytes. Never rename an asset manually
or copy assets between packages. The browser smoke must be run against the
packaged server after an upgrade-cache check: load release A, replace the
package with release B on the same origin, reload, and verify the B asset names
and `/api/v1/build` identity are observed.

## Release checklist

- [ ] Clean checkout and reproducible dependency download (`go mod download`).
- [ ] `make build`, `make wasm-test`, and `make smoke` pass.
- [ ] `make check-import-boundaries`, `make performance`, and `make reproducible` pass.
- [ ] `scripts/release-acceptance.sh` passes with packaged binaries. It creates
      and resumes a SQLite game, commits a move, runs `wormsctl` verification
      and replay, creates/finishes a tournament match, shows a brain by ID,
      exports/imports a diagnostic into an empty DB, and checks backup restore.
- [ ] Real Chromium evidence exists for 1280x720 and 320x480. The browser
      flow performs move, pause, refresh/resume, responsive resize, and brain
      inspection; no console/page errors or HTTP request >=400 is allowed.
- [ ] `make metadata` and `make checksums` are archived with packages and
      `sha256sum -c dist/SHA256SUMS` passes.
- [ ] Native packages contain `worms-server`, `worms-client`, `wormsctl`, and
      `worms-agent` and are built for each declared OS/architecture.
- [ ] No generated WASM, binary, database, credentials, or local config is in the
      source archive.
- [ ] Upgrade cache, backup, restore, and rollback were tested against a copy of
      a representative DB.
- [ ] Operator, configuration, player, debug, and API docs match published flags
      and endpoints.
- [ ] `scripts/license-audit.sh --strict` passed and third-party/original-branding
      notices are included in distribution materials.

The complete clean-checkout command is:

```sh
make clean
go mod download
make build wasm-test smoke check-import-boundaries performance reproducible
scripts/license-audit.sh --strict
ARTIFACT_DIR=dist/release-acceptance scripts/release-acceptance.sh
```


## Clean deploy, rollback, and evidence

Use a clean checkout and keep the source tree separate from the runtime
directory:

```sh
git clone "$RELEASE_SOURCE_URL" worms-ng-release
cd worms-ng-release
go mod download
make clean build wasm-test smoke
scripts/license-audit.sh --strict
make release VERSION=v0.1.0 GOOS=linux GOARCH=amd64
ARTIFACT_DIR=dist/release-acceptance scripts/release-acceptance.sh
```

Install `dist/worms-ng-*.tar.gz` into a new versioned directory, copy the
server binary into place, and start it with an explicit `-db` path as described
in [the operator runbook](operator.md). Never run a release binary against a
database that has not been backed up and migration-tested. The clean-deploy
acceptance path also creates a game, resumes it, runs the CLI verification and
diagnostic export, and checks the browser flow in
[browser-support.md](browser-support.md).

Rollback is a binary-and-database compatibility decision, not just a process
restart:

1. Stop writes and preserve the current binary, logs, database, `-wal`, and
   `-shm` files for forensics.
2. Verify a backup with `sha256sum` and `make db-check`; restore it to a separate
   path and run the old binary there.
3. Roll back only when the old binary understands the database schema reported
   by `/api/v1/schema`. A forward migration may require restoring the pre-upgrade
   backup; do not edit migration rows or copy a live database.
4. Start the old binary on the verified copy, run health/build/schema and replay
   checks, then promote the copy atomically and monitor the logs.

Archive `dist/build-metadata.json`, `dist/SHA256SUMS`, package hashes, browser
snapshots/screenshots/network logs, smoke logs, benchmark summaries, and the
database backup checksum together. Metadata contains source commit, normalized
build time (`SOURCE_DATE_EPOCH`), Go/Gio versions, API/schema/protocol versions,
target, and dependency hash. A dirty tree or a missing checksum is a release
blocker.