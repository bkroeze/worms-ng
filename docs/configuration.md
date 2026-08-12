# Configuration

The server currently uses command-line flags rather than a configuration file.
Keep the database path and listener explicit in production:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `:8080` | HTTP listen address. Prefer a loopback address behind a proxy. |
| `-db` | `worms.db` | SQLite database filename or URI. Parent directories must already exist. |
| `-cors-origin` | empty | Comma- or space-separated allowed origins; `*` allows every origin. |

Example:

```sh
./bin/worms-server -addr 127.0.0.1:8080 \
  -db /var/lib/worms-ng/worms.db \
  -cors-origin https://play.example.test
```

## CLI environment

`wormsctl` accepts `WORMS_DB` and `WORMS_API_URL` as defaults. A command-line
`--db` or `--api` value overrides the corresponding environment value. Do not
set both for one invocation. `--out -` writes to stdout; `--out path` writes a
diagnostic result to that path. `--json` selects JSON output where the command
supports a human-readable mode, and `--redact` removes sensitive diagnostic
fields during export.

```sh
export WORMS_DB=/var/lib/worms-ng/worms.db
wormsctl game list --json
WORMS_API_URL=http://127.0.0.1:8080 wormsctl game list --json
```

## Deployment policy

There is no authentication, TLS, rate limiting, or secret management in the
binary. Put those controls in the deployment layer. Restrict write access to
the SQLite file to the service account, keep backups outside the web root, and
avoid putting API URLs containing credentials in shell history. Keep CORS as a
narrow allow-list and use a separate database for development, smoke tests, and
benchmarks.


See [the operator runbook](operator.md) for startup/health checks,
[backup and restore](replay-migration-backup.md) for SQLite lifecycle work, and
[browser support](browser-support.md) before exposing the embedded client. A
production configuration record should include the binary checksum, database
path, listener/CORS values, API/schema versions, and service-account ownership.
The API accepts one JSON value per request and limits request bodies to 1 MiB.
Unknown JSON fields are rejected. Every request body that carries a versioned
resource must include `"version":"v1"`.
