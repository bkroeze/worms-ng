// Package protocol contains the versioned, transport-neutral API exchanged
// with external agents. It deliberately does not import the game engine.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	// APIVersion is the version segment used by all HTTP endpoints.
	APIVersion = "v1"
	// SchemaVersion is the external-agent JSON schema version.
	SchemaVersion = "v1"
	// ServiceVersion identifies this workspace bootstrap implementation.
	ServiceVersion = "0.1.0"
)

// Health is the response returned by the versioned health endpoint.
type Health struct {
	Version string `json:"version"`
	Status  string `json:"status"`
	Demo    Demo   `json:"demo"`
}

// Demo is a small, stable payload used to prove end-to-end serving.
type Demo struct {
	Message        string `json:"message"`
	Database       string `json:"database"`
	RecordedChecks int    `json:"recorded_checks"`
}

// DemoResponse is the standalone demo endpoint response.
type DemoResponse struct {
	Version string `json:"version"`
	Demo    Demo   `json:"demo"`
}

// Position uses the logical board coordinates. Coordinates are intentionally
// opaque to this package; an engine adapter owns bounds and neighbor rules.
type Position struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// CoordinateBounds bounds coordinates at a protocol boundary without
// imposing a particular board topology. Bounded boards may use zero-based
// coordinates and may expose a one-cell outside sentinel for edge neighbors.
// Adapters should use ValidateWith when they have stricter board dimensions.
type CoordinateBounds struct {
	MinX int
	MaxX int
	MinY int
	MaxY int
}

// DefaultCoordinateBounds is deliberately generous enough for modern boards,
// while preventing unbounded integers from crossing a transport boundary.
var DefaultCoordinateBounds = CoordinateBounds{
	MinX: -(1 << 20), MaxX: 1 << 20, MinY: -(1 << 20), MaxY: 1 << 20,
}

func (b CoordinateBounds) validate() error {
	if b.MinX > b.MaxX || b.MinY > b.MaxY {
		return errors.New("coordinate bounds must have minimums no greater than maximums")
	}
	return nil
}

func (b CoordinateBounds) contains(p Position) bool {
	return p.X >= b.MinX && p.X <= b.MaxX && p.Y >= b.MinY && p.Y <= b.MaxY
}

func (b CoordinateBounds) check(name string, p Position) error {
	if !b.contains(p) {
		return fmt.Errorf("%s position (%d,%d) is outside configured coordinate bounds [%d..%d]x[%d..%d]", name, p.X, p.Y, b.MinX, b.MaxX, b.MinY, b.MaxY)
	}
	return nil
}

// Neighbor describes one of the six outgoing directions.
type Neighbor struct {
	Direction int      `json:"direction"`
	Position  Position `json:"position"`
	Occupied  bool     `json:"occupied"`
}

// TrailState describes the trail leaving a dot in a direction.
type TrailState string

const (
	TrailEmpty TrailState = "empty"
	TrailOwn   TrailState = "own"
	TrailOther TrailState = "other"
)

// ActionKind is intentionally a closed set. A response containing any other
// action is rejected before it can reach a game adapter.
type ActionKind string

const (
	ActionMove   ActionKind = "move"
	ActionResign ActionKind = "resign"
)

// Direction constants use the canonical absolute clockwise order.
const (
	East      = 0
	SouthEast = 1
	SouthWest = 2
	West      = 3
	NorthWest = 4
	NorthEast = 5
)

const (
	KindMove   = ActionMove
	KindResign = ActionResign
)

// Action is a legal agent decision. Direction is meaningful only for move.
type Action struct {
	Kind      ActionKind `json:"kind"`
	Direction int        `json:"direction,omitempty"`
}

