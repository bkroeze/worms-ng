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
	"sort"
	"strconv"
	"strings"
	"sync"

	"worms.ng/internal/engine"
	"worms.ng/internal/extension"
	"worms.ng/internal/planner"
	"worms.ng/internal/protocol"
	"worms.ng/internal/sharing"
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
		if closeErr := d.Close(); closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	if _, err = s.db.Exec(`INSERT INTO server_checks DEFAULT VALUES`); err != nil {
		if closeErr := d.Close(); closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
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
	case "experiments":
		s.experimentsRoute(w, r, parts[1:])
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
	Version         string               `json:"version"`
	ID              string               `json:"id"`
	BrainVersionID  string               `json:"brain_version_id"`
	Status          string               `json:"status"`
	Ruleset         string               `json:"ruleset"`
	Width           int                  `json:"width"`
	Height          int                  `json:"height"`
	Rules           json.RawMessage      `json:"rules"`
	RulesPayload    json.RawMessage      `json:"rules_payload"`
	ExtensionConfig *extension.Config    `json:"extension_config"`
	Seed            int64                `json:"seed"`
	Participants    []participantRequest `json:"participants"`
	State           json.RawMessage      `json:"state"`
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

func configIsClassic(c extension.Config) bool {
	return !c.Enabled && c.Version == 0 && len(c.Obstacles) == 0 && len(c.Holes) == 0 && len(c.OneWayTrails) == 0 && len(c.Teams) == 0 && len(c.WeightedTerritories) == 0 && c.TemporaryTrailTTL == 0 && c.EnergyLimit == 0 && !c.FogOfWar && c.Width == 0 && c.Height == 0 && c.ObstacleRate == 0 && c.HoleRate == 0
}
func gameJSONFor(g store.Game, fog bool) map[string]any {
	out := gameJSON(g)
	if fog {
		delete(out, "rules_payload")
		delete(out, "seed")
		if g.Status != "completed" {
			delete(out, "scores")
			delete(out, "winners")
		}
	}
	return out
}
func publicGameJSON(g store.Game) map[string]any {
	cfg, extended, _ := extensionConfigFromRules(g.RulesPayload)
	return gameJSONFor(g, extended && cfg.FogOfWar)
}

func extensionConfigFromRules(raw []byte) (extension.Config, bool, error) {
	data := payloadData(raw)
	var envelope struct {
		ExtensionConfig *extension.Config `json:"extension_config"`
		Config          *extension.Config `json:"config"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return extension.Config{}, false, nil
	}
	if envelope.ExtensionConfig != nil {
		return *envelope.ExtensionConfig, !configIsClassic(*envelope.ExtensionConfig), nil
	}
	if envelope.Config != nil && !configIsClassic(*envelope.Config) {
		return *envelope.Config, true, nil
	}
	return extension.Config{}, false, nil
}

func extensionResponse(st extension.State, wormID string) (map[string]any, error) {
	if wormID == "" {
		if st.Base.Pending != nil {
			wormID = st.Base.Pending.WormID
		} else if st.Base.ActiveSlot >= 0 && st.Base.ActiveSlot < len(st.Base.Worms) {
			wormID = st.Base.Worms[st.Base.ActiveSlot].ID
		} else if len(st.Base.Worms) > 0 {
			wormID = st.Base.Worms[0].ID
		}
	}
	obs, err := st.Observe(wormID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"config": st.Config, "observation": obs, "team_winners": []string{}}
	if st.Config.FogOfWar {
		out["config"] = st.Config.SafeClientConfig()
	}
	if st.Base.GameOver {
		out["team_winners"] = st.TeamWinners()
	}
	if st.Base.GameOver {
		scores := st.Scores()
		winners := []string{}
		max := -1
		for id, score := range scores {
			if score > max {
				max, winners = score, []string{id}
			} else if score == max {
				winners = append(winners, id)
			}
		}
		sort.Strings(winners)
		out["scores"], out["winners"] = scores, winners
	}
	return out, nil
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
	case "abort", "act", "teach", "pause", "tick":
		if r.Method == http.MethodPost {
			s.gameOperation(w, r, id, p[1])
			return
		}
		methodNotAllowed(w, http.MethodPost)
		return
	case "plan":
		if r.Method == http.MethodPost {
			s.planGame(w, r, id)
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
			if closeErr := rows.Close(); closeErr != nil {
				e = errors.Join(e, closeErr)
			}
			serverError(w, e)
			return
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		if closeErr := rows.Close(); closeErr != nil {
			e = errors.Join(e, closeErr)
		}
		serverError(w, e)
		return
	}
	if e = rows.Close(); e != nil {
		serverError(w, e)
		return
	}
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
		out = append(out, publicGameJSON(g))
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
	cfg := extension.Config{}
	if in.ExtensionConfig != nil {
		cfg = *in.ExtensionConfig
	}
	base, e := initialEngineState(in.Seed, in.Participants)
	if e != nil {
		writeAPIError(w, 400, "invalid_participants", e.Error(), nil)
		return
	}
	width, height := in.Width, in.Height
	if width == 0 {
		width = cfg.Width
	}
	if height == 0 {
		height = cfg.Height
	}
	if width > 0 || height > 0 {
		if width == 0 {
			width = base.Width
		}
		if height == 0 {
			height = base.Height
		}
		worms := append([]engine.Worm(nil), base.Worms...)
		if strings.EqualFold(in.Ruleset, "classic") {
			base = engine.NewToroidal(width, height, worms)
		} else {
			base = engine.New(width, height, worms)
		}
	}
	if in.ExtensionConfig != nil {
		if cfg.Width == 0 && width > 0 {
			cfg.Width = width
		}
		if cfg.Height == 0 && height > 0 {
			cfg.Height = height
		}
	}
	if in.ExtensionConfig != nil {
		cfg = extension.NormalizeConfig(cfg, in.Seed)
	}
	var suppliedState json.RawMessage
	if len(in.State) > 0 {
		st, stateErr := requestPayload(in.State)
		if stateErr != nil {
			writeAPIError(w, 400, "invalid_payload", stateErr.Error(), nil)
			return
		}
		suppliedState = payloadData(st)
		probe, probeErr := engine.UnmarshalSnapshot(suppliedState)
		if probeErr != nil {
			extProbe, extErr := extension.UnmarshalSnapshot(suppliedState)
			if extErr != nil {
				writeAPIError(w, 400, "invalid_state", "state must be a valid snapshot", nil)
				return
			}
			probe = extProbe.Base
		}
		if !validInitialState(probe, in.Participants) {
			writeAPIError(w, 400, "invalid_state", "state does not match server-created game setup", nil)
			return
		}
		base = probe
	}
	var createdExt extension.State
	var extendedCreation bool
	if in.ExtensionConfig != nil && !configIsClassic(cfg) {
		if e = cfg.Validate(base); e != nil {
			writeAPIError(w, 400, "invalid_extension_config", e.Error(), nil)
			return
		}
		if len(suppliedState) > 0 {
			if createdExt, e = extension.UnmarshalSnapshot(suppliedState); e != nil {
				writeAPIError(w, 400, "invalid_state", "extended state must be an extension snapshot", nil)
				return
			}
			cb, _ := json.Marshal(createdExt.Config)
			want, _ := json.Marshal(cfg)
			if e = createdExt.Validate(); e != nil || string(cb) != string(want) {
				writeAPIError(w, 400, "invalid_state", "extension state does not match configuration", nil)
				return
			}
		} else if createdExt, e = extension.New(base, cfg, in.Seed); e != nil {
			writeAPIError(w, 400, "invalid_extension_config", e.Error(), nil)
			return
		}
		extendedCreation = true
		extRaw, extErr := createdExt.MarshalSnapshot()
		if extErr != nil {
			serverError(w, extErr)
			return
		}
		raw, e = withExtensionConfig(raw, cfg, extRaw)
		if e != nil {
			writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
			return
		}
	} else if len(suppliedState) > 0 {
		raw, e = store.EncodePayload(json.RawMessage(suppliedState))
		if e != nil {
			writeAPIError(w, 400, "invalid_payload", e.Error(), nil)
			return
		}
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
	out := map[string]any{"version": protocol.APIVersion, "game": publicGameJSON(g)}
	if extendedCreation {
		out["extension"], e = extensionResponse(createdExt, "")
		if e != nil {
			mapStoreError(w, e)
			return
		}
	}
	writeVersioned(w, 201, out)
}
func (s *Service) getGame(w http.ResponseWriter, r *http.Request, id string, resume bool) {
	g, e := s.data.GetGame(r.Context(), id)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	out := map[string]any{"version": protocol.APIVersion, "game": publicGameJSON(g)}
	cfg, extended, cfgErr := extensionConfigFromRules(g.RulesPayload)
	if cfgErr != nil {
		mapStoreError(w, cfgErr)
		return
	}
	if resume {
		var st engine.State
		if extended {
			extSt, loadErr := s.loadExtensionState(r.Context(), g, cfg)
			if loadErr != nil {
				mapStoreError(w, loadErr)
				return
			}
			st = extSt.Base
			out["extension"], loadErr = extensionResponse(extSt, r.URL.Query().Get("worm_id"))
			if loadErr != nil {
				mapStoreError(w, loadErr)
				return
			}
		} else {
			st, e = s.loadState(r.Context(), g)
			if e != nil {
				mapStoreError(w, e)
				return
			}
		}
		if !extended || !cfg.FogOfWar {
			out["state"] = stateJSON(st)
		}
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

func persistedExtensionSnapshot(raw []byte) (extension.State, error) {
	data := payloadData(raw)
	var env struct {
		Extension      json.RawMessage `json:"extension"`
		ExtensionState json.RawMessage `json:"extension_snapshot"`
		Engine         json.RawMessage `json:"engine"`
		State          json.RawMessage `json:"state"`
	}
	if json.Unmarshal(data, &env) == nil {
		switch {
		case len(env.Extension) > 0:
			data = env.Extension
		case len(env.ExtensionState) > 0:
			data = env.ExtensionState
		case len(env.State) > 0:
			data = env.State
		case len(env.Engine) > 0:
			data = env.Engine
		}
	}
	return extension.UnmarshalSnapshot(data)
}

func extensionStateFromRules(raw []byte) (extension.State, error) {
	data := payloadData(raw)
	var holder struct {
		State json.RawMessage `json:"state"`
	}
	if json.Unmarshal(data, &holder) != nil || len(holder.State) == 0 {
		return extension.State{}, store.ErrNotFound
	}
	return extension.UnmarshalSnapshot(holder.State)
}

func stateFromRules(g store.Game) (engine.State, error) {
	data := payloadData(g.RulesPayload)
	var holder struct {
		State json.RawMessage `json:"state"`
	}
	if json.Unmarshal(data, &holder) == nil && len(holder.State) > 0 {
		data = payloadData(holder.State)
	}
	return engine.UnmarshalSnapshot(data)
}

func (s *Service) loadExtensionState(ctx context.Context, g store.Game, cfg extension.Config) (extension.State, error) {
	if err := s.data.VerifyEventChain(ctx, g.ID); err != nil {
		return extension.State{}, err
	}
	if snap, e := s.data.LoadLatestSnapshot(ctx, g.ID); e == nil && snap.Sequence == g.Sequence {
		st, decodeErr := persistedExtensionSnapshot(snap.Payload)
		if decodeErr != nil {
			return extension.State{}, fmt.Errorf("%w: malformed extension snapshot: %v", store.ErrCorruptEvent, decodeErr)
		}
		cb, _ := json.Marshal(st.Config)
		want, _ := json.Marshal(cfg)
		if string(cb) != string(want) || st.GameEventSequence > g.Sequence {
			return extension.State{}, fmt.Errorf("%w: extension snapshot configuration mismatch", store.ErrCorruptEvent)
		}
		if err := st.Validate(); err != nil {
			return extension.State{}, fmt.Errorf("%w: invalid extension snapshot: %v", store.ErrCorruptEvent, err)
		}
		return st, nil
	} else if e != nil && !errors.Is(e, store.ErrNotFound) {
		return extension.State{}, e
	}
	if g.Sequence == 0 {
		if st, ruleErr := extensionStateFromRules(g.RulesPayload); ruleErr == nil {
			cb, _ := json.Marshal(st.Config)
			want, _ := json.Marshal(cfg)
			if string(cb) != string(want) {
				return extension.State{}, fmt.Errorf("%w: extension rules configuration mismatch", store.ErrCorruptEvent)
			}
			if err := st.Validate(); err != nil {
				return extension.State{}, fmt.Errorf("%w: invalid initialized extension state: %v", store.ErrCorruptEvent, err)
			}
			return st, nil
		}
	}
	base, e := stateFromRules(g)
	if e != nil {
		participants := make([]participantRequest, 0, len(g.Participants))
		for _, p := range g.Participants {
			participants = append(participants, participantRequest{ID: p.ID, BrainVersionID: p.BrainVersionID, Kind: p.Kind})
		}
		base, e = initialEngineState(g.Seed, participants)
		if e != nil {
			return extension.State{}, e
		}
	}
	if (cfg.Width > 0 && cfg.Width != base.Width) || (cfg.Height > 0 && cfg.Height != base.Height) {
		width, height := cfg.Width, cfg.Height
		if width == 0 {
			width = base.Width
		}
		if height == 0 {
			height = base.Height
		}
		worms := append([]engine.Worm(nil), base.Worms...)
		base = engine.New(width, height, worms)
	}
	if g.Sequence != 0 {
		return extension.State{}, fmt.Errorf("%w: missing extension snapshot", store.ErrCorruptEvent)
	}
	return extension.New(base, cfg, g.Seed)
}

func initialEngineState(seed int64, participants []participantRequest) (engine.State, error) {
	worms := make([]engine.Worm, 0, len(participants))
	for _, p := range participants {
		w := engine.Worm{ID: p.ID, Alive: true, Color: engine.Color(p.ID), BrainID: p.BrainVersionID}
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
		if err := engine.ConfigureWorm(&w, kind, uint64(seed)); err != nil {
			return engine.State{}, err
		}
		worms = append(worms, w)
	}
	return engine.NewClassic(worms), nil
}

func withExtensionConfig(raw []byte, c extension.Config, state json.RawMessage) (json.RawMessage, error) {
	data := payloadData(raw)
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		obj = map[string]any{}
	}
	obj["extension_config"] = c
	if len(state) > 0 {
		var value any
		if json.Unmarshal(state, &value) != nil {
			return nil, errors.New("invalid extension state")
		}
		obj["state"] = value
	}
	return store.EncodePayload(obj)
}

func snapshotPayload(st engine.State, ext *extension.State) (json.RawMessage, error) {
	var baseRaw []byte
	var err error
	if ext != nil {
		ext.Base = st
		baseRaw, err = st.MarshalSnapshot()
		if err != nil {
			return nil, err
		}
		extRaw, extErr := ext.MarshalSnapshot()
		if extErr != nil {
			return nil, extErr
		}
		body, marshalErr := json.Marshal(map[string]any{
			"version": 1, "engine": json.RawMessage(baseRaw), "extension": json.RawMessage(extRaw),
		})
		if marshalErr != nil {
			return nil, marshalErr
		}
		return store.EncodePayload(json.RawMessage(body))
	}
	baseRaw, err = st.MarshalSnapshot()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"version": 1, "engine": json.RawMessage(baseRaw)})
	if err != nil {
		return nil, err
	}
	return store.EncodePayload(json.RawMessage(body))
}

func (s *Service) loadState(ctx context.Context, g store.Game) (engine.State, error) {
	cfg, extended, cfgErr := extensionConfigFromRules(g.RulesPayload)
	if cfgErr != nil {
		return engine.State{}, cfgErr
	}
	if extended {
		st, err := s.loadExtensionState(ctx, g, cfg)
		if err != nil {
			return engine.State{}, err
		}
		return st.Base, nil
	}
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
	cfg, extended, cfgErr := extensionConfigFromRules(g.RulesPayload)
	if cfgErr != nil {
		mapStoreError(w, cfgErr)
		return
	}
	var extState extension.State
	if extended {
		extState, e = s.loadExtensionState(r.Context(), g, cfg)
		if e != nil {
			mapStoreError(w, e)
			return
		}
	}
	if op == "pause" || op == "abort" {
		status := "cancelled"
		if op == "pause" {
			status = "paused"
			if in.Payload != nil {
				var x struct {
					Status string `json:"status"`
				}
				if json.Unmarshal(in.Payload, &x) == nil && x.Status != "" {
					status = x.Status
				}
			}
		}
		e = s.data.UpdateGame(r.Context(), store.UpdateGameInput{ID: id, Status: status, ExpectedSequence: cursor, ExpectedHash: eh})
		if e != nil {
			mapStoreError(w, e)
			return
		}
		g, _ = s.data.GetGame(r.Context(), id)
		st := extState.Base
		if !extended {
			st, e = s.loadState(r.Context(), g)
			if e != nil {
				mapStoreError(w, e)
				return
			}
		}
		out := map[string]any{"version": protocol.APIVersion, "game": publicGameJSON(g)}
		if !extended || !cfg.FogOfWar {
			out["state"] = stateJSON(st)
		}
		if extended {
			out["extension"], e = extensionResponse(extState, in.WormID)
			if e != nil {
				mapStoreError(w, e)
				return
			}
		}
		writeVersioned(w, 200, out)
		return
	}
	var st engine.State
	if extended {
		st = extState.Base
	} else {
		st, e = s.loadState(r.Context(), g)
	}
	if e != nil {
		mapStoreError(w, e)
		return
	}
	var ep store.EventInput
	switch op {
	case "act":
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
			if extended {
				ev, e = extState.Apply(extension.Action{WormID: in.WormID, Direction: engine.Direction(action.Direction)})
				st = extState.Base
			} else {
				ev, e = st.Step(in.WormID, engine.Direction(action.Direction))
			}
			if e != nil {
				writeAPIError(w, 422, "illegal_action", e.Error(), nil)
				return
			}
			ep = store.EventInput{Type: "worm_moved", Payload: eventPayload(ev)}
		} else {
			st.GameOver = true
			if extended {
				extState.Base = st
			}
			ep = store.EventInput{Type: "resigned", Payload: eventPayload(map[string]any{"worm_id": in.WormID})}
		}
	case "tick":
		var advanced []engine.Event
		if extended {
			advanced, e = extState.AdvanceRound()
			st = extState.Base
		} else {
			advanced, e = st.AdvanceRound()
		}
		if e != nil {
			writeAPIError(w, 422, "tick_failed", e.Error(), nil)
			return
		}
		payload := map[string]any{"events": advanced}
		if !extended {
			payload["state"] = stateJSON(st)
		}
		ep = store.EventInput{Type: "tick", Payload: eventPayload(payload)}
	case "teach":
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
		if extended {
			extState.Base = st
		}
		ep = store.EventInput{Type: "taught", Payload: raw}
	default:
		writeAPIError(w, 400, "invalid_operation", "unsupported game operation", nil)
		return
	}
	snap, e := snapshotPayload(st, func() *extension.State {
		if extended {
			return &extState
		}
		return nil
	}())
	if e != nil {
		serverError(w, e)
		return
	}

	events, e := s.data.AppendGameEventsWithSnapshot(r.Context(), id, cursor, eh, []store.EventInput{ep}, store.Snapshot{GameID: id, Sequence: cursor + 1, Payload: snap})
	if e != nil {
		mapStoreError(w, e)
		return
	}
	if st.GameOver {
		if extended {
			if e = s.persistAuthoritativeExtensionResults(r.Context(), g, &extState); e != nil {
				serverError(w, e)
				return
			}
		} else if e = s.persistAuthoritativeResults(r.Context(), g, st); e != nil {
			serverError(w, e)
			return
		}
		g, _ = s.data.GetGame(r.Context(), id)
	}
	g, _ = s.data.GetGame(r.Context(), id)
	out := map[string]any{"version": protocol.APIVersion, "game": publicGameJSON(g), "events": events}
	if !extended || !cfg.FogOfWar {
		out["state"] = stateJSON(st)
	}
	if extended {
		out["extension"], e = extensionResponse(extState, in.WormID)
		if e != nil {
			mapStoreError(w, e)
			return
		}
	}
	writeVersioned(w, 200, out)
}

type planRequest struct {
	Version         string         `json:"version"`
	Cursor          *int64         `json:"cursor"`
	Sequence        *int64         `json:"sequence"`
	ExpectedCursor  *int64         `json:"expected_cursor"`
	ExpectedVersion *int64         `json:"expected_version"`
	EventHash       string         `json:"event_hash"`
	WormID          string         `json:"worm_id"`
	Config          planner.Config `json:"config"`
	PlannerConfig   planner.Config `json:"planner_config"`
	Teach           bool           `json:"teach"`
}

func (s *Service) planGame(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, e := s.data.GetGame(r.Context(), id)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	var in planRequest
	if e = s.readJSON(r, &in); e != nil {
		writeAPIError(w, 400, "invalid_json", e.Error(), nil)
		return
	}
	if e = versionOK(in.Version); e != nil {
		writeAPIError(w, 400, "invalid_version", e.Error(), nil)
		return
	}
	actionReq := gameActionRequest{Version: in.Version, Cursor: in.Cursor, Sequence: in.Sequence, ExpectedCursor: in.ExpectedCursor, ExpectedVersion: in.ExpectedVersion, EventHash: in.EventHash}
	cursor, eh, e := expectedCursor(&actionReq, r, g)
	if e != nil {
		writeAPIError(w, 400, "invalid_cursor", e.Error(), nil)
		return
	}
	if cursor != g.Sequence || eh != g.EventHash {
		mapStoreError(w, &store.ConflictError{Resource: "game", ID: id, ExpectedSequence: cursor, ActualSequence: g.Sequence, ExpectedHash: eh, ActualHash: g.EventHash})
		return
	}
	cfg, extended, cfgErr := extensionConfigFromRules(g.RulesPayload)
	if cfgErr != nil {
		mapStoreError(w, cfgErr)
		return
	}
	var extSt extension.State
	var st engine.State
	if extended {
		extSt, e = s.loadExtensionState(r.Context(), g, cfg)
		st = extSt.Base
	} else {
		st, e = s.loadState(r.Context(), g)
	}
	if e != nil {
		mapStoreError(w, e)
		return
	}
	if st.Pending == nil || st.Pending.WormID == "" {
		writeAPIError(w, 409, "no_pending_decision", "planner is only available for a pending unknown decision", nil)
		return
	}
	if in.WormID == "" {
		in.WormID = st.Pending.WormID
	}
	if in.WormID != st.Pending.WormID {
		writeAPIError(w, 409, "pending_mismatch", "worm_id does not match pending decision", nil)
		return
	}
	pc := in.PlannerConfig
	if pc.Version == 0 && pc.Mode == "" && in.Config.Version != 0 {
		pc = in.Config
	}
	p, e := planner.New(pc)
	if e != nil {
		writeAPIError(w, 400, "invalid_planner_config", e.Error(), nil)
		return
	}
	normalizedPC := p.Config()
	if extended && cfg.FogOfWar && (normalizedPC.Capabilities.GlobalState || normalizedPC.Capabilities.Observation == planner.GlobalObservation) {
		writeAPIError(w, 400, "planner_visibility", "global planner capability is unavailable under fog of war", nil)
		return
	}
	decision, e := p.Plan(st, in.WormID)
	if e != nil {
		writeAPIError(w, 409, "planner_unavailable", e.Error(), nil)
		return
	}
	if extended {
		legal := false
		for _, d := range extSt.LegalMoves(in.WormID) {
			if d == engine.Direction(decision.Action) {
				legal = true
				break
			}
		}
		if !legal {
			writeAPIError(w, 409, "planner_unavailable", "planner selected a move disallowed by extension rules", nil)
			return
		}
	}
	out := map[string]any{"version": protocol.APIVersion, "decision": decision, "alternatives": decision.Alternatives, "provenance": decision.Provenance, "game": publicGameJSON(g)}
	if !in.Teach {
		writeVersioned(w, 200, out)
		return
	}
	pendingRequest := st.Pending.Request
	if extended {
		if _, e = extSt.Submit(engine.Direction(decision.Action)); e != nil {
			writeAPIError(w, 422, "planner_teach_failed", e.Error(), nil)
			return
		}
		st = extSt.Base
	} else if _, e = p.Teach(&st, in.WormID); e != nil {
		writeAPIError(w, 422, "planner_teach_failed", e.Error(), nil)
		return
	}
	payload := eventPayload(map[string]any{"worm_id": decision.WormID, "mask": decision.Mask, "request": pendingRequest, "direction": int(decision.Action)})
	snap, e := snapshotPayload(st, func() *extension.State {
		if extended {
			return &extSt
		}
		return nil
	}())
	if e != nil {
		serverError(w, e)
		return
	}
	events, e := s.data.AppendGameEventsWithSnapshot(r.Context(), id, cursor, eh, []store.EventInput{{Type: "taught", Payload: payload}}, store.Snapshot{GameID: id, Sequence: cursor + 1, Payload: snap})
	if e != nil {
		mapStoreError(w, e)
		return
	}
	g, _ = s.data.GetGame(r.Context(), id)
	out["game"], out["events"] = publicGameJSON(g), events
	if !extended || !cfg.FogOfWar {
		out["state"] = stateJSON(st)
	}
	if extended {
		out["extension"], e = extensionResponse(extSt, in.WormID)
		if e != nil {
			mapStoreError(w, e)
			return
		}
	}
	writeVersioned(w, 200, out)
}

type shareRequest struct {
	Version            string           `json:"version"`
	Config             sharing.Config   `json:"config"`
	SharingConfig      sharing.Config   `json:"sharing_config"`
	TargetBrainID      string           `json:"target_brain_id"`
	RecipientBrainID   string           `json:"recipient_brain_id"`
	BrainID            string           `json:"brain_id"`
	RecipientVersionID string           `json:"recipient_version_id"`
	SourceVersionIDs   []string         `json:"source_version_ids"`
	Sources            []sharing.Source `json:"sources"`
}

func (s *Service) experimentsRoute(w http.ResponseWriter, r *http.Request, p []string) {
	if len(p) == 1 && p[0] == "share" && r.Method == http.MethodPost {
		s.shareExperiment(w, r)
		return
	}
	if len(p) == 1 && p[0] == "share" {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	writeAPIError(w, http.StatusNotFound, "not_found", "endpoint not found", nil)
}

func (s *Service) shareExperiment(w http.ResponseWriter, r *http.Request) {
	var in shareRequest
	if e := s.readJSON(r, &in); e != nil {
		writeAPIError(w, 400, "invalid_json", e.Error(), nil)
		return
	}
	if e := versionOK(in.Version); e != nil {
		writeAPIError(w, 400, "invalid_version", e.Error(), nil)
		return
	}
	cfg := in.SharingConfig
	if cfg.Policy == "" {
		cfg = in.Config
	}
	if len(cfg.Sources) == 0 {
		cfg.Sources = append([]sharing.Source(nil), in.Sources...)
	}
	for i, id := range in.SourceVersionIDs {
		if i < len(cfg.Sources) {
			cfg.Sources[i].BrainVersionID = id
		} else {
			cfg.Sources = append(cfg.Sources, sharing.Source{WormID: "source-" + strconv.Itoa(i), BrainVersionID: id})
		}
	}
	target := in.TargetBrainID
	if target == "" {
		target = in.RecipientBrainID
	}
	if target == "" {
		target = in.BrainID
	}
	if in.RecipientVersionID != "" {
		v, e := s.data.GetBrainVersion(r.Context(), in.RecipientVersionID)
		if e != nil {
			mapStoreError(w, e)
			return
		}
		if target != "" && target != v.BrainID {
			writeAPIError(w, 400, "invalid_request", "recipient version does not belong to target brain", nil)
			return
		}
		target = v.BrainID
	}
	if target == "" {
		writeAPIError(w, 400, "invalid_request", "target brain is required", nil)
		return
	}
	for i := range cfg.Sources {
		if cfg.Sources[i].BrainVersionID == "" {
			continue
		}
		v, e := s.data.GetBrainVersion(r.Context(), cfg.Sources[i].BrainVersionID)
		if e != nil {
			mapStoreError(w, e)
			return
		}
		cfg.Sources[i].Rules = append(json.RawMessage(nil), v.Rules.Payload...)
	}
	out, e := sharing.DeriveFromStore(r.Context(), s.data, cfg)
	if e != nil {
		if errors.Is(e, sharing.ErrInvalid) || errors.Is(e, sharing.ErrMissing) {
			writeAPIError(w, 400, "invalid_sharing_config", e.Error(), nil)
		} else {
			mapStoreError(w, e)
		}
		return
	}
	persistOut := out
	if in.RecipientVersionID != "" {
		matches := make([]sharing.Derived, 0, 1)
		for _, d := range out.Derived {
			if d.Lineage.RecipientVersionID == in.RecipientVersionID || d.Recipient.BrainVersionID == in.RecipientVersionID {
				matches = append(matches, d)
			}
		}
		if len(matches) != 1 {
			writeAPIError(w, 400, "invalid_request", "recipient version must identify exactly one derived recipient", nil)
			return
		}
		persistOut.Derived = matches
	} else if len(out.Derived) != 1 {
		writeAPIError(w, 400, "invalid_request", "recipient version is required when sharing has multiple recipients", nil)
		return
	}
	versions, e := persistOut.Persist(r.Context(), s.data, target)
	if e != nil {
		mapStoreError(w, e)
		return
	}
	changes, additions, removals := 0, 0, 0
	metrics := map[string]any{"derived": len(out.Derived), "versions": len(versions), "changes": changes, "additions": additions, "removals": removals}
	provenance := make([]sharing.Provenance, 0, len(out.Derived))
	for _, d := range out.Derived {
		changes += len(d.Changes)
		additions += len(d.Additions)
		removals += len(d.Removals)
		provenance = append(provenance, d.Provenance)
	}
	metrics["changes"] = changes
	metrics["additions"] = additions
	metrics["removals"] = removals
	versionJSONs := make([]any, 0, len(versions))
	for _, v := range versions {
		versionJSONs = append(versionJSONs, versionJSON(v))
	}
	writeVersioned(w, 200, map[string]any{"version": protocol.APIVersion, "policy": out.Policy, "seed": out.Seed, "hash": out.Hash, "derived": out.Derived, "brain_versions": versionJSONs, "metrics": metrics, "provenance": provenance})
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
func (s *Service) persistAuthoritativeExtensionResults(ctx context.Context, g store.Game, ext *extension.State) error {
	if ext == nil || !ext.Base.GameOver {
		return nil
	}
	scores := make(map[string]int64, len(ext.Variant.Scores))
	for id, score := range ext.Variant.Scores {
		scores[id] = int64(score)
	}
	moves := int64(0)
	for _, ev := range ext.Base.Events {
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
					if closeErr := rows.Close(); closeErr != nil {
						e = errors.Join(e, closeErr)
					}
					serverError(w, e)
					return
				}
				ids = append(ids, id)
			}
			if e = rows.Err(); e != nil {
				if closeErr := rows.Close(); closeErr != nil {
					e = errors.Join(e, closeErr)
				}
				serverError(w, e)
				return
			}
			if e = rows.Close(); e != nil {
				serverError(w, e)
				return
			}
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
			if closeErr := rows.Close(); closeErr != nil {
				e = errors.Join(e, closeErr)
			}
			serverError(w, e)
			return
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		if closeErr := rows.Close(); closeErr != nil {
			e = errors.Join(e, closeErr)
		}
		serverError(w, e)
		return
	}
	if e = rows.Close(); e != nil {
		serverError(w, e)
		return
	}
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
