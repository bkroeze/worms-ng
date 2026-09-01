package ui

import (
	"context"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gioui.org/f32"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestSetupValidatesOneToFourStableSlotsWithoutDroppingSelections(t *testing.T) {
	for count := 1; count <= 4; count++ {
		setup := defaultSetup()
		setup.SlotCount = count
		got, ok := setup.Validate()
		if !ok {
			t.Fatalf("%d slots rejected: %v", count, got.Errors)
		}
		if got.ResolvedSeed != 1 || got.ResolvedWidth != 18 || got.ResolvedHeight != 18 || got.ResolvedRuleset != "classic" {
			t.Fatalf("%d slots lost resolved setup: %+v", count, got)
		}
	}

	setup := defaultSetup()
	setup.Slots[0].Name = "retained name"
	setup.Slots[0].Controller = ControllerNamed
	setup.Seed = "not-a-number"
	got, ok := setup.Validate()
	if ok || got.Errors["seed"] == "" || got.Errors["slot.0.brain"] == "" {
		t.Fatalf("field errors not specific: %+v", got.Errors)
	}
	if got.Slots[0].Name != "retained name" || got.Slots[1].Controller != ControllerWild {
		t.Fatalf("validation discarded other selections: %+v", got.Slots)
	}
}

func TestStartGameSendsEveryConfiguredFieldAndUsesCommittedResponse(t *testing.T) {
	var created CreateGameRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/games":
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Error(err)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "v1", "game": map[string]any{"id": created.ID, "status": "active", "seed": created.Seed, "cursor": 0, "participants": created.Participants}})
		case r.Method == http.MethodGet:
			worms := make([]map[string]any, 0, len(created.Participants))
			for _, participant := range created.Participants {
				worms = append(worms, map[string]any{"id": participant.ID, "position": map[string]int{"q": participant.Start.Q, "r": participant.Start.R}, "alive": participant.Kind != ControllerAsleep, "controller": participant.Kind, "brain_id": participant.BrainVersionID})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "v1", "game": map[string]any{"id": created.ID, "status": "active", "seed": created.Seed, "cursor": 0, "participants": created.Participants}, "state": map[string]any{"width": created.Width, "height": created.Height, "mode": 1, "topology": 1, "active_slot": 0, "worms": worms}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	shell := NewShell(server.URL)
	setup := defaultSetup()
	setup.SlotCount, setup.Width, setup.Height, setup.Seed = 4, 20, 16, "773"
	setup.Ruleset = "classic"
	setup.Slots[0].Controller = ControllerNamed
	setup.Slots[0].BrainID = "brain-version-stable"
	setup.Slots[0].Start = Point{X: 1, Y: 1}
	setup.Slots[1].Start = Point{X: 20, Y: 16}
	setup.Slots[2].Start = Point{X: 20, Y: 1}
	setup.Slots[3].Start = Point{X: 1, Y: 16}
	shell.Model.SetSetup(setup)
	shell.startGame()

	if created.Version != "v1" || created.Seed != 773 || created.Width != 20 || created.Height != 16 || created.Ruleset != "classic" || len(created.Participants) != 4 {
		t.Fatalf("create request lost setup fields: %+v", created)
	}
	if created.Participants[0].Kind != ControllerNamed || created.Participants[0].BrainVersionID != "brain-version-stable" || created.Participants[3].Kind != ControllerAsleep {
		t.Fatalf("controller/brain/asleep fields = %+v", created.Participants)
	}
	view := shell.Model.Snapshot()
	if view.Screen != ScreenPlay || view.GameID == "" || view.Board.Width != 20 || view.Board.Worms[0].BrainID != "brain-version-stable" {
		t.Fatalf("committed response was not authoritative: %+v", view)
	}
}

