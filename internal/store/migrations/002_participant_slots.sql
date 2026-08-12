PRAGMA foreign_keys = ON;
ALTER TABLE games ADD COLUMN move_count INTEGER NOT NULL DEFAULT 0;


CREATE TABLE participants_new (
    game_id TEXT NOT NULL,
    id TEXT NOT NULL,
    name TEXT NOT NULL,
    brain_version_id TEXT,
    kind TEXT NOT NULL DEFAULT '',
    score INTEGER NOT NULL DEFAULT 0,
    payload BLOB NOT NULL,
    slot INTEGER NOT NULL,
    PRIMARY KEY(game_id, id),
    UNIQUE(game_id, slot),
    FOREIGN KEY(game_id) REFERENCES games(id) ON DELETE CASCADE,
    FOREIGN KEY(brain_version_id) REFERENCES brain_versions(id)
);

INSERT INTO participants_new(game_id,id,name,brain_version_id,kind,score,payload,slot)
SELECT p.game_id,p.id,p.name,p.brain_version_id,p.kind,p.score,p.payload,
       (SELECT COUNT(*) - 1 FROM participants prior
        WHERE prior.game_id = p.game_id AND prior.rowid <= p.rowid)
FROM participants p;

DROP TABLE participants;
ALTER TABLE participants_new RENAME TO participants;

CREATE INDEX idx_participants_game ON participants(game_id, slot);
CREATE TRIGGER participants_slot_immutable_update BEFORE UPDATE OF slot ON participants BEGIN SELECT RAISE(ABORT, 'immutable participant slot'); END;

ALTER TABLE brains ADD COLUMN type TEXT NOT NULL DEFAULT '';
ALTER TABLE brains ADD COLUMN frozen INTEGER NOT NULL DEFAULT 0 CHECK(frozen IN (0,1));
CREATE UNIQUE INDEX idx_brain_rules_canonical ON brain_rules(hash, payload);
CREATE INDEX idx_brains_directory ON brains(name, type, frozen, created_at, id);
CREATE INDEX idx_brain_versions_id_brain ON brain_versions(id, brain_id);
CREATE INDEX idx_games_brain_status ON games(brain_version_id, status, updated_at, id);
CREATE INDEX idx_events_game_range ON game_events(game_id, sequence, hash);
CREATE TRIGGER games_completed_immutable_update BEFORE UPDATE ON games
WHEN OLD.status IN ('completed','cancelled')
BEGIN SELECT RAISE(ABORT, 'immutable completed game'); END;
CREATE TRIGGER games_completed_immutable_delete BEFORE DELETE ON games
WHEN OLD.status = 'completed'
BEGIN SELECT RAISE(ABORT, 'immutable completed game'); END;
