// Package experiment orchestrates reproducible sharing and planner experiments.
// It is intentionally a thin boundary around sharing, planner, tournament, and
// store: runners decide games, while this package owns definitions and lineage.
package experiment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"worms.ng/internal/engine"
	"worms.ng/internal/match"
	"worms.ng/internal/planner"
	"worms.ng/internal/sharing"
	"worms.ng/internal/store"
	"worms.ng/internal/tournament"
)

var (
	ErrInvalid            = errors.New("experiment: invalid configuration")
	ErrSeedLeakage        = planner.ErrSeedLeakage
	ErrDefinitionMismatch = errors.New("experiment: persisted definition differs")
	ErrFrozen             = errors.New("experiment: held-out brain changed")
)

// Sharing aliases keep experiment manifests independent of the sharing package
// while preserving its stable persisted policy values.
type Policy = sharing.Policy
type Metrics = sharing.Metrics
type Observation = sharing.Observation
type Comparison = sharing.Comparison

const (
	NoSharing          = sharing.NoSharing
	SameTeamSharing    = sharing.SameTeamSharing
	AllWormSharing     = sharing.AllWormSharing
	SeededNoisySharing = sharing.SeededNoisySharing
)

// Participant is one fixed slot in every matched board.
type Participant struct {
	ID             string       `json:"id"`
	BrainVersionID string       `json:"brain_version_id,omitempty"`
	Color          engine.Color `json:"color,omitempty"`
	Team           string       `json:"team,omitempty"`
}

// PolicyDefinition combines one sharing policy with its immutable source
// versions. Sources are optional for a runner-only experiment.
type PolicyDefinition struct {
	Name         sharing.Policy `json:"name"`
	Sharing      sharing.Config `json:"sharing"`
	Participants []Participant  `json:"participants,omitempty"`
}

// Config is a complete, serializable experiment definition. Callers may use
// StateFactory to construct an identical board from a seed; it is never called
// for a held-out seed during planner training.
type Config struct {
	ID                string                                                                 `json:"id,omitempty"`
	Name              string                                                                 `json:"name"`
	Seed              int64                                                                  `json:"seed"`
	State             engine.State                                                           `json:"state"`
	StateFactory      func(int64) (engine.State, error)                                      `json:"-"`
	Participants      []Participant                                                          `json:"participants"`
	Policies          []PolicyDefinition                                                     `json:"policies"`
	Rounds            int                                                                    `json:"rounds"`
	MaxTurns          int                                                                    `json:"max_turns"`
	Deadline          time.Duration                                                          `json:"deadline"`
	TrainingSeeds     []int64                                                                `json:"training_seeds,omitempty"`
	HeldOutSeeds      []int64                                                                `json:"held_out_seeds,omitempty"`
	Planner           planner.Config                                                         `json:"planner"`
	Train             func(context.Context, *planner.Planner, *engine.State, int64) error    `json:"-"`
	EvaluatePlanner   func(context.Context, engine.State, int64) (planner.SeedResult, error) `json:"-"`
	Store             *store.Store                                                           `json:"-"`
	Runner            Runner                                                                 `json:"-"`
	Evaluator         Evaluator                                                              `json:"-"`
	ControllerFactory func(MatchRequest) []match.Controller                                  `json:"-"`
}

// ExperimentConfig is the descriptive alias used by integration callers.
type ExperimentConfig = Config

// Board is a matched seed/color assignment. It is immutable by convention;
// State is copied before being passed to a runner.
type Board struct {
	Round  int                     `json:"round"`
	Seed   int64                   `json:"seed"`
	Colors map[string]engine.Color `json:"colors"`
	State  engine.State            `json:"state"`
}

// Schedule returns the same boards for every policy. Seed derivation matches
// tournament's stable round derivation, and color rotation is explicit rather
// than left to map or participant iteration order.
func Schedule(seed int64, rounds int, state engine.State, participants []Participant) []Board {
	if len(participants) == 0 {
		return nil
	}
	if rounds <= 0 {
		rounds = 1
	}
	palette := make([]engine.Color, len(participants))
	for i, p := range participants {
		palette[i] = p.Color
		if palette[i] == "" {
			palette[i] = engine.Color(fmt.Sprintf("color-%d", i))
		}
	}
	out := make([]Board, 0, rounds)
	for round := 0; round < rounds; round++ {
		colors := make(map[string]engine.Color, len(participants))
		s := state.Snapshot()
		for i, p := range participants {
			c := palette[(i+round)%len(palette)]
			colors[p.ID] = c
			if i < len(s.Worms) {
				s.Worms[i].ID, s.Worms[i].Color, s.Worms[i].Alive = p.ID, c, true
			}
		}
		if len(s.Worms) > len(participants) {
			s.Worms = s.Worms[:len(participants)]
		}
		out = append(out, Board{Round: round, Seed: deriveSeed(seed, round), Colors: colors, State: s})
	}
	return out
}

