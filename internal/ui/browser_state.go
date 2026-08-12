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
	Ready          bool                  `json:"ready"`
	Screen         string                `json:"screen"`
	GameID         string                `json:"gameID"`
	Cursor         int64                 `json:"cursor"`
	EventHash      string                `json:"eventHash"`
	Paused         bool                  `json:"paused"`
	ActionInFlight bool                  `json:"actionInFlight"`
	PauseRequested bool                  `json:"pauseRequested"`
	Tick           uint64                `json:"tick"`
	Pending        *DecisionView         `json:"pending"`
	Capture        []browserCaptureFace  `json:"capture"`
	Inspector      browserInspectorState `json:"inspector"`
	Error          string                `json:"error"`
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
	return browserTestState{
		Ready:          true,
		Screen:         view.Screen.String(),
		GameID:         view.GameID,
		Cursor:         view.GameCursor,
		EventHash:      view.EventHash,
		Paused:         view.HUD.Paused,
		ActionInFlight: s.actionInFlight.Load(),
		PauseRequested: s.pauseRequested.Load(),
		Tick:           view.Board.Tick,
		Pending:        view.Board.Pending,
		Capture:        faces,
		Inspector: browserInspectorState{
			BrainID: view.Inspector.BrainID, VersionID: view.Inspector.VersionID, Version: view.Inspector.Version,
			Offset: view.Inspector.Offset, Limit: view.Inspector.Limit,
			Total: view.Inspector.Total, NextOffset: view.Inspector.NextOffset,
			RuleCount: len(view.Inspector.Rules), Filter: view.Inspect.Filter,
		},
		Error: view.Error.Message,
	}
}
