// Package engine implements the deterministic, dependency-free Worms? rules.
//
// Classic is deliberately separate from the bounded safety-oriented constructor:
// Classic follows the historical rules (co-location is allowed and the board wraps),
// while New/ NewBounded retain the small bounded API used by tools and tests.
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Direction is absolute clockwise order: E, SE, SW, W, NW, NE.
type Direction uint8

const (
	East Direction = iota
	SouthEast
	SouthWest
	West
	NorthWest
	NorthEast
	NOMOVE Direction = 6
)

const (
	E  Direction = East
	SE Direction = SouthEast
	SW Direction = SouthWest
	W  Direction = West
	NW Direction = NorthWest
	NE Direction = NorthEast
)

var opposite = [...]Direction{West, NorthWest, NorthEast, East, SouthEast, SouthWest}

func (d Direction) Valid() bool { return d <= NorthEast }
func (d Direction) Opposite() Direction {
	if !d.Valid() {
		return NOMOVE
	}
	return opposite[d]
}
func Opposite(d Direction) Direction { return d.Opposite() }

type Point struct {
	Q int `json:"q"`
	R int `json:"r"`
}

func PointXY(x, y int) Point { return Point{Q: x, R: y} }
func (p Point) X() int       { return p.Q }
func (p Point) Y() int       { return p.R }
func (p Point) Neighbor(d Direction) Point {
	dq := [...]int{1, 0, -1, -1, 0, 1}
	dr := [...]int{0, 1, 1, 0, -1, -1}
	if !d.Valid() {
		return p
	}
	return Point{p.Q + dq[d], p.R + dr[d]}
}
func (p Point) String() string { return fmt.Sprintf("(%d,%d)", p.Q, p.R) }

// Edge is one canonical undirected trail. Endpoints are sorted; direction is
// never used as an identity, so reciprocal observations cannot duplicate it.
type Edge struct {
	A Point `json:"a"`
	B Point `json:"b"`
}

func pointLess(a, b Point) bool { return a.Q < b.Q || a.Q == b.Q && a.R < b.R }
func NewEdge(a, b Point) Edge {
	if pointLess(b, a) {
		a, b = b, a
	}
	return Edge{A: a, B: b}
}

// Controller actions use negative values for historical sentinels.
type Action int8

const (
	ActionGetNew Action = -1
	ActionDoAI   Action = -2
	ActionDie    Action = -3
	GETNEW              = ActionGetNew
	DOAI                = ActionDoAI
	DIE                 = ActionDie
)

type ControllerKind string

const (
	ControllerNew    ControllerKind = "NEW"
	ControllerAuto   ControllerKind = "AUTO"
	ControllerWild   ControllerKind = "WILD"
	ControllerSame   ControllerKind = "SAME"
	ControllerNamed  ControllerKind = "NAMED"
	ControllerAsleep ControllerKind = "-----"
	NEW                             = ControllerNew
	AUTO                            = ControllerAuto
	WILD                            = ControllerWild
	SAME                            = ControllerSame
	NAMED                           = ControllerNamed
)

func (k ControllerKind) Valid() bool {
	switch k {
	case ControllerNew, ControllerAuto, ControllerWild, ControllerSame, ControllerNamed, ControllerAsleep:
		return true
	}
	return false
}

type Color string

// ErrInvalidState identifies malformed imported or constructed state.
var ErrInvalidState = errors.New("invalid engine state")

// RejectReason is stable machine-readable classification for rejected actions.
type RejectReason string

const (
	RejectUnknownWorm      RejectReason = "unknown_worm"
	RejectDeadWorm         RejectReason = "dead_worm"
	RejectInvalidDirection RejectReason = "invalid_direction"
	RejectOccupiedSpoke    RejectReason = "occupied_spoke"
	RejectOutOfBounds      RejectReason = "out_of_bounds"
	RejectOccupiedDest     RejectReason = "occupied_destination"
	RejectExistingTrail    RejectReason = "existing_trail"
	RejectGameOver         RejectReason = "game_over"
	RejectPendingDecision  RejectReason = "pending_decision"
	RejectFrozenUnknown    RejectReason = "frozen_unknown"
)

// TransitionError is returned before mutation for rejected actions.
type TransitionError struct {
	Reason RejectReason
	WormID string
	Dir    Direction
}

func (e *TransitionError) Error() string {
	if e.WormID != "" {
		return fmt.Sprintf("transition rejected (%s) for %s", e.Reason, e.WormID)
	}
	return fmt.Sprintf("transition rejected (%s)", e.Reason)
}

func (e *TransitionError) Is(target error) bool {
	t, ok := target.(*TransitionError)
	return ok && e.Reason == t.Reason
}

