// Package extension contains versioned, opt-in rules layered on engine.State.
// It intentionally has no dependencies on the server, store, or UI packages.
package extension

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"

	"worms.ng/internal/engine"
)

const CurrentVersion = 1
const ObservationVersion = 1

var (
	ErrInvalidConfig      = errors.New("extension: invalid config")
	ErrUnsupportedVersion = errors.New("extension: unsupported version")
	ErrIncompatible       = errors.New("extension: incompatible features")
	ErrInvalidAction      = errors.New("extension: invalid action")
	ErrHidden             = errors.New("extension: value hidden")
)

type OneWayTrail struct {
	From engine.Point `json:"from"`
	To   engine.Point `json:"to"`
}

// Config is the complete JSON-versioned extension configuration. A zero value
// is classic mode; no extension field is consulted in that mode.
type Config struct {
	Version             int                  `json:"version,omitempty"`
	Enabled             bool                 `json:"enabled,omitempty"`
	Width               int                  `json:"width,omitempty"`  // optional generated-world dimensions
	Height              int                  `json:"height,omitempty"` // optional generated-world dimensions
	Seed                int64                `json:"seed,omitempty"`
	Obstacles           []engine.Point       `json:"obstacles,omitempty"`
	Holes               []engine.Point       `json:"holes,omitempty"`
	WeightedTerritories map[engine.Point]int `json:"-"`
	OneWayTrails        []OneWayTrail        `json:"one_way_trails,omitempty"`
	TemporaryTrailTTL   uint64               `json:"temporary_trail_ttl,omitempty"`
	EnergyLimit         int                  `json:"energy_limit,omitempty"`
	Teams               map[string]string    `json:"teams,omitempty"` // worm ID -> team ID
	FogOfWar            bool                 `json:"fog_of_war,omitempty"`
	VisibilityRadius    int                  `json:"visibility_radius,omitempty"`
	ObstacleRate        uint8                `json:"obstacle_rate,omitempty"` // seeded worlds, percentage
	HoleRate            uint8                `json:"hole_rate,omitempty"`
}

func (c Config) classic() bool {
	return !c.Enabled && c.Version == 0 && len(c.Obstacles) == 0 && len(c.Holes) == 0 && len(c.OneWayTrails) == 0 && len(c.Teams) == 0 && len(c.WeightedTerritories) == 0 && c.TemporaryTrailTTL == 0 && c.EnergyLimit == 0 && !c.FogOfWar && c.Width == 0 && c.Height == 0 && c.ObstacleRate == 0 && c.HoleRate == 0
}

// NormalizeConfig returns the canonical, detached configuration used by an
// extension state. A zero config seed inherits the request seed; an explicit
// config seed wins. Callers should persist this value rather than the raw
// request so reload comparisons are stable.
func NormalizeConfig(c Config, seed int64) Config {
	c = copyConfig(c)
	if c.Seed == 0 {
		c.Seed = seed
	}
	return c
}

func (c Config) Normalized(seed int64) Config { return NormalizeConfig(c, seed) }

// NormalizeCapabilities applies the planner capability implication without
// importing the planner package. Global observation always implies global
// state, including when the JSON omitted global_state.
func NormalizeCapabilities(observation string, globalState bool) (string, bool, error) {
	if observation == "" {
		observation = "local"
	}
	if observation != "local" && observation != "global" {
		return "", false, fmt.Errorf("%w: unsupported observation capability %q", ErrInvalidConfig, observation)
	}
	if observation == "global" {
		globalState = true
	}
	return observation, globalState, nil
}

func (c Config) SafeClientConfig() ClientConfig {
	return ClientConfig{
		Version: c.Version, Enabled: c.Enabled, Width: c.Width, Height: c.Height,
		TemporaryTrailTTL: c.TemporaryTrailTTL, EnergyLimit: c.EnergyLimit,
		Teams: copyStringString(c.Teams), FogOfWar: c.FogOfWar,
		VisibilityRadius: c.VisibilityRadius,
	}
}
func (c Config) Validate(base engine.State) error {
	if c.classic() {
		return nil
	}
	if !c.Enabled {
		return fmt.Errorf("%w: features require enabled=true", ErrInvalidConfig)
	}
	if c.Version == 0 {
		return fmt.Errorf("%w: version is required", ErrUnsupportedVersion)
	}
	if c.Version != CurrentVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, c.Version)
	}
	if c.Width < 0 || c.Height < 0 || c.Width > 0 && c.Width != base.Width || c.Height > 0 && c.Height != base.Height {
		return fmt.Errorf("%w: dimensions do not match base", ErrInvalidConfig)
	}
	if c.ObstacleRate > 100 || c.HoleRate > 100 || int(c.ObstacleRate)+int(c.HoleRate) > 100 {
		return fmt.Errorf("%w: rates must sum to <=100", ErrInvalidConfig)
	}
	if c.ObstacleRate > 0 || c.HoleRate > 0 {
		if c.Seed == 0 {
			return fmt.Errorf("%w: generated worlds require a non-zero seed", ErrInvalidConfig)
		}
	}
	seen := map[engine.Point]bool{}
	for _, p := range append(append([]engine.Point{}, c.Obstacles...), c.Holes...) {
		if !inBounds(base, p) {
			return fmt.Errorf("%w: point %s outside board", ErrInvalidConfig, p)
		}
		if seen[p] {
			return fmt.Errorf("%w: duplicate or overlapping obstacle/hole", ErrInvalidConfig)
		}
		seen[p] = true
		for _, w := range base.Worms {
			if w.Position == p {
				return fmt.Errorf("%w: start lies in obstacle/hole", ErrInvalidConfig)
			}
		}
	}
	for p, w := range c.WeightedTerritories {
		if !inBounds(base, p) || w <= 0 {
			return fmt.Errorf("%w: invalid territory weight", ErrInvalidConfig)
		}
	}
	for _, x := range c.OneWayTrails {
		if !inBounds(base, x.From) || !inBounds(base, x.To) || !adjacent(base, x.From, x.To) {
			return fmt.Errorf("%w: one-way trail is not adjacent", ErrInvalidConfig)
		}
		if contains(c.Obstacles, x.From) || contains(c.Obstacles, x.To) || contains(c.Holes, x.From) || contains(c.Holes, x.To) {
			return fmt.Errorf("%w: one-way trail touches blocked point", ErrIncompatible)
		}
	}
	if c.TemporaryTrailTTL > 0 && len(c.OneWayTrails) > 0 {
		return fmt.Errorf("%w: temporary and one-way trails cannot be combined", ErrIncompatible)
	}
	if c.EnergyLimit < 0 {
		return fmt.Errorf("%w: negative energy limit", ErrInvalidConfig)
	}
	if c.FogOfWar && c.VisibilityRadius < 0 {
		return fmt.Errorf("%w: negative visibility radius", ErrInvalidConfig)
	}
	if len(c.Teams) > 0 {
		teams := map[string]bool{}
		for id, t := range c.Teams {
			if id == "" || t == "" {
				return fmt.Errorf("%w: empty team identity", ErrInvalidConfig)
			}
			if _, ok := worm(base, id); !ok {
				return fmt.Errorf("%w: unknown team worm", ErrInvalidConfig)
			}
			teams[t] = true
		}
		if len(teams) < 2 {
			return fmt.Errorf("%w: teams require at least two teams", ErrIncompatible)
		}
	}
	return nil
}

