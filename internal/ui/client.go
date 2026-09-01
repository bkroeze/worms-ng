package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	"worms.ng/internal/protocol"
)

// API DTOs intentionally contain transport data only; the browser never opens
// SQLite and does not import a server implementation.
type HealthStatus struct {
	Version, Status, Database string
	RecordedChecks            int
	Message                   string
}

type ParticipantSummary struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	BrainVersionID string          `json:"brain_version_id"`
	Kind           string          `json:"kind"`
	Score          int64           `json:"score"`
	Color          string          `json:"color,omitempty"`
	Start          *StatePoint     `json:"start,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

type GameSummary struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	Status       string               `json:"status"`
	Ruleset      string               `json:"ruleset,omitempty"`
	Seed         int64                `json:"seed"`
	Width        int                  `json:"width,omitempty"`
	Height       int                  `json:"height,omitempty"`
	Players      int                  `json:"players"`
	Tick         int                  `json:"tick"`
	Sequence     int64                `json:"sequence"`
	Cursor       int64                `json:"cursor"`
	EventHash    string               `json:"event_hash"`
	Participants []ParticipantSummary `json:"participants"`
}

type BrainSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type InspectorRule struct {
	Mask         uint8             `json:"mask"`
	Orientation  int               `json:"orientation"`
	Incoming     int               `json:"incoming"`
	Action       int               `json:"action"`
	ActionName   string            `json:"action_name"`
	Directions   []string          `json:"directions"`
	Legal        []int             `json:"legal"`
	TrailMask    uint8             `json:"trail_mask"`
	OccupiedMask uint8             `json:"occupied_mask"`
	Provenance   map[string]string `json:"provenance"`
}

// UnmarshalJSON accepts the canonical numeric rule fields and the string mask
// produced when a persisted rules map is projected through JSON. Presentation
// fields are derived when the server omits them; paging and filtering remain
// exclusively server-authoritative.
func (r *InspectorRule) UnmarshalJSON(data []byte) error {
	var wire struct {
		Mask         json.RawMessage   `json:"mask"`
		Orientation  *int              `json:"orientation"`
		Incoming     *int              `json:"incoming"`
		Action       int               `json:"action"`
		ActionName   string            `json:"action_name"`
		Directions   []string          `json:"directions"`
		Legal        []int             `json:"legal"`
		TrailMask    json.RawMessage   `json:"trail_mask"`
		OccupiedMask json.RawMessage   `json:"occupied_mask"`
		Provenance   map[string]string `json:"provenance"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	mask, err := inspectorMask(wire.Mask)
	if err != nil {
		return fmt.Errorf("inspect rule mask: %w", err)
	}
	orientation := 0
	if wire.Orientation != nil {
		orientation = *wire.Orientation
	}
	*r = decodedRule(mask, orientation, wire.Action, wire.Provenance)
	if wire.Incoming != nil {
		r.Incoming = *wire.Incoming
	}
	if wire.ActionName != "" {
		r.ActionName = wire.ActionName
	}
	if wire.Directions != nil {
		r.Directions = wire.Directions
	}
	r.Legal = wire.Legal
	if len(wire.TrailMask) > 0 {
		r.TrailMask, err = inspectorMask(wire.TrailMask)
		if err != nil {
			return fmt.Errorf("inspect rule trail mask: %w", err)
		}
	}
	if len(wire.OccupiedMask) > 0 {
		r.OccupiedMask, err = inspectorMask(wire.OccupiedMask)
		if err != nil {
			return fmt.Errorf("inspect rule occupied mask: %w", err)
		}
	}
	return nil
}

func inspectorMask(raw json.RawMessage) (uint8, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var numeric uint8
	if err := json.Unmarshal(raw, &numeric); err == nil {
		if numeric > 63 {
			return 0, errors.New("must be 0..63")
		}
		return numeric, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, errors.New("must be a number or numeric string")
	}
	value, err := strconv.ParseUint(strings.TrimSpace(text), 10, 8)
	if err != nil || value > 63 {
		return 0, errors.New("must be 0..63")
	}
	return uint8(value), nil
}

type InspectorResult struct {
	ID         string          `json:"id"`
	BrainID    string          `json:"brain_id"`
	VersionID  string          `json:"version_id"`
	Version    int             `json:"version_number"`
	Rules      []InspectorRule `json:"rules"`
	Offset     int             `json:"offset"`
	Limit      int             `json:"limit"`
	Total      int             `json:"total"`
	NextOffset int             `json:"next_offset"`
}