// ScheduleBoards is an alias retained for callers that prefer an imperative name.
func ScheduleBoards(seed int64, rounds int, state engine.State, participants []Participant) []Board {
	return Schedule(seed, rounds, state, participants)
}

// MatchRequest is the narrow runner boundary. A runner must not mutate the
// definition or reuse a board between policies.
type MatchRequest struct {
	ExperimentID      string             `json:"experiment_id"`
	TournamentID      string             `json:"tournament_id"`
	MatchID           string             `json:"match_id"`
	Policy            sharing.Policy     `json:"policy"`
	Board             Board              `json:"board"`
	Participants      []Participant      `json:"participants"`
	Store             *store.Store       `json:"-"`
	MaxTurns          int                `json:"max_turns"`
	Deadline          time.Duration      `json:"deadline"`
	Controllers       []match.Controller `json:"-"`
	FrozenBrainHashes map[string]string  `json:"frozen_brain_hashes,omitempty"`
}

// MatchResult is the runner's serializable result. ReplayID is an opaque store
// game ID and is persisted verbatim; hashes identify the exact frozen brains.
type MatchResult struct {
	Policy          sharing.Policy    `json:"policy"`
	Round           int               `json:"round"`
	Seed            int64             `json:"seed"`
	MatchID         string            `json:"match_id"`
	ReplayID        string            `json:"replay_id,omitempty"`
	Status          string            `json:"status"`
	Scores          map[string]int    `json:"scores,omitempty"`
	Winners         []string          `json:"winners,omitempty"`
	Turns           int               `json:"turns,omitempty"`
	MoveCount       int               `json:"move_count,omitempty"`
	Survived        bool              `json:"survived"`
	KnownPatterns   int               `json:"known_patterns,omitempty"`
	UnknownPatterns int               `json:"unknown_patterns,omitempty"`
	BrainVersionIDs map[string]string `json:"brain_version_ids,omitempty"`
	BrainHashes     map[string]string `json:"brain_hashes,omitempty"`
}

// Runner executes one board. The interface deliberately contains no store
// methods, so deterministic fakes and production tournament runners share it.
type Runner interface {
	Run(context.Context, MatchRequest) (MatchResult, error)
}
type RunnerFunc func(context.Context, MatchRequest) (MatchResult, error)

func (f RunnerFunc) Run(ctx context.Context, r MatchRequest) (MatchResult, error) { return f(ctx, r) }

// Evaluator can add domain observations without widening Runner.
type Evaluator interface {
	Evaluate(context.Context, MatchResult) (sharing.Observation, error)
}
type EvaluatorFunc func(context.Context, MatchResult) (sharing.Observation, error)

func (f EvaluatorFunc) Evaluate(ctx context.Context, r MatchResult) (sharing.Observation, error) {
	return f(ctx, r)
}

// PolicyReport and Report are deterministic comparison artifacts.
type PolicyReport struct {
	Policy          sharing.Policy    `json:"policy"`
	TournamentIDs   []string          `json:"tournament_ids"`
	Matches         []MatchResult     `json:"matches"`
	BrainVersionIDs map[string]string `json:"brain_version_ids,omitempty"`
	BrainHashes     map[string]string `json:"brain_hashes,omitempty"`
	Metrics         sharing.Metrics   `json:"metrics"`
}
type Report struct {
	ExperimentID   string                          `json:"experiment_id"`
	DefinitionHash string                          `json:"definition_hash"`
	Definition     json.RawMessage                 `json:"definition"`
	Planner        planner.HeldOutResult           `json:"planner"`
	Policies       map[sharing.Policy]PolicyReport `json:"policies"`
	Comparison     sharing.Comparison              `json:"comparison"`
}
type ComparisonReport = Report

