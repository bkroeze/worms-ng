package engine

import "testing"

func TestClassicRoundOrderStallsAtPendingSlot(t *testing.T) {
	ws := []Worm{{ID: "w0", Alive: true}, {ID: "w1", Alive: true}, {ID: "w2", Alive: true}}
	s := NewClassic(ws)
	for i := range s.Worms {
		for m := range s.Worms[i].Rules {
			s.Worms[i].Rules[m] = ActionDie
		}
	}
	s.Worms[0].Rules[0] = Action(East)
	s.Worms[1].Rules[1] = ActionGetNew
	s.Worms[2].Rules[1] = Action(East)
	initial := s.Snapshot()
	if _, err := s.AdvanceRound(); err != nil {
		t.Fatal(err)
	}
	if s.Pending == nil || s.Pending.WormID != "w1" || s.ActiveSlot != 1 {
		t.Fatalf("pending=%#v slot=%d", s.Pending, s.ActiveSlot)
	}
	if s.Worms[2].Position != (Point{9, 9}) {
		t.Fatal("later slot overtook pending input")
	}
	if _, err := s.Submit(SouthEast); err != nil {
		t.Fatal(err)
	}
	if s.Pending != nil {
		t.Fatal("unexpected second pending decision")
	}
	if s.Round != 1 {
		t.Fatalf("round=%d want 1", s.Round)
	}
	if len(s.Trails) != 2 {
		t.Fatalf("trails=%d want 2", len(s.Trails))
	}
	replayed, err := Replay(initial, s.EventsCopy())
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if replayed.HashHex() != s.HashHex() {
		t.Fatalf("replay hash %s, want %s", replayed.HashHex(), s.HashHex())
	}
}
