package agent

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"worms.ng/internal/protocol"
)

func agentObservation(id string) protocol.Observation {
	return protocol.Observation{
		Version: protocol.SchemaVersion, GameID: "g", WormID: "w", DecisionID: id, Position: protocol.Position{X: 1, Y: 1},
		Neighbors:    []protocol.Neighbor{{Direction: 0, Position: protocol.Position{X: 1, Y: 1}}, {Direction: 1, Position: protocol.Position{X: 1, Y: 1}}, {Direction: 2, Position: protocol.Position{X: 1, Y: 1}}, {Direction: 3, Position: protocol.Position{X: 1, Y: 1}}, {Direction: 4, Position: protocol.Position{X: 1, Y: 1}}, {Direction: 5, Position: protocol.Position{X: 1, Y: 1}}},
		TrailStates:  []protocol.TrailState{protocol.TrailEmpty, protocol.TrailEmpty, protocol.TrailEmpty, protocol.TrailEmpty, protocol.TrailEmpty, protocol.TrailEmpty},
		LegalActions: []protocol.Action{{Kind: protocol.ActionMove, Direction: 2}, {Kind: protocol.ActionMove, Direction: 4}, {Kind: protocol.ActionResign}},
		Mode:         "test", Deadline: time.Now().Add(time.Minute),
	}
}

func agentRequest(id string) protocol.DecisionRequest {
	observation := agentObservation(id)
	return protocol.DecisionRequest{Version: protocol.SchemaVersion, DecisionID: id, Observation: observation, Deadline: time.Now().Add(time.Minute)}
}