// UnmarshalJSON rejects unknown action fields and catches omitted required
// fields (where an ordinary int field cannot distinguish omission from zero).
func (a *Action) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := strictDecode(data, &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("action must be an object")
	}
	if _, ok := raw["kind"]; !ok {
		return errors.New("action.kind is required")
	}
	for key := range raw {
		if key != "kind" && key != "direction" {
			return fmt.Errorf("action: unknown field %q", key)
		}
	}
	*a = Action{}
	if err := json.Unmarshal(raw["kind"], &a.Kind); err != nil {
		return errors.New("action.kind must be a string")
	}
	if direction, ok := raw["direction"]; ok {
		if a.Kind == ActionResign {
			return errors.New("resign action must not specify direction")
		}
		if err := json.Unmarshal(direction, &a.Direction); err != nil {
			return errors.New("action.direction must be an integer")
		}
	} else if a.Kind == ActionMove {
		return errors.New("move action.direction is required")
	}
	return a.Validate()
}

// Validate checks an action without requiring a JSON round trip.
func (a Action) Validate() error {
	switch a.Kind {
	case ActionMove:
		if a.Direction < 0 || a.Direction > 5 {
			return fmt.Errorf("move direction %d is outside 0..5", a.Direction)
		}
	case ActionResign:
		if a.Direction != 0 {
			return errors.New("resign action must not specify direction")
		}
	default:
		return fmt.Errorf("unknown action kind %q", a.Kind)
	}
	return nil
}
func (a Action) MarshalJSON() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if a.Kind == ActionMove {
		return json.Marshal(struct {
			Kind      ActionKind `json:"kind"`
			Direction int        `json:"direction"`
		}{a.Kind, a.Direction})
	}
	return json.Marshal(struct {
		Kind ActionKind `json:"kind"`
	}{a.Kind})
}

// IsMove reports whether this action advances the worm.
func (a Action) IsMove() bool { return a.Kind == ActionMove }

// Observation is the complete, bounded input to one agent decision. TrailStates
// is positional and always follows the canonical East, SouthEast, SouthWest,
// West, NorthWest, NorthEast order. Optional provenance fields are identifiers
// only; credentials and hidden engine state MUST NOT be placed in an observation.
type Observation struct {
	Version        string            `json:"version"`
	GameID         string            `json:"game_id"`
	WormID         string            `json:"worm_id"`
	WormInstanceID string            `json:"worm_instance_id,omitempty"`
	DecisionID     string            `json:"decision_id"`
	Tick           uint64            `json:"tick"`
	Position       Position          `json:"position"`
	Orientation    int               `json:"orientation"`
	Neighbors      []Neighbor        `json:"neighbors"`
	TrailStates    []TrailState      `json:"trail_states"`
	LegalActions   []Action          `json:"legal_actions"`
	Scores         map[string]int    `json:"scores"`
	Mode           string            `json:"mode"`
	Deadline       time.Time         `json:"deadline"`
	BrainID        string            `json:"brain_id,omitempty"`
	BrainVersion   string            `json:"brain_version,omitempty"`
	PatternKey     string            `json:"pattern_key,omitempty"`
	Provenance     map[string]string `json:"provenance,omitempty"`
}

// ObservationKey is the stable key a scripted/controller policy may assert.
// It is supplied by the engine adapter because protocol does not define board
// geometry or orientation canonicalization.
func (o Observation) ObservationKey() string { return o.PatternKey }

// Validate checks the external observation contract with the conservative
// transport coordinate envelope.
func (o Observation) Validate() error { return o.validate(DefaultCoordinateBounds) }

// ValidateWith checks an observation using adapter-supplied coordinate bounds.
// This keeps board geometry out of the protocol package.
func (o Observation) ValidateWith(bounds CoordinateBounds) error {
	return o.validate(bounds)
}

