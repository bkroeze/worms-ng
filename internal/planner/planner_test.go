package planner

import (
	"context"
	"errors"
	"testing"

	"worms.ng/internal/engine"
)

func teachingState() engine.State {
	s := engine.New(6, 6, []engine.Worm{{ID: "teacher", Position: engine.Point{Q: 3, R: 3}, Alive: true}})
	if err := engine.ConfigureWorm(&s.Worms[0], engine.ControllerNew, 17); err != nil {
		panic(err)
	}
	return s
}

func TestTeachUsesLocalLookupAndIsDeterministic(t *testing.T) {
	a, b := teachingState(), teachingState()
	cfg := Config{Mode: Lookahead, Depth: 3, Seed: 44, CaptureWeight: 10, BorderWeight: 2, SurvivalWeight: 1}
	pa, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := New(cfg)
	da, err := pa.Teach(&a, "teacher")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pb.Teach(&b, "teacher")
	if err != nil {
		t.Fatal(err)
	}
	if da.Action != db.Action || da.Mask != db.Mask {
		t.Fatalf("seeded plans differ: %#v %#v", da, db)
	}
	if len(da.Alternatives) == 0 || len(da.Provenance.Alternatives) != len(da.Alternatives) {
		t.Fatal("decision did not retain explainable alternatives")
	}
	got, err := a.Lookup("teacher")
	if err != nil || got != da.Action {
		t.Fatalf("ordinary local lookup=%v,%v want %v", got, err, da.Action)
	}
	if _, err := pa.Plan(a, "teacher"); !errors.Is(err, ErrKnownPattern) {
		t.Fatalf("known pattern plan error=%v", err)
	}
}

func TestFrozenLookupNeverCallsPlanner(t *testing.T) {
	s := teachingState()
	s.Worms[0].Frozen = true
	s.Worms[0].Rules[0] = engine.ActionGetNew
	p := MustNew(DefaultConfig())
	if _, err := p.Plan(s, "teacher"); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen plan error=%v", err)
	}
	if p.Calls() != 0 {
		t.Fatalf("frozen plan incremented planner calls=%d", p.Calls())
	}
	if _, err := FrozenLookup(s, "teacher"); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen lookup error=%v", err)
	}
}

