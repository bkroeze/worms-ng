package engine

import (
	"errors"
	"fmt"
)

// ConfigureControllers applies the typed setup semantics from the classic
// setup screen. It does not change the shared INITPOS or CRIX metadata.
func (s *State) ConfigureControllers(kinds []ControllerKind, seed uint64) error {
	if len(kinds) != len(s.Worms) {
		return errors.New("controller count does not match worm count")
	}
	before := append([]Worm(nil), s.Worms...)
	for i, k := range kinds {
		if err := ConfigureWorm(&s.Worms[i], k, seed+uint64(i)); err != nil {
			s.Worms = before
			return err
		}
	}
	return nil
}
func ConfigureWorm(w *Worm, kind ControllerKind, seed uint64) error {
	if w == nil {
		return errors.New("nil worm")
	}
	if !kind.Valid() {
		return fmt.Errorf("unknown controller kind %q", kind)
	}
	before := *w
	w.Controller = kind
	w.BrainSeed = seed
	w.BrainVersion = "xorshift-v1"
	if w.BrainID == "" {
		w.BrainID = w.ID + ":brain"
	}
	if w.CRIX == 0 && w.Previous == 0 {
		w.CRIX = NOMOVE
	}
	w.Previous = NOMOVE
	switch kind {
	case ControllerAsleep:
		w.Alive = false
		installForced(&w.Rules)
	case ControllerNew:
		w.Alive, w.Frozen = true, false
		for i := range w.Rules {
			w.Rules[i] = ActionGetNew
			w.RuleUses[i] = 0
		}
		installForced(&w.Rules)
	case ControllerAuto:
		w.Alive, w.Frozen = true, false
		for i := range w.Rules {
			w.Rules[i] = ActionDoAI
			w.RuleUses[i] = 0
		}
		installForced(&w.Rules)
	case ControllerWild:
		w.Alive, w.Frozen = true, true
		fillWild(&w.Rules, seed)
		installForced(&w.Rules)
	case ControllerSame, ControllerNamed:
		w.Alive = true
		installForced(&w.Rules)
	}
	if err := ValidateRules(*w); err != nil {
		*w = before
		return err
	}
	return nil
}
func installForced(r *[64]Action) {
	for d := East; d <= NorthEast; d++ {
		r[63^(1<<d)] = Action(d)
	}
	r[63] = ActionDie
}
func fillWild(r *[64]Action, seed uint64) {
	x := seed
	for m := 0; m < 64; m++ {
		if m == 63 {
			r[m] = ActionDie
			continue
		}
		exits := []Direction{}
		for d := East; d <= NorthEast; d++ {
			if m&(1<<d) == 0 {
				exits = append(exits, d)
			}
		}
		if len(exits) == 0 {
			r[m] = ActionDie
		} else {
			x = xorshift(x)
			r[m] = Action(exits[x%uint64(len(exits))])
		}
	}
}
func xorshift(x uint64) uint64 {
	if x == 0 {
		x = 0x9e3779b97f4a7c15
	}
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	return x
}
func ValidateRules(w Worm) error {
	for m, a := range w.Rules {
		if a >= 0 {
			if a > Action(NorthEast) {
				return fmt.Errorf("mask %#02x has invalid direction", m)
			}
			if m&(1<<a) != 0 {
				return fmt.Errorf("mask %#02x remembers occupied direction", m)
			}
		} else if a != ActionGetNew && a != ActionDoAI && a != ActionDie {
			return fmt.Errorf("mask %#02x has invalid sentinel", m)
		}
	}
	for d := East; d <= NorthEast; d++ {
		if w.Rules[63^(1<<d)] != Action(d) {
			return fmt.Errorf("forced mask %#02x is not direction %d", 63^(1<<d), d)
		}
	}
	if w.Rules[63] != ActionDie {
		return errors.New("fully occupied mask must be DIE")
	}
	return nil
}
func rulesEmpty(r [64]Action) bool {
	for _, a := range r {
		if a != 0 {
			return false
		}
	}
	return true
}

// NormalizeRules is the safety boundary for imported historical tables. It
// downgrades only unsafe directional entries and rejects malformed sentinels.
func NormalizeRules(r [64]Action) ([64]Action, error) {
	for m, a := range r {
		if a >= 0 {
			if a > Action(NorthEast) || m&(1<<a) != 0 {
				r[m] = ActionGetNew
			}
		} else if a != ActionGetNew && a != ActionDoAI && a != ActionDie {
			return r, fmt.Errorf("invalid sentinel at %#02x", m)
		}
	}
	installForced(&r)
	return r, nil
}
func (s *State) NormalizeControllers() error {
	normalized := make([][64]Action, len(s.Worms))
	for i := range s.Worms {
		r, e := NormalizeRules(s.Worms[i].Rules)
		if e != nil {
			return e
		}
		normalized[i] = r
	}
	for i := range s.Worms {
		s.Worms[i].Rules = normalized[i]
	}
	return nil
}
func (s State) Lookup(id string) (Action, error) {
	w, ok := s.worm(id)
	if !ok {
		return 0, errors.New("unknown worm")
	}
	if !w.Alive {
		return ActionDie, nil
	}
	return w.Rules[s.mask(w.Position)&63], nil
}

