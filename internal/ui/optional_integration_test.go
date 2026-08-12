package ui

import (
	"encoding/json"
	"image"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestExtensionPresetDTOsRemainOptionalForClassicAndModern(t *testing.T) {
	for _, ruleset := range []string{"classic", "modern"} {
		setup := defaultSetup()
		setup.Ruleset = ruleset
		resolved, ok := setup.Validate()
		if !ok {
			t.Fatalf("%s setup rejected: %v", ruleset, resolved.Errors)
		}
		request := CreateGameRequest{Ruleset: ruleset, ExtensionConfig: resolved.ExtensionPreset()}
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "extension_config") {
			t.Fatalf("%s request changed classic wire path: %s", ruleset, raw)
		}
	}

	setup := defaultSetup()
	setup.Ruleset = "variants-terrain"
	resolved, ok := setup.Validate()
	if !ok {
		t.Fatalf("terrain setup rejected: %v", resolved.Errors)
	}
	config := resolved.ExtensionPreset()
	if config == nil || !config.Enabled || config.Version != 1 || config.Seed != 1 || config.ObstacleRate == 0 || config.HoleRate == 0 || len(config.WeightedTerritories) != 1 {
		t.Fatalf("terrain preset lost extension fields: %+v", config)
	}
	raw, err := json.Marshal(CreateGameRequest{ExtensionConfig: config})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire["extension_config"]) == 0 {
		t.Fatalf("variant request omitted extension_config: %s", raw)
	}
}

func TestExtensionResponseProjectionRendersOnlyObservedFogContent(t *testing.T) {
	energy := 7
	response := GameResponse{
		Game: GameSummary{
			ID: "fog-game", Status: "active", Width: 3, Height: 1, Tick: 9,
			Participants: []ParticipantSummary{
				{ID: "visible", Name: "Visible Worm", Kind: ControllerNew},
				{ID: "hidden", Name: "Hidden Worm", Kind: ControllerWild},
			},
		},
		// A fog response now omits state. Poisoning this value proves the UI does
		// not depend on an authoritative snapshot if an older server sends one.
		State: GameState{
			Width: 99, Height: 99, ActiveSlot: 1,
			Worms:       []StateWorm{{ID: "hidden", Position: StatePoint{Q: 2, R: 0}, Alive: true}},
			Territories: []StateTerritory{{ID: StatePoint{Q: 2, R: 0}, Owner: "hidden"}},
		},
		Extension: &ExtensionResponse{
			Config: ExtensionConfig{Enabled: true, FogOfWar: true, Width: 3, Height: 1, Teams: map[string]string{"visible": "north", "hidden": "south"}},
			Observation: ExtensionObservation{
				WormID: "visible", Energy: &energy, TeamScore: 11, UnknownCount: 2,
				Base: ExtensionBaseObservation{
					WormID: "visible", Position: StatePoint{Q: 0, R: 0},
					Legal: []int{1, 4}, Scores: []int{5},
				},
				Visible: []ExtensionVisibleCell{
					{Point: StatePoint{Q: 0, R: 0}, Visible: true, Obstacle: true, TerritoryScore: 3},
					{Point: StatePoint{Q: 1, R: 0}, Visible: false},
					{Point: StatePoint{Q: 2, R: 0}, Visible: false},
				},
			},
		},
	}

	board := BoardFromGameResponse(response)
	if board.Width != 3 || board.Height != 1 || board.Tick != 9 {
		t.Fatalf("fog-safe dimensions/tick = %dx%d/%d", board.Width, board.Height, board.Tick)
	}
	if len(board.Worms) != 1 || board.Worms[0].ID != "visible" || board.Worms[0].Team != "north" {
		t.Fatalf("authoritative hidden state leaked through fog: %+v", board.Worms)
	}
	if len(board.Territory) != 0 || len(board.Trails) != 0 {
		t.Fatalf("authoritative territory/trail rendered through fog: territory=%v trails=%v", board.Territory, board.Trails)
	}
	if !board.Legal[1] || !board.Legal[4] || board.Legal[0] {
		t.Fatalf("extension legal moves were not authoritative: %v", board.Legal)
	}
	if !board.Obstacles[Point{X: 0, Y: 0}] || board.Weights[Point{X: 0, Y: 0}] != 3 {
		t.Fatalf("visible obstacle/weight missing: obstacles=%v weights=%v", board.Obstacles, board.Weights)
	}
	if len(board.Holes) != 0 || len(board.Unknown) != 2 || board.Visible[Point{X: 2, Y: 0}] {
		t.Fatalf("hidden feature rendered instead of unknown cue: holes=%v unknown=%v visible=%v", board.Holes, board.Unknown, board.Visible)
	}

	model := NewModel()
	model.SetGame("fog-game", response)
	view := model.Snapshot()
	if !view.HUD.HasEnergy || view.HUD.Energy != 7 || view.HUD.Team != "north" || view.HUD.TeamScore != 11 {
		t.Fatalf("extension HUD lost observed values: %+v", view.HUD)
	}
}