func TestSeededRandomPolicyIsReproducible(t *testing.T) {
	first := NewRandomPolicy(42)
	second := NewRandomPolicy(42)
	request := agentRequest("d")
	for i := 0; i < 8; i++ {
		a, err := first.Decide(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		b, err := second.Decide(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("seeded policies diverged at %d: %#v %#v", i, a, b)
		}
	}
}
func TestScriptedPolicyChecksPatternKey(t *testing.T) {
	policy, err := NewScriptedPolicyWithKeys(ScriptStep{ExpectedPatternKey: "pattern-a", Action: protocol.Action{Kind: protocol.ActionMove, Direction: 2}})
	if err != nil {
		t.Fatal(err)
	}
	request := agentRequest("script-key")
	request.Observation.PatternKey = "wrong"
	if _, err := policy.Decide(context.Background(), request); err == nil || !strings.Contains(err.Error(), "pattern mismatch") {
		t.Fatalf("got %v, want useful pattern mismatch", err)
	}
	request.Observation.PatternKey = "pattern-a"
	if _, err := policy.Decide(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRejectsStaleAndDuplicateAndTimesOut(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	logger := NewMemoryLogger()
	manager := NewSessionManager(SessionConfig{Now: func() time.Time { return clock }, DefaultDeadline: time.Second, NewToken: func() (string, error) { return "secret-token", nil }, Logger: logger})
	script, err := NewScriptedPolicy(protocol.Action{Kind: protocol.ActionMove, Direction: 2})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := manager.Start("g", "w", script)
	if err != nil {
		t.Fatal(err)
	}
	request := agentRequest("d1")
	request.Deadline = time.Time{}
	if err := manager.Begin(credentials, request); err != nil {
		t.Fatal(err)
	}
	stale, err := manager.Submit(credentials, protocol.DecisionResponse{Version: protocol.SchemaVersion, DecisionID: "old", Action: protocol.Action{Kind: protocol.ActionMove, Direction: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Outcome != protocol.OutcomeStale {
		t.Fatalf("got %q", stale.Outcome)
	}
	accepted, err := manager.Submit(credentials, protocol.DecisionResponse{Version: protocol.SchemaVersion, DecisionID: "d1", Action: protocol.Action{Kind: protocol.ActionMove, Direction: 2}})
	if err != nil || accepted.Outcome != protocol.OutcomeAccepted {
		t.Fatalf("accepted=%#v err=%v", accepted, err)
	}
	duplicate, err := manager.Submit(credentials, protocol.DecisionResponse{Version: protocol.SchemaVersion, DecisionID: "d1", Action: protocol.Action{Kind: protocol.ActionMove, Direction: 2}})
	if err != nil || duplicate.Outcome != protocol.OutcomeDuplicate {
		t.Fatalf("duplicate=%#v err=%v", duplicate, err)
	}
	request2 := agentRequest("d2")
	request2.Deadline = time.Time{}
	if err := manager.Begin(credentials, request2); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Second)
	timedOut, err := manager.ResolveTimeout(credentials)
	if err != nil || timedOut.Outcome != protocol.OutcomeTimeout {
		t.Fatalf("timeout=%#v err=%v", timedOut, err)
	}
	if len(logger.Entries()) < 5 {
		t.Fatal("expected structured lifecycle logs")
	}
	if strings.Contains(string(mustJSON(logger.Entries())), credentials.Token) {
		t.Fatal("credential leaked in logs")
	}
}

type fixtureClient struct {
	response      string
	authorization string
}

func (c *fixtureClient) Do(request *http.Request) (*http.Response, error) {
	c.authorization = request.Header.Get("Authorization")
	body := io.NopCloser(strings.NewReader(c.response))
	return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
}

func TestOpenAIAdapterStrictFixtureAndCredentialRedaction(t *testing.T) {
	client := &fixtureClient{response: `{"choices":[{"message":{"content":"{\"version\":\"v1\",\"decision_id\":\"d\",\"action\":{\"kind\":\"move\",\"direction\":4}}"}}]}`}
	adapter, err := NewOpenAIAdapter(OpenAIConfig{Endpoint: "https://user:password@fixture/v1/chat/completions?auth=super-secret#fragment", APIKey: "super-secret", Model: "fixture", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	action, err := adapter.Decide(context.Background(), agentRequest("d"))
	if err != nil || action.Direction != 4 {
		t.Fatalf("action=%#v err=%v", action, err)
	}
	if client.authorization != "Bearer super-secret" {
		t.Fatalf("authorization not sent correctly")
	}
	provenance := adapter.Provenance()
	if provenance["endpoint"] != "https://fixture" {
		t.Fatalf("endpoint provenance = %q, want https://fixture", provenance["endpoint"])
	}
	if strings.Contains(strings.Join([]string{provenance["model"], provenance["endpoint"]}, " "), "super-secret") {
		t.Fatal("credential leaked in provenance")
	}
}

func TestOpenAIAdapterRejectsDuplicateEnvelopeFields(t *testing.T) {
	client := &fixtureClient{response: `{"choices":[],"choices":[]}`}
	adapter, err := NewOpenAIAdapter(OpenAIConfig{Endpoint: "https://fixture.test", Model: "fixture", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Decide(context.Background(), agentRequest("duplicate")); err == nil || !strings.Contains(err.Error(), "duplicate JSON fields") {
		t.Fatalf("duplicate envelope fields accepted: %v", err)
	}
}
func TestSessionMalformedIllegalResignAndProvenance(t *testing.T) {
	logger := NewMemoryLogger()
	manager := NewSessionManager(SessionConfig{NewToken: func() (string, error) { return "session-secret", nil }, Logger: logger})
	policy, err := NewScriptedPolicy(protocol.Action{Kind: protocol.ActionMove, Direction: 2})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := manager.Start("matrix-game", "worm", policy)
	if err != nil {
		t.Fatal(err)
	}
	request := agentRequest("matrix-1")
	request.Observation.GameID = credentials.GameID
	request.Observation.WormID = "worm"
	request.Observation.WormInstanceID = "worm-instance-1"
	request.Observation.BrainID = "brain-1"
	request.Observation.BrainVersion = "brain-1-v1"
	request.Observation.PatternKey = "0xabc"
	request.Observation.Scores = map[string]int{"worm": 7}
	request.Observation.Provenance = map[string]string{"round": "3"}
	if err := manager.Begin(credentials, request); err != nil {
		t.Fatal(err)
	}
	malformed, err := manager.Submit(credentials, protocol.DecisionResponse{Version: "v2", DecisionID: "matrix-1"})
	if err != nil || malformed.Outcome != protocol.OutcomeMalformed {
		t.Fatalf("malformed=%#v err=%v", malformed, err)
	}
	illegal, err := manager.Submit(credentials, protocol.DecisionResponse{Version: protocol.SchemaVersion, DecisionID: "matrix-1", Action: protocol.Action{Kind: protocol.ActionMove, Direction: 5}})
	if err != nil || illegal.Outcome != protocol.OutcomeIllegal {
		t.Fatalf("illegal=%#v err=%v", illegal, err)
	}
	resigned, err := manager.Submit(credentials, protocol.DecisionResponse{Version: protocol.SchemaVersion, DecisionID: "matrix-1", Action: protocol.Action{Kind: protocol.ActionResign}})
	if err != nil || resigned.Outcome != protocol.OutcomeResigned {
		t.Fatalf("resigned=%#v err=%v", resigned, err)
	}
	encoded := string(mustJSON(logger.Entries()))
	if strings.Contains(encoded, credentials.Token) {
		t.Fatal("credential leaked")
	}
	if !strings.Contains(encoded, "worm-instance-1") || !strings.Contains(encoded, "0xabc") {
		t.Fatalf("provenance fields missing: %s", encoded)
	}
	entries := logger.Entries()
	if len(entries) == 0 || entries[len(entries)-1].Scores["worm"] != 7 || entries[len(entries)-1].Provenance["round"] != "3" {
		t.Fatalf("observation scores/provenance missing from logger: %#v", entries)
	}
}

func TestReferenceExternalProcessCompletesFixtureDecision(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "protocol", "testdata", "decision-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocol.DecodeDecisionRequest(fixture)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewScriptedPolicy(request.Observation.LegalActions[0])
	if err != nil {
		t.Fatal(err)
	}
	manager := NewSessionManager(SessionConfig{NewToken: func() (string, error) { return "reference-token", nil }})
	credentials, err := manager.Start(request.Observation.GameID, request.Observation.WormID, policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Begin(credentials, request); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "../../cmd/worms-agent")
	cmd.Stdin = strings.NewReader(string(fixture))
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("reference process: %v", err)
	}
	response, err := protocol.DecodeDecisionResponse(output)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := manager.Submit(credentials, response)
	if err != nil || outcome.Outcome != protocol.OutcomeAccepted {
		t.Fatalf("reference session outcome=%#v err=%v", outcome, err)
	}
	if response.DecisionID != "decision-1" || response.Action.Direction != 0 {
		t.Fatalf("unexpected reference response: %#v", response)
	}
}

func TestClosedSessionRejectsMutationsAndCancelsPending(t *testing.T) {
	logger := NewMemoryLogger()
	manager := NewSessionManager(SessionConfig{
		Now:      time.Now,
		NewToken: func() (string, error) { return "close-token", nil },
		Logger:   logger,
	})
	script, err := NewScriptedPolicy(protocol.Action{Kind: protocol.ActionMove, Direction: 2})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := manager.Start("closed-game", "w", script)
	if err != nil {
		t.Fatal(err)
	}
	request := agentRequest("close-decision")
	request.Observation.GameID = "closed-game"
	if err := manager.Begin(credentials, request); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := manager.Close(credentials); err != nil {
		t.Fatalf("close: %v", err)
	}
	if entries := logger.Entries(); len(entries) == 0 || entries[len(entries)-1].Reason != "session closed" {
		t.Fatalf("pending decision was not cancelled in the lifecycle log: %#v", entries)
	}
	response := protocol.DecisionResponse{
		Version: protocol.SchemaVersion, DecisionID: "close-decision",
		Action: protocol.Action{Kind: protocol.ActionMove, Direction: 2},
	}
	if _, err := manager.Submit(credentials, response); err != ErrSession {
		t.Fatalf("submit after close error = %v, want %v", err, ErrSession)
	}
	if _, err := manager.ResolveTimeout(credentials); err != ErrSession {
		t.Fatalf("timeout after close error = %v, want %v", err, ErrSession)
	}
	if _, err := manager.Disconnect(credentials, "late"); err != ErrSession {
		t.Fatalf("disconnect after close error = %v, want %v", err, ErrSession)
	}
	if err := manager.Begin(credentials, agentRequest("late-begin")); err != ErrSession {
		t.Fatalf("begin after close error = %v, want %v", err, ErrSession)
	}
	if err := manager.Close(credentials); err != ErrSession {
		t.Fatalf("second close error = %v, want %v", err, ErrSession)
	}
}

type blockingPolicy struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingPolicy) Name() string { return "blocking" }

func (p *blockingPolicy) Decide(_ context.Context, _ protocol.DecisionRequest) (protocol.Action, error) {
	close(p.entered)
	<-p.release
	return protocol.Action{Kind: protocol.ActionMove, Direction: 2}, nil
}

func TestRequestCloseRaceRejectsDecisionAfterClose(t *testing.T) {
	policy := &blockingPolicy{entered: make(chan struct{}), release: make(chan struct{})}
	manager := NewSessionManager(SessionConfig{NewToken: func() (string, error) { return "race-token", nil }})
	credentials, err := manager.Start("race-game", "w", policy)
	if err != nil {
		t.Fatal(err)
	}
	request := agentRequest("race-decision")
	request.Observation.GameID = "race-game"
	result := make(chan error, 1)
	go func() {
		_, requestErr := manager.Request(context.Background(), credentials, request)
		result <- requestErr
	}()
	<-policy.entered
	if err := manager.Close(credentials); err != nil {
		t.Fatalf("close during request: %v", err)
	}
	close(policy.release)
	if err := <-result; err != ErrSession {
		t.Fatalf("request after close error = %v, want %v", err, ErrSession)
	}
}

func TestRedactEndpointRemovesAllReplayComponents(t *testing.T) {
	got := redactEndpoint("https://user:password@example.test/v1?auth=secret&x=also-secret#fragment")
	if got != "https://example.test" {
		t.Fatalf("redacted endpoint = %q, want %q", got, "https://example.test")
	}
	for _, secret := range []string{"user", "password", "/v1", "auth", "secret", "fragment"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted endpoint contains %q: %q", secret, got)
		}
	}
	if got := redactEndpoint("not a URL"); got != "[redacted]" {
		t.Fatalf("malformed endpoint = %q, want [redacted]", got)
	}
}
