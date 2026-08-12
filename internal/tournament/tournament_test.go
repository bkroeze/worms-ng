package tournament

import (
	"context"
	"testing"

	"worms.ng/internal/engine"
	"worms.ng/internal/store"
)

func blockedState() engine.State {
	s := engine.New(3, 3, []engine.Worm{
		{ID: "a", Position: engine.Point{Q: 0, R: 0}, Alive: true},
		{ID: "b", Position: engine.Point{Q: 2, R: 2}, Alive: true},
	})
	for i, id := range []string{"a", "b"} {
		p := s.Worms[i].Position
		for d := engine.East; d <= engine.NorthEast; d++ {
			n := s.Neighbor(p, d)
			if n.Q >= 0 && n.Q < s.Width && n.R >= 0 && n.R < s.Height {
				_ = s.InsertTrail(p, n, id)
			}
		}
	}
	return s
}

func TestSeededTournamentStoreResumeMetrics(t *testing.T) {
	ctx := context.Background()
	st, e := store.OpenMemory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	s := blockedState()
	_ = s.ConfigureControllers([]engine.ControllerKind{engine.ControllerAuto, engine.ControllerAuto}, 1)
	tm, e := NewTournament(ctx, Config{Store: st, Name: "t", State: s, Participants: []Participant{{ID: "a"}, {ID: "b"}}, Rounds: 1, Seed: 4, MaxTurns: 64})
	if e != nil {
		t.Fatal(e)
	}
	r, e := tm.Run(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if r.Completed != 1 || len(r.Matches) != 1 || r.Matches[0].Status != "finished" {
		t.Fatalf("report=%+v", r)
	}
	r2, e := tm.Run(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if r2.Completed != 1 || len(r2.Matches) != 1 || r2.Matches[0].GameID != r.Matches[0].GameID {
		t.Fatalf("resume=%+v", r2)
	}

	tm2, e := ResumeTournament(ctx, Config{Store: st, ID: tm.ID(), State: s, Participants: []Participant{{ID: "a"}, {ID: "b"}}, Rounds: 2, Seed: 4, MaxTurns: 64})
	if e != nil {
		t.Fatal(e)
	}
	r3, e := tm2.Run(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if r3.Completed != 2 || len(r3.Matches) != 2 {
		t.Fatalf("partial resume=%+v", r3)
	}
	if r3.Matches[0].GameID != r.Matches[0].GameID || r3.Matches[1].GameID == "" {
		t.Fatalf("resumed match reports=%+v", r3.Matches)
	}
	expectedWins := map[string]int{}
	for _, mr := range r3.Matches {
		for _, winner := range mr.Winners {
			expectedWins[winner]++
		}
	}
	for id, wins := range expectedWins {
		if r3.Wins[id] != wins {
			t.Fatalf("resumed wins=%+v expected=%+v", r3.Wins, expectedWins)
		}
	}
}

func TestTurnCapStoresIncompleteReportWithoutWinner(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := engine.NewClassic([]engine.Worm{{ID: "a"}, {ID: "b"}})
	_ = s.ConfigureControllers([]engine.ControllerKind{engine.ControllerAuto, engine.ControllerAuto}, 2)
	tm, err := NewTournament(ctx, Config{Store: st, Name: "cap", State: s, Participants: []Participant{{ID: "a"}, {ID: "b"}}, Rounds: 1, Seed: 9, MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	r, err := tm.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Completed != 0 || len(r.Matches) != 1 || r.Matches[0].Status != "stopped" || len(r.Matches[0].Winners) != 0 || r.Matches[0].Tie {
		t.Fatalf("capped report=%+v", r)
	}
	r2, err := tm.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Completed != 0 || len(r2.Matches) != 1 || r2.Matches[0].Status != "stopped" {
		t.Fatalf("capped resume=%+v", r2)
	}
}
