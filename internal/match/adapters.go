package match

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"worms.ng/internal/agent"
	"worms.ng/internal/protocol"
)

// PolicyController adapts the engine-independent agent policy boundary.
type PolicyController struct{ policy agent.Policy }

func NewPolicyController(p agent.Policy) (*PolicyController, error) {
	if p == nil {
		return nil, errors.New("match: nil policy")
	}
	return &PolicyController{policy: p}, nil
}
func (c *PolicyController) Decide(ctx context.Context, r protocol.DecisionRequest) (protocol.Action, error) {
	return c.policy.Decide(ctx, r)
}
func (c *PolicyController) Name() string {
	if n, ok := c.policy.(interface{ Name() string }); ok {
		return n.Name()
	}
	return "policy"
}
func (c *PolicyController) Provenance() map[string]string {
	if p, ok := c.policy.(interface{ Provenance() map[string]string }); ok {
		return p.Provenance()
	}
	return map[string]string{"controller": c.Name()}
}

// StatefulController allows a resumed match to restore policy progress without
// coupling the match package to agent implementation details.
type StatefulController interface {
	ControllerState() json.RawMessage
	RestoreControllerState(json.RawMessage) error
}

func restoreRequest(id string, legal []protocol.Action) protocol.DecisionRequest {
	neighbors := make([]protocol.Neighbor, 6)
	for i := range neighbors {
		neighbors[i] = protocol.Neighbor{Direction: i, Position: protocol.Position{X: 1, Y: 1}}
	}
	trails := make([]protocol.TrailState, 6)
	for i := range trails {
		trails[i] = protocol.TrailEmpty
	}
	deadline := time.Now().Add(time.Hour)
	return protocol.DecisionRequest{Version: protocol.SchemaVersion, DecisionID: id, Deadline: deadline, Observation: protocol.Observation{Version: protocol.SchemaVersion, GameID: "restore", WormID: "restore", DecisionID: id, Position: protocol.Position{X: 1, Y: 1}, Orientation: 0, Neighbors: neighbors, TrailStates: trails, LegalActions: legal, Mode: "restore", Deadline: deadline}}
}

type ControllerOutcomeError struct {
	Kind   protocol.OutcomeKind
	Reason string
}

func (e *ControllerOutcomeError) Error() string {
	if e.Reason == "" {
		return string(e.Kind)
	}
	return e.Reason
}

// ScriptedController consumes a fixed sequence and is deterministic across
// retries. It uses the existing validated ScriptedPolicy implementation.
type ScriptedController struct {
	policy  *agent.ScriptedPolicy
	actions []protocol.Action
	calls   int
}

func NewScriptedController(actions ...protocol.Action) (*ScriptedController, error) {
	p, e := agent.NewScriptedPolicy(actions...)
	if e != nil {
		return nil, e
	}
	return &ScriptedController{policy: p, actions: append([]protocol.Action(nil), actions...)}, nil
}
func NewScriptedControllerUnchecked(actions ...protocol.Action) *ScriptedController {
	return &ScriptedController{policy: agent.NewScriptedPolicyUnchecked(actions...), actions: append([]protocol.Action(nil), actions...)}
}
func (c *ScriptedController) Decide(ctx context.Context, r protocol.DecisionRequest) (protocol.Action, error) {
	a, err := c.policy.Decide(ctx, r)
	if err == nil {
		c.calls++
	}
	return a, err
}
func (c *ScriptedController) Name() string { return "scripted" }
func (c *ScriptedController) Provenance() map[string]string {
	return map[string]string{"controller": "scripted"}
}
func (c *ScriptedController) Reset() { c.policy.Reset(); c.calls = 0 }
func (c *ScriptedController) ControllerState() json.RawMessage {
	b, _ := json.Marshal(struct {
		Calls int `json:"calls"`
	}{c.calls})
	return b
}
func (c *ScriptedController) RestoreControllerState(raw json.RawMessage) error {
	var st struct {
		Calls int `json:"calls"`
	}
	if err := json.Unmarshal(raw, &st); err != nil || st.Calls < 0 {
		if err == nil {
			err = errors.New("invalid scripted controller state")
		}
		return err
	}
	c.Reset()
	for i := range st.Calls {
		if i >= len(c.actions) {
			return errors.New("scripted controller state exceeds script")
		}
		req := restoreRequest(fmt.Sprintf("restore:%d", i), []protocol.Action{c.actions[i]})
		if _, err := c.policy.Decide(context.Background(), req); err != nil {
			return err
		}
		c.calls++
	}
	return nil
}

// RandomController chooses from the already ordered legal actions using a
// private seeded policy, never the process-global random source.
type RandomController struct {
	policy *agent.RandomPolicy
	seed   int64
	calls  int
}