func (c Config) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("%w: name", ErrInvalid)
	}
	if len(c.Participants) < 2 {
		return fmt.Errorf("%w: participants", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, p := range c.Participants {
		if p.ID == "" || seen[p.ID] {
			return fmt.Errorf("%w: participant id", ErrInvalid)
		}
		seen[p.ID] = true
	}
	if err := planner.ValidateSeedPartitions(c.TrainingSeeds, c.HeldOutSeeds); err != nil {
		return err
	}
	if len(c.Policies) == 0 {
		return fmt.Errorf("%w: policies", ErrInvalid)
	}
	ps := map[sharing.Policy]bool{}
	for _, p := range c.Policies {
		if p.Name == "" || ps[p.Name] {
			return fmt.Errorf("%w: policy", ErrInvalid)
		}
		ps[p.Name] = true
	}
	return nil
}

func (c Config) stateFor(seed int64) (engine.State, error) {
	if c.StateFactory != nil {
		return c.StateFactory(seed)
	}
	s := c.State.Snapshot()
	if len(s.Worms) == 0 {
		return engine.State{}, fmt.Errorf("%w: state or state factory", ErrInvalid)
	}
	return s, nil
}

// Run executes sharing derivation, planner train/held-out evaluation, matched
// boards, and frozen policy evaluation. Every store-backed artifact is resumed
// by deterministic tournament/match IDs rather than database-generated IDs.
func Run(ctx context.Context, c Config) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.Name == "" {
		c.Name = "experiment"
	}
	if len(c.Policies) == 0 {
		c.Policies = []PolicyDefinition{{Name: NoSharing}}
	}
	if len(c.Participants) == 0 && len(c.Policies[0].Participants) > 0 {
		c.Participants = append([]Participant(nil), c.Policies[0].Participants...)
	}
	if err := c.Validate(); err != nil {
		return Report{}, err
	}
	state, err := c.stateFor(c.Seed)
	if err != nil {
		return Report{}, err
	}
	definition, err := canonicalDefinition(c, state)
	if err != nil {
		return Report{}, err
	}
	defHash := hash(definition)
	expID := c.ID
	if expID == "" {
		expID = "experiment-" + defHash[:24]
	}
	report := Report{ExperimentID: expID, DefinitionHash: defHash, Definition: definition, Policies: map[sharing.Policy]PolicyReport{}}
	if len(c.TrainingSeeds) > 0 || len(c.HeldOutSeeds) > 0 {
		pc := planner.HeldOutConfig{Planner: c.Planner, TrainingSeeds: append([]int64(nil), c.TrainingSeeds...), HeldOutSeeds: append([]int64(nil), c.HeldOutSeeds...), NewState: c.StateFactory, Train: c.Train, Evaluate: c.EvaluatePlanner}
		if pc.NewState == nil {
			pc.NewState = func(int64) (engine.State, error) { return state.Snapshot(), nil }
		}
		report.Planner, err = planner.RunHeldOut(ctx, pc)
		if err != nil {
			return report, err
		}
	}
	boards := Schedule(c.Seed, c.Rounds, state, c.Participants)
	for _, pd := range c.Policies {
		participants := append([]Participant(nil), pd.Participants...)
		if len(participants) == 0 {
			participants = append([]Participant(nil), c.Participants...)
		}
		if c.Store != nil && len(pd.Sharing.Sources) > 0 {
			participants, err = deriveParticipants(ctx, c.Store, pd.Sharing, participants)
			if err != nil {
				return report, err
			}
		}
		pr, err := runPolicy(ctx, c, expID, defHash, definition, pd.Name, participants, boards)
		if err != nil {
			return report, err
		}
		report.Policies[pd.Name] = pr
	}
	observations := make([]sharing.Observation, 0)
	keys := make([]string, 0, len(report.Policies))
	for p := range report.Policies {
		keys = append(keys, string(p))
	}
	sort.Strings(keys)
	for _, k := range keys {
		pr := report.Policies[sharing.Policy(k)]
		for _, m := range pr.Matches {
			observations = append(observations, observationFor(pr.Policy, m, c.Evaluator))
		}
	}
	report.Comparison = sharing.CompareMetrics(observations)
	return report, nil
}

