package extension

import (
	"bytes"
	"encoding/json"
	"testing"

	"worms.ng/internal/engine"
)

func fixtureState() engine.State {
	return engine.New(4, 4, []engine.Worm{{ID: "a", Alive: true, Position: engine.PointXY(1, 1)}, {ID: "b", Alive: true, Position: engine.PointXY(3, 3)}})
}

func TestClassicDelegatesByteForByte(t *testing.T) {
	base := fixtureState()
	want, err := base.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ext, err := New(base, Config{}, 99)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ext.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("classic extension changed snapshot bytes")
	}
	if _, err := ext.Apply("a", engine.East); err != nil {
		t.Fatal(err)
	}
	if _, err := base.Step("a", engine.East); err != nil {
		t.Fatal(err)
	}
	wantAfter, err := base.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	gotAfter, err := ext.Base.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotAfter, wantAfter) {
		t.Fatal("classic transition changed engine state")
	}
}

func TestVariantRoundTripFogAndEnergy(t *testing.T) {
	base := fixtureState()
	cfg := Config{Version: 1, Enabled: true, Obstacles: []engine.Point{engine.PointXY(0, 0)}, EnergyLimit: 1, FogOfWar: true, VisibilityRadius: 0}
	s, err := New(base, cfg, 7)
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.Observe("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Base.Scores) != 1 || o.UnknownCount == 0 {
		t.Fatalf("fog leaked score/map: %+v", o)
	}
	d := s.LegalMoves("a")[0]
	if _, err := s.Apply("a", d); err != nil {
		t.Fatal(err)
	}
	if s.Base.Worms[0].Alive || s.Variant.Energy["a"] != 0 {
		t.Fatal("energy exhaustion did not kill worm")
	}
	raw, err := s.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	r, err := UnmarshalSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw2, err := r.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, raw2) {
		t.Fatal("variant snapshot is not byte stable")
	}
}

func TestAdvanceRoundExpiresTemporaryTrails(t *testing.T) {
	base := engine.New(4, 4, []engine.Worm{{ID: "a", Alive: true, Position: engine.PointXY(1, 1)}})
	if err := engine.ConfigureWorm(&base.Worms[0], engine.ControllerNew, 7); err != nil {
		t.Fatal(err)
	}
	s, err := New(base, Config{Version: 1, Enabled: true, TemporaryTrailTTL: 1}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply("a", engine.East); err != nil {
		t.Fatal(err)
	}
	if len(s.Base.Trails) != 1 {
		t.Fatalf("trails=%d, want one temporary trail", len(s.Base.Trails))
	}
	s.Base.Tick = 2
	s.expire()
	if len(s.Variant.Temporary) != 0 {
		t.Fatalf("temporary trail expiry markers remain: %v", s.Variant.Temporary)
	}
}

func TestOneWayRoundTripPreservesEndpoints(t *testing.T) {
	base := fixtureState()
	cfg := Config{
		Version: 1, Enabled: true,
		OneWayTrails: []OneWayTrail{{From: engine.PointXY(1, 1), To: engine.PointXY(2, 1)}},
	}
	s, err := New(base, cfg, 9)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	r, err := UnmarshalSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw2, err := r.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, raw2) {
		t.Fatal("one-way snapshot is not byte stable")
	}
	if got := r.Config.OneWayTrails[0]; got.From != engine.PointXY(1, 1) || got.To != engine.PointXY(2, 1) {
		t.Fatalf("one-way endpoints changed: %+v", got)
	}
}

func TestConfigAndSnapshotAreDeepCopied(t *testing.T) {
	base := fixtureState()
	cfg := Config{Version: 1, Enabled: true, Obstacles: []engine.Point{engine.PointXY(0, 0)}, Teams: map[string]string{"a": "red", "b": "blue"}}
	s, err := New(base, cfg, 3)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Obstacles[0] = engine.PointXY(2, 2)
	cfg.Teams["a"] = "blue"
	snap := s.Snapshot()
	snap.Config.Obstacles[0] = engine.PointXY(3, 3)
	snap.Config.Teams["b"] = "red"
	if s.Config.Obstacles[0] != engine.PointXY(0, 0) || s.Config.Teams["a"] != "red" || s.Config.Teams["b"] != "blue" {
		t.Fatal("config aliases crossed state boundaries")
	}
}

func TestReplayConsumesSequencedEvents(t *testing.T) {
	base := fixtureState()
	s, err := New(base, Config{Version: 1, Enabled: true, EnergyLimit: 3}, 4)
	if err != nil {
		t.Fatal(err)
	}
	initial := s.Snapshot()
	if _, err := s.Apply("a", s.LegalMoves("a")[0]); err != nil {
		t.Fatal(err)
	}
	events := s.EventsCopy()
	replayed, err := Replay(initial, events)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.HashHex() != s.HashHex() {
		t.Fatal("replay did not restore extension state")
	}
	events[0].Seq++
	if _, err := Replay(initial, events); err == nil {
		t.Fatal("replay accepted a sequence violation")
	}
}