const Version = CurrentVersion

type ExtensionConfig = Config
type ExtensionState = State
type Ruleset = Config

func ValidateConfig(c Config, base engine.State) error  { return c.Validate(base) }
func (c Config) ValidateConfig(base engine.State) error { return c.Validate(base) }
func DefaultConfig() Config                             { return Config{} }

// MarshalJSON uses an array for point keyed weights, avoiding Go's inability to
// encode map[engine.Point] as JSON while retaining the convenient Go API.
func (c Config) MarshalJSON() ([]byte, error) {
	type weight struct {
		Point  engine.Point `json:"point"`
		Weight int          `json:"weight"`
	}
	ws := make([]weight, 0, len(c.WeightedTerritories))
	for p, v := range c.WeightedTerritories {
		ws = append(ws, weight{p, v})
	}
	sort.Slice(ws, func(i, j int) bool { return pointLess(ws[i].Point, ws[j].Point) })
	type plain Config
	return json.Marshal(struct {
		plain
		Weights []weight `json:"weighted_territories,omitempty"`
	}{plain: plain(c), Weights: ws})
}
func (c *Config) UnmarshalJSON(b []byte) error {
	type weight struct {
		Point  engine.Point `json:"point"`
		Weight int          `json:"weight"`
	}
	type plain Config
	var x struct {
		plain
		Weights []weight `json:"weighted_territories"`
	}
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	*c = Config(x.plain)
	if len(x.Weights) > 0 {
		c.WeightedTerritories = map[engine.Point]int{}
		for _, w := range x.Weights {
			c.WeightedTerritories[w.Point] = w.Weight
		}
	}
	return nil
}

type VariantState struct {
	Obstacles map[engine.Point]bool            `json:"obstacles,omitempty"`
	Holes     map[engine.Point]bool            `json:"holes,omitempty"`
	Weights   map[engine.Point]int             `json:"weights,omitempty"`
	OneWay    map[engine.Edge]engine.Direction `json:"one_way,omitempty"`
	Temporary map[engine.Edge]uint64           `json:"temporary,omitempty"`
	Energy    map[string]int                   `json:"energy,omitempty"`
	Teams     map[string]string                `json:"teams,omitempty"`
	// Scores stores weighted territory totals per worm. Base.Worms.Score
	// remains the engine's completed-territory count.
	Scores     map[string]int `json:"scores,omitempty"`
	TeamScores map[string]int `json:"team_scores,omitempty"`
}

// MarshalJSON keeps struct-keyed maps portable by representing all point and
// edge maps as deterministic arrays.
func (v VariantState) MarshalJSON() ([]byte, error) {
	type pointBool struct {
		Point engine.Point `json:"point"`
		Value bool         `json:"value"`
	}
	type pointInt struct {
		Point engine.Point `json:"point"`
		Value int          `json:"value"`
	}
	type edgeDir struct {
		Edge      engine.Edge      `json:"edge"`
		Direction engine.Direction `json:"direction"`
	}
	type edgeTTL struct {
		Edge   engine.Edge `json:"edge"`
		Expiry uint64      `json:"expiry"`
	}
	ob := make([]pointBool, 0, len(v.Obstacles))
	for p, x := range v.Obstacles {
		ob = append(ob, pointBool{p, x})
	}
	sort.Slice(ob, func(i, j int) bool { return pointLess(ob[i].Point, ob[j].Point) })
	ho := make([]pointBool, 0, len(v.Holes))
	for p, x := range v.Holes {
		ho = append(ho, pointBool{p, x})
	}
	sort.Slice(ho, func(i, j int) bool { return pointLess(ho[i].Point, ho[j].Point) })
	we := make([]pointInt, 0, len(v.Weights))
	for p, x := range v.Weights {
		we = append(we, pointInt{p, x})
	}
	sort.Slice(we, func(i, j int) bool { return pointLess(we[i].Point, we[j].Point) })
	one := make([]edgeDir, 0, len(v.OneWay))
	for e, d := range v.OneWay {
		one = append(one, edgeDir{e, d})
	}
	sort.Slice(one, func(i, j int) bool { return edgeLess(one[i].Edge, one[j].Edge) })
	tmp := make([]edgeTTL, 0, len(v.Temporary))
	for e, x := range v.Temporary {
		tmp = append(tmp, edgeTTL{e, x})
	}
	sort.Slice(tmp, func(i, j int) bool { return edgeLess(tmp[i].Edge, tmp[j].Edge) })
	type wire struct {
		Obstacles  []pointBool       `json:"obstacles,omitempty"`
		Holes      []pointBool       `json:"holes,omitempty"`
		Weights    []pointInt        `json:"weights,omitempty"`
		OneWay     []edgeDir         `json:"one_way,omitempty"`
		Temporary  []edgeTTL         `json:"temporary,omitempty"`
		Energy     map[string]int    `json:"energy,omitempty"`
		Teams      map[string]string `json:"teams,omitempty"`
		Scores     map[string]int    `json:"scores,omitempty"`
		TeamScores map[string]int    `json:"team_scores,omitempty"`
	}
	return json.Marshal(wire{ob, ho, we, one, tmp, v.Energy, v.Teams, v.Scores, v.TeamScores})
}
func (v *VariantState) UnmarshalJSON(b []byte) error {
	type pointBool struct {
		Point engine.Point `json:"point"`
		Value bool         `json:"value"`
	}
	type pointInt struct {
		Point engine.Point `json:"point"`
		Value int          `json:"value"`
	}
	type edgeDir struct {
		Edge      engine.Edge      `json:"edge"`
		Direction engine.Direction `json:"direction"`
	}
	type edgeTTL struct {
		Edge   engine.Edge `json:"edge"`
		Expiry uint64      `json:"expiry"`
	}
	var x struct {
		Obstacles  []pointBool       `json:"obstacles"`
		Holes      []pointBool       `json:"holes"`
		Weights    []pointInt        `json:"weights"`
		OneWay     []edgeDir         `json:"one_way"`
		Temporary  []edgeTTL         `json:"temporary"`
		Energy     map[string]int    `json:"energy"`
		Teams      map[string]string `json:"teams"`
		Scores     map[string]int    `json:"scores"`
		TeamScores map[string]int    `json:"team_scores"`
	}
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	*v = VariantState{
		Obstacles: map[engine.Point]bool{}, Holes: map[engine.Point]bool{},
		Weights: map[engine.Point]int{}, OneWay: map[engine.Edge]engine.Direction{},
		Temporary: map[engine.Edge]uint64{}, Energy: x.Energy, Teams: x.Teams,
		Scores: x.Scores, TeamScores: x.TeamScores,
	}
	for _, p := range x.Obstacles {
		v.Obstacles[p.Point] = p.Value
	}
	for _, p := range x.Holes {
		v.Holes[p.Point] = p.Value
	}
	for _, p := range x.Weights {
		v.Weights[p.Point] = p.Value
	}
	for _, e := range x.OneWay {
		v.OneWay[e.Edge] = e.Direction
	}
	for _, e := range x.Temporary {
		v.Temporary[e.Edge] = e.Expiry
	}
	return nil
}