func TestBoardProjectionCarriesAuthoritativeControllerBrainPendingAndClassicCoordinates(t *testing.T) {
	state := GameState{
		Width: 18, Height: 18, Topology: 1, Mode: 1, Tick: 9, ActiveSlot: 0,
		Worms:       []StateWorm{{ID: "alpha", Position: StatePoint{Q: 18, R: 18}, Alive: true, Controller: ControllerNamed, BrainID: "brain-stable", Score: 7}},
		Territories: []StateTerritory{{ID: StatePoint{Q: 18, R: 18}, Owner: "alpha", Color: "amber"}},
		Pending:     &StateDecision{WormID: "alpha", Slot: 0, Mask: 5, Request: 44, Legal: []int{1, 4}},
	}
	board := BoardFromState(state)
	if board.Worms[0].Position != (Point{X: 17, Y: 17}) || board.Worms[0].Controller != ControllerNamed || board.Worms[0].BrainID != "brain-stable" {
		t.Fatalf("authoritative worm projection lost fields: %+v", board.Worms[0])
	}
	if board.Pending == nil || board.Pending.Request != 44 || !board.Legal[1] || !board.Legal[4] || board.Legal[0] {
		t.Fatalf("pending/legal projection = %+v / %v", board.Pending, board.Legal)
	}
	if _, ok := board.Territory[Point{X: 17, Y: 17}]; !ok {
		t.Fatalf("classic endpoint was clipped: %+v", board.Territory)
	}
}

func TestCaptureDiffReportsZeroOneOrTwoChangedFacesExactly(t *testing.T) {
	old := BoardView{Tick: 1, Territory: map[Point]uint32{{0, 0}: 1, {1, 0}: 2}}
	unchanged := BoardView{Tick: 2, Territory: map[Point]uint32{{0, 0}: 1, {1, 0}: 2}}
	if got := capturedDiff(old, unchanged, nil); len(got.Points) != 0 {
		t.Fatalf("zero capture flashed: %+v", got)
	}
	one := BoardView{Tick: 2, Territory: map[Point]uint32{{0, 0}: 3, {1, 0}: 2}}
	if got := capturedDiff(old, one, nil); len(got.Points) != 1 || got.Points[Point{0, 0}] != 3 {
		t.Fatalf("one capture = %+v", got)
	}
	two := BoardView{Tick: 2, Territory: map[Point]uint32{{0, 0}: 3, {1, 0}: 2, {2, 0}: 4}}
	if got := capturedDiff(old, two, nil); len(got.Points) != 2 || got.Points[Point{2, 0}] != 4 {
		t.Fatalf("two captures = %+v", got)
	}
	events := []StateEvent{{Type: "territory_captured", Territory: StatePoint{Q: 1, R: 0}}}
	if got := capturedDiff(old, two, events); len(got.Points) != 1 || got.Points[Point{1, 0}] != 2 {
		t.Fatalf("event identity did not override broad diff: %+v", got)
	}
}

func TestSchedulerChangesPresentationDelayAndNeverCatchesUp(t *testing.T) {
	if presentationDelay(1) <= presentationDelay(9) {
		t.Fatalf("speed did not alter presentation delay: slow=%v fast=%v", presentationDelay(1), presentationDelay(9))
	}
	var scheduler TickScheduler
	start := time.Unix(100, 0)
	ready, due := scheduler.Due(start, 5, false, true, "g:1:h")
	if ready || due.IsZero() {
		t.Fatalf("first observation was not armed: ready=%v due=%v", ready, due)
	}
	ready, next := scheduler.Due(due.Add(10*time.Minute), 5, false, true, "g:1:h")
	if !ready || !next.After(due.Add(10*time.Minute)) {
		t.Fatalf("large gap did not issue exactly one/re-arm from now: ready=%v next=%v", ready, next)
	}
	ready, _ = scheduler.Due(due.Add(10*time.Minute), 5, false, true, "g:1:h")
	if ready {
		t.Fatal("same frame caught up a second tick")
	}
	ready, _ = scheduler.Due(next.Add(time.Second), 5, true, true, "g:1:h")
	if ready {
		t.Fatal("paused scheduler dispatched")
	}
	ready, pausedDue := scheduler.Due(next.Add(2*time.Second), 5, false, true, "g:1:h")
	if ready || pausedDue.IsZero() {
		t.Fatal("resume did not re-arm exactly once")
	}
}

