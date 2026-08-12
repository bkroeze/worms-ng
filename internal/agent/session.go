package agent

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"worms.ng/internal/protocol"
)

// ObservationProvider and ActionApplier are the only boundaries an engine
// adapter needs to implement. The agent package does not import engine.State.
type ObservationProvider interface {
	Observe(context.Context, string, string) (protocol.DecisionRequest, error)
}

type ActionApplier interface {
	Apply(context.Context, string, string, protocol.Action) error
}

// EngineAdapter combines the two narrow callbacks required to connect an
// engine to a session. It intentionally mentions only protocol values, so an
// engine package can implement it without creating an import cycle.
type EngineAdapter interface {
	ObservationProvider
	ActionApplier
}

var (
	ErrUnauthorized = errors.New("invalid game credential")
	ErrSession      = errors.New("unknown or closed session")
	ErrNoDecision   = errors.New("no decision is pending")
	ErrDeadline     = errors.New("decision deadline has passed")
)

// Credentials are scoped to one game session and must never be reused across
// games. Token is intentionally opaque and is not included in log entries.
type Credentials struct {
	GameID string `json:"game_id"`
	Token  string `json:"token"`
}

type SessionConfig struct {
	DefaultDeadline time.Duration
	Now             func() time.Time
	NewToken        func() (string, error)
	Logger          Logger
}

type pendingDecision struct {
	request  protocol.DecisionRequest
	deadline time.Time
	started  time.Time
}

type session struct {
	gameID     string
	wormID     string
	credential string
	policy     Policy
	closed     bool
	pending    *pendingDecision
	resolved   map[string]protocol.DecisionOutcome
}

// SessionManager serializes one synchronous decision at a time per game.
// It stores no engine state: callers adapt requests and apply accepted actions
// through their own narrow engine boundary.
type SessionManager struct {
	mu              sync.Mutex
	sessions        map[string]*session
	defaultDeadline time.Duration
	now             func() time.Time
	newToken        func() (string, error)
	logger          Logger
}