func (o Observation) validate(bounds CoordinateBounds) error {
	if err := bounds.validate(); err != nil {
		return err
	}
	if o.Version != SchemaVersion {
		return fmt.Errorf("unsupported schema version %q", o.Version)
	}
	for name, value := range map[string]string{"game_id": o.GameID, "worm_id": o.WormID, "decision_id": o.DecisionID} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	if o.Orientation < 0 || o.Orientation > 5 {
		return fmt.Errorf("orientation %d is outside 0..5", o.Orientation)
	}
	if err := bounds.check("position", o.Position); err != nil {
		return err
	}
	if len(o.Neighbors) != 6 {
		return fmt.Errorf("neighbors must contain six entries, got %d", len(o.Neighbors))
	}
	seenDirections := [6]bool{}
	for i, n := range o.Neighbors {
		if err := bounds.check(fmt.Sprintf("neighbors[%d]", i), n.Position); err != nil {
			return err
		}
		if n.Direction < 0 || n.Direction > 5 {
			return fmt.Errorf("neighbors[%d] direction is outside 0..5", i)
		}
		if seenDirections[n.Direction] {
			return fmt.Errorf("neighbors[%d] repeats direction %d", i, n.Direction)
		}
		seenDirections[n.Direction] = true
	}
	for i := range seenDirections {
		if !seenDirections[i] {
			return fmt.Errorf("neighbors are missing direction %d", i)
		}
	}
	if len(o.TrailStates) != 6 {
		return fmt.Errorf("trail_states must contain six entries, got %d", len(o.TrailStates))
	}
	for i, state := range o.TrailStates {
		if state != TrailEmpty && state != TrailOwn && state != TrailOther {
			return fmt.Errorf("trail_states[%d] has unknown value %q", i, state)
		}
	}
	if len(o.LegalActions) == 0 {
		return errors.New("legal_actions must not be empty")
	}
	for i, action := range o.LegalActions {
		if err := action.Validate(); err != nil {
			return fmt.Errorf("legal_actions[%d]: %w", i, err)
		}
	}
	if o.Mode == "" {
		return errors.New("mode must not be empty")
	}
	return nil
}

// DecisionRequest wraps one observation and repeats the ID at the transport
// boundary so agents cannot accidentally answer a different turn.
type DecisionRequest struct {
	Version     string      `json:"version"`
	DecisionID  string      `json:"decision_id"`
	Observation Observation `json:"observation"`
	Deadline    time.Time   `json:"deadline"`
}

func (r DecisionRequest) Validate() error {
	if r.Version != SchemaVersion {
		return fmt.Errorf("unsupported schema version %q", r.Version)
	}
	if strings.TrimSpace(r.DecisionID) == "" {
		return errors.New("decision_id must not be empty")
	}
	if r.Observation.DecisionID != r.DecisionID {
		return errors.New("request and observation decision IDs differ")
	}
	if r.Deadline.IsZero() {
		return errors.New("deadline is required")
	}
	if err := r.Observation.Validate(); err != nil {
		return fmt.Errorf("observation: %w", err)
	}
	return nil
}

// DecisionResponse is the only accepted external-agent response.
type DecisionResponse struct {
	Version    string `json:"version"`
	DecisionID string `json:"decision_id"`
	Action     Action `json:"action"`
}

// AgentObservation and Decision are descriptive aliases for integrations
// whose transport names use those terms.
type AgentObservation = Observation
type Decision = DecisionResponse

func (r DecisionResponse) Validate() error {
	if r.Version != SchemaVersion {
		return fmt.Errorf("unsupported schema version %q", r.Version)
	}
	if strings.TrimSpace(r.DecisionID) == "" {
		return errors.New("decision_id must not be empty")
	}
	if err := r.Action.Validate(); err != nil {
		return fmt.Errorf("action: %w", err)
	}
	return nil
}

// OutcomeKind is the explicit terminal/accepted result of a decision.
type OutcomeKind string

const (
	OutcomeAccepted   OutcomeKind = "accepted"
	OutcomeTimeout    OutcomeKind = "timeout"
	OutcomeDisconnect OutcomeKind = "disconnect"
	OutcomeResigned   OutcomeKind = "resigned"
	OutcomeDuplicate  OutcomeKind = "duplicate"
	OutcomeStale      OutcomeKind = "stale"
	OutcomeMalformed  OutcomeKind = "malformed"
	OutcomeIllegal    OutcomeKind = "illegal"
)