func TestNeedsAuthoritativeTickBootstrapsBeforeAnyWormIsActive(t *testing.T) {
	if !needsAuthoritativeTick(BoardView{}) {
		t.Fatal("initial authoritative turn was not scheduled")
	}
	if !needsAuthoritativeTick(BoardView{
		ActiveWorm: "wild",
		Worms:      []WormView{{ID: "wild", Controller: ControllerWild, Active: true}},
	}) {
		t.Fatal("wild controller was not scheduled")
	}
	if needsAuthoritativeTick(BoardView{Pending: &DecisionView{WormID: "new"}}) {
		t.Fatal("pending teaching decision scheduled another tick")
	}
	if needsAuthoritativeTick(BoardView{GameOver: true}) {
		t.Fatal("finished board scheduled another tick")
	}
}

func TestBlockedDirectionBeforeFirstTurnDoesNotRaiseError(t *testing.T) {
	shell := NewShell("http://browser.test")
	shell.Model.SetGame("g", GameResponse{
		Game:  GameSummary{ID: "g", Status: "active", EventHash: "initial"},
		State: GameState{Width: 18, Height: 18, ActiveSlot: -1},
	})

	shell.submitDirection(East)

	view := shell.Model.Snapshot()
	if view.Screen != ScreenPlay || view.Error.Message != "" {
		t.Fatalf("blocked direction changed screens or raised an error: %+v", view.Error)
	}
}

func TestPauseQueuedDuringAutonomousRequestCommitsImmediatelyAfterIt(t *testing.T) {
	tickStarted := make(chan struct{})
	releaseTick := make(chan struct{})
	pauseFinished := make(chan struct{})
	pauseCalls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/api/v1/games/g/tick":
			close(tickStarted)
			<-releaseTick
			body = `{"version":"v1","game":{"id":"g","status":"active","cursor":1,"event_hash":"after-tick"},"state":{"width":18,"height":18,"active_slot":0}}`
		case "/api/v1/games/g/pause":
			pauseCalls++
			var command GameCommandRequest
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
				t.Error(err)
			}
			if command.Cursor != 1 || command.EventHash != "after-tick" {
				t.Errorf("pause did not use settled authoritative head: %+v", command)
			}
			body = `{"version":"v1","game":{"id":"g","status":"paused","cursor":2,"event_hash":"after-pause"}}`
			close(pauseFinished)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"not found"}`)), Request: r}, nil
		}
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})

	shell := NewShell("http://browser.test")
	shell.Client.WithHTTPClient(&http.Client{Transport: transport})
	shell.Model.SetGame("g", GameResponse{
		Game:  GameSummary{ID: "g", Status: "active", EventHash: "initial"},
		State: GameState{Width: 18, Height: 18, ActiveSlot: 0},
	})
	go shell.submitAutonomous()
	<-tickStarted
	shell.requestPause(true)
	if !shell.pauseRequested.Load() {
		t.Fatal("pause intent was discarded while tick was in flight")
	}
	close(releaseTick)
	select {
	case <-pauseFinished:
	case <-time.After(2 * time.Second):
		t.Fatal("queued pause was not committed after tick")
	}
	deadline := time.Now().Add(time.Second)
	for shell.actionInFlight.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	view := shell.Model.Snapshot()
	if pauseCalls != 1 || shell.actionInFlight.Load() || shell.pauseRequested.Load() || !view.HUD.Paused || view.GameCursor != 2 {
		t.Fatalf("queued pause completion calls=%d inFlight=%v requested=%v view=%+v", pauseCalls, shell.actionInFlight.Load(), shell.pauseRequested.Load(), view)
	}
}

func TestAbortQueuedDuringAutonomousRequestUsesSettledHeadAndResetsGame(t *testing.T) {
	tickStarted := make(chan struct{})
	releaseTick := make(chan struct{})
	abortFinished := make(chan struct{})
	abortCalls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/api/v1/games/g/tick":
			close(tickStarted)
			<-releaseTick
			body = `{"version":"v1","game":{"id":"g","status":"active","cursor":1,"event_hash":"after-tick"},"state":{"width":18,"height":18,"active_slot":0}}`
		case "/api/v1/games/g/abort":
			abortCalls++
			var command GameCommandRequest
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
				t.Error(err)
			}
			if command.Cursor != 1 || command.EventHash != "after-tick" {
				t.Errorf("abort did not use settled authoritative head: %+v", command)
			}
			body = `{"version":"v1","game":{"id":"g","status":"cancelled","cursor":1,"event_hash":"after-tick"},"state":{"width":18,"height":18,"active_slot":0}}`
			close(abortFinished)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"not found"}`)), Request: r}, nil
		}
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})

	shell := NewShell("http://browser.test")
	shell.Client.WithHTTPClient(&http.Client{Transport: transport})
	shell.Model.SetGame("g", GameResponse{
		Game:  GameSummary{ID: "g", Status: "active", EventHash: "initial"},
		State: GameState{Width: 18, Height: 18, ActiveSlot: -1},
	})
	go shell.submitAutonomous()
	<-tickStarted
	shell.requestAbort()
	if !shell.abortRequested.Load() {
		t.Fatal("abort intent was discarded while tick was in flight")
	}
	close(releaseTick)
	select {
	case <-abortFinished:
	case <-time.After(2 * time.Second):
		t.Fatal("queued abort was not committed after tick")
	}
	deadline := time.Now().Add(time.Second)
	for shell.actionInFlight.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	view := shell.Model.Snapshot()
	if abortCalls != 1 || shell.actionInFlight.Load() || shell.abortRequested.Load() {
		t.Fatalf("queued abort completion calls=%d inFlight=%v requested=%v", abortCalls, shell.actionInFlight.Load(), shell.abortRequested.Load())
	}
	if view.Screen != ScreenSetup || view.GameID != "" || view.Board.Width != 0 {
		t.Fatalf("aborted game was not reset to setup: %+v", view)
	}
}

