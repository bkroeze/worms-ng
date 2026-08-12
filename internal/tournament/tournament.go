// Package tournament provides seeded, resumable tournament scheduling and
// frozen evaluation around the match orchestration boundary.
package tournament

import (
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
	"worms.ng/internal/store"
)

var (
	ErrInvalidConfig = errors.New("tournament: invalid configuration")
	ErrFrozenRules   = errors.New("tournament: frozen rule hash changed")
)

type Participant struct {
	ID             string
	Name           string
	BrainVersionID string
	Controller     match.Controller
	Color          engine.Color
}
type Config struct {
	Store        *store.Store
	ID           string
	Name         string
	State        engine.State
	Participants []Participant
	Rounds       int
	Seed         int64
	Deadline     time.Duration
	Now          func() time.Time
	MaxTurns     int
}
type MatchReport struct {
	ID              string                  `json:"id"`
	GameID          string                  `json:"game_id"`
	Round           int                     `json:"round"`
	Seed            int64                   `json:"seed"`
	Colors          map[string]engine.Color `json:"colors"`
	Scores          map[string]int          `json:"scores"`
	Winners         []string                `json:"winners"`
	Tie             bool                    `json:"tie"`
	Status          string                  `json:"status"`
	Turns           int                     `json:"turns"`
	MoveCount       int                     `json:"move_count"`
	Timeouts        int                     `json:"timeouts"`
	Errors          int                     `json:"errors"`
	Resigns         int                     `json:"resigns"`
	BrainVersionIDs map[string]string       `json:"brain_version_ids,omitempty"`
	EventStart      int64                   `json:"event_start,omitempty"`
	EventEnd        int64                   `json:"event_end,omitempty"`
	ReplayLink      string                  `json:"replay_link,omitempty"`
	InspectionLinks map[string]string       `json:"inspection_links,omitempty"`
}
type Report struct {
	TournamentID string         `json:"tournament_id"`
	Seed         int64          `json:"seed"`
	Matches      []MatchReport  `json:"matches"`
	Wins         map[string]int `json:"wins"`
	Ties         int            `json:"ties"`
	Completed    int            `json:"completed"`
	Timeouts     int            `json:"timeouts"`
	Errors       int            `json:"errors"`
	Resigns      int            `json:"resigns"`
}
type Tournament struct {
	cfg Config
	id  string
}

func NewTournament(ctx context.Context, cfg Config) (*Tournament, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}
	if cfg.Rounds <= 0 {
		cfg.Rounds = 1
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 32
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	t := &Tournament{cfg: cfg, id: cfg.ID}
	if cfg.Store != nil && t.id == "" {
		rules, _ := store.EncodePayload(map[string]any{"seed": cfg.Seed, "rounds": cfg.Rounds, "participants": len(cfg.Participants)})
		x, e := cfg.Store.CreateTournament(ctx, store.CreateTournamentInput{Name: cfg.Name, RulesPayload: rules})
		if e != nil {
			return nil, e
		}
		t.id = x.ID
	}
	return t, nil
}
func ResumeTournament(ctx context.Context, cfg Config) (*Tournament, error) {
	if cfg.Store == nil || cfg.ID == "" {
		return nil, fmt.Errorf("%w: store and id required", ErrInvalidConfig)
	}
	if _, err := cfg.Store.GetTournament(ctx, cfg.ID); err != nil {
		return nil, err
	}
	return NewTournament(ctx, cfg)
}
func RunTournament(ctx context.Context, cfg Config) (Report, error) {
	t, err := NewTournament(ctx, cfg)
	if err != nil {
		return Report{}, err
	}
	return t.Run(ctx)
}
func validate(c Config) error {
	if len(c.Participants) < 2 {
		return fmt.Errorf("%w: at least two participants", ErrInvalidConfig)
	}
	if len(c.State.Worms) < len(c.Participants) {
		return fmt.Errorf("%w: state has fewer worms", ErrInvalidConfig)
	}
	seen := map[string]bool{}
	for _, p := range c.Participants {
		if p.ID == "" || seen[p.ID] {
			return fmt.Errorf("%w: participant id", ErrInvalidConfig)
		}
		seen[p.ID] = true
	}
	return nil
}
func (t *Tournament) ID() string { return t.id }