func TestHeldOutRejectsLeakageAndDoesNotTrainHeldOut(t *testing.T) {
	if _, err := NewHeldOut([]int64{3}, []int64{3}); !errors.Is(err, ErrSeedLeakage) {
		t.Fatalf("overlap error=%v", err)
	}
	calls := 0
	cfg, err := NewHeldOut([]int64{1}, []int64{2})
	if err != nil {
		t.Fatal(err)
	}
	cfg.NewState = func(seed int64) (engine.State, error) {
		s := teachingState()
		s.Tick = uint64(seed)
		return s, nil
	}
	cfg.Train = func(_ context.Context, p *Planner, s *engine.State, _ int64) error {
		calls++
		_, err := p.Teach(s, "teacher")
		return err
	}
	cfg.Evaluate = func(_ context.Context, s engine.State, _ int64) (SeedResult, error) {
		if s.Worms[0].Rules[0] != engine.ActionGetNew {
			t.Fatal("held-out state was trained")
		}
		return SeedResult{StateHash: s.HashHex()}, nil
	}
	result, err := RunHeldOut(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(result.Training) != 1 || len(result.HeldOut) != 1 {
		t.Fatalf("unexpected experiment result calls=%d result=%+v", calls, result)
	}
}

func TestPlannerTaughtBrainComparedWithAutoAndNew(t *testing.T) {
	planned := teachingState()
	destination := planned.Neighbor(planned.Worms[0].Position, engine.East)
	planned.Territories[destination] = engine.Territory{ID: destination, Mask: 63}
	auto := planned.Snapshot()
	if err := engine.ConfigureWorm(&auto.Worms[0], engine.ControllerAuto, 17); err != nil {
		t.Fatal(err)
	}
	newBrain := planned.Snapshot()
	if err := engine.ConfigureWorm(&newBrain.Worms[0], engine.ControllerNew, 17); err != nil {
		t.Fatal(err)
	}
	p := MustNew(Config{Mode: Greedy, Seed: 17, CaptureWeight: 100, BorderWeight: 1, SurvivalWeight: 1})
	d, err := p.Teach(&planned, "teacher")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != engine.Action(engine.East) {
		t.Fatalf("planner ignored immediate capture: %v", d.Action)
	}
	if _, err := planned.AdvanceRound(); err != nil {
		t.Fatal(err)
	}
	if _, err := auto.AdvanceRound(); err != nil {
		t.Fatal(err)
	}
	if auto.Worms[0].Rules[0] != engine.Action(engine.East) {
		t.Fatalf("AUTO baseline did not capture: %v", auto.Worms[0].Rules[0])
	}
	if _, err := newBrain.AdvanceRound(); err != nil {
		t.Fatal(err)
	}
	if newBrain.Pending == nil || newBrain.Pending.Mask != 0 {
		t.Fatalf("NEW baseline did not expose unknown pattern: %+v", newBrain.Pending)
	}
}

func TestGlobalPlanSnapshotsEveryCandidate(t *testing.T) {
	s := teachingState()
	before := s.Snapshot()
	p := MustNew(Config{Capabilities: Capabilities{Observation: GlobalObservation, GlobalState: true}, Seed: 7})
	if _, err := p.Plan(s, "teacher"); err != nil {
		t.Fatal(err)
	}
	if got, want := s.HashHex(), before.HashHex(); got != want {
		t.Fatalf("global planning mutated input: got %s want %s", got, want)
	}
}

func TestLocalPlanNoninterferenceAndProvenance(t *testing.T) {
	a := teachingState()
	b := a.Snapshot()
	b.Tick, b.Round = 91, 12
	b.Trails[engine.NewEdge(engine.Point{Q: 0, R: 0}, engine.Point{Q: 1, R: 0})] = "remote"
	b.Territories[engine.Point{Q: 0, R: 0}] = engine.Territory{ID: engine.Point{Q: 0, R: 0}, Mask: 63, Owner: "remote"}
	p := MustNew(Config{Seed: 19})
	da, err := p.Plan(a, "teacher")
	if err != nil {
		t.Fatal(err)
	}
	db, err := p.Plan(b, "teacher")
	if err != nil {
		t.Fatal(err)
	}
	if da.Action != db.Action || da.Provenance.StateHash != db.Provenance.StateHash {
		t.Fatalf("hidden state influenced local plan: %#v %#v", da, db)
	}
	if da.Provenance.Tick != 0 || da.Provenance.Round != 0 {
		t.Fatalf("local provenance disclosed hidden time: %+v", da.Provenance)
	}
}

func TestTeachResolvesMatchingPendingAndRejectsMismatch(t *testing.T) {
	s := teachingState()
	if _, err := s.AdvanceRound(); err != nil {
		t.Fatal(err)
	}
	if s.Pending == nil {
		t.Fatal("expected pending decision")
	}
	p := MustNew(DefaultConfig())
	if _, err := p.Teach(&s, "teacher"); err != nil {
		t.Fatal(err)
	}
	if s.Pending != nil {
		t.Fatal("Teach left pending decision unresolved")
	}
	found := false
	for _, e := range s.Events {
		if e.Type == "rule_learned" && e.WormID == "teacher" {
			found = true
		}
	}
	if !found {
		t.Fatal("pending Teach did not emit rule_learned")
	}

	bad := teachingState()
	bad.Pending = &engine.Decision{WormID: "other", Mask: 0, Slot: 0}
	before := bad.Snapshot()
	if _, err := p.Teach(&bad, "teacher"); !errors.Is(err, ErrPendingMismatch) {
		t.Fatalf("mismatched pending error=%v", err)
	}
	if bad.HashHex() != before.HashHex() {
		t.Fatal("mismatched pending Teach mutated state")
	}
}

func TestLookupDeadWormDelegatesEngineSemantics(t *testing.T) {
	s := teachingState()
	s.Worms[0].Alive = false
	s.Worms[0].Rules[0] = engine.Action(engine.East)
	if got, err := Lookup(s, "teacher"); err != nil || got != engine.ActionDie {
		t.Fatalf("dead Lookup=%v,%v want DIE,nil", got, err)
	}
}

func TestHeldOutSnapshotsFreezesAndDetachesScores(t *testing.T) {
	template := teachingState()
	cfg, err := NewHeldOut(nil, []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	cfg.NewState = func(int64) (engine.State, error) { return template, nil }
	shared := map[string]int{}
	cfg.Evaluate = func(_ context.Context, s engine.State, seed int64) (SeedResult, error) {
		if !s.Worms[0].Frozen {
			t.Fatal("held-out worm was not frozen")
		}
		if _, err := MustNew(DefaultConfig()).Teach(&s, "teacher"); !errors.Is(err, ErrFrozen) {
			t.Fatalf("frozen held-out Teach error=%v", err)
		}
		shared["seed"] = int(seed)
		return SeedResult{Score: shared}, nil
	}
	out, err := RunHeldOut(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.HeldOut[0].Score["seed"] != 1 || out.HeldOut[1].Score["seed"] != 2 {
		t.Fatalf("held-out score alias leaked: %+v", out.HeldOut)
	}
	if template.Worms[0].Rules[0] != engine.ActionGetNew {
		t.Fatal("generator template was mutated through callback state")
	}
}

func TestHeldOutRejectsRuleMutation(t *testing.T) {
	cfg, err := NewHeldOut(nil, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	cfg.NewState = func(int64) (engine.State, error) { return teachingState(), nil }
	cfg.Evaluate = func(_ context.Context, s engine.State, _ int64) (SeedResult, error) {
		s.Worms[0].Rules[0] = engine.Action(engine.East)
		return SeedResult{}, nil
	}
	if _, err := RunHeldOut(context.Background(), cfg); err == nil {
		t.Fatal("held-out rule mutation was accepted")
	}
}