type State struct {
	Base    engine.State `json:"base"`
	Config  Config       `json:"config"`
	Seed    int64        `json:"seed"`
	Variant VariantState `json:"variant"`
	Events  []Event      `json:"events,omitempty"`
	// GameEventSequence/Hash identify the engine/store event cursor covered by
	// this snapshot. They are deliberately separate from Base.Tick: one tick
	// can emit several game events.
	GameEventSequence int64  `json:"game_event_sequence,omitempty"`
	GameEventHash     string `json:"game_event_hash,omitempty"`
}
type Event struct {
	Version    int         `json:"version"`
	Seq        uint64      `json:"seq"`
	Tick       uint64      `json:"tick"`
	Type       string      `json:"type"`
	WormID     string      `json:"worm_id,omitempty"`
	Edge       engine.Edge `json:"edge,omitempty"`
	Expiry     uint64      `json:"expiry,omitempty"`
	Energy     int         `json:"energy,omitempty"`
	Team       string      `json:"team,omitempty"`
	ScoreDelta int         `json:"score_delta,omitempty"`
	PrevHash   string      `json:"prev_hash,omitempty"`
	Hash       string      `json:"hash,omitempty"`
}
type Action struct {
	WormID    string           `json:"worm_id"`
	Direction engine.Direction `json:"direction"`
}

func New(base engine.State, config Config, seed int64) (State, error) {
	config = NormalizeConfig(config, seed)
	if config.Seed != 0 {
		seed = config.Seed
	}
	if err := config.Validate(base); err != nil {
		return State{}, err
	}
	s := State{Base: base.Snapshot(), Config: config, Seed: seed}
	if config.classic() {
		return s, nil
	}
	s.Variant = VariantState{
		Obstacles: map[engine.Point]bool{}, Holes: map[engine.Point]bool{},
		Weights: map[engine.Point]int{}, OneWay: map[engine.Edge]engine.Direction{},
		Temporary: map[engine.Edge]uint64{}, Energy: map[string]int{},
		Teams: map[string]string{}, Scores: map[string]int{}, TeamScores: map[string]int{},
	}
	for _, p := range config.Obstacles {
		s.Variant.Obstacles[p] = true
	}
	for _, p := range config.Holes {
		s.Variant.Holes[p] = true
	}
	for p, w := range config.WeightedTerritories {
		s.Variant.Weights[p] = w
	}
	for _, x := range config.OneWayTrails {
		s.Variant.OneWay[engine.NewEdge(x.From, x.To)] = direction(s.Base, x.From, x.To)
	}
	for id, t := range config.Teams {
		s.Variant.Teams[id] = t
	}
	for _, w := range s.Base.Worms {
		if config.EnergyLimit > 0 {
			s.Variant.Energy[w.ID] = config.EnergyLimit
		}
		s.Variant.Scores[w.ID] = weightedScore(s.Base, s.Variant, w.ID)
	}
	if config.ObstacleRate > 0 || config.HoleRate > 0 {
		s.generateWorld()
	}
	for _, w := range s.Base.Worms {
		if w.Alive && len(s.LegalMoves(w.ID)) == 0 {
			return State{}, fmt.Errorf("%w: generated world blocks start %s", ErrInvalidConfig, w.ID)
		}
	}
	for id, team := range s.Variant.Teams {
		s.Variant.TeamScores[team] += s.Variant.Scores[id]
	}
	return s, nil
}
func NewState(base engine.State, c Config, seed int64) (State, error) { return New(base, c, seed) }

func (s State) Classic() bool { return s.Config.classic() }
func (s State) Validate() error {
	if err := s.Base.Validate(); err != nil {
		return err
	}
	if err := s.Config.Validate(s.Base); err != nil {
		return err
	}
	if s.Classic() {
		return nil
	}
	return s.validateVariant()
}
func (s State) validateVariant() error {
	expected, err := New(s.Base, s.Config, s.Seed)
	if err != nil {
		return err
	}
	if !equalPointBool(s.Variant.Obstacles, expected.Variant.Obstacles) ||
		!equalPointBool(s.Variant.Holes, expected.Variant.Holes) ||
		!equalPointInt(s.Variant.Weights, expected.Variant.Weights) ||
		!equalEdgeDir(s.Variant.OneWay, expected.Variant.OneWay) ||
		!equalStringString(s.Variant.Teams, expected.Variant.Teams) {
		return fmt.Errorf("%w: variant immutable fields differ from config", ErrInvalidConfig)
	}
	for _, w := range s.Base.Worms {
		if s.Config.EnergyLimit > 0 {
			x, ok := s.Variant.Energy[w.ID]
			if !ok || x < 0 || x > s.Config.EnergyLimit || (x == 0 && w.Alive) {
				return fmt.Errorf("%w: invalid energy for %s", ErrInvalidConfig, w.ID)
			}
		} else if len(s.Variant.Energy) != 0 {
			return fmt.Errorf("%w: energy without limit", ErrInvalidConfig)
		}
	}
	if s.Config.EnergyLimit > 0 && len(s.Variant.Energy) != len(s.Base.Worms) {
		return fmt.Errorf("%w: incomplete energy map", ErrInvalidConfig)
	}
	for e, expiry := range s.Variant.Temporary {
		if expiry <= s.Base.Tick || !inBounds(s.Base, e.A) || !inBounds(s.Base, e.B) || !adjacent(s.Base, e.A, e.B) {
			return fmt.Errorf("%w: invalid temporary trail", ErrInvalidConfig)
		}
		if _, ok := s.Base.Trails[e]; !ok {
			return fmt.Errorf("%w: temporary trail is not occupied", ErrInvalidConfig)
		}
	}
	expectedScores := map[string]int{}
	expectedTeams := map[string]int{}
	for _, w := range s.Base.Worms {
		expectedScores[w.ID] = weightedScore(s.Base, s.Variant, w.ID)
	}
	for id, team := range s.Variant.Teams {
		expectedTeams[team] += expectedScores[id]
	}
	if !equalStringInt(s.Variant.Scores, expectedScores) || !equalStringInt(s.Variant.TeamScores, expectedTeams) {
		return fmt.Errorf("%w: score totals do not match territories", ErrInvalidConfig)
	}
	return validateEvents(s)
}

func weightedScore(base engine.State, v VariantState, id string) int {
	total := 0
	for p, t := range base.Territories {
		if t.Mask != 63 || t.Owner != id {
			continue
		}
		weight := v.Weights[p]
		if weight == 0 {
			weight = 1
		}
		total += weight
	}
	return total
}