// Run executes rounds in stable participant order. Stored terminal matches are
// skipped; active/pending matches resume from their verified game cursor.
func (t *Tournament) Run(ctx context.Context) (Report, error) {
	r := Report{TournamentID: t.id, Seed: t.cfg.Seed, Wins: map[string]int{}}
	var existing []store.Match
	if t.cfg.Store != nil {
		rows, err := t.cfg.Store.DB().QueryContext(ctx, "SELECT id,tournament_id,game_id,round,status,payload,created_at,updated_at FROM tournament_matches WHERE tournament_id=? ORDER BY round,id", t.id)
		if err != nil {
			return r, err
		}
		for rows.Next() {
			var x store.Match
			var p []byte
			if err := rows.Scan(&x.ID, &x.TournamentID, &x.GameID, &x.Round, &x.Status, &p, &x.CreatedAt, &x.UpdatedAt); err != nil {
				rows.Close()
				return r, err
			}
			x.Payload = append([]byte(nil), p...)
			existing = append(existing, x)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return r, err
		}
		rows.Close()
	}
	stored := map[int]bool{}
	active := map[int]store.Match{}
	for _, x := range existing {
		if x.Status != "finished" && x.Status != "stopped" {
			if x.Status == "active" || x.Status == "pending" {
				active[int(x.Round)] = x
			}
			continue
		}
		mr, err := decodeMatchReport(x.Payload)
		if err != nil {
			return r, fmt.Errorf("tournament match %s: %w", x.ID, err)
		}
		mr.ID = x.ID
		mr.Status = x.Status
		r.Matches = append(r.Matches, mr)
		stored[int(x.Round)] = true
	}
	for round := 0; round < t.cfg.Rounds; round++ {
		if stored[round] {
			continue
		}
		mr, err := t.runRound(ctx, round, active[round])
		if err != nil {
			return r, err
		}
		r.Matches = append(r.Matches, mr)
	}
	sort.Slice(r.Matches, func(i, j int) bool {
		if r.Matches[i].Round != r.Matches[j].Round {
			return r.Matches[i].Round < r.Matches[j].Round
		}
		return r.Matches[i].ID < r.Matches[j].ID
	})
	for _, mr := range r.Matches {
		r.Timeouts += mr.Timeouts
		r.Errors += mr.Errors
		r.Resigns += mr.Resigns
		if mr.Status != "finished" {
			continue
		}
		r.Completed++
		for _, w := range mr.Winners {
			r.Wins[w]++
		}
		if mr.Tie {
			r.Ties++
		}
	}
	return r, nil
}

