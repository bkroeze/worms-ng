# Integration evidence

Integration evidence must cross the real engine, persistence, HTTP, CLI, and
browser boundaries. Unit tests that replace SQLite or call Gio model methods are
useful diagnostics but do not close this release gate.

## Native and SQLite path

From a clean checkout:

```sh
make check-import-boundaries
make build-wasm
make smoke SMOKE_ARTIFACT_DIR=dist/smoke
ARTIFACT_DIR=dist/release-acceptance scripts/release-acceptance.sh
```

`make smoke` starts the actual server binary against a fresh SQLite file, checks
all embedded content-hashed WASM assets and MIME types, exercises health/build/
demo, creates and resumes a game, appends an optimistic move, and runs
`wormsctl game list` and `wormsctl game verify` against the same database.
`scripts/release-acceptance.sh` then runs the packaged first-release path:
game replay and verification, a completed tournament/match, live brain show,
diagnostic export/import into an empty DB, backup integrity/restore, and both
real-browser release viewports. On failure it keeps server logs, request/
response JSON, CLI output, generated assets, and the database evidence.

For the longer persistence scenarios, run the real-store suites (not only
in-memory tests):

```sh
go test ./internal/store ./internal/match ./internal/tournament ./internal/server
```

The scenarios named `SQLitePersistenceAndResume`,
`SQLiteBlackBoxRestartAfterTransitionKeepsStateHash`,
`TeachAppliesDecisionAndSurvivesRestart`, `SameCopiesLastCompletedSlot`, and
`SQLiteBlackBoxResumesActiveRoundWithoutDuplicate` cover restart/resume,
teach-save, SAME reuse, and tournament resume.
The server contract tests additionally exercise optimistic conflicts and
transaction rollback. Preserve the `-json` test output in the release record.

## Browser and authoritative effects

Use `make browser-smoke` only against the isolated loopback server it starts.
The procedure in [browser-support.md](browser-support.md) sends a real pointer
press/release to the visible Gio start control, observes the rendered canvas,
records browser errors/network traffic, and verifies the resulting `ui-*` game,
cursor, and SQLite health through the API. A canvas screenshot or a DOM
placeholder alone is not evidence. Repeat every supported viewport and attach
artifacts to the release.

## Failure and retry semantics

A stale cursor/hash must return `409` and leave the event sequence unchanged.
Retry only after the caller explicitly reloads the authoritative cursor and
resubmits the intended action. A failed transaction must not create a partial
game/event/snapshot; verify this through `wormsctl game verify` and a fresh
resume. Never retry a timeout blindly, because a committed response may have
been lost in transit.
