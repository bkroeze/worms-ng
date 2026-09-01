package ui

import (
	"testing"

	"gioui.org/f32"
)

func TestDirectionTeachingAndSpeedBounds(t *testing.T) {
	if d, ok := DirectionFromKey("ArrowDown"); !ok || d != SouthEast {
		t.Fatalf("arrow down = %v,%v", d, ok)
	}
	if d := DirectionFromPointer(f32.Pt(0, 0), f32.Pt(0, 1)); d != SouthEast {
		t.Fatalf("pointer down = %v", d)
	}
	m := NewModel()
	m.SetSpeed(99)
	if m.Snapshot().HUD.Speed != 9 {
		t.Fatal("speed upper bound missing")
	}
	m.SetSpeed(0)
	if m.Snapshot().HUD.Speed != 1 {
		t.Fatal("speed lower bound missing")
	}
}
func TestRankedScoresReportsTiesDeterministically(t *testing.T) {
	got, tie := RankedScores([]WormView{{ID: "b", Score: 4}, {ID: "a", Score: 4}, {ID: "c", Score: 1}})
	if !tie || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("scores=%+v tie=%v", got, tie)
	}
}
func TestBoardFromStateUsesAuthoritativeActiveWorm(t *testing.T) {
	st := GameState{Width: 4, Height: 4, Tick: 7, ActiveSlot: 1, Worms: []StateWorm{
		{ID: "one", Position: StatePoint{Q: 1, R: 1}, Alive: true},
		{ID: "two", Position: StatePoint{Q: 2, R: 2}, Alive: true},
	}, Territories: []StateTerritory{{ID: StatePoint{Q: 1, R: 1}, Owner: "one"}}}
	b := BoardFromState(st)
	if b.ActiveWorm != "two" || b.Worms[1].Position != (Point{X: 2, Y: 2}) || b.Territory == nil {
		t.Fatalf("board=%+v", b)
	}
}

func TestBoardFromStateSkipsEmptyTerritoriesAndKeepsHalfColors(t *testing.T) {
	st := GameState{
		Width: 2, Height: 1,
		Territories: []StateTerritory{
			{ID: StatePoint{Q: 0, R: 0}, Color: "red"},
			{ID: StatePoint{Q: 1, R: 0}, Color: "blue"},
			{ID: StatePoint{Q: 9, R: 9}},
		},
		Trails: []StateTrail{{Owner: "red"}},
	}
	st.Trails[0].Edge.A = StatePoint{Q: 0, R: 0}
	st.Trails[0].Edge.B = StatePoint{Q: 1, R: 0}
	b := BoardFromState(st)
	if len(b.Territory) != 2 {
		t.Fatalf("territories=%v, want only colored endpoints", b.Territory)
	}
	if len(b.Trails) != 1 || b.Trails[0].AColor == b.Trails[0].BColor {
		t.Fatalf("trail endpoint colors collapsed: %+v", b.Trails)
	}
	if b.Territory[Point{X: 0, Y: 0}] == b.Trails[0].AColor || b.Territory[Point{X: 1, Y: 0}] == b.Trails[0].BColor {
		t.Fatalf("territory background matches trail color: territory=%v trails=%+v", b.Territory, b.Trails)
	}
	if b.Trails[0].AColor&0xff != 0xff || b.Trails[0].BColor&0xff != 0xff {
		t.Fatalf("colors are not opaque RGBA: %+v", b.Trails)
	}
}
