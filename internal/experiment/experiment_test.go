package experiment

import (
	"context"
	"encoding/json"
	"testing"

	"worms.ng/internal/engine"
	"worms.ng/internal/sharing"
	"worms.ng/internal/store"
	"worms.ng/internal/tournament"
)

func classicExperimentState() engine.State {
	return engine.NewClassic([]engine.Worm{
		{ID: "a", Alive: true, Position: engine.Point{Q: 5, R: 5}},
		{ID: "b", Alive: true, Position: engine.Point{Q: 12, R: 12}},
	})
}

func fakeConfig(s *store.Store, calls *int) Config {
	return Config{
		Name: "classic-baseline", Seed: 71, State: classicExperimentState(), Store: s,
		Participants: []Participant{{ID: "a", Color: "red"}, {ID: "b", Color: "blue"}},
		Policies:     []PolicyDefinition{{Name: sharing.NoSharing}}, Rounds: 2,
		MaxTurns: 1,
		Runner: RunnerFunc(func(_ context.Context, r MatchRequest) (MatchResult, error) {
			*calls++
			return MatchResult{Status: "finished", Survived: true, Scores: map[string]int{"a": 2, "b": 0}, ReplayID: "replay-" + r.MatchID}, nil
		}),
	}
}

func TestSQLiteDeterministicResumeNoDuplicateMatches(t *testing.T) {
	ctx := context.Background()
	s, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	}()
	calls := 0
	cfg := fakeConfig(s, &calls)
	first, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("runner calls=%d want 2", calls)
	}
	second, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("resume reran matches: calls=%d", calls)
	}
	if first.DefinitionHash != second.DefinitionHash {
		t.Fatal("definition hash changed on resume")
	}
	if got, err := s.ListMatches(ctx, first.Policies[sharing.NoSharing].TournamentIDs[0], store.BrainListOptions{}); err != nil || len(got) != 1 {
		t.Fatalf("stored matches=%d err=%v", len(got), err)
	}
	var tournaments, matches int
	if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM tournaments").Scan(&tournaments); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM tournament_matches").Scan(&matches); err != nil {
		t.Fatal(err)
	}
	if tournaments != 2 || matches != 2 {
		t.Fatalf("tournaments=%d matches=%d want 2/2", tournaments, matches)
	}
	if first.Comparison.Policies[sharing.NoSharing] != second.Comparison.Policies[sharing.NoSharing] {
		t.Fatal("comparison changed on resume")
	}
}

func TestClassicBaselineUsesRealSQLiteTournamentRunner(t *testing.T) {
	ctx := context.Background()
	s, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	}()
	cfg := Config{
		Name: "classic", Seed: 3, State: classicExperimentState(), Store: s,
		Participants: []Participant{{ID: "a"}, {ID: "b"}},
		Policies:     []PolicyDefinition{{Name: sharing.NoSharing}}, Rounds: 1, MaxTurns: 1,
	}
	report, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := report.Policies[sharing.NoSharing].Matches
	if len(m) != 1 || m[0].ReplayID == "" {
		t.Fatalf("missing replay result: %#v", m)
	}
	resumed, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Policies[sharing.NoSharing].Matches) != 1 {
		t.Fatalf("resume matches=%d", len(resumed.Policies[sharing.NoSharing].Matches))
	}
	var count int
	if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM tournament_matches").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate classic matches=%d", count)
	}
	var payload []byte
	if err := s.DB().QueryRowContext(ctx, "SELECT payload FROM tournament_matches LIMIT 1").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]json.RawMessage
	if err := store.DecodePayload(payload, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata["experiment_result"]) == 0 || len(metadata["replay_id"]) == 0 {
		t.Fatal("match metadata omitted result/replay")
	}
}

