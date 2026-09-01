package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"worms.ng/internal/protocol"
)

// HTTPDoer is deliberately narrow so local fixtures can provide a deterministic
// in-memory HTTP implementation.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type OpenAIConfig struct {
	Endpoint   string
	APIKey     string
	Model      string
	HTTPClient HTTPDoer
	Timeout    time.Duration
}

// OpenAIAdapter speaks the OpenAI chat-completions shape used by compatible
// servers. The API key is held only in the request header and is never put in
// provenance or errors.
type OpenAIAdapter struct {
	endpoint string
	apiKey   string
	model    string
	client   HTTPDoer
	timeout  time.Duration
}

func NewOpenAIAdapter(config OpenAIConfig) (*OpenAIAdapter, error) {
	if strings.TrimSpace(config.Endpoint) == "" || strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("OpenAI endpoint and model are required")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	return &OpenAIAdapter{endpoint: config.Endpoint, apiKey: config.APIKey, model: config.Model, client: config.HTTPClient, timeout: config.Timeout}, nil
}

func (a *OpenAIAdapter) Name() string { return "openai-compatible" }

func (a *OpenAIAdapter) Provenance() map[string]string {
	config := a.model + "\x00" + redactEndpoint(a.endpoint)
	hash := sha256.Sum256([]byte(config))
	return map[string]string{"policy": a.Name(), "model": a.model, "endpoint": redactEndpoint(a.endpoint), "config_sha256": hex.EncodeToString(hash[:])}
}

func redactEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[redacted]"
	}
	// Provenance is an audit identifier, not a request replay URL. Keep only
	// the explicit safe allowlist of scheme and host; paths, userinfo, query,
	// fragments, and force-query markers can all carry credentials.
	return parsed.Scheme + "://" + parsed.Host
}

var actionJSONSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"version", "decision_id", "action"},
	"properties": map[string]any{
		"version":     map[string]any{"type": "string", "const": protocol.SchemaVersion},
		"decision_id": map[string]any{"type": "string", "minLength": 1},
		"action": map[string]any{
			"anyOf": []any{
				map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "direction"}, "properties": map[string]any{"kind": map[string]any{"const": string(protocol.ActionMove)}, "direction": map[string]any{"type": "integer", "minimum": 0, "maximum": 5}}},
				map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind"}, "properties": map[string]any{"kind": map[string]any{"const": string(protocol.ActionResign)}}},
			},
		},
	},
}

func (a *OpenAIAdapter) Decide(ctx context.Context, request protocol.DecisionRequest) (action protocol.Action, err error) {
	if err := request.Validate(); err != nil {
		return protocol.Action{}, fmt.Errorf("invalid LLM request: %w", err)
	}
	payload := map[string]any{
		"model":           a.model,
		"messages":        []map[string]string{{"role": "system", "content": "Return exactly one Worms decision JSON object."}, {"role": "user", "content": string(mustJSON(request))}},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "worms_decision", "strict": true, "schema": actionJSONSchema}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return protocol.Action{}, fmt.Errorf("encode LLM request: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return protocol.Action{}, fmt.Errorf("create LLM request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return protocol.Action{}, fmt.Errorf("LLM request failed: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close LLM response: %w", closeErr)
		}
	}()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return protocol.Action{}, fmt.Errorf("read LLM response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return protocol.Action{}, fmt.Errorf("LLM returned HTTP %d", resp.StatusCode)
	}
	if err := protocol.RejectDuplicateFields(responseBody); err != nil {
		return protocol.Action{}, errors.New("LLM response contains duplicate JSON fields")
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return protocol.Action{}, errors.New("LLM response is not valid JSON")
	}
	if len(envelope.Choices) != 1 || len(envelope.Choices[0].Message.Content) == 0 {
		return protocol.Action{}, errors.New("LLM response must contain exactly one choice with content")
	}
	var content string
	if err := json.Unmarshal(envelope.Choices[0].Message.Content, &content); err != nil {
		return protocol.Action{}, errors.New("LLM content must be a JSON string")
	}
	response, err := protocol.DecodeDecisionResponse([]byte(content))
	if err != nil {
		return protocol.Action{}, fmt.Errorf("LLM decision failed strict validation: %w", err)
	}
	if response.DecisionID != request.DecisionID {
		return protocol.Action{}, errors.New("LLM decision ID does not match request")
	}
	return response.Action, nil
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("null")
	}
	return encoded
}

// RequestProvenance returns non-secret identifiers useful to an audit log.
func (a *OpenAIAdapter) RequestProvenance(request protocol.DecisionRequest) map[string]string {
	encoded := mustJSON(request)
	digest := sha256.Sum256(encoded)
	return map[string]string{"policy": a.Name(), "model": a.model, "request_sha256": hex.EncodeToString(digest[:])}
}
