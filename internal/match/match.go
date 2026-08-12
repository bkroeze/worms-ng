// Package match orchestrates deterministic engine turns and their durable audit trail.
package match

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"worms.ng/internal/engine"
	"worms.ng/internal/protocol"
	"worms.ng/internal/store"
)

var (
	ErrPendingDecision = errors.New("match: decision pending")
	ErrFinished        = errors.New("match: already finished")
	ErrInvalidConfig   = errors.New("match: invalid configuration")
	ErrUnverified      = errors.New("match: unverified persistence")
	ErrMissingHistory  = errors.New("match: SAME history missing")
	// Compatibility names make the typed outcome discoverable to callers.
	ErrMissingSAMEHistory = ErrMissingHistory
	ErrSAMEHistoryMissing = ErrMissingHistory
)

// Controller is the only decision boundary used by a match. A nil controller
// means that NEW is deliberately left as a human teaching decision.
type Controller interface {
	Decide(context.Context, protocol.DecisionRequest) (protocol.Action, error)
}

type Provenance interface{ Provenance() map[string]string }
type Named interface{ Name() string }

// Config describes a match. Initial is copied before use. Controllers are
// indexed by worm slot; nil is valid only for a pending human NEW slot.
type Config struct {
	Store  *store.Store
	GameID string
	// PreviousGameID pins SAME history to a specific completed game. When
	// empty, the newest completed game containing the matching profile/slot is
	// selected; incomplete and cancelled games are never eligible.
	PreviousGameID string
	// ProfileIDs identifies the persisted profile for each slot. It defaults
	// to the worm ID, which keeps legacy callers deterministic.
	ProfileIDs       []string
	BrainVersionID   string
	BrainVersionIDs  []string
	Initial          engine.State
	Controllers      []Controller
	Seed             int64
	Deadline         time.Duration
	Now              func() time.Time
	PersistSnapshots bool
}

type Pending struct {
	Request protocol.DecisionRequest
	Slot    int
	WormID  string
}

type Outcome struct {
	DecisionID string               `json:"decision_id"`
	GameID     string               `json:"game_id"`
	WormID     string               `json:"worm_id"`
	Kind       protocol.OutcomeKind `json:"kind"`
	Action     *protocol.Action     `json:"action,omitempty"`
	At         time.Time            `json:"at"`
	Reason     string               `json:"reason,omitempty"`
	Provenance map[string]string    `json:"provenance,omitempty"`
}

type Result struct {
	Events  []engine.Event
	Pending *Pending
	Outcome *Outcome
	State   engine.State
}

type Match struct {
	mu               sync.Mutex
	store            *store.Store
	gameID           string
	state            engine.State
	controllers      []Controller
	brainVersions    []string
	seed             int64
	deadline         time.Duration
	now              func() time.Time
	persistSnapshots bool
	lastAction       *engine.Direction
	lastSlot         int
	decision         uint64
	pendingDeadline  time.Time
	verified         bool
}

// NewMatch creates a match and, when Store is configured, its game and initial
// verified snapshot. Persistence is intentionally done before the match is
// returned, so every later transition has a known optimistic head.
func NewMatch(ctx context.Context, cfg Config) (*Match, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	state := cfg.Initial.Snapshot()
	if err := state.Validate(); err != nil {
		return nil, fmt.Errorf("initial state: %w", err)
	}
	m := &Match{store: cfg.Store, gameID: cfg.GameID, state: state, controllers: append([]Controller(nil), cfg.Controllers...), seed: cfg.Seed, deadline: cfg.Deadline, now: now, persistSnapshots: cfg.PersistSnapshots, lastSlot: -1, verified: true}
	if m.deadline <= 0 {
		m.deadline = 5 * time.Second
	}
	m.brainVersions = append([]string(nil), cfg.BrainVersionIDs...)
	if len(m.brainVersions) == 0 {
		m.brainVersions = make([]string, len(state.Worms))
		for i := range m.brainVersions {
			m.brainVersions[i] = cfg.BrainVersionID
		}
	}
	if err := m.hydrateSAME(ctx, cfg); err != nil {
		return nil, err
	}
	if m.gameID == "" {
		h := RulesHash(m.state)
		m.gameID = fmt.Sprintf("local-%s-%d", h[:16], cfg.Seed)
	}
	if m.store == nil {
		return m, nil
	}
	created := false
	shouldCreate := cfg.GameID == ""
	if !shouldCreate {
		_, err := m.store.GetGame(ctx, cfg.GameID)
		shouldCreate = errors.Is(err, store.ErrNotFound)
		if err != nil && !shouldCreate {
			return nil, err
		}
	}
	if shouldCreate {
		parts := make([]store.Participant, len(state.Worms))
		for i, w := range state.Worms {
			bv := ""
			if i < len(m.brainVersions) {
				bv = m.brainVersions[i]
			}
			profile := w.ID
			if i < len(cfg.ProfileIDs) && cfg.ProfileIDs[i] != "" {
				profile = cfg.ProfileIDs[i]
			}
			pp, _ := store.EncodePayload(map[string]any{"profile_id": profile})
			parts[i] = store.Participant{ID: w.ID, Name: w.ID, BrainVersionID: bv, Kind: string(w.Controller), Payload: pp}
		}
		rules, _ := store.EncodePayload(map[string]any{"ruleset": state.Provenance.Ruleset, "version": state.Provenance.Version})
		g, e := m.store.CreateGame(ctx, store.CreateGameInput{ID: m.gameID, BrainVersionID: cfg.BrainVersionID, Status: "active", RulesPayload: rules, Seed: cfg.Seed, Participants: parts})
		if e != nil {
			return nil, e
		}
		m.gameID = g.ID
		created = true
	}
	if created {
		if err := m.persist(ctx, nil, "active"); err != nil {
			return nil, err
		}
	}
	return m, nil
}
func (m *Match) GameID() string { m.mu.Lock(); defer m.mu.Unlock(); return m.gameID }