func TestExtendedLegalMovesNeverFallBackToBaseInference(t *testing.T) {
	state := GameState{
		Width: 4, Height: 4, ActiveSlot: 0,
		Worms: []StateWorm{{ID: "active", Position: StatePoint{Q: 1, R: 1}, Alive: true}},
	}
	response := GameResponse{
		State: state,
		Extension: &ExtensionResponse{
			Config: ExtensionConfig{Enabled: true},
			Observation: ExtensionObservation{
				WormID: "active",
				Base:   ExtensionBaseObservation{WormID: "active", Legal: []int{2}},
			},
		},
	}
	board := BoardFromGameResponse(response)
	if !board.Legal[2] {
		t.Fatalf("extension-provided legal move disabled: %v", board.Legal)
	}
	for direction, legal := range board.Legal {
		if direction != 2 && legal {
			t.Fatalf("base inference enabled direction %d: %v", direction, board.Legal)
		}
	}

	response.Extension.Observation.WormID = "other"
	response.Extension.Observation.Base.WormID = "other"
	board = BoardFromGameResponse(response)
	if board.Legal != ([6]bool{}) {
		t.Fatalf("legality from another worm was consumed: %v", board.Legal)
	}
}

func TestExtensionCompletionScoresAndWinnersDriveHUD(t *testing.T) {
	response := GameResponse{
		Game: GameSummary{
			ID: "finished", Status: "completed", Tick: 21,
			Participants: []ParticipantSummary{
				{ID: "alpha", Name: "Alpha", Color: "amber"},
				{ID: "beta", Name: "Beta", Color: "cobalt"},
			},
		},
		Extension: &ExtensionResponse{
			Config:      ExtensionConfig{Enabled: true, FogOfWar: true, Width: 5, Height: 5},
			Observation: ExtensionObservation{WormID: "alpha", Base: ExtensionBaseObservation{WormID: "alpha"}},
			Scores:      map[string]int{"alpha": 13, "beta": 8}, Winners: []string{"alpha"},
			TeamWinners: []string{"north"},
		},
	}
	model := NewModel()
	model.SetGame("finished", response)
	view := model.Snapshot()
	if !view.Board.GameOver || len(view.HUD.Scores) != 2 || view.HUD.Scores[0].ID != "alpha" || view.HUD.Scores[0].Score != 13 {
		t.Fatalf("extension completion scores not displayed: board=%+v scores=%+v", view.Board, view.HUD.Scores)
	}
	if len(view.HUD.Winners) != 1 || view.HUD.Winners[0] != "Alpha" || len(view.HUD.TeamWinners) != 1 {
		t.Fatalf("extension completion winners not displayed: %+v", view.HUD)
	}
}

func TestVariantPresetAndPlannerControlsAreKeyboardReachable(t *testing.T) {
	preset := "classic"
	for _, want := range []string{"modern", "variants-terrain", "variants-trails", "variants-teams-fog", "classic"} {
		preset = nextRuleset(preset)
		if preset != want {
			t.Fatalf("next preset = %q, want %q", preset, want)
		}
	}

	shell := NewShell("")
	shell.Model.Navigate(ScreenPlay)
	withoutPending := shell.focusOrder()
	if containsFocus(withoutPending, focusPlan) || containsFocus(withoutPending, focusPlanTeach) {
		t.Fatalf("planner controls exposed without a pending decision: %v", withoutPending)
	}
	board := BoardView{Pending: &DecisionView{WormID: "worm-1", Request: 4}}
	shell.Model.SetBoard(board)
	withPending := shell.focusOrder()
	if !containsFocus(withPending, focusPlan) || !containsFocus(withPending, focusPlanTeach) {
		t.Fatalf("pending planner controls are not keyboard reachable: %v", withPending)
	}
	shell.Model.Navigate(ScreenExperiments)
	experiment := shell.focusOrder()
	for _, focus := range []string{focusSharePolicy, focusShareRecipient, focusShareSources, focusShareSeed, focusShareNoise, focusShareRun} {
		if !containsFocus(experiment, focus) {
			t.Fatalf("experiment control %q missing from focus order: %v", focus, experiment)
		}
	}
}

func TestVariantLayoutAndLabelsDoNotDependOnColorAlone(t *testing.T) {
	geometry := NewBoardGeometry(3, 1, image.Pt(360, 160))
	for _, point := range []Point{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 2, Y: 0}} {
		if !geometry.ContainsScreen(geometry.DotAt(point)) {
			t.Fatalf("variant cue at %v is outside board layout", point)
		}
	}
	if design.Obstacle == design.Hole || design.Hole == design.Unknown || design.Unknown == design.Weight {
		t.Fatal("variant feature tokens are not visually distinct")
	}

	hud := variantHUDLabel(AppView{HUD: HUDView{HasEnergy: true, Energy: 8, Team: "north", TeamScore: 13}, Board: BoardView{UnknownCount: 5}})
	for _, cue := range []string{"ENERGY 8", "TEAM north", "score 13", "FOG 5 unknown cells"} {
		if !strings.Contains(hud, cue) {
			t.Fatalf("HUD label %q lacks noncolor cue %q", hud, cue)
		}
	}
	score := scoreHUDLabel(ScoreView{Name: "Amber", Controller: ControllerNew, Team: "north", Alive: true, Active: true})
	for _, cue := range []string{"WORM", "Amber", "alive ACTIVE", "TEAM north"} {
		if !strings.Contains(score, cue) {
			t.Fatalf("score label %q lacks noncolor cue %q", score, cue)
		}
	}
	alternative := plannerAlternativeLabel(PlannerAlternative{Action: 1, Total: 12, Capture: 1, Border: 2, Survival: 3, Chosen: true, Reason: "best legal score"})
	for _, cue := range []string{"CHOSEN", "SE", "total 12", "best legal score"} {
		if !strings.Contains(alternative, cue) {
			t.Fatalf("planner label %q lacks cue %q", alternative, cue)
		}
	}
}

