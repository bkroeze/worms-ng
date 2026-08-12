package ui

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gioui.org/f32"
)

type Screen uint8

const (
	ScreenSetup Screen = iota
	ScreenPlay
	ScreenGames
	ScreenBrains
	ScreenInspector
	ScreenTournament
	ScreenExperiments
	ScreenError
)

func (s Screen) String() string {
	switch s {
	case ScreenSetup:
		return "setup"
	case ScreenPlay:
		return "play"
	case ScreenGames:
		return "games"
	case ScreenBrains:
		return "brains"
	case ScreenInspector:
		return "inspector"
	case ScreenTournament:
		return "tournament"
	case ScreenExperiments:
		return "experiments"
	case ScreenError:
		return "error"
	}
	return "unknown"
}

const (
	ControllerNew    = "NEW"
	ControllerAuto   = "AUTO"
	ControllerWild   = "WILD"
	ControllerSame   = "SAME"
	ControllerNamed  = "NAMED"
	ControllerAsleep = "-----"
)

var controllerKinds = [...]string{ControllerNew, ControllerAuto, ControllerWild, ControllerSame, ControllerNamed, ControllerAsleep}
var directionNames = [...]string{"E", "SE", "SW", "W", "NW", "NE"}

var rulesetPresets = [...]string{"classic", "modern", "variants-terrain", "variants-trails", "variants-teams-fog"}

func nextRuleset(current string) string {
	for i, preset := range rulesetPresets {
		if current == preset {
			return rulesetPresets[(i+1)%len(rulesetPresets)]
		}
	}
	return rulesetPresets[0]
}

type SetupSlot struct {
	ID, Name, Controller, BrainID, Color string
	Start                                Point
}

type SetupConfig struct {
	SlotCount       int
	Width, Height   int
	Seed            string
	Ruleset         string
	Slots           [4]SetupSlot
	Errors          map[string]string
	ResolvedSeed    int64
	ResolvedRuleset string
	ResolvedWidth   int
	ResolvedHeight  int
	ResolvedStarts  [4]Point
}

func defaultSetup() SetupConfig {
	return SetupConfig{
		SlotCount: 2,
		Width:     18,
		Height:    18,
		Seed:      "1",
		Ruleset:   "classic",
		Slots: [4]SetupSlot{
			{ID: "worm-1", Name: "Amber", Controller: ControllerNew, Color: "amber", Start: Point{X: 1, Y: 1}},
			{ID: "worm-2", Name: "Cobalt", Controller: ControllerWild, Color: "cobalt", Start: Point{X: 18, Y: 18}},
			{ID: "worm-3", Name: "Moss", Controller: ControllerAsleep, Color: "moss", Start: Point{X: 18, Y: 1}},
			{ID: "worm-4", Name: "Rose", Controller: ControllerAsleep, Color: "rose", Start: Point{X: 1, Y: 18}},
		},
	}
}

func (s SetupConfig) Validate() (SetupConfig, bool) {
	s.Errors = make(map[string]string)
	if s.SlotCount < 1 || s.SlotCount > 4 {
		s.Errors["slots"] = "Choose between 1 and 4 slots."
	}
	if s.Width < 4 || s.Width > 64 {
		s.Errors["width"] = "Width must be from 4 to 64."
	}
	if s.Height < 4 || s.Height > 64 {
		s.Errors["height"] = "Height must be from 4 to 64."
	}
	seed, err := strconv.ParseInt(strings.TrimSpace(s.Seed), 10, 64)
	if err != nil {
		s.Errors["seed"] = "Seed must be a whole number."
	} else {
		s.ResolvedSeed = seed
	}
	validRuleset := false
	for _, preset := range rulesetPresets {
		validRuleset = validRuleset || s.Ruleset == preset
	}
	if !validRuleset {
		s.Errors["ruleset"] = "Choose a supported rules preset."
	}
	if s.Ruleset == "variants-teams-fog" && s.SlotCount < 2 {
		s.Errors["slots"] = "Team fog needs at least two slots."
	}
	seenIDs := make(map[string]bool, s.SlotCount)
	seenStarts := make(map[Point]bool, s.SlotCount)
	for i := range min(s.SlotCount, len(s.Slots)) {
		slot := s.Slots[i]
		prefix := fmt.Sprintf("slot.%d.", i)
		if strings.TrimSpace(slot.ID) == "" {
			s.Errors[prefix+"id"] = "Stable worm ID is required."
		} else if seenIDs[slot.ID] {
			s.Errors[prefix+"id"] = "Stable worm IDs must be unique."
		}
		seenIDs[slot.ID] = true
		validController := false
		for _, kind := range controllerKinds {
			validController = validController || slot.Controller == kind
		}
		if !validController {
			s.Errors[prefix+"controller"] = "Choose a supported controller."
		}
		if slot.Controller == ControllerNamed && strings.TrimSpace(slot.BrainID) == "" {
			s.Errors[prefix+"brain"] = "NAMED requires a stable brain version ID."
		}
		if slot.Start.X < 1 || slot.Start.X > s.Width || slot.Start.Y < 1 || slot.Start.Y > s.Height {
			s.Errors[prefix+"start"] = fmt.Sprintf("Start must fit the %d×%d board.", s.Width, s.Height)
		} else if seenStarts[slot.Start] {
			s.Errors[prefix+"start"] = "Start positions must be unique."
		}
		seenStarts[slot.Start] = true
	}
	s.ResolvedRuleset, s.ResolvedWidth, s.ResolvedHeight = s.Ruleset, s.Width, s.Height
	for i := range s.Slots {
		s.ResolvedStarts[i] = s.Slots[i].Start
	}
	return s, len(s.Errors) == 0
}