// RunExperiment is an alias for Run.
func RunExperiment(ctx context.Context, c Config) (Report, error) { return Run(ctx, c) }

func observationFor(policy sharing.Policy, r MatchResult, e Evaluator) sharing.Observation {
	if e != nil {
		if x, err := e.Evaluate(context.Background(), r); err == nil {
			x.Policy = policy
			return x
		}
	}
	score := 0
	for _, v := range r.Scores {
		score += v
	}
	n := len(r.Scores)
	if n > 0 {
		score /= n
	}
	return sharing.Observation{Policy: policy, Score: float64(score), Survived: r.Survived, KnownPatterns: r.KnownPatterns, UnknownPatterns: r.UnknownPatterns}
}

func runPolicy(ctx context.Context, c Config, expID, defHash string, definition json.RawMessage, policy sharing.Policy, participants []Participant, boards []Board) (PolicyReport, error) {
	out := PolicyReport{Policy: policy, BrainVersionIDs: map[string]string{}, BrainHashes: map[string]string{}}
	frozenHashes := map[string]string{}
	for _, p := range participants {
		if p.BrainVersionID != "" {
			out.BrainVersionIDs[p.ID] = p.BrainVersionID
			if c.Store != nil {
				v, err := c.Store.GetBrainVersion(ctx, p.BrainVersionID)
				if err != nil {
					return out, err
				}
				// Reports are keyed by participant, while immutable freeze
				// verification must be keyed by the version being frozen.
				out.BrainHashes[p.ID] = v.Hash
				frozenHashes[p.BrainVersionID] = v.Hash
			}
		}
	}
	runner := c.Runner
	if runner == nil {
		runner = TournamentRunner{}
	}
	for _, b := range boards {
		tid := "tournament-" + hash([]byte(expID + "\x00" + string(policy) + "\x00" + fmt.Sprint(b.Round)))[:24]
		mid := tid + "-round-0"
		out.TournamentIDs = append(out.TournamentIDs, tid)
		if c.Store != nil {
			if err := ensureTournament(ctx, c.Store, tid, c.Name, definitionPayload(defHash, definition, policy, b)); err != nil {
				return out, err
			}
		}
		req := MatchRequest{ExperimentID: expID, TournamentID: tid, MatchID: mid, Policy: policy, Board: b, Participants: participants, Store: c.Store, MaxTurns: c.MaxTurns, Deadline: c.Deadline, FrozenBrainHashes: cloneStrings(frozenHashes)}
		if c.ControllerFactory != nil {
			req.Controllers = c.ControllerFactory(req)
		}
		mr, err := loadMatch(ctx, c.Store, mid)
		if err != nil {
			return out, err
		}
		if isTerminalMatch(mr) {
			if c.Store != nil {
				if err := VerifyFrozen(ctx, c.Store, frozenHashes); err != nil {
					return out, err
				}
			}
			out.Matches = append(out.Matches, mr)
			continue
		}
		// An active/pending row is only a persisted checkpoint. The runner
		// must resume it so a partial match cannot be reported as a result.
		mr, err = runner.Run(ctx, req)
		if err != nil {
			return out, err
		}
		mr.Policy, mr.Round, mr.Seed, mr.MatchID = policy, b.Round, b.Seed, mid
		if mr.ReplayID == "" {
			mr.ReplayID = mid
		}
		if mr.BrainVersionIDs == nil {
			mr.BrainVersionIDs = cloneStrings(out.BrainVersionIDs)
		}
		if mr.BrainHashes == nil {
			mr.BrainHashes = cloneStrings(out.BrainHashes)
		}
		if c.Store != nil {
			if err := VerifyFrozen(ctx, c.Store, frozenHashes); err != nil {
				return out, err
			}
		}
		if c.Store != nil {
			if err := augmentStoredMatch(ctx, c.Store, mid, mr); err != nil && !errors.Is(err, store.ErrNotFound) {
				return out, err
			}
		}
		if c.Store != nil {
			raw, e := store.EncodePayload(mr)
			if e != nil {
				return out, e
			}
			if _, e = c.Store.CreateMatch(ctx, store.CreateMatchInput{ID: mid, TournamentID: tid, Round: 0, Status: status(mr), Payload: raw}); e != nil {
				// A concurrent/resumed writer is valid only if its exact result is present.
				existing, ge := loadMatch(ctx, c.Store, mid)
				if ge != nil || !isTerminalMatch(existing) {
					return out, e
				}
				mr = existing
			}
		}
		out.Matches = append(out.Matches, mr)
	}
	obs := make([]sharing.Observation, 0, len(out.Matches))
	for _, m := range out.Matches {
		obs = append(obs, observationFor(policy, m, c.Evaluator))
	}
	out.Metrics = sharing.AggregateMetrics(obs)
	return out, nil
}