func decodedRule(mask uint8, orientation, action int, provenance map[string]string) InspectorRule {
	names := make([]string, 0, 6)
	for d, name := range directionNames {
		if mask&(1<<d) != 0 {
			names = append(names, name)
		}
	}
	actionName := "invalid"
	if action >= 0 && action < len(directionNames) {
		actionName = directionNames[action]
	} else {
		switch action {
		case -1:
			actionName = "GETNEW"
		case -2:
			actionName = "DOAI"
		case -3:
			actionName = "DIE"
		}
	}
	return InspectorRule{Mask: mask & 63, TrailMask: mask & 63, Orientation: orientation, Incoming: orientation, Action: action, ActionName: actionName, Directions: names, Provenance: provenance}
}

// State DTOs mirror the engine's canonical snapshot representation emitted by
// the server's resume and action endpoints.
type StatePoint struct {
	Q int `json:"q"`
	R int `json:"r"`
}

// Extension DTOs mirror the optional HTTP contract without importing the
// engine-coupled extension package into the WASM client.
type ExtensionWeight struct {
	Point  StatePoint `json:"point"`
	Weight int        `json:"weight"`
}

type ExtensionOneWayTrail struct {
	From StatePoint `json:"from"`
	To   StatePoint `json:"to"`
}

type ExtensionConfig struct {
	Version             int                    `json:"version,omitempty"`
	Enabled             bool                   `json:"enabled,omitempty"`
	Width               int                    `json:"width,omitempty"`
	Height              int                    `json:"height,omitempty"`
	Seed                int64                  `json:"seed,omitempty"`
	Obstacles           []StatePoint           `json:"obstacles,omitempty"`
	Holes               []StatePoint           `json:"holes,omitempty"`
	WeightedTerritories []ExtensionWeight      `json:"weighted_territories,omitempty"`
	OneWayTrails        []ExtensionOneWayTrail `json:"one_way_trails,omitempty"`
	TemporaryTrailTTL   uint64                 `json:"temporary_trail_ttl,omitempty"`
	EnergyLimit         int                    `json:"energy_limit,omitempty"`
	Teams               map[string]string      `json:"teams,omitempty"`
	FogOfWar            bool                   `json:"fog_of_war,omitempty"`
	VisibilityRadius    int                    `json:"visibility_radius,omitempty"`
	ObstacleRate        uint8                  `json:"obstacle_rate,omitempty"`
	HoleRate            uint8                  `json:"hole_rate,omitempty"`
}

type ExtensionVisibleCell struct {
	Point          StatePoint `json:"point"`
	Visible        bool       `json:"visible"`
	Obstacle       bool       `json:"obstacle,omitempty"`
	Hole           bool       `json:"hole,omitempty"`
	TerritoryScore int        `json:"territory_score,omitempty"`
}

type ExtensionBaseObservation struct {
	Version              int        `json:"version"`
	WormID               string     `json:"worm_id"`
	Position             StatePoint `json:"position"`
	RawMask              uint8      `json:"raw_mask"`
	Mask                 uint8      `json:"mask"`
	OccupiedMask         uint8      `json:"occupied_mask"`
	Incoming             int        `json:"incoming"`
	Legal                []int      `json:"legal"`
	Scores               []int      `json:"scores"`
	LocalTerritoryCounts [6]uint8   `json:"local_territory_counts"`
	Pending              bool       `json:"pending"`
}

type ExtensionObservation struct {
	Version      int                      `json:"version"`
	WormID       string                   `json:"worm_id"`
	Base         ExtensionBaseObservation `json:"base"`
	Visible      []ExtensionVisibleCell   `json:"visible,omitempty"`
	UnknownCount int                      `json:"unknown_count,omitempty"`
	TeamScore    int                      `json:"team_score,omitempty"`
	Energy       *int                     `json:"energy,omitempty"`
}

type ExtensionResponse struct {
	Config      ExtensionConfig      `json:"config"`
	Observation ExtensionObservation `json:"observation"`
	Scores      map[string]int       `json:"scores,omitempty"`
	Winners     []string             `json:"winners,omitempty"`
	TeamWinners []string             `json:"team_winners,omitempty"`
}

