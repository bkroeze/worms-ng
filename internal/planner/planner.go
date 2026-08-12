// Package planner provides an explainable, bounded teaching policy for unknown
// engine local patterns. It never changes the engine's execution semantics:
// once a rule is taught, engine.Lookup is the only execution path.
package planner

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"worms.ng/internal/engine"
)

const Version = "planner-v1"

var (
	ErrKnownPattern    = errors.New("planner: pattern is already known")
	ErrFrozen          = errors.New("planner: frozen brain cannot be taught")
	ErrUnknownWorm     = errors.New("planner: unknown worm")
	ErrNoAction        = errors.New("planner: no legal action")
	ErrPendingMismatch = errors.New("planner: pending decision does not match worm and pattern")
)

type Mode string

const (
	Greedy    Mode = "greedy"
	Lookahead Mode = "lookahead"
	// BoundedLookahead is a descriptive alias for Lookahead.
	BoundedLookahead Mode = Lookahead
)

type ObservationCapability string

const (
	LocalObservation  ObservationCapability = "local"
	GlobalObservation ObservationCapability = "global"
)

// Capabilities is deliberately part of Config and provenance. A planner must
// not silently acquire a wider view than the brain declared.
type Capabilities struct {
	Observation ObservationCapability `json:"observation"`
	GlobalState bool                  `json:"global_state"`
}

func (c Capabilities) normalized() Capabilities {
	if c.Observation == "" {
		c.Observation = LocalObservation
	}
	if c.Observation == GlobalObservation {
		c.GlobalState = true
	}
	return c
}

// Config controls bounded planning. All tie-breaking is derived from Seed;
// no process-global random source is used.
type Config struct {
	Version        int          `json:"version"`
	Mode           Mode         `json:"mode"`
	Depth          int          `json:"depth"`
	Seed           int64        `json:"seed"`
	CaptureWeight  int          `json:"capture_weight"`
	BorderWeight   int          `json:"border_weight"`
	SurvivalWeight int          `json:"survival_weight"`
	Capabilities   Capabilities `json:"capabilities"`
}

func DefaultConfig() Config {
	return Config{Version: 1, Mode: Greedy, Depth: 1, CaptureWeight: 100, BorderWeight: 1, SurvivalWeight: 1, Capabilities: Capabilities{Observation: LocalObservation}}
}

func (c Config) normalize() (Config, error) {
	d := DefaultConfig()
	if c.Version == 0 {
		c.Version = d.Version
	}
	if c.Version != 1 {
		return c, fmt.Errorf("planner: unsupported config version %d", c.Version)
	}
	if c.Mode == "" {
		c.Mode = d.Mode
	}
	if c.Mode != Greedy && c.Mode != Lookahead {
		return c, fmt.Errorf("planner: unsupported mode %q", c.Mode)
	}
	if c.Depth == 0 {
		c.Depth = d.Depth
	}
	if c.Depth < 1 || c.Depth > 8 {
		return c, fmt.Errorf("planner: depth must be between 1 and 8")
	}
	if c.Mode == Greedy {
		c.Depth = 1
	}
	if c.CaptureWeight == 0 && c.BorderWeight == 0 && c.SurvivalWeight == 0 {
		c.CaptureWeight, c.BorderWeight, c.SurvivalWeight = d.CaptureWeight, d.BorderWeight, d.SurvivalWeight
	}
	c.Capabilities = c.Capabilities.normalized()
	if c.Capabilities.Observation != LocalObservation && c.Capabilities.Observation != GlobalObservation {
		return c, fmt.Errorf("planner: unsupported observation capability %q", c.Capabilities.Observation)
	}
	return c, nil
}

func (c Config) Validate() error { _, err := c.normalize(); return err }

// Alternative is an explainable candidate considered at the unknown pattern.
type Alternative struct {
	Action    engine.Direction `json:"action"`
	Capture   int              `json:"capture"`
	Border    int              `json:"border"`
	Survival  int              `json:"survival"`
	Lookahead int              `json:"lookahead,omitempty"`
	Total     int              `json:"total"`
	Chosen    bool             `json:"chosen"`
	Reason    string           `json:"reason"`
}

