# HTTP API

The service exposes a versioned JSON API under `/api/v1`. Responses carry a
`version` field. Request JSON is strict: one value, no trailing values, no
unknown fields, and at most 1 MiB.

## Read-only endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/v1/health` | Service/database readiness. |
| `GET` | `/api/v1/build` | API, service, and schema versions. |
| `GET` | `/api/v1/metadata` | Same metadata without the build alias. |
| `GET` | `/api/v1/schema` | API and migration schema metadata. |
| `GET` | `/api/v1/demo` | Small embedded-client smoke response. |
| `GET` | `/api/v1/games` | List games; `limit` and `offset` page, `status` filters. |
| `GET` | `/api/v1/games/{id}` | Fetch a game. |
| `GET` | `/api/v1/games/{id}/resume` | Fetch game and reconstructable state. |
| `GET` | `/api/v1/brains` | List brains. |
| `GET` | `/api/v1/brains/{id}` | Fetch an immutable brain. |
| `GET` | `/api/v1/brains/{id}/versions` | List immutable versions. |
| `GET` | `/api/v1/brain-versions/{id}` | Fetch one version. |
| `GET` | `/api/v1/brains/{id}/inspect` | Summarize versions and latest number. |
| `GET` | `/api/v1/brains/{id}/diff?from=A&to=B` | Compare two version IDs. |
| `GET` | `/api/v1/tournaments` | List tournaments. |
| `GET` | `/api/v1/tournaments/{id}/matches` | List tournament matches. |
| `GET` | `/api/v1/matches/{id}` | Fetch a match. |

Paging defaults to 100 rows and supports `limit` (1–1000) and non-negative
`offset`. Unsupported API versions return a versioned `not_found` error.

## Create resources

Create a brain:

```sh
curl --fail -X POST http://127.0.0.1:8080/api/v1/brains \
  -H 'content-type: application/json' \
  -d '{"version":"v1","id":"baseline","name":"Baseline","description":"Example"}'
```

Create a version. Payload components are JSON and are stored in a versioned
payload envelope by the server:

```sh
curl --fail -X POST http://127.0.0.1:8080/api/v1/brains/baseline/versions \
  -H 'content-type: application/json' \
  -d '{"version":"v1","number":1,"rules":{"board":"classic"},"lineage":{},"provenance":{"source":"local"},"payload":{"policy":"safe"}}'
```

Create a game. Each participant needs an ID; the current classic state can be
constructed from participant IDs when no explicit snapshot is supplied:

```sh
curl --fail -X POST http://127.0.0.1:8080/api/v1/games \
  -H 'content-type: application/json' \
  -d '{"version":"v1","id":"demo-1","status":"running","participants":[{"id":"w1","name":"Player 1","kind":"human"}]}'
```

`rules` is preferred over the compatibility alias `rules_payload`; `state` can
supply an initial snapshot. `brain_version_id`, `seed`, and participant payloads
are optional.

## Optimistic game writes

`POST /api/v1/games/{id}/act`, `/teach`, `/pause`, `/tick`, and `/abort`
require the current cursor and event hash. The cursor may be sent as `cursor`,
`sequence`, `expected_cursor`, or `expected_version` (if multiple are supplied
they must agree). The hash may be `event_hash` or the `If-Match` header.

Example move:

```sh
curl --fail -X POST http://127.0.0.1:8080/api/v1/games/demo-1/act \
  -H 'content-type: application/json' \
  -d '{"version":"v1","cursor":0,"event_hash":"","worm_id":"w1","direction":0}'
```

A successful write returns the updated game and authoritative state;
event-producing commands also return their appended events. A stale cursor/hash
returns `409` with conflict details. Invalid JSON/version/action is `400`; a
legal-but-impossible move is `422`.

`POST /api/v1/games/{id}/abort` atomically changes the game status to
`cancelled` when the supplied cursor and event hash match. It does not append an
event or advance the cursor or event hash, and returns the updated game with the
current state (plus extension state for extension games). A cancelled game is
terminal, so later `/act`, `/teach`, `/pause`, and `/tick` writes are rejected.

## Errors and compatibility

Errors have this shape:

```json
{"version":"v1","error":{"code":"invalid_request","message":"...","details":null}}
```

Clients should branch on the error code, not human text, and treat unknown
fields as forward-compatible. The server intentionally rejects unknown request
fields so clients discover contract drift early.

The browser client uses the same-origin API by default. Cross-origin WASM
hosting requires an explicit server CORS allow-list and a successful OPTIONS
preflight; do not use `*` for an authenticated or otherwise untrusted
deployment. The release smoke checks content types for `main.wasm` and
`wasm.js` and checks that health/build responses came from the SQLite-backed
server.
