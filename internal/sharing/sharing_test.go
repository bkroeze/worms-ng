package sharing

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"testing"

	"worms.ng/internal/store"
)

func envelope(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := store.EncodePayload(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSQLiteSharingPoliciesAndSourceImmutability(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "sharing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	if _, err = s.CreateBrain(ctx, store.CreateBrainInput{ID: "sources", Name: "sources"}); err != nil {
		t.Fatal(err)
	}
	left := envelope(t, map[string]any{"left": 1, "conflict": 10})
	right := envelope(t, map[string]any{"right": 2, "conflict": 20})
	v1, err := s.CreateBrainVersion(ctx, store.CreateBrainVersionInput{ID: "left-v1", BrainID: "sources", Version: 1, Rules: left, Lineage: envelope(t, nil), Provenance: envelope(t, map[string]any{"source": "left"}), Payload: envelope(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.CreateBrainVersion(ctx, store.CreateBrainVersionInput{ID: "right-v1", BrainID: "sources", Version: 2, Rules: right, Lineage: envelope(t, nil), Provenance: envelope(t, map[string]any{"source": "right"}), Payload: envelope(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	before1, before2 := v1.Rules.Payload, v2.Rules.Payload
	base := Config{Sources: []Source{{WormID: "a", Team: "red", BrainVersionID: v1.ID}, {WormID: "b", Team: "blue", BrainVersionID: v2.ID}}}
	for _, policy := range []Policy{NoSharing, SameTeamSharing, AllWormSharing} {
		base.Policy = policy
		out, err := DeriveFromStore(ctx, s, base)
		if err != nil {
			t.Fatalf("%s: %v", policy, err)
		}
		if len(out.Derived) != 2 || out.Hash == "" {
			t.Fatalf("%s: invalid output %#v", policy, out)
		}
		for _, d := range out.Derived {
			if len(d.Rules) == 0 || d.Hash == "" || d.Provenance.Policy != policy {
				t.Fatalf("%s: incomplete derived table %#v", policy, d)
			}
		}
		expectedAdditions := map[Policy]int{NoSharing: 0, SameTeamSharing: 0, AllWormSharing: 1}[policy]
		if len(out.Derived[0].Additions) != expectedAdditions {
			t.Fatalf("%s: unexpected additions=%#v", policy, out.Derived[0].Additions)
		}
	}
	got1, _ := s.GetBrainVersion(ctx, v1.ID)
	got2, _ := s.GetBrainVersion(ctx, v2.ID)
	if string(got1.Rules.Payload) != string(before1) || string(got2.Rules.Payload) != string(before2) {
		t.Fatal("sharing mutated an immutable source version")
	}

	base.Policy = SameTeamSharing
	base.Sources[1].Team = "red"
	out, err := DeriveFromStore(ctx, s, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateBrain(ctx, store.CreateBrainInput{ID: "derived", Name: "derived", Frozen: true}); err != nil {
		t.Fatal(err)
	}
	persisted, err := out.Persist(ctx, s, "derived")
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 2 || persisted[0].Rules.Hash == "" {
		t.Fatalf("persisted=%#v", persisted)
	}
	stored, err := s.GetBrainVersion(ctx, persisted[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Rules.Hash != out.Derived[0].Hash {
		t.Fatalf("stored hash=%s derived=%s", stored.Rules.Hash, out.Derived[0].Hash)
	}
}

func TestDeterministicSeedHashConflictAndCorruption(t *testing.T) {
	cfg := Config{Policy: AllWormSharing, Sources: []Source{
		{WormID: "z", Team: "x", BrainVersionID: "z-v", Rules: envelope(t, map[string]any{"same": 9, "z": 1})},
		{WormID: "a", Team: "x", BrainVersionID: "a-v", Rules: envelope(t, map[string]any{"same": 2, "a": 3})},
	}}
	one, err := Derive(cfg)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Derive(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if one.Hash != two.Hash || !reflect.DeepEqual(one, two) {
		t.Fatal("same inputs produced different sharing hash/output")
	}
	// For recipient z, donor a wins the conflict because donor IDs are the
	// deterministic tie-break key, and the replacement is reported as a change.
	if len(one.Derived[1].Changes) == 0 || one.Derived[1].Changes[0].DonorID != "a" {
		t.Fatalf("conflict was not deterministically recorded: %#v", one.Derived[1].Changes)
	}
	noisy := cfg
	noisy.Policy, noisy.Seed, noisy.NoiseRate = SeededNoisySharing, 42, 1
	a, err := Derive(noisy)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Derive(noisy)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash || len(a.Derived[0].Provenance.Corruptions) == 0 {
		t.Fatal("seeded corruption is not reproducible/audited")
	}
	noisy.Seed++
	c, err := Derive(noisy)
	if err != nil {
		t.Fatal(err)
	}
	if c.Hash == a.Hash {
		t.Fatal("changing seed did not change noisy output hash")
	}
}

func TestComparisonMetrics(t *testing.T) {
	c := CompareMetrics([]Observation{
		{Policy: NoSharing, Score: 4, Survived: true, KnownPatterns: 3, UnknownPatterns: 1},
		{Policy: NoSharing, Score: 2, KnownPatterns: 1, UnknownPatterns: 1},
		{Policy: AllWormSharing, Score: 8, Survived: true, KnownPatterns: 4},
	})
	m := c.Policies[NoSharing]
	if m.Score != 3 || m.Survival != .5 || m.Coverage != 2.0/3.0 || m.UnknownPatternRate != 1.0/3.0 {
		t.Fatalf("metrics=%#v", m)
	}
	if c.Policies[AllWormSharing].UnknownPatternRate != 0 {
		t.Fatalf("all-worm metrics=%#v", c.Policies[AllWormSharing])
	}
}

func TestAdversarialValidationAndCanonicalOrder(t *testing.T) {
	duplicate := envelope(t, []map[string]any{{"key": "x", "value": 1}, {"key": "x", "value": 2}})
	if _, err := Derive(Config{Policy: NoSharing, Sources: []Source{{WormID: "a", Rules: duplicate}}}); err == nil {
		t.Fatal("duplicate normalized keys were accepted")
	}
	for _, rate := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.01, 1.01} {
		if _, err := Derive(Config{Policy: SeededNoisySharing, NoiseRate: rate, Sources: []Source{
			{WormID: "a", Rules: envelope(t, map[string]any{"a": 1})},
		}}); err == nil {
			t.Fatalf("invalid rate %v was accepted", rate)
		}
	}
	a := envelope(t, []map[string]any{{"key": "b", "value": 2}, {"key": "a", "value": 1}})
	b := envelope(t, []map[string]any{{"key": "a", "value": 1}, {"key": "b", "value": 2}})
	cfg := Config{Policy: SeededNoisySharing, Seed: 17, NoiseRate: .5, Sources: []Source{
		{WormID: "recipient", Rules: envelope(t, map[string]any{"r": 0})},
		{WormID: "donor", Rules: a},
	}}
	first, err := Derive(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Sources[1].Rules = b
	second, err := Derive(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || !reflect.DeepEqual(first, second) {
		t.Fatal("keyed rule order changed deterministic output")
	}
}

func TestDeriveFromStoreRejectsMismatchedRulesAndBadVersionHash(t *testing.T) {
	ctx := context.Background()
	v := store.BrainVersion{ID: "v1", Rules: store.Rules{Payload: envelope(t, map[string]any{"x": 1}), Hash: "wrong"}}
	reader := fakeBrainReader{versions: map[string]store.BrainVersion{"v1": v}}
	_, err := DeriveFromStore(ctx, reader, Config{Policy: NoSharing, Sources: []Source{{WormID: "a", BrainVersionID: "v1"}}})
	if err == nil {
		t.Fatal("bad source payload hash was accepted")
	}
	v.Rules.Hash = stableHash(v.Rules.Payload)
	reader.versions["v1"] = v
	_, err = DeriveFromStore(ctx, reader, Config{Policy: NoSharing, Sources: []Source{{
		WormID: "a", BrainVersionID: "v1", Rules: envelope(t, map[string]any{"x": 2}),
	}}})
	if err == nil {
		t.Fatal("mismatched supplied rules were accepted")
	}
}

type fakeBrainReader struct {
	versions map[string]store.BrainVersion
}

func (r fakeBrainReader) GetBrainVersion(_ context.Context, id string) (store.BrainVersion, error) {
	v, ok := r.versions[id]
	if !ok {
		return store.BrainVersion{}, store.ErrNotFound
	}
	return v, nil
}