func TestSixDirectionsHaveKeyboardAndPointerPaths(t *testing.T) {
	keys := []string{"D", "X", "C", "A", "Q", "Z"}
	points := []f32.Point{{X: 1, Y: 0}, {X: .5, Y: .866}, {X: -.5, Y: .866}, {X: -1, Y: 0}, {X: -.5, Y: -.866}, {X: .5, Y: -.866}}
	for direction := range 6 {
		got, ok := DirectionFromKey(keys[direction])
		if !ok || int(got) != direction {
			t.Fatalf("key %q = %v,%v, want %d", keys[direction], got, ok, direction)
		}
		if got := DirectionFromPointer(f32.Pt(0, 0), points[direction]); int(got) != direction {
			t.Fatalf("pointer direction %d = %v", direction, got)
		}
	}
}

func TestRepeatedPhysicalKeyPressChangesToggleOnce(t *testing.T) {
	shell := NewShell("")
	shell.Model.Navigate(ScreenPlay)
	press := key.Event{Name: "G", State: key.Press}
	shell.key(press)
	shell.key(press)
	if shell.Model.Snapshot().Toggles.Grid {
		t.Fatal("repeated press toggled twice")
	}
	shell.key(key.Event{Name: "G", State: key.Release})
	shell.key(press)
	if !shell.Model.Snapshot().Toggles.Grid {
		t.Fatal("new physical press was not accepted")
	}
}

func TestPlaySpeedCanBeAdjustedUpAndDown(t *testing.T) {
	shell := NewShell("")
	shell.Model.Navigate(ScreenPlay)

	shell.key(key.Event{Name: "+", State: key.Press})
	if got := shell.Model.Snapshot().HUD.Speed; got != 6 {
		t.Fatalf("speed after increase = %d, want 6", got)
	}
	shell.key(key.Event{Name: "+", State: key.Release})
	shell.key(key.Event{Name: "-", State: key.Press})
	if got := shell.Model.Snapshot().HUD.Speed; got != 5 {
		t.Fatalf("speed after decrease = %d, want 5", got)
	}

	var ops op.Ops
	gtx := layout.Context{Ops: &ops, Constraints: layout.Exact(image.Pt(1280, 720))}
	shell.speedUp.Click()
	shell.hud(gtx, shell.Model.Snapshot())
	if got := shell.Model.Snapshot().HUD.Speed; got != 6 {
		t.Fatalf("speed after button increase = %d, want 6", got)
	}
	shell.speedDown.Click()
	shell.hud(gtx, shell.Model.Snapshot())
	if got := shell.Model.Snapshot().HUD.Speed; got != 5 {
		t.Fatalf("speed after button decrease = %d, want 5", got)
	}
}

