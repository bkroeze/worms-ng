package match

import (
	"context"
	"testing"

	"worms.ng/internal/engine"
)

func TestSameCopiesLastCompletedSlot(t *testing.T) {
	s := engine.NewClassic([]engine.Worm{{ID: "auto"}, {ID: "same"}})
	if err := engine.ConfigureWorm(&s.Worms[0], engine.ControllerAuto, 1); err != nil {
		t.Fatal(err)
	}
	s.Worms[1].Controller = engine.ControllerSame
	s.Worms[1].Alive = true
	for mask := 0; mask < 64; mask++ {
		a := engine.Action(engine.ActionDie)
		for d := engine.East; d <= engine.NorthEast; d++ {
			if mask&(1<<d) == 0 {
				a = engine.Action(d)
				break
			}
		}
		s.Worms[1].Rules[mask] = a
	}
	s.Worms[1].Position = engine.PointXY(10, 9)
	m, err := NewMatch(context.Background(), Config{Initial: s})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Advance(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := m.State()
	if got.Worms[0].Previous != got.Worms[1].Previous {
		t.Fatalf("same previous=%v auto=%v", got.Worms[1].Previous, got.Worms[0].Previous)
	}
}