func (s SetupConfig) ExtensionPreset() *ExtensionConfig {
	if s.Ruleset == "classic" || s.Ruleset == "modern" {
		return nil
	}
	config := &ExtensionConfig{Version: 1, Enabled: true, Width: s.Width, Height: s.Height, Seed: s.ResolvedSeed}
	switch s.Ruleset {
	case "variants-terrain":
		config.ObstacleRate = 12
		config.HoleRate = 4
		config.WeightedTerritories = []ExtensionWeight{{Point: StatePoint{Q: s.Width / 2, R: s.Height / 2}, Weight: 3}}
	case "variants-trails":
		config.TemporaryTrailTTL = 8
		config.EnergyLimit = 24
	case "variants-teams-fog":
		config.EnergyLimit = 24
		config.FogOfWar = true
		config.VisibilityRadius = 2
		config.Teams = make(map[string]string, s.SlotCount)
		for i := range s.SlotCount {
			config.Teams[s.Slots[i].ID] = fmt.Sprintf("team-%d", i%2+1)
		}
	}
	return config
}

func (s *SetupConfig) fitStarts() {
	corners := [4]Point{{1, 1}, {s.Width, s.Height}, {s.Width, 1}, {1, s.Height}}
	for i := range s.Slots {
		s.Slots[i].Start = corners[i]
	}
}

type ToggleState struct{ Grid, Flash, ReducedMotion bool }

type WormView struct {
	ID, Name, Controller, BrainID, Team string
	Position                            Point
	Color                               uint32
	Score                               int
	Alive, Asleep, Active               bool
}

type DecisionView struct {
	WormID  string
	Mask    uint8
	Request uint64
	Legal   [6]bool
}

type CaptureView struct {
	Points map[Point]uint32
	Serial uint64
	Until  time.Time
}

type BoardView struct {
	Width, Height   int
	Topology, Mode  int
	Worms           []WormView
	Trails          []Trail
	Territory       map[Point]uint32
	TerritoryOwners map[Point]string
	Visible         map[Point]bool
	Unknown         map[Point]bool
	Obstacles       map[Point]bool
	Holes           map[Point]bool
	Weights         map[Point]int
	Tick, Round     uint64
	ActiveWorm      string
	Legal           [6]bool
	Pending         *DecisionView
	GameOver        bool
	FogOfWar        bool
	UnknownCount    int
	Provenance      StateProvenance
	Capture         CaptureView
}

type HUDView struct {
	Paused      bool
	Speed       int
	Status      string
	Scores      []ScoreView
	Winners     []string
	Tie         bool
	Energy      int
	HasEnergy   bool
	TeamScore   int
	Team        string
	TeamWinners []string
}

type ScoreView struct {
	ID, Name, Controller, BrainID, Team string
	Score                               int
	Color                               uint32
	Alive, Asleep, Active               bool
}

type ErrorView struct {
	Message, Code, Retry string
}

type InspectorQuery struct {
	BrainID, Version, Filter string
	Offset, Limit            int
	Error                    string
}

type PlannerView struct {
	Config   PlannerConfig
	Teach    bool
	Decision *PlannerDecision
	Error    string
}

type ShareExperimentView struct {
	Policy, RecipientVersionID, SourceVersionIDs string
	Seed, NoiseRate                              string
	Result                                       *ShareExperimentResponse
	Error                                        string
	Running                                      bool
}