func isTerminalMatch(r MatchResult) bool {
	if r.MatchID == "" || r.ReplayID == "" {
		return false
	}
	return r.Status == "finished" || r.Status == "stopped"
}

// TournamentRunner is the production runner. It delegates all move execution
// to tournament/match and therefore has no alternate game semantics.
type TournamentRunner struct{}

func (TournamentRunner) Run(ctx context.Context, req MatchRequest) (MatchResult, error) {
	ps := make([]tournament.Participant, len(req.Participants))
	for i, p := range req.Participants {
		var ctl match.Controller
		if i < len(req.Controllers) {
			ctl = req.Controllers[i]
		}
		color := p.Color
		if c, ok := req.Board.Colors[p.ID]; ok {
			color = c
		}
		ps[i] = tournament.Participant{ID: p.ID, BrainVersionID: p.BrainVersionID, Controller: ctl, Color: color}
	}
	t, err := tournament.NewTournament(ctx, tournament.Config{Store: req.Store, ID: req.TournamentID, Name: req.ExperimentID, State: req.Board.State, Participants: ps, Rounds: 1, Seed: req.Board.Seed, Deadline: req.Deadline, MaxTurns: req.MaxTurns})
	if err != nil {
		return MatchResult{}, err
	}
	r, err := t.Run(ctx)
	if err != nil {
		return MatchResult{}, err
	}
	if len(r.Matches) == 0 {
		return MatchResult{Status: "finished", Survived: true}, nil
	}
	m := r.Matches[0]
	out := MatchResult{Round: req.Board.Round, Seed: req.Board.Seed, MatchID: m.ID, ReplayID: m.ReplayLink, Status: m.Status, Scores: m.Scores, Winners: m.Winners, Turns: m.Turns, MoveCount: m.MoveCount, Survived: m.Status == "finished", BrainVersionIDs: m.BrainVersionIDs}
	if out.ReplayID == "" {
		out.ReplayID = m.GameID
	}
	return out, nil
}