func TestTeachRequestCarriesExactPendingIdentity(t *testing.T) {
	var path string
	var got TeachRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v1","game":{"id":"g","cursor":8,"event_hash":"next"},"state":{"width":18,"height":18,"active_slot":-1}}`))
	}))
	defer server.Close()
	client := NewHTTPClient(server.URL)
	_, _, err := client.Teach(context.Background(), "g", TeachRequest{Cursor: 7, EventHash: "head", WormID: "w", Direction: 4, Mask: 13, Request: 99, PendingMask: 13, PendingRequest: 99})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/games/g/teach" || got.Request != 99 || got.PendingRequest != 99 || got.Mask != 13 || got.PendingMask != 13 || got.WormID != "w" {
		t.Fatalf("teach identity path=%q request=%+v", path, got)
	}
}

func TestInspectorDecodesSixBitsActionsAndStablePagingQuery(t *testing.T) {
	var rules []InspectorRule
	if err := json.Unmarshal([]byte(`[{"mask":"5","action":2,"provenance":{"source":"fixture"}},{"mask":63,"action":-3}]`), &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("decoded rules = %+v", rules)
	}
	byMask := map[uint8]InspectorRule{}
	for _, rule := range rules {
		byMask[rule.Mask] = rule
	}
	if got := byMask[5]; got.ActionName != "SW" || len(got.Directions) != 2 || got.Directions[0] != "E" || got.Directions[1] != "SW" || got.Provenance["source"] != "fixture" {
		t.Fatalf("mask 5 decoding = %+v", got)
	}
	if byMask[63].ActionName != "DIE" {
		t.Fatalf("sentinel decoding = %+v", byMask[63])
	}

	var rawQuery string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rawQuery = r.URL.RawQuery
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		body := `{"version":"v1","brain_id":"arbitrary brain:id","version_id":"arbitrary-version-id","version_number":3,"rules":[{"mask":5,"orientation":1,"incoming":1,"action":2,"action_name":"SW"}],"total":64,"limit":12,"offset":24,"next_offset":36}`
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	client := NewHTTPClient("http://browser.test").WithHTTPClient(&http.Client{Transport: transport})
	result, _, err := client.InspectPage(context.Background(), "arbitrary brain:id", 3, 12, "mask:5", 24)
	if err != nil {
		t.Fatal(err)
	}
	if rawQuery != "filter=mask%3A5&limit=12&offset=24&version=3" {
		t.Fatalf("paging/filter query = %q", rawQuery)
	}
	if result.BrainID != "arbitrary brain:id" || result.VersionID != "arbitrary-version-id" || result.Version != 3 || result.Offset != 24 || result.Limit != 12 || result.Total != 64 || result.NextOffset != 36 || len(result.Rules) != 1 || result.Rules[0].Mask != 5 {
		t.Fatalf("authoritative rule page was not consumed directly: %+v", result)
	}
}

func TestSupportedViewportLayoutsStayWithinConstraints(t *testing.T) {
	viewports := []image.Point{{320, 480}, {768, 576}, {1280, 577}, {1440, 900}}
	for _, viewport := range viewports {
		shell := NewShell("")
		var ops op.Ops
		gtx := layout.Context{Ops: &ops, Constraints: layout.Exact(viewport), Now: time.Unix(1, 0)}
		dims := shell.screen(gtx, shell.Model.Snapshot())
		if dims.Size.X < 0 || dims.Size.Y < 0 || dims.Size.X > viewport.X || dims.Size.Y > viewport.Y {
			t.Fatalf("setup layout %v escaped viewport: %v", viewport, dims.Size)
		}

		play := shell.Model.Snapshot()
		play.Screen = ScreenPlay
		play.Board = BoardView{Width: 64, Height: 64, Territory: map[Point]uint32{}, TerritoryOwners: map[Point]string{}}
		play.HUD.Scores = []ScoreView{{ID: "one", Name: "one", Alive: true}, {ID: "two", Name: "two", Asleep: true}}
		ops.Reset()
		gtx = layout.Context{Ops: &ops, Constraints: layout.Exact(viewport), Now: time.Unix(1, 0)}
		dims = shell.screen(gtx, play)
		if dims.Size.X > viewport.X || dims.Size.Y > viewport.Y {
			t.Fatalf("play layout %v escaped viewport: %v", viewport, dims.Size)
		}
	}
}