type AppView struct {
	Screen      Screen
	Health      HealthStatus
	HealthOK    bool
	Games       []GameSummary
	Brains      []BrainSummary
	Inspector   InspectorResult
	Inspect     InspectorQuery
	ActiveBrain string
	Tournaments []TournamentSummary
	Matches     []MatchSummary
	GameID      string
	GameCursor  int64
	EventHash   string
	Setup       SetupConfig
	Board       BoardView
	HUD         HUDView
	Planner     PlannerView
	Share       ShareExperimentView
	Toggles     ToggleState
	Error       ErrorView
}

type Model struct {
	mu   sync.RWMutex
	view AppView
}

func NewModel() *Model {
	planner := PlannerConfig{Version: 1, Mode: "greedy", Depth: 1, CaptureWeight: 100, BorderWeight: 1, SurvivalWeight: 1, Capabilities: PlannerCapabilities{Observation: "local"}}
	share := ShareExperimentView{Policy: "all_worms", Seed: "1", NoiseRate: "0"}
	return &Model{view: AppView{Screen: ScreenSetup, Setup: defaultSetup(), Inspect: InspectorQuery{Limit: 12}, HUD: HUDView{Speed: 5}, Planner: PlannerView{Config: planner}, Share: share, Toggles: ToggleState{Grid: true}}}
}

// Snapshot is allocation-free. Published slices and maps are immutable:
// updates always replace them under the lock, and renderers only read them.
// This keeps large boards and brain pages out of the per-frame allocation path.
func (m *Model) Snapshot() AppView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.view
}

func cloneView(v AppView) AppView {
	v.Games = append([]GameSummary(nil), v.Games...)
	v.Brains = append([]BrainSummary(nil), v.Brains...)
	v.Tournaments = append([]TournamentSummary(nil), v.Tournaments...)
	v.Matches = append([]MatchSummary(nil), v.Matches...)
	v.Inspector.Rules = append([]InspectorRule(nil), v.Inspector.Rules...)
	v.Setup.Errors = copyErrors(v.Setup.Errors)
	v.Board.Worms = append([]WormView(nil), v.Board.Worms...)
	v.Board.Trails = append([]Trail(nil), v.Board.Trails...)
	v.Board.Territory = copyTerritory(v.Board.Territory)
	v.Board.TerritoryOwners = copyOwners(v.Board.TerritoryOwners)
	v.Board.Visible = copyPointBools(v.Board.Visible)
	v.Board.Unknown = copyPointBools(v.Board.Unknown)
	v.Board.Obstacles = copyPointBools(v.Board.Obstacles)
	v.Board.Holes = copyPointBools(v.Board.Holes)
	v.Board.Weights = copyPointInts(v.Board.Weights)
	v.Board.Capture.Points = copyTerritory(v.Board.Capture.Points)
	if v.Board.Pending != nil {
		pending := *v.Board.Pending
		v.Board.Pending = &pending
	}
	v.HUD.Scores = append([]ScoreView(nil), v.HUD.Scores...)
	v.HUD.Winners = append([]string(nil), v.HUD.Winners...)
	v.HUD.TeamWinners = append([]string(nil), v.HUD.TeamWinners...)
	if v.Planner.Decision != nil {
		decision := *v.Planner.Decision
		decision.Alternatives = append([]PlannerAlternative(nil), decision.Alternatives...)
		v.Planner.Decision = &decision
	}
	return v
}