// AdvanceRound dispatches slots in index order. A NEW/GETNEW decision leaves
// Pending set and does not let a later slot overtake it.
func (s *State) AdvanceRound() (events []Event, err error) {
	if s.Pending != nil {
		return nil, &TransitionError{Reason: RejectPendingDecision}
	}
	if s.GameOver {
		return nil, &TransitionError{Reason: RejectGameOver}
	}
	beforeState := s.Snapshot()
	defer func() {
		if err != nil {
			*s = beforeState
			events = nil
		}
	}()
	start := 0
	if s.ActiveSlot >= 0 {
		start = s.ActiveSlot
	}
	before := len(s.Events)
	for slot := start; slot < len(s.Worms); slot++ {
		s.ActiveSlot = slot
		w := &s.Worms[slot]
		if !w.Alive {
			continue
		}
		m := s.mask(w.Position)
		if len(s.LegalMoves(w.ID)) == 0 {
			s.kill(slot, "worm_blocked")
			continue
		}
		a := w.Rules[m&63]
		if a == ActionGetNew {
			s.Pending = &Decision{WormID: w.ID, Mask: m & 63, Slot: slot, Request: uint64(len(s.Events) + 1), Reason: "unknown_pattern", Frozen: w.Frozen}
			s.emit(Event{Type: "decision_pending", WormID: w.ID, Mask: m & 63, Slot: slot, Request: s.Pending.Request, BrainID: w.BrainID, BrainVersion: w.BrainVersion, Provenance: s.Pending.Reason})
			return append([]Event(nil), s.Events[before:]...), nil
		}
		if a == ActionDoAI {
			a = Action(chooseAuto(s, slot))
			if !w.Frozen {
				w.Rules[m&63] = a
				s.emit(Event{Type: "rule_learned", WormID: w.ID, Slot: slot, RuleMask: m & 63, RuleAction: a, BrainID: w.BrainID, BrainVersion: w.BrainVersion, Provenance: "auto"})
			}
		}
		if a == ActionDie {
			s.kill(slot, "worm_died")
			continue
		}
		if a < 0 || !Direction(a).Valid() || m&(1<<a) != 0 {
			return nil, &IllegalRuleError{WormID: w.ID, Mask: m & 63, Action: a}
		}
		w.RuleUses[m&63]++
		s.emit(Event{Type: "rule_used", WormID: w.ID, Slot: slot, RuleMask: m & 63, RuleAction: a, UseCount: w.RuleUses[m&63], BrainID: w.BrainID, BrainVersion: w.BrainVersion})
		if _, e := s.Step(w.ID, Direction(a)); e != nil {
			return nil, e
		}
	}
	s.ActiveSlot = -1
	s.Round++
	s.emit(Event{Type: "round_completed", Slot: -1})
	if s.AliveCount() == 0 {
		s.GameOver = true
		s.emit(Event{Type: "game_over"})
	}
	return append([]Event(nil), s.Events[before:]...), nil
}
func (s *State) kill(slot int, typ string) {
	s.Worms[slot].Alive = false
	s.Worms[slot].CRIX = NOMOVE
	s.emit(Event{Type: typ, WormID: s.Worms[slot].ID, To: s.Worms[slot].Position})
}
func (s *State) Submit(d Direction) (events []Event, err error) {
	if s.Pending == nil {
		return nil, &TransitionError{Reason: RejectPendingDecision}
	}
	p := *s.Pending
	w := s.Worms[p.Slot]
	if p.Frozen || w.Frozen {
		return nil, &TransitionError{Reason: RejectFrozenUnknown, WormID: w.ID, Dir: d}
	}
	if !d.Valid() {
		return nil, &TransitionError{Reason: RejectInvalidDirection, WormID: w.ID, Dir: d}
	}
	allowed := false
	for _, x := range s.LegalMoves(w.ID) {
		if x == d {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, &TransitionError{Reason: RejectOccupiedSpoke, WormID: w.ID, Dir: d}
	}
	beforeState := s.Snapshot()
	defer func() {
		if err != nil {
			*s = beforeState
			events = nil
		}
	}()
	before := len(s.Events)
	s.Worms[p.Slot].Rules[p.Mask] = Action(d)
	s.emit(Event{Type: "rule_learned", WormID: w.ID, Slot: p.Slot, RuleMask: p.Mask, RuleAction: Action(d), BrainID: w.BrainID, BrainVersion: w.BrainVersion, Provenance: "new"})
	s.Pending = nil
	s.ActiveSlot = p.Slot + 1
	s.Worms[p.Slot].RuleUses[p.Mask]++
	s.emit(Event{Type: "rule_used", WormID: w.ID, Slot: p.Slot, RuleMask: p.Mask, RuleAction: Action(d), UseCount: s.Worms[p.Slot].RuleUses[p.Mask], BrainID: w.BrainID, BrainVersion: w.BrainVersion})
	if _, e := s.Step(w.ID, d); e != nil {
		return nil, e
	}
	return s.advanceAfterSubmit(before)
}
func (s *State) advanceAfterSubmit(before int) ([]Event, error) {
	for slot := s.ActiveSlot; slot < len(s.Worms); slot++ {
		s.ActiveSlot = slot
		w := &s.Worms[slot]
		if !w.Alive {
			continue
		}
		m := s.mask(w.Position)
		if len(s.LegalMoves(w.ID)) == 0 {
			s.kill(slot, "worm_blocked")
			continue
		}
		a := w.Rules[m&63]
		if a == ActionGetNew {
			s.Pending = &Decision{WormID: w.ID, Mask: m & 63, Slot: slot, Request: uint64(len(s.Events) + 1), Reason: "unknown_pattern", Frozen: w.Frozen}
			s.emit(Event{Type: "decision_pending", WormID: w.ID, Mask: m & 63, Slot: slot, Request: s.Pending.Request, BrainID: w.BrainID, BrainVersion: w.BrainVersion, Provenance: s.Pending.Reason})
			return append([]Event(nil), s.Events[before:]...), nil
		}
		if a == ActionDoAI {
			a = Action(chooseAuto(s, slot))
			if !w.Frozen {
				w.Rules[m&63] = a
				s.emit(Event{Type: "rule_learned", WormID: w.ID, Slot: slot, RuleMask: m & 63, RuleAction: a, BrainID: w.BrainID, BrainVersion: w.BrainVersion, Provenance: "auto"})
			}
		}
		if a == ActionDie {
			s.kill(slot, "worm_died")
			continue
		}
		if a < 0 || !Direction(a).Valid() || m&(1<<a) != 0 {
			return nil, &IllegalRuleError{WormID: w.ID, Mask: m & 63, Action: a}
		}
		w.RuleUses[m&63]++
		s.emit(Event{Type: "rule_used", WormID: w.ID, Slot: slot, RuleMask: m & 63, RuleAction: a, UseCount: w.RuleUses[m&63], BrainID: w.BrainID, BrainVersion: w.BrainVersion})
		if _, e := s.Step(w.ID, Direction(a)); e != nil {
			return nil, e
		}
	}
	s.ActiveSlot = -1
	s.Round++
	s.emit(Event{Type: "round_completed", Slot: -1})
	if s.AliveCount() == 0 {
		s.GameOver = true
		s.emit(Event{Type: "game_over"})
	}
	return append([]Event(nil), s.Events[before:]...), nil
}

type autoMoveScore struct {
	capture   int
	collision int
	survival  int
}

func chooseAuto(s *State, slot int) Direction {
	moves := s.LegalMoves(s.Worms[slot].ID)
	if len(moves) == 0 {
		return NOMOVE
	}
	best := autoMoveScore{capture: -1, collision: 1 << 30, survival: -1}
	ties := make([]Direction, 0, len(moves))
	w := s.Worms[slot]
	for _, d := range moves {
		trial := s.Snapshot()
		beforeScore := trial.Worms[slot].Score
		if _, err := trial.Step(w.ID, d); err != nil {
			continue
		}
		collision := 0
		to := s.Neighbor(w.Position, d)
		for i, other := range s.Worms {
			if i != slot && other.Alive && other.Position == to {
				collision++
			}
		}
		score := autoMoveScore{
			capture:   trial.Worms[slot].Score - beforeScore,
			collision: collision,
			survival:  len(trial.LegalMoves(w.ID)),
		}
		if score.capture > best.capture ||
			score.capture == best.capture && score.collision < best.collision ||
			score.capture == best.capture && score.collision == best.collision && score.survival > best.survival {
			best = score
			ties = ties[:0]
			ties = append(ties, d)
		} else if score == best {
			ties = append(ties, d)
		}
	}
	if len(ties) == 0 {
		return moves[0]
	}
	if len(ties) == 1 {
		return ties[0]
	}
	x := w.BrainSeed ^ uint64(s.Round+1)<<32 ^ uint64(s.Tick+1)<<16 ^ uint64(slot+1)
	x = xorshift(x)
	return ties[int(x%uint64(len(ties)))]
}

func firstLegal(s *State, slot int) Direction { return chooseAuto(s, slot) }
