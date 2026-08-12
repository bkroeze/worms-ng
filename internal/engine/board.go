package engine

import "fmt"

// BoardConfig describes a reproducible initial board. Classic boards retain
// INITPOS's shared center; modern boards choose distinct legal starts.
type BoardConfig struct {
	Ruleset       RulesMode
	Width, Height int
	Participants  int
	Seed          uint64
}

// GenerateBoard creates a validated deterministic initial state. The output is
// byte-equivalent for equal config values and does not use process-global RNG.
func GenerateBoard(cfg BoardConfig) (State, error) {
	if cfg.Participants < 1 || cfg.Participants > 4 {
		return State{}, fmt.Errorf("%w: participants must be 1..4", ErrInvalidState)
	}
	if cfg.Ruleset == ClassicRules {
		if cfg.Width == 0 {
			cfg.Width = 18
		}
		if cfg.Height == 0 {
			cfg.Height = 18
		}
		if cfg.Width != 18 || cfg.Height != 18 {
			return State{}, fmt.Errorf("%w: classic board is 18x18", ErrInvalidState)
		}
	} else if cfg.Width < 1 || cfg.Height < 1 {
		return State{}, fmt.Errorf("%w: dimensions must be positive", ErrInvalidState)
	}
	worms := make([]Worm, cfg.Participants)
	for i := range worms {
		worms[i] = Worm{ID: fmt.Sprintf("worm-%d", i), Alive: true, Controller: ControllerNew, CRIX: NOMOVE, Previous: NOMOVE}
	}
	if cfg.Ruleset == ClassicRules {
		s, err := NewClassicValidated(worms)
		if err != nil {
			return State{}, err
		}
		return s, nil
	}
	// Draw without replacement using a local xorshift stream. The candidate
	// list is stable because State.points is row-major and deterministic.
	if err := ValidateWorms(worms); err != nil {
		return State{}, err
	}
	s := New(cfg.Width, cfg.Height, worms)
	points := s.points()
	if len(points) < cfg.Participants {
		return State{}, fmt.Errorf("%w: not enough start points", ErrInvalidState)
	}
	x := cfg.Seed
	for i := len(points) - 1; i > 0; i-- {
		x = xorshift(x)
		j := int(x % uint64(i+1))
		points[i], points[j] = points[j], points[i]
	}
	for i := range s.Worms {
		s.Worms[i].Position = points[i]
	}
	return s, s.Validate()
}

// NewSeededBoard is a concise compatibility alias for GenerateBoard.
func NewSeededBoard(ruleset RulesMode, width, height, participants int, seed uint64) (State, error) {
	return GenerateBoard(BoardConfig{Ruleset: ruleset, Width: width, Height: height, Participants: participants, Seed: seed})
}