func copyErrors(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func copyTerritory(in map[Point]uint32) map[Point]uint32 {
	if in == nil {
		return nil
	}
	out := make(map[Point]uint32, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func copyOwners(in map[Point]string) map[Point]string {
	if in == nil {
		return nil
	}
	out := make(map[Point]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyPointBools(in map[Point]bool) map[Point]bool {
	if in == nil {
		return nil
	}
	out := make(map[Point]bool, len(in))
	for point, value := range in {
		out[point] = value
	}
	return out
}

func copyPointInts(in map[Point]int) map[Point]int {
	if in == nil {
		return nil
	}
	out := make(map[Point]int, len(in))
	for point, value := range in {
		out[point] = value
	}
	return out
}

func (m *Model) Navigate(s Screen) {
	m.mu.Lock()
	m.view.Screen = s
	m.mu.Unlock()
}
func (m *Model) SetHealth(h HealthStatus, err error) {
	m.mu.Lock()
	m.view.Health = h
	m.view.HealthOK = err == nil && h.Status == "ok"
	if err != nil {
		m.view.Error = errorView(err, "retry health")
	} else if m.view.Screen != ScreenError || m.view.Error.Retry == "retry health" {
		m.view.Error = ErrorView{}
	}
	m.mu.Unlock()
}
func (m *Model) SetGames(v []GameSummary, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.view.Screen, m.view.Error = ScreenError, errorView(err, "retry games")
		return
	}
	m.view.Games, m.view.Error = append([]GameSummary(nil), v...), ErrorView{}
}
func (m *Model) SetBrains(v []BrainSummary, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.view.Screen, m.view.Error = ScreenError, errorView(err, "retry brains")
		return
	}
	m.view.Brains, m.view.Error = append([]BrainSummary(nil), v...), ErrorView{}
}
func (m *Model) SetInspector(v InspectorResult, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.view.Inspect.Error = err.Error()
		m.view.Error = errorView(err, "retry inspector")
		return
	}
	m.view.Inspector = v
	m.view.Inspect.Error = ""
	m.view.ActiveBrain = v.BrainID
	m.view.Error = ErrorView{}
	m.view.Screen = ScreenInspector
}
func (m *Model) SetTournaments(v []TournamentSummary, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.view.Screen, m.view.Error = ScreenError, errorView(err, "retry tournament")
		return
	}
	m.view.Tournaments, m.view.Error = append([]TournamentSummary(nil), v...), ErrorView{}
}
func (m *Model) SetMatches(v []MatchSummary, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.view.Screen, m.view.Error = ScreenError, errorView(err, "retry tournament")
		return
	}
	m.view.Matches, m.view.Error = append([]MatchSummary(nil), v...), ErrorView{}
}

func errorView(err error, retry string) ErrorView {
	out := ErrorView{Message: err.Error(), Retry: retry}
	var api *APIError
	if errorsAs(err, &api) {
		out.Code = api.Code
		out.Message = api.Message
	}
	return out
}

