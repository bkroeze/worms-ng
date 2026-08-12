// Package server owns the native HTTP service and its SQLite database.
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"worms.ng/internal/engine"
	"worms.ng/internal/protocol"
	"worms.ng/internal/store"
)

const (
	maxRequestBytesDefault int64 = 1 << 20
	maxParticipantsDefault       = 4
	maxRulesDefault              = 64 * 1024
)

// BuildMetadata is the diagnostic version tuple exposed by /build.
type BuildMetadata struct {
	GoVersion     string `json:"go_version"`
	GioVersion    string `json:"gio_version"`
	AppVersion    string `json:"app_version"`
	APIVersion    string `json:"api_version"`
	SchemaVersion string `json:"schema_version"`
}

// Service is an HTTP service with an exclusively server-owned SQLite handle.
type Service struct {
	db              *sql.DB
	data            *store.Store
	assets          fs.FS
	origins         map[string]bool
	mu              sync.Mutex
	maxRequestBytes int64
	maxParticipants int
	maxRules        int
}

// Options configures HTTP policy. An empty origin list disables cross-origin
// requests; the special value * allows all origins (only use deliberately).
type Options struct {
	CORSOrigins     []string
	MaxRequestBytes int64
	MaxParticipants int
	MaxRules        int
}

// Open opens (or creates) the SQLite database and initializes its schema. The
// returned service must be closed by its owner.
func Open(databasePath string, assets fs.FS) (*Service, error) {
	origins := strings.FieldsFunc(os.Getenv("WORMS_CORS_ORIGINS"), func(r rune) bool { return r == ',' || r == ' ' })
	return OpenWithOptions(databasePath, assets, Options{CORSOrigins: origins})
}

func OpenWithOptions(databasePath string, assets fs.FS, options Options) (*Service, error) {
	if assets == nil {
		return nil, errors.New("static assets are required")
	}
	d, err := store.Open(context.Background(), databasePath)
	if err != nil {
		return nil, err
	}
	requestLimit := options.MaxRequestBytes
	if requestLimit <= 0 {
		requestLimit = maxRequestBytesDefault
	}
	participantLimit := options.MaxParticipants
	if participantLimit <= 0 {
		participantLimit = maxParticipantsDefault
	}
	ruleLimit := options.MaxRules
	if ruleLimit <= 0 {
		ruleLimit = maxRulesDefault
	}
	s := &Service{db: d.DB(), data: d, assets: assets, origins: map[string]bool{}, maxRequestBytes: requestLimit, maxParticipants: participantLimit, maxRules: ruleLimit}
	for _, origin := range options.CORSOrigins {
		s.origins[strings.TrimSpace(origin)] = true
	}
	if _, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS server_checks (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		d.Close()
		return nil, err
	}
	if _, err = s.db.Exec(`INSERT INTO server_checks DEFAULT VALUES`); err != nil {
		d.Close()
		return nil, err
	}
	return s, nil
}

func (s *Service) Close() error {
	if s == nil || s.data == nil {
		return nil
	}
	return s.data.Close()
}

// Handler returns the complete API and embedded static asset handler.
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.route) }

