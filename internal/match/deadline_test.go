package match

import (
	"context"
	"testing"
	"time"

	"worms.ng/internal/engine"
	"worms.ng/internal/protocol"
	"worms.ng/internal/store"
)

type lateController struct {
	clock *time.Time
}

func (c lateController) Decide(_ context.Context, _ protocol.DecisionRequest) (protocol.Action, error) {
	*c.clock = c.clock.Add(2 * time.Second)
	return protocol.Action{Kind: protocol.ActionMove, Direction: int(engine.East)}, nil
}

func TestPendingDeadlineProducesTimeout(t *testing.T) {
	clock := time.Unix(100, 0)
	m, err := NewMatch(context.Background(), Config{Initial: testState(), Now: func() time.Time { return clock }, Deadline: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Advance(context.Background())
	if err != nil || r.Pending == nil {
		t.Fatalf("advance result=%+v err=%v", r, err)
	}
	clock = clock.Add(2 * time.Second)
	r, err = m.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome == nil || r.Outcome.Kind != "timeout" {
		t.Fatalf("outcome=%+v", r.Outcome)
	}
	if m.State().Worms[0].Alive {
		t.Fatal("timed out worm remained alive")
	}
}

func TestControllerReturningAfterDeadlineTimesOutAndPersists(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(100, 0)
	st, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := NewMatch(ctx, Config{
		Store:       st,
		Initial:     testState(),
		Controllers: []Controller{lateController{clock: &clock}, nil},
		Deadline:    time.Second,
		Now:         func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Advance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome == nil || r.Outcome.Kind != protocol.OutcomeTimeout {
		t.Fatalf("late controller outcome=%+v", r.Outcome)
	}
	if m.State().Worms[0].Alive {
		t.Fatal("late controller action was accepted")
	}
	events, err := st.ListEvents(ctx, m.GameID(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Type != "decision_outcome" {
		t.Fatalf("persisted events=%+v", events)
	}
	resumed, err := ResumeMatch(ctx, Config{Store: st, GameID: m.GameID(), Controllers: []Controller{nil, nil}})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State().Worms[0].Alive {
		t.Fatal("resumed state lost timeout")
	}
}

func TestTerminalPersistenceFailureRestoresPendingState(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(100, 0).Add(2 * time.Second)
	st, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMatch(ctx, Config{Store: st, Initial: testState(), Deadline: time.Second, Now: func() time.Time { return clock }})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	clock = time.Unix(100, 0)
	if _, err := m.Advance(ctx); err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()
	clock = clock.Add(2 * time.Second)
	if _, err := m.Resolve(ctx); err == nil {
		t.Fatal("Resolve succeeded after store close")
	}
	if m.Pending() == nil || !m.State().Worms[0].Alive {
		t.Fatalf("failed terminal transition mutated state: pending=%+v state=%+v", m.Pending(), m.State())
	}
}
