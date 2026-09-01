package debug

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"worms.ng/internal/store"
)

func testStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worms.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateBrain(context.Background(), store.CreateBrainInput{ID: "b1", Name: "brain"})
	if err != nil {
		t.Fatal(err)
	}
	_ = b
	wrap := func(v any) []byte {
		p, e := store.EncodePayload(v)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	if _, err = s.CreateBrainVersion(context.Background(), store.CreateBrainVersionInput{ID: "v1", BrainID: "b1", Version: 1, Rules: wrap(map[string]any{"token": "secret"}), Lineage: wrap(map[string]any{}), Provenance: wrap(map[string]any{"source": "test"}), Payload: wrap(map[string]any{"name": "p"})}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateGame(context.Background(), store.CreateGameInput{ID: "g1", BrainVersionID: "v1", RulesPayload: wrap(map[string]any{}), Participants: []store.Participant{{ID: "p1", Name: "one", Payload: wrap(map[string]any{})}}}); err != nil {
		t.Fatal(err)
	}
	es, err := s.AppendGameEvents(context.Background(), "g1", 0, "", []store.EventInput{{Type: "decision", Payload: wrap(map[string]any{"brain": "v1"})}, {Type: "worm_died", Payload: wrap(map[string]any{})}})
	if err != nil {
		t.Fatal(err)
	}
	_ = es
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestSQLiteReaderReadOnlyAndHashes(t *testing.T) {
	path := testStore(t)
	r, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := r.Close(); closeErr != nil {
			t.Errorf("close reader: %v", closeErr)
		}
	}()
	if _, err := r.Brain(context.Background(), "b1"); err != nil {
		t.Fatal(err)
	}
	g, err := r.Game(context.Background(), "g1")
	if err != nil {
		t.Fatal(err)
	}
	es, err := r.Events(context.Background(), "g1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyEvents(g, es); err != nil {
		t.Fatal(err)
	}
	if _, err = r.db.Exec("CREATE TABLE no_writes(id INTEGER)"); err == nil {
		t.Fatal("read-only connection allowed write")
	}
}
func TestDiagnosticRedactionRoundTrip(t *testing.T) {
	path := testStore(t)
	r, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := r.Close(); closeErr != nil {
			t.Errorf("close reader: %v", closeErr)
		}
	}()
	d, err := Export(context.Background(), r, "b1", "g1", true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Versioned(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatal("secret leaked")
	}
	if _, err = ImportDiagnostic(raw); err != nil {
		t.Fatal(err)
	}
}
func TestRunExitCodesAndVersionedOutput(t *testing.T) {
	path := testStore(t)
	var out, errs bytes.Buffer
	code := Run(context.Background(), []string{"--db", path, "brain", "show", "b1"}, &out, &errs)
	if code != ExitOK {
		t.Fatalf("code=%d err=%s", code, errs.String())
	}
	var x struct {
		Version int            `json:"version"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &x); err != nil {
		t.Fatal(err)
	}
	if x.Version != 1 {
		t.Fatalf("version=%d", x.Version)
	}
	code = Run(context.Background(), []string{"--db", path, "brain", "show", "missing"}, &out, &errs)
	if code != ExitNotFound {
		t.Fatalf("not found code=%d", code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