func (s *Service) route(w http.ResponseWriter, r *http.Request) {
	w = &requestWriter{ResponseWriter: w, ifNoneMatch: r.Header.Get("If-None-Match")}
	if !s.originAllowed(r) {
		writeAPIError(w, http.StatusForbidden, "origin_forbidden", "origin is not allowed", nil)
		return
	}
	s.cors(w, r)
	if r.Method == http.MethodOptions {
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/api/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/") &&
		r.Method != http.MethodGet && r.Method != http.MethodOptions {
		ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
		if ct != "application/json" {
			writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "mutating API requests require Content-Type: application/json", nil)
			return
		}
	}
	if r.URL.Path == "/" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.serveIndex(w)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/"+protocol.APIVersion+"/") && r.URL.Path != "/api/"+protocol.APIVersion {
		writeAPIError(w, http.StatusNotFound, "not_found", "unsupported API version", nil)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/"+protocol.APIVersion) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.serveAssetOrIndex(w, r.URL.Path)
		return
	}
	p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"+protocol.APIVersion), "/")
	if p == "health" {
		s.health(w, r)
		return
	}
	if p == "build" || p == "metadata" || p == "schema" {
		s.metadata(w, r, p)
		return
	}
	parts := splitPath(p)
	if len(parts) == 0 {
		writeAPIError(w, http.StatusNotFound, "not_found", "endpoint not found", nil)
		return
	}
	switch parts[0] {
	case "demo":
		s.demo(w, r)
	case "games":
		s.gamesRoute(w, r, parts[1:])
	case "brains":
		s.brainsRoute(w, r, parts[1:])
	case "brain-versions":
		s.brainVersionRoute(w, r, parts[1:])
	case "tournaments":
		s.tournamentsRoute(w, r, parts[1:])
	case "matches":
		s.matchesRoute(w, r, parts[1:])
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "endpoint not found", nil)
	}
}
func (s *Service) originAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || s.origins["*"] || s.origins[origin] {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host == r.Host &&
		(parsed.Scheme == "http" || parsed.Scheme == "https")
}
func (s *Service) serveAssetOrIndex(w http.ResponseWriter, requestPath string) {
	clean := path.Clean("/" + requestPath)
	if strings.Contains(requestPath, "..") || clean != "/"+strings.TrimPrefix(requestPath, "/") {
		writeAPIError(w, http.StatusNotFound, "not_found", "asset not found", nil)
		return
	}
	name := strings.TrimPrefix(clean, "/")
	if name == "" {
		s.serveIndex(w)
		return
	}
	if _, err := fs.Stat(s.assets, name); err == nil {
		if strings.HasSuffix(name, ".wasm") {
			w.Header().Set("Content-Type", "application/wasm")
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		} else if strings.HasSuffix(name, ".js") {
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		}
		http.FileServer(http.FS(s.assets)).ServeHTTP(w, &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/" + name}})
		return
	}
	if strings.Contains(path.Base(name), ".") {
		writeAPIError(w, http.StatusNotFound, "not_found", "asset not found", nil)
		return
	}
	// Client-side routes are deep links into the WASM shell.
	s.serveIndex(w)
}
func splitPath(p string) []string {
	var out []string
	for _, x := range strings.Split(p, "/") {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func validHTTPID(id string) bool {
	return id != "" && len(id) <= 128 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, `/\`)
}

type requestWriter struct {
	http.ResponseWriter
	ifNoneMatch string
}

func (s *Service) cors(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" && (s.origins["*"] || s.origins[origin]) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, If-Match, X-Expected-Cursor, X-Expected-Version")
		w.Header().Set("Access-Control-Max-Age", "600")
	}
}
func (s *Service) serveIndex(w http.ResponseWriter) {
	b, err := fs.ReadFile(s.assets, "index.html")
	if err != nil {
		writeAPIError(w, 500, "asset_unavailable", "index unavailable", nil)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	var checks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM server_checks`).Scan(&checks); err != nil {
		serverError(w, err)
		return
	}
	writeVersioned(w, http.StatusOK, protocol.Health{Version: protocol.APIVersion, Status: "ok", Demo: protocol.Demo{Message: "Worms service is ready", Database: "sqlite", RecordedChecks: checks}})
}
func (s *Service) demo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM server_checks`).Scan(&n); err != nil {
		serverError(w, err)
		return
	}
	writeVersioned(w, 200, protocol.DemoResponse{Version: protocol.APIVersion, Demo: protocol.Demo{Message: "Embedded WASM client can use this endpoint", Database: "sqlite", RecordedChecks: n}})
}
func (s *Service) metadata(w http.ResponseWriter, r *http.Request, kind string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	sv, _ := s.data.SchemaVersion(r.Context())
	meta := buildMetadata()
	payload := map[string]any{
		"version": protocol.APIVersion, "service_version": protocol.ServiceVersion,
		"schema_version": sv, "schema": protocol.SchemaVersion,
		"build": meta,
	}
	if kind == "schema" {
		payload["schema"] = protocol.SchemaVersion
	}
	writeVersioned(w, 200, payload)
}
func buildMetadata() BuildMetadata {
	meta := BuildMetadata{GoVersion: runtime.Version(), GioVersion: "v0.10.2", AppVersion: protocol.ServiceVersion, APIVersion: protocol.APIVersion, SchemaVersion: protocol.SchemaVersion}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "gioui.org" {
				meta.GioVersion = dep.Version
				break
			}
		}
	}
	return meta
}

func readJSON(r *http.Request, dst any) error {
	return readJSONLimit(r, dst, maxRequestBytesDefault)
}
func (s *Service) readJSON(r *http.Request, dst any) error {
	return readJSONLimit(r, dst, s.maxRequestBytes)
}
func readJSONLimit(r *http.Request, dst any, limit int64) error {
	if r.Body == nil {
		return errors.New("request body is required")
	}
	if r.ContentLength > limit {
		return fmt.Errorf("request body exceeds %d byte limit", limit)
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if int64(len(raw)) > limit {
		return fmt.Errorf("request body exceeds %d byte limit", limit)
	}
	if err := rejectDuplicateJSON(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("JSON must contain one value")
	}
	return nil
}
func rejectDuplicateJSON(raw []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := walkJSON(dec); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("JSON must contain one value")
	}
	return nil
}
func walkJSON(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch x := tok.(type) {
	case json.Delim:
		switch x {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if seen[name] {
					return fmt.Errorf("duplicate JSON field %q", name)
				}
				seen[name] = true
				if err := walkJSON(dec); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case '[':
			for dec.More() {
				if err := walkJSON(dec); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
	case nil, bool, string, float64, json.Number:
		_ = x
	}
	return nil
}
func versionOK(v string) error {
	if v != protocol.APIVersion {
		return fmt.Errorf("unsupported API version %q", v)
	}
	return nil
}
func parsePage(r *http.Request) (store.PageOptions, error) {
	limit, offset := 100, 0
	var err error
	if x := r.URL.Query().Get("limit"); x != "" {
		limit, err = strconv.Atoi(x)
		if err != nil || limit < 1 || limit > 1000 {
			return store.PageOptions{}, errors.New("limit must be 1..1000")
		}
	}
	if x := r.URL.Query().Get("offset"); x != "" {
		offset, err = strconv.Atoi(x)
		if err != nil || offset < 0 {
			return store.PageOptions{}, errors.New("offset must be non-negative")
		}
	}
	return store.PageOptions{Limit: limit, Offset: offset}, nil
}
func requestPayload(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return store.EncodePayload(map[string]any{})
	}
	var e struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Version == nil || *e.Version != 1 {
		return nil, errors.New("payload version must be 1")
	}
	return raw, nil
}
func payloadData(raw json.RawMessage) json.RawMessage {
	var e struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &e) == nil && len(e.Data) > 0 {
		return e.Data
	}
	return raw
}
func teachingDecision(in gameActionRequest) (string, uint8, uint64, int, json.RawMessage, error) {
	var d struct {
		WormID         string  `json:"worm_id"`
		Mask           *uint8  `json:"mask"`
		PendingMask    *uint8  `json:"pending_mask"`
		Request        *uint64 `json:"request"`
		PendingRequest *uint64 `json:"pending_request"`
		Direction      *int    `json:"direction"`
	}
	if len(in.Payload) > 0 {
		raw, err := requestPayload(in.Payload)
		if err != nil {
			return "", 0, 0, 0, nil, err
		}
		if err := json.Unmarshal(payloadData(raw), &d); err != nil {
			return "", 0, 0, 0, nil, errors.New("teaching payload must be an object")
		}
	}
	wormID := in.WormID
	if wormID == "" {
		wormID = d.WormID
	}
	mask := in.Mask
	if mask == nil {
		mask = in.PendingMask
	}
	if mask == nil {
		mask = d.Mask
	}
	if mask == nil {
		mask = d.PendingMask
	}
	request := in.Request
	if request == nil {
		request = in.PendingRequest
	}
	if request == nil {
		request = d.Request
	}
	if request == nil {
		request = d.PendingRequest
	}
	direction := in.Direction
	if direction == nil {
		direction = d.Direction
	}
	if wormID == "" || mask == nil || request == nil || direction == nil {
		return "", 0, 0, 0, nil, errors.New("worm_id, mask, request, and direction are required")
	}
	if *mask > 63 {
		return "", 0, 0, 0, nil, errors.New("teaching mask must be 0..63")
	}
	if *direction < 0 || *direction > int(engine.NorthEast) {
		return "", 0, 0, 0, nil, errors.New("teaching direction must be 0..5")
	}
	canonical, err := store.EncodePayload(map[string]any{
		"worm_id": wormID, "mask": *mask, "request": *request, "direction": *direction,
	})
	if err != nil {
		return "", 0, 0, 0, nil, err
	}
	return wormID, *mask, *request, *direction, canonical, nil
}

func writeVersioned(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	b, err := json.Marshal(value)
	if err != nil {
		serverError(w, err)
		return
	}
	if status >= 200 && status < 300 {
		etag := `"` + hash(b) + `"`
		w.Header().Set("ETag", etag)
		if rw, ok := w.(*requestWriter); ok && rw.ifNoneMatch == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func writeAPIError(w http.ResponseWriter, status int, code, message string, details any) {
	writeVersioned(w, status, map[string]any{"version": protocol.APIVersion, "error": map[string]any{"code": code, "message": message, "details": details}})
}
func methodNotAllowed(w http.ResponseWriter, allow ...string) {
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writeAPIError(w, 405, "method_not_allowed", "method not allowed", nil)
}
func serverError(w http.ResponseWriter, err error) {
	_ = err
	writeAPIError(w, 500, "internal_error", "database unavailable", nil)
}
func mapStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, 404, "not_found", "resource not found", nil)
	case errors.Is(err, store.ErrConflict):
		writeAPIError(w, 409, "conflict", "resource changed; retry with current cursor", nil)
	case errors.Is(err, store.ErrInvalidArgument), errors.Is(err, store.ErrInvalidPayload):
		writeAPIError(w, 400, "invalid_request", err.Error(), nil)
	case errors.Is(err, store.ErrCorruptPayload), errors.Is(err, store.ErrCorruptEvent):
		writeAPIError(w, 422, "corrupt_persistence", "persisted state failed integrity verification", nil)
	case errors.Is(err, store.ErrImmutable):
		writeAPIError(w, 409, "immutable", "resource is immutable", nil)
	case errors.Is(err, store.ErrConstraint):
		writeAPIError(w, 409, "constraint_violation", "resource violates a persistence constraint", nil)
	case errors.Is(err, store.ErrCanceled):
		writeAPIError(w, 499, "canceled", "request canceled", nil)
	case errors.Is(err, store.ErrMigration):
		writeAPIError(w, 500, "migration_failed", "database migration failed", nil)
	default:
		serverError(w, err)
	}
}

// Game transport types intentionally retain the stable cursor and hash fields.
type participantRequest struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	BrainVersionID string `json:"brain_version_id"`
	Kind           string `json:"kind"`
	Color          string `json:"color"`
	Start          *struct {
		Q int `json:"q"`
		R int `json:"r"`
	} `json:"start"`
	Score   int64           `json:"score"`
	Payload json.RawMessage `json:"payload"`
}
type gameRequest struct {
	Version        string               `json:"version"`
	ID             string               `json:"id"`
	BrainVersionID string               `json:"brain_version_id"`
	Status         string               `json:"status"`
	Ruleset        string               `json:"ruleset"`
	Width          int                  `json:"width"`
	Height         int                  `json:"height"`
	Rules          json.RawMessage      `json:"rules"`
	RulesPayload   json.RawMessage      `json:"rules_payload"`
	Seed           int64                `json:"seed"`
	Participants   []participantRequest `json:"participants"`
	State          json.RawMessage      `json:"state"`
}
type gameActionRequest struct {
	Version         string           `json:"version"`
	Cursor          *int64           `json:"cursor"`
	Sequence        *int64           `json:"sequence"`
	ExpectedCursor  *int64           `json:"expected_cursor"`
	ExpectedVersion *int64           `json:"expected_version"`
	EventHash       string           `json:"event_hash"`
	WormID          string           `json:"worm_id"`
	Direction       *int             `json:"direction"`
	Mask            *uint8           `json:"mask"`
	Request         *uint64          `json:"request"`
	PendingMask     *uint8           `json:"pending_mask"`
	PendingRequest  *uint64          `json:"pending_request"`
	Action          *protocol.Action `json:"action"`
	Payload         json.RawMessage  `json:"payload"`
}

func expectedCursor(r *gameActionRequest, req *http.Request, g store.Game) (int64, string, error) {
	var c int64
	found := false
	for _, p := range []*int64{r.Cursor, r.Sequence, r.ExpectedCursor, r.ExpectedVersion} {
		if p != nil {
			if found && *p != c {
				return 0, "", errors.New("cursor fields disagree")
			}
			c = *p
			found = true
		}
	}
	if !found {
		if h := req.Header.Get("X-Expected-Cursor"); h != "" {
			n, e := strconv.ParseInt(h, 10, 64)
			if e != nil {
				return 0, "", errors.New("invalid expected cursor")
			}
			c = n
			found = true
		}
	}
	if !found {
		return 0, "", errors.New("expected cursor is required")
	}
	h := r.EventHash
	if h == "" {
		h = req.Header.Get("If-Match")
		h = strings.Trim(h, `"`)
	}
	if h == "" {
		h = g.EventHash
	}
	return c, h, nil
}
func gameJSON(g store.Game) map[string]any {
	scores := map[string]int64{}
	maxScore := int64(-1)
	for _, p := range g.Participants {
		scores[p.ID] = p.Score
		if p.Score > maxScore {
			maxScore = p.Score
		}
	}
	winners := make([]string, 0)
	for _, p := range g.Participants {
		if p.Score == maxScore {
			winners = append(winners, p.ID)
		}
	}
	return map[string]any{"id": g.ID, "name": g.ID, "brain_version_id": g.BrainVersionID, "status": g.Status, "players": len(g.Participants), "tick": g.Sequence, "rules_payload": g.RulesPayload, "seed": g.Seed, "sequence": g.Sequence, "cursor": g.Sequence, "event_hash": g.EventHash, "created_at": g.CreatedAt, "updated_at": g.UpdatedAt, "participants": g.Participants, "scores": scores, "winners": winners, "move_count": g.MoveCount, "event_range": map[string]int64{"from": 1, "to": g.Sequence}}
}
func (s *Service) gamesRoute(w http.ResponseWriter, r *http.Request, p []string) {
	if len(p) == 0 {
		if r.Method == http.MethodGet {
			s.listGames(w, r)
			return
		}
		if r.Method == http.MethodPost {
			s.createGame(w, r)
			return
		}
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	id := p[0]
	if !validHTTPID(id) {
		writeAPIError(w, 400, "invalid_id", "resource ID is malformed", nil)
		return
	}
	if len(p) == 1 {
		if r.Method == http.MethodGet {
			s.getGame(w, r, id, false)
			return
		}
		methodNotAllowed(w, http.MethodGet)
		return
	}
	switch p[1] {
	case "resume":
		if r.Method == http.MethodGet || r.Method == http.MethodPost {
			s.getGame(w, r, id, true)
			return
		}
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	case "act", "teach", "pause", "tick":
		if r.Method == http.MethodPost {
			s.gameOperation(w, r, id, p[1])
			return
		}
		methodNotAllowed(w, http.MethodPost)
		return
	case "events":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.listGameEvents(w, r, id)
		return
	case "snapshot":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		s.getGameSnapshot(w, r, id)
		return
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "endpoint not found", nil)
		return
	}
}
func (s *Service) listGameEvents(w http.ResponseWriter, r *http.Request, id string) {
	o, e := parsePage(r)
	if e != nil {
		writeAPIError(w, 400, "invalid_request", e.Error(), nil)
		return
	}
	after := int64(0)
	if x := r.URL.Query().Get("after"); x != "" {
		after, e = strconv.ParseInt(x, 10, 64)
		if e != nil || after < 0 {
			writeAPIError(w, 400, "invalid_request", "after must be non-negative", nil)
			return
		}
	}
	events, e := s.data.ListEvents(r.Context(), id, after, o.Limit)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	out := make([]map[string]any, 0, len(events))
	for _, event := range events {
		out = append(out, map[string]any{
			"game_id": event.GameID, "sequence": event.Sequence, "type": event.Type,
			"payload": event.Payload, "prev_hash": event.PrevHash, "hash": event.Hash,
			"created_at": event.CreatedAt,
		})
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "events": out, "limit": o.Limit, "after": after, "next_after": func() int64 {
		if len(events) == 0 {
			return after
		}
		return events[len(events)-1].Sequence
	}()})
}
func (s *Service) getGameSnapshot(w http.ResponseWriter, r *http.Request, id string) {
	snap, e := s.data.LoadLatestSnapshot(r.Context(), id)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "snapshot": map[string]any{
		"game_id": snap.GameID, "sequence": snap.Sequence, "payload": snap.Payload,
		"hash": snap.Hash, "created_at": snap.CreatedAt,
	}})
}
func (s *Service) listGames(w http.ResponseWriter, r *http.Request) {
	o, e := parsePage(r)
	if e != nil {
		writeAPIError(w, 400, "invalid_request", e.Error(), nil)
		return
	}
	var rows *sql.Rows
	if status := r.URL.Query().Get("status"); status != "" {
		rows, e = s.db.QueryContext(r.Context(), "SELECT id FROM games WHERE status=? ORDER BY updated_at DESC,id LIMIT ? OFFSET ?", status, o.Limit, o.Offset)
	} else {
		rows, e = s.db.QueryContext(r.Context(), "SELECT id FROM games ORDER BY updated_at DESC,id LIMIT ? OFFSET ?", o.Limit, o.Offset)
	}
	if e != nil {
		serverError(w, e)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			serverError(w, e)
			return
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		serverError(w, e)
		return
	}
	rows.Close()
	gs := make([]store.Game, 0, len(ids))
	for _, id := range ids {
		g, ge := s.data.GetGame(r.Context(), id)
		if ge != nil {
			mapStoreError(w, ge)
			return
		}
		gs = append(gs, g)
	}
	out := make([]any, 0, len(gs))
	for _, g := range gs {
		out = append(out, gameJSON(g))
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "games": out, "limit": o.Limit, "offset": o.Offset, "next_offset": o.Offset + len(gs)})
}
func (s *Service) createGame(w http.ResponseWriter, r *http.Request) {
	var in gameRequest
	if e := s.readJSON(r, &in); e != nil {
		writeAPIError(w, 400, "invalid_json", e.Error(), nil)
		return
	}
	if e := versionOK(in.Version); e != nil {
		writeAPIError(w, 400, "invalid_version", e.Error(), nil)
		return
	}
	raw := in.Rules
	if len(raw) == 0 {
		raw = in.RulesPayload
	}
	if len(raw) == 0 {
		raw, _ = store.EncodePayload(map[string]any{})
	}
	raw, e := requestPayload(raw)
	if e != nil {
		writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
		return
	}
	if len(raw) > s.maxRules {
		writeAPIError(w, 413, "payload_too_large", "rules payload exceeds configured limit", nil)
		return
	}
	if len(in.Participants) == 0 || len(in.Participants) > s.maxParticipants {
		writeAPIError(w, 400, "invalid_participants", fmt.Sprintf("participants must contain 1..%d entries", s.maxParticipants), nil)
		return
	}
	seenParticipants := map[string]bool{}
	for _, p := range in.Participants {
		if err := engine.ValidateID(p.ID); err != nil || seenParticipants[p.ID] {
			writeAPIError(w, 400, "invalid_participants", "participant IDs must be unique valid IDs", nil)
			return
		}
		seenParticipants[p.ID] = true
		if p.Score != 0 {
			writeAPIError(w, 400, "authoritative_field", "participant scores are server-owned", nil)
			return
		}
	}
	if len(in.State) > 0 {
		st, e := requestPayload(in.State)
		if e != nil {
			writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
			return
		}
		var d any
		if e := json.Unmarshal(payloadData(st), &d); e != nil {
			writeAPIError(w, 400, "invalid_payload", "state must be a valid snapshot", nil)
			return
		}
		wrapped, _ := store.EncodePayload(d)
		probe, e := engine.UnmarshalSnapshot(payloadData(wrapped))
		if e != nil || !validInitialState(probe, in.Participants) {
			writeAPIError(w, 400, "invalid_state", "state does not match server-created game setup", nil)
			return
		}
		raw = wrapped
	}
	participants := make([]store.Participant, 0, len(in.Participants))
	for _, p := range in.Participants {
		participants = append(participants, store.Participant{ID: p.ID, Name: p.Name, BrainVersionID: p.BrainVersionID, Kind: p.Kind, Score: p.Score, Payload: p.Payload})
	}
	g, e := s.data.CreateGame(r.Context(), store.CreateGameInput{ID: in.ID, BrainVersionID: in.BrainVersionID, Status: in.Status, RulesPayload: raw, Seed: in.Seed, Participants: participants})
	if e != nil {
		mapStoreError(w, e)
		return
	}
	writeVersioned(w, 201, map[string]any{"version": protocol.APIVersion, "game": gameJSON(g)})
}
func (s *Service) getGame(w http.ResponseWriter, r *http.Request, id string, resume bool) {
	g, e := s.data.GetGame(r.Context(), id)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	out := map[string]any{"version": protocol.APIVersion, "game": gameJSON(g)}
	if resume {
		st, e := s.loadState(r.Context(), g)
		if e != nil {
			mapStoreError(w, e)
			return
		}
		out["state"] = stateJSON(st)
	}
	writeVersioned(w, 200, out)
}
func validInitialState(st engine.State, participants []participantRequest) bool {
	if st.Tick != 0 || st.Round != 0 || st.GameOver || len(st.Worms) != len(participants) {
		return false
	}
	// A pending teaching decision is the sole setup projection emitted by the
	// engine before the first turn; all other client-supplied events are
	// authoritative and rejected.
	if len(st.Events) > 0 {
		if len(st.Events) != 1 || st.Events[0].Type != "decision_pending" || st.Pending == nil ||
			st.Events[0].WormID != st.Pending.WormID || st.Events[0].Mask != st.Pending.Mask ||
			st.Events[0].Request != st.Pending.Request {
			return false
		}
	}
	if len(st.Trails) != 0 {
		return false
	}
	baseWorms := make([]engine.Worm, 0, len(participants))
	for _, p := range participants {
		baseWorms = append(baseWorms, engine.Worm{ID: p.ID, Alive: true, Color: engine.Color(p.ID)})
	}
	base := engine.NewClassic(baseWorms)
	if st.Width != base.Width || st.Height != base.Height || st.Topology != base.Topology || st.Mode != base.Mode {
		return false
	}
	for i, w := range st.Worms {
		b := base.Worms[i]
		if w.ID != b.ID || w.Position != b.Position || (w.Color != "" && w.Color != b.Color) || !w.Alive || w.Score != 0 {
			return false
		}
	}
	for _, t := range st.Territories {
		if t.Owner != "" || t.Color != "" || t.Mask != 0 {
			return false
		}
	}
	return true
}
func decodePersistedState(raw []byte) (engine.State, error) {
	data := payloadData(raw)
	// Match snapshots use the canonical persisted envelope and store the
	// engine snapshot under "engine"; HTTP-created snapshots may use "state"
	// or be a direct engine snapshot for backwards compatibility.
	var env struct {
		Engine json.RawMessage `json:"engine"`
		State  json.RawMessage `json:"state"`
	}
	if json.Unmarshal(data, &env) == nil {
		if len(env.Engine) > 0 {
			return engine.UnmarshalSnapshot(payloadData(env.Engine))
		}
		if len(env.State) > 0 {
			return engine.UnmarshalSnapshot(payloadData(env.State))
		}
	}
	return engine.UnmarshalSnapshot(data)
}