// BrainVersionIDs returns the immutable version attached to each worm slot.
func (m *Match) BrainVersionIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.brainVersions...)
}
func validateConfig(c Config) error {
	if len(c.Initial.Worms) == 0 {
		return fmt.Errorf("%w: no worms", ErrInvalidConfig)
	}
	if len(c.Controllers) != 0 && len(c.Controllers) != len(c.Initial.Worms) {
		return fmt.Errorf("%w: controller count", ErrInvalidConfig)
	}
	for _, w := range c.Initial.Worms {
		if w.ID == "" {
			return fmt.Errorf("%w: worm id", ErrInvalidConfig)
		}
	}
	return nil
}

// hydrateSAME resolves each SAME slot from the corresponding slot/profile in
// the newest completed game. The lookup is deliberately performed before the
// new game is created, so a pending or cancelled game cannot become history.
func (m *Match) hydrateSAME(ctx context.Context, cfg Config) error {
	if m.store == nil {
		return nil
	}
	games := []store.Game{}
	if cfg.PreviousGameID != "" {
		g, err := m.store.GetGame(ctx, cfg.PreviousGameID)
		if err != nil {
			return err
		}
		if g.Status != "finished" {
			return fmt.Errorf("%w: game %s is not completed", ErrMissingHistory, cfg.PreviousGameID)
		}
		games = append(games, g)
	} else {
		var err error
		games, err = m.store.ListGames(ctx, store.GameListOptions{Status: "finished", Limit: 1000})
		if err != nil {
			return err
		}
	}
	for slot := range m.state.Worms {
		if m.state.Worms[slot].Controller != engine.ControllerSame {
			continue
		}
		if slot < len(cfg.BrainVersionIDs) && cfg.BrainVersionIDs[slot] != "" {
			continue
		}
		profile := m.state.Worms[slot].ID
		if slot < len(cfg.ProfileIDs) && cfg.ProfileIDs[slot] != "" {
			profile = cfg.ProfileIDs[slot]
		}
		found := false
		for _, g := range games {
			for _, p := range g.Participants {
				persistedProfile := p.ID
				var meta struct {
					ProfileID string `json:"profile_id"`
				}
				if json.Unmarshal(unpack(p.Payload), &meta) == nil && meta.ProfileID != "" {
					persistedProfile = meta.ProfileID
				}
				if p.Slot != slot || persistedProfile != profile || p.BrainVersionID == "" {
					continue
				}
				v, err := m.store.GetBrainVersion(ctx, p.BrainVersionID)
				if err != nil {
					return err
				}
				var rules [64]engine.Action
				if err := json.Unmarshal(unpack(v.Rules.Payload), &rules); err != nil {
					return fmt.Errorf("%w: SAME rules %s", ErrUnverified, v.ID)
				}
				w := m.state.Worms[slot]
				w.Rules = rules
				if err := engine.ValidateRules(w); err != nil {
					return fmt.Errorf("%w: SAME rules %s: %v", ErrUnverified, v.ID, err)
				}
				m.state.Worms[slot].Rules = rules
				m.brainVersions[slot] = v.ID
				found = true
				break
			}
			if found {
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: profile %s slot %d", ErrMissingHistory, profile, slot)
		}
	}
	return nil
}
func (m *Match) State() engine.State { m.mu.Lock(); defer m.mu.Unlock(); return m.state.Snapshot() }
func (m *Match) Pending() *Pending   { m.mu.Lock(); defer m.mu.Unlock(); return m.pendingLocked() }

// Stop ends a match at an orchestration boundary (for example a tournament
// turn cap) without fabricating a winner.
func (m *Match) Stop(ctx context.Context, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.GameOver {
		return nil
	}
	beforeRuntime := m.runtimeSnapshotLocked()
	m.state.Pending = nil
	m.pendingDeadline = time.Time{}
	m.state.GameOver = true
	if err := m.persist(ctx, []engine.Event{{Type: "match_stopped"}}, "stopped"); err != nil {
		m.restoreRuntimeLocked(beforeRuntime)
		return err
	}
	return nil
}
func (m *Match) pendingLocked() *Pending {
	if m.state.Pending == nil {
		return nil
	}
	p := *m.state.Pending
	return &Pending{Slot: p.Slot, WormID: p.WormID}
}

// Advance performs one deterministic round. Automated adapters are invoked in
// slot order; a nil controller leaves a NEW decision pending synchronously.
func (m *Match) Advance(ctx context.Context) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.GameOver {
		return Result{State: m.state.Snapshot()}, ErrFinished
	}
	if m.state.Pending != nil {
		if m.controllerLocked(m.state.Pending.Slot) == nil {
			return Result{Pending: m.pendingLocked(), State: m.state.Snapshot()}, ErrPendingDecision
		}
		return m.resolveAutomatedLocked(ctx, len(m.state.Events))
	}
	beforeRuntime := m.runtimeSnapshotLocked()
	before := len(m.state.Events)
	if m.store == nil {
		m.prepareSameLocked()
	}
	events, err := m.state.AdvanceRound()
	if err != nil {
		m.restoreRuntimeLocked(beforeRuntime)
		return Result{State: m.state.Snapshot()}, err
	}
	m.recordLastActionLocked(events)
	m.applySamePreviousLocked()
	if m.state.Pending != nil && m.pendingDeadline.IsZero() {
		m.pendingDeadline = m.now().Add(m.deadline)
	}
	persistEvents := events
	if m.state.Pending != nil && len(persistEvents) == 0 {
		persistEvents = []engine.Event{{Type: "decision_pending"}}
	}
	if err := m.persist(ctx, persistEvents, m.statusLocked()); err != nil {
		m.restoreRuntimeLocked(beforeRuntime)
		return Result{State: m.state.Snapshot()}, err
	}
	if m.state.Pending != nil {
		return m.resolveAutomatedLocked(ctx, before)
	}
	return m.resultLocked(events), nil
}
func (m *Match) resolveAutomatedLocked(ctx context.Context, before int) (Result, error) {
	// AdvanceRound can stall on NEW. If a controller is attached, consume it and
	// resume through Submit; otherwise expose the pending request to the caller.
	if m.state.Pending == nil {
		return m.resultLocked(m.state.Events[before:]), nil
	}
	beforeRuntime := m.runtimeSnapshotLocked()
	p := *m.state.Pending
	req, err := m.requestLocked(p)
	if err != nil {
		m.restoreRuntimeLocked(beforeRuntime)
		return Result{}, err
	}
	c := m.controllerLocked(p.Slot)
	if c == nil {
		return Result{Pending: &Pending{Request: req, Slot: p.Slot, WormID: p.WormID}, State: m.state.Snapshot()}, nil
	}
	deadlineCtx, cancel := context.WithDeadline(ctx, req.Deadline)
	defer cancel()
	action, err := c.Decide(deadlineCtx, req)
	if !m.now().Before(req.Deadline) {
		o, err := m.resolveTerminalLocked(ctx, p, req, protocol.OutcomeTimeout, nil, "deadline expired")
		if err != nil {
			m.restoreRuntimeLocked(beforeRuntime)
			return Result{}, err
		}
		return Result{Outcome: &o, State: m.state.Snapshot()}, nil
	}
	if err != nil {
		kind := protocol.OutcomeDisconnect
		var outcomeErr *ControllerOutcomeError
		if errors.As(err, &outcomeErr) && outcomeErr.Kind == protocol.OutcomeTimeout {
			kind = protocol.OutcomeTimeout
		}
		o, err := m.resolveTerminalLocked(ctx, p, req, kind, nil, err.Error())
		if err != nil {
			m.restoreRuntimeLocked(beforeRuntime)
			return Result{}, err
		}
		return Result{Outcome: &o, State: m.state.Snapshot()}, nil
	}
	if err := action.Validate(); err != nil {
		return Result{}, err
	}
	if action.Kind == protocol.ActionResign {
		o, err := m.resolveTerminalLocked(ctx, p, req, protocol.OutcomeResigned, &action, "controller resigned")
		if err != nil {
			m.restoreRuntimeLocked(beforeRuntime)
			return Result{}, err
		}
		return Result{Outcome: &o, State: m.state.Snapshot()}, nil
	}
	if err := m.submitLocked(ctx, p, req, action); err != nil {
		m.restoreRuntimeLocked(beforeRuntime)
		return Result{}, err
	}
	return Result{Outcome: &Outcome{DecisionID: req.DecisionID, GameID: m.gameID, WormID: p.WormID, Kind: protocol.OutcomeAccepted, Action: &action, At: m.now(), Provenance: controllerProvenance(c)}, State: m.state.Snapshot()}, nil
}
func (m *Match) controllerLocked(slot int) Controller {
	if slot < 0 || slot >= len(m.controllers) {
		return nil
	}
	return m.controllers[slot]
}
func (m *Match) Finished() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.GameOver
}
func (m *Match) Resolve(ctx context.Context) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Pending == nil {
		return Result{State: m.state.Snapshot()}, errors.New("match: no pending decision")
	}
	p := *m.state.Pending
	req, err := m.requestLocked(p)
	if err != nil {
		return Result{}, err
	}
	if m.now().Before(req.Deadline) {
		return Result{}, errors.New("match: deadline has not passed")
	}
	o, err := m.resolveTerminalLocked(ctx, p, req, protocol.OutcomeTimeout, nil, "deadline expired")
	if err != nil {
		return Result{}, err
	}
	return Result{Outcome: &o, State: m.state.Snapshot()}, nil
}

