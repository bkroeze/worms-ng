package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFixtureRoundTripPreservesEveryObservationField(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "decision-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request DecisionRequest
	if err := DecodeStrict(raw, &request); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip DecisionRequest
	if err := DecodeStrict(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request, roundTrip) {
		t.Fatalf("request changed after strict JSON round trip: %#v != %#v", request, roundTrip)
	}
}

func fixtureObservation() Observation {
	return Observation{
		Version: SchemaVersion, GameID: "game-1", WormID: "worm-1", DecisionID: "decision-1",
		Position: Position{X: 1, Y: 1}, Orientation: 0,
		Neighbors:    []Neighbor{{Direction: 0, Position: Position{X: 1, Y: 1}}, {Direction: 1, Position: Position{X: 1, Y: 1}}, {Direction: 2, Position: Position{X: 1, Y: 1}}, {Direction: 3, Position: Position{X: 1, Y: 1}}, {Direction: 4, Position: Position{X: 1, Y: 1}}, {Direction: 5, Position: Position{X: 1, Y: 1}}},
		TrailStates:  []TrailState{TrailEmpty, TrailEmpty, TrailEmpty, TrailEmpty, TrailEmpty, TrailEmpty},
		LegalActions: []Action{{Kind: ActionMove, Direction: 0}, {Kind: ActionMove, Direction: 1}, {Kind: ActionResign}},
		Scores:       map[string]int{"worm-1": 0}, Mode: "test", Deadline: time.Now().Add(time.Second),
	}
}

func TestStrictDecisionResponseRejectsUnknownVersionAndFields(t *testing.T) {
	valid := `{"version":"v1","decision_id":"d","action":{"kind":"move","direction":2}}`
	if _, err := DecodeDecisionResponse([]byte(valid)); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	if _, err := DecodeDecisionResponse([]byte(strings.Replace(valid, `"v1"`, `"v2"`, 1))); err == nil {
		t.Fatal("unknown version accepted")
	}
	if _, err := DecodeDecisionResponse([]byte(`{"version":"v1","decision_id":"d","action":{"kind":"move","direction":2,"extra":true}}`)); err == nil {
		t.Fatal("unknown action field accepted")
	}
	if _, err := DecodeDecisionResponse([]byte(valid + " {}")); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestObservationRequiresSixDirectionsAndLegalActions(t *testing.T) {
	observation := fixtureObservation()
	if err := observation.Validate(); err != nil {
		t.Fatal(err)
	}
	observation.LegalActions[0].Direction = 6
	if err := observation.Validate(); err == nil {
		t.Fatal("invalid direction accepted")
	}
}

func TestObservationRequiresExactlySixTrailStates(t *testing.T) {
	for _, length := range []int{0, 5, 7} {
		observation := fixtureObservation()
		observation.TrailStates = make([]TrailState, length)
		if err := observation.Validate(); err == nil {
			t.Fatalf("trail state length %d accepted", length)
		}
	}
}

func TestObservationTrailStatesUseCanonicalDirectionOrder(t *testing.T) {
	observation := fixtureObservation()
	observation.TrailStates = []TrailState{TrailEmpty, TrailOwn, TrailOther, TrailEmpty, TrailOwn, TrailOther}
	if err := observation.Validate(); err != nil {
		t.Fatalf("canonical trail states rejected: %v", err)
	}
}

func TestModernCoordinatesAndConfigurableBounds(t *testing.T) {
	observation := fixtureObservation()
	observation.Position = Position{X: 0, Y: 0}
	for i := range observation.Neighbors {
		observation.Neighbors[i].Position = Position{X: -1, Y: 0}
	}
	if err := observation.Validate(); err != nil {
		t.Fatalf("zero-based bounded coordinates rejected: %v", err)
	}
	observation.Position = Position{X: 8, Y: 4}
	for i := range observation.Neighbors {
		observation.Neighbors[i].Position = Position{X: 8, Y: 4}
	}
	bounds := CoordinateBounds{MinX: 0, MaxX: 8, MinY: 0, MaxY: 4}
	if err := observation.ValidateWith(bounds); err != nil {
		t.Fatalf("configured coordinate bounds rejected: %v", err)
	}
	observation.Position.X = 9
	if err := observation.ValidateWith(bounds); err == nil {
		t.Fatal("coordinate outside configured bounds accepted")
	}
	observation.Position.X = DefaultCoordinateBounds.MaxX + 1
	if err := observation.Validate(); err == nil {
		t.Fatal("unbounded coordinate accepted")
	}
}

func TestStrictDecodersRejectDuplicateFieldsAtEveryObjectDepth(t *testing.T) {
	response := `{"version":"v1","decision_id":"d","decision_id":"other","action":{"kind":"move","direction":2}}`
	if _, err := DecodeDecisionResponse([]byte(response)); err == nil {
		t.Fatal("duplicate top-level field accepted")
	}
	response = `{"version":"v1","decision_id":"d","action":{"kind":"move","direction":2,"direction":3}}`
	if _, err := DecodeDecisionResponse([]byte(response)); err == nil {
		t.Fatal("duplicate nested field accepted")
	}
}
