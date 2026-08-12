package engine

import (
	"bytes"
	"testing"
)

func TestClassicGeometryGolden(t *testing.T) {
	s := NewClassic(nil)
	want := []Point{{10, 9}, {10, 10}, {9, 10}, {8, 9}, {9, 8}, {10, 8}}
	got := s.Neighbors(Point{9, 9})
	for d := range want {
		if got[d] != want[d] {
			t.Fatalf("neighbor %d=%v want %v", d, got[d], want[d])
		}
	}
	wantEven := []Point{{10, 10}, {9, 11}, {8, 11}, {8, 10}, {8, 9}, {9, 9}}
	got = s.Neighbors(Point{9, 10})
	for d := range wantEven {
		if got[d] != wantEven[d] {
			t.Fatalf("even neighbor %d=%v", d, got[d])
		}
	}
	if s.Neighbor(Point{18, 9}, East) != (Point{1, 9}) || s.Neighbor(Point{1, 9}, West) != (Point{18, 9}) {
		t.Fatal("horizontal seam did not wrap")
	}
	if s.Neighbor(Point{9, 18}, SouthEast) != (Point{9, 1}) {
		t.Fatal("vertical seam did not wrap")
	}
	if len(s.points()) != 324 || s.EdgeCount() != 972 {
		t.Fatalf("geometry counts points=%d edges=%d", len(s.points()), s.EdgeCount())
	}
	for _, p := range s.points() {
		for d := East; d <= NorthEast; d++ {
			if s.Neighbor(s.Neighbor(p, d), d.Opposite()) != p {
				t.Fatalf("reciprocity failed p=%v d=%d", p, d)
			}
		}
	}
}
func TestClassicReciprocalMaskAndDoubleCapture(t *testing.T) {
	w := Worm{ID: "red", Color: "red", Alive: true}
	s := NewClassic([]Worm{w})
	a := Point{9, 9}
	b := s.Neighbor(a, East)
	if err := s.InsertTrail(a, s.Neighbor(a, SouthEast), "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTrail(a, s.Neighbor(a, SouthWest), "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTrail(b, s.Neighbor(b, SouthWest), "x"); err != nil {
		t.Fatal(err)
	}
	// Give source and destination five spokes except the move spoke.
	s.Territories[a] = Territory{ID: a, Mask: 0x3e, Color: "blue"}
	s.Territories[b] = Territory{ID: b, Mask: 0x37, Color: "green"}
	initial := s.Snapshot()
	if _, err := s.Step("red", East); err != nil {
		t.Fatal(err)
	}
	replayed, err := Replay(initial, s.EventsCopy())
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if replayed.HashHex() != s.HashHex() {
		t.Fatalf("replay hash %s, want %s", replayed.HashHex(), s.HashHex())
	}
	if s.Territory(a).Mask != 63 || s.Territory(b).Mask != 63 || s.Worms[0].Score != 2 {
		t.Fatalf("source=%#x destination=%#x score=%d", s.Territory(a).Mask, s.Territory(b).Mask, s.Worms[0].Score)
	}
	first, dest := false, false
	for _, e := range s.Events {
		if e.Type == "territory_captured" && !first {
			first = true
		} else if e.Type == "territory_captured" && first {
			dest = true
		}
	}
	if !first || !dest {
		t.Fatal("capture events missing")
	}
}
func TestControllerTableValidationAndSnapshotHash(t *testing.T) {
	w := Worm{ID: "w", Alive: true}
	if err := ConfigureWorm(&w, ControllerWild, 9); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRules(w); err != nil {
		t.Fatal(err)
	}
	r := w.Rules
	r[8] = Action(West)
	nr, err := NormalizeRules(r)
	if err != nil {
		t.Fatal(err)
	}
	if nr[8] != ActionGetNew {
		t.Fatal("unsafe imported entry not normalized")
	}
	s := NewClassic([]Worm{w})
	a, _ := s.MarshalSnapshot()
	copyState, err := UnmarshalSnapshot(a)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := copyState.MarshalSnapshot()
	if !bytes.Equal(a, b) {
		t.Fatalf("snapshot is not canonical")
	}
	if s.HashHex() != copyState.HashHex() {
		t.Fatal("snapshot hash changed")
	}
}

func TestClassicRawMaskEncoding(t *testing.T) {
	for mask := uint8(0); mask < 64; mask++ {
		if got := EncodeHistoricalRawMask(mask | 0xc0); got != mask {
			t.Fatalf("mask %#x encoded as %#x", mask, got)
		}
		decoded, err := DecodeHistoricalRawMask(mask)
		if err != nil || decoded != mask {
			t.Fatalf("mask %#x decoded as %#x/%v", mask, decoded, err)
		}
	}
	if got := RawMaskBits(0x08); got != "001000" {
		t.Fatalf("raw mask bits = %q", got)
	}
	if _, err := DecodeHistoricalRawMask(64); err == nil {
		t.Fatal("out-of-range historical mask accepted")
	}
}

func TestSeededBoardGenerationContracts(t *testing.T) {
	classic, err := GenerateBoard(BoardConfig{Ruleset: ClassicRules, Participants: 4, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range classic.Worms {
		if w.Position != (Point{9, 9}) || w.CRIX != NOMOVE {
			t.Fatalf("classic start %+v crix=%v", w.Position, w.CRIX)
		}
	}
	a, err := GenerateBoard(BoardConfig{Ruleset: ModernRules, Width: 6, Height: 6, Participants: 4, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateBoard(BoardConfig{Ruleset: ModernRules, Width: 6, Height: 6, Participants: 4, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	aj, _ := a.MarshalSnapshot()
	bj, _ := b.MarshalSnapshot()
	if !bytes.Equal(aj, bj) {
		t.Fatal("equal seeded configurations are not byte-equivalent")
	}
	seen := map[Point]bool{}
	for _, w := range a.Worms {
		if seen[w.Position] {
			t.Fatalf("modern start collision at %v", w.Position)
		}
		seen[w.Position] = true
	}
	if _, err := GenerateBoard(BoardConfig{Ruleset: ModernRules, Width: 0, Height: 6, Participants: 1}); err == nil {
		t.Fatal("invalid modern dimensions accepted")
	}
}

func BenchmarkTransitionDefault(b *testing.B) {
	for b.Loop() {
		s := New(12, 12, []Worm{{ID: "bench", Alive: true, Position: Point{Q: 0, R: 0}}})
		_, _ = s.Step("bench", East)
	}
}

func BenchmarkTransitionMaximum18x18(b *testing.B) {
	for b.Loop() {
		s := NewClassic([]Worm{{ID: "bench", Alive: true}})
		_, _ = s.Step("bench", East)
	}
}
