# Replay, migration, backup, and restore

## Replay integrity

Game events are append-only and linked by sequence and hash. Snapshots accelerate
resume but are not a substitute for event verification. Before exporting an
incident, run:

```sh
wormsctl --db /var/lib/worms-ng/worms.db game verify GAME_ID --json \
  > GAME_ID.verify.json
wormsctl --db /var/lib/worms-ng/worms.db game replay GAME_ID --json \
  > GAME_ID.replay.json
```

Keep the verification output with the corresponding database checksum. A failed
verification means the data is corrupt or was captured while being changed;
stop writes and restore the last known-good backup.

## Schema migrations

Opening a database runs embedded, ordered SQL migrations. The current schema
version is reported by `/api/v1/schema` and `wormsctl` output. Migrations are
forward-only; do not edit migration history or manually alter tables in a live
installation.

Upgrade procedure:

1. Announce a maintenance window and stop the server cleanly.
2. Make and verify a backup.
3. Start the new binary against a copy of the database first.
4. Check `/api/v1/schema`, run `make db-check DB=/path/to/copy.db`, and verify a
   representative game replay.
5. Replace the production binary, start it against the original database, and
   repeat health/schema/replay checks.

If migration fails, keep the original file untouched and roll back the binary;
do not delete migration rows to force startup.

## Online-consistent backup

With the SQLite command-line client installed, use its backup API while the
server is running:

```sh
sqlite3 /var/lib/worms-ng/worms.db \
  ".backup '/var/backups/worms-ng-$(date -u +%Y%m%dT%H%M%SZ).db'"
```

The repository also provides a stopped-server check:

```sh
make db-check DB=/var/backups/worms-ng-20260811T000000Z.db
sha256sum /var/backups/worms-ng-20260811T000000Z.db
```

If `sqlite3` is unavailable, stop the service, copy the database and any
SQLite `-wal`/`-shm` files together (or remove them only after a clean stop),
then run `make db-check` on the copy. Keep several generations and store at
least one off host.

## Restore

1. Stop the server and preserve the damaged file for forensics.
2. Verify the candidate backup checksum and run `make db-check DB=BACKUP`.
3. Start a temporary server on the candidate with `make smoke` or
   `./bin/worms-server -addr 127.0.0.1:18080 -db BACKUP`.
4. Check health, schema, and representative `game verify`/replay output.
5. Install the verified file with ownership and permissions matching the service,
   then restart and monitor writes.

A diagnostic export is useful evidence but cannot restore SQLite state. Never
restore by copying a database over a live process.


For a release rollback, archive the failed database before restoring and record
both the old/new `/api/v1/schema` responses. If the new binary applied a
forward migration, restore the pre-upgrade backup rather than pointing the old
binary at the migrated file. Keep the old package checksum and run the same
CLI verification commands against the restored copy before promoting it.