func sqliteBrainVersion(t *testing.T, s *store.Store, brainID, versionID string) store.BrainVersion {
	t.Helper()
	ctx := context.Background()
	rules, err := store.EncodePayload([]int{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	lineage, err := store.EncodePayload(map[string]any{"root": true})
	if err != nil {
		t.Fatal(err)
	}
	prov, err := store.EncodePayload(map[string]any{"source": brainID})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := store.EncodePayload(map[string]any{"brain": brainID})
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.CreateBrainVersion(ctx, store.CreateBrainVersionInput{
		ID: versionID, BrainID: brainID, Version: 1,
		Rules: rules, Lineage: lineage, Provenance: prov, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSQLiteFreezeUsesImmutableBrainVersionIDs(t *testing.T) {
	ctx := context.Background()
	s, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	}()
	if _, err := s.CreateBrain(ctx, store.CreateBrainInput{ID: "brain-a", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBrain(ctx, store.CreateBrainInput{ID: "brain-b", Name: "b"}); err != nil {
		t.Fatal(err)
	}
	v1 := sqliteBrainVersion(t, s, "brain-a", "v1")
	v2 := sqliteBrainVersion(t, s, "brain-b", "v2")
	calls := 0
	var frozen map[string]string
	cfg := Config{
		ID: "freeze-regression", Name: "freeze", Seed: 9, State: classicExperimentState(), Store: s,
		Participants: []Participant{{ID: "a", BrainVersionID: v1.ID}, {ID: "b", BrainVersionID: v2.ID}},
		Policies:     []PolicyDefinition{{Name: sharing.NoSharing}}, Rounds: 1, MaxTurns: 1,
		Runner: RunnerFunc(func(_ context.Context, req MatchRequest) (MatchResult, error) {
			calls++
			frozen = req.FrozenBrainHashes
			return MatchResult{Status: "finished", ReplayID: "replay"}, nil
		}),
	}
	report, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || frozen[v1.ID] != v1.Hash || frozen[v2.ID] != v2.Hash || frozen["a"] != "" {
		t.Fatalf("frozen hashes=%v calls=%d", frozen, calls)
	}
	if report.Policies[sharing.NoSharing].BrainHashes["a"] != v1.Hash {
		t.Fatal("report hash is not participant keyed")
	}
	if _, err := Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("terminal resume reran runner: %d", calls)
	}
}

func TestSQLiteActiveExperimentMatchResumesRunner(t *testing.T) {
	ctx := context.Background()
	s, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	}()
	calls := 0
	cfg := Config{
		ID: "active-regression", Name: "active", Seed: 11, State: classicExperimentState(), Store: s,
		Participants: []Participant{{ID: "a"}, {ID: "b"}},
		Policies:     []PolicyDefinition{{Name: sharing.NoSharing}}, Rounds: 1, MaxTurns: 1,
		Runner: RunnerFunc(func(_ context.Context, req MatchRequest) (MatchResult, error) {
			calls++
			return MatchResult{Status: "finished", ReplayID: "resumed-" + req.MatchID}, nil
		}),
	}
	state, err := cfg.stateFor(cfg.Seed)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := canonicalDefinition(cfg, state)
	if err != nil {
		t.Fatal(err)
	}
	tid := "tournament-" + hash([]byte(cfg.ID + "\x00" + string(sharing.NoSharing) + "\x000"))[:24]
	mid := tid + "-round-0"
	if err := ensureTournament(ctx, s, tid, cfg.Name, definitionPayload(hash(definition), definition, sharing.NoSharing, Schedule(cfg.Seed, 1, state, cfg.Participants)[0])); err != nil {
		t.Fatal(err)
	}
	active, err := store.EncodePayload(tournament.MatchReport{GameID: "active-game", Status: "active", ReplayLink: "active-game"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMatch(ctx, store.CreateMatchInput{ID: mid, TournamentID: tid, Round: 0, Status: "active", Payload: active}); err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	matches := report.Policies[sharing.NoSharing].Matches
	if calls != 1 || len(matches) != 1 || matches[0].Status != "finished" {
		t.Fatalf("active report=%+v calls=%d", matches, calls)
	}
	var status string
	if err := s.DB().QueryRowContext(ctx, "SELECT status FROM tournament_matches WHERE id=?", mid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "finished" {
		t.Fatalf("persisted active status=%q", status)
	}
}

func TestSQLiteDerivedIDsAreRecipientScoped(t *testing.T) {
	ctx := context.Background()
	s, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	}()
	if _, err := s.CreateBrain(ctx, store.CreateBrainInput{ID: "brain-one", Name: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBrain(ctx, store.CreateBrainInput{ID: "brain-two", Name: "two"}); err != nil {
		t.Fatal(err)
	}
	v1 := sqliteBrainVersion(t, s, "brain-one", "one-v1")
	v2 := sqliteBrainVersion(t, s, "brain-two", "two-v1")
	cfg := sharing.Config{Policy: sharing.NoSharing}
	cfg.Sources = []sharing.Source{{WormID: "a", BrainVersionID: v1.ID}}
	first, err := DeriveBrains(ctx, s, cfg, []Participant{{ID: "a", BrainVersionID: v1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Sources = []sharing.Source{{WormID: "a", BrainVersionID: v2.ID}}
	second, err := DeriveBrains(ctx, s, cfg, []Participant{{ID: "a", BrainVersionID: v2.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].BrainVersionID == second[0].BrainVersionID {
		t.Fatalf("derived IDs collided: first=%+v second=%+v", first, second)
	}
	got1, err := s.GetBrainVersion(ctx, first[0].BrainVersionID)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetBrainVersion(ctx, second[0].BrainVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if got1.BrainID != v1.BrainID || got2.BrainID != v2.BrainID {
		t.Fatalf("derived lineage crossed recipients: %q/%q", got1.BrainID, got2.BrainID)
	}
}
