package tournament

import (
	"context"
	"testing"

	"worms.ng/internal/engine"
	"worms.ng/internal/match"
	"worms.ng/internal/store"
)

// This test deliberately crosses the public match/tournament/store boundaries
// using SQLite rather than an in-memory fake repository.
func TestSQLiteBlackBoxResumesActiveRoundWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state := engine.NewClassic([]engine.Worm{{ID: "a"}, {ID: "b"}})
	if err := state.ConfigureControllers([]engine.ControllerKind{engine.ControllerAuto, engine.ControllerAuto}, 17); err != nil {
		t.Fatal(err)
	}
	tm, err := NewTournament(ctx, Config{Store: st, Name: "black-box", State: state, Participants: []Participant{{ID: "a"}, {ID: "b"}}, Rounds: 1, Seed: 23, MaxTurns: 64})
	if err != nil {
		t.Fatal(err)
	}
	gameID := tm.ID() + "-round-0-game"
	m, err := match.NewMatch(ctx, match.Config{Store: st, GameID: gameID, Initial: state, Controllers: []match.Controller{match.NewRandomController(deriveSeed(23, 0)), match.NewRandomController(deriveSeed(23, 0) + 1)}, Seed: deriveSeed(23, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Advance(ctx); err != nil {
		t.Fatal(err)
	}
	payload, err := store.EncodePayload(MatchReport{Round: 0, Seed: deriveSeed(23, 0), GameID: m.GameID(), Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateMatch(ctx, store.CreateMatchInput{ID: tm.ID() + "-round-0", TournamentID: tm.ID(), GameID: m.GameID(), Round: 0, Status: "active", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	before, err := st.QueryMatches(ctx, tm.ID(), store.MatchListOptions{Limit: 10})
	if err != nil || len(before) != 1 {
		t.Fatalf("before rows=%d err=%v", len(before), err)
	}
	report, err := tm.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Matches) != 1 || report.Matches[0].GameID != gameID {
		t.Fatalf("report=%+v", report)
	}
	after, err := st.QueryMatches(ctx, tm.ID(), store.MatchListOptions{Limit: 10})
	if err != nil || len(after) != 1 {
		t.Fatalf("after rows=%d err=%v", len(after), err)
	}
	if after[0].GameID != gameID || after[0].Status == "active" {
		t.Fatalf("active row was not resumed: %+v", after[0])
	}
}
