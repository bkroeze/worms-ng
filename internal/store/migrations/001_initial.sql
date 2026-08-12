PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_metadata (
    key TEXT PRIMARY KEY NOT NULL,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS migrations (
    version INTEGER PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS brains (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS brain_rules (
    id TEXT PRIMARY KEY NOT NULL,
    payload BLOB NOT NULL,
    hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS brain_lineages (
    id TEXT PRIMARY KEY NOT NULL,
    parent_version_id TEXT,
    payload BLOB NOT NULL,
    hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY(parent_version_id) REFERENCES brain_versions(id)
);
CREATE TABLE IF NOT EXISTS brain_provenance (
    id TEXT PRIMARY KEY NOT NULL,
    payload BLOB NOT NULL,
    hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS brain_versions (
    id TEXT PRIMARY KEY NOT NULL,
    brain_id TEXT NOT NULL,
    version INTEGER NOT NULL,
    rules_id TEXT NOT NULL,
    lineage_id TEXT NOT NULL,
    provenance_id TEXT NOT NULL,
    payload BLOB NOT NULL,
    hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY(brain_id) REFERENCES brains(id) ON DELETE CASCADE,
    FOREIGN KEY(rules_id) REFERENCES brain_rules(id),
    FOREIGN KEY(lineage_id) REFERENCES brain_lineages(id),
    FOREIGN KEY(provenance_id) REFERENCES brain_provenance(id),
    UNIQUE(brain_id, version)
);

CREATE TABLE IF NOT EXISTS games (
    id TEXT PRIMARY KEY NOT NULL,
    brain_version_id TEXT,
    rules_payload BLOB NOT NULL,
    status TEXT NOT NULL,
    seed INTEGER NOT NULL,
    sequence INTEGER NOT NULL DEFAULT 0,
    event_hash TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(brain_version_id) REFERENCES brain_versions(id)
);
CREATE TABLE IF NOT EXISTS participants (
    game_id TEXT NOT NULL,
    id TEXT NOT NULL,
    name TEXT NOT NULL,
    brain_version_id TEXT,
    kind TEXT NOT NULL DEFAULT '',
    score INTEGER NOT NULL DEFAULT 0,
    payload BLOB NOT NULL,
    PRIMARY KEY(game_id, id),
    FOREIGN KEY(game_id) REFERENCES games(id) ON DELETE CASCADE,
    FOREIGN KEY(brain_version_id) REFERENCES brain_versions(id)
);
CREATE TABLE IF NOT EXISTS game_events (
    game_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    type TEXT NOT NULL,
    payload BLOB NOT NULL,
    prev_hash TEXT NOT NULL,
    hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(game_id, sequence),
    UNIQUE(game_id, hash),
    FOREIGN KEY(game_id) REFERENCES games(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS game_snapshots (
    game_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    payload BLOB NOT NULL,
    hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(game_id, sequence),
    FOREIGN KEY(game_id) REFERENCES games(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS tournaments (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL,
    rules_payload BLOB NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tournament_matches (
    id TEXT PRIMARY KEY NOT NULL,
    tournament_id TEXT NOT NULL,
    game_id TEXT,
    round INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    payload BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY(tournament_id) REFERENCES tournaments(id) ON DELETE CASCADE,
    FOREIGN KEY(game_id) REFERENCES games(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_brain_versions_brain ON brain_versions(brain_id, version);
CREATE INDEX IF NOT EXISTS idx_games_status_updated ON games(status, updated_at, id);
CREATE INDEX IF NOT EXISTS idx_participants_game ON participants(game_id, id);
CREATE INDEX IF NOT EXISTS idx_events_game_sequence ON game_events(game_id, sequence);
CREATE INDEX IF NOT EXISTS idx_snapshots_game_sequence ON game_snapshots(game_id, sequence);
CREATE INDEX IF NOT EXISTS idx_matches_tournament_round ON tournament_matches(tournament_id, round, id);
CREATE TRIGGER IF NOT EXISTS brains_immutable_update BEFORE UPDATE ON brains BEGIN SELECT RAISE(ABORT, 'immutable brain'); END;
CREATE TRIGGER IF NOT EXISTS brains_immutable_delete BEFORE DELETE ON brains BEGIN SELECT RAISE(ABORT, 'immutable brain'); END;
CREATE TRIGGER IF NOT EXISTS brain_rules_immutable_update BEFORE UPDATE ON brain_rules BEGIN SELECT RAISE(ABORT, 'immutable brain rules'); END;
CREATE TRIGGER IF NOT EXISTS brain_rules_immutable_delete BEFORE DELETE ON brain_rules BEGIN SELECT RAISE(ABORT, 'immutable brain rules'); END;
CREATE TRIGGER IF NOT EXISTS brain_lineages_immutable_update BEFORE UPDATE ON brain_lineages BEGIN SELECT RAISE(ABORT, 'immutable brain lineage'); END;
CREATE TRIGGER IF NOT EXISTS brain_lineages_immutable_delete BEFORE DELETE ON brain_lineages BEGIN SELECT RAISE(ABORT, 'immutable brain lineage'); END;
CREATE TRIGGER IF NOT EXISTS brain_provenance_immutable_update BEFORE UPDATE ON brain_provenance BEGIN SELECT RAISE(ABORT, 'immutable brain provenance'); END;
CREATE TRIGGER IF NOT EXISTS brain_provenance_immutable_delete BEFORE DELETE ON brain_provenance BEGIN SELECT RAISE(ABORT, 'immutable brain provenance'); END;
CREATE TRIGGER IF NOT EXISTS brain_versions_immutable_update BEFORE UPDATE ON brain_versions BEGIN SELECT RAISE(ABORT, 'immutable brain version'); END;
CREATE TRIGGER IF NOT EXISTS brain_versions_immutable_delete BEFORE DELETE ON brain_versions BEGIN SELECT RAISE(ABORT, 'immutable brain version'); END;