func TestNormalizeConfigCarriesRequestSeed(t *testing.T) {
	c := NormalizeConfig(Config{Version: 1, Enabled: true}, 41)
	if c.Seed != 41 {
		t.Fatalf("normalized seed=%d, want 41", c.Seed)
	}
	c = NormalizeConfig(Config{Version: 1, Enabled: true, Seed: 9}, 41)
	if c.Seed != 9 {
		t.Fatalf("explicit seed overwritten: %d", c.Seed)
	}
}

func TestAdvanceRoundUsesExtensionLegalityAndReplaysMarker(t *testing.T) {
	base := engine.New(4, 4, []engine.Worm{{ID: "a", Alive: true, Position: engine.PointXY(1, 1)}})
	if err := engine.ConfigureWorm(&base.Worms[0], engine.ControllerAuto, 7); err != nil {
		t.Fatal(err)
	}
	s, err := New(base, Config{Version: 1, Enabled: true, Obstacles: []engine.Point{engine.PointXY(2, 1)}}, 7)
	if err != nil {
		t.Fatal(err)
	}
	initial := s.Snapshot()
	if _, err := s.AdvanceRound(); err != nil {
		t.Fatal(err)
	}
	if len(s.Events) == 0 || s.Events[len(s.Events)-1].Type != "round_advanced" {
		t.Fatalf("missing round marker: %+v", s.Events)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	replayed, err := Replay(initial, s.EventsCopy())
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Events) != len(s.Events) || replayed.Events[len(replayed.Events)-1].Type != "round_advanced" {
		t.Fatalf("round replay did not consume marker: %+v", replayed.Events)
	}
}
func TestWildControllerAvoidsExtensionObstacle(t *testing.T) {
	base := engine.New(4, 4, []engine.Worm{{ID: "a", Alive: true, Position: engine.PointXY(1, 1)}})
	if err := engine.ConfigureWorm(&base.Worms[0], engine.ControllerWild, 2); err != nil {
		t.Fatal(err)
	}
	s, err := New(base, Config{Version: 1, Enabled: true, Obstacles: []engine.Point{engine.PointXY(2, 1)}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceRound(); err != nil {
		t.Fatal(err)
	}
	if s.Base.Worms[0].Position == engine.PointXY(2, 1) {
		t.Fatal("wild controller entered extension obstacle")
	}
}

func TestReplayAdvancesHashCursorAcrossBatch(t *testing.T) {
	s, err := New(fixtureState(), Config{Version: 1, Enabled: true, EnergyLimit: 3}, 4)
	if err != nil {
		t.Fatal(err)
	}
	initial := s.Snapshot()
	for range 2 {
		moves := s.LegalMoves("a")
		if len(moves) == 0 {
			t.Fatal("no legal move")
		}
		if _, err := s.Apply("a", moves[0]); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.Events) < 2 {
		t.Fatal("expected two events")
	}
	if replayed, err := Replay(initial, s.EventsCopy()); err != nil {
		t.Fatal(err)
	} else if replayed.HashHex() != s.HashHex() {
		t.Fatal("replay hash mismatch")
	}
}

func TestFogClientProjectionRedactsTerrain(t *testing.T) {
	s, err := New(fixtureState(), Config{
		Version: 1, Enabled: true, FogOfWar: true, VisibilityRadius: 0,
		Obstacles:           []engine.Point{engine.PointXY(0, 0)},
		Holes:               []engine.Point{engine.PointXY(0, 1)},
		WeightedTerritories: map[engine.Point]int{engine.PointXY(2, 2): 9},
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ClientProjection("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Config.Teams) != 0 {
		t.Fatal("unexpected teams")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"obstacles"`)) || bytes.Contains(raw, []byte(`"holes"`)) || bytes.Contains(raw, []byte(`"seed"`)) {
		t.Fatalf("terrain leaked in projection: %s", raw)
	}
	if p.TeamScores != nil || len(p.TeamWinners) != 0 {
		t.Fatal("authoritative team data leaked in fog projection")
	}
}
func TestScoreAndTeamAccessors(t *testing.T) {
	base := fixtureState()
	s, err := New(base, Config{Version: 1, Enabled: true, Teams: map[string]string{"a": "red", "b": "blue"}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Score("a"); got != s.Variant.Scores["a"] {
		t.Fatalf("score=%d, want %d", got, s.Variant.Scores["a"])
	}
	if got := s.TeamTotals()["red"]; got != s.Variant.TeamScores["red"] {
		t.Fatalf("team score=%d, want %d", got, s.Variant.TeamScores["red"])
	}
}