func (m *Match) prepareSameLocked() {
	for i := range m.state.Worms {
		if m.state.Worms[i].Controller != engine.ControllerSame || !m.state.Worms[i].Alive {
			continue
		}
		var action *engine.Action
		for j := i - 1; j >= 0; j-- {
			if !m.state.Worms[j].Alive {
				continue
			}
			a := m.actionForSlotLocked(j)
			if a >= engine.Action(engine.East) && a <= engine.Action(engine.NorthEast) {
				action = &a
			}
			break
		}
		if action == nil && m.lastAction != nil {
			a := engine.Action(*m.lastAction)
			action = &a
		}
		if action == nil {
			continue
		}
		for mask := 0; mask < 64; mask++ {
			a := engine.Action(engine.ActionDie)
			if mask&(1<<*action) == 0 {
				a = *action
			} else {
				for d := engine.East; d <= engine.NorthEast; d++ {
					if mask&(1<<d) == 0 {
						a = engine.Action(d)
						break
					}
				}
			}
			m.state.Worms[i].Rules[mask] = a
		}
	}
}
func (m *Match) applySamePreviousLocked() {
	for i := range m.state.Worms {
		if m.state.Worms[i].Controller != engine.ControllerSame || !m.state.Worms[i].Alive {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if m.state.Worms[j].Previous >= engine.East && m.state.Worms[j].Previous <= engine.NorthEast {
				m.state.Worms[i].Previous = m.state.Worms[j].Previous
				break
			}
		}
	}
}
func (m *Match) actionForSlotLocked(slot int) engine.Action {
	w := m.state.Worms[slot]
	if !w.Alive {
		return engine.ActionDie
	}
	mask := m.state.Mask(w.Position)
	a := w.Rules[mask&63]
	if a == engine.ActionDoAI {
		moves := m.state.LegalMoves(w.ID)
		if len(moves) > 0 {
			return engine.Action(moves[0])
		}
		return engine.ActionDie
	}
	return a
}