// RuleProvenance records every input needed to reproduce a learned rule.
type RuleProvenance struct {
	Version      string        `json:"version"`
	Source       string        `json:"source"`
	BrainID      string        `json:"brain_id,omitempty"`
	BrainVersion string        `json:"brain_version,omitempty"`
	WormID       string        `json:"worm_id"`
	Mask         uint8         `json:"mask"`
	RawMask      string        `json:"raw_mask"`
	Action       engine.Action `json:"action"`
	Seed         int64         `json:"seed"`
	Mode         Mode          `json:"mode"`
	Depth        int           `json:"depth"`
	Capabilities Capabilities  `json:"capabilities"`
	StateHash    string        `json:"state_hash"`
	Tick         uint64        `json:"tick"`
	Round        uint64        `json:"round"`
	Alternatives []Alternative `json:"alternatives"`
}

// Decision is both the teaching result and its human/audit explanation.
type Decision struct {
	WormID       string         `json:"worm_id"`
	Mask         uint8          `json:"mask"`
	Action       engine.Action  `json:"action"`
	Alternatives []Alternative  `json:"alternatives"`
	Provenance   RuleProvenance `json:"provenance"`
}

// Planner plans only; it does not install or execute rules unless Teach is
// explicitly called.
type Planner struct {
	config Config
	calls  uint64
}

func New(c Config) (*Planner, error) {
	c, err := c.normalize()
	if err != nil {
		return nil, err
	}
	return &Planner{config: c}, nil
}

func MustNew(c Config) *Planner {
	p, err := New(c)
	if err != nil {
		panic(err)
	}
	return p
}

// NewPlanner is an explicit constructor alias for callers that prefer the
// longer name in experiment manifests.
func NewPlanner(c Config) (*Planner, error) { return New(c) }

// Strategy aliases Mode for configuration-oriented call sites.
type Strategy = Mode

const (
	StrategyGreedy    = Greedy
	StrategyLookahead = Lookahead
)

func (p *Planner) Config() Config { return p.config }
func (p *Planner) Calls() uint64  { return p.calls }

// Plan is the only planning entry point. It rejects known local rules, so a
// caller cannot accidentally invoke strategic planning during ordinary play.
func (p *Planner) Plan(state engine.State, wormID string) (Decision, error) {
	idx := wormIndex(state, wormID)
	if idx < 0 {
		return Decision{}, ErrUnknownWorm
	}
	w := state.Worms[idx]
	if w.Frozen {
		return Decision{}, ErrFrozen
	}
	mask := state.Mask(w.Position) & 63
	if w.Rules[mask] != engine.ActionGetNew {
		return Decision{}, ErrKnownPattern
	}
	p.calls++
	planningState := state
	localHash := ""
	if !p.config.Capabilities.GlobalState {
		obs, err := state.Observe(wormID)
		if err != nil {
			return Decision{}, ErrUnknownWorm
		}
		planningState = localPlanningState(state, idx, obs)
		idx = 0
		localHash = localObservationHash(obs, p.config)
	}
	return p.planUnknown(planningState, idx, mask, localHash)
}

// PlanUnknown is a descriptive alias for Plan.
func (p *Planner) PlanUnknown(state engine.State, wormID string) (Decision, error) {
	return p.Plan(state, wormID)
}

// TeachUnknown is a descriptive alias for Teach.
func (p *Planner) TeachUnknown(state *engine.State, wormID string) (Decision, error) {
	return p.Teach(state, wormID)
}

// Plan constructs a one-shot planner with the supplied configuration.
func Plan(state engine.State, wormID string, c Config) (Decision, error) {
	p, err := New(c)
	if err != nil {
		return Decision{}, err
	}
	return p.Plan(state, wormID)
}

// Teach constructs a one-shot planner and installs the resulting local rule.
func Teach(state *engine.State, wormID string, c Config) (Decision, error) {
	p, err := New(c)
	if err != nil {
		return Decision{}, err
	}
	return p.Teach(state, wormID)
}

// Teach plans and installs exactly one local table entry. Subsequent
// execution must use state.Lookup; this method does not provide a second
// strategic execution path.
func (p *Planner) Teach(state *engine.State, wormID string) (Decision, error) {
	if state == nil {
		return Decision{}, ErrUnknownWorm
	}
	idx := wormIndex(*state, wormID)
	if idx < 0 {
		return Decision{}, ErrUnknownWorm
	}
	if state.Worms[idx].Frozen {
		return Decision{}, ErrFrozen
	}
	if state.Pending != nil {
		mask := state.Mask(state.Worms[idx].Position) & 63
		if state.Pending.WormID != wormID || state.Pending.Slot != idx || state.Pending.Mask != mask {
			return Decision{}, ErrPendingMismatch
		}
		d, err := p.Plan(*state, wormID)
		if err != nil {
			return Decision{}, err
		}
		if _, err := state.Submit(engine.Direction(d.Action)); err != nil {
			return Decision{}, err
		}
		return d, nil
	}
	d, err := p.Plan(*state, wormID)
	if err != nil {
		return Decision{}, err
	}
	mask := d.Mask & 63
	state.Worms[idx].Rules[mask] = d.Action
	return d, nil
}

