// Command worms-agent is the reference synchronous external process. It reads
// one v1 DecisionRequest JSON object from stdin and writes one DecisionResponse
// JSON object to stdout. It has no engine or repository dependency.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"worms.ng/internal/protocol"
)

func main() {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail(err)
	}
	request, err := protocol.DecodeDecisionRequest(input)
	if err != nil {
		fail(err)
	}
	if len(request.Observation.LegalActions) == 0 {
		fail(fmt.Errorf("request has no legal actions"))
	}
	// Deterministic reference policy: choose the first legal action, preserving
	// the order declared by the engine adapter. No hidden board state is read.
	response := protocol.DecisionResponse{Version: protocol.SchemaVersion, DecisionID: request.DecisionID, Action: request.Observation.LegalActions[0]}
	encoded, err := json.Marshal(response)
	if err != nil {
		fail(err)
	}
	_, err = os.Stdout.Write(append(encoded, '\n'))
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