func NewRandomController(seed int64) *RandomController {
	return &RandomController{policy: agent.NewRandomPolicy(seed), seed: seed}
}
func (c *RandomController) Decide(_ context.Context, r protocol.DecisionRequest) (protocol.Action, error) {
	if err := r.Validate(); err != nil {
		return protocol.Action{}, err
	}
	legal := r.Observation.LegalActions
	if len(legal) == 0 {
		return protocol.Action{}, errors.New("random controller: no legal actions")
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", c.seed, r.DecisionID)))
	idx := int(binary.BigEndian.Uint64(h[:8]) % uint64(len(legal)))
	c.calls++
	return legal[idx], nil
}
func (c *RandomController) Name() string { return "random" }
func (c *RandomController) Seed() int64  { return c.seed }
func (c *RandomController) Provenance() map[string]string {
	return map[string]string{"policy": c.Name(), "seed": fmt.Sprintf("%d", c.seed)}
}
func (c *RandomController) ControllerState() json.RawMessage {
	b, _ := json.Marshal(struct {
		Seed  int64 `json:"seed"`
		Calls int   `json:"calls"`
	}{c.seed, c.calls})
	return b
}
func (c *RandomController) RestoreControllerState(raw json.RawMessage) error {
	var st struct {
		Seed  int64 `json:"seed"`
		Calls int   `json:"calls"`
	}
	if err := json.Unmarshal(raw, &st); err != nil || st.Calls < 0 || st.Seed != c.seed {
		if err == nil {
			err = errors.New("invalid random controller state")
		}
		return err
	}
	c.policy = agent.NewRandomPolicy(c.seed)
	c.calls = 0
	for range st.Calls {
		req := restoreRequest("restore", []protocol.Action{{Kind: protocol.ActionMove, Direction: 0}, {Kind: protocol.ActionMove, Direction: 1}, {Kind: protocol.ActionMove, Direction: 2}, {Kind: protocol.ActionMove, Direction: 3}, {Kind: protocol.ActionMove, Direction: 4}, {Kind: protocol.ActionMove, Direction: 5}})
		if _, err := c.policy.Decide(context.Background(), req); err != nil {
			return err
		}
		c.calls++
	}
	return nil
}

type ExternalDecider func(context.Context, protocol.DecisionRequest) (protocol.Action, error)
type ExternalController struct {
	decider    ExternalDecider
	name       string
	provenance map[string]string
}

func NewExternalController(d ExternalDecider) (*ExternalController, error) {
	if d == nil {
		return nil, errors.New("match: nil external decider")
	}
	return &ExternalController{decider: d, name: "external"}, nil
}
func (c *ExternalController) Decide(ctx context.Context, r protocol.DecisionRequest) (protocol.Action, error) {
	if err := r.Validate(); err != nil {
		return protocol.Action{}, err
	}
	return c.decider(ctx, r)
}
func (c *ExternalController) Name() string { return c.name }
func (c *ExternalController) Provenance() map[string]string {
	out := map[string]string{"controller": c.name}
	for k, v := range c.provenance {
		out[k] = v
	}
	return out
}
func (c *ExternalController) SetProvenance(p map[string]string) {
	c.provenance = map[string]string{}
	for k, v := range p {
		c.provenance[k] = v
	}
}

// LLMController is intentionally policy-backed: OpenAI-compatible and other
// LLM adapters already enforce strict protocol validation in agent.Policy.
type LLMController struct {
	policy agent.Policy
	mu     sync.Mutex
}

func NewLLMController(p agent.Policy) (*LLMController, error) {
	if p == nil {
		return nil, errors.New("match: nil llm policy")
	}
	return &LLMController{policy: p}, nil
}
func (c *LLMController) Decide(ctx context.Context, r protocol.DecisionRequest) (protocol.Action, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, e := c.policy.Decide(ctx, r)
	if e != nil {
		return a, e
	}
	if e = a.Validate(); e != nil {
		return a, fmt.Errorf("llm action: %w", e)
	}
	return a, nil
}
func (c *LLMController) Name() string { return "llm" }
func (c *LLMController) Provenance() map[string]string {
	if p, ok := c.policy.(interface{ Provenance() map[string]string }); ok {
		return p.Provenance()
	}
	return map[string]string{"controller": "llm"}
}

// Convenience aliases for callers that prefer adapter-oriented names.
func NewScripted(actions ...protocol.Action) (Controller, error) {
	return NewScriptedController(actions...)
}
func NewRandom(seed int64) Controller                   { return NewRandomController(seed) }
func NewExternal(d ExternalDecider) (Controller, error) { return NewExternalController(d) }
func NewLLM(p agent.Policy) (Controller, error)         { return NewLLMController(p) }

// SessionController routes decisions through the authenticated SessionManager
// transport, so in-process and external policies share identical validation,
// deadline, and outcome semantics.
type SessionController struct {
	manager     *agent.SessionManager
	credentials agent.Credentials
}

func NewSessionController(manager *agent.SessionManager, credentials agent.Credentials) (*SessionController, error) {
	if manager == nil || credentials.GameID == "" || credentials.Token == "" {
		return nil, errors.New("match: invalid session controller")
	}
	return &SessionController{manager: manager, credentials: credentials}, nil
}
func (c *SessionController) Decide(ctx context.Context, r protocol.DecisionRequest) (protocol.Action, error) {
	outcome, err := c.manager.Request(ctx, c.credentials, r)
	if err != nil {
		return protocol.Action{}, err
	}
	if outcome.Action == nil {
		return protocol.Action{}, &ControllerOutcomeError{Kind: outcome.Outcome, Reason: outcome.Reason}
	}
	return *outcome.Action, nil
}
func (c *SessionController) Name() string { return "session" }
func (c *SessionController) Provenance() map[string]string {
	return map[string]string{"controller": "session", "game_id": c.credentials.GameID}
}