func (s *State) recordEvent(e Event) Event {
	e.Seq = uint64(len(s.Events) + 1)
	if len(s.Events) != 0 {
		e.PrevHash = s.Events[len(s.Events)-1].Hash
	}
	e.Hash = eventHash(e)
	s.Events = append(s.Events, e)
	return e
}
func eventHash(e Event) string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	return hash(b)
}
func validateEvents(s State) error {
	prev := ""
	for i, e := range s.Events {
		if e.Version != CurrentVersion || e.Seq != uint64(i+1) || e.PrevHash != prev || e.Hash == "" || eventHash(e) != e.Hash {
			return fmt.Errorf("%w: invalid extension event chain", ErrInvalidConfig)
		}
		if e.Tick > s.Base.Tick {
			return fmt.Errorf("%w: extension event is from the future", ErrInvalidConfig)
		}
		switch e.Type {
		case "worm_moved", "trail_expired":
		case "round_advanced":
			if e.WormID != "" || e.Edge != (engine.Edge{}) {
				return fmt.Errorf("%w: malformed round event", ErrInvalidConfig)
			}
		default:
			return fmt.Errorf("%w: unknown extension event type", ErrInvalidConfig)
		}
		prev = e.Hash
	}
	if s.Base.Tick > 0 && len(s.Events) == 0 {
		return fmt.Errorf("%w: missing extension events", ErrInvalidConfig)
	}
	return nil
}
func (s *State) Replay(events []Event) error {
	prev := ""
	nextSeq := uint64(1)
	if n := len(s.Events); n > 0 {
		prev, nextSeq = s.Events[n-1].Hash, uint64(n+1)
	}
	for i, e := range events {
		if e.Version != CurrentVersion || e.Seq != nextSeq+uint64(i) || e.PrevHash != prev || e.Hash == "" || eventHash(e) != e.Hash {
			return fmt.Errorf("%w: replay metadata mismatch", ErrInvalidConfig)
		}
		prev = e.Hash
	}
	for pos := 0; pos < len(events); {
		input := events[pos]
		if input.Type == "round_advanced" {
			if input.WormID != "" || input.Edge != (engine.Edge{}) || input.Tick != s.Base.Tick {
				return fmt.Errorf("%w: malformed round event", ErrInvalidConfig)
			}
			before := len(s.Events)
			s.Base.ActiveSlot = -1
			s.Base.Round++
			s.recordEvent(input)
			if len(s.Events) != before+1 || !sameEvent(s.Events[before], input) {
				return fmt.Errorf("%w: replay round mismatch", ErrInvalidConfig)
			}
			if s.aliveCount() == 0 {
				s.Base.GameOver = true
			}
			pos++
			continue
		}
		if input.Type == "trail_expired" {
			before := len(s.Events)
			s.expire()
			generated := s.Events[before:]
			if len(generated) == 0 || pos+len(generated) > len(events) {
				return fmt.Errorf("%w: replay expiry count", ErrInvalidConfig)
			}
			for i, got := range generated {
				if !sameEvent(got, events[pos+i]) {
					return fmt.Errorf("%w: replay expiry mismatch", ErrInvalidConfig)
				}
			}
			pos += len(generated)
			continue
		}
		w, ok := worm(s.Base, input.WormID)
		if !ok {
			return fmt.Errorf("%w: replay worm %s", ErrInvalidConfig, input.WormID)
		}
		var d engine.Direction
		found := false
		for candidate := engine.East; candidate <= engine.NorthEast; candidate++ {
			if s.Base.Neighbor(w.Position, candidate) == other(input.Edge, w.Position) {
				d, found = candidate, true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: replay edge direction", ErrInvalidConfig)
		}
		before := len(s.Events)
		if _, err := s.Apply(Action{WormID: input.WormID, Direction: d}); err != nil {
			return err
		}
		generated := s.Events[before:]
		if len(generated) == 0 || pos+len(generated) > len(events) {
			return fmt.Errorf("%w: replay event count", ErrInvalidConfig)
		}
		for i, got := range generated {
			if !sameEvent(got, events[pos+i]) {
				return fmt.Errorf("%w: replay event mismatch", ErrInvalidConfig)
			}
		}
		pos += len(generated)
	}
	return nil
}
func Replay(initial State, events []Event) (State, error) {
	out := initial.Snapshot()
	if err := out.Replay(events); err != nil {
		return State{}, err
	}
	return out, nil
}
func sameEvent(a, b Event) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
func (s State) LegalMoves(id string) []engine.Direction {
	if s.Classic() {
		return s.Base.LegalMoves(id)
	}
	w, ok := worm(s.Base, id)
	if !ok || !w.Alive {
		return nil
	}
	out := []engine.Direction{}
	for d := engine.East; d <= engine.NorthEast; d++ {
		if s.legal(id, d) {
			out = append(out, d)
		}
	}
	return out
}
func (s State) legal(id string, d engine.Direction) bool {
	w, ok := worm(s.Base, id)
	if !ok || !w.Alive || !d.Valid() {
		return false
	}
	if s.Config.EnergyLimit > 0 && s.Variant.Energy[id] <= 0 {
		return false
	}
	from := w.Position
	to := s.Base.Neighbor(from, d)
	if !inBounds(s.Base, to) || s.Variant.Obstacles[to] || s.Variant.Holes[to] || s.Base.Mask(from)&1<<d != 0 {
		return false
	}
	if s.Base.Mode == engine.ModernRules && hasWorm(s.Base, to) {
		return false
	}
	e := engine.NewEdge(from, to)
	if _, ok := s.Base.Trails[e]; ok {
		return false
	}
	if _, ok := s.Variant.Temporary[e]; ok {
		return false
	}
	if od, ok := s.Variant.OneWay[e]; ok && od != d {
		return false
	}
	return true
}
func (s *State) Apply(input any, dirs ...engine.Direction) (engine.Event, error) {
	var a Action
	switch x := input.(type) {
	case Action:
		a = x
	case string:
		if len(dirs) != 1 {
			return engine.Event{}, fmt.Errorf("%w: direction required", ErrInvalidAction)
		}
		a = Action{WormID: x, Direction: dirs[0]}
	default:
		return engine.Event{}, fmt.Errorf("%w: unsupported action type", ErrInvalidAction)
	}
	if s.Classic() {
		return s.Base.Step(a.WormID, a.Direction)
	}
	// Expiry is authoritative transition state, never an observation side
	// effect. This also makes a due imported trail safe before legality checks.
	s.expire()
	if !s.legal(a.WormID, a.Direction) {
		return engine.Event{}, fmt.Errorf("%w: illegal move", ErrInvalidAction)
	}
	idx := s.BaseIndex(a.WormID)
	before := s.Base.Worms[idx].Score
	start := len(s.Base.Events)
	e, err := s.Base.Step(a.WormID, a.Direction)
	if err != nil {
		return engine.Event{}, err
	}
	delta := s.Base.Worms[idx].Score - before
	for _, ev := range s.Base.Events[start:] {
		if ev.Type != "territory_captured" {
			continue
		}
		weight := s.Variant.Weights[ev.Territory]
		if weight == 0 {
			weight = 1
		}
		s.Variant.Scores[a.WormID] += weight
		delta += weight - 1
	}
	if s.Config.EnergyLimit > 0 {
		s.Variant.Energy[a.WormID]--
		if s.Variant.Energy[a.WormID] < 0 {
			s.Variant.Energy[a.WormID] = 0
		}
		if s.Variant.Energy[a.WormID] == 0 {
			s.Base.Worms[idx].Alive = false
			s.Base.Worms[idx].CRIX = engine.NOMOVE
			allDead := true
			for _, otherW := range s.Base.Worms {
				if otherW.Alive {
					allDead = false
					break
				}
			}
			if allDead {
				s.Base.GameOver = true
			}
		}
	}
	if s.Config.TemporaryTrailTTL > 0 {
		s.Variant.Temporary[e.Edge] = s.Base.Tick + s.Config.TemporaryTrailTTL
	}
	s.expire()
	team := s.Variant.Teams[a.WormID]
	if team != "" {
		s.Variant.TeamScores[team] += delta
	}
	s.recordEvent(Event{Version: CurrentVersion, Tick: s.Base.Tick, Type: e.Type, WormID: a.WormID, Edge: e.Edge, Energy: s.Variant.Energy[a.WormID], Team: team, ScoreDelta: delta})
	return e, nil
}
func Apply(s *State, input any, dirs ...engine.Direction) (engine.Event, error) {
	if s == nil {
		return engine.Event{}, ErrInvalidAction
	}
	return s.Apply(input, dirs...)
}
func (s *State) Step(id string, d engine.Direction) (engine.Event, error) {
	return s.Apply(Action{WormID: id, Direction: d})
}

// AdvanceRound preserves the engine's deterministic controller order while
// applying extension expiry and replay bookkeeping around the transition.
// AdvanceRound dispatches controllers in engine slot order, but validates and
// applies every selected move through the extension legality boundary.
func (s *State) AdvanceRound() ([]engine.Event, error) {
	if s == nil {
		return nil, ErrInvalidAction
	}
	if s.Classic() {
		return s.Base.AdvanceRound()
	}
	if s.Base.Pending != nil {
		return nil, &engine.TransitionError{Reason: engine.RejectPendingDecision}
	}
	if s.Base.GameOver {
		return nil, &engine.TransitionError{Reason: engine.RejectGameOver}
	}
	before := s.Snapshot()
	startEvents := len(s.Base.Events)
	start := 0
	if s.Base.ActiveSlot >= 0 {
		start = s.Base.ActiveSlot
	}
	_, err := s.advanceSlots(start)
	if err != nil {
		*s = before
		return nil, err
	}
	if s.Base.Pending != nil {
		return append([]engine.Event(nil), s.Base.Events[startEvents:]...), nil
	}
	s.Base.ActiveSlot = -1
	s.Base.Round++
	s.emitBaseEvent(engine.Event{Type: "round_completed", Slot: -1})
	if s.aliveCount() == 0 {
		s.Base.GameOver = true
		s.emitBaseEvent(engine.Event{Type: "game_over"})
	}
	s.recordEvent(Event{Version: CurrentVersion, Tick: s.Base.Tick, Type: "round_advanced"})
	return append([]engine.Event(nil), s.Base.Events[startEvents:]...), nil
}

// Submit resolves a pending NEW/GETNEW decision and continues the same
// extension-aware round. It is the teaching/action boundary for callers that
// must not mutate Base directly.
func (s *State) Submit(d engine.Direction) ([]engine.Event, error) {
	if s == nil {
		return nil, ErrInvalidAction
	}
	if s.Classic() {
		return s.Base.Submit(d)
	}
	if s.Base.Pending == nil {
		return nil, &engine.TransitionError{Reason: engine.RejectPendingDecision}
	}
	p := *s.Base.Pending
	if p.Frozen || p.Slot < 0 || p.Slot >= len(s.Base.Worms) {
		return nil, &engine.TransitionError{Reason: engine.RejectFrozenUnknown, WormID: p.WormID, Dir: d}
	}
	if !d.Valid() || !containsDirection(s.LegalMoves(p.WormID), d) {
		return nil, &engine.TransitionError{Reason: engine.RejectOccupiedSpoke, WormID: p.WormID, Dir: d}
	}
	before := s.Snapshot()
	start := len(s.Base.Events)
	w := &s.Base.Worms[p.Slot]
	w.Rules[p.Mask] = engine.Action(d)
	s.emitBaseEvent(engine.Event{Type: "rule_learned", WormID: w.ID, Slot: p.Slot, RuleMask: p.Mask, RuleAction: engine.Action(d), BrainID: w.BrainID, BrainVersion: w.BrainVersion, Provenance: "new"})
	s.Base.Pending = nil
	s.Base.ActiveSlot = p.Slot + 1
	w.RuleUses[p.Mask]++
	s.emitBaseEvent(engine.Event{Type: "rule_used", WormID: w.ID, Slot: p.Slot, RuleMask: p.Mask, RuleAction: engine.Action(d), UseCount: w.RuleUses[p.Mask], BrainID: w.BrainID, BrainVersion: w.BrainVersion})
	if _, err := s.Apply(Action{WormID: w.ID, Direction: d}); err != nil {
		*s = before
		return nil, err
	}
	events, err := s.advanceSlots(p.Slot + 1)
	if err != nil {
		*s = before
		return nil, err
	}
	if s.Base.Pending != nil {
		return append([]engine.Event(nil), s.Base.Events[start:]...), nil
	}
	s.Base.ActiveSlot = -1
	s.Base.Round++
	s.emitBaseEvent(engine.Event{Type: "round_completed", Slot: -1})
	if s.aliveCount() == 0 {
		s.Base.GameOver = true
		s.emitBaseEvent(engine.Event{Type: "game_over"})
	}
	s.recordEvent(Event{Version: CurrentVersion, Tick: s.Base.Tick, Type: "round_advanced"})
	_ = events
	return append([]engine.Event(nil), s.Base.Events[start:]...), nil
}

func (s *State) advanceSlots(start int) ([]engine.Event, error) {
	before := len(s.Base.Events)
	for slot := start; slot < len(s.Base.Worms); slot++ {
		s.Base.ActiveSlot = slot
		w := &s.Base.Worms[slot]
		if !w.Alive {
			continue
		}
		m := s.Base.Mask(w.Position) & 63
		legal := s.LegalMoves(w.ID)
		if len(legal) == 0 {
			w.Alive = false
			w.CRIX = engine.NOMOVE
			s.emitBaseEvent(engine.Event{Type: "worm_blocked", WormID: w.ID, To: w.Position})
			continue
		}
		a := w.Rules[m]
		if a == engine.ActionGetNew {
			s.Base.Pending = &engine.Decision{WormID: w.ID, Mask: m, Slot: slot, Request: uint64(len(s.Base.Events) + 1), Reason: "unknown_pattern", Frozen: w.Frozen}
			s.emitBaseEvent(engine.Event{Type: "decision_pending", WormID: w.ID, Mask: m, Slot: slot, Request: s.Base.Pending.Request, BrainID: w.BrainID, BrainVersion: w.BrainVersion, Provenance: s.Base.Pending.Reason})
			return append([]engine.Event(nil), s.Base.Events[before:]...), nil
		}
		if a == engine.ActionDoAI {
			d := s.chooseAuto(slot, legal)
			a = engine.Action(d)
			if !w.Frozen {
				w.Rules[m] = a
				s.emitBaseEvent(engine.Event{Type: "rule_learned", WormID: w.ID, Slot: slot, RuleMask: m, RuleAction: a, BrainID: w.BrainID, BrainVersion: w.BrainVersion, Provenance: "auto"})
			}
		}
		if a == engine.ActionDie {
			w.Alive = false
			w.CRIX = engine.NOMOVE
			s.emitBaseEvent(engine.Event{Type: "worm_died", WormID: w.ID, To: w.Position})
			continue
		}
		if a < 0 || !containsDirection(legal, engine.Direction(a)) {
			if w.Controller == engine.ControllerWild {
				a = engine.Action(s.chooseAuto(slot, legal))
			} else {
				return nil, &engine.IllegalRuleError{WormID: w.ID, Mask: m, Action: a}
			}
		}
		w.RuleUses[m]++
		s.emitBaseEvent(engine.Event{Type: "rule_used", WormID: w.ID, Slot: slot, RuleMask: m, RuleAction: a, UseCount: w.RuleUses[m], BrainID: w.BrainID, BrainVersion: w.BrainVersion})
		if _, err := s.Apply(Action{WormID: w.ID, Direction: engine.Direction(a)}); err != nil {
			return nil, err
		}
	}
	return append([]engine.Event(nil), s.Base.Events[before:]...), nil
}
func (s *State) chooseAuto(slot int, legal []engine.Direction) engine.Direction {
	if len(legal) == 1 {
		return legal[0]
	}
	w := s.Base.Worms[slot]
	best := -1 << 30
	ties := make([]engine.Direction, 0, len(legal))
	for _, d := range legal {
		trial := s.Snapshot()
		before := trial.Variant.Scores[w.ID]
		if _, err := trial.Apply(Action{WormID: w.ID, Direction: d}); err != nil {
			continue
		}
		score := (trial.Variant.Scores[w.ID]-before)*100 + len(trial.LegalMoves(w.ID))
		if score > best {
			best, ties = score, ties[:0]
		}
		if score == best {
			ties = append(ties, d)
		}
	}
	if len(ties) == 0 {
		return legal[0]
	}
	x := w.BrainSeed ^ uint64(s.Base.Round+1)<<32 ^ uint64(s.Base.Tick+1)<<16 ^ uint64(slot+1)
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	return ties[int(x%uint64(len(ties)))]
}

func containsDirection(ds []engine.Direction, want engine.Direction) bool {
	for _, d := range ds {
		if d == want {
			return true
		}
	}
	return false
}

func (s *State) aliveCount() int {
	n := 0
	for _, w := range s.Base.Worms {
		if w.Alive {
			n++
		}
	}
	return n
}

func (s *State) emitBaseEvent(e engine.Event) engine.Event {
	e.Version = engine.EventVersion
	e.Seq = uint64(len(s.Base.Events) + 1)
	e.Tick = s.Base.Tick
	e.Round = s.Base.Round
	s.Base.Events = append(s.Base.Events, e)
	return e
}

func (s *State) Tick() ([]engine.Event, error) { return s.AdvanceRound() }
func (s *State) expire() {
	for e, x := range s.Variant.Temporary {
		if x > s.Base.Tick {
			continue
		}
		delete(s.Variant.Temporary, e)
		delete(s.Base.Trails, e)
		for _, p := range []engine.Point{e.A, e.B} {
			t := s.Base.Territories[p]
			d := direction(s.Base, p, other(e, p))
			t.Mask &^= 1 << d
			if t.Mask != 63 && t.Owner != "" {
				owner := t.Owner
				weight := s.Variant.Weights[p]
				if weight == 0 {
					weight = 1
				}
				if i := s.BaseIndex(owner); i >= 0 && s.Base.Worms[i].Score > 0 {
					s.Base.Worms[i].Score--
				}
				s.Variant.Scores[owner] -= weight
				if team := s.Variant.Teams[owner]; team != "" {
					s.Variant.TeamScores[team] -= weight
				}
				t.Owner, t.Color = "", ""
			}
			s.Base.Territories[p] = t
		}
		s.recordEvent(Event{Version: CurrentVersion, Tick: s.Base.Tick, Type: "trail_expired", Edge: e, Expiry: x})
	}
}
func other(e engine.Edge, p engine.Point) engine.Point {
	if e.A == p {
		return e.B
	}
	return e.A
}
func (s *State) generateWorld() {
	r := rand.New(rand.NewPCG(uint64(s.Seed), uint64(s.Seed)^0x9e3779b97f4a7c15))
	reservedStarts := initialPositions(s.Base)
	for _, p := range points(s.Base) {
		if _, reserved := reservedStarts[p]; reserved {
			continue
		}
		reserved := false
		for _, x := range s.Config.OneWayTrails {
			if p == x.From || p == x.To {
				reserved = true
				break
			}
		}
		if reserved {
			continue
		}
		n := r.IntN(100)
		if n < int(s.Config.HoleRate) {
			s.Variant.Holes[p] = true
		} else if n < int(s.Config.HoleRate+s.Config.ObstacleRate) {
			s.Variant.Obstacles[p] = true
		}
	}
}

// ClientConfig contains policy visible to clients. Terrain coordinates,
// generation rates, one-way edges, weights, and the seed are intentionally
// absent so fog clients cannot reconstruct hidden terrain.
type ClientConfig struct {
	Version           int               `json:"version"`
	Enabled           bool              `json:"enabled,omitempty"`
	Width             int               `json:"width,omitempty"`
	Height            int               `json:"height,omitempty"`
	TemporaryTrailTTL uint64            `json:"temporary_trail_ttl,omitempty"`
	EnergyLimit       int               `json:"energy_limit,omitempty"`
	Teams             map[string]string `json:"teams,omitempty"`
	FogOfWar          bool              `json:"fog_of_war,omitempty"`
	VisibilityRadius  int               `json:"visibility_radius,omitempty"`
}

type ClientProjection struct {
	Version     int            `json:"version"`
	WormID      string         `json:"worm_id"`
	Config      ClientConfig   `json:"config"`
	Observation Observation    `json:"observation"`
	TeamWinners []string       `json:"team_winners,omitempty"`
	TeamScores  map[string]int `json:"team_scores,omitempty"`
	Score       int            `json:"score"`
}

func (s State) ClientProjection(id string) (ClientProjection, error) {
	o, err := s.Observe(id)
	if err != nil {
		return ClientProjection{}, err
	}
	projection := ClientProjection{
		Version: ObservationVersion, WormID: id, Config: s.Config.SafeClientConfig(),
		Observation: o, Score: s.Score(id),
	}
	if !s.Config.FogOfWar {
		projection.TeamWinners = s.TeamWinners()
		projection.TeamScores = s.TeamTotals()
	}
	return projection, nil
}
func initialPositions(s engine.State) map[engine.Point]bool {
	out := map[engine.Point]bool{}
	seen := map[string]bool{}
	for _, ev := range s.Events {
		if ev.Type == "worm_moved" && !seen[ev.WormID] {
			out[ev.From] = true
			seen[ev.WormID] = true
		}
	}
	for _, w := range s.Worms {
		if !seen[w.ID] && w.Alive {
			out[w.Position] = true
		}
	}
	return out
}

// Observation retains the classic local values and adds only values visible to
// the requesting worm. Unknown cells/edges are represented by Visible=false;
// authoritative hidden coordinates, owners, scores, and energy are omitted.
type Observation struct {
	Version      int                `json:"version"`
	WormID       string             `json:"worm_id"`
	Base         engine.Observation `json:"base"`
	Visible      []VisibleCell      `json:"visible,omitempty"`
	UnknownCount int                `json:"unknown_count,omitempty"`
	TeamScore    int                `json:"team_score,omitempty"`
	Energy       *int               `json:"energy,omitempty"`
}
type VisibleCell struct {
	Point          engine.Point `json:"point"`
	Visible        bool         `json:"visible"`
	Obstacle       bool         `json:"obstacle,omitempty"`
	Hole           bool         `json:"hole,omitempty"`
	TerritoryScore int          `json:"territory_score,omitempty"`
}

func (s State) Observe(id string) (Observation, error) {
	o, err := s.Base.Observe(id)
	if err != nil {
		return Observation{}, err
	}
	out := Observation{Version: ObservationVersion, WormID: id, Base: o}
	if s.Classic() {
		return out, nil
	}
	out.Base.Legal = s.LegalMoves(id)
	w, _ := worm(s.Base, id)
	if s.Config.EnergyLimit > 0 {
		x := s.Variant.Energy[id]
		out.Energy = &x
	}
	team := s.Variant.Teams[id]
	out.TeamScore = s.Variant.TeamScores[team]
	radius := s.Config.VisibilityRadius
	if s.Config.FogOfWar {
		out.Base.Scores = []int{s.Variant.Scores[id]}
		if s.Config.VisibilityRadius == 0 {
			out.Base.LocalTerritoryCounts = [6]uint8{}
		}
	}
	if !s.Config.FogOfWar {
		radius = -1
	}
	for _, p := range points(s.Base) {
		visible := radius < 0 || distance(s.Base, w.Position, p) <= radius || p == w.Position
		if visible {
			out.Visible = append(out.Visible, VisibleCell{Point: p, Visible: true, Obstacle: s.Variant.Obstacles[p], Hole: s.Variant.Holes[p], TerritoryScore: s.Variant.Weights[p]})
		} else {
			out.Visible = append(out.Visible, VisibleCell{Point: p, Visible: false})
			out.UnknownCount++
		}
	}
	return out, nil
}

// PlannerObservation is the visibility-safe planning input. Callers must use
// LegalMoves/ApplyPlannerAction for extension legality instead of mutating
// the embedded base state.
func (s State) PlannerObservation(id string) (Observation, error) {
	return s.Observe(id)
}

func (s State) PlannerLegalMoves(id string) []engine.Direction {
	return s.LegalMoves(id)
}

func (s *State) ApplyPlannerAction(id string, d engine.Direction) (engine.Event, error) {
	if s == nil {
		return engine.Event{}, ErrInvalidAction
	}
	if !containsDirection(s.LegalMoves(id), d) {
		return engine.Event{}, fmt.Errorf("%w: planner action is not extension-legal", ErrInvalidAction)
	}
	return s.Apply(Action{WormID: id, Direction: d})
}

func (s State) MarshalSnapshot() ([]byte, error) {
	if s.Classic() {
		return s.Base.MarshalSnapshot()
	}
	return marshal(s)
}
func marshal(s State) ([]byte, error) {
	base, e := s.Base.MarshalSnapshot()
	if e != nil {
		return nil, e
	}
	type wire struct {
		Base              json.RawMessage `json:"base"`
		Config            Config          `json:"config"`
		Seed              int64           `json:"seed"`
		Variant           VariantState    `json:"variant"`
		Events            []Event         `json:"events,omitempty"`
		GameEventSequence int64           `json:"game_event_sequence,omitempty"`
		GameEventHash     string          `json:"game_event_hash,omitempty"`
	}
	raw, e := json.Marshal(wire{
		Base: base, Config: copyConfig(s.Config), Seed: s.Seed, Variant: s.Variant,
		Events: s.Events, GameEventSequence: s.GameEventSequence, GameEventHash: s.GameEventHash,
	})
	if e != nil {
		return nil, e
	}
	return json.Marshal(envelope{CurrentVersion, raw, hash(raw)})
}
func Marshal(s State) ([]byte, error)    { return s.MarshalSnapshot() }
func (s State) Marshal() ([]byte, error) { return s.MarshalSnapshot() }
func UnmarshalSnapshot(b []byte) (State, error) {
	if parsed, e := engine.UnmarshalSnapshot(b); e == nil {
		return New(parsed, Config{}, 0)
	}
	var x envelope
	if err := json.Unmarshal(b, &x); err != nil {
		return State{}, err
	}
	if x.Version != CurrentVersion {
		return State{}, fmt.Errorf("%w: %d", ErrUnsupportedVersion, x.Version)
	}
	if x.Hash == "" {
		return State{}, errors.New("extension: missing state hash")
	}
	if hash(x.Data) != x.Hash {
		return State{}, fmt.Errorf("extension: state hash mismatch")
	}
	var wire struct {
		Base              json.RawMessage `json:"base"`
		Config            Config          `json:"config"`
		Seed              int64           `json:"seed"`
		Variant           VariantState    `json:"variant"`
		Events            []Event         `json:"events,omitempty"`
		GameEventSequence int64           `json:"game_event_sequence,omitempty"`
		GameEventHash     string          `json:"game_event_hash,omitempty"`
	}
	if err := json.Unmarshal(x.Data, &wire); err != nil {
		return State{}, err
	}
	base, err := engine.UnmarshalSnapshot(wire.Base)
	if err != nil {
		return State{}, err
	}
	s, err := New(base, wire.Config, wire.Seed)
	if err != nil {
		return State{}, err
	}
	s.Variant = copyVariant(wire.Variant)
	s.Events = append([]Event(nil), wire.Events...)
	s.GameEventSequence = wire.GameEventSequence
	s.GameEventHash = wire.GameEventHash
	if err = s.Validate(); err != nil {
		return State{}, err
	}
	return s, nil
}
func Unmarshal(b []byte) (State, error) { return UnmarshalSnapshot(b) }
func (s State) HashHex() string {
	if s.Classic() {
		return s.Base.HashHex()
	}
	b, _ := s.MarshalSnapshot()
	return hash(b)
}
func (s State) StateHash() [32]byte {
	if s.Classic() {
		return s.Base.StateHash()
	}
	b, _ := s.MarshalSnapshot()
	return sha256.Sum256(b)
}

type envelope struct {
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
	Hash    string          `json:"hash"`
}

func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (s State) Snapshot() State {
	out := s
	out.Base = s.Base.Snapshot()
	out.Config = copyConfig(s.Config)
	out.Variant = copyVariant(s.Variant)
	out.Events = append([]Event(nil), s.Events...)
	return out
}
func (s State) EventsCopy() []Event { return append([]Event(nil), s.Events...) }
func (s State) TeamWinners() []string {
	if len(s.Variant.TeamScores) == 0 {
		if len(s.Variant.Weights) == 0 {
			return s.Base.Winners()
		}
		max := -1
		for _, score := range s.Variant.Scores {
			if score > max {
				max = score
			}
		}
		out := []string{}
		for id, score := range s.Variant.Scores {
			if score == max {
				out = append(out, id)
			}
		}
		sort.Strings(out)
		return out
	}
	max := -1
	for _, v := range s.Variant.TeamScores {
		if v > max {
			max = v
		}
	}
	out := []string{}
	for id, v := range s.Variant.TeamScores {
		if v == max {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
func (s State) Score(id string) int {
	if v, ok := s.Variant.Scores[id]; ok {
		return v
	}
	for _, w := range s.Base.Worms {
		if w.ID == id {
			return w.Score
		}
	}
	return 0
}

func (s State) Scores() map[string]int {
	out := map[string]int{}
	for _, w := range s.Base.Worms {
		out[w.ID] = s.Score(w.ID)
	}
	return out
}

func (s State) TeamTotals() map[string]int {
	out := map[string]int{}
	for team, score := range s.Variant.TeamScores {
		out[team] = score
	}
	return out
}

func (s State) ParticipantScores() map[string]int { return s.Scores() }
func (s State) TeamScores() map[string]int        { return s.TeamTotals() }
func (s State) EffectiveSeed() int64              { return s.Seed }
func (s State) SeedValue() int64                  { return s.Seed }

func (s State) ParticipantWinners() []string {
	scores := s.Scores()
	max := -1
	for _, score := range scores {
		if score > max {
			max = score
		}
	}
	out := []string{}
	for id, score := range scores {
		if score == max {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
func equalPointBool(a, b map[engine.Point]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func equalPointInt(a, b map[engine.Point]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func equalEdgeDir(a, b map[engine.Edge]engine.Direction) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func equalStringInt(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func equalStringString(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func copyConfig(c Config) Config {
	c.Obstacles = append([]engine.Point(nil), c.Obstacles...)
	c.Holes = append([]engine.Point(nil), c.Holes...)
	c.OneWayTrails = append([]OneWayTrail(nil), c.OneWayTrails...)
	c.WeightedTerritories = copyPointInt(c.WeightedTerritories)
	c.Teams = copyStringString(c.Teams)
	return c
}

func copyVariant(v VariantState) VariantState {
	out := v
	out.Obstacles = copyPointBool(v.Obstacles)
	out.Holes = copyPointBool(v.Holes)
	out.Weights = copyPointInt(v.Weights)
	out.OneWay = copyEdgeDir(v.OneWay)
	out.Temporary = copyEdgeTTL(v.Temporary)
	out.Energy = copyStringInt(v.Energy)
	out.Teams = copyStringString(v.Teams)
	out.Scores = copyStringInt(v.Scores)
	out.TeamScores = copyStringInt(v.TeamScores)
	return out
}
func copyPointBool(m map[engine.Point]bool) map[engine.Point]bool {
	o := map[engine.Point]bool{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
func copyPointInt(m map[engine.Point]int) map[engine.Point]int {
	o := map[engine.Point]int{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
func copyEdgeDir(m map[engine.Edge]engine.Direction) map[engine.Edge]engine.Direction {
	o := map[engine.Edge]engine.Direction{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
func copyEdgeTTL(m map[engine.Edge]uint64) map[engine.Edge]uint64 {
	o := map[engine.Edge]uint64{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
func copyStringInt(m map[string]int) map[string]int {
	o := map[string]int{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
func copyStringString(m map[string]string) map[string]string {
	o := map[string]string{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
func inBounds(s engine.State, p engine.Point) bool {
	return contains(points(s), p)
}
func points(s engine.State) []engine.Point {
	out := make([]engine.Point, 0, s.Width*s.Height)
	start := 0
	if s.Mode == engine.ClassicRules {
		start = 1
	}
	for q := start; q < start+s.Width; q++ {
		for r := start; r < start+s.Height; r++ {
			out = append(out, engine.PointXY(q, r))
		}
	}
	return out
}
func contains(a []engine.Point, p engine.Point) bool {
	for _, x := range a {
		if x == p {
			return true
		}
	}
	return false
}
func worm(s engine.State, id string) (engine.Worm, bool) {
	for _, w := range s.Worms {
		if w.ID == id {
			return w, true
		}
	}
	return engine.Worm{}, false
}
func hasWorm(s engine.State, p engine.Point) bool {
	for _, w := range s.Worms {
		if w.Alive && w.Position == p {
			return true
		}
	}
	return false
}
func adjacent(s engine.State, a, b engine.Point) bool {
	for d := engine.East; d <= engine.NorthEast; d++ {
		if s.Neighbor(a, d) == b {
			return true
		}
	}
	return false
}
func direction(s engine.State, a, b engine.Point) engine.Direction {
	for d := engine.East; d <= engine.NorthEast; d++ {
		if s.Neighbor(a, d) == b {
			return d
		}
	}
	return engine.East
}
func pointLess(a, b engine.Point) bool { return a.Q < b.Q || a.Q == b.Q && a.R < b.R }
func distance(s engine.State, a, b engine.Point) int {
	if a == b {
		return 0
	}
	seen := map[engine.Point]bool{a: true}
	frontier := []engine.Point{a}
	for d := 1; len(frontier) > 0; d++ {
		next := make([]engine.Point, 0, len(frontier)*2)
		for _, p := range frontier {
			for dir := engine.East; dir <= engine.NorthEast; dir++ {
				q := s.Neighbor(p, dir)
				if !inBounds(s, q) || seen[q] {
					continue
				}
				if q == b {
					return d
				}
				seen[q] = true
				next = append(next, q)
			}
		}
		frontier = next
	}
	return int(^uint(0) >> 1)
}
func edgeLess(a, b engine.Edge) bool {
	if a.A != b.A {
		return pointLess(a.A, b.A)
	}
	return pointLess(a.B, b.B)
}
func (s State) BaseIndex(id string) int {
	for i, w := range s.Base.Worms {
		if w.ID == id {
			return i
		}
	}
	return -1
}