func (m *Match) recordLastActionLocked(events []engine.Event) {
	for _, e := range events {
		if e.Type != "worm_moved" {
			continue
		}
		for i, w := range m.state.Worms {
			if w.ID == e.WormID {
				d := w.Previous
				m.lastAction = &d
				m.lastSlot = i
				break
			}
		}
	}
}
func (m *Match) prepareSameForActionLocked(d engine.Direction) {
	m.lastAction = &d
	// Legacy in-memory matches retain the historical immediate SAME fallback.
	// Persisted matches always source SAME from a completed-game brain.
	if m.store == nil {
		m.prepareSameLocked()
	}
}

func (m *Match) requestLocked(p engine.Decision) (protocol.DecisionRequest, error) {
	obs, err := m.state.Observe(p.WormID)
	if err != nil {
		return protocol.DecisionRequest{}, err
	}
	m.decision++
	id := fmt.Sprintf("%s:%d", m.gameID, p.Request)
	if m.pendingDeadline.IsZero() {
		m.pendingDeadline = m.now().Add(m.deadline)
	}
	deadline := m.pendingDeadline
	orientation := int(obs.Incoming)
	if orientation < 0 || orientation > 5 {
		orientation = 0
	}
	worm := m.state.Worms[p.Slot]
	scoreMap := make(map[string]int, len(m.state.Worms))
	for _, w := range m.state.Worms {
		scoreMap[w.ID] = w.Score
	}
	po := protocol.Observation{Version: protocol.SchemaVersion, GameID: m.gameID, WormID: p.WormID, WormInstanceID: fmt.Sprintf("%s:%d", p.WormID, p.Slot), DecisionID: id, Tick: m.state.Tick, Position: protocol.Position{X: obs.Position.X(), Y: obs.Position.Y()}, Orientation: orientation, Scores: scoreMap, Mode: string(worm.Controller), Deadline: deadline, BrainID: worm.BrainID, BrainVersion: worm.BrainVersion, PatternKey: engine.RawMaskBits(obs.RawMask), Provenance: map[string]string{"seed": fmt.Sprintf("%d", m.seed), "worm_slot": fmt.Sprintf("%d", p.Slot)}}
	if po.BrainVersion == "" && p.Slot < len(m.brainVersions) {
		po.BrainVersion = m.brainVersions[p.Slot]
	}
	for d := engine.East; d <= engine.NorthEast; d++ {
		n := m.state.Neighbor(obs.Position, d)
		po.Neighbors = append(po.Neighbors, protocol.Neighbor{Direction: int(d), Position: protocol.Position{X: n.X(), Y: n.Y()}, Occupied: obs.OccupiedMask&(1<<d) != 0})
		edge := engine.NewEdge(obs.Position, n)
		trailState := protocol.TrailEmpty
		if owner, ok := m.state.Trails[edge]; ok {
			if owner == p.WormID {
				trailState = protocol.TrailOwn
			} else {
				trailState = protocol.TrailOther
			}
		}
		po.TrailStates = append(po.TrailStates, trailState)
	}
	for _, d := range obs.Legal {
		po.LegalActions = append(po.LegalActions, protocol.Action{Kind: protocol.ActionMove, Direction: int(d)})
	}
	r := protocol.DecisionRequest{Version: protocol.SchemaVersion, DecisionID: id, Observation: po, Deadline: deadline}
	if err := r.Validate(); err != nil {
		return protocol.DecisionRequest{}, err
	}
	return r, nil
}