// IllegalRuleError reports a remembered action that cannot be applied to the
// current raw-mask pattern. Callers may retrain the slot via Submit after
// replacing it with ActionGetNew.
type IllegalRuleError struct {
	WormID string
	Mask   uint8
	Action Action
}

func (e *IllegalRuleError) Error() string {
	return fmt.Sprintf("illegal remembered rule for %s mask %#x: %d", e.WormID, e.Mask, e.Action)
}

// Worm is the complete deterministic per-slot state. Rules are indexed by the
// raw six-bit absolute mask; no rotation, arrival, color, or occupancy enters it.
type Worm struct {
	ID           string         `json:"id"`
	Color        Color          `json:"color,omitempty"`
	Position     Point          `json:"position"`
	Alive        bool           `json:"alive"`
	Score        int            `json:"score"`
	Previous     Direction      `json:"previous_direction"`
	CRIX         Direction      `json:"crix"`
	Controller   ControllerKind `json:"controller"`
	Rules        [64]Action     `json:"rules"`
	RuleUses     [64]uint32     `json:"rule_uses,omitempty"`
	BrainID      string         `json:"brain_id,omitempty"`
	BrainVersion string         `json:"brain_version,omitempty"`
	BrainSeed    uint64         `json:"brain_seed,omitempty"`
	Frozen       bool           `json:"frozen,omitempty"`
}

func (w Worm) Rule(mask uint8) Action { return w.Rules[mask&63] }

type Topology uint8

const (
	Bounded Topology = iota
	Toroidal
)

type RulesMode uint8

const (
	ModernRules RulesMode = iota
	ClassicRules
)

