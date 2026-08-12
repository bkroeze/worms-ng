package engine

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestStepCanonicalizesEdgeAndAdvancesTick(t *testing.T) {
	state := New(3, 3, []Worm{{ID: "gold", Position: Point{Q: 1, R: 1}, Alive: true}})
	event, err := state.Step("gold", East)
	if err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if event.Edge != NewEdge(Point{Q: 1, R: 1}, Point{Q: 2, R: 1}) {
		t.Fatalf("unexpected edge: %#v", event.Edge)
	}
	if state.Tick != 1 || state.Worms[0].Position != (Point{Q: 2, R: 1}) {
		t.Fatalf("state did not advance: tick=%d position=%#v", state.Tick, state.Worms[0].Position)
	}
	if len(state.Trails) != 1 || state.Trails[event.Edge] != "gold" {
		t.Fatalf("trail not recorded: %#v", state.Trails)
	}
}

func TestStepReturnsRecordedMetadata(t *testing.T) {
	state := New(3, 3, []Worm{{ID: "gold", Position: Point{Q: 1, R: 1}, Alive: true}})
	event, err := state.Step("gold", East)
	if err != nil {
		t.Fatal(err)
	}
	recorded := state.Events[len(state.Events)-1]
	if event != recorded {
		t.Fatalf("Step event %#v differs from recorded event %#v", event, recorded)
	}
	if event.Seq == 0 || event.Tick != 1 || event.Round != 0 {
		t.Fatalf("event metadata not finalized: %#v", event)
	}
}

