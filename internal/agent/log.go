package agent

import (
	"strings"
	"sync"
	"time"

	"worms.ng/internal/protocol"
)

// LogEntry is an append-only, credential-free provenance record. Provenance is
// limited to policy metadata supplied by the policy itself. Response fields
// are validated protocol values; raw payloads and credentials are never stored.
type LogEntry struct {
	Version        string               `json:"version"`
	At             time.Time            `json:"at"`
	Event          string               `json:"event"`
	GameID         string               `json:"game_id"`
	WormID         string               `json:"worm_id"`
	WormInstanceID string               `json:"worm_instance_id,omitempty"`
	BrainID        string               `json:"brain_id,omitempty"`
	BrainVersion   string               `json:"brain_version,omitempty"`
	ObservationKey string               `json:"observation_key,omitempty"`
	Scores         map[string]int       `json:"scores,omitempty"`
	DecisionID     string               `json:"decision_id"`
	Outcome        protocol.OutcomeKind `json:"outcome"`
	Action         *protocol.Action     `json:"action,omitempty"`
	Latency        time.Duration        `json:"latency,omitempty"`
	Reason         string               `json:"reason,omitempty"`
	Policy         string               `json:"policy,omitempty"`
	Provenance     map[string]string    `json:"provenance,omitempty"`
}

// Logger receives structured session records. Implementations must not block
// decision handling indefinitely.
type Logger interface{ Record(LogEntry) }

type NopLogger struct{}

func (NopLogger) Record(LogEntry) {}

// MemoryLogger is deterministic and useful for local fixture tests.
type MemoryLogger struct {
	mu      sync.Mutex
	entries []LogEntry
}

func NewMemoryLogger() *MemoryLogger { return &MemoryLogger{} }

func (l *MemoryLogger) Record(entry LogEntry) {
	entry.Provenance = copyStrings(entry.Provenance)
	entry.Scores = copyScores(entry.Scores)
	if entry.Action != nil {
		action := *entry.Action
		entry.Action = &action
	}
	l.mu.Lock()
	l.entries = append(l.entries, entry)
	l.mu.Unlock()
}

func (l *MemoryLogger) Entries() []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := make([]LogEntry, len(l.entries))
	copy(entries, l.entries)
	for i := range entries {
		entries[i].Provenance = copyStrings(entries[i].Provenance)
		entries[i].Scores = copyScores(entries[i].Scores)
		if entries[i].Action != nil {
			action := *entries[i].Action
			entries[i].Action = &action
		}
	}
	return entries
}

func copyStrings(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	copyValues := make(map[string]string, len(values))
	for key, value := range values {
		copyValues[key] = value
	}
	return copyValues
}

func copyScores(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	copyValues := make(map[string]int, len(values))
	for key, value := range values {
		copyValues[key] = value
	}
	return copyValues
}

// RedactCredential is intended for diagnostics. It never returns the full
// secret, even when a caller supplies a short token.
func RedactCredential(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "[redacted]"
	}
	return token[:4] + "…" + token[len(token)-4:]
}