// errorsAs is kept tiny so the model stays independent of HTTP implementation details.
func errorsAs(err error, target **APIError) bool {
	for err != nil {
		if value, ok := err.(*APIError); ok {
			*target = value
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (m *Model) SetGame(id string, response GameResponse) {
	m.mu.Lock()
	oldGameID := m.view.GameID
	old := m.view.Board
	m.view.GameID = id
	m.view.GameCursor = response.Game.Cursor
	if m.view.GameCursor == 0 {
		m.view.GameCursor = response.Game.Sequence
	}
	m.view.EventHash = response.Game.EventHash
	board := BoardFromGameResponse(response)
	enrichBoard(&board, response.Game.Participants)
	if oldGameID == id && old.Tick < board.Tick {
		board.Capture = capturedDiff(old, board, response.Events)
	}
	m.view.Board = board
	m.view.HUD.Paused = response.Game.Status == "paused"
	m.view.HUD.Scores, m.view.HUD.Tie = RankedScores(board.Worms)
	m.view.HUD.Winners = nil
	m.view.HUD.Status = authoritativeStatus(response.Game, board)
	m.view.HUD.Energy, m.view.HUD.HasEnergy = 0, false
	m.view.HUD.TeamScore, m.view.HUD.Team = 0, ""
	m.view.HUD.TeamWinners = nil
	if response.Extension != nil {
		observation := response.Extension.Observation
		if observation.Energy != nil {
			m.view.HUD.Energy, m.view.HUD.HasEnergy = *observation.Energy, true
		}
		m.view.HUD.TeamScore = observation.TeamScore
		m.view.HUD.Team = response.Extension.Config.Teams[observation.WormID]
		m.view.HUD.TeamWinners = append([]string(nil), response.Extension.TeamWinners...)
		if board.GameOver && len(response.Extension.Scores) > 0 {
			m.view.HUD.Scores, m.view.HUD.Tie = rankedExtensionScores(response.Extension.Scores, response.Game.Participants, response.Extension.Config.Teams)
			m.view.HUD.Winners = extensionWinnerNames(response.Extension.Winners, response.Game.Participants)
		}
	}
	m.view.Screen = ScreenPlay
	m.view.Error = ErrorView{}
	m.mu.Unlock()
}

func (m *Model) SetGameCommand(game GameSummary, paused bool) {
	m.mu.Lock()
	m.view.GameCursor = game.Cursor
	if m.view.GameCursor == 0 {
		m.view.GameCursor = game.Sequence
	}
	m.view.EventHash = game.EventHash
	m.view.HUD.Paused = paused
	m.view.HUD.Status = authoritativeStatus(game, m.view.Board)
	m.view.Screen = ScreenPlay
	m.view.Error = ErrorView{}
	m.mu.Unlock()
}
func (m *Model) resetGame() {
	m.mu.Lock()
	speed := m.view.HUD.Speed
	m.view.Screen = ScreenSetup
	m.view.GameID = ""
	m.view.GameCursor = 0
	m.view.EventHash = ""
	m.view.Board = BoardView{}
	m.view.HUD = HUDView{Speed: speed}
	m.view.Planner.Decision = nil
	m.view.Planner.Error = ""
	m.view.Error = ErrorView{}
	m.mu.Unlock()
}

func authoritativeStatus(game GameSummary, board BoardView) string {
	if board.GameOver {
		return fmt.Sprintf("finished at tick %d", board.Tick)
	}
	if game.Status == "paused" {
		return fmt.Sprintf("paused at tick %d", board.Tick)
	}
	if board.Pending != nil {
		return fmt.Sprintf("teaching %s · request %d", board.Pending.WormID, board.Pending.Request)
	}
	if board.ActiveWorm == "" {
		return fmt.Sprintf("tick %d · awaiting authoritative turn", board.Tick)
	}
	return fmt.Sprintf("tick %d · active %s", board.Tick, board.ActiveWorm)
}

func enrichBoard(board *BoardView, participants []ParticipantSummary) {
	byID := make(map[string]ParticipantSummary, len(participants))
	for _, p := range participants {
		byID[p.ID] = p
	}
	for i := range board.Worms {
		if p, ok := byID[board.Worms[i].ID]; ok {
			if p.Name != "" {
				board.Worms[i].Name = p.Name
			}
			if p.Kind != "" {
				board.Worms[i].Controller = p.Kind
				board.Worms[i].Asleep = p.Kind == ControllerAsleep
			}
			if board.Worms[i].BrainID == "" {
				board.Worms[i].BrainID = p.BrainVersionID
			}
		}
	}
}

func capturedDiff(old, next BoardView, events []StateEvent) CaptureView {
	points := make(map[Point]uint32, 2)
	for _, event := range events {
		if event.Type == "territory_captured" || event.Type == "territory_recolored" || event.Type == "capture" {
			p := boardPoint(event.Territory, next.Mode)
			if c, ok := next.Territory[p]; ok {
				points[p] = c
			}
		}
	}
	if len(points) == 0 {
		for p, c := range next.Territory {
			if before, existed := old.Territory[p]; !existed || before != c {
				points[p] = c
			}
		}
	}
	if len(points) == 0 {
		return CaptureView{}
	}
	return CaptureView{Points: points, Serial: next.Tick, Until: time.Now().Add(design.CaptureDuration)}
}

func BoardFromState(st GameState) BoardView {
	out := BoardView{Width: st.Width, Height: st.Height, Topology: st.Topology, Mode: st.Mode, Tick: st.Tick, Round: st.Round, Territory: make(map[Point]uint32), TerritoryOwners: make(map[Point]string), GameOver: st.GameOver, Provenance: st.Provenance}
	territoryLineColors := make(map[Point]uint32)
	activeSlot := st.ActiveSlot
	if activeSlot < 0 || activeSlot >= len(st.Worms) || !st.Worms[activeSlot].Alive || st.Worms[activeSlot].Controller == ControllerAsleep {
		activeSlot = -1
		if st.Pending != nil && st.Pending.Slot >= 0 && st.Pending.Slot < len(st.Worms) {
			activeSlot = st.Pending.Slot
		}
	}
	for i, w := range st.Worms {
		p := boardPoint(w.Position, st.Mode)
		active := i == activeSlot
		if active {
			out.ActiveWorm = w.ID
		}
		out.Worms = append(out.Worms, WormView{ID: w.ID, Name: w.ID, Controller: w.Controller, BrainID: w.BrainID, Position: p, Color: colorForID(w.Color, w.ID), Score: w.Score, Alive: w.Alive, Asleep: w.Controller == ControllerAsleep, Active: active})
	}
	for _, t := range st.Territories {
		if t.Color == "" && t.Owner == "" {
			continue
		}
		p := boardPoint(t.ID, st.Mode)
		lineColor := colorForID(t.Color, t.Owner)
		out.Territory[p] = territoryBackgroundColor(lineColor)
		territoryLineColors[p] = lineColor
		out.TerritoryOwners[p] = t.Owner
	}
	for _, t := range st.Trails {
		a := boardPoint(t.Edge.A, st.Mode)
		b := boardPoint(t.Edge.B, st.Mode)
		aColor, ok := territoryLineColors[a]
		if !ok {
			aColor = colorForID(t.Owner, t.Owner)
		}
		bColor, ok := territoryLineColors[b]
		if !ok {
			bColor = colorForID(t.Owner, t.Owner)
		}
		out.Trails = append(out.Trails, Trail{A: a, B: b, AColor: aColor, BColor: bColor, Owner: t.Owner})
	}
	out.Legal = legalDirections(st, activeSlot)
	if st.Pending != nil {
		pending := &DecisionView{WormID: st.Pending.WormID, Mask: st.Pending.Mask, Request: st.Pending.Request, Legal: out.Legal}
		if len(st.Pending.Legal) > 0 {
			pending.Legal = [6]bool{}
			for _, d := range st.Pending.Legal {
				if d >= 0 && d < 6 {
					pending.Legal[d] = true
				}
			}
			out.Legal = pending.Legal
		}
		out.Pending = pending
	}
	return out
}

func BoardFromGameResponse(response GameResponse) BoardView {
	if response.Extension == nil {
		return BoardFromState(response.State)
	}
	extension := response.Extension
	var board BoardView
	if extension.Config.FogOfWar {
		board = boardFromFogObservation(response.Game, *extension)
	} else {
		board = BoardFromState(response.State)
	}
	board.FogOfWar = extension.Config.FogOfWar
	board.UnknownCount = extension.Observation.UnknownCount
	board.Visible = make(map[Point]bool, len(extension.Observation.Visible))
	board.Unknown = make(map[Point]bool, extension.Observation.UnknownCount)
	board.Obstacles = make(map[Point]bool)
	board.Holes = make(map[Point]bool)
	board.Weights = make(map[Point]int)
	for _, cell := range extension.Observation.Visible {
		point := boardPoint(cell.Point, board.Mode)
		if !cell.Visible {
			board.Unknown[point] = true
			continue
		}
		board.Visible[point] = true
		if cell.Obstacle {
			board.Obstacles[point] = true
		}
		if cell.Hole {
			board.Holes[point] = true
		}
		if cell.TerritoryScore > 0 {
			board.Weights[point] = cell.TerritoryScore
		}
	}
	for i := range board.Worms {
		board.Worms[i].Team = extension.Config.Teams[board.Worms[i].ID]
		if score, ok := extension.Scores[board.Worms[i].ID]; ok && board.GameOver {
			board.Worms[i].Score = score
		}
	}

	// Extended legality is authoritative only in the observation. Never retain
	// the base-state inference: it cannot account for variant terrain, trails,
	// energy, or information hidden by fog.
	board.Legal = [6]bool{}
	observation := extension.Observation
	if observation.WormID == board.ActiveWorm && (observation.Base.WormID == "" || observation.Base.WormID == board.ActiveWorm) {
		board.Legal = directionSet(observation.Base.Legal)
	}
	if board.Pending != nil {
		board.Pending.Legal = board.Legal
	}
	return board
}

func boardFromFogObservation(game GameSummary, extension ExtensionResponse) BoardView {
	observation := extension.Observation
	width, height := extension.Config.Width, extension.Config.Height
	if width < 1 {
		width = game.Width
	}
	if height < 1 {
		height = game.Height
	}
	board := BoardView{
		Width: width, Height: height, Tick: uint64(max(game.Tick, 0)),
		Territory: make(map[Point]uint32), TerritoryOwners: make(map[Point]string),
		ActiveWorm: observation.WormID, GameOver: game.Status == "completed",
	}
	if observation.WormID == "" {
		return board
	}
	score := 0
	if len(observation.Base.Scores) > 0 {
		score = observation.Base.Scores[0]
	}
	board.Worms = []WormView{{
		ID: observation.WormID, Name: observation.WormID,
		Position: boardPoint(observation.Base.Position, board.Mode),
		Score:    score, Alive: true, Active: true,
		Team: extension.Config.Teams[observation.WormID],
	}}
	return board
}

func directionSet(directions []int) [6]bool {
	var legal [6]bool
	for _, direction := range directions {
		if direction >= 0 && direction < len(legal) {
			legal[direction] = true
		}
	}
	return legal
}

func boardPoint(p StatePoint, mode int) Point {
	if mode == 1 {
		return Point{X: p.Q - 1, Y: p.R - 1}
	}
	return Point{X: p.Q, Y: p.R}
}

func legalDirections(st GameState, slot int) [6]bool {
	var legal [6]bool
	if slot < 0 || slot >= len(st.Worms) || !st.Worms[slot].Alive {
		return legal
	}
	worm := st.Worms[slot]
	mask := uint8(0)
	for _, territory := range st.Territories {
		if territory.ID == worm.Position {
			mask |= territory.Mask
			break
		}
	}
	for d := range 6 {
		if mask&(1<<d) != 0 {
			continue
		}
		to := stateNeighbor(worm.Position, d, st)
		if st.Topology != 1 && !stateInBounds(to, st) {
			continue
		}
		blocked := false
		for _, other := range st.Worms {
			if other.Alive && other.Position == to {
				blocked = true
				break
			}
		}
		if !blocked {
			legal[d] = true
		}
	}
	return legal
}

func stateNeighbor(p StatePoint, d int, st GameState) StatePoint {
	odd := p.R&1 == 1
	dqEven := [...]int{1, 0, -1, -1, -1, 0}
	dqOdd := [...]int{1, 1, 0, -1, 0, 1}
	dr := [...]int{0, 1, 1, 0, -1, -1}
	dq := dqEven[d]
	if odd {
		dq = dqOdd[d]
	}
	to := StatePoint{Q: p.Q + dq, R: p.R + dr[d]}
	if st.Topology == 1 {
		start := 0
		if st.Mode == 1 {
			start = 1
		}
		to.Q = ((to.Q-start)%st.Width+st.Width)%st.Width + start
		to.R = ((to.R-start)%st.Height+st.Height)%st.Height + start
	}
	return to
}
func stateInBounds(p StatePoint, st GameState) bool {
	start := 0
	if st.Mode == 1 {
		start = 1
	}
	return p.Q >= start && p.Q < start+st.Width && p.R >= start && p.R < start+st.Height
}

func colorForID(value, fallback string) uint32 {
	if value == "" {
		value = fallback
	}
	var h uint32 = 2166136261
	for i := range value {
		h ^= uint32(value[i])
		h *= 16777619
	}
	// Tint generated colors away from black so every owner remains visible on the dark board.
	r := byte(0x58 + (h>>16)&0x7f)
	g := byte(0x58 + (h>>8)&0x7f)
	b := byte(0x58 + h&0x7f)
	return uint32(r)<<24 | uint32(g)<<16 | uint32(b)<<8 | 0xff
}

func territoryBackgroundColor(c uint32) uint32 {
	const backgroundScale = 3
	const colorScale = 5
	r := uint32(byte(c>>24)) * backgroundScale / colorScale
	g := uint32(byte(c>>16)) * backgroundScale / colorScale
	b := uint32(byte(c>>8)) * backgroundScale / colorScale
	return r<<24 | g<<16 | b<<8 | uint32(byte(c))
}

func (m *Model) SetBoard(v BoardView) {
	m.mu.Lock()
	m.view.Board = cloneView(AppView{Board: v}).Board
	m.view.HUD.Scores, m.view.HUD.Tie = RankedScores(m.view.Board.Worms)
	m.view.HUD.Status = authoritativeStatus(GameSummary{}, m.view.Board)
	m.mu.Unlock()
}
func (m *Model) SetError(message string) {
	m.mu.Lock()
	m.view.Screen = ScreenError
	m.view.Error = ErrorView{Message: message, Retry: "retry"}
	m.mu.Unlock()
}
func (m *Model) SetGameError(err error) {
	m.mu.Lock()
	m.view.Screen = ScreenError
	m.view.Error = errorView(err, "retry game")
	m.mu.Unlock()
}
func (m *Model) ToggleGrid() {
	m.mu.Lock()
	m.view.Toggles.Grid = !m.view.Toggles.Grid
	m.mu.Unlock()
}
func (m *Model) ToggleFlash() {
	m.mu.Lock()
	m.view.Toggles.Flash = !m.view.Toggles.Flash
	m.mu.Unlock()
}
func (m *Model) ToggleReducedMotion() {
	m.mu.Lock()
	m.view.Toggles.ReducedMotion = !m.view.Toggles.ReducedMotion
	m.mu.Unlock()
}
func (m *Model) SetPaused(paused bool) {
	m.mu.Lock()
	m.view.HUD.Paused = paused
	m.mu.Unlock()
}
func (m *Model) SetSpeed(speed int) {
	if speed < 1 {
		speed = 1
	}
	if speed > 9 {
		speed = 9
	}
	m.mu.Lock()
	m.view.HUD.Speed = speed
	m.mu.Unlock()
}
func (m *Model) SetSetup(setup SetupConfig) {
	m.mu.Lock()
	m.view.Setup = setup
	m.mu.Unlock()
}
func (m *Model) ValidateSetup() (SetupConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	validated, ok := m.view.Setup.Validate()
	m.view.Setup = validated
	return validated, ok
}
func (m *Model) SetInspectorQuery(query InspectorQuery) {
	if query.Limit < 1 {
		query.Limit = 12
	}
	m.mu.Lock()
	m.view.Inspect = query
	m.mu.Unlock()
}

// Direction is the stable clockwise E,SE,SW,W,NW,NE teaching order.
type Direction uint8

const (
	East Direction = iota
	SouthEast
	SouthWest
	West
	NorthWest
	NorthEast
)

func (d Direction) String() string {
	if int(d) < len(directionNames) {
		return directionNames[d]
	}
	return "?"
}
func DirectionFromKey(name string) (Direction, bool) {
	switch name {
	case "ArrowRight", "→", "d", "D", "e", "E", "6":
		return East, true
	case "ArrowDown", "↓", "x", "X", "3":
		return SouthEast, true
	case "c", "C", "1":
		return SouthWest, true
	case "ArrowLeft", "←", "a", "A", "4":
		return West, true
	case "q", "Q", "7":
		return NorthWest, true
	case "ArrowUp", "↑", "z", "Z", "9":
		return NorthEast, true
	}
	return 0, false
}
func DirectionFromPointer(center, point f32.Point) Direction {
	angle := math.Atan2(float64(point.Y-center.Y), float64(point.X-center.X))
	if angle < 0 {
		angle += 2 * math.Pi
	}
	return Direction(int(math.Round(angle/(math.Pi/3))) % 6)
}
func IsLegalDirection(legal [6]bool, direction Direction) bool {
	return direction <= NorthEast && legal[direction]
}

func (m *Model) Scores() []ScoreView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]ScoreView(nil), m.view.HUD.Scores...)
}
func RankedScores(worms []WormView) ([]ScoreView, bool) {
	ranked := make([]ScoreView, 0, len(worms))
	for _, w := range worms {
		ranked = append(ranked, ScoreView{ID: w.ID, Name: w.Name, Controller: w.Controller, BrainID: w.BrainID, Team: w.Team, Score: w.Score, Color: w.Color, Alive: w.Alive, Asleep: w.Asleep, Active: w.Active})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].ID < ranked[j].ID
	})
	return ranked, len(ranked) > 1 && ranked[0].Score == ranked[1].Score
}

