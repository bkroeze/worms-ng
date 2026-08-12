package debug

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"worms.ng/internal/store"
)

func TestDecodeRulesCanonicalSixDirections(t *testing.T) {
	rules, err := DecodeRules(json.RawMessage(`{"version":1,"data":[-1,0,1,2,3,4,-2,-3]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 8 || rules[1].DirectionName != "east" || rules[2].DirectionName != "south-east" {
		t.Fatalf("unexpected decoded rules: %#v", rules)
	}
	if rules[0].Action != "learn" || rules[7].Action != "die" {
		t.Fatalf("sentinels not decoded: %#v", rules)
	}
}

func TestDiagnosticRestoreIntoEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	source := testStore(t)
	r, err := OpenSQLite(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Export(ctx, r, "b1", "g1", true)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored.db")
	if err := RestoreDiagnostic(ctx, dest, d); err != nil {
		t.Fatal(err)
	}
	r2, err := OpenSQLite(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	g, err := r2.Game(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Sequence != d.Games[0].Sequence || g.EventHash != d.Games[0].EventHash {
		t.Fatalf("restored head=%+v source=%+v", g, d.Games[0])
	}
	es, err := r2.Events(ctx, "g1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEvents(g, es); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteBrainPageIsBoundedAndCarriesTotals(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "large.db")
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.CreateBrain(ctx, store.CreateBrainInput{ID: "large", Name: "large"}); err != nil {
		t.Fatal(err)
	}
	wrap := func(v any) []byte {
		b, e := store.EncodePayload(v)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	for i := 1; i <= 120; i++ {
		if _, err = s.CreateBrainVersion(ctx, store.CreateBrainVersionInput{
			ID: fmt.Sprintf("v%03d", i), BrainID: "large", Version: int64(i),
			Rules: wrap([]int{i}), Lineage: wrap(map[string]any{"sequence": i}),
			Provenance: wrap(map[string]any{"learned_event_id": fmt.Sprintf("event-%d", i), "learned_sequence": i, "rule_usage": map[string]int{strconv.Itoa(i): i + 1}}),
			Payload:    wrap(map[string]any{"i": i}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	page, err := r.BrainPage(ctx, "large", 7, 51)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 120 || len(page.Versions) != 7 || page.Versions[0].ID != "v052" || page.NextOffset != 58 {
		t.Fatalf("unexpected page: total=%d len=%d first=%s next=%d", page.Total, len(page.Versions), page.Versions[0].ID, page.NextOffset)
	}
	if got := page.Versions[0].References; len(got) == 0 || !strings.Contains(strings.Join(got, ","), "learned_event_id=event-52") {
		t.Fatalf("learned provenance missing: %#v", got)
	}
}

func TestDiagnosticRestoreRefusesPopulatedDestination(t *testing.T) {
	ctx := context.Background()
	source := testStore(t)
	r, err := OpenSQLite(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Export(ctx, r, "b1", "g1", false)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "existing.db")
	if err = RestoreDiagnostic(ctx, dest, d); err != nil {
		t.Fatal(err)
	}
	if err = RestoreDiagnostic(ctx, dest, d); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected populated destination refusal, got %v", err)
	}
}

func TestProvenanceRenderingIncludesLearnedEventAndUsage(t *testing.T) {
	v := BrainVersion{Payload: json.RawMessage(`{"version":1,"data":{}}`), Lineage: Lineage{Component: Component{Payload: json.RawMessage(`{"version":1,"data":{}}`)}}, Provenance: Component{Payload: json.RawMessage(`{"version":1,"data":{"source":"auto","learned_event_id":"e7","learned_sequence":42,"rule_usage":{"3":9}}}`)}}
	usage, refs := metadataReferences(v)
	if !strings.Contains(strings.Join(refs, ","), "learned_event_id=e7") || !strings.Contains(strings.Join(refs, ","), "learned_sequence=42") {
		t.Fatalf("learned provenance refs=%v", refs)
	}
	if !strings.Contains(strings.Join(usage, ","), "rule_usage.3=9") {
		t.Fatalf("rule usage=%v", usage)
	}
}

func TestAPIReaderMatchesSQLiteBrain(t *testing.T) {
	ctx := context.Background()
	source := testStore(t)
	sqlr, err := OpenSQLite(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlr.Close()
	want, err := sqlr.Brain(ctx, "b1")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("content-type", "application/json")
		var body any
		switch req.URL.Path {
		case "/api/v1/brains/b1":
			body = want.Brain
		case "/api/v1/brains/b1/inspect":
			body = map[string]any{"versions": want.Versions, "total": len(want.Versions)}
		default:
			http.NotFound(w, req)
			return
		}
		raw, _ := Versioned(body)
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	api := NewAPIReader(server.URL)
	got, err := api.Brain(ctx, "b1")
	if err != nil {

		t.Fatal(err)
	}
	if got.Brain.ID != want.Brain.ID || len(got.Versions) != len(want.Versions) || got.Versions[0].ID != want.Versions[0].ID {
		t.Fatalf("API=%+v SQLite=%+v", got, want)
	}
}
func TestAPIReaderDiffResolvesBrainRoute(t *testing.T) {
	ctx := context.Background()
	source := testStore(t)
	r, err := OpenSQLite(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.BrainVersion(ctx, "v1")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("content-type", "application/json")
		var body any
		switch req.URL.Path {
		case "/api/v1/brain-versions/v1":
			body = v
		case "/api/v1/brains/b1/diff":
			if req.URL.Query().Get("from") != "v1" || req.URL.Query().Get("to") != "v1" {
				t.Errorf("unexpected diff query: %s", req.URL.RawQuery)
			}
			body = BrainDiff{From: v, To: v}
		default:
			http.NotFound(w, req)
			return
		}
		raw, _ := Versioned(body)
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	d, err := NewAPIReader(server.URL).Diff(ctx, "v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if d.From.BrainID != "b1" || d.To.BrainID != "b1" {
		t.Fatalf("unexpected diff: %+v", d)
	}
}