func (s *Service) loadState(ctx context.Context, g store.Game) (engine.State, error) {
	if err := s.data.VerifyEventChain(ctx, g.ID); err != nil {
		return engine.State{}, err
	}
	if snap, e := s.data.LoadLatestSnapshot(ctx, g.ID); e == nil && snap.Sequence == g.Sequence {
		st, decodeErr := decodePersistedState(snap.Payload)
		if decodeErr != nil {
			return engine.State{}, fmt.Errorf("%w: malformed current snapshot: %v", store.ErrCorruptEvent, decodeErr)
		}
		return st, nil
	} else if e != nil && !errors.Is(e, store.ErrNotFound) {
		return engine.State{}, e
	}
	data := payloadData(g.RulesPayload)
	var holder struct {
		State json.RawMessage `json:"state"`
	}
	if json.Unmarshal(data, &holder) == nil && len(holder.State) > 0 {
		data = payloadData(holder.State)
	}
	var initial engine.State
	if st, e := decodePersistedState(data); e == nil {
		initial = st
	} else {
		worms := make([]engine.Worm, 0, len(g.Participants))
		for _, p := range g.Participants {
			worm := engine.Worm{ID: p.ID, Alive: true, Color: engine.Color(p.ID), BrainID: p.BrainVersionID}
			kind := engine.ControllerNew
			switch strings.ToLower(p.Kind) {
			case "auto":
				kind = engine.ControllerAuto
			case "wild":
				kind = engine.ControllerWild
			case "same":
				kind = engine.ControllerSame
			case "named", "scripted", "external", "llm":
				kind = engine.ControllerNamed
			case "asleep":
				kind = engine.ControllerAsleep
			}
			if err := engine.ConfigureWorm(&worm, kind, uint64(g.Seed)); err != nil {
				return engine.State{}, err
			}
			worms = append(worms, worm)
		}
		initial = engine.NewClassic(worms)
	}
	if g.Sequence == 0 {
		return initial, nil
	}
	var persisted []store.Event
	for after := int64(0); after < g.Sequence; {
		page, e := s.data.ListEvents(ctx, g.ID, after, 1000)
		if e != nil {
			return engine.State{}, e
		}
		if len(page) == 0 {
			return engine.State{}, fmt.Errorf("%w: event chain ended at %d", store.ErrCorruptEvent, after+1)
		}
		persisted = append(persisted, page...)
		after = page[len(page)-1].Sequence
	}
	if len(persisted) != int(g.Sequence) {
		return engine.State{}, fmt.Errorf("%w: event count mismatch", store.ErrCorruptEvent)
	}
	st := initial.Snapshot()
	for i, row := range persisted {
		if row.Sequence != int64(i+1) {
			return engine.State{}, fmt.Errorf("%w: sequence %d, want %d", store.ErrCorruptEvent, row.Sequence, i+1)
		}
		var transition struct {
			State     json.RawMessage `json:"state"`
			StateHash string          `json:"state_hash"`
		}
		if json.Unmarshal(payloadData(row.Payload), &transition) == nil && len(transition.State) > 0 {
			next, err := decodePersistedState(transition.State)
			if err != nil {
				return engine.State{}, fmt.Errorf("%w: malformed transition state", store.ErrCorruptEvent)
			}
			if transition.StateHash != "" && next.HashHex() != transition.StateHash {
				return engine.State{}, fmt.Errorf("%w: transition state hash mismatch", store.ErrCorruptEvent)
			}
			st = next
			continue
		}
		switch row.Type {
		case "worm_moved":
			var ev engine.Event
			if json.Unmarshal(payloadData(row.Payload), &ev) != nil {
				return engine.State{}, fmt.Errorf("%w: malformed move", store.ErrCorruptEvent)
			}
			var moved engine.Event
			var err error
			var next engine.State
			for candidate := engine.East; candidate <= engine.NorthEast; candidate++ {
				probe := st.Snapshot()
				moved, err = probe.Step(ev.WormID, candidate)
				if err == nil && moved.To == ev.To {
					next = probe
					break
				}
			}
			if next.Width == 0 {
				return engine.State{}, fmt.Errorf("%w: invalid move at %d", store.ErrCorruptEvent, row.Sequence)
			}
			st = next
		case "taught":
			var teach struct {
				WormID    string `json:"worm_id"`
				Mask      uint8  `json:"mask"`
				Direction int    `json:"direction"`
			}
			if json.Unmarshal(payloadData(row.Payload), &teach) != nil || teach.WormID == "" || teach.Mask > 63 || teach.Direction < 0 || teach.Direction > 5 {
				return engine.State{}, fmt.Errorf("%w: malformed teaching event", store.ErrCorruptEvent)
			}
			found := false
			for i := range st.Worms {
				if st.Worms[i].ID == teach.WormID {
					st.Worms[i].Rules[teach.Mask] = engine.Action(teach.Direction)
					found = true
					break
				}
			}
			if !found {
				return engine.State{}, fmt.Errorf("%w: unknown taught worm", store.ErrCorruptEvent)
			}
			st.Pending = nil
		case "tick":
			var tick struct {
				State json.RawMessage `json:"state"`
			}
			if json.Unmarshal(payloadData(row.Payload), &tick) != nil || len(tick.State) == 0 {
				return engine.State{}, fmt.Errorf("%w: malformed tick event", store.ErrCorruptEvent)
			}
			next, err := engine.UnmarshalSnapshot(tick.State)
			if err != nil {
				return engine.State{}, fmt.Errorf("%w: malformed tick state", store.ErrCorruptEvent)
			}
			st = next
		case "resigned":
			st.GameOver = true
		default:
			return engine.State{}, fmt.Errorf("%w: unsupported event %q", store.ErrCorruptEvent, row.Type)
		}
	}
	return st, nil
}
func stateJSON(st engine.State) any {
	b, err := st.MarshalSnapshot()
	if err != nil {
		return nil
	}
	var value any
	if json.Unmarshal(b, &value) != nil {
		return nil
	}
	return value
}
func eventPayload(e any) json.RawMessage { b, _ := store.EncodePayload(e); return b }
func (s *Service) gameOperation(w http.ResponseWriter, r *http.Request, id, op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, e := s.data.GetGame(r.Context(), id)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	var in gameActionRequest
	if e = s.readJSON(r, &in); e != nil {
		writeAPIError(w, 400, "invalid_json", e.Error(), nil)
		return
	}
	if e = versionOK(in.Version); e != nil {
		writeAPIError(w, 400, "invalid_version", e.Error(), nil)
		return
	}
	cursor, eh, e := expectedCursor(&in, r, g)
	if e != nil {
		writeAPIError(w, 400, "invalid_cursor", e.Error(), nil)
		return
	}
	if cursor != g.Sequence || eh != g.EventHash {
		mapStoreError(w, &store.ConflictError{Resource: "game", ID: id, ExpectedSequence: cursor, ActualSequence: g.Sequence, ExpectedHash: eh, ActualHash: g.EventHash})
		return
	}
	if op == "pause" {
		status := "paused"
		if in.Payload != nil {
			var x struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(in.Payload, &x) == nil && x.Status != "" {
				status = x.Status
			}
		}
		e = s.data.UpdateGame(r.Context(), store.UpdateGameInput{ID: id, Status: status, ExpectedSequence: cursor, ExpectedHash: eh})
		if e != nil {
			mapStoreError(w, e)
			return
		}
		g, _ = s.data.GetGame(r.Context(), id)
		st, stateErr := s.loadState(r.Context(), g)
		if stateErr != nil {
			mapStoreError(w, stateErr)
			return
		}
		writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "game": gameJSON(g), "state": stateJSON(st)})
		return
	}
	st, e := s.loadState(r.Context(), g)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	var ep store.EventInput
	if op == "act" {
		var action protocol.Action
		if in.Action != nil {
			action = *in.Action
		} else {
			if in.Direction == nil {
				writeAPIError(w, 400, "invalid_action", "direction or action is required", nil)
				return
			}
			action = protocol.Action{Kind: protocol.ActionMove, Direction: *in.Direction}
		}
		if e = action.Validate(); e != nil {
			writeAPIError(w, 400, "invalid_action", e.Error(), nil)
			return
		}
		if in.WormID == "" {
			writeAPIError(w, 400, "invalid_action", "worm_id is required", nil)
			return
		}
		if action.Kind == protocol.ActionMove {
			var ev engine.Event
			ev, e = st.Step(in.WormID, engine.Direction(action.Direction))
			if e != nil {
				writeAPIError(w, 422, "illegal_action", e.Error(), nil)
				return
			}
			ep = store.EventInput{Type: "worm_moved", Payload: eventPayload(ev)}
		} else {
			st.GameOver = true
			ep = store.EventInput{Type: "resigned", Payload: eventPayload(map[string]any{"worm_id": in.WormID})}
		}
	} else if op == "tick" {
		var advanced []engine.Event
		advanced, e = st.AdvanceRound()
		if e != nil {
			writeAPIError(w, 422, "tick_failed", e.Error(), nil)
			return
		}
		ep = store.EventInput{Type: "tick", Payload: eventPayload(map[string]any{"events": advanced, "state": stateJSON(st)})}
	} else if op == "teach" {
		wormID, mask, request, direction, raw, err := teachingDecision(in)
		if err != nil {
			writeAPIError(w, 400, "invalid_teach", err.Error(), nil)
			return
		}
		if st.Pending == nil {
			writeAPIError(w, 409, "no_pending_decision", "no teaching decision is pending", nil)
			return
		}
		if st.Pending.WormID != wormID || st.Pending.Mask != mask || st.Pending.Request != request {
			writeAPIError(w, 409, "stale_teaching", "teaching decision does not match the pending request", map[string]any{"worm_id": st.Pending.WormID, "mask": st.Pending.Mask, "request": st.Pending.Request})
			return
		}
		if _, e = st.Submit(engine.Direction(direction)); e != nil {
			writeAPIError(w, 422, "illegal_teach", e.Error(), nil)
			return
		}
		ep = store.EventInput{Type: "taught", Payload: raw}
	} else {
		writeAPIError(w, 400, "invalid_operation", "unsupported game operation", nil)
		return
	}
	b, e := st.MarshalSnapshot()
	if e != nil {
		serverError(w, e)
		return
	}
	snapBody, e := json.Marshal(map[string]any{"version": 1, "engine": json.RawMessage(b)})
	if e != nil {
		serverError(w, e)
		return
	}
	snap, e := store.EncodePayload(json.RawMessage(snapBody))
	if e != nil {
		serverError(w, e)
		return
	}
	events, e := s.data.AppendGameEventsWithSnapshot(r.Context(), id, cursor, eh, []store.EventInput{ep}, store.Snapshot{GameID: id, Sequence: cursor + 1, Payload: snap})
	if e != nil {
		mapStoreError(w, e)
		return
	}
	g, _ = s.data.GetGame(r.Context(), id)
	if st.GameOver {
		if e = s.persistAuthoritativeResults(r.Context(), g, st); e != nil {
			serverError(w, e)
			return
		}
		g, _ = s.data.GetGame(r.Context(), id)
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "game": gameJSON(g), "events": events, "state": stateJSON(st)})
}

