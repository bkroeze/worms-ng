package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthUsesVersionedEndpointAndDecodesSQLiteStatus(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v1","status":"ok","demo":{"message":"ready","database":"sqlite","recorded_checks":3}}`))
	}))
	defer srv.Close()
	h, seq, err := NewHTTPClient(srv.URL).Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if seq == 0 {
		t.Fatal("missing request sequence")
	}
	if gotPath != "/api/v1/health" {
		t.Fatalf("path %q", gotPath)
	}
	if h.Database != "sqlite" || h.RecordedChecks != 3 {
		t.Fatalf("health %+v", h)
	}
}
func TestUnknownVersionFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"version":"v9","status":"ok"}`)) }))
	defer srv.Close()
	_, _, err := NewHTTPClient(srv.URL).Health(context.Background())
	if err == nil {
		t.Fatal("unknown API version accepted")
	}
}
func TestPathEscapeUsesUTF8Bytes(t *testing.T) {
	if got := urlPathEscape("café/brain"); got != "caf%C3%A9%2Fbrain" {
		t.Fatalf("escaped id %q", got)
	}
}
func TestResourceGenerationsDoNotCrossInvalidate(t *testing.T) {
	c := NewHTTPClient("")
	games := c.next(resourceGames)
	_ = c.next(resourceHealth)
	if !c.IsCurrentFor(resourceGames, games) {
		t.Fatal("health request invalidated games response")
	}
}
func TestInspectorDecodesAuthoritativeRulePage(t *testing.T) {
	var got struct {
		Version string `json:"version"`
		InspectorResult
	}
	raw := `{"version":"v1","brain_id":"b","version_id":"v2","version_number":2,"rules":[{"mask":5,"action":2}],"offset":12,"limit":12,"total":25,"next_offset":24}`
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "v1" || got.InspectorResult.Version != 2 || got.VersionID != "v2" || len(got.Rules) != 1 || got.Rules[0].Mask != 5 || got.NextOffset != 24 {
		t.Fatalf("decoded inspection page %+v", got)
	}
}