func (m *Match) submitLocked(ctx context.Context, p engine.Decision, req protocol.DecisionRequest, action protocol.Action) error {
	beforeRuntime := m.runtimeSnapshotLocked()
	d := engine.Direction(action.Direction)
	m.prepareSameForActionLocked(d)
	before := len(m.state.Events)
	if _, err := m.state.Submit(d); err != nil {
		m.restoreRuntimeLocked(beforeRuntime)
		return err
	}
	m.pendingDeadline = time.Time{}
	m.recordLastActionLocked(m.state.Events[before:])
	o := Outcome{DecisionID: req.DecisionID, GameID: m.gameID, WormID: p.WormID, Kind: protocol.OutcomeAccepted, Action: &action, At: m.now()}
	if err := m.persistAccepted(ctx, m.state.Events[before:], o); err != nil {
		m.restoreRuntimeLocked(beforeRuntime)
		return err
	}
	return nil
}

func (m *Match) Submit(ctx context.Context, action protocol.Action) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Pending == nil {
		return Result{}, errors.New("match: no pending decision")
	}
	p := *m.state.Pending
	req, err := m.requestLocked(p)
	if err != nil {
		return Result{}, err
	}
	if !m.now().Before(req.Deadline) {
		o, err := m.resolveTerminalLocked(ctx, p, req, protocol.OutcomeTimeout, nil, "deadline expired")
		if err != nil {
			return Result{}, err
		}
		return Result{Outcome: &o, State: m.state.Snapshot()}, nil
	}
	if err := action.Validate(); err != nil {
		return Result{}, err
	}
	if action.Kind == protocol.ActionResign {
		o, err := m.resolveTerminalLocked(ctx, p, req, protocol.OutcomeResigned, &action, "human resigned")
		if err != nil {
			return Result{}, err
		}
		return Result{Outcome: &o, State: m.state.Snapshot()}, nil
	}
	if err := m.submitLocked(ctx, p, req, action); err != nil {
		return Result{}, err
	}
	o := Outcome{DecisionID: req.DecisionID, GameID: m.gameID, WormID: p.WormID, Kind: protocol.OutcomeAccepted, Action: &action, At: m.now()}
	return Result{Outcome: &o, State: m.state.Snapshot()}, nil
}
func (m *Match) resolveTerminalLocked(ctx context.Context, p engine.Decision, req protocol.DecisionRequest, kind protocol.OutcomeKind, action *protocol.Action, reason string) (Outcome, error) {
	beforeRuntime := m.runtimeSnapshotLocked()
	m.state.Worms[p.Slot].Alive = false
	m.state.Worms[p.Slot].CRIX = engine.NOMOVE
	m.state.Pending = nil
	m.pendingDeadline = time.Time{}
	m.state.ActiveSlot = p.Slot + 1
	if m.state.AliveCount() == 0 {
		m.state.GameOver = true
	}
	o := Outcome{DecisionID: req.DecisionID, GameID: m.gameID, WormID: p.WormID, Kind: kind, Action: action, At: m.now(), Reason: reason, Provenance: map[string]string{"controller": m.controllerNameLocked(p.Slot)}}
	if err := m.persistOutcome(ctx, o); err != nil {
		m.restoreRuntimeLocked(beforeRuntime)
		return Outcome{}, err
	}
	return o, nil
}
func (m *Match) statusLocked() string {
	if m.state.GameOver {
		return "finished"
	}
	if m.state.Pending != nil {
		return "pending"
	}
	return "active"
}
func (m *Match) resultLocked(events []engine.Event) Result {
	var p *Pending
	if m.state.Pending != nil {
		p = m.pendingLocked()
	}
	return Result{Events: append([]engine.Event(nil), events...), Pending: p, State: m.state.Snapshot()}
}
func (m *Match) controllerNameLocked(slot int) string {
	if c := m.controllerLocked(slot); c != nil {
		if n, ok := c.(Named); ok {
			return n.Name()
		}
	}
	return "human"
}
func controllerProvenance(c Controller) map[string]string {
	if p, ok := c.(Provenance); ok {
		return p.Provenance()
	}
	if n, ok := c.(Named); ok {
		return map[string]string{"controller": n.Name()}
	}
	return map[string]string{"controller": "controller"}
}