func decodeMatchReport(raw []byte) (MatchReport, error) {
	var env struct {
		Version int             `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return MatchReport{}, err
	}
	if env.Version != 1 || len(env.Data) == 0 {
		return MatchReport{}, errors.New("invalid match report payload")
	}
	var mr MatchReport
	if err := json.Unmarshal(env.Data, &mr); err != nil {
		return MatchReport{}, err
	}
	return mr, nil
}
func (t *Tournament) runRound(ctx context.Context, round int, existing store.Match) (MatchReport, error) {
	seed := deriveSeed(t.cfg.Seed, round)
	state := t.cfg.State.Snapshot()
	controllers := make([]match.Controller, len(t.cfg.Participants))
	colors := map[string]engine.Color{}
	palette := make([]engine.Color, len(t.cfg.Participants))
	for i, p := range t.cfg.Participants {
		if p.Color != "" {
			palette[i] = p.Color
		} else {
			palette[i] = engine.Color(fmt.Sprintf("color-%d", i))
		}
	}
	for i, p := range t.cfg.Participants {
		w := &state.Worms[i]
		w.ID = p.ID
		w.Color = palette[(i+round)%len(palette)]
		colors[p.ID] = w.Color
		w.Alive = true
		controllers[i] = p.Controller
		if controllers[i] == nil {
			controllers[i] = match.NewRandomController(seed + int64(i))
		}
	}
	state.Worms = state.Worms[:len(t.cfg.Participants)]
	brainVersions := make([]string, len(t.cfg.Participants))
	for i, p := range t.cfg.Participants {
		brainVersions[i] = p.BrainVersionID
	}
	mcfg := match.Config{Store: t.cfg.Store, GameID: fmt.Sprintf("%s-round-%d-game", t.id, round), Initial: state, Controllers: controllers, BrainVersionIDs: brainVersions, Seed: seed, Deadline: t.cfg.Deadline, Now: t.cfg.Now}
	var m *match.Match
	var err error
	if existing.ID != "" {
		mcfg.GameID = existing.GameID
		m, err = match.ResumeMatch(ctx, mcfg)
	} else {
		m, err = match.NewMatch(ctx, mcfg)
	}
	if err != nil {
		return MatchReport{}, err
	}
	mr := MatchReport{ID: existing.ID, Round: round, Seed: seed, GameID: m.GameID(), Colors: colors, Scores: map[string]int{}, BrainVersionIDs: map[string]string{}, Status: "active", ReplayLink: m.GameID(), InspectionLinks: map[string]string{}}
	for i, id := range m.BrainVersionIDs() {
		if i < len(t.cfg.Participants) && id != "" {
			mr.BrainVersionIDs[t.cfg.Participants[i].ID] = id
		}
	}
	if existing.ID != "" {
		if prior, decodeErr := decodeMatchReport(existing.Payload); decodeErr == nil {
			mr.Timeouts, mr.Errors, mr.Resigns = prior.Timeouts, prior.Errors, prior.Resigns
			mr.Turns, mr.MoveCount = prior.Turns, prior.MoveCount
			if len(prior.Colors) > 0 {
				mr.Colors = prior.Colors
			}
		}
	}
	if t.cfg.Store != nil && existing.ID == "" {
		p, err := store.EncodePayload(mr)
		if err != nil {
			return mr, err
		}
		x, err := t.cfg.Store.CreateMatch(ctx, store.CreateMatchInput{ID: fmt.Sprintf("%s-round-%d", t.id, round), TournamentID: t.id, GameID: m.GameID(), Round: int64(round), Status: "active", Payload: p})
		if err != nil {
			return mr, err
		}
		mr.ID = x.ID
	}
	turns := mr.Turns
	capped := false
	for !m.Finished() {
		if t.cfg.MaxTurns > 0 && turns >= t.cfg.MaxTurns {
			if err := m.Stop(ctx, "turn cap"); err != nil {
				return mr, err
			}
			capped = true
			break
		}
		turns++
		res, err := m.Advance(ctx)
		mr.Turns++
		if res.Outcome != nil {
			switch res.Outcome.Kind {
			case "timeout":
				mr.Timeouts++
			case "disconnect":
				mr.Errors++
			case "resigned":
				mr.Resigns++
			}
		}
		if err != nil && res.Pending == nil {
			mr.Errors++
			if stopErr := m.Stop(ctx, "controller error: "+err.Error()); stopErr != nil {
				return mr, stopErr
			}
			capped = true
			break
		}
		if res.Pending != nil {
			if _, e := m.Resolve(ctx); e != nil {
				mr.Timeouts++
				if stopErr := m.Stop(ctx, "pending decision"); stopErr != nil {
					return mr, stopErr
				}
				capped = true
				break
			}
		}
		if t.cfg.Store != nil {
			p, e := store.EncodePayload(mr)
			if e != nil {
				return mr, e
			}
			if _, e = t.cfg.Store.DB().ExecContext(ctx, "UPDATE tournament_matches SET payload=?,updated_at=? WHERE id=?", p, time.Now().UTC().Format(time.RFC3339Nano), mr.ID); e != nil {
				return mr, e
			}
		}
	}
	s := m.State()
	for _, w := range s.Worms {
		mr.Scores[w.ID] = w.Score
	}
	mr.MoveCount = 0
	for _, event := range s.Events {
		if event.Type == "worm_moved" {
			mr.MoveCount++
		}
	}
	mr.EventStart = 0
	if t.cfg.Store != nil {
		if g, err := t.cfg.Store.GetGame(ctx, m.GameID()); err == nil {
			mr.EventEnd = g.Sequence
		}
	}
	if capped {
		mr.Status = "stopped"
	} else {
		mr.Winners = s.Winners()
		mr.Tie = s.Tied()
		mr.Status = "finished"
	}
	if t.cfg.Store != nil {
		p, err := store.EncodePayload(mr)
		if err != nil {
			return mr, err
		}
		if _, err := t.cfg.Store.DB().ExecContext(ctx, "UPDATE tournament_matches SET status=?,payload=?,updated_at=? WHERE id=?", mr.Status, p, time.Now().UTC().Format(time.RFC3339Nano), mr.ID); err != nil {
			return mr, err
		}
	}
	return mr, nil
}
func deriveSeed(seed int64, round int) int64 {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", seed, round)))
	var x int64
	for _, b := range h[:8] {
		x = x<<8 | int64(b)
	}
	return x
}
func EvaluateHeldOut(ctx context.Context, cases []HeldOutCase) (Evaluation, error) {
	out := Evaluation{Cases: len(cases), Passed: true, RuleHashes: map[string]string{}, Wins: map[string]int{}}
	training := map[int64]bool{}
	for _, c := range cases {
		for _, seed := range c.TrainingSeeds {
			training[seed] = true
		}
	}
	for i, c := range cases {
		if training[c.Seed] {
			out.Passed = false
			out.Error = fmt.Sprintf("%s: held-out seed %d is in training partition", c.Name, c.Seed)
			return out, ErrFrozenRules
		}
		before := match.RulesHash(c.State)
		if err := verifyFrozenVersions(ctx, c, before); err != nil {
			out.Passed = false
			out.Error = err.Error()
			return out, ErrFrozenRules
		}
		controllers := append([]match.Controller(nil), c.Controllers...)
		if len(controllers) == 0 {
			controllers = make([]match.Controller, len(c.State.Worms))
		}
		for j := range c.State.Worms {
			if j >= len(controllers) {
				break
			}
			if controllers[j] == nil {
				controllers[j] = match.NewRandomController(c.Seed + int64(j))
			}
		}
		m, err := match.NewMatch(ctx, match.Config{Initial: c.State, Controllers: controllers, Seed: c.Seed, Deadline: c.Deadline})
		if err != nil {
			return out, err
		}
		report := MatchReport{Round: i, Seed: c.Seed, Status: "finished", Scores: map[string]int{}}
		maxTurns := c.MaxTurns
		if maxTurns <= 0 {
			maxTurns = 64
		}
		for turns := 0; !m.Finished(); turns++ {
			if turns >= maxTurns {
				if err := m.Stop(ctx, "held-out turn cap"); err != nil {
					return out, err
				}
				report.Status = "stopped"
				break
			}
			report.Turns++
			res, err := m.Advance(ctx)
			if res.Outcome != nil {
				switch res.Outcome.Kind {
				case "timeout":
					report.Timeouts++
				case "disconnect":
					report.Errors++
				case "resigned":
					report.Resigns++
				}
			}
			if err != nil && res.Pending == nil {
				report.Errors++
				if err := m.Stop(ctx, "held-out controller error"); err != nil {
					return out, err
				}
				report.Status = "stopped"
				break
			}
			if res.Pending != nil {
				if _, err := m.Resolve(ctx); err != nil {
					return out, err
				}
			}
		}
		afterState := m.State()
		after := match.RulesHash(afterState)
		out.RuleHashes[c.Name] = before
		if before != after {
			out.Passed = false
			out.Error = fmt.Sprintf("%s: %s -> %s", c.Name, before, after)
			return out, ErrFrozenRules
		}
		report.Winners = afterState.Winners()
		report.Tie = afterState.Tied()
		for _, w := range afterState.Worms {
			report.Scores[w.ID] = w.Score
		}
		for _, event := range afterState.Events {
			if event.Type == "worm_moved" {
				report.MoveCount++
			}
		}
		for _, winner := range report.Winners {
			out.Wins[winner]++
		}
		if report.Tie {
			out.Ties++
		}
		out.Completed++
		out.Results = append(out.Results, report)
	}
	return out, nil
}

func verifyFrozenVersions(ctx context.Context, c HeldOutCase, expected string) error {
	ids := c.BrainVersionIDs
	if len(ids) == 0 && c.BrainVersionID != "" {
		ids = []string{c.BrainVersionID}
	}
	if len(ids) == 0 {
		return nil
	}
	if c.Store == nil || len(ids) != len(c.State.Worms) {
		return errors.New("frozen evaluation: persisted brain versions required for every slot")
	}
	for i, id := range ids {
		v, err := c.Store.GetBrainVersion(ctx, id)
		if err != nil {
			return fmt.Errorf("frozen evaluation: version %s: %w", id, err)
		}
		if i >= len(c.State.Worms) || !c.State.Worms[i].Frozen {
			return fmt.Errorf("frozen evaluation: slot %d runtime state is mutable", i)
		}
		brain, err := c.Store.GetBrain(ctx, v.BrainID)
		if err != nil {
			return fmt.Errorf("frozen evaluation: brain %s: %w", v.BrainID, err)
		}
		if !brain.Frozen {
			return fmt.Errorf("frozen evaluation: brain %s is mutable", brain.ID)
		}
		var rules [64]engine.Action
		if err := json.Unmarshal(matchPayload(v.Rules.Payload), &rules); err != nil {
			return fmt.Errorf("frozen evaluation: version %s rules: %w", id, err)
		}
		if rules != c.State.Worms[i].Rules {
			return fmt.Errorf("frozen evaluation: version %s silently cloned or mismatched", id)
		}
		var meta struct {
			RulesHash string `json:"rules_hash"`
		}
		if json.Unmarshal(matchPayload(v.Payload), &meta) == nil && meta.RulesHash != "" && meta.RulesHash != expected {
			return fmt.Errorf("frozen evaluation: version %s lineage hash mismatch", id)
		}
	}
	return nil
}

func matchPayload(raw []byte) []byte {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) == nil && len(env.Data) > 0 {
		return env.Data
	}
	return raw
}

type HeldOutCase struct {
	Name            string
	Store           *store.Store
	State           engine.State
	Controllers     []match.Controller
	Seed            int64
	TrainingSeeds   []int64
	BrainVersionID  string
	BrainVersionIDs []string
	Deadline        time.Duration
	MaxTurns        int
}
type Evaluation struct {
	Cases      int
	Passed     bool
	RuleHashes map[string]string
	Results    []MatchReport
	Wins       map[string]int
	Ties       int
	Completed  int
	Error      string
}

func FrozenRuleHash(s engine.State) string {
	h := sha256.New()
	for _, w := range s.Worms {
		h.Write([]byte(w.ID))
		for _, a := range w.Rules {
			h.Write([]byte{byte(a)})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
