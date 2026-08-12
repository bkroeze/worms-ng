# Brain debugging and replay

`wormsctl` can inspect the SQLite database directly or read the same data through
the HTTP API. Set exactly one source with `--db`/`WORMS_DB` or
`--api`/`WORMS_API_URL`.

## Brain inspection

```sh
wormsctl --db worms.db brain show baseline --json --rules --provenance --games
wormsctl --db worms.db brain diff v1-id v2-id --json
wormsctl --api http://127.0.0.1:8080 brain show baseline --json
```

`brain show` accepts `--version ID-or-number` and `--pattern text`; `--games`
adds associated games. `brain diff` accepts two IDs as positional arguments or
`--from ID --to ID`. Output is versioned JSON suitable for archiving. Rules and
provenance are omitted unless requested.

## Game replay and verification

```sh
wormsctl --db worms.db game list --json
wormsctl --db worms.db game list --status running --brain version-id --json
wormsctl --db worms.db game replay game-id --json
wormsctl --db worms.db game replay game-id --seek 25 --json
wormsctl --db worms.db game replay game-id --pattern "unknown" --json
wormsctl --db worms.db game replay game-id --stop-on-brain brain-version-id --json
wormsctl --db worms.db game verify game-id --json
```

`game verify` checks the stored event chain and returns the verified sequence.
`game replay` reconstructs the game and can stop at a sequence (`--seek`), a
pattern, or a brain version. `game list --decisions`, `--captures`, and `--deaths`
return replay reports filtered to those event classes. Replays should be
performed against a backup or a quiesced database when evidence must remain
stable.

CLI failures have stable exit codes: `2` invalid input, `3` not found, `4`
connection/I/O, `5` schema incompatibility, and `6` corrupt data. Preserve both
stdout and stderr in incident records.

## Diagnostic export/import

Export a redacted, portable diagnostic document:

```sh
wormsctl --db worms.db --redact diagnostic export baseline game-id --out incident.json
wormsctl diagnostic import incident.json --out normalized.json
```

Export accepts an optional brain ID and game ID. Import validates and rewrites a
diagnostic document; it is not a database restore and does not mutate SQLite.
Use the database backup/restore procedure for recovery.

For release evidence, preserve the exact CLI version, database SHA-256, command
line (without secrets), stdout, and stderr. Compare `game verify` and replay
output before and after restore; a diagnostic import succeeding is not proof
that SQLite state was restored. The in-game inspector is expected to show the
same brain ID/version and hashes as `wormsctl brain show --json`.