type StateDecision struct {
	WormID  string `json:"worm_id"`
	Mask    uint8  `json:"mask"`
	Slot    int    `json:"slot"`
	Request uint64 `json:"request"`
	Legal   []int  `json:"legal,omitempty"`
}

type StateWorm struct {
	ID           string     `json:"id"`
	Color        string     `json:"color"`
	Position     StatePoint `json:"position"`
	Alive        bool       `json:"alive"`
	Score        int        `json:"score"`
	Previous     int        `json:"previous_direction"`
	CRIX         int        `json:"crix"`
	Controller   string     `json:"controller"`
	Rules        [64]int    `json:"rules"`
	RuleUses     [64]uint32 `json:"rule_uses"`
	BrainID      string     `json:"brain_id"`
	BrainVersion string     `json:"brain_version"`
	BrainSeed    uint64     `json:"brain_seed"`
	Frozen       bool       `json:"frozen"`
}

type StateTrail struct {
	Edge  struct{ A, B StatePoint } `json:"edge"`
	Owner string                    `json:"owner"`
}

type StateTerritory struct {
	ID    StatePoint `json:"id"`
	Mask  uint8      `json:"mask"`
	Color string     `json:"color"`
	Owner string     `json:"owner"`
}

type StateEvent struct {
	Seq        uint64     `json:"seq"`
	Type       string     `json:"type"`
	WormID     string     `json:"worm_id,omitempty"`
	Territory  StatePoint `json:"territory,omitempty"`
	Color      string     `json:"color,omitempty"`
	Tick       uint64     `json:"tick"`
	Mask       uint8      `json:"mask,omitempty"`
	Request    uint64     `json:"request,omitempty"`
	RuleMask   uint8      `json:"rule_mask,omitempty"`
	RuleAction int        `json:"rule_action,omitempty"`
}

