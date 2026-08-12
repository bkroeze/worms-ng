package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func payload(t testing.TB, value any) []byte {
	t.Helper()
	p, err := EncodePayload(value)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSQLitePersistenceAndResume(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "worms.sqlite")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	brain, err := s.CreateBrain(ctx, CreateBrainInput{ID: "brain-1", Name: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	version, err := s.CreateBrainVersion(ctx, CreateBrainVersionInput{ID: "v1", BrainID: brain.ID, Version: 1, Rules: payload(t, map[string]any{"move": "classic"}), Lineage: payload(t, map[string]any{"root": true}), Provenance: payload(t, map[string]any{"source": "test"}), Payload: payload(t, map[string]any{"frozen": false})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateBrainVersion(ctx, CreateBrainVersionInput{ID: "v2", BrainID: brain.ID, Version: 2, ParentVersionID: version.ID, Rules: payload(t, map[string]any{"move": "classic"}), Lineage: payload(t, map[string]any{"root": true}), Provenance: payload(t, map[string]any{"source": "test"}), Payload: payload(t, map[string]any{"frozen": true})})
	if err != nil {
		t.Fatal(err)
	}
	game, err := s.CreateGame(ctx, CreateGameInput{ID: "game-1", BrainVersionID: version.ID, RulesPayload: payload(t, map[string]any{"board": 18}), Participants: []Participant{{ID: "p1", Name: "one", BrainVersionID: version.ID, Payload: payload(t, map[string]any{})}}})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.AppendGameEvents(ctx, game.ID, 0, "", []EventInput{{Type: "turn", Payload: payload(t, map[string]any{"direction": 1})}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("events = %#v", events)
	}
	if _, err = s.AppendGameEvents(ctx, game.ID, 0, "", []EventInput{{Type: "stale", Payload: payload(t, nil)}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale append error = %v", err)
	}
	if err = s.SaveSnapshot(ctx, game.ID, Snapshot{Sequence: 1, Payload: payload(t, map[string]any{"state": "saved"})}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resumed, err := s.ResumeGame(ctx, game.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Sequence != 1 || resumed.EventHash != events[0].Hash || len(resumed.Participants) != 1 {
		t.Fatalf("resumed = %#v", resumed)
	}
	snap, err := s.LoadLatestSnapshot(ctx, game.ID)
	if err != nil || snap.Sequence != 1 {
		t.Fatalf("snapshot = %#v, err=%v", snap, err)
	}
}

func TestImmutableBrainAndCorruptPayload(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b, err := s.CreateBrain(ctx, CreateBrainInput{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().ExecContext(ctx, "UPDATE brains SET name='changed' WHERE id=?", b.ID); err == nil {
		t.Fatal("brain update unexpectedly succeeded")
	}
	if _, err = s.DB().ExecContext(ctx, "UPDATE brain_rules SET payload=? WHERE id='missing'", []byte("bad")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().ExecContext(ctx, "UPDATE games SET rules_payload=? WHERE id='missing'", []byte("bad")); err != nil {
		t.Fatal(err)
	}
	v, err := s.CreateBrainVersion(ctx, CreateBrainVersionInput{BrainID: b.ID, Version: 1, Rules: payload(t, nil), Lineage: payload(t, nil), Provenance: payload(t, nil), Payload: payload(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().ExecContext(ctx, "UPDATE brain_versions SET payload=? WHERE id=?", []byte("not-json"), v.ID); err == nil {
		t.Fatal("version update unexpectedly succeeded")
	}
	if _, err = s.DB().ExecContext(ctx, "INSERT INTO games(id,rules_payload,status,seed,created_at,updated_at) VALUES('broken',?, 'active', 0,?,?)", []byte("not-json"), now(), now()); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetGame(ctx, "broken")
	if !errors.Is(err, ErrCorruptPayload) {
		t.Fatalf("corrupt payload error=%v", err)
	}
}

func TestContextCancellationAndForeignKeys(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.CreateBrain(ctx, CreateBrainInput{Name: "cancelled"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	if _, err = s.CreateMatch(context.Background(), CreateMatchInput{TournamentID: "missing", Payload: payload(t, nil)}); err == nil {
		t.Fatal("foreign key unexpectedly succeeded")
	}
	var n int
	if err = s.DB().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n < 1 {
		t.Fatalf("migrations=%d err=%v", n, err)
	}
}
func TestListHydratesRecordsWithoutNestedConnectionDeadlock(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lists.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tournament, err := s.CreateTournament(ctx, CreateTournamentInput{ID: "t1", Name: "cup"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateMatch(ctx, CreateMatchInput{ID: "m1", TournamentID: tournament.ID, Round: 1, Payload: payload(t, map[string]any{"n": 1})}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateMatch(ctx, CreateMatchInput{ID: "m2", TournamentID: tournament.ID, Round: 2, Payload: payload(t, map[string]any{"n": 2})}); err != nil {
		t.Fatal(err)
	}
	if tournaments, err := s.ListTournaments(ctx, TournamentListOptions{}); err != nil || len(tournaments) != 1 {
		t.Fatalf("tournaments=%#v err=%v", tournaments, err)
	}
	if matches, err := s.ListMatches(ctx, tournament.ID, MatchListOptions{}); err != nil || len(matches) != 2 {
		t.Fatalf("matches=%#v err=%v", matches, err)
	}
	if _, err = s.CreateGame(ctx, CreateGameInput{ID: "g1", Participants: []Participant{{ID: "z", Payload: payload(t, nil)}, {ID: "a", Payload: payload(t, nil)}}}); err != nil {
		t.Fatal(err)
	}
	games, err := s.ListGames(ctx, GameListOptions{})
	if err != nil || len(games) != 1 {
		t.Fatalf("games=%#v err=%v", games, err)
	}
	if games[0].Participants[0].ID != "z" || games[0].Participants[1].ID != "a" {
		t.Fatalf("participant order=%v,%v", games[0].Participants[0].ID, games[0].Participants[1].ID)
	}
}

func TestEventPayloadAndSnapshotHashValidation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "integrity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	game, err := s.CreateGame(ctx, CreateGameInput{ID: "g1"})
	if err != nil {
		t.Fatal(err)
	}
	eventPayload := payload(t, map[string]any{"direction": 2})
	if _, err = s.AppendGameEvents(ctx, game.ID, 0, "", []EventInput{{Type: "turn", Payload: eventPayload}}); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListEvents(ctx, game.ID, 0, 10)
	if err != nil || len(events) != 1 || !bytes.Equal(events[0].Payload, eventPayload) {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	if err = s.VerifyEventChain(ctx, game.ID); err != nil {
		t.Fatalf("verify event chain: %v", err)
	}
	snapshotPayload := payload(t, map[string]any{"state": "ok"})
	if err = s.SaveSnapshot(ctx, game.ID, Snapshot{Sequence: 1, Payload: snapshotPayload}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadLatestSnapshot(ctx, game.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().ExecContext(ctx, "UPDATE game_snapshots SET hash='wrong' WHERE game_id=?", game.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadLatestSnapshot(ctx, game.ID); !errors.Is(err, ErrCorruptPayload) {
		t.Fatalf("snapshot hash error=%v", err)
	}
}

func TestSnapshotEnvelopeRequiresVersionAndData(t *testing.T) {
	if err := ValidateSnapshot([]byte(`{"version":1}`)); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("missing data accepted: %v", err)
	}
	raw, err := EncodeSnapshot(map[string]any{"state": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err = DecodeSnapshot(raw, &decoded); err != nil || decoded["state"] != "ok" {
		t.Fatalf("decoded=%v err=%v", decoded, err)
	}
}

func TestAppendEventsWithSnapshotIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	game, err := s.CreateGame(ctx, CreateGameInput{ID: "g1"})
	if err != nil {
		t.Fatal(err)
	}
	event := EventInput{Type: "turn", Payload: payload(t, map[string]any{"direction": 1})}
	snap := Snapshot{Sequence: 1, Payload: payload(t, map[string]any{"state": "one"})}
	events, err := s.AppendGameEventsWithSnapshot(ctx, game.ID, 0, "", []EventInput{event}, snap)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	if err = s.SaveSnapshot(ctx, game.ID, Snapshot{Sequence: 2, Payload: payload(t, map[string]any{"reserved": true})}); err != nil {
		t.Fatal(err)
	}
	_, err = s.AppendGameEventsWithSnapshot(ctx, game.ID, 1, events[0].Hash, []EventInput{{Type: "second", Payload: payload(t, nil)}}, Snapshot{Sequence: 2, Payload: payload(t, map[string]any{"state": "two"})})
	if err == nil {
		t.Fatal("duplicate snapshot unexpectedly succeeded")
	}
	g, err := s.GetGame(ctx, game.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.Sequence != 1 {
		t.Fatalf("sequence advanced after rollback: %d", g.Sequence)
	}
	listed, err := s.ListEvents(ctx, game.ID, 0, 10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("events after rollback=%#v err=%v", listed, err)
	}
}

func TestParticipantSlotMigrationUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY NOT NULL, name TEXT NOT NULL, applied_at TEXT NOT NULL); CREATE TABLE schema_metadata (key TEXT PRIMARY KEY NOT NULL, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	initial, err := migrationFS.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, string(initial)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,applied_at) VALUES(1,'001_initial.sql',?)", now()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO schema_metadata(key,value) VALUES('schema_version','1')"); err != nil {
		t.Fatal(err)
	}
	legacyPayload := payload(t, nil)
	if _, err = db.ExecContext(ctx, "INSERT INTO games(id,rules_payload,status,seed,created_at,updated_at) VALUES('g1',?,'active',0,?,?)", legacyPayload, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO participants(game_id,id,name,kind,payload) VALUES('g1','z','Z','',?)", legacyPayload); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO participants(game_id,id,name,kind,payload) VALUES('g1','a','A','',?)", legacyPayload); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if version, err := s.SchemaVersion(ctx); err != nil || version != 2 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	game, err := s.GetGame(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if len(game.Participants) != 2 || game.Participants[0].ID != "z" || game.Participants[1].ID != "a" || game.Participants[0].Slot != 0 || game.Participants[1].Slot != 1 {
		t.Fatalf("migrated participants=%#v", game.Participants)
	}
}

func TestMigrationFailureRollsBackSchemaAndBookkeeping(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	initial, err := migrationFS.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, string(initial)); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}
	err = s.migrateFiles(ctx, []migrationSpec{{name: "009_failing.sql", sql: "CREATE TABLE rollback_probe(id INTEGER); SELECT no_such_function();"}})
	if !errors.Is(err, ErrMigration) {
		t.Fatalf("migration error=%v", err)
	}
	var n int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version=9").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("failed migration recorded: %d", n)
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM schema_metadata WHERE key='schema_version'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("failed migration metadata recorded: %d", n)
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='rollback_probe'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("failed migration schema change committed")
	}
}

func TestConcurrentRepositoryWritersProduceOneCommittedSequence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.db")
	one, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if _, err = one.CreateGame(ctx, CreateGameInput{ID: "g1"}); err != nil {
		t.Fatal(err)
	}
	in := EventInput{Type: "turn", Payload: payload(t, map[string]any{"n": 1})}
	results := make(chan error, 2)
	go func() { _, e := one.AppendGameEvents(ctx, "g1", 0, "", []EventInput{in}); results <- e }()
	go func() { _, e := two.AppendGameEvents(ctx, "g1", 0, "", []EventInput{in}); results <- e }()
	var committed, conflicts int
	for range 2 {
		switch e := <-results; {
		case e == nil:
			committed++
		case errors.Is(e, ErrConflict):
			conflicts++
		default:
			t.Fatalf("writer error=%v", e)
		}
	}
	if committed != 1 || conflicts != 1 {
		t.Fatalf("committed=%d conflicts=%d", committed, conflicts)
	}
	events, err := one.ListEvents(ctx, "g1", 0, 10)
	if err != nil || len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestBrainRuleCanonicalRoundTripAndQueryPlans(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "query.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b, err := s.CreateBrain(ctx, CreateBrainInput{ID: "b1", Name: "demo", Type: "NEW", Frozen: true})
	if err != nil {
		t.Fatal(err)
	}
	rules := payload(t, map[string]any{"move": "left"})
	v1, err := s.CreateBrainVersion(ctx, CreateBrainVersionInput{BrainID: b.ID, Version: 1, Rules: rules, Lineage: payload(t, nil), Provenance: payload(t, nil), Payload: payload(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.CreateBrainVersion(ctx, CreateBrainVersionInput{BrainID: b.ID, Version: 2, ParentVersionID: v1.ID, Rules: rules, Lineage: payload(t, nil), Provenance: payload(t, nil), Payload: payload(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if v1.Rules.ID != v2.Rules.ID || !bytes.Equal(v1.Rules.Payload, v2.Rules.Payload) {
		t.Fatalf("rules were not canonicalized: %#v %#v", v1.Rules, v2.Rules)
	}
	loaded, err := s.GetBrainVersion(ctx, v2.ID)
	if err != nil || !bytes.Equal(loaded.Rules.Payload, rules) {
		t.Fatalf("loaded rules=%#v err=%v", loaded.Rules, err)
	}
	frozen := true
	brains, err := s.ListBrains(ctx, BrainListOptions{Name: "demo", Type: "NEW", Frozen: &frozen, Limit: 10})
	if err != nil || len(brains) != 1 || !brains[0].Frozen {
		t.Fatalf("filtered brains=%#v err=%v", brains, err)
	}
	for _, q := range []string{
		"EXPLAIN QUERY PLAN SELECT id FROM brain_versions WHERE brain_id='b1' ORDER BY version LIMIT 10",
		"EXPLAIN QUERY PLAN SELECT sequence FROM game_events WHERE game_id='missing' AND sequence>0 ORDER BY sequence LIMIT 10",
	} {
		rows, e := s.DB().QueryContext(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		var detail string
		found := false
		for rows.Next() {
			var id, parent, notused, d any
			if e = rows.Scan(&id, &parent, &notused, &d); e != nil {
				rows.Close()
				t.Fatal(e)
			}
			detail = fmt.Sprint(d)
			if strings.Contains(strings.ToUpper(detail), "USING") {
				found = true
			}
		}
		rows.Close()
		if !found {
			t.Fatalf("query plan did not use index: %s (%s)", q, detail)
		}
	}
}

func TestSQLiteReadCancellationUsesTypedRepositoryError(t *testing.T) {
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.CreateGame(context.Background(), CreateGameInput{ID: "cancel-game"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, call := range map[string]func() error{
		"get":  func() error { _, e := s.GetGame(ctx, "cancel-game"); return e },
		"list": func() error { _, e := s.ListEvents(ctx, "cancel-game", 0, 10); return e },
		"write": func() error {
			_, e := s.AppendGameEvents(ctx, "cancel-game", 0, "", []EventInput{{Type: "move", Payload: payload(t, nil)}})
			return e
		},
	} {
		if err := call(); !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("%s cancellation=%v", name, err)
		}
	}
}

func TestVerifyEventChainBindsPersistedHead(t *testing.T) {
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g, err := s.CreateGame(context.Background(), CreateGameInput{ID: "head-game"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.AppendGameEvents(context.Background(), g.ID, 0, "", []EventInput{{Type: "worm_moved", Payload: payload(t, map[string]any{"n": 1})}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.VerifyEventChain(context.Background(), g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec("UPDATE games SET event_hash='tampered' WHERE id=?", g.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.VerifyEventChain(context.Background(), g.ID); !errors.Is(err, ErrCorruptEvent) {
		t.Fatalf("tampered head verification=%v", err)
	}
	var n int
	if err = s.DB().QueryRow("SELECT count(*) FROM game_events WHERE game_id=?", g.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(events) {
		t.Fatalf("event rows changed after failed verification: %d", n)
	}
}

func TestCompleteGameScoresAreTransactional(t *testing.T) {
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g, err := s.CreateGame(context.Background(), CreateGameInput{ID: "score-game", Participants: []Participant{{ID: "a", Payload: payload(t, nil)}, {ID: "b", Payload: payload(t, nil)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteGame(context.Background(), g.ID, "completed", 0, "", map[string]int64{"a": 7, "missing": 1}, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing participant completion=%v", err)
	}
	got, err := s.GetGame(context.Background(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || got.Participants[0].Score != 0 {
		t.Fatalf("partial completion committed: %#v", got)
	}
	if err = s.CompleteGame(context.Background(), g.ID, "completed", 0, "", map[string]int64{"a": 7, "b": 3}, 9); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetGame(context.Background(), g.ID)
	if err != nil || got.Status != "completed" || got.MoveCount != 9 || got.Participants[0].Score != 7 || got.Participants[1].Score != 3 {
		t.Fatalf("completion=%#v err=%v", got, err)
	}
}

func TestPagedRulesProvenanceAndUsageQueries(t *testing.T) {
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b, err := s.CreateBrain(context.Background(), CreateBrainInput{ID: "query-brain", Name: "query"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.CreateBrainVersion(context.Background(), CreateBrainVersionInput{ID: "query-v1", BrainID: b.ID, Version: 1, Rules: payload(t, map[string]any{"rule": 1}), Lineage: payload(t, nil), Provenance: payload(t, map[string]any{"source": "test"}), Payload: payload(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateGame(context.Background(), CreateGameInput{ID: "query-game", BrainVersionID: v.ID, Participants: []Participant{{ID: "query-p", BrainVersionID: v.ID, Payload: payload(t, nil)}}}); err != nil {
		t.Fatal(err)
	}
	if rules, err := s.QueryRules(context.Background(), PageOptions{Limit: 1}); err != nil || len(rules) != 1 {
		t.Fatalf("rules=%#v err=%v", rules, err)
	}
	if provenance, err := s.QueryProvenance(context.Background(), PageOptions{Limit: 1}); err != nil || len(provenance) != 1 {
		t.Fatalf("provenance=%#v err=%v", provenance, err)
	}
	usage, err := s.QueryBrainUsage(context.Background(), v.ID, PageOptions{Limit: 1})
	if err != nil || len(usage) != 1 || usage[0].GameID != "query-game" {
		t.Fatalf("usage=%#v err=%v", usage, err)
	}
}

func BenchmarkSQLiteEventAppend(b *testing.B) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	game, err := s.CreateGame(ctx, CreateGameInput{ID: "benchmark", RulesPayload: payload(b, map[string]any{})})
	if err != nil {
		b.Fatal(err)
	}
	eventPayload := payload(b, map[string]any{"direction": 0})
	sequence, eventHash := game.Sequence, game.EventHash
	for b.Loop() {
		events, err := s.AppendGameEvents(ctx, game.ID, sequence, eventHash, []EventInput{{Type: "worm_moved", Payload: eventPayload}})
		if err != nil {
			b.Fatal(err)
		}
		sequence, eventHash = events[0].Sequence, events[0].Hash
	}
}
