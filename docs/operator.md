# Operator runbook

This document covers a small, single-process deployment of the Worms NG server. The
server owns one SQLite database and serves the embedded browser client.

## Start and stop

Build the client assets and server, then run with an explicit database path:

```sh
make build-server
./bin/worms-server -addr 127.0.0.1:8080 -db /var/lib/worms-ng/worms.db
```

`-addr` defaults to `:8080`; `-db` defaults to `worms.db`. The process handles
SIGINT and SIGTERM and allows five seconds for HTTP shutdown. Run it under a
supervisor (systemd, a container runtime, or an equivalent service manager) and
send SIGTERM for normal shutdown. Do not copy a live SQLite file while the server
is writing it; use the backup procedure in [backup and restore](replay-migration-backup.md).

The service has no built-in authentication. Bind to loopback or put it behind an
authenticated, TLS-terminating reverse proxy before exposing it to a network.
Never use `-cors-origin '*'` on an untrusted public listener.

For a packaged deployment, verify `dist/SHA256SUMS` first, unpack into a new
versioned directory, and switch the supervisor to that directory only after
the temporary health/schema and browser smoke checks pass. Keep the previous
binary directory available for rollback; the database compatibility decision
and restore procedure are documented in
[replay-migration-backup.md](replay-migration-backup.md).

## Health and build checks

The versioned API is currently `v1`:

```sh
curl --fail http://127.0.0.1:8080/api/v1/health
curl --fail http://127.0.0.1:8080/api/v1/build
curl --fail http://127.0.0.1:8080/api/v1/schema
```

A healthy response has `status: "ok"`; build and schema responses expose service
and migration versions. `make smoke` exercises these checks against a temporary
SQLite database and should be used after packaging or deployment.

## CORS and logs

Pass a comma- or space-separated allow-list when a browser client is hosted on a
different origin:

```sh
./bin/worms-server -addr 127.0.0.1:8080 \
  -cors-origin https://play.example.test,https://admin.example.test
```

An empty value (the default) disables cross-origin requests. The server logs its
listen URL and fatal startup/serve errors to stderr; collect stdout/stderr with
the service manager. Treat database-open errors, migration errors, and repeated
HTTP 5xx responses as incidents rather than retrying writes blindly.

## Incident checklist

1. Preserve logs and the database before attempting repair.
2. Check `/api/v1/health`, `/api/v1/build`, and `/api/v1/schema`.
3. Stop the server before filesystem-level database work.
4. Make a verified backup, then run `make db-check DB=/path/to/worms.db`.
5. Restore to a separate path and verify there before replacing the live file.
6. Re-run `make smoke` with the exact release artifacts.

SQLite writes use optimistic cursors and event hashes. A `409` conflict means a
client submitted stale state; fetch the game again and retry only after comparing
its cursor and `event_hash`.