func brainJSON(b store.Brain) map[string]any {
	return map[string]any{"id": b.ID, "name": b.Name, "description": b.Description, "type": b.Type, "frozen": b.Frozen, "created_at": b.CreatedAt}
}
func versionJSON(v store.BrainVersion) map[string]any {
	return map[string]any{"id": v.ID, "brain_id": v.BrainID, "version": v.Version, "rules": v.Rules.Payload, "lineage": v.Lineage.Payload, "provenance": v.Provenance.Payload, "payload": v.Payload, "hash": v.Hash, "created_at": v.CreatedAt}
}

// persistAuthoritativeResults copies completion data from the verified engine
// state into the relational summary. Client-provided participant scores are
// never used for this write.
func (s *Service) persistAuthoritativeResults(ctx context.Context, g store.Game, st engine.State) error {
	if !st.GameOver {
		return nil
	}
	scores := make(map[string]int64, len(st.Worms))
	moves := int64(0)
	for _, w := range st.Worms {
		scores[w.ID] = int64(w.Score)
	}
	for _, ev := range st.Events {
		if ev.Type == "worm_moved" || ev.Type == "worm_move" {
			moves++
		}
	}
	return s.data.CompleteGame(ctx, g.ID, "completed", g.Sequence, g.EventHash, scores, moves)
}
func (s *Service) brainsRoute(w http.ResponseWriter, r *http.Request, p []string) {
	if len(p) == 0 {
		if r.Method == http.MethodGet {
			s.listBrains(w, r)
			return
		}
		if r.Method == http.MethodPost {
			s.createBrain(w, r)
			return
		}
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	id := p[0]
	if !validHTTPID(id) {
		writeAPIError(w, 400, "invalid_id", "resource ID is malformed", nil)
		return
	}
	if len(p) == 1 {
		if r.Method == http.MethodGet {
			b, e := s.data.GetBrain(r.Context(), id)
			if e != nil {
				mapStoreError(w, e)
				return
			}
			writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "brain": brainJSON(b)})
			return
		}
		if r.Method == http.MethodPut {
			writeAPIError(w, 409, "immutable", "brains are immutable; create a version instead", nil)
			return
		}
	}
	if len(p) == 3 && p[1] == "versions" && r.Method == http.MethodPut {
		s.updateBrainVersion(w, r, id, p[2])
		return
	}
	switch p[1] {
	case "versions":
		if len(p) == 2 {
			if r.Method == http.MethodGet {
				s.listBrainVersions(w, r, id)
				return
			}
			if r.Method == http.MethodPost {
				s.createBrainVersion(w, r, id)
				return
			}
		}
	case "inspect":
		if r.Method == http.MethodGet {
			s.inspectBrain(w, r, id)
			return
		}
	case "diff":
		if r.Method == http.MethodGet {
			s.diffBrain(w, r, id)
			return
		}
	}
	writeAPIError(w, 404, "not_found", "endpoint not found", nil)
}
func (s *Service) listBrains(w http.ResponseWriter, r *http.Request) {
	o, e := parsePage(r)
	if e != nil {
		writeAPIError(w, 400, "invalid_request", e.Error(), nil)
		return
	}
	filter := store.BrainListOptions{Limit: o.Limit, Offset: o.Offset, Name: r.URL.Query().Get("name"), Type: r.URL.Query().Get("type")}
	if x := r.URL.Query().Get("frozen"); x != "" {
		v, err := strconv.ParseBool(x)
		if err != nil {
			writeAPIError(w, 400, "invalid_request", "frozen must be true or false", nil)
			return
		}
		filter.Frozen = &v
	}
	bs, e := s.data.ListBrains(r.Context(), filter)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	out := make([]any, 0, len(bs))
	for _, b := range bs {
		out = append(out, brainJSON(b))
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "brains": out, "limit": o.Limit, "offset": o.Offset, "next_offset": o.Offset + len(out)})
}
func (s *Service) createBrain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version     string `json:"version"`
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		Frozen      bool   `json:"frozen"`
	}
	if e := s.readJSON(r, &in); e != nil {
		writeAPIError(w, 400, "invalid_json", e.Error(), nil)
		return
	}
	if e := versionOK(in.Version); e != nil {
		writeAPIError(w, 400, "invalid_version", e.Error(), nil)
		return
	}
	b, e := s.data.CreateBrain(r.Context(), store.CreateBrainInput{ID: in.ID, Name: in.Name, Description: in.Description, Type: in.Type, Frozen: in.Frozen})
	if e != nil {
		mapStoreError(w, e)
		return
	}
	writeVersioned(w, 201, map[string]any{"version": protocol.APIVersion, "brain": brainJSON(b)})
}
func (s *Service) listBrainVersions(w http.ResponseWriter, r *http.Request, id string) {
	o, e := parsePage(r)
	if e != nil {
		writeAPIError(w, 400, "invalid_request", e.Error(), nil)
		return
	}
	vs, e := s.data.ListBrainVersions(r.Context(), id, o)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	out := make([]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, versionJSON(v))
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "brain_versions": out, "versions": out, "limit": o.Limit, "offset": o.Offset})
}
func (s *Service) createBrainVersion(w http.ResponseWriter, r *http.Request, brainID string) {
	var in struct {
		Version         string          `json:"version"`
		ID              string          `json:"id"`
		Number          int64           `json:"number"`
		BrainVersion    int64           `json:"brain_version"`
		Rules           json.RawMessage `json:"rules"`
		Lineage         json.RawMessage `json:"lineage"`
		Provenance      json.RawMessage `json:"provenance"`
		Payload         json.RawMessage `json:"payload"`
		ParentVersionID string          `json:"parent_version_id"`
	}
	if e := s.readJSON(r, &in); e != nil {
		writeAPIError(w, 400, "invalid_json", e.Error(), nil)
		return
	}
	if e := versionOK(in.Version); e != nil {
		writeAPIError(w, 400, "invalid_version", e.Error(), nil)
		return
	}
	n := in.Number
	if n == 0 {
		n = in.BrainVersion
	}
	if n <= 0 {
		n = 1
	}
	rules, e := requestPayload(in.Rules)
	if e != nil {
		writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
		return
	}
	if len(rules) > s.maxRules {
		writeAPIError(w, 413, "payload_too_large", "rules payload exceeds configured limit", nil)
		return
	}
	lineage, e := requestPayload(in.Lineage)
	if e != nil {
		writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
		return
	}
	prov, e := requestPayload(in.Provenance)
	if e != nil {
		writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
		return
	}
	payload, e := requestPayload(in.Payload)
	if e != nil {
		writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
		return
	}
	v, e := s.data.CreateBrainVersion(r.Context(), store.CreateBrainVersionInput{ID: in.ID, BrainID: brainID, Version: n, Rules: rules, Lineage: lineage, Provenance: prov, Payload: payload, ParentVersionID: in.ParentVersionID})
	if e != nil {
		mapStoreError(w, e)
		return
	}
	writeVersioned(w, 201, map[string]any{"version": protocol.APIVersion, "brain_version": versionJSON(v)})
}
func (s *Service) updateBrainVersion(w http.ResponseWriter, r *http.Request, brainID, baseID string) {
	var in struct {
		Version       string          `json:"version"`
		BaseVersionID string          `json:"base_version_id"`
		Rules         json.RawMessage `json:"rules"`
		Lineage       json.RawMessage `json:"lineage"`
		Provenance    json.RawMessage `json:"provenance"`
		Payload       json.RawMessage `json:"payload"`
	}
	if e := s.readJSON(r, &in); e != nil {
		writeAPIError(w, 400, "invalid_json", e.Error(), nil)
		return
	}
	if e := versionOK(in.Version); e != nil {
		writeAPIError(w, 400, "invalid_version", e.Error(), nil)
		return
	}
	if in.BaseVersionID != "" {
		baseID = in.BaseVersionID
	}
	rules, e := requestPayload(in.Rules)
	if e != nil {
		writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
		return
	}
	if len(rules) > s.maxRules {
		writeAPIError(w, 413, "payload_too_large", "rules payload exceeds configured limit", nil)
		return
	}
	lineage, e := requestPayload(in.Lineage)
	if e != nil {
		writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
		return
	}
	prov, e := requestPayload(in.Provenance)
	if e != nil {
		writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
		return
	}
	payload, e := requestPayload(in.Payload)
	if e != nil {
		writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
		return
	}
	v, e := s.data.UpdateBrainVersion(r.Context(), store.UpdateBrainVersionInput{BrainID: brainID, BaseVersionID: baseID, Rules: rules, Lineage: lineage, Provenance: prov, Payload: payload})
	if e != nil {
		mapStoreError(w, e)
		return
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "brain_version": versionJSON(v)})
}
func (s *Service) brainVersionRoute(w http.ResponseWriter, r *http.Request, p []string) {
	if len(p) != 1 {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !validHTTPID(p[0]) {
		writeAPIError(w, 400, "invalid_id", "resource ID is malformed", nil)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	v, e := s.data.GetBrainVersion(r.Context(), p[0])
	if e != nil {
		mapStoreError(w, e)
		return
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "brain_version": versionJSON(v)})
}
func inspectionRules(raw json.RawMessage) []any {
	data := payloadData(raw)
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return nil
	}
	if obj, ok := decoded.(map[string]any); ok {
		if r, found := obj["rules"]; found {
			decoded = r
		}
	}
	switch values := decoded.(type) {
	case []any:
		out := make([]any, 0, len(values))
		for i, value := range values {
			if obj, ok := value.(map[string]any); ok {
				if _, hasMask := obj["mask"]; !hasMask {
					obj["mask"] = i
				}
				out = append(out, obj)
			} else {
				out = append(out, map[string]any{"mask": i, "action": value})
			}
		}
		return out
	case map[string]any:
		out := make([]any, 0, len(values))
		for mask, action := range values {
			maskNumber, parseErr := strconv.Atoi(mask)
			if parseErr == nil {
				out = append(out, map[string]any{"mask": maskNumber, "action": action})
			} else {
				out = append(out, map[string]any{"pattern": mask, "action": action})
			}
		}
		return out
	default:
		return nil
	}
}