// RulesHash hashes only rule tables, making frozen evaluations independent of
// mutable positions, scores, or event metadata.
func RulesHash(s engine.State) string {
	h := sha256.New()
	for _, w := range s.Worms {
		h.Write([]byte(w.ID))
		h.Write([]byte{0})
		for _, a := range w.Rules {
			h.Write([]byte{byte(a)})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ContinueBrainVersion creates an immutable child version for a taught rule
// table. Existing versions are never updated.
func (m *Match) ContinueBrainVersion(ctx context.Context, slot int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store == nil {
		return "", errors.New("match: store is required")
	}
	if slot < 0 || slot >= len(m.state.Worms) {
		return "", ErrInvalidConfig
	}
	parent := ""
	if slot < len(m.brainVersions) {
		parent = m.brainVersions[slot]
	}
	brainID := ""
	if parent != "" {
		v, err := m.store.GetBrainVersion(ctx, parent)
		if err != nil {
			return "", err
		}
		brainID = v.BrainID
	}
	if brainID == "" {
		return "", errors.New("match: parent brain version is required")
	}
	rules, _ := store.EncodePayload(m.state.Worms[slot].Rules)
	lineage, _ := store.EncodePayload(map[string]any{"parent": parent, "slot": slot})
	prov, _ := store.EncodePayload(map[string]any{"source": "match-teaching", "game_id": m.gameID})
	payload, _ := store.EncodePayload(map[string]any{"rules_hash": RulesHash(m.state), "worm_id": m.state.Worms[slot].ID})
	v, err := m.store.CreateBrainVersion(ctx, store.CreateBrainVersionInput{BrainID: brainID, Version: m.nextBrainVersion(ctx, brainID), ParentVersionID: parent, Rules: rules, Lineage: lineage, Provenance: prov, Payload: payload})
	if err != nil {
		return "", err
	}
	m.brainVersions[slot] = v.ID
	return v.ID, nil
}

func (m *Match) nextBrainVersion(ctx context.Context, brainID string) int64 {
	versions, err := m.store.ListBrainVersions(ctx, brainID, store.BrainListOptions{Limit: 100000})
	if err != nil {
		return time.Now().UnixNano()
	}
	var n int64
	for _, v := range versions {
		if v.Version > n {
			n = v.Version
		}
	}
	return n + 1
}

// ResumeMatch loads and verifies the latest snapshot, event hash chain, and
// state payload before returning a match. It refuses to continue corrupt data.
func ResumeMatch(ctx context.Context, cfg Config) (*Match, error) {
	if cfg.Store == nil || cfg.GameID == "" {
		return nil, fmt.Errorf("%w: store and game id required", ErrInvalidConfig)
	}
	g, err := cfg.Store.GetGame(ctx, cfg.GameID)
	if err != nil {
		return nil, err
	}
	if err := cfg.Store.VerifyEventChain(ctx, cfg.GameID); err != nil {
		return nil, fmt.Errorf("%w: event chain: %v", ErrUnverified, err)
	}
	snap, err := cfg.Store.LoadLatestVerifiedSnapshot(ctx, cfg.GameID)
	if err != nil {
		return nil, err
	}
	if hashBytes(snap.Payload) != snap.Hash {
		return nil, fmt.Errorf("%w: snapshot hash", ErrUnverified)
	}
	var env struct {
		Version int             `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(snap.Payload, &env) != nil || env.Version != 1 {
		return nil, ErrUnverified
	}
	var ps persisted
	if json.Unmarshal(env.Data, &ps) != nil {
		return nil, ErrUnverified
	}
	st, err := engine.UnmarshalSnapshot(ps.Engine)
	if err != nil {
		return nil, fmt.Errorf("%w: state: %v", ErrUnverified, err)
	}
	events, err := cfg.Store.ListEvents(ctx, cfg.GameID, snap.Sequence, 1000000)
	if err != nil {
		return nil, err
	}
	expected := snap.Sequence
	prev := g.EventHash
	if len(events) > 0 {
		prev = events[0].PrevHash
	}
	for _, e := range events {
		if e.Sequence != expected+1 || e.PrevHash != prev || hashEvent(e.Sequence, e.PrevHash, e.Type, e.Payload) != e.Hash {
			return nil, fmt.Errorf("%w: event chain", ErrUnverified)
		}
		expected = e.Sequence
		prev = e.Hash
		var tr transition
		if json.Unmarshal(unpack(e.Payload), &tr) == nil && len(tr.State) > 0 {
			st, err = engine.UnmarshalSnapshot(tr.State)
			if err != nil {
				return nil, fmt.Errorf("%w: event state: %v", ErrUnverified, err)
			}
			if tr.StateHash != "" && st.HashHex() != tr.StateHash {
				return nil, fmt.Errorf("%w: transition state hash", ErrUnverified)
			}
			if tr.Persisted != nil {
				ps = *tr.Persisted
			}
		}
	}
	if expected != g.Sequence || prev != g.EventHash {
		return nil, fmt.Errorf("%w: game head", ErrUnverified)
	}
	cfg.Initial = st
	cfg.GameID = g.ID
	if len(ps.BrainVersions) > 0 {
		cfg.BrainVersionIDs = append([]string(nil), ps.BrainVersions...)
	}
	m, err := NewMatch(ctx, cfg)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.pendingDeadline = ps.PendingDeadline
	m.decision = ps.Decision
	m.lastAction = ps.LastAction
	m.lastSlot = ps.LastSlot
	if len(ps.BrainVersions) > 0 {
		m.brainVersions = append([]string(nil), ps.BrainVersions...)
	}
	if err := m.restoreControllerStatesLocked(ps.Controllers); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: controller state: %v", ErrUnverified, err)
	}
	m.mu.Unlock()
	m.verified = true
	return m, nil
}

type persisted struct {
	Version         int                     `json:"version"`
	Engine          json.RawMessage         `json:"engine"`
	BrainVersions   []string                `json:"brain_versions,omitempty"`
	LastAction      *engine.Direction       `json:"last_action,omitempty"`
	LastSlot        int                     `json:"last_slot"`
	Decision        uint64                  `json:"decision"`
	PendingDeadline time.Time               `json:"pending_deadline,omitempty"`
	Controllers     map[int]json.RawMessage `json:"controllers,omitempty"`
}
type transition struct {
	Version    int             `json:"version"`
	EventTypes []string        `json:"event_types,omitempty"`
	State      json.RawMessage `json:"state"`
	StateHash  string          `json:"state_hash"`
	Persisted  *persisted      `json:"persisted,omitempty"`
	Outcome    *Outcome        `json:"outcome,omitempty"`
}

func (m *Match) controllerStatesLocked() map[int]json.RawMessage {
	out := map[int]json.RawMessage{}
	for i, c := range m.controllers {
		if sc, ok := c.(StatefulController); ok {
			out[i] = append(json.RawMessage(nil), sc.ControllerState()...)
		}
	}
	return out
}
func (m *Match) restoreControllerStatesLocked(states map[int]json.RawMessage) error {
	for i, raw := range states {
		if i < 0 || i >= len(m.controllers) {
			continue
		}
		if sc, ok := m.controllers[i].(StatefulController); ok {
			if err := sc.RestoreControllerState(raw); err != nil {
				return err
			}
		}
	}
	return nil
}

type runtimeSnapshot struct {
	state           engine.State
	lastAction      *engine.Direction
	lastSlot        int
	decision        uint64
	pendingDeadline time.Time
	controllers     map[int]json.RawMessage
}

func (m *Match) runtimeSnapshotLocked() runtimeSnapshot {
	var action *engine.Direction
	if m.lastAction != nil {
		x := *m.lastAction
		action = &x
	}
	return runtimeSnapshot{state: m.state.Snapshot(), lastAction: action, lastSlot: m.lastSlot, decision: m.decision, pendingDeadline: m.pendingDeadline, controllers: m.controllerStatesLocked()}
}
func (m *Match) restoreRuntimeLocked(s runtimeSnapshot) {
	m.state = s.state
	m.lastAction = s.lastAction
	m.lastSlot = s.lastSlot
	m.decision = s.decision
	m.pendingDeadline = s.pendingDeadline
	_ = m.restoreControllerStatesLocked(s.controllers)
}

func (m *Match) persistPendingLocked(ctx context.Context) { _ = m.persist(ctx, nil, "pending") }
func (m *Match) persist(ctx context.Context, events []engine.Event, status string) error {
	if m.store == nil {
		return nil
	}
	raw, err := m.state.MarshalSnapshot()
	if err != nil {
		return err
	}
	p := persisted{Version: 1, Engine: raw, BrainVersions: append([]string(nil), m.brainVersions...), LastAction: m.lastAction, LastSlot: m.lastSlot, Decision: m.decision, PendingDeadline: m.pendingDeadline, Controllers: m.controllerStatesLocked()}
	payload, err := store.EncodePayload(p)
	if err != nil {
		return err
	}
	var inputs []store.EventInput
	for _, e := range events {
		tr := transition{Version: 1, State: raw, StateHash: m.state.HashHex(), Persisted: &p, EventTypes: []string{e.Type}}
		ep, _ := store.EncodePayload(tr)
		inputs = append(inputs, store.EventInput{Type: e.Type, Payload: ep})
	}
	return m.persistAtomic(ctx, inputs, payload, status)
}

func (m *Match) persistOutcome(ctx context.Context, o Outcome) error {
	if m.store == nil {
		return nil
	}
	raw, err := m.state.MarshalSnapshot()
	if err != nil {
		return err
	}
	p := persisted{Version: 1, Engine: raw, BrainVersions: append([]string(nil), m.brainVersions...), LastAction: m.lastAction, LastSlot: m.lastSlot, Decision: m.decision, PendingDeadline: m.pendingDeadline, Controllers: m.controllerStatesLocked()}
	snap, err := store.EncodePayload(p)
	if err != nil {
		return err
	}
	tr := transition{Version: 1, State: raw, StateHash: m.state.HashHex(), Persisted: &p, Outcome: &o}
	ep, err := store.EncodePayload(tr)
	if err != nil {
		return err
	}
	return m.persistAtomic(ctx, []store.EventInput{{Type: "decision_outcome", Payload: ep}}, snap, m.statusLocked())
}
func (m *Match) persistAccepted(ctx context.Context, events []engine.Event, o Outcome) error {
	if m.store == nil {
		return nil
	}
	raw, err := m.state.MarshalSnapshot()
	if err != nil {
		return err
	}
	p := persisted{Version: 1, Engine: raw, BrainVersions: append([]string(nil), m.brainVersions...), LastAction: m.lastAction, LastSlot: m.lastSlot, Decision: m.decision, PendingDeadline: m.pendingDeadline, Controllers: m.controllerStatesLocked()}
	snap, err := store.EncodePayload(p)
	if err != nil {
		return err
	}
	inputs := make([]store.EventInput, 0, len(events)+1)
	for _, ev := range events {
		tr := transition{Version: 1, State: raw, StateHash: m.state.HashHex(), Persisted: &p, EventTypes: []string{ev.Type}}
		ep, encErr := store.EncodePayload(tr)
		if encErr != nil {
			return encErr
		}
		inputs = append(inputs, store.EventInput{Type: ev.Type, Payload: ep})
	}
	tr := transition{Version: 1, State: raw, StateHash: m.state.HashHex(), Persisted: &p, Outcome: &o}
	ep, err := store.EncodePayload(tr)
	if err != nil {
		return err
	}
	inputs = append(inputs, store.EventInput{Type: "decision_outcome", Payload: ep})
	return m.persistAtomic(ctx, inputs, snap, m.statusLocked())
}
func (m *Match) persistAtomic(ctx context.Context, inputs []store.EventInput, snap json.RawMessage, status string) error {
	g, err := m.store.GetGame(ctx, m.gameID)
	if err != nil {
		return err
	}
	expectedSeq, expectedHash := g.Sequence, g.EventHash
	nextSeq := expectedSeq
	prev := expectedHash
	hashes := make([]string, 0, len(inputs))
	for _, in := range inputs {
		nextSeq++
		h := hashEvent(nextSeq, prev, in.Type, in.Payload)
		hashes = append(hashes, h)
		prev = h
	}
	snapHash := hashBytes(snap)
	return m.store.WithTx(ctx, func(tx *store.Tx) error {
		for i, in := range inputs {
			t := time.Now().UTC().Format(time.RFC3339Nano)
			prior := expectedHash
			if i > 0 {
				prior = hashes[i-1]
			}
			if _, e := tx.ExecContext(ctx, "INSERT INTO game_events(game_id,sequence,type,payload,prev_hash,hash,created_at) VALUES(?,?,?,?,?,?,?)", m.gameID, expectedSeq+int64(i)+1, in.Type, in.Payload, prior, hashes[i], t); e != nil {
				return e
			}
		}
		r, e := tx.ExecContext(ctx, "UPDATE games SET sequence=?,event_hash=?,status=?,updated_at=? WHERE id=? AND sequence=? AND event_hash=?", nextSeq, prev, status, time.Now().UTC().Format(time.RFC3339Nano), m.gameID, expectedSeq, expectedHash)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return fmt.Errorf("%w: optimistic game head", store.ErrConflict)
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO game_snapshots(game_id,sequence,payload,hash,created_at) VALUES(?,?,?,?,?)", m.gameID, nextSeq, snap, snapHash, time.Now().UTC().Format(time.RFC3339Nano))
		return e
	})
}
func unpack(raw []byte) []byte {
	var e struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &e) == nil && len(e.Data) > 0 {
		return e.Data
	}
	return raw
}
func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func hashEvent(seq int64, prev, typ string, p []byte) string {
	return hashBytes([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s", seq, prev, typ, string(p))))
}