func TestReplayDerivesDirectionFromMovingWormOrigin(t *testing.T) {
	state := NewClassic([]Worm{{ID: "mover", Alive: true}, {ID: "other", Alive: true}})
	a := Point{Q: 9, R: 9}
	b := state.Neighbor(a, East)
	state.Worms[0].Position = b
	state.Worms[1].Position = a
	initial := state.Snapshot()
	event, err := state.Step("mover", West)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := Replay(initial, []Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Worms[0].Position != a {
		t.Fatalf("replayed mover position %v, want %v", replayed.Worms[0].Position, a)
	}
}

func TestSubmitRejectedDestinationDoesNotTeachRule(t *testing.T) {
	state := New(2, 2, []Worm{
		{ID: "gold", Position: Point{Q: 0, R: 0}, Alive: true},
		{ID: "blue", Position: Point{Q: 1, R: 0}, Alive: true},
	})
	mask := state.Mask(state.Worms[0].Position)
	state.Pending = &Decision{WormID: "gold", Mask: mask, Slot: 0, Request: 1}
	state.ActiveSlot = 0
	beforeRules := state.Worms[0].Rules
	beforeEvents := state.EventsCopy()
	if _, err := state.Submit(East); err == nil {
		t.Fatal("Submit accepted occupied destination")
	}
	if state.Worms[0].Rules != beforeRules || state.Pending == nil || *state.Pending != (Decision{WormID: "gold", Mask: mask, Slot: 0, Request: 1}) || state.ActiveSlot != 0 {
		t.Fatalf("rejected Submit mutated controller state: rules changed=%v pending=%#v slot=%d", state.Worms[0].Rules != beforeRules, state.Pending, state.ActiveSlot)
	}
	if len(state.Events) != len(beforeEvents) {
		t.Fatalf("rejected Submit appended events")
	}
}

func TestModernBlockedWormDiesWithoutLearningNoMove(t *testing.T) {
	state := New(2, 2, []Worm{
		{ID: "gold", Position: Point{Q: 0, R: 0}, Alive: true},
		{ID: "east", Position: Point{Q: 1, R: 0}, Alive: true},
		{ID: "south", Position: Point{Q: 0, R: 1}, Alive: true},
	})
	if err := ConfigureWorm(&state.Worms[0], ControllerAuto, 1); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(state.Worms); i++ {
		for m := range state.Worms[i].Rules {
			state.Worms[i].Rules[m] = ActionDie
		}
	}
	mask := state.Mask(state.Worms[0].Position)
	if len(state.LegalMoves("gold")) != 0 || mask == 63 {
		t.Fatalf("test setup is not destination-blocked: mask=%#x legal=%v", mask, state.LegalMoves("gold"))
	}
	if _, err := state.AdvanceRound(); err != nil {
		t.Fatal(err)
	}
	if state.Worms[0].Alive {
		t.Fatal("blocked modern worm survived")
	}
	if state.Worms[0].Rules[mask] == Action(NOMOVE) {
		t.Fatal("blocked modern worm learned NOMOVE")
	}
}

func TestLegalMovesAreStableAndRejectOccupiedOrUsedEdges(t *testing.T) {
	state := New(2, 2, []Worm{
		{ID: "gold", Position: Point{Q: 0, R: 0}, Alive: true},
		{ID: "blue", Position: Point{Q: 1, R: 0}, Alive: true},
	})
	state.Trails[NewEdge(Point{Q: 0, R: 0}, Point{Q: 0, R: 1})] = "gold"
	moves := state.LegalMoves("gold")
	if len(moves) != 0 {
		t.Fatalf("LegalMoves() = %v, want no legal moves", moves)
	}
	if _, err := state.Step("gold", East); err == nil {
		t.Fatal("Step() accepted an occupied destination")
	}
}
func TestBoundedAdjacencyCardinality(t *testing.T) {
	s := New(1, 1, []Worm{{ID: "w", Alive: true, Position: Point{0, 0}}})
	if got := s.AdjacentTerritories(Edge{A: Point{0, 0}, B: Point{1, 0}}); len(got) != 1 || got[0] != (Point{0, 0}) {
		t.Fatalf("boundary adjacency=%v, want one in-bounds endpoint", got)
	}
	if got := s.AdjacentTerritories(Edge{A: Point{-1, 0}, B: Point{1, 0}}); len(got) != 0 {
		t.Fatalf("exterior adjacency=%v, want zero endpoints", got)
	}
	edges := s.AllEdges()
	if len(edges) != 6 {
		t.Fatalf("bounded 1x1 edges=%d, want six boundary spokes", len(edges))
	}
	for _, edge := range edges {
		if got := s.AdjacentTerritories(edge); len(got) != 1 {
			t.Fatalf("edge %v adjacency=%v, want one in-bounds endpoint", edge, got)
		}
	}
}

func TestValidateDerivesScoresAndOwners(t *testing.T) {
	s := NewClassic([]Worm{{ID: "w", Alive: true, Score: 1}})
	if err := s.Validate(); err == nil {
		t.Fatal("positive score without captures was accepted")
	}
	s.Worms[0].Score = 0
	s.Territories[Point{9, 9}] = Territory{ID: Point{9, 9}, Mask: 63, Owner: "missing"}
	if err := s.Validate(); err == nil {
		t.Fatal("capture owner without worm was accepted")
	}
}

func TestReplayRejectsVersionAndSequenceWithTypedErrors(t *testing.T) {
	s := New(3, 3, []Worm{{ID: "w", Alive: true, Position: Point{1, 1}}})
	ev, err := s.Step("w", East)
	if err != nil {
		t.Fatal(err)
	}
	ev.Version = 999
	var replayErr *ReplayError
	if _, err := Replay(New(3, 3, []Worm{{ID: "w", Alive: true, Position: Point{1, 1}}}), []Event{ev}); !errors.As(err, &replayErr) || replayErr.Kind != "version" {
		t.Fatalf("version error=%T %v", err, err)
	}
	ev.Version = EventVersion
	ev.Seq = 999
	replayErr = nil
	if _, err := Replay(New(3, 3, []Worm{{ID: "w", Alive: true, Position: Point{1, 1}}}), []Event{ev}); !errors.As(err, &replayErr) || replayErr.Kind != "sequence" {
		t.Fatalf("sequence error=%T %v", err, err)
	}
}

func TestSnapshotHashDetectsStateTampering(t *testing.T) {
	s := NewClassic([]Worm{{ID: "w", Alive: true}})
	data, err := s.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["tick"] = float64(99)
	tampered, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var hashErr *SnapshotHashError
	if _, err := UnmarshalSnapshot(tampered); !errors.As(err, &hashErr) {
		t.Fatalf("tampered snapshot error=%T %v", err, err)
	}
}

func TestAdvanceRoundIllegalRuleIsAtomic(t *testing.T) {
	s := NewClassic([]Worm{{ID: "a", Alive: true}, {ID: "b", Alive: true}})
	s.Worms[0].Rules[0] = Action(East)
	s.Worms[1].Rules[0] = Action(-9)
	before := s.HashHex()
	var illegal *IllegalRuleError
	if _, err := s.AdvanceRound(); !errors.As(err, &illegal) {
		t.Fatalf("error=%T %v, want IllegalRuleError", err, err)
	}
	if got := s.HashHex(); got != before {
		t.Fatalf("state mutated on illegal later slot: before=%s after=%s", before, got)
	}
}

func TestAutoUsesCaptureCollisionAndSeededTies(t *testing.T) {
	capture := NewClassic([]Worm{{ID: "a", Alive: true, Position: Point{9, 9}}, {ID: "b", Alive: true, Position: Point{9, 10}}})
	capture.Territories[capture.Worms[0].Position] = Territory{ID: capture.Worms[0].Position, Mask: 62}
	if got := chooseAuto(&capture, 0); got != East {
		t.Fatalf("AUTO capture=%v, want East", got)
	}
	collision := NewClassic([]Worm{{ID: "a", Alive: true, Position: Point{9, 9}}, {ID: "b", Alive: true, Position: Point{10, 9}}})
	if got := chooseAuto(&collision, 0); got == East {
		t.Fatal("AUTO selected occupied collision destination")
	}
	left := New(5, 5, []Worm{{ID: "a", Alive: true, Position: Point{2, 2}}})
	right := left.Snapshot()
	left.Worms[0].BrainSeed = 1
	right.Worms[0].BrainSeed = 2
	if chooseAuto(&left, 0) == chooseAuto(&right, 0) {
		t.Fatal("seeded AUTO ties did not choose an alternative")
	}
}