type StateProvenance struct {
	Ruleset string `json:"ruleset"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

type GameState struct {
	Width, Height int
	Topology      int `json:"topology"`
	Mode          int `json:"mode"`
	Tick, Round   uint64
	Worms         []StateWorm      `json:"worms"`
	Trails        []StateTrail     `json:"trails"`
	Territories   []StateTerritory `json:"territories"`
	Events        []StateEvent     `json:"events"`
	ActiveSlot    int              `json:"active_slot"`
	Pending       *StateDecision   `json:"pending,omitempty"`
	GameOver      bool             `json:"game_over"`
	Provenance    StateProvenance  `json:"provenance"`
}

type GameResponse struct {
	Game      GameSummary        `json:"game"`
	State     GameState          `json:"state"`
	Events    []StateEvent       `json:"events"`
	Extension *ExtensionResponse `json:"extension,omitempty"`
}

type CreateGameRequest struct {
	Version         string               `json:"version"`
	ID              string               `json:"id"`
	BrainVersionID  string               `json:"brain_version_id,omitempty"`
	Status          string               `json:"status"`
	Ruleset         string               `json:"ruleset,omitempty"`
	Width           int                  `json:"width,omitempty"`
	Height          int                  `json:"height,omitempty"`
	RulesPayload    json.RawMessage      `json:"rules_payload,omitempty"`
	Seed            int64                `json:"seed"`
	Participants    []ParticipantRequest `json:"participants"`
	ExtensionConfig *ExtensionConfig     `json:"extension_config,omitempty"`
}

type ParticipantRequest struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	BrainVersionID string          `json:"brain_version_id,omitempty"`
	Kind           string          `json:"kind"`
	Color          string          `json:"color,omitempty"`
	Start          *StatePoint     `json:"start,omitempty"`
	Score          int64           `json:"score"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

type ActRequest struct {
	Version   string `json:"version"`
	Cursor    int64  `json:"cursor"`
	EventHash string `json:"event_hash"`
	WormID    string `json:"worm_id"`
	Direction int    `json:"direction"`
}

type TeachRequest struct {
	Version        string `json:"version"`
	Cursor         int64  `json:"cursor"`
	EventHash      string `json:"event_hash"`
	WormID         string `json:"worm_id"`
	Direction      int    `json:"direction"`
	Mask           uint8  `json:"mask"`
	Request        uint64 `json:"request"`
	PendingMask    uint8  `json:"pending_mask"`
	PendingRequest uint64 `json:"pending_request"`
}

type GameCommandRequest struct {
	Version   string          `json:"version"`
	Cursor    int64           `json:"cursor"`
	EventHash string          `json:"event_hash"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type PlannerCapabilities struct {
	Observation string `json:"observation"`
	GlobalState bool   `json:"global_state"`
}

type PlannerConfig struct {
	Version        int                 `json:"version"`
	Mode           string              `json:"mode"`
	Depth          int                 `json:"depth"`
	Seed           int64               `json:"seed"`
	CaptureWeight  int                 `json:"capture_weight"`
	BorderWeight   int                 `json:"border_weight"`
	SurvivalWeight int                 `json:"survival_weight"`
	Capabilities   PlannerCapabilities `json:"capabilities"`
}

type PlanRequest struct {
	Version       string        `json:"version"`
	Cursor        int64         `json:"cursor"`
	EventHash     string        `json:"event_hash"`
	WormID        string        `json:"worm_id"`
	PlannerConfig PlannerConfig `json:"planner_config"`
	Teach         bool          `json:"teach"`
}

type PlannerAlternative struct {
	Action    int    `json:"action"`
	Capture   int    `json:"capture"`
	Border    int    `json:"border"`
	Survival  int    `json:"survival"`
	Lookahead int    `json:"lookahead,omitempty"`
	Total     int    `json:"total"`
	Chosen    bool   `json:"chosen"`
	Reason    string `json:"reason"`
}

type PlannerProvenance struct {
	Version      string               `json:"version"`
	Source       string               `json:"source"`
	BrainID      string               `json:"brain_id,omitempty"`
	BrainVersion string               `json:"brain_version,omitempty"`
	WormID       string               `json:"worm_id"`
	Mask         uint8                `json:"mask"`
	RawMask      string               `json:"raw_mask"`
	Action       int                  `json:"action"`
	Seed         int64                `json:"seed"`
	Mode         string               `json:"mode"`
	Depth        int                  `json:"depth"`
	Capabilities PlannerCapabilities  `json:"capabilities"`
	StateHash    string               `json:"state_hash"`
	Tick         uint64               `json:"tick"`
	Round        uint64               `json:"round"`
	Alternatives []PlannerAlternative `json:"alternatives"`
}

type PlannerDecision struct {
	WormID       string               `json:"worm_id"`
	Mask         uint8                `json:"mask"`
	Action       int                  `json:"action"`
	Alternatives []PlannerAlternative `json:"alternatives"`
	Provenance   PlannerProvenance    `json:"provenance"`
}

type PlanResponse struct {
	Decision PlannerDecision `json:"decision"`
	GameResponse
}

type SharingSource struct {
	WormID         string `json:"worm_id"`
	Team           string `json:"team,omitempty"`
	BrainVersionID string `json:"brain_version_id,omitempty"`
}

type SharingConfig struct {
	Policy         string          `json:"policy"`
	Seed           int64           `json:"seed"`
	NoiseRate      float64         `json:"noise_rate,omitempty"`
	CorruptionRate float64         `json:"corruption_rate,omitempty"`
	Sources        []SharingSource `json:"sources"`
}

type ShareExperimentRequest struct {
	Version            string        `json:"version"`
	SharingConfig      SharingConfig `json:"sharing_config"`
	RecipientVersionID string        `json:"recipient_version_id"`
	SourceVersionIDs   []string      `json:"source_version_ids"`
}

type SharingMetrics struct {
	Derived   int `json:"derived"`
	Versions  int `json:"versions"`
	Changes   int `json:"changes"`
	Additions int `json:"additions"`
	Removals  int `json:"removals"`
}

type SharedBrainVersion struct {
	ID      string `json:"id"`
	BrainID string `json:"brain_id"`
	Version int    `json:"version"`
	Hash    string `json:"hash"`
}

type SharingDerived struct {
	Recipient SharingSource `json:"recipient"`
	Hash      string        `json:"hash"`
}

type ShareExperimentResponse struct {
	Policy        string               `json:"policy"`
	Seed          int64                `json:"seed"`
	Hash          string               `json:"hash"`
	Derived       []SharingDerived     `json:"derived"`
	BrainVersions []SharedBrainVersion `json:"brain_versions"`
	Metrics       SharingMetrics       `json:"metrics"`
}

type TournamentSummary struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Status       string          `json:"status"`
	RulesPayload json.RawMessage `json:"rules_payload"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

type MatchSummary struct {
	ID           string          `json:"id"`
	TournamentID string          `json:"tournament_id"`
	GameID       string          `json:"game_id"`
	Round        int             `json:"round"`
	Status       string          `json:"status"`
	Payload      json.RawMessage `json:"payload"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

type APIError struct {
	Status  int
	Code    string
	Message string
	Details json.RawMessage
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

type Result[T any] struct {
	Value    T
	Err      error
	Sequence uint64
}

type resource uint8

const (
	resourceHealth resource = iota + 1
	resourceGames
	resourceBrains
	resourceInspector
	resourceTournament
	resourceGame
)

type HTTPClient struct {
	baseURL, version                                   string
	client                                             *http.Client
	sequence                                           atomic.Uint64
	health, games, brains, inspector, tournament, game atomic.Uint64
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), version: protocol.APIVersion, client: http.DefaultClient}
}
func (c *HTTPClient) WithHTTPClient(h *http.Client) *HTTPClient {
	if h != nil {
		c.client = h
	}
	return c
}
func (c *HTTPClient) CurrentSequence() uint64 { return c.sequence.Load() }
func (c *HTTPClient) IsCurrent(sequence uint64) bool {
	if sequence == 0 {
		return false
	}
	for _, generation := range []*atomic.Uint64{&c.health, &c.games, &c.brains, &c.inspector, &c.tournament, &c.game} {
		if generation.Load() == sequence {
			return true
		}
	}
	return false
}
func (c *HTTPClient) generation(r resource) *atomic.Uint64 {
	switch r {
	case resourceHealth:
		return &c.health
	case resourceGames:
		return &c.games
	case resourceBrains:
		return &c.brains
	case resourceInspector:
		return &c.inspector
	case resourceTournament:
		return &c.tournament
	default:
		return &c.game
	}
}
func (c *HTTPClient) IsCurrentFor(r resource, sequence uint64) bool {
	return sequence != 0 && sequence == c.generation(r).Load()
}
func (c *HTTPClient) next(r resource) uint64 {
	seq := c.sequence.Add(1)
	c.generation(r).Store(seq)
	return seq
}
func (c *HTTPClient) endpoint(path string) string {
	return c.baseURL + "/api/" + c.version + "/" + strings.TrimLeft(path, "/")
}

func (c *HTTPClient) get(ctx context.Context, path string, dst any, resources ...resource) (seq uint64, err error) {
	r := resourceGame
	if len(resources) > 0 {
		r = resources[0]
	}
	seq = c.next(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(path), nil)
	if err != nil {
		return seq, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return seq, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close response body: %w", closeErr)
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return seq, decodeAPIError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return seq, err
	}
	return seq, nil
}

func (c *HTTPClient) do(ctx context.Context, method, path string, body any, dst any, r resource) (seq uint64, err error) {
	seq = c.next(r)
	encoded, err := json.Marshal(body)
	if err != nil {
		return seq, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), bytes.NewReader(encoded))
	if err != nil {
		return seq, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return seq, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close response body: %w", closeErr)
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return seq, decodeAPIError(resp)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			return seq, err
		}
	}
	return seq, nil
}

func decodeAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var wire struct {
		Error struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	_ = json.Unmarshal(raw, &wire)
	if wire.Error.Code != "" || wire.Error.Message != "" {
		wire.Code, wire.Message, wire.Details = wire.Error.Code, wire.Error.Message, wire.Error.Details
	}
	if wire.Message == "" {
		wire.Message = strings.TrimSpace(string(raw))
	}
	if wire.Message == "" {
		wire.Message = resp.Status
	}
	return &APIError{Status: resp.StatusCode, Code: wire.Code, Message: wire.Message, Details: wire.Details}
}

func checkVersion(v string) error {
	if v == "" {
		return errors.New("api response missing version")
	}
	if v != protocol.APIVersion {
		return fmt.Errorf("unsupported api version %q", v)
	}
	return nil
}

func (c *HTTPClient) Health(ctx context.Context) (HealthStatus, uint64, error) {
	var wire struct {
		Version, Status string
		Demo            struct {
			Message, Database string
			RecordedChecks    int `json:"recorded_checks"`
		} `json:"demo"`
	}
	seq, err := c.get(ctx, "health", &wire, resourceHealth)
	if err != nil {
		return HealthStatus{}, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return HealthStatus{}, seq, err
	}
	return HealthStatus{wire.Version, wire.Status, wire.Demo.Database, wire.Demo.RecordedChecks, wire.Demo.Message}, seq, nil
}

func (c *HTTPClient) Games(ctx context.Context) ([]GameSummary, uint64, error) {
	var wire struct {
		Version string        `json:"version"`
		Games   []GameSummary `json:"games"`
	}
	seq, err := c.get(ctx, "games", &wire, resourceGames)
	if err != nil {
		return nil, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return nil, seq, err
	}
	return wire.Games, seq, nil
}

func (c *HTTPClient) Brains(ctx context.Context) ([]BrainSummary, uint64, error) {
	var wire struct {
		Version string         `json:"version"`
		Brains  []BrainSummary `json:"brains"`
	}
	seq, err := c.get(ctx, "brains", &wire, resourceBrains)
	if err != nil {
		return nil, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return nil, seq, err
	}
	return wire.Brains, seq, nil
}

func urlPathEscape(s string) string { return url.PathEscape(s) }

func (c *HTTPClient) Inspect(ctx context.Context, brainID string) (InspectorResult, uint64, error) {
	return c.InspectPage(ctx, brainID, 0, 25, "", 0)
}
func (c *HTTPClient) InspectPage(ctx context.Context, brainID string, version, limit int, filter string, offset int) (InspectorResult, uint64, error) {
	query := url.Values{}
	if version > 0 {
		query.Set("version", strconv.Itoa(version))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	if filter != "" {
		query.Set("filter", filter)
	}
	path := "brains/" + urlPathEscape(brainID) + "/inspect"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var wire struct {
		Version string `json:"version"`
		InspectorResult
	}
	seq, err := c.get(ctx, path, &wire, resourceInspector)
	if err != nil {
		return InspectorResult{}, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return InspectorResult{}, seq, err
	}
	if wire.Limit == 0 {
		wire.Limit = limit
	}
	return wire.InspectorResult, seq, nil
}

func (c *HTTPClient) CreateGame(ctx context.Context, in CreateGameRequest) (GameSummary, uint64, error) {
	in.Version = protocol.APIVersion
	var wire struct {
		Version string      `json:"version"`
		Game    GameSummary `json:"game"`
	}
	seq, err := c.do(ctx, http.MethodPost, "games", in, &wire, resourceGame)
	if err != nil {
		return GameSummary{}, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return GameSummary{}, seq, err
	}
	return wire.Game, seq, nil
}

func (c *HTTPClient) Resume(ctx context.Context, id string) (GameResponse, uint64, error) {
	var wire struct {
		Version   string             `json:"version"`
		Game      GameSummary        `json:"game"`
		State     GameState          `json:"state"`
		Events    []StateEvent       `json:"events"`
		Extension *ExtensionResponse `json:"extension,omitempty"`
	}
	seq, err := c.get(ctx, "games/"+url.PathEscape(id)+"/resume", &wire, resourceGame)
	if err != nil {
		return GameResponse{}, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return GameResponse{}, seq, err
	}
	return GameResponse{Game: wire.Game, State: wire.State, Events: wire.Events, Extension: wire.Extension}, seq, nil
}

func (c *HTTPClient) Act(ctx context.Context, id string, in ActRequest) (GameResponse, uint64, error) {
	in.Version = protocol.APIVersion
	return c.gameCommand(ctx, id, "act", in)
}
func (c *HTTPClient) Teach(ctx context.Context, id string, in TeachRequest) (GameResponse, uint64, error) {
	in.Version = protocol.APIVersion
	return c.gameCommand(ctx, id, "teach", in)
}
func (c *HTTPClient) Tick(ctx context.Context, id string, in GameCommandRequest) (GameResponse, uint64, error) {
	in.Version = protocol.APIVersion
	return c.gameCommand(ctx, id, "tick", in)
}
func (c *HTTPClient) Pause(ctx context.Context, id string, in GameCommandRequest, paused bool) (GameResponse, uint64, error) {
	in.Version = protocol.APIVersion
	status := "active"
	if paused {
		status = "paused"
	}
	in.Payload, _ = json.Marshal(map[string]string{"status": status})
	return c.gameCommand(ctx, id, "pause", in)
}
func (c *HTTPClient) Abort(ctx context.Context, id string, in GameCommandRequest) (GameResponse, uint64, error) {
	in.Version = protocol.APIVersion
	return c.gameCommand(ctx, id, "abort", in)
}
func (c *HTTPClient) gameCommand(ctx context.Context, id, operation string, in any) (GameResponse, uint64, error) {
	var wire struct {
		Version   string             `json:"version"`
		Game      GameSummary        `json:"game"`
		State     GameState          `json:"state"`
		Events    []StateEvent       `json:"events"`
		Extension *ExtensionResponse `json:"extension,omitempty"`
	}
	seq, err := c.do(ctx, http.MethodPost, "games/"+url.PathEscape(id)+"/"+operation, in, &wire, resourceGame)
	if err != nil {
		return GameResponse{}, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return GameResponse{}, seq, err
	}
	return GameResponse{Game: wire.Game, State: wire.State, Events: wire.Events, Extension: wire.Extension}, seq, nil
}
func (c *HTTPClient) Plan(ctx context.Context, id string, in PlanRequest) (PlanResponse, uint64, error) {
	in.Version = protocol.APIVersion
	var wire struct {
		Version   string             `json:"version"`
		Decision  PlannerDecision    `json:"decision"`
		Game      GameSummary        `json:"game"`
		State     GameState          `json:"state"`
		Events    []StateEvent       `json:"events"`
		Extension *ExtensionResponse `json:"extension,omitempty"`
	}
	seq, err := c.do(ctx, http.MethodPost, "games/"+url.PathEscape(id)+"/plan", in, &wire, resourceGame)
	if err != nil {
		return PlanResponse{}, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return PlanResponse{}, seq, err
	}
	return PlanResponse{Decision: wire.Decision, GameResponse: GameResponse{Game: wire.Game, State: wire.State, Events: wire.Events, Extension: wire.Extension}}, seq, nil
}

func (c *HTTPClient) ShareExperiment(ctx context.Context, in ShareExperimentRequest) (ShareExperimentResponse, uint64, error) {
	in.Version = protocol.APIVersion
	var wire struct {
		Version string `json:"version"`
		ShareExperimentResponse
	}
	seq, err := c.do(ctx, http.MethodPost, "experiments/share", in, &wire, resourceBrains)
	if err != nil {
		return ShareExperimentResponse{}, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return ShareExperimentResponse{}, seq, err
	}
	return wire.ShareExperimentResponse, seq, nil
}

func (c *HTTPClient) Tournaments(ctx context.Context) ([]TournamentSummary, uint64, error) {
	var wire struct {
		Version     string              `json:"version"`
		Tournaments []TournamentSummary `json:"tournaments"`
	}
	seq, err := c.get(ctx, "tournaments", &wire, resourceTournament)
	if err != nil {
		return nil, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return nil, seq, err
	}
	return wire.Tournaments, seq, nil
}

func (c *HTTPClient) Matches(ctx context.Context, tournamentID string) ([]MatchSummary, uint64, error) {
	var wire struct {
		Version string         `json:"version"`
		Matches []MatchSummary `json:"matches"`
	}
	seq, err := c.get(ctx, "tournaments/"+url.PathEscape(tournamentID)+"/matches", &wire, resourceTournament)
	if err != nil {
		return nil, seq, err
	}
	if err = checkVersion(wire.Version); err != nil {
		return nil, seq, err
	}
	return wire.Matches, seq, nil
}

func (c *HTTPClient) HealthAsync(ctx context.Context) <-chan Result[HealthStatus] {
	ch := make(chan Result[HealthStatus], 1)
	go func() { v, s, err := c.Health(ctx); ch <- Result[HealthStatus]{v, err, s}; close(ch) }()
	return ch
}
func (c *HTTPClient) GamesAsync(ctx context.Context) <-chan Result[[]GameSummary] {
	ch := make(chan Result[[]GameSummary], 1)
	go func() { v, s, err := c.Games(ctx); ch <- Result[[]GameSummary]{v, err, s}; close(ch) }()
	return ch
}
func (c *HTTPClient) BrainsAsync(ctx context.Context) <-chan Result[[]BrainSummary] {
	ch := make(chan Result[[]BrainSummary], 1)
	go func() { v, s, err := c.Brains(ctx); ch <- Result[[]BrainSummary]{v, err, s}; close(ch) }()
	return ch
}
func (c *HTTPClient) InspectAsync(ctx context.Context, id string) <-chan Result[InspectorResult] {
	ch := make(chan Result[InspectorResult], 1)
	go func() { v, s, err := c.Inspect(ctx, id); ch <- Result[InspectorResult]{v, err, s}; close(ch) }()
	return ch
}
