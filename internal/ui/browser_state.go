package ui

import "sort"

type browserCaptureFace struct {
	X     int    `json:"x"`
	Y     int    `json:"y"`
	Color uint32 `json:"color"`
}

type browserInspectorState struct {
	BrainID    string `json:"brainID"`
	VersionID  string `json:"versionID"`
	Version    int    `json:"version"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
	Total      int    `json:"total"`
	NextOffset int    `json:"nextOffset"`
	RuleCount  int    `json:"ruleCount"`
	Filter     string `json:"filter"`
}

type browserTestState struct {
	Ready               bool                  `json:"ready"`
	Screen              string                `json:"screen"`
	SetupRuleset        string                `json:"setupRuleset"`
	GameID              string                `json:"gameID"`
	Cursor              int64                 `json:"cursor"`
	EventHash           string                `json:"eventHash"`
	Paused              bool                  `json:"paused"`
	ActionInFlight      bool                  `json:"actionInFlight"`
	PauseRequested      bool                  `json:"pauseRequested"`
	Tick                uint64                `json:"tick"`
	Pending             *DecisionView         `json:"pending"`
	Capture             []browserCaptureFace  `json:"capture"`
	VisibleCells        int                   `json:"visibleCells"`
	UnknownCells        int                   `json:"unknownCells"`
	Obstacles           int                   `json:"obstacles"`
	Holes               int                   `json:"holes"`
	Weights             int                   `json:"weights"`
	Energy              *int                  `json:"energy,omitempty"`
	Team                string                `json:"team,omitempty"`
	TeamScore           int                   `json:"teamScore,omitempty"`
	Winners             []string              `json:"winners,omitempty"`
	TeamWinners         []string              `json:"teamWinners,omitempty"`
	PlannerAlternatives int                   `json:"plannerAlternatives"`
	Inspector           browserInspectorState `json:"inspector"`
	Error               string                `json:"error"`
}

func (s *Shell) browserState() browserTestState {
	view := s.Model.Snapshot()
	faces := make([]browserCaptureFace, 0, len(view.Board.Capture.Points))
	for point, capturedColor := range view.Board.Capture.Points {
		faces = append(faces, browserCaptureFace{X: point.X, Y: point.Y, Color: capturedColor})
	}
	sort.Slice(faces, func(i, j int) bool {
		return faces[i].Y < faces[j].Y || (faces[i].Y == faces[j].Y && faces[i].X < faces[j].X)
	})
	var energy *int
	if view.HUD.HasEnergy {
		value := view.HUD.Energy
		energy = &value
	}
	alternatives := 0
	if view.Planner.Decision != nil {
		alternatives = len(view.Planner.Decision.Alternatives)
	}
	return browserTestState{
		Ready:               true,
		Screen:              view.Screen.String(),
		SetupRuleset:        view.Setup.Ruleset,
		GameID:              view.GameID,
		Cursor:              view.GameCursor,
		EventHash:           view.EventHash,
		Paused:              view.HUD.Paused,
		ActionInFlight:      s.actionInFlight.Load(),
		PauseRequested:      s.pauseRequested.Load(),
		Tick:                view.Board.Tick,
		Pending:             view.Board.Pending,
		Capture:             faces,
		VisibleCells:        len(view.Board.Visible),
		UnknownCells:        len(view.Board.Unknown),
		Obstacles:           len(view.Board.Obstacles),
		Holes:               len(view.Board.Holes),
		Weights:             len(view.Board.Weights),
		Energy:              energy,
		Team:                view.HUD.Team,
		TeamScore:           view.HUD.TeamScore,
		Winners:             append([]string(nil), view.HUD.Winners...),
		TeamWinners:         append([]string(nil), view.HUD.TeamWinners...),
		PlannerAlternatives: alternatives,
		Inspector: browserInspectorState{
			BrainID: view.Inspector.BrainID, VersionID: view.Inspector.VersionID, Version: view.Inspector.Version,
			Offset: view.Inspector.Offset, Limit: view.Inspector.Limit,
			Total: view.Inspector.Total, NextOffset: view.Inspector.NextOffset,
			RuleCount: len(view.Inspector.Rules), Filter: view.Inspect.Filter,
		},
		Error: view.Error.Message,
	}
}