// Lookup performs ordinary local execution. It never invokes Plan, including
// for frozen brains; frozen unknown rules are an explicit error.
func Lookup(state engine.State, wormID string) (engine.Action, error) {
	idx := wormIndex(state, wormID)
	if idx < 0 {
		return 0, ErrUnknownWorm
	}
	a, err := state.Lookup(wormID)
	if err != nil {
		return 0, ErrUnknownWorm
	}
	if state.Worms[idx].Frozen && a == engine.ActionGetNew {
		return 0, ErrFrozen
	}
	return a, nil
}

// FrozenLookup is a named alias useful to evaluation harnesses. It cannot
// call a planner because it accepts no Planner value.
func FrozenLookup(state engine.State, wormID string) (engine.Action, error) {
	return Lookup(state, wormID)
}

// Evaluate executes a frozen or taught brain through its ordinary local rule
// table. It intentionally does not consult this planner or increment Calls.
func (p *Planner) Evaluate(state engine.State, wormID string) (engine.Action, error) {
	return Lookup(state, wormID)
}

// Execute is a concise alias for ordinary local execution.
func Execute(state engine.State, wormID string) (engine.Action, error) {
	return Lookup(state, wormID)
}

func wormIndex(state engine.State, id string) int {
	for i := range state.Worms {
		if state.Worms[i].ID == id {
			return i
		}
	}
	return -1
}
func localMaskForCount(n uint8) uint8 {
	if n > 6 {
		n = 6
	}
	if n == 0 {
		return 0
	}
	return uint8((uint16(1) << n) - 1)
}

// localPlanningState is a deliberately lossy simulation world. It contains
// only the subject worm, the observed current legality, and the six observed
// neighboring territory summaries. Unknown geometry and ownership are never
// copied into a local plan.
func localPlanningState(state engine.State, idx int, obs engine.Observation) engine.State {
	s := state.Snapshot()
	w := s.Worms[idx]
	s.Width, s.Height = 1<<30, 1<<30
	s.Topology, s.Mode = engine.Bounded, engine.ModernRules
	s.Tick, s.Round = 0, 0
	s.Events = nil
	s.ActiveSlot = -1
	s.Pending = nil
	s.GameOver = false
	s.Worms = []engine.Worm{w}
	s.Trails = map[engine.Edge]string{}
	s.Territories = map[engine.Point]engine.Territory{}
	blocked := uint8(63)
	for _, d := range obs.Legal {
		blocked &^= 1 << d
	}
	s.Territories[w.Position] = engine.Territory{ID: w.Position, Mask: blocked}
	for d := engine.East; d <= engine.NorthEast; d++ {
		q := s.Neighbor(w.Position, d)
		s.Territories[q] = engine.Territory{ID: q, Mask: localMaskForCount(obs.LocalTerritoryCounts[d])}
	}
	return s
}

func localObservationHash(obs engine.Observation, c Config) string {
	type input struct {
		Observation engine.Observation `json:"observation"`
		Config      Config             `json:"config"`
	}
	b, _ := json.Marshal(input{Observation: obs, Config: c})
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h[:])
}

