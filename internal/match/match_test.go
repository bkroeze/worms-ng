package match

import (
	"context"
	"testing"
	"time"

	"worms.ng/internal/engine"
	"worms.ng/internal/protocol"
	"worms.ng/internal/store"
)

func testState() engine.State {
	return func() engine.State {
		s := engine.NewClassic([]engine.Worm{{ID: "a"}, {ID: "b"}})
		_ = s.ConfigureControllers([]engine.ControllerKind{engine.ControllerNew, engine.ControllerNew}, 7)
		return s
	}()
}
func TestPendingTeachingAndScriptedAdapter(t *testing.T) {
	ctx := context.Background()
	m, e := NewMatch(ctx, Config{Initial: testState()})
	if e != nil {
		t.Fatal(e)
	}
	r, e := m.Advance(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if r.Pending == nil || r.Pending.Request.DecisionID == "" {
		t.Fatalf("pending=%+v", r.Pending)
	}
	if _, e = m.Submit(ctx, protocol.Action{Kind: protocol.ActionMove, Direction: int(engine.East)}); e != nil {
		t.Fatal(e)
	}
	if m.Pending() == nil {
		t.Fatal("next slot should remain pending")
	}
	s := testState()
	c, e := NewScriptedController(protocol.Action{Kind: protocol.ActionMove, Direction: int(engine.East)}, protocol.Action{Kind: protocol.ActionMove, Direction: int(engine.SouthEast)})
	if e != nil {
		t.Fatal(e)
	}
	m, e = NewMatch(ctx, Config{Initial: s, Controllers: []Controller{c, c}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.Advance(ctx); e != nil {
		t.Fatal(e)
	}
	if m.State().Tick == 0 {
		t.Fatal("scripted adapter did not advance")
	}
}
func TestRealStoreResumeVerifiesSnapshot(t *testing.T) {
	ctx := context.Background()
	st, e := store.OpenMemory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	s := testState()
	c, e := NewScriptedController(protocol.Action{Kind: protocol.ActionMove, Direction: int(engine.East)})
	if e != nil {
		t.Fatal(e)
	}
	m, e := NewMatch(ctx, Config{Store: st, Initial: s, Controllers: []Controller{c, nil}, Deadline: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.Advance(ctx); e != nil {
		t.Fatal(e)
	}
	g, e := st.GetGame(ctx, m.GameID())
	if e != nil {
		t.Fatal(e)
	}
	if g.Sequence == 0 {
		t.Fatal("transition was not persisted")
	}
	r, e := ResumeMatch(ctx, Config{Store: st, GameID: m.GameID(), Controllers: []Controller{c, nil}})
	if e != nil {
		t.Fatal(e)
	}
	if r.State().Tick != m.State().Tick {
		t.Fatalf("resume tick=%d want %d", r.State().Tick, m.State().Tick)
	}
}

func TestObservationReportsCanonicalTrailOwnership(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	s := testState()
	from := s.Worms[0].Position
	if err := s.InsertTrail(from, s.Neighbor(from, engine.East), "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTrail(from, s.Neighbor(from, engine.SouthEast), "b"); err != nil {
		t.Fatal(err)
	}
	m, err := NewMatch(ctx, Config{Store: st, Initial: s})
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Advance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Pending == nil {
		t.Fatalf("advance result=%+v", r)
	}
	states := r.Pending.Request.Observation.TrailStates
	if states[int(engine.East)] != protocol.TrailOwn {
		t.Fatalf("east trail state=%q", states[int(engine.East)])
	}
	if states[int(engine.SouthEast)] != protocol.TrailOther {
		t.Fatalf("southeast trail state=%q", states[int(engine.SouthEast)])
	}
}

func BenchmarkMatchAdvance(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		s := engine.NewClassic([]engine.Worm{{ID: "a"}})
		controller, err := NewScriptedController(protocol.Action{Kind: protocol.ActionMove, Direction: int(engine.East)})
		if err != nil {
			b.Fatal(err)
		}
		m, err := NewMatch(ctx, Config{Initial: s, Controllers: []Controller{controller}})
		if err != nil {
			b.Fatal(err)
		}
		if _, err = m.Advance(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