func rankedExtensionScores(scores map[string]int, participants []ParticipantSummary, teams map[string]string) ([]ScoreView, bool) {
	participantsByID := make(map[string]ParticipantSummary, len(participants))
	for _, participant := range participants {
		participantsByID[participant.ID] = participant
	}
	worms := make([]WormView, 0, len(scores))
	for id, score := range scores {
		participant := participantsByID[id]
		name := participant.Name
		if name == "" {
			name = id
		}
		worms = append(worms, WormView{
			ID: id, Name: name, Controller: participant.Kind, BrainID: participant.BrainVersionID,
			Team: teams[id], Score: score, Color: colorForID(participant.Color, id),
		})
	}
	return RankedScores(worms)
}

func extensionWinnerNames(winners []string, participants []ParticipantSummary) []string {
	names := make(map[string]string, len(participants))
	for _, participant := range participants {
		names[participant.ID] = participant.Name
	}
	out := make([]string, 0, len(winners))
	for _, winner := range winners {
		if name := names[winner]; name != "" {
			out = append(out, name)
		} else {
			out = append(out, winner)
		}
	}
	return out
}
func UpdateScores(board *BoardView) ([]ScoreView, bool) { return RankedScores(board.Worms) }

func (m *Model) SetPlannerTeach(teach bool) {
	m.mu.Lock()
	m.view.Planner.Teach = teach
	m.mu.Unlock()
}