func (p *Planner) planUnknown(state engine.State, idx int, mask uint8, localHash string) (Decision, error) {
	w := state.Worms[idx]
	legal := state.LegalMoves(w.ID)
	if len(legal) == 0 {
		return Decision{}, ErrNoAction
	}
	sort.Slice(legal, func(i, j int) bool { return legal[i] < legal[j] })
	alts := make([]Alternative, 0, len(legal))
	for _, d := range legal {
		alts = append(alts, p.score(state, idx, d))
	}
	bestTotal := alts[0].Total
	for _, a := range alts[1:] {
		if a.Total > bestTotal {
			bestTotal = a.Total
		}
	}
	ties := make([]int, 0, len(alts))
	for i := range alts {
		if alts[i].Total == bestTotal {
			ties = append(ties, i)
		}
	}
	chosen := ties[p.tieIndex(localHash, state, w.ID, mask, len(ties))]
	for i := range alts {
		alts[i].Chosen = i == chosen
		if alts[i].Chosen {
			alts[i].Reason = "highest bounded utility; seeded tie-break"
		} else if alts[i].Total == bestTotal {
			alts[i].Reason = "utility tie; not selected by seeded tie-break"
		} else {
			alts[i].Reason = "lower bounded utility"
		}
	}
	action := engine.Action(alts[chosen].Action)
	stateHash, tick, round := state.HashHex(), state.Tick, state.Round
	if localHash != "" {
		stateHash, tick, round = localHash, 0, 0
	}
	prov := RuleProvenance{Version: Version, Source: "bounded-strategic-planner", BrainID: w.BrainID, BrainVersion: w.BrainVersion, WormID: w.ID, Mask: mask, RawMask: engine.RawMaskBits(mask), Action: action, Seed: p.config.Seed, Mode: p.config.Mode, Depth: p.config.Depth, Capabilities: p.config.Capabilities, StateHash: stateHash, Tick: tick, Round: round, Alternatives: append([]Alternative(nil), alts...)}
	return Decision{WormID: w.ID, Mask: mask, Action: action, Alternatives: alts, Provenance: prov}, nil
}

func (p *Planner) tieIndex(localHash string, state engine.State, id string, mask uint8, n int) int {
	if n <= 1 {
		return 0
	}
	var b [16]byte
	binary.LittleEndian.PutUint64(b[:8], uint64(p.config.Seed))
	if localHash == "" {
		binary.LittleEndian.PutUint64(b[8:], uint64(state.Tick)^uint64(state.Round)<<32^uint64(mask))
	} else {
		copy(b[8:], []byte(localHash))
	}
	h := sha256.Sum256(append(append(b[:], []byte(id)...), byte(p.config.Mode[0])))
	return int(binary.LittleEndian.Uint64(h[:8]) % uint64(n))
}

func (p *Planner) score(state engine.State, idx int, d engine.Direction) Alternative {
	trial := state.Snapshot()
	w := trial.Worms[idx]
	before := trial.Worms[idx].Score
	if _, err := trial.Step(w.ID, d); err != nil {
		return Alternative{Action: d, Total: -1 << 30, Reason: "illegal candidate"}
	}
	capture := trial.Worms[idx].Score - before
	border := borderScore(trial, trial.Worms[idx].Position)
	survival := len(trial.LegalMoves(w.ID))
	look := 0
	if p.config.Mode == Lookahead && p.config.Depth > 1 && trial.Worms[idx].Alive {
		look = p.future(trial, idx, p.config.Depth-1)
	}
	total := p.config.CaptureWeight*capture + p.config.BorderWeight*border + p.config.SurvivalWeight*survival + look
	return Alternative{Action: d, Capture: capture, Border: border, Survival: survival, Lookahead: look, Total: total}
}

func (p *Planner) future(state engine.State, idx, depth int) int {
	if depth <= 0 || idx < 0 || idx >= len(state.Worms) || !state.Worms[idx].Alive {
		return 0
	}
	moves := state.LegalMoves(state.Worms[idx].ID)
	best := -1 << 30
	for _, d := range moves {
		trial := state.Snapshot()
		before := trial.Worms[idx].Score
		if _, err := trial.Step(trial.Worms[idx].ID, d); err != nil {
			continue
		}
		v := p.config.CaptureWeight*(trial.Worms[idx].Score-before) + p.config.BorderWeight*borderScore(trial, trial.Worms[idx].Position) + p.config.SurvivalWeight*len(trial.LegalMoves(trial.Worms[idx].ID)) + p.future(trial, idx, depth-1)
		if v > best {
			best = v
		}
	}
	if best == -1<<30 {
		return 0
	}
	return best
}

func borderScore(state engine.State, p engine.Point) int {
	// A toroidal board has no border. For bounded boards, count traversable
	// neighboring cells; this is a stable, explainable openness score.
	if state.Topology == engine.Toroidal {
		return 6
	}
	start := 0
	if state.Mode == engine.ClassicRules {
		start = 1
	}
	n := 0
	for d := engine.East; d <= engine.NorthEast; d++ {
		q := state.Neighbor(p, d)
		if q.Q >= start && q.Q < start+state.Width && q.R >= start && q.R < start+state.Height {
			n++
		}
	}
	return n
}

func (d Decision) MarshalJSON() ([]byte, error) {
	type plain Decision
	return json.Marshal(plain(d))
}