func (s *Service) inspectBrain(w http.ResponseWriter, r *http.Request, id string) {
	o, e := parsePage(r)
	if e != nil {
		writeAPIError(w, 400, "invalid_request", e.Error(), nil)
		return
	}
	var x store.BrainInspection
	if requestedID := r.URL.Query().Get("version_id"); requestedID != "" {
		vs, pageErr := s.data.InspectBrainPage(r.Context(), id, requestedID, "", 0, 1)
		if pageErr != nil {
			mapStoreError(w, pageErr)
			return
		}
		x = store.BrainInspection{Versions: vs}
	} else {
		x, e = s.data.InspectBrain(r.Context(), id)
	}
	if e != nil {
		mapStoreError(w, e)
		return
	}
	if len(x.Versions) == 0 {
		if r.URL.Query().Get("version_id") != "" || r.URL.Query().Get("version") != "" {
			writeAPIError(w, 404, "not_found", "brain version not found", map[string]any{"brain_id": id})
		} else {
			writeAPIError(w, 422, "empty_brain", "brain has no saved versions", map[string]any{"brain_id": id})
		}
		return
	}
	versionID := r.URL.Query().Get("version_id")
	versionNumber := int64(0)
	if q := r.URL.Query().Get("version"); q != "" {
		versionNumber, e = strconv.ParseInt(q, 10, 64)
		if e != nil || versionNumber < 1 {
			writeAPIError(w, 400, "invalid_request", "version must be a positive integer", nil)
			return
		}
	}
	var selected *store.BrainVersion
	if versionID == "" && versionNumber == 0 {
		selected = &x.Versions[len(x.Versions)-1]
	}
	for i := range x.Versions {
		v := &x.Versions[i]
		if selected == nil && (versionID == "" || v.ID == versionID) && (versionNumber == 0 || v.Version == versionNumber) {
			selected = v
			break
		}
	}
	if selected == nil {
		writeAPIError(w, 404, "not_found", "brain version not found", map[string]any{"brain_id": id, "version_id": versionID, "version": versionNumber})
		return
	}
	filter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("filter")))
	if filter == "" {
		filter = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("pattern")))
	}
	allRules := inspectionRules(selected.Rules.Payload)
	filtered := make([]any, 0, len(allRules))
	for _, rule := range allRules {
		if filter == "" || strings.Contains(strings.ToLower(fmt.Sprint(rule)), filter) {
			filtered = append(filtered, rule)
		}
	}
	start := o.Offset
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + o.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	rules := filtered[start:end]
	usage, _ := s.data.BrainUsageCount(r.Context(), selected.ID)
	writeVersioned(w, 200, map[string]any{
		"version": protocol.APIVersion, "id": id, "brain_id": id,
		"version_id": selected.ID, "version_number": selected.Version,
		"versions": []any{versionJSON(*selected)}, "rules": rules,
		"usage": usage, "offset": o.Offset, "limit": o.Limit,
		"total": len(filtered), "next_offset": o.Offset + len(rules),
	})
}
func (s *Service) diffBrain(w http.ResponseWriter, r *http.Request, id string) {
	a, b := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if a == "" || b == "" {
		writeAPIError(w, 400, "invalid_request", "from and to are required", nil)
		return
	}
	d, e := s.data.DiffBrainVersions(r.Context(), a, b)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	if d.From.BrainID != id || d.To.BrainID != id {
		writeAPIError(w, 400, "invalid_request", "versions must belong to the requested brain", nil)
		return
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "brain_id": id, "from": versionJSON(d.From), "to": versionJSON(d.To), "rules_changed": d.RulesChanged, "lineage_changed": d.LineageChanged, "provenance_changed": d.ProvenanceChanged, "payload_changed": d.PayloadChanged})
}
func tournamentJSON(t store.Tournament) map[string]any {
	return map[string]any{"id": t.ID, "name": t.Name, "status": t.Status, "rules_payload": t.RulesPayload, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt}
}
func matchJSON(m store.Match) map[string]any {
	return map[string]any{"id": m.ID, "tournament_id": m.TournamentID, "game_id": m.GameID, "round": m.Round, "status": m.Status, "payload": m.Payload, "created_at": m.CreatedAt, "updated_at": m.UpdatedAt}
}
func (s *Service) tournamentsRoute(w http.ResponseWriter, r *http.Request, p []string) {
	if len(p) == 0 {
		if r.Method == http.MethodGet {
			o, e := parsePage(r)
			if e != nil {
				writeAPIError(w, 400, "invalid_request", e.Error(), nil)
				return
			}
			rows, e := s.db.QueryContext(r.Context(), "SELECT id FROM tournaments ORDER BY updated_at DESC,id LIMIT ? OFFSET ?", o.Limit, o.Offset)
			if e != nil {
				serverError(w, e)
				return
			}
			var ids []string
			for rows.Next() {
				var id string
				if e = rows.Scan(&id); e != nil {
					rows.Close()
					serverError(w, e)
					return
				}
				ids = append(ids, id)
			}
			if e = rows.Err(); e != nil {
				rows.Close()
				serverError(w, e)
				return
			}
			rows.Close()
			ts := make([]store.Tournament, 0, len(ids))
			for _, id := range ids {
				t, te := s.data.GetTournament(r.Context(), id)
				if te != nil {
					mapStoreError(w, te)
					return
				}
				ts = append(ts, t)
			}
			out := make([]any, 0, len(ts))
			for _, t := range ts {
				out = append(out, tournamentJSON(t))
			}
			writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "tournaments": out, "limit": o.Limit, "offset": o.Offset})
			return
		}
		if r.Method == http.MethodPost {
			var in struct {
				Version          string `json:"version"`
				ID, Name, Status string
				RulesPayload     json.RawMessage `json:"rules_payload"`
			}
			if e := s.readJSON(r, &in); e != nil {
				writeAPIError(w, 400, "invalid_json", e.Error(), nil)
				return
			}
			if e := versionOK(in.Version); e != nil {
				writeAPIError(w, 400, "invalid_version", e.Error(), nil)
				return
			}
			raw, e := requestPayload(in.RulesPayload)
			if e != nil {
				writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
				return
			}
			t, e := s.data.CreateTournament(r.Context(), store.CreateTournamentInput{ID: in.ID, Name: in.Name, Status: in.Status, RulesPayload: raw})
			if e != nil {
				mapStoreError(w, e)
				return
			}
			writeVersioned(w, 201, map[string]any{"version": protocol.APIVersion, "tournament": tournamentJSON(t)})
			return
		}
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	id := p[0]
	if !validHTTPID(id) {
		writeAPIError(w, 400, "invalid_id", "resource ID is malformed", nil)
		return
	}
	if len(p) == 1 && r.Method == http.MethodGet {
		t, e := s.data.GetTournament(r.Context(), id)
		if e != nil {
			mapStoreError(w, e)
			return
		}
		writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "tournament": tournamentJSON(t)})
		return
	}
	if len(p) == 2 && p[1] == "matches" {
		if r.Method == http.MethodGet {
			s.listMatches(w, r, id)
			return
		}
		if r.Method == http.MethodPost {
			s.createMatch(w, r, id)
			return
		}
	}
	writeAPIError(w, 404, "not_found", "endpoint not found", nil)
}
func (s *Service) listMatches(w http.ResponseWriter, r *http.Request, tid string) {
	o, e := parsePage(r)
	if e != nil {
		writeAPIError(w, 400, "invalid_request", e.Error(), nil)
		return
	}
	rows, e := s.db.QueryContext(r.Context(), "SELECT id FROM tournament_matches WHERE tournament_id=? ORDER BY round,id LIMIT ? OFFSET ?", tid, o.Limit, o.Offset)
	if e != nil {
		serverError(w, e)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			serverError(w, e)
			return
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		serverError(w, e)
		return
	}
	rows.Close()
	ms := make([]store.Match, 0, len(ids))
	for _, id := range ids {
		m, me := s.data.GetMatch(r.Context(), id)
		if me != nil {
			mapStoreError(w, me)
			return
		}
		ms = append(ms, m)
	}
	out := make([]any, 0, len(ms))
	for _, m := range ms {
		out = append(out, matchJSON(m))
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "matches": out, "limit": o.Limit, "offset": o.Offset})
}
func (s *Service) createMatch(w http.ResponseWriter, r *http.Request, tid string) {
	var in struct {
		Version string          `json:"version"`
		ID      string          `json:"id"`
		GameID  string          `json:"game_id"`
		Status  string          `json:"status"`
		Round   int64           `json:"round"`
		Payload json.RawMessage `json:"payload"`
	}
	if e := s.readJSON(r, &in); e != nil {
		writeAPIError(w, 400, "invalid_json", e.Error(), nil)
		return
	}
	if e := versionOK(in.Version); e != nil {
		writeAPIError(w, 400, "invalid_version", e.Error(), nil)
		return
	}
	raw, e := requestPayload(in.Payload)
	if e != nil {
		writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
		return
	}
	m, e := s.data.CreateMatch(r.Context(), store.CreateMatchInput{ID: in.ID, TournamentID: tid, GameID: in.GameID, Status: in.Status, Round: in.Round, Payload: raw})
	if e != nil {
		mapStoreError(w, e)
		return
	}
	writeVersioned(w, 201, map[string]any{"version": protocol.APIVersion, "match": matchJSON(m)})
}
func (s *Service) matchesRoute(w http.ResponseWriter, r *http.Request, p []string) {
	if len(p) != 1 || r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	m, e := s.data.GetMatch(r.Context(), p[0])
	if e != nil {
		mapStoreError(w, e)
		return
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "match": matchJSON(m)})
}