func TestOptionalRequestDTOsUseServerContractFieldNames(t *testing.T) {
	plan, err := json.Marshal(PlanRequest{PlannerConfig: PlannerConfig{Version: 1}, Teach: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan), `"planner_config"`) || strings.Contains(string(plan), `"planner":`) {
		t.Fatalf("planner request fields = %s", plan)
	}
	share, err := json.Marshal(ShareExperimentRequest{SharingConfig: SharingConfig{Policy: "all_worms"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(share), `"sharing_config"`) || strings.Contains(string(share), `"config":`) {
		t.Fatalf("sharing request fields = %s", share)
	}
}

func TestOptionalResponseDTOsDecodePlannerAndSharingResults(t *testing.T) {
	var plan struct {
		Decision PlannerDecision `json:"decision"`
	}
	if err := json.Unmarshal([]byte(`{"decision":{"worm_id":"w","mask":3,"action":1,"alternatives":[{"action":1,"total":9,"chosen":true,"reason":"best"}],"provenance":{"version":"planner-v1","worm_id":"w","mask":3}}}`), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Decision.Alternatives) != 1 || !plan.Decision.Alternatives[0].Chosen || plan.Decision.Alternatives[0].Action != 1 {
		t.Fatalf("planner alternatives failed to decode: %+v", plan.Decision)
	}
	var share ShareExperimentResponse
	if err := json.Unmarshal([]byte(`{"policy":"all_worms","seed":4,"brain_versions":[{"id":"v-derived","brain_id":"brain","version":4,"hash":"version-hash"}],"metrics":{"derived":2,"versions":1,"changes":3,"additions":4,"removals":1},"hash":"abc"}`), &share); err != nil {
		t.Fatal(err)
	}
	if len(share.BrainVersions) != 1 || share.BrainVersions[0].ID != "v-derived" || share.Metrics.Changes != 3 || share.Hash != "abc" {
		t.Fatalf("sharing result failed to decode: %+v", share)
	}
}

func TestPlannerAndSharingScreensConsumeSupportedHTTPResults(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.Path {
		case "/api/v1/games/game-1/plan":
			body = `{"version":"v1","decision":{"worm_id":"worm-1","mask":3,"action":1,"alternatives":[{"action":1,"total":9,"chosen":true,"reason":"best"}],"provenance":{"version":"planner-v1","worm_id":"worm-1","mask":3}},"game":{"id":"game-1","status":"active","cursor":2}}`
		case "/api/v1/experiments/share":
			body = `{"version":"v1","policy":"all_worms","seed":4,"brain_versions":[{"id":"v-derived","brain_id":"brain","version":4}],"metrics":{"derived":2,"versions":1,"changes":3,"additions":4,"removals":1},"hash":"abc"}`
		default:
			t.Fatalf("unexpected optional endpoint %s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})

	shell := NewShell("http://example.test")
	shell.Client.WithHTTPClient(&http.Client{Transport: transport})
	shell.Model.SetGame("game-1", GameResponse{
		Game: GameSummary{ID: "game-1", Status: "active", Cursor: 2},
		State: GameState{
			Width: 1, Height: 1, ActiveSlot: 0,
			Worms:   []StateWorm{{ID: "worm-1", Position: StatePoint{}, Alive: true, Controller: ControllerNew}},
			Pending: &StateDecision{WormID: "worm-1", Slot: 0, Mask: 3, Request: 7, Legal: []int{1}},
		},
	})
	shell.planPending()
	if decision := shell.Model.Snapshot().Planner.Decision; decision == nil || len(decision.Alternatives) != 1 || !decision.Alternatives[0].Chosen {
		t.Fatalf("planner screen did not retain alternatives: %+v", decision)
	}

	view := shell.Model.Snapshot()
	view.Share.RecipientVersionID = "recipient-version"
	view.Share.SourceVersionIDs = "donor-version"
	view.Share.Seed = "4"
	view.Share.NoiseRate = "0"
	shell.Model.SetShare(view.Share)
	shell.runShareExperiment()
	result := shell.Model.Snapshot().Share.Result
	if result == nil || len(result.BrainVersions) != 1 || result.BrainVersions[0].ID != "v-derived" || result.Metrics.Changes != 3 {
		t.Fatalf("sharing screen did not retain API result: %+v", result)
	}
}

func containsFocus(order []string, target string) bool {
	for _, focus := range order {
		if focus == target {
			return true
		}
	}
	return false
}