type Territory struct {
	ID    Point  `json:"id"`
	Mask  uint8  `json:"mask"`
	Color Color  `json:"color,omitempty"`
	Owner string `json:"owner,omitempty"`
}
type Provenance struct {
	Ruleset string `json:"ruleset"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

// State is intentionally value-copyable at the API boundary, while mutating
// methods are explicit. Events are append-only and carry sequence numbers.
type State struct {
	Width       int                 `json:"width"`
	Height      int                 `json:"height"`
	Topology    Topology            `json:"topology"`
	Mode        RulesMode           `json:"mode"`
	Tick        uint64              `json:"tick"`
	Round       uint64              `json:"round"`
	Worms       []Worm              `json:"worms"`
	Trails      map[Edge]string     `json:"trails"`
	Territories map[Point]Territory `json:"territories"`
	Events      []Event             `json:"events"`
	ActiveSlot  int                 `json:"active_slot"`
	Pending     *Decision           `json:"pending,omitempty"`
	GameOver    bool                `json:"game_over"`
	Provenance  Provenance          `json:"provenance"`
}

// New is the compatibility bounded constructor. NewClassic is the historical
// 18x18 torus. NewBounded is an explicit modern safety constructor.
func New(width, height int, worms []Worm) State {
	return newState(width, height, Bounded, ModernRules, worms)
}
func NewBounded(width, height int, worms []Worm) State { return New(width, height, worms) }
func NewClassic(worms []Worm) State                    { return newState(18, 18, Toroidal, ClassicRules, worms) }
func Classic(worms []Worm) State                       { return NewClassic(worms) }
func NewToroidal(width, height int, worms []Worm) State {
	return newState(width, height, Toroidal, ClassicRules, worms)
}
func newState(width, height int, top Topology, mode RulesMode, worms []Worm) State {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	ws := append([]Worm(nil), worms...)
	s := State{Width: width, Height: height, Topology: top, Mode: mode, Worms: ws, Trails: map[Edge]string{}, Territories: map[Point]Territory{}, ActiveSlot: -1, Provenance: Provenance{Ruleset: "classic", Source: "worms-inspired-agent-game.md", Version: "v1"}}
	if mode == ClassicRules {
		s.Provenance.Ruleset = "classic"
		s.initClassicPosition()
	}
	for _, p := range s.points() {
		s.Territories[p] = Territory{ID: p}
	}
	for i := range s.Worms {
		if s.Worms[i].CRIX == 0 && s.Worms[i].Previous == 0 {
			s.Worms[i].CRIX = NOMOVE
		}
		if s.Worms[i].Controller == "" {
			s.Worms[i].Controller = ControllerNew
		}
	}
	return s
}
func (s *State) initClassicPosition() {
	c := Point{9, 9}
	for i := range s.Worms {
		s.Worms[i].Position = c
		s.Worms[i].CRIX = NOMOVE
		s.Worms[i].Previous = NOMOVE
		if s.Worms[i].ID != "" && !s.Worms[i].Alive {
			continue
		}
		s.Worms[i].Alive = true
	}
}
func (s State) points() []Point {
	out := make([]Point, 0, s.Width*s.Height)
	start := 0
	if s.Mode == ClassicRules {
		start = 1
	}
	for y := start; y < start+s.Height; y++ {
		for x := start; x < start+s.Width; x++ {
			out = append(out, Point{x, y})
		}
	}
	return out
}
func (s State) inBounds(p Point) bool {
	start := 0
	if s.Mode == ClassicRules {
		start = 1
	}
	return p.Q >= start && p.Q < start+s.Width && p.R >= start && p.R < start+s.Height
}
func (s State) wrap(p Point) Point {
	start := 0
	if s.Mode == ClassicRules {
		start = 1
	}
	if s.Width > 0 {
		p.Q = ((p.Q-start)%s.Width+s.Width)%s.Width + start
	}
	if s.Height > 0 {
		p.R = ((p.R-start)%s.Height+s.Height)%s.Height + start
	}
	return p
}
func (s State) Neighbor(p Point, d Direction) Point {
	if !d.Valid() {
		return p
	}
	if s.Mode == ClassicRules { // odd-row offset, absolute screen directions
		odd := p.R&1 == 1
		var dq, dr int
		if odd {
			dq = [6]int{1, 1, 0, -1, 0, 1}[d]
			dr = [6]int{0, 1, 1, 0, -1, -1}[d]
		} else {
			dq = [6]int{1, 0, -1, -1, -1, 0}[d]
			dr = [6]int{0, 1, 1, 0, -1, -1}[d]
		}
		q := Point{p.Q + dq, p.R + dr}
		if s.Topology == Toroidal {
			return s.wrap(q)
		}
		return q
	}
	q := p.Neighbor(d)
	if s.Topology == Toroidal {
		return s.wrap(q)
	}
	return q
}
func (s State) Neighbors(p Point) [6]Point {
	var out [6]Point
	for d := East; d <= NorthEast; d++ {
		out[d] = s.Neighbor(p, d)
	}
	return out
}
func (s State) Edge(p Point, d Direction) Edge { return NewEdge(p, s.Neighbor(p, d)) }
func (s State) wormIndex(id string) int {
	for i := range s.Worms {
		if s.Worms[i].ID == id {
			return i
		}
	}
	return -1
}
func (s State) worm(id string) (Worm, bool) {
	i := s.wormIndex(id)
	if i < 0 {
		return Worm{}, false
	}
	return s.Worms[i], true
}
func (s State) hasWorm(p Point) bool {
	for _, w := range s.Worms {
		if w.Alive && w.Position == p {
			return true
		}
	}
	return false
}
func (s State) hasTrail(a, b Point) bool { _, ok := s.Trails[NewEdge(a, b)]; return ok }

func (s State) mask(p Point) uint8 {
	var m uint8
	for d := East; d <= NorthEast; d++ {
		if s.hasTrail(p, s.Neighbor(p, d)) {
			m |= 1 << d
		}
	}
	if t, ok := s.Territories[p]; ok && t.Mask != 0 {
		m |= t.Mask
	}
	return m
}
func (s State) Mask(p Point) uint8 { return s.mask(p) & 63 }
func (s State) Territory(p Point) Territory {
	t := s.Territories[p]
	t.ID = p
	t.Mask = s.mask(p)
	return t
}
func (s State) Colors() map[Point]Color {
	out := map[Point]Color{}
	for p, t := range s.Territories {
		out[p] = t.Color
	}
	return out
}
func (s State) LegalMoves(id string) []Direction {
	w, ok := s.worm(id)
	if !ok || !w.Alive {
		return nil
	}
	m := s.mask(w.Position)
	out := make([]Direction, 0, 6)
	for d := East; d <= NorthEast; d++ {
		if m&(1<<d) != 0 {
			continue
		}
		to := s.Neighbor(w.Position, d)
		if !s.inBounds(to) {
			continue
		}
		if s.Mode == ModernRules && s.hasWorm(to) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// ValidateID enforces stable non-empty serializable identities.
func ValidateID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("id is empty")
	}
	if !utf8.ValidString(id) {
		return errors.New("id is not UTF-8")
	}
	if len(id) > 128 {
		return errors.New("id is too long")
	}
	return nil
}
func (s State) Validate() error {
	if s.Width < 1 || s.Height < 1 {
		return fmt.Errorf("%w: invalid dimensions", ErrInvalidState)
	}
	if s.Mode == ClassicRules && s.Topology == Toroidal && s.Height%2 != 0 {
		return fmt.Errorf("%w: odd torus height", ErrInvalidState)
	}
	if len(s.Worms) > 4 {
		return fmt.Errorf("%w: at most four worm slots", ErrInvalidState)
	}
	seen := map[string]bool{}
	occupied := map[Point]string{}
	for _, w := range s.Worms {
		if err := ValidateID(w.ID); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidState, err)
		}
		if seen[w.ID] {
			return fmt.Errorf("%w: duplicate worm id %q", ErrInvalidState, w.ID)
		}
		seen[w.ID] = true
		if !s.inBounds(w.Position) && w.Alive {
			return fmt.Errorf("%w: worm out of bounds", ErrInvalidState)
		}
		if !w.CRIX.Valid() && w.CRIX != NOMOVE {
			return fmt.Errorf("%w: invalid crix", ErrInvalidState)
		}
		if !w.Previous.Valid() && w.Previous != NOMOVE {
			return fmt.Errorf("%w: invalid previous direction", ErrInvalidState)
		}
		if w.Score < 0 {
			return fmt.Errorf("%w: negative score", ErrInvalidState)
		}
		if s.Mode == ModernRules && w.Alive {
			if prior, ok := occupied[w.Position]; ok {
				return fmt.Errorf("%w: duplicate live occupancy %s/%s", ErrInvalidState, prior, w.ID)
			}
			occupied[w.Position] = w.ID
		}
		if !rulesEmpty(w.Rules) {
			if err := ValidateRules(w); err != nil {
				return fmt.Errorf("%w: worm %s rules: %v", ErrInvalidState, w.ID, err)
			}
		}
	}
	for e := range s.Trails {
		if e.A == e.B || !s.inBounds(e.A) || !s.inBounds(e.B) {
			return fmt.Errorf("%w: invalid edge", ErrInvalidState)
		}
		found := false
		for d := East; d <= NorthEast; d++ {
			if s.Edge(e.A, d) == e {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: non-neighbor edge %v", ErrInvalidState, e)
		}
	}
	expectedScores := make(map[string]int, len(s.Worms))
	for _, w := range s.Worms {
		expectedScores[w.ID] = 0
	}
	for _, t := range s.Territories {
		if t.Mask != 63 {
			continue
		}
		if t.Owner == "" {
			return fmt.Errorf("%w: completed territory has no owner", ErrInvalidState)
		}
		if _, ok := expectedScores[t.Owner]; !ok {
			return fmt.Errorf("%w: completed territory owner %q is not a worm", ErrInvalidState, t.Owner)
		}
		expectedScores[t.Owner]++
	}
	for _, w := range s.Worms {
		if expectedScores[w.ID] != w.Score {
			return fmt.Errorf("%w: score for %s=%d, completed territories=%d", ErrInvalidState, w.ID, w.Score, expectedScores[w.ID])
		}
	}
	return s.ReciprocalOK()
}

// Event is a versioned value record. State.Events is append-only and callers receive copies.
type Event struct {
	Version      int    `json:"version"`
	Seq          uint64 `json:"seq"`
	Type         string `json:"type"`
	WormID       string `json:"worm_id,omitempty"`
	From         Point  `json:"from,omitempty"`
	To           Point  `json:"to,omitempty"`
	Edge         Edge   `json:"edge,omitempty"`
	Territory    Point  `json:"territory,omitempty"`
	Color        Color  `json:"color,omitempty"`
	ScoreDelta   int    `json:"score_delta,omitempty"`
	Tick         uint64 `json:"tick"`
	Round        uint64 `json:"round"`
	Mask         uint8  `json:"mask,omitempty"`
	Slot         int    `json:"slot,omitempty"`
	Request      uint64 `json:"request,omitempty"`
	RuleMask     uint8  `json:"rule_mask,omitempty"`
	RuleAction   Action `json:"rule_action,omitempty"`
	BrainID      string `json:"brain_id,omitempty"`
	BrainVersion string `json:"brain_version,omitempty"`
	Provenance   string `json:"provenance,omitempty"`
	UseCount     uint32 `json:"use_count,omitempty"`
}

const EventVersion = 1

// ReplayErrorKind identifies the rejected event metadata field.
type ReplayErrorKind string

const (
	ReplayVersionMismatch  ReplayErrorKind = "version"
	ReplaySequenceMismatch ReplayErrorKind = "sequence"
)

// ReplayError identifies a version or sequence contract violation before any
// replay event is applied.
type ReplayError struct {
	ExpectedVersion int
	ActualVersion   int
	ExpectedSeq     uint64
	ActualSeq       uint64
	Kind            ReplayErrorKind
}

func (e *ReplayError) Error() string {
	if e.Kind == ReplayVersionMismatch {
		return fmt.Sprintf("event version %d, want %d", e.ActualVersion, e.ExpectedVersion)
	}
	return fmt.Sprintf("event sequence %d, want %d", e.ActualSeq, e.ExpectedSeq)
}

// SnapshotHashError identifies a snapshot whose declared hash does not match
// its canonical state representation.
type SnapshotHashError struct {
	Expected string
	Actual   string
}

func (e *SnapshotHashError) Error() string {
	return fmt.Sprintf("snapshot state hash %q, want %q", e.Actual, e.Expected)
}

func (s State) EventsCopy() []Event { return append([]Event(nil), s.Events...) }
func (s *State) emit(e Event) Event {
	if e.Version == 0 {
		e.Version = EventVersion
	}
	e.Seq = uint64(len(s.Events) + 1)
	e.Tick = s.Tick
	e.Round = s.Round
	s.Events = append(s.Events, e)
	return e
}

// Step applies a legal move atomically. Classic ordering is source trail,
// source capture/recolor, position change, destination recolor/trail/capture.
func (s *State) Step(id string, d Direction) (Event, error) {
	if s.GameOver {
		return Event{}, &TransitionError{Reason: RejectGameOver, WormID: id, Dir: d}
	}
	i := s.wormIndex(id)
	if i < 0 {
		return Event{}, &TransitionError{Reason: RejectUnknownWorm, WormID: id, Dir: d}
	}
	if !s.Worms[i].Alive {
		return Event{}, &TransitionError{Reason: RejectDeadWorm, WormID: id, Dir: d}
	}
	if !d.Valid() {
		return Event{}, &TransitionError{Reason: RejectInvalidDirection, WormID: id, Dir: d}
	}
	from := s.Worms[i].Position
	mask := s.mask(from)
	if mask&(1<<d) != 0 {
		return Event{}, &TransitionError{Reason: RejectOccupiedSpoke, WormID: id, Dir: d}
	}
	to := s.Neighbor(from, d)
	if !s.inBounds(to) {
		return Event{}, &TransitionError{Reason: RejectOutOfBounds, WormID: id, Dir: d}
	}
	if s.Mode == ModernRules && s.hasWorm(to) {
		return Event{}, &TransitionError{Reason: RejectOccupiedDest, WormID: id, Dir: d}
	}
	edge := NewEdge(from, to)
	if _, ok := s.Trails[edge]; ok {
		return Event{}, &TransitionError{Reason: RejectExistingTrail, WormID: id, Dir: d}
	}
	color := s.Worms[i].Color
	if color == "" {
		color = Color(id)
	}
	s.Tick++
	s.Trails[edge] = id
	s.emit(Event{Type: "trail_claimed", WormID: id, From: from, To: to, Edge: edge, Color: color, Mask: s.mask(from)})
	s.recolorCapture(i, from, color)
	s.Worms[i].Position = to
	s.Worms[i].Previous = d
	s.Worms[i].CRIX = d.Opposite()
	s.Trails[edge] = id // reciprocal occupancy is represented by this one canonical record
	moveEvent := Event{Type: "worm_moved", WormID: id, From: from, To: to, Edge: edge, Color: color}
	s.recolorCapture(i, to, color)
	moveEvent = s.emit(moveEvent)
	if s.mask(to) == 63 {
		s.Worms[i].Alive = false
		s.Worms[i].CRIX = NOMOVE
		s.emit(Event{Type: "worm_died", WormID: id, To: to})
	}
	return moveEvent, nil
}
func (s *State) recolorCapture(i int, p Point, color Color) {
	t := s.Territories[p]
	t.ID = p
	t.Mask |= s.maskWithoutTerritory(p)
	if t.Mask == 63 {
		if t.Owner == "" {
			t.Owner = s.Worms[i].ID
			t.Color = color
			s.Worms[i].Score++
			s.Territories[p] = t
			s.emit(Event{Type: "territory_captured", WormID: s.Worms[i].ID, Territory: p, Color: color, ScoreDelta: 1, Mask: 63})
			return
		}
		s.Territories[p] = t
		return
	}
	if t.Owner == "" {
		t.Color = color
		s.emit(Event{Type: "territory_color_changed", WormID: s.Worms[i].ID, Territory: p, Color: color, Mask: t.Mask})
	}
	s.Territories[p] = t
}
func (s State) maskWithoutTerritory(p Point) uint8 {
	var m uint8
	for d := East; d <= NorthEast; d++ {
		if s.hasTrail(p, s.Neighbor(p, d)) {
			m |= 1 << d
		}
	}
	return m
}

// Decision is a persistent synchronous input stall for NEW/GETNEW patterns.
type Decision struct {
	WormID  string `json:"worm_id"`
	Mask    uint8  `json:"mask"`
	Slot    int    `json:"slot"`
	Request uint64 `json:"request"`
	Reason  string `json:"reason,omitempty"`
	Frozen  bool   `json:"frozen,omitempty"`
}
type Observation struct {
	Version  int    `json:"version"`
	WormID   string `json:"worm_id"`
	Position Point  `json:"position"`
	// RawMask is the exact six-bit absolute historical key. OccupiedMask and
	// Incoming are UI/controller metadata and never affect this key.
	RawMask              uint8       `json:"raw_mask"`
	Mask                 uint8       `json:"mask"`
	OccupiedMask         uint8       `json:"occupied_mask"`
	Incoming             Direction   `json:"incoming"`
	Legal                []Direction `json:"legal"`
	Scores               []int       `json:"scores"`
	LocalTerritoryCounts [6]uint8    `json:"local_territory_counts"`
	Pending              bool        `json:"pending"`
}

func (s State) Observe(id string) (Observation, error) {
	w, ok := s.worm(id)
	if !ok {
		return Observation{}, errors.New("unknown worm")
	}
	raw := s.mask(w.Position) & 63
	o := Observation{Version: 1, WormID: id, Position: w.Position, RawMask: raw, Mask: raw, Incoming: w.CRIX, Legal: s.LegalMoves(id), Pending: s.Pending != nil}
	for d := East; d <= NorthEast; d++ {
		neighbor := s.Neighbor(w.Position, d)
		if s.hasWorm(neighbor) {
			o.OccupiedMask |= 1 << d
		}
		o.LocalTerritoryCounts[d] = bitCount(s.Territory(neighbor).Mask)
	}
	o.Scores = make([]int, len(s.Worms))
	for i, x := range s.Worms {
		o.Scores[i] = x.Score
	}
	return o, nil
}
func bitCount(m uint8) uint8 {
	var n uint8
	for m != 0 {
		n += m & 1
		m >>= 1
	}
	return n
}

// RawMaskBits renders the historical key in direction order d5..d0.
func RawMaskBits(mask uint8) string {
	mask &= 63
	var b [6]byte
	for i := 0; i < 6; i++ {
		if mask&(1<<uint(i)) != 0 {
			b[5-i] = '1'
		} else {
			b[5-i] = '0'
		}
	}
	return string(b[:])
}

func EncodeHistoricalRawMask(mask uint8) uint8 { return mask & 63 }
func DecodeHistoricalRawMask(v uint8) (uint8, error) {
	if v > 63 {
		return 0, fmt.Errorf("raw mask %#x out of range", v)
	}
	return v, nil
}

func EncodeLocalObservation(mask, occupied uint8, incoming Direction) uint32 {
	if incoming > NOMOVE {
		incoming = NOMOVE
	}
	return uint32(mask&63) | uint32(occupied&63)<<6 | uint32(incoming&7)<<12
}
func (s State) Winners() []string {
	best := -1
	for _, w := range s.Worms {
		if w.Score > best {
			best = w.Score
		}
	}
	var out []string
	for _, w := range s.Worms {
		if w.Score == best {
			out = append(out, w.ID)
		}
	}
	return out
}
func (s State) Tied() bool { return len(s.Winners()) > 1 }
func (s State) AliveCount() int {
	n := 0
	for _, w := range s.Worms {
		if w.Alive {
			n++
		}
	}
	return n
}

// canonicalSnapshot avoids map iteration order in hashes and replay records.
type snapshot struct {
	Version     int           `json:"version"`
	Width       int           `json:"width"`
	Height      int           `json:"height"`
	Topology    Topology      `json:"topology"`
	Mode        RulesMode     `json:"mode"`
	Tick        uint64        `json:"tick"`
	Round       uint64        `json:"round"`
	Worms       []Worm        `json:"worms"`
	Trails      []trailRecord `json:"trails"`
	Territories []Territory   `json:"territories"`
	Events      []Event       `json:"events"`
	EventCursor uint64        `json:"event_cursor"`
	StateHash   string        `json:"state_hash"`
	ActiveSlot  int           `json:"active_slot"`
	Pending     *Decision     `json:"pending,omitempty"`
	GameOver    bool          `json:"game_over"`
	Provenance  Provenance    `json:"provenance"`
}
type trailRecord struct {
	Edge  Edge   `json:"edge"`
	Owner string `json:"owner"`
}

func (s State) canonical() snapshot {
	t := make([]trailRecord, 0, len(s.Trails))
	for e, o := range s.Trails {
		t = append(t, trailRecord{e, o})
	}
	sort.Slice(t, func(i, j int) bool {
		if t[i].Edge.A != t[j].Edge.A {
			return pointLess(t[i].Edge.A, t[j].Edge.A)
		}
		return pointLess(t[i].Edge.B, t[j].Edge.B)
	})
	ts := make([]Territory, 0, len(s.Territories))
	for _, x := range s.Territories {
		ts = append(ts, x)
	}
	sort.Slice(ts, func(i, j int) bool { return pointLess(ts[i].ID, ts[j].ID) })
	return snapshot{
		Version: 1, Width: s.Width, Height: s.Height, Topology: s.Topology, Mode: s.Mode,
		Tick: s.Tick, Round: s.Round, Worms: append([]Worm(nil), s.Worms...), Trails: t,
		Territories: ts, Events: append([]Event(nil), s.Events...), EventCursor: uint64(len(s.Events)),
		ActiveSlot: s.ActiveSlot, Pending: s.Pending, GameOver: s.GameOver, Provenance: s.Provenance,
	}
}
func canonicalSnapshotHash(x snapshot) (string, error) {
	x.StateHash = ""
	b, err := json.Marshal(x)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func (s State) MarshalSnapshot() ([]byte, error) {
	x := s.canonical()
	hash, err := canonicalSnapshotHash(x)
	if err != nil {
		return nil, err
	}
	x.StateHash = hash
	return json.Marshal(x)
}
func (s State) StateHash() [32]byte {
	x := s.canonical()
	b, _ := json.Marshal(x)
	return sha256.Sum256(b)
}
func (s State) HashHex() string   { h := s.StateHash(); return hex.EncodeToString(h[:]) }
func SnapshotHash(s State) string { return s.HashHex() }
func (s State) Snapshot() State {
	out := s
	out.Worms = append([]Worm(nil), s.Worms...)
	out.Trails = map[Edge]string{}
	for e, v := range s.Trails {
		out.Trails[e] = v
	}
	out.Territories = map[Point]Territory{}
	for p, v := range s.Territories {
		out.Territories[p] = v
	}
	out.Events = append([]Event(nil), s.Events...)
	if s.Pending != nil {
		x := *s.Pending
		out.Pending = &x
	}
	return out
}

func UnmarshalSnapshot(data []byte) (State, error) {
	var x snapshot
	if err := json.Unmarshal(data, &x); err != nil {
		return State{}, err
	}
	if x.Version != 1 {
		return State{}, errors.New("unsupported snapshot version")
	}
	declaredHash := x.StateHash
	expectedHash, err := canonicalSnapshotHash(x)
	if err != nil {
		return State{}, err
	}
	if declaredHash != expectedHash {
		return State{}, &SnapshotHashError{Expected: expectedHash, Actual: declaredHash}
	}
	s := newState(x.Width, x.Height, x.Topology, x.Mode, nil)
	s.Worms = append([]Worm(nil), x.Worms...)
	s.Tick = x.Tick
	s.Round = x.Round
	s.Trails = map[Edge]string{}
	for _, r := range x.Trails {
		s.Trails[NewEdge(r.Edge.A, r.Edge.B)] = r.Owner
	}
	s.Territories = map[Point]Territory{}
	for _, t := range x.Territories {
		s.Territories[t.ID] = t
	}
	s.Events = append([]Event(nil), x.Events...)
	s.ActiveSlot = x.ActiveSlot
	s.Pending = x.Pending
	s.GameOver = x.GameOver
	s.Provenance = x.Provenance
	if x.EventCursor != uint64(len(x.Events)) {
		return State{}, fmt.Errorf("snapshot event cursor %d, want %d", x.EventCursor, len(x.Events))
	}
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	return s, nil
}
func Replay(initial State, events []Event) (State, error) {
	s := initial.Snapshot()
	if len(events) == 0 {
		return s, nil
	}
	legacy := true
	for _, e := range events {
		if e.Type != "worm_moved" {
			legacy = false
			break
		}
	}
	probe := s.Snapshot()
	baseSeq := uint64(len(s.Events))
	for n, e := range events {
		expectedSeq := baseSeq + uint64(n) + 1
		if e.Version != 0 && e.Version != EventVersion {
			return State{}, &ReplayError{Kind: "version", ExpectedVersion: EventVersion, ActualVersion: e.Version, ExpectedSeq: expectedSeq, ActualSeq: e.Seq}
		}
		if legacy {
			d, err := directionForEvent(probe, e)
			if err == nil {
				generated, stepErr := probe.Step(e.WormID, d)
				if stepErr == nil && e.Seq != expectedSeq && e.Seq != generated.Seq {
					return State{}, &ReplayError{Kind: "sequence", ExpectedVersion: EventVersion, ActualVersion: e.Version, ExpectedSeq: expectedSeq, ActualSeq: e.Seq}
				}
				if stepErr == nil {
					continue
				}
			}
		}
		if e.Seq != expectedSeq {
			return State{}, &ReplayError{Kind: "sequence", ExpectedVersion: EventVersion, ActualVersion: e.Version, ExpectedSeq: expectedSeq, ActualSeq: e.Seq}
		}
	}
	if legacy {
		// Older persistence streams contain only worm_moved records. Keep
		// accepting those streams, while deriving each move from its own
		// recorded origin rather than from unrelated worms.
		for _, e := range events {
			d, err := directionForEvent(s, e)
			if err != nil {
				return State{}, err
			}
			if _, err := s.Step(e.WormID, d); err != nil {
				return State{}, err
			}
		}
		return s, nil
	}
	for _, e := range events {
		if err := applyReplayEvent(&s, e); err != nil {
			return State{}, err
		}
		s.Events = append(s.Events, e)
	}
	return s, nil
}

func applyReplayEvent(s *State, e Event) error {
	if e.Type == "trail_claimed" {
		if e.Edge != NewEdge(e.From, e.To) {
			return errors.New("trail event has invalid edge")
		}
		if e.Tick != s.Tick+1 {
			return fmt.Errorf("trail event tick %d, want %d", e.Tick, s.Tick+1)
		}
		s.Tick++
		if s.Trails == nil {
			s.Trails = map[Edge]string{}
		}
		if _, exists := s.Trails[e.Edge]; exists {
			return errors.New("trail event repeats an existing edge")
		}
		s.Trails[e.Edge] = e.WormID
		return nil
	}
	if e.Tick != s.Tick {
		return fmt.Errorf("%s event tick %d, want %d", e.Type, e.Tick, s.Tick)
	}
	switch e.Type {
	case "territory_color_changed":
		t := s.Territories[e.Territory]
		t.ID = e.Territory
		t.Mask = e.Mask
		if t.Owner == "" {
			t.Color = e.Color
		}
		s.Territories[e.Territory] = t
	case "territory_captured":
		i := s.wormIndex(e.WormID)
		if i < 0 {
			return fmt.Errorf("unknown worm %q in capture event", e.WormID)
		}
		t := s.Territories[e.Territory]
		t.ID = e.Territory
		t.Mask = e.Mask
		t.Owner = e.WormID
		t.Color = e.Color
		s.Territories[e.Territory] = t
		s.Worms[i].Score += e.ScoreDelta
	case "worm_moved":
		i := s.wormIndex(e.WormID)
		if i < 0 || !s.Worms[i].Alive {
			return fmt.Errorf("worm %q is not alive for move event", e.WormID)
		}
		d, err := directionForEvent(*s, e)
		if err != nil {
			return err
		}
		s.Worms[i].Position = e.To
		s.Worms[i].Previous = d
		s.Worms[i].CRIX = d.Opposite()
	case "worm_died", "worm_blocked":
		i := s.wormIndex(e.WormID)
		if i < 0 {
			return fmt.Errorf("unknown worm %q in death event", e.WormID)
		}
		if e.To != s.Worms[i].Position {
			return fmt.Errorf("death event origin for %q does not match position", e.WormID)
		}
		s.Worms[i].Alive = false
		s.Worms[i].CRIX = NOMOVE
	case "rule_learned":
		i := s.wormIndex(e.WormID)
		if i < 0 {
			return fmt.Errorf("unknown worm %q in rule event", e.WormID)
		}
		s.Worms[i].Rules[e.RuleMask&63] = e.RuleAction
		if s.Pending != nil && s.Pending.WormID == e.WormID && s.Pending.Mask == e.RuleMask&63 {
			s.ActiveSlot = s.Pending.Slot + 1
			s.Pending = nil
		}
	case "rule_used":
		i := s.wormIndex(e.WormID)
		if i < 0 {
			return fmt.Errorf("unknown worm %q in use event", e.WormID)
		}
		s.Worms[i].RuleUses[e.RuleMask&63] = e.UseCount
	case "decision_pending":
		i := s.wormIndex(e.WormID)
		if i < 0 || e.Slot != i {
			return fmt.Errorf("invalid pending slot for %q", e.WormID)
		}
		s.Pending = &Decision{WormID: e.WormID, Mask: e.Mask & 63, Slot: e.Slot, Request: e.Request, Reason: e.Provenance, Frozen: s.Worms[i].Frozen}
		s.ActiveSlot = e.Slot
	case "round_completed":
		if e.Round != s.Round+1 {
			return fmt.Errorf("round event %d, want %d", e.Round, s.Round+1)
		}
		s.Round = e.Round
		s.ActiveSlot = -1
		if s.Pending != nil {
			return errors.New("round completed with pending decision")
		}
	case "game_over":
		s.GameOver = true
	default:
		return fmt.Errorf("unsupported replay event %q", e.Type)
	}
	return nil
}

func directionForEvent(s State, e Event) (Direction, error) {
	i := s.wormIndex(e.WormID)
	if i < 0 {
		return NOMOVE, fmt.Errorf("unknown worm %q in move event", e.WormID)
	}
	if s.Worms[i].Position != e.From {
		return NOMOVE, fmt.Errorf("worm %q is at %v, event starts at %v", e.WormID, s.Worms[i].Position, e.From)
	}
	if e.Edge != NewEdge(e.From, e.To) {
		return NOMOVE, errors.New("move event has invalid edge")
	}
	for d := East; d <= NorthEast; d++ {
		if s.Neighbor(e.From, d) == e.To {
			return d, nil
		}
	}
	return NOMOVE, errors.New("move event endpoints are not neighbors")
}
