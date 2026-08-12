// Package agent contains synchronous, engine-independent agent orchestration.
package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"

	"worms.ng/internal/protocol"
)

// Policy chooses one action for one validated request. Implementations must
// not mutate the request and should be deterministic when configured so.
type Policy interface {
	Name() string
	Decide(context.Context, protocol.DecisionRequest) (protocol.Action, error)
}

// ProvenanceProvider lets logs identify a policy without exposing secrets.
type ProvenanceProvider interface {
	Provenance() map[string]string
}

func validateRequest(request protocol.DecisionRequest) error {
	if err := request.Validate(); err != nil {
		return fmt.Errorf("invalid decision request: %w", err)
	}
	return nil
}

func isLegal(action protocol.Action, legal []protocol.Action) bool {
	for _, candidate := range legal {
		if candidate.Kind == action.Kind && candidate.Direction == action.Direction {
			return true
		}
	}
	return false
}

// ScriptStep couples an action with the observation key it was authored for.
// An empty ExpectedPatternKey explicitly opts out of key checking.
type ScriptStep struct {
	ExpectedPatternKey string
	Action             protocol.Action
}

// ScriptedPolicy consumes a fixed sequence of actions and, when configured,
// asserts the adapter-provided pattern key before consuming each step.
type ScriptedPolicy struct {
	mu       sync.Mutex
	actions  []protocol.Action
	expected []string
	index    int
	name     string
}

func NewScriptedPolicy(actions ...protocol.Action) (*ScriptedPolicy, error) {
	copyActions := append([]protocol.Action(nil), actions...)
	for i, action := range copyActions {
		if err := action.Validate(); err != nil {
			return nil, fmt.Errorf("script action %d: %w", i, err)
		}
	}
	return &ScriptedPolicy{actions: copyActions, name: "scripted"}, nil
}

// NewScriptedPolicyWithKeys constructs a script that checks every non-empty
// expected key against request.Observation.PatternKey before choosing.
func NewScriptedPolicyWithKeys(steps ...ScriptStep) (*ScriptedPolicy, error) {
	actions := make([]protocol.Action, len(steps))
	expected := make([]string, len(steps))
	for i, step := range steps {
		if err := step.Action.Validate(); err != nil {
			return nil, fmt.Errorf("script step %d: %w", i, err)
		}
		actions[i], expected[i] = step.Action, step.ExpectedPatternKey
	}
	return &ScriptedPolicy{actions: actions, expected: expected, name: "scripted"}, nil
}

// NewScriptedPolicySteps is a descriptive alias for transport/config adapters.
func NewScriptedPolicySteps(steps ...ScriptStep) (*ScriptedPolicy, error) {
	return NewScriptedPolicyWithKeys(steps...)
}

// NewScriptedPolicyUnchecked is a convenience for callers that construct
// scripts dynamically; invalid entries fail at decision time.
func NewScriptedPolicyUnchecked(actions ...protocol.Action) *ScriptedPolicy {
	return &ScriptedPolicy{actions: append([]protocol.Action(nil), actions...), name: "scripted"}
}

func (p *ScriptedPolicy) Name() string { return p.name }

func (p *ScriptedPolicy) Decide(_ context.Context, request protocol.DecisionRequest) (protocol.Action, error) {
	if err := validateRequest(request); err != nil {
		return protocol.Action{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.index >= len(p.actions) {
		return protocol.Action{}, errors.New("script exhausted")
	}
	if p.index < len(p.expected) && p.expected[p.index] != "" && p.expected[p.index] != request.Observation.ObservationKey() {
		return protocol.Action{}, fmt.Errorf("script pattern mismatch at step %d: expected %q, got %q", p.index, p.expected[p.index], request.Observation.ObservationKey())
	}
	action := p.actions[p.index]
	if err := action.Validate(); err != nil {
		return protocol.Action{}, err
	}
	if !isLegal(action, request.Observation.LegalActions) {
		return protocol.Action{}, fmt.Errorf("scripted action is not legal: %+v", action)
	}
	return action, nil
}

func (p *ScriptedPolicy) Reset() {
	p.mu.Lock()
	p.index = 0
	p.mu.Unlock()
}

// RandomPolicy chooses uniformly from the request's already ordered legal
// actions. A private rand.Rand makes seeded runs reproducible and concurrency
// safe without touching the process-global random source.
type RandomPolicy struct {
	mu   sync.Mutex
	rng  *rand.Rand
	seed int64
}

func NewRandomPolicy(seed int64) *RandomPolicy {
	return &RandomPolicy{rng: rand.New(rand.NewPCG(uint64(seed), 0)), seed: seed}
}

func (p *RandomPolicy) Name() string { return "random" }

func (p *RandomPolicy) Seed() int64 { return p.seed }

func (p *RandomPolicy) Provenance() map[string]string {
	return map[string]string{"policy": p.Name(), "seed": fmt.Sprintf("%d", p.seed)}
}

func (p *RandomPolicy) Decide(_ context.Context, request protocol.DecisionRequest) (protocol.Action, error) {
	if err := validateRequest(request); err != nil {
		return protocol.Action{}, err
	}
	p.mu.Lock()
	choice := request.Observation.LegalActions[p.rng.IntN(len(request.Observation.LegalActions))]
	p.mu.Unlock()
	return choice, nil
}
