package engine

import (
	"errors"
	"fmt"
)

// NewClassicValidated constructs and validates stable worm identities before
// exposing a state. The legacy constructors remain compatibility APIs; callers
// crossing persistence or network boundaries should use this constructor.
func NewClassicValidated(worms []Worm) (State, error) {
	if err := ValidateWorms(worms); err != nil {
		return State{}, err
	}
	s := NewClassic(worms)
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	return s, nil
}

// NewValidated is the checked constructor for modern bounded states. Unlike
// New, it never clamps dimensions or silently accepts malformed worm setup.
func NewValidated(width, height int, worms []Worm) (State, error) {
	if width < 1 || height < 1 {
		return State{}, fmt.Errorf("%w: dimensions must be positive", ErrInvalidState)
	}
	if err := ValidateWorms(worms); err != nil {
		return State{}, err
	}
	s := New(width, height, worms)
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	return s, nil
}

// ValidateWorms checks IDs and setup ordering without mutating a state.
func ValidateWorms(worms []Worm) error {
	seen := map[string]bool{}
	for _, w := range worms {
		if err := ValidateID(w.ID); err != nil {
			return err
		}
		if seen[w.ID] {
			return errors.New("duplicate worm id")
		}
		seen[w.ID] = true
	}
	return nil
}

// InsertTrail is the only mutation path for external setup/imports. It stores
// exactly one canonical edge and therefore updates both endpoint masks.
func (s *State) InsertTrail(a, b Point, owner string) error {
	if !s.inBounds(a) || !s.inBounds(b) || a == b {
		return errors.New("trail endpoint out of bounds")
	}
	e := NewEdge(a, b)
	found := false
	for d := East; d <= NorthEast; d++ {
		if s.Edge(a, d) == e {
			found = true
			break
		}
	}
	if !found {
		return errors.New("trail endpoints are not adjacent")
	}
	if _, ok := s.Trails[e]; ok {
		return errors.New("trail already occupied")
	}
	s.Trails[e] = owner
	s.Territories[a] = s.Territory(a)
	s.Territories[b] = s.Territory(b)
	return s.ReciprocalOK()
}

// HalfTrailColor derives rendering color from the endpoint territory. The
// reciprocal half may therefore have a different color by design.
func (s State) HalfTrailColor(p Point, d Direction) (Color, bool) {
	if !s.Occupied(p, d) {
		return "", false
	}
	return s.Territory(p).Color, true
}

func (s State) Occupied(p Point, d Direction) bool {
	if !d.Valid() {
		return true
	}
	return s.hasTrail(p, s.Neighbor(p, d))
}
func (s State) ReciprocalOK() error {
	for e := range s.Trails {
		okA, okB := false, false
		for d := East; d <= NorthEast; d++ {
			if s.Edge(e.A, d) == e {
				okA = true
			}
			if s.Edge(e.B, d) == e {
				okB = true
			}
		}
		if !okA || !okB {
			return errors.New("non-reciprocal edge")
		}
	}
	return nil
}

// AdjacentTerritories returns only valid territory IDs for an edge. Bounded
// boundary edges can therefore have zero or one adjacent territory; toroidal
// edges retain the historical two-endpoint fast path.
func (s State) AdjacentTerritories(e Edge) []Point {
	if s.Topology == Toroidal && s.inBounds(e.A) && s.inBounds(e.B) {
		return []Point{e.A, e.B}
	}
	out := make([]Point, 0, 2)
	if s.inBounds(e.A) {
		out = append(out, e.A)
	}
	if s.inBounds(e.B) && e.B != e.A {
		out = append(out, e.B)
	}
	return out
}
func (s State) AllEdges() []Edge {
	out := make([]Edge, 0, s.Width*s.Height*3)
	seen := map[Edge]bool{}
	for _, p := range s.points() {
		for d := East; d <= NorthEast; d++ {
			e := s.Edge(p, d)
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out
}
func (s State) TerritoryCount() int { return s.Width * s.Height }
func (s State) EdgeCount() int      { return len(s.AllEdges()) }