func NewSessionManager(config ...SessionConfig) *SessionManager {
	var cfg SessionConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.DefaultDeadline <= 0 {
		cfg.DefaultDeadline = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewToken == nil {
		cfg.NewToken = randomToken
	}
	if cfg.Logger == nil {
		cfg.Logger = NopLogger{}
	}
	return &SessionManager{sessions: make(map[string]*session), defaultDeadline: cfg.DefaultDeadline, now: cfg.Now, newToken: cfg.NewToken, logger: cfg.Logger}
}

func randomToken() (string, error) {
	var token [32]byte
	if _, err := cryptorand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate credential: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

// Start creates a game-scoped credential and binds one policy to the game.
func (m *SessionManager) Start(gameID, wormID string, policy Policy) (Credentials, error) {
	if gameID == "" || wormID == "" || policy == nil {
		return Credentials{}, errors.New("game ID, worm ID, and policy are required")
	}
	token, err := m.newToken()
	if err != nil {
		return Credentials{}, err
	}
	if token == "" {
		return Credentials{}, errors.New("credential generator returned an empty token")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[gameID]; exists {
		return Credentials{}, errors.New("game session already exists")
	}
	m.sessions[gameID] = &session{gameID: gameID, wormID: wormID, credential: token, policy: policy, resolved: make(map[string]protocol.DecisionOutcome)}
	return Credentials{GameID: gameID, Token: token}, nil
}

// CreateSession is an explicit alias for Start for transport adapters.
func (m *SessionManager) CreateSession(gameID, wormID string, policy Policy) (Credentials, error) {
	return m.Start(gameID, wormID, policy)
}

// Begin publishes a validated decision request. A second Begin before the
// first response is rejected, preventing concurrent turns for one game.
func (m *SessionManager) Begin(credentials Credentials, request protocol.DecisionRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.authorizeLocked(credentials)
	if err != nil {
		return err
	}
	if s.closed {
		return ErrSession
	}
	if s.pending != nil {
		return errors.New("a decision is already pending")
	}
	if request.Deadline.IsZero() {
		request.Deadline = m.now().Add(m.defaultDeadline)
	}
	if err := request.Validate(); err != nil {
		return fmt.Errorf("invalid decision request: %w", err)
	}
	if request.Observation.GameID != credentials.GameID {
		return errors.New("request game ID does not match credential")
	}
	if request.Observation.WormID != s.wormID {
		return errors.New("request worm ID does not match session")
	}
	if !request.Deadline.After(m.now()) {
		return ErrDeadline
	}
	s.pending = &pendingDecision{request: request, deadline: request.Deadline, started: m.now()}
	m.logLocked(s, request.DecisionID, protocol.OutcomeKind("pending"), nil, "")
	return nil
}

// Request asks the bound policy to choose synchronously and submits that
// choice through the same credential and stale-response checks as an external
// caller.
func (m *SessionManager) Request(ctx context.Context, credentials Credentials, request protocol.DecisionRequest) (protocol.DecisionOutcome, error) {
	if request.Deadline.IsZero() {
		request.Deadline = m.now().Add(m.defaultDeadline)
	}
	if err := m.Begin(credentials, request); err != nil {
		return protocol.DecisionOutcome{}, err
	}
	m.mu.Lock()
	s, err := m.authorizeLocked(credentials)
	if err != nil {
		m.mu.Unlock()
		return protocol.DecisionOutcome{}, err
	}
	policy := s.policy
	m.mu.Unlock()

	action, err := policy.Decide(ctx, request)
	if err != nil {
		m.Disconnect(credentials, err.Error())
		return protocol.DecisionOutcome{}, err
	}
	return m.Submit(credentials, protocol.DecisionResponse{Version: protocol.SchemaVersion, DecisionID: request.DecisionID, Action: action})
}

// Submit accepts exactly the currently pending decision, or returns an
// explicit stale/duplicate outcome. Invalid payloads do not consume a turn.
func (m *SessionManager) Submit(credentials Credentials, response protocol.DecisionResponse) (protocol.DecisionOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.authorizeLocked(credentials)
	if err != nil {
		return protocol.DecisionOutcome{}, err
	}
	if err := response.Validate(); err != nil {
		outcome := m.outcomeLocked(s, response.DecisionID, protocol.OutcomeMalformed, nil, err.Error())
		return outcome, nil
	}
	if prior, ok := s.resolved[response.DecisionID]; ok {
		prior.Outcome = protocol.OutcomeDuplicate
		prior.Reason = "decision was already resolved"
		m.logLocked(s, response.DecisionID, protocol.OutcomeDuplicate, &response.Action, prior.Reason)
		return prior, nil
	}
	if s.pending == nil {
		outcome := m.outcomeLocked(s, response.DecisionID, protocol.OutcomeStale, nil, "no decision is pending")
		return outcome, nil
	}
	if !response.Action.IsMove() && response.Action.Kind != protocol.ActionResign {
		outcome := m.outcomeLocked(s, response.DecisionID, protocol.OutcomeMalformed, nil, "unsupported action")
		return outcome, nil
	}
	pending := s.pending
	if m.now().Compare(pending.deadline) >= 0 {
		return m.resolveLocked(s, pending.request.DecisionID, protocol.OutcomeTimeout, nil, "deadline expired")
	}
	if response.DecisionID != pending.request.DecisionID {
		outcome := m.outcomeLocked(s, response.DecisionID, protocol.OutcomeStale, nil, "decision ID does not match pending turn")
		return outcome, nil
	}
	if !isLegal(response.Action, pending.request.Observation.LegalActions) {
		outcome := m.outcomeLocked(s, response.DecisionID, protocol.OutcomeIllegal, nil, "action is not legal for pending observation")
		return outcome, nil
	}
	if response.Action.Kind == protocol.ActionResign {
		return m.resolveLocked(s, response.DecisionID, protocol.OutcomeResigned, &response.Action, "agent resigned")
	}
	return m.resolveLocked(s, response.DecisionID, protocol.OutcomeAccepted, &response.Action, "")
}

func (m *SessionManager) ResolveTimeout(credentials Credentials) (protocol.DecisionOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.authorizeLocked(credentials)
	if err != nil {
		return protocol.DecisionOutcome{}, err
	}
	if s.pending == nil {
		return protocol.DecisionOutcome{}, ErrNoDecision
	}
	if m.now().Before(s.pending.deadline) {
		return protocol.DecisionOutcome{}, errors.New("decision deadline has not passed")
	}
	return m.resolveLocked(s, s.pending.request.DecisionID, protocol.OutcomeTimeout, nil, "deadline expired")
}

func (m *SessionManager) Disconnect(credentials Credentials, reason string) (protocol.DecisionOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.authorizeLocked(credentials)
	if err != nil {
		return protocol.DecisionOutcome{}, err
	}
	if s.pending == nil {
		return protocol.DecisionOutcome{}, ErrNoDecision
	}
	return m.resolveLocked(s, s.pending.request.DecisionID, protocol.OutcomeDisconnect, nil, reason)
}

func (m *SessionManager) Close(credentials Credentials) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.authorizeLocked(credentials)
	if err != nil {
		return err
	}
	if s.pending != nil {
		_, _ = m.resolveLocked(s, s.pending.request.DecisionID, protocol.OutcomeDisconnect, nil, "session closed")
	}
	s.closed = true
	return nil
}

func (m *SessionManager) authorizeLocked(credentials Credentials) (*session, error) {
	if credentials.GameID == "" || credentials.Token == "" {
		return nil, ErrUnauthorized
	}
	s, ok := m.sessions[credentials.GameID]
	if !ok {
		return nil, ErrSession
	}
	if s.credential != credentials.Token {
		return nil, ErrUnauthorized
	}
	if s.closed {
		return nil, ErrSession
	}
	return s, nil
}

func (m *SessionManager) outcomeLocked(s *session, decisionID string, kind protocol.OutcomeKind, action *protocol.Action, reason string) protocol.DecisionOutcome {
	outcome := protocol.DecisionOutcome{Version: protocol.SchemaVersion, GameID: s.gameID, WormID: s.wormID, DecisionID: decisionID, Outcome: kind, Action: action, At: m.now(), Reason: reason}
	m.logLocked(s, decisionID, kind, action, reason)
	return outcome
}

func (m *SessionManager) resolveLocked(s *session, decisionID string, kind protocol.OutcomeKind, action *protocol.Action, reason string) (protocol.DecisionOutcome, error) {
	outcome := m.outcomeLocked(s, decisionID, kind, action, reason)
	s.resolved[decisionID] = outcome
	s.pending = nil
	return outcome, nil
}
func (m *SessionManager) logLocked(s *session, decisionID string, outcome protocol.OutcomeKind, action *protocol.Action, reason string) {
	entry := LogEntry{Version: protocol.SchemaVersion, At: m.now(), Event: string(outcome), GameID: s.gameID, WormID: s.wormID, DecisionID: decisionID, Outcome: outcome, Action: action, Reason: reason, Policy: s.policy.Name()}
	if s.pending != nil && s.pending.request.DecisionID == decisionID {
		observation := s.pending.request.Observation
		entry.WormInstanceID = observation.WormInstanceID
		entry.BrainID = observation.BrainID
		entry.BrainVersion = observation.BrainVersion
		entry.ObservationKey = observation.ObservationKey()
		entry.Scores = copyScores(observation.Scores)
		entry.Provenance = copyStrings(observation.Provenance)
		entry.Latency = m.now().Sub(s.pending.started)
	}
	if provider, ok := s.policy.(ProvenanceProvider); ok {
		if entry.Provenance == nil {
			entry.Provenance = make(map[string]string)
		}
		for key, value := range provider.Provenance() {
			entry.Provenance[key] = value
		}
	}
	m.logger.Record(entry)
}

// Game ID routing is kept on the observation so this package remains a
// transport-neutral adapter.