func augmentStoredMatch(ctx context.Context, s *store.Store, id string, result MatchResult) error {
	m, err := s.GetMatch(ctx, id)
	if err != nil {
		return err
	}
	var data map[string]json.RawMessage
	if err := store.DecodePayload(m.Payload, &data); err != nil {
		return err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	data["experiment_result"] = raw
	data["brain_hashes"], _ = json.Marshal(result.BrainHashes)
	data["replay_id"], _ = json.Marshal(result.ReplayID)
	payload, err := store.EncodePayload(data)
	if err != nil {
		return err
	}
	_, err = s.DB().ExecContext(ctx, "UPDATE tournament_matches SET status=?,payload=?,updated_at=? WHERE id=?", status(result), payload, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func status(r MatchResult) string {
	if r.Status == "" {
		return "finished"
	}
	return r.Status
}
func cloneStrings(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func loadMatch(ctx context.Context, s *store.Store, id string) (MatchResult, error) {
	if s == nil {
		return MatchResult{}, nil
	}
	m, err := s.GetMatch(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return MatchResult{}, nil
	}
	if err != nil {
		return MatchResult{}, err
	}
	var r MatchResult
	// Stored tournament metadata can contain both the active checkpoint and
	// the completed experiment result. Prefer the authoritative result rather
	// than interpreting the metadata's replay link as a terminal result.
	var data map[string]json.RawMessage
	if err := store.DecodePayload(m.Payload, &data); err == nil {
		if raw, ok := data["experiment_result"]; ok {
			if err := json.Unmarshal(raw, &r); err == nil {
				r.MatchID = m.ID
				return r, nil
			}
		}
	}
	if err := store.DecodePayload(m.Payload, &r); err == nil && (r.Policy != "" || r.ReplayID != "") {
		r.MatchID = m.ID
		return r, nil
	}
	var legacy tournament.MatchReport
	if err := store.DecodePayload(m.Payload, &legacy); err != nil {
		return MatchResult{}, err
	}
	replay := legacy.ReplayLink
	if replay == "" {
		replay = legacy.GameID
	}
	return MatchResult{
		MatchID: m.ID, ReplayID: replay, Round: legacy.Round,
		Seed: legacy.Seed, Status: legacy.Status, Scores: legacy.Scores,
		Winners: legacy.Winners, Turns: legacy.Turns, MoveCount: legacy.MoveCount,
		Survived: legacy.Status == "finished", BrainVersionIDs: legacy.BrainVersionIDs,
	}, nil
}
func ensureTournament(ctx context.Context, s *store.Store, id, name string, payload json.RawMessage) error {
	got, err := s.GetTournament(ctx, id)
	if err == nil {
		if hash(got.RulesPayload) != hash(payload) {
			return fmt.Errorf("%w: tournament %s", ErrDefinitionMismatch, id)
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err = s.CreateTournament(ctx, store.CreateTournamentInput{ID: id, Name: name, Status: "active", RulesPayload: payload})
	if err != nil {
		if got, ge := s.GetTournament(ctx, id); ge == nil && hash(got.RulesPayload) == hash(payload) {
			return nil
		}
	}
	return err
}
func definitionPayload(defHash string, definition json.RawMessage, p sharing.Policy, b Board) json.RawMessage {
	raw, _ := store.EncodePayload(struct {
		DefinitionHash string                  `json:"definition_hash"`
		Definition     json.RawMessage         `json:"definition"`
		Policy         sharing.Policy          `json:"policy"`
		Round          int                     `json:"round"`
		Seed           int64                   `json:"seed"`
		Colors         map[string]engine.Color `json:"colors"`
	}{defHash, definition, p, b.Round, b.Seed, b.Colors})
	return raw
}

func canonicalDefinition(c Config, state engine.State) (json.RawMessage, error) {
	return store.EncodePayload(struct {
		Name         string             `json:"name"`
		Seed         int64              `json:"seed"`
		StateHash    string             `json:"state_hash"`
		Participants []Participant      `json:"participants"`
		Policies     []PolicyDefinition `json:"policies"`
		Rounds       int                `json:"rounds"`
		MaxTurns     int                `json:"max_turns"`
		Training     []int64            `json:"training_seeds,omitempty"`
		HeldOut      []int64            `json:"held_out_seeds,omitempty"`
		Planner      planner.Config     `json:"planner"`
	}{c.Name, c.Seed, state.HashHex(), c.Participants, c.Policies, c.Rounds, c.MaxTurns, sortedInts(c.TrainingSeeds), sortedInts(c.HeldOutSeeds), c.Planner})
}
func sortedInts(in []int64) []int64 {
	out := append([]int64(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func deriveSeed(seed int64, round int) int64 {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", seed, round)))
	var x int64
	for _, b := range h[:8] {
		x = x<<8 | int64(b)
	}
	return x
}

func deriveParticipants(ctx context.Context, s *store.Store, cfg sharing.Config, participants []Participant) ([]Participant, error) {
	out := append([]Participant(nil), participants...)
	derived, err := sharing.DeriveFromStore(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	by := map[string]string{}
	for _, d := range derived.Derived {
		v, err := persistDerived(ctx, s, derived, d)
		if err != nil {
			return nil, err
		}
		by[d.Recipient.WormID] = v.ID
	}
	for i := range out {
		if id := by[out[i].ID]; id != "" {
			out[i].BrainVersionID = id
		}
	}
	return out, nil
}
func persistDerived(ctx context.Context, s *store.Store, o sharing.Output, d sharing.Derived) (store.BrainVersion, error) {
	target := d.Recipient.BrainVersionID
	if target == "" {
		return store.BrainVersion{}, fmt.Errorf("%w: recipient version", ErrInvalid)
	}
	base, err := s.GetBrainVersion(ctx, target)
	if err != nil {
		return store.BrainVersion{}, err
	}
	lineage, err := store.EncodePayload(d.Lineage)
	if err != nil {
		return store.BrainVersion{}, err
	}
	prov, err := store.EncodePayload(d.Provenance)
	if err != nil {
		return store.BrainVersion{}, err
	}
	payload, err := store.EncodePayload(struct {
		Policy    sharing.Policy       `json:"policy"`
		Recipient string               `json:"recipient"`
		Hash      string               `json:"hash"`
		Additions []sharing.RuleChange `json:"additions,omitempty"`
		Removals  []sharing.RuleChange `json:"removals,omitempty"`
		Changes   []sharing.RuleChange `json:"changes,omitempty"`
	}{o.Policy, d.Recipient.WormID, d.Hash, d.Additions, d.Removals, d.Changes})
	if err != nil {
		return store.BrainVersion{}, err
	}
	// The immutable recipient and provenance are part of the ID. A common
	// worm label or equal resulting rules must not cross brain lineages.
	idMaterial, err := store.EncodePayload(struct {
		BrainID            string         `json:"brain_id"`
		RecipientVersionID string         `json:"recipient_version_id"`
		Policy             sharing.Policy `json:"policy"`
		RulesHash          string         `json:"rules_hash"`
		LineageHash        string         `json:"lineage_hash"`
		ProvenanceHash     string         `json:"provenance_hash"`
	}{base.BrainID, target, o.Policy, hash(d.Rules), hash(lineage), hash(prov)})
	if err != nil {
		return store.BrainVersion{}, err
	}
	id := "sharing-" + hash(idMaterial)
	verify := func(v store.BrainVersion) (store.BrainVersion, error) {
		if v.BrainID != base.BrainID ||
			v.Rules.Hash != hash(d.Rules) ||
			!bytes.Equal(v.Rules.Payload, d.Rules) ||
			!bytes.Equal(v.Lineage.Payload, lineage) ||
			!bytes.Equal(v.Provenance.Payload, prov) ||
			!bytes.Equal(v.Payload, payload) {
			return store.BrainVersion{}, fmt.Errorf("%w: derived version %s", ErrDefinitionMismatch, id)
		}
		return v, nil
	}
	if v, e := s.GetBrainVersion(ctx, id); e == nil {
		return verify(v)
	} else if !errors.Is(e, store.ErrNotFound) {
		return store.BrainVersion{}, e
	}
	versions, err := s.ListBrainVersions(ctx, base.BrainID, store.BrainListOptions{Limit: 100000})
	if err != nil {
		return store.BrainVersion{}, err
	}
	latest := base
	for _, v := range versions {
		if v.Version > latest.Version {
			latest = v
		}
	}
	v, err := s.CreateBrainVersion(ctx, store.CreateBrainVersionInput{ID: id, BrainID: base.BrainID, Version: latest.Version + 1, ParentVersionID: latest.ID, Rules: d.Rules, Lineage: lineage, Provenance: prov, Payload: payload})
	if err != nil {
		if got, e := s.GetBrainVersion(ctx, id); e == nil {
			return verify(got)
		}
	}
	return v, err
}

// DeriveBrains resolves a sharing policy into deterministic immutable versions.
func DeriveBrains(ctx context.Context, s *store.Store, cfg sharing.Config, participants []Participant) ([]Participant, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: store required", ErrInvalid)
	}
	return deriveParticipants(ctx, s, cfg, participants)
}

// FreezeHeldOut captures hashes for versions used by held-out evaluation.
func FreezeHeldOut(ctx context.Context, s *store.Store, versionIDs []string) (map[string]string, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: store required", ErrInvalid)
	}
	out := make(map[string]string, len(versionIDs))
	for _, id := range versionIDs {
		v, err := s.GetBrainVersion(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = v.Hash
	}
	return out, nil
}

// VerifyFrozen checks that immutable versions still have their captured hashes.
func VerifyFrozen(ctx context.Context, s *store.Store, hashes map[string]string) error {
	if s == nil {
		return fmt.Errorf("%w: store required", ErrInvalid)
	}
	for id, expected := range hashes {
		v, err := s.GetBrainVersion(ctx, id)
		if err != nil {
			return err
		}
		if v.Hash != expected {
			return fmt.Errorf("%w: version %s", ErrFrozen, id)
		}
	}
	return nil
}

func RunHeldOut(ctx context.Context, cfg planner.HeldOutConfig) (planner.HeldOutResult, error) {
	return planner.RunHeldOut(ctx, cfg)
}

func Compare(observations []sharing.Observation) sharing.Comparison {
	return sharing.CompareMetrics(observations)
}