// DecisionOutcome is recorded by session managers and loggers.
type DecisionOutcome struct {
	Version    string      `json:"version"`
	GameID     string      `json:"game_id"`
	WormID     string      `json:"worm_id"`
	DecisionID string      `json:"decision_id"`
	Outcome    OutcomeKind `json:"outcome"`
	Action     *Action     `json:"action,omitempty"`
	At         time.Time   `json:"at"`
	Reason     string      `json:"reason,omitempty"`
}

func (o DecisionOutcome) Validate() error {
	if o.Version != SchemaVersion {
		return fmt.Errorf("unsupported schema version %q", o.Version)
	}
	if o.GameID == "" || o.WormID == "" || o.DecisionID == "" {
		return errors.New("outcome IDs must not be empty")
	}
	switch o.Outcome {
	case OutcomeAccepted, OutcomeTimeout, OutcomeDisconnect, OutcomeResigned, OutcomeDuplicate, OutcomeStale, OutcomeMalformed, OutcomeIllegal:
	default:
		return fmt.Errorf("unknown outcome %q", o.Outcome)
	}
	if o.Action != nil {
		return o.Action.Validate()
	}
	return nil
}

// DecodeStrict decodes one complete versioned JSON value and rejects unknown
// fields, trailing values, and trailing non-whitespace bytes.
func DecodeStrict(data []byte, dst any) error {
	if err := strictDecode(data, dst); err != nil {
		return err
	}
	switch value := dst.(type) {
	case *Observation:
		return value.Validate()
	case *DecisionRequest:
		return value.Validate()
	case *DecisionResponse:
		return value.Validate()
	case *DecisionOutcome:
		return value.Validate()
	case *Action:
		return value.Validate()
	default:
		return nil
	}
}

func strictDecode(data []byte, dst any) error {
	if err := rejectDuplicateFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("JSON must contain one value")
		}
		return err
	}
	return nil
}

// rejectDuplicateFields performs a token-level pass because encoding/json
// otherwise silently applies last-value-wins semantics to repeated keys.
func rejectDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	return scanJSONValue(decoder)
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key must be a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("JSON object is not terminated")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("JSON array is not terminated")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func marshalVersioned(value any, validate func() error) ([]byte, error) {
	if err := validate(); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (o Observation) MarshalJSON() ([]byte, error) {
	type plain Observation
	return marshalVersioned(plain(o), o.Validate)
}

func (r DecisionRequest) MarshalJSON() ([]byte, error) {
	type plain DecisionRequest
	return marshalVersioned(plain(r), r.Validate)
}

func (r DecisionResponse) MarshalJSON() ([]byte, error) {
	type plain DecisionResponse
	return marshalVersioned(plain(r), r.Validate)
}

func (o DecisionOutcome) MarshalJSON() ([]byte, error) {
	type plain DecisionOutcome
	return marshalVersioned(plain(o), o.Validate)
}

// ValidateObservation is a convenience for callers that do not need a
// method-value.
func ValidateObservation(o Observation) error { return o.Validate() }

// ValidateObservationWithBounds is the adapter-friendly form of
// Observation.ValidateWith.
func ValidateObservationWithBounds(o Observation, bounds CoordinateBounds) error {
	return o.ValidateWith(bounds)
}
func ValidateDecisionRequest(r DecisionRequest) error { return r.Validate() }

// RejectDuplicateFields checks one JSON value for repeated object member names.
// Callers that add their own strict typed decoder can use this before decoding.
func RejectDuplicateFields(data []byte) error {
	return rejectDuplicateFields(data)
}

func ValidateDecisionResponse(r DecisionResponse) error { return r.Validate() }

func ValidateAction(a Action) error { return a.Validate() }

func DecodeObservation(data []byte) (Observation, error) {
	var value Observation
	return value, DecodeStrict(data, &value)
}

func DecodeDecisionRequest(data []byte) (DecisionRequest, error) {
	var value DecisionRequest
	return value, DecodeStrict(data, &value)
}

func DecodeDecisionResponse(data []byte) (DecisionResponse, error) {
	var value DecisionResponse
	return value, DecodeStrict(data, &value)
}