func (m *Model) SetPlannerResult(decision PlannerDecision, response *GameResponse, err error) {
	m.mu.Lock()
	if err != nil {
		m.view.Planner.Error = err.Error()
		m.view.Planner.Decision = nil
		m.mu.Unlock()
		return
	}
	decision.Alternatives = append([]PlannerAlternative(nil), decision.Alternatives...)
	m.view.Planner.Decision = &decision
	m.view.Planner.Error = ""
	m.mu.Unlock()
	if response != nil {
		m.SetGame(m.Snapshot().GameID, *response)
	}
}

func (m *Model) SetShare(view ShareExperimentView) {
	m.mu.Lock()
	m.view.Share = view
	m.mu.Unlock()
}

func IsAutonomousController(controller string) bool {
	switch controller {
	case ControllerAuto, ControllerWild, ControllerSame, ControllerNamed:
		return true
	}
	return false
}
func ActiveController(board BoardView) string {
	for _, worm := range board.Worms {
		if worm.Active {
			return worm.Controller
		}
	}
	return ""
}

func needsAuthoritativeTick(board BoardView) bool {
	if board.Pending != nil || board.GameOver {
		return false
	}
	if board.ActiveWorm == "" {
		return true
	}
	controller := ActiveController(board)
	return controller == ControllerNew || IsAutonomousController(controller)
}
