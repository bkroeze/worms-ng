package match

import (
	"context"
	"testing"

	"worms.ng/internal/store"
)

func TestSQLiteBlackBoxRestartAfterTransitionKeepsStateHash(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state := testState()
	controllers := []Controller{NewRandomController(31), NewRandomController(32)}
	m, err := NewMatch(ctx, Config{Store: st, Initial: state, Controllers: controllers, Seed: 31})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if m.Finished() {
			break
		}
		if _, err := m.Advance(ctx); err != nil {
			t.Fatal(err)
		}
		want := m.State().HashHex()
		resumed, err := ResumeMatch(ctx, Config{Store: st, GameID: m.GameID(), Controllers: controllers, Seed: 31})
		if err != nil {
			t.Fatal(err)
		}
		if got := resumed.State().HashHex(); got != want {
			t.Fatalf("restart state hash=%s want=%s", got, want)
		}
		m = resumed
	}
	g, err := st.GetGame(ctx, m.GameID())
	if err != nil {
		t.Fatal(err)
	}
	if g.Sequence == 0 {
		t.Fatal("no SQLite transition cursor persisted")
	}
}
