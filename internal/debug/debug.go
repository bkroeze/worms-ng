// Package debug provides read-only inspection, replay and portable diagnostics.
// It intentionally never opens a database in read/write mode and never shells
// out to sqlite or other tools.
package debug

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"worms.ng/internal/engine"
)

var (
	ErrInvalid    = errors.New("debug: invalid input")
	ErrNotFound   = errors.New("debug: not found")
	ErrEmptyBrain = errors.New("debug: empty brain")
	ErrConnection = errors.New("debug: connection failure")
	ErrSchema     = errors.New("debug: unsupported or missing schema")
	ErrCorrupt    = errors.New("debug: corrupt data")
	ErrDivergence = errors.New("debug: replay divergence")
)

const maxSchemaVersion = 2

func validID(id string) bool {
	if strings.TrimSpace(id) == "" || len(id) > 512 {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == '/' || r == '\\' || r == '?' || r == '#' {
			return false
		}
	}
	return true
}

type Brain struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at"`
}
type Component struct {
	ID        string          `json:"id"`
	Payload   json.RawMessage `json:"payload"`
	Hash      string          `json:"hash"`
	CreatedAt string          `json:"created_at"`
}

func (c *Component) UnmarshalJSON(raw []byte) error {
	type plain Component
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return fmt.Errorf("%w: component JSON", ErrCorrupt)
	}
	if _, ok := probe["payload"]; ok {
		var x plain
		if err := json.Unmarshal(raw, &x); err != nil {
			return err
		}
		*c = Component(x)
		return nil
	}
	c.Payload = append([]byte(nil), raw...)
	return nil
}

type Lineage struct {
	Component
	ParentVersionID string `json:"parent_version_id,omitempty"`
}
type BrainVersion struct {
	ID           string          `json:"id"`
	BrainID      string          `json:"brain_id"`
	Version      int64           `json:"version"`
	Payload      json.RawMessage `json:"payload"`
	Hash         string          `json:"hash"`
	CreatedAt    string          `json:"created_at"`
	Rules        Component       `json:"rules"`
	RulesDecoded []DecodedRule   `json:"rules_decoded,omitempty"`
	Lineage      Lineage         `json:"lineage"`
	Provenance   Component       `json:"provenance"`
	Usage        []string        `json:"usage,omitempty"`
	References   []string        `json:"references,omitempty"`
}
type BrainInspection struct {
	Brain    Brain          `json:"brain"`
	Versions []BrainVersion `json:"versions"`
}
type BrainPage struct {
	Brain      Brain          `json:"brain"`
	Versions   []BrainVersion `json:"versions"`
	Total      int            `json:"total"`
	Offset     int            `json:"offset"`
	Limit      int            `json:"limit"`
	NextOffset int            `json:"next_offset,omitempty"`
}

func (l *Lineage) UnmarshalJSON(raw []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	var c Component
	if err := c.UnmarshalJSON(raw); err != nil {
		return err
	}
	l.Component = c
	if p, ok := probe["parent_version_id"]; ok {
		_ = json.Unmarshal(p, &l.ParentVersionID)
	}
	return nil
}

type RuleChange struct {
	Key     string          `json:"key"`
	Kind    string          `json:"kind"`
	Before  json.RawMessage `json:"before,omitempty"`
	After   json.RawMessage `json:"after,omitempty"`
	Mask    *uint8          `json:"mask,omitempty"`
	Pattern string          `json:"pattern,omitempty"`
	Action  string          `json:"action,omitempty"`
	Diagram string          `json:"diagram,omitempty"`
}
type BrainDiff struct {
	From              BrainVersion `json:"from"`
	To                BrainVersion `json:"to"`
	RulesChanged      bool         `json:"rules_changed"`
	LineageChanged    bool         `json:"lineage_changed"`
	ProvenanceChanged bool         `json:"provenance_changed"`
	PayloadChanged    bool         `json:"payload_changed"`
	Additions         []RuleChange `json:"additions,omitempty"`
	Removals          []RuleChange `json:"removals,omitempty"`
	Changes           []RuleChange `json:"changes,omitempty"`
}
type Participant struct {
	GameID         string          `json:"game_id"`
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	BrainVersionID string          `json:"brain_version_id,omitempty"`
	Kind           string          `json:"kind,omitempty"`
	Score          int64           `json:"score"`
	Payload        json.RawMessage `json:"payload"`
}
type Game struct {
	ID             string          `json:"id"`
	BrainVersionID string          `json:"brain_version_id,omitempty"`
	RulesPayload   json.RawMessage `json:"rules_payload"`
	Status         string          `json:"status"`
	Seed           int64           `json:"seed"`
	Sequence       int64           `json:"sequence"`
	EventHash      string          `json:"event_hash"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	Participants   []Participant   `json:"participants"`
}
type Event struct {
	GameID    string          `json:"game_id"`
	Sequence  int64           `json:"sequence"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	PrevHash  string          `json:"prev_hash"`
	Hash      string          `json:"hash"`
	CreatedAt string          `json:"created_at"`
}
type Snapshot struct {
	GameID    string          `json:"game_id"`
	Sequence  int64           `json:"sequence"`
	Payload   json.RawMessage `json:"payload"`
	Hash      string          `json:"hash"`
	CreatedAt string          `json:"created_at"`
}

type Reader interface {
	Brain(context.Context, string) (BrainInspection, error)
	// BrainPage performs bounded version inspection. Implementations must push
	// limit/offset to their backing store/API rather than slicing a full brain.
	BrainPage(context.Context, string, int, int) (BrainPage, error)
	BrainVersion(context.Context, string) (BrainVersion, error)
	Diff(context.Context, string, string) (BrainDiff, error)
	Games(context.Context, string) ([]Game, error)
	Game(context.Context, string) (Game, error)
	Events(context.Context, string, int64) ([]Event, error)
	Snapshot(context.Context, string) (Snapshot, error)
	SchemaVersion(context.Context) (int, error)
	Close() error
}

// SQLiteReader uses a separate read-only SQLite connection. It does not run
// migrations, PRAGMAs that write, or any other mutating statement.
type SQLiteReader struct{ db *sql.DB }

func OpenSQLite(ctx context.Context, filename string) (*SQLiteReader, error) {
	if strings.TrimSpace(filename) == "" {
		return nil, fmt.Errorf("%w: empty database path", ErrInvalid)
	}
	target := filename
	if !strings.Contains(target, "mode=") {
		if strings.HasPrefix(target, "file:") {
			sep := "?"
			if strings.Contains(target, "?") {
				sep = "&"
			}
			target += sep + "mode=ro"
		} else {
			abs, err := filepath.Abs(target)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			target = "file:" + url.PathEscape(filepath.ToSlash(abs)) + "?mode=ro"
		}
	}
	db, err := sql.Open("sqlite", target)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	r := &SQLiteReader{db: db}
	if _, err = db.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	if err = r.checkSchema(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	if _, err = r.SchemaVersion(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	if err = db.PingContext(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return nil, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	return r, nil
}
func OpenReadOnlySQLite(ctx context.Context, filename string) (*SQLiteReader, error) {
	return OpenSQLite(ctx, filename)
}
func (r *SQLiteReader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}
func (r *SQLiteReader) checkSchema(ctx context.Context) error {
	var n int
	for _, table := range []string{"schema_metadata", "brains", "brain_versions", "brain_rules", "brain_lineages", "brain_provenance", "games", "participants", "game_events", "game_snapshots"} {
		err := r.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrConnection, err)
		}
		if n != 1 {
			return fmt.Errorf("%w: missing table %s", ErrSchema, table)
		}
	}
	return nil
}
func (r *SQLiteReader) SchemaVersion(ctx context.Context) (int, error) {
	var s string
	err := r.db.QueryRowContext(ctx, "SELECT value FROM schema_metadata WHERE key='schema_version'").Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: schema version", ErrSchema)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	n, e := strconv.Atoi(s)
	if e != nil || n < 1 {
		return 0, fmt.Errorf("%w: schema version %q", ErrSchema, s)
	}
	if n > maxSchemaVersion {
		return 0, fmt.Errorf("%w: schema version %d", ErrSchema, n)
	}
	return n, nil
}
func (r *SQLiteReader) Brain(ctx context.Context, id string) (BrainInspection, error) {
	if !validID(id) {
		return BrainInspection{}, fmt.Errorf("%w: brain id", ErrInvalid)
	}
	var b Brain
	err := r.db.QueryRowContext(ctx, "SELECT id,name,description,created_at FROM brains WHERE id=?", id).Scan(&b.ID, &b.Name, &b.Description, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BrainInspection{}, fmt.Errorf("%w: brain %s", ErrNotFound, id)
	}
	if err != nil {
		return BrainInspection{}, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	rows, e := r.db.QueryContext(ctx, `SELECT v.id,v.brain_id,v.version,v.payload,v.hash,v.created_at,r.id,r.payload,r.hash,r.created_at,l.id,COALESCE(l.parent_version_id,''),l.payload,l.hash,l.created_at,p.id,p.payload,p.hash,p.created_at FROM brain_versions v JOIN brain_rules r ON r.id=v.rules_id JOIN brain_lineages l ON l.id=v.lineage_id JOIN brain_provenance p ON p.id=v.provenance_id WHERE v.brain_id=? ORDER BY v.version`, id)
	if e != nil {
		return BrainInspection{}, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	defer func() { _ = rows.Close() }()
	out := BrainInspection{Brain: b}
	for rows.Next() {
		v, e := scanVersion(rows)
		if e != nil {
			return BrainInspection{}, e
		}
		out.Versions = append(out.Versions, v)
	}
	if e = rows.Err(); e != nil {
		return BrainInspection{}, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	if len(out.Versions) == 0 {
		return BrainInspection{}, fmt.Errorf("%w: brain %s has no versions", ErrEmptyBrain, id)
	}
	return out, nil
}
func (r *SQLiteReader) BrainPage(ctx context.Context, id string, limit, offset int) (BrainPage, error) {
	if !validID(id) || limit < 1 || offset < 0 {
		return BrainPage{}, fmt.Errorf("%w: brain pagination", ErrInvalid)
	}
	var b Brain
	err := r.db.QueryRowContext(ctx, "SELECT id,name,description,created_at FROM brains WHERE id=?", id).Scan(&b.ID, &b.Name, &b.Description, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BrainPage{}, fmt.Errorf("%w: brain %s", ErrNotFound, id)
	}
	if err != nil {
		return BrainPage{}, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	var total int
	if err = r.db.QueryRowContext(ctx, "SELECT count(*) FROM brain_versions WHERE brain_id=?", id).Scan(&total); err != nil {
		return BrainPage{}, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	if total == 0 {
		return BrainPage{}, fmt.Errorf("%w: brain %s has no versions", ErrEmptyBrain, id)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT v.id,v.brain_id,v.version,v.payload,v.hash,v.created_at,r.id,r.payload,r.hash,r.created_at,l.id,COALESCE(l.parent_version_id,''),l.payload,l.hash,l.created_at,p.id,p.payload,p.hash,p.created_at
		FROM brain_versions v JOIN brain_rules r ON r.id=v.rules_id JOIN brain_lineages l ON l.id=v.lineage_id JOIN brain_provenance p ON p.id=v.provenance_id
		WHERE v.brain_id=? ORDER BY v.version LIMIT ? OFFSET ?`, id, limit, offset)
	if err != nil {
		return BrainPage{}, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	defer func() { _ = rows.Close() }()
	page := BrainPage{Brain: b, Total: total, Limit: limit, Offset: offset}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return BrainPage{}, err
		}
		page.Versions = append(page.Versions, v)
	}
	if err = rows.Err(); err != nil {
		return BrainPage{}, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	if offset+len(page.Versions) < total {
		page.NextOffset = offset + len(page.Versions)
	}
	return page, nil
}
func scanVersion(row interface{ Scan(...any) error }) (BrainVersion, error) {
	var v BrainVersion
	var payload, rules, lineage, prov []byte
	var parent string
	err := row.Scan(&v.ID, &v.BrainID, &v.Version, &payload, &v.Hash, &v.CreatedAt, &v.Rules.ID, &rules, &v.Rules.Hash, &v.Rules.CreatedAt, &v.Lineage.ID, &parent, &lineage, &v.Lineage.Hash, &v.Lineage.CreatedAt, &v.Provenance.ID, &prov, &v.Provenance.Hash, &v.Provenance.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return v, fmt.Errorf("%w", ErrNotFound)
		}
		return v, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	v.Payload = append([]byte(nil), payload...)
	v.Rules.Payload = append([]byte(nil), rules...)
	v.Lineage.Payload = append([]byte(nil), lineage...)
	v.Lineage.ParentVersionID = parent
	v.Provenance.Payload = append([]byte(nil), prov...)
	for n, x := range map[string]struct {
		p []byte
		h string
	}{"version": {payload, v.Hash}, "rules": {rules, v.Rules.Hash}, "lineage": {lineage, v.Lineage.Hash}, "provenance": {prov, v.Provenance.Hash}} {
		if !json.Valid(x.p) || !isEnvelope(x.p) {
			return v, fmt.Errorf("%w: invalid %s %s", ErrCorrupt, n, v.ID)
		}
		if hash(x.p) != x.h {
			return v, fmt.Errorf("%w: %s hash mismatch", ErrCorrupt, n)
		}
	}
	if decoded, de := DecodeRules(v.Rules.Payload); de == nil {
		v.RulesDecoded = decoded
	}
	v.Usage, v.References = metadataReferences(v)
	return v, nil
}
func isEnvelope(p []byte) bool {
	var x struct {
		Version int `json:"version"`
	}
	return json.Unmarshal(p, &x) == nil && x.Version == 1
}
func metadataReferences(v BrainVersion) (usage, refs []string) {
	seenUsage, seenRefs := map[string]bool{}, map[string]bool{}
	add := func(dst *[]string, seen map[string]bool, value string) {
		if value != "" && !seen[value] {
			seen[value] = true
			*dst = append(*dst, value)
		}
	}
	var walk func(string, json.RawMessage)
	walk = func(prefix string, raw json.RawMessage) {
		raw = payloadData(raw)
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil {
			keys := make([]string, 0, len(obj))
			for k := range obj {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				key := k
				if prefix != "" {
					key = prefix + "." + k
				}
				walk(key, obj[k])
			}
			return
		}
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) == nil {
			for i, item := range values {
				walk(fmt.Sprintf("%s[%d]", prefix, i), item)
			}
			return
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return
		}
		text := ""
		switch x := value.(type) {
		case string:
			text = x
		case float64:
			text = strconv.FormatFloat(x, 'f', -1, 64)
		case bool:
			text = strconv.FormatBool(x)
		default:
			return
		}
		lk := strings.ToLower(prefix)
		switch {
		case strings.Contains(lk, "usage") || strings.Contains(lk, "count"):
			add(&usage, seenUsage, prefix+"="+text)
		case strings.Contains(lk, "game") || strings.Contains(lk, "event") ||
			strings.Contains(lk, "reference") || strings.Contains(lk, "source") ||
			strings.Contains(lk, "sequence") || strings.Contains(lk, "learned"):
			add(&refs, seenRefs, prefix+"="+text)
		}
	}
	walk("", v.Payload)
	walk("", v.Lineage.Payload)
	walk("", v.Provenance.Payload)
	if v.Lineage.ParentVersionID != "" {
		add(&refs, seenRefs, "parent_version_id="+v.Lineage.ParentVersionID)
	}
	sort.Strings(usage)
	sort.Strings(refs)
	return usage, refs
}
func enrichVersionMetadata(v *BrainVersion) {
	if len(v.RulesDecoded) == 0 {
		v.RulesDecoded, _ = DecodeRules(v.Rules.Payload)
	}
	v.Usage, v.References = metadataReferences(*v)
}
func (r *SQLiteReader) BrainVersion(ctx context.Context, id string) (BrainVersion, error) {
	if !validID(id) {
		return BrainVersion{}, fmt.Errorf("%w: version id", ErrInvalid)
	}
	row := r.db.QueryRowContext(ctx, `SELECT v.id,v.brain_id,v.version,v.payload,v.hash,v.created_at,r.id,r.payload,r.hash,r.created_at,l.id,COALESCE(l.parent_version_id,''),l.payload,l.hash,l.created_at,p.id,p.payload,p.hash,p.created_at FROM brain_versions v JOIN brain_rules r ON r.id=v.rules_id JOIN brain_lineages l ON l.id=v.lineage_id JOIN brain_provenance p ON p.id=v.provenance_id WHERE v.id=?`, id)
	v, e := scanVersion(row)
	if errors.Is(e, ErrNotFound) {
		return v, e
	}
	return v, e
}
func (r *SQLiteReader) Diff(ctx context.Context, a, b string) (BrainDiff, error) {
	x, e := r.BrainVersion(ctx, a)
	if e != nil {
		return BrainDiff{}, e
	}
	y, e := r.BrainVersion(ctx, b)
	if e != nil {
		return BrainDiff{}, e
	}
	return BrainDiff{
		From: x, To: y,
		RulesChanged:      x.Rules.Hash != y.Rules.Hash,
		LineageChanged:    x.Lineage.Hash != y.Lineage.Hash,
		ProvenanceChanged: x.Provenance.Hash != y.Provenance.Hash,
		PayloadChanged:    x.Hash != y.Hash,
		Additions:         ruleChanges(x.Rules.Payload, y.Rules.Payload, "added"),
		Removals:          ruleChanges(y.Rules.Payload, x.Rules.Payload, "removed"),
		Changes:           changedRules(x.Rules.Payload, y.Rules.Payload),
	}, nil
}
func ruleChanges(before, after json.RawMessage, kind string) []RuleChange {
	a, ae := DecodeRules(before)
	b, be := DecodeRules(after)
	if ae != nil || be != nil {
		return nil
	}
	am, bm := map[uint8]DecodedRule{}, map[uint8]DecodedRule{}
	for _, r := range a {
		am[r.Mask] = r
	}
	for _, r := range b {
		bm[r.Mask] = r
	}
	var out []RuleChange
	for mask, r := range bm {
		if _, ok := am[mask]; ok {
			continue
		}
		raw, _ := json.Marshal(r)
		out = append(out, RuleChange{Key: strconv.Itoa(int(mask)), Kind: kind, After: raw, Mask: &r.Mask, Pattern: r.Pattern, Action: r.Action, Diagram: r.Diagram})
	}
	return out
}
func changedRules(before, after json.RawMessage) []RuleChange {
	a, ae := DecodeRules(before)
	b, be := DecodeRules(after)
	if ae != nil || be != nil {
		return nil
	}
	am, bm := map[uint8]DecodedRule{}, map[uint8]DecodedRule{}
	for _, r := range a {
		am[r.Mask] = r
	}
	for _, r := range b {
		bm[r.Mask] = r
	}
	var out []RuleChange
	for mask, r := range bm {
		old, ok := am[mask]
		if !ok || old.Action == r.Action {
			continue
		}
		bo, _ := json.Marshal(old)
		an, _ := json.Marshal(r)
		out = append(out, RuleChange{Key: strconv.Itoa(int(mask)), Kind: "changed", Before: bo, After: an, Mask: &r.Mask, Pattern: r.Pattern, Action: r.Action, Diagram: r.Diagram})
	}
	return out
}
func (r *SQLiteReader) Game(ctx context.Context, id string) (Game, error) {
	if !validID(id) {
		return Game{}, fmt.Errorf("%w: game id", ErrInvalid)
	}
	var g Game
	var brain sql.NullString
	var rules []byte
	e := r.db.QueryRowContext(ctx, "SELECT id,brain_version_id,rules_payload,status,seed,sequence,event_hash,created_at,updated_at FROM games WHERE id=?", id).Scan(&g.ID, &brain, &rules, &g.Status, &g.Seed, &g.Sequence, &g.EventHash, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return g, fmt.Errorf("%w: game %s", ErrNotFound, id)
	}
	if e != nil {
		return g, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	g.BrainVersionID = brain.String
	g.RulesPayload = append([]byte(nil), rules...)
	if !isEnvelope(rules) {
		return g, fmt.Errorf("%w: game payload", ErrCorrupt)
	}
	rows, e := r.db.QueryContext(ctx, "SELECT game_id,id,name,brain_version_id,kind,score,payload FROM participants WHERE game_id=? ORDER BY id", id)
	if e != nil {
		return g, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p Participant
		var bp sql.NullString
		var pl []byte
		if e = rows.Scan(&p.GameID, &p.ID, &p.Name, &bp, &p.Kind, &p.Score, &pl); e != nil {
			return g, fmt.Errorf("%w: %v", ErrConnection, e)
		}
		p.BrainVersionID = bp.String
		if !isEnvelope(pl) {
			return g, fmt.Errorf("%w: participant %s", ErrCorrupt, p.ID)
		}
		p.Payload = append([]byte(nil), pl...)
		g.Participants = append(g.Participants, p)
	}
	if e = rows.Err(); e != nil {
		return g, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	return g, nil
}
func (r *SQLiteReader) Games(ctx context.Context, brainID string) ([]Game, error) {
	q := "SELECT id FROM games"
	args := []any{}
	if brainID != "" {
		q += " WHERE brain_version_id=?"
		args = append(args, brainID)
	}
	q += " ORDER BY updated_at DESC,id"
	rows, e := r.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			if closeErr := rows.Close(); closeErr != nil {
				e = errors.Join(e, closeErr)
			}
			return nil, fmt.Errorf("%w: %v", ErrConnection, e)
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		if closeErr := rows.Close(); closeErr != nil {
			e = errors.Join(e, closeErr)
		}
		return nil, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	if e = rows.Close(); e != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	var out []Game
	for _, id := range ids {
		g, e := r.Game(ctx, id)
		if e != nil {
			return nil, e
		}
		out = append(out, g)
	}
	return out, nil
}
func eventHash(seq int64, prev, typ string, p []byte) string {
	return hash([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s", seq, prev, typ, string(p))))
}
func hash(p []byte) string { h := sha256.Sum256(p); return hex.EncodeToString(h[:]) }
func (r *SQLiteReader) Events(ctx context.Context, id string, after int64) ([]Event, error) {
	if !validID(id) || after < 0 {
		return nil, fmt.Errorf("%w: events query", ErrInvalid)
	}
	var exists int
	if err := r.db.QueryRowContext(ctx, "SELECT count(*) FROM games WHERE id=?", id).Scan(&exists); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnection, err)
	}
	if exists == 0 {
		return nil, fmt.Errorf("%w: game %s", ErrNotFound, id)
	}
	rows, e := r.db.QueryContext(ctx, "SELECT game_id,sequence,type,payload,prev_hash,hash,created_at FROM game_events WHERE game_id=? AND sequence>? ORDER BY sequence", id, after)
	if e != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var x Event
		var p []byte
		if e = rows.Scan(&x.GameID, &x.Sequence, &x.Type, &p, &x.PrevHash, &x.Hash, &x.CreatedAt); e != nil {
			return nil, fmt.Errorf("%w: %v", ErrConnection, e)
		}
		if !isEnvelope(p) {
			return nil, fmt.Errorf("%w: event %d payload", ErrCorrupt, x.Sequence)
		}
		want := eventHash(x.Sequence, x.PrevHash, x.Type, p)
		if want != x.Hash {
			return nil, fmt.Errorf("%w: event sequence=%d expected_hash=%s actual_hash=%s", ErrDivergence, x.Sequence, want, x.Hash)
		}
		x.Payload = append([]byte(nil), p...)
		out = append(out, x)
	}
	if e = rows.Err(); e != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	return out, nil
}
func (r *SQLiteReader) Snapshot(ctx context.Context, id string) (Snapshot, error) {
	var x Snapshot
	var p []byte
	e := r.db.QueryRowContext(ctx, "SELECT game_id,sequence,payload,hash,created_at FROM game_snapshots WHERE game_id=? ORDER BY sequence DESC LIMIT 1", id).Scan(&x.GameID, &x.Sequence, &p, &x.Hash, &x.CreatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return x, fmt.Errorf("%w: snapshot %s", ErrNotFound, id)
	}
	if e != nil {
		return x, fmt.Errorf("%w: %v", ErrConnection, e)
	}
	if !isEnvelope(p) || hash(p) != x.Hash {
		return x, fmt.Errorf("%w: snapshot %s", ErrCorrupt, id)
	}
	x.Payload = append([]byte(nil), p...)
	return x, nil
}

type DecodedRule struct {
	Mask          uint8  `json:"mask"`
	Pattern       string `json:"pattern"`
	Action        string `json:"action"`
	Direction     *int   `json:"direction,omitempty"`
	DirectionName string `json:"direction_name,omitempty"`
	Diagram       string `json:"diagram"`
}

var directionNames = [...]string{"east", "south-east", "south-west", "west", "north-west", "north-east"}

func DecodeDirection(n int) (string, error) {
	if n < 0 || n > 5 {
		return "", fmt.Errorf("%w: direction %d", ErrInvalid, n)
	}
	return directionNames[n], nil
}

func decodeRule(mask uint8, action int) DecodedRule {
	pattern := strings.Builder{}
	for d := 0; d < 6; d++ {
		if mask&(1<<d) != 0 {
			pattern.WriteByte('#')
		} else {
			pattern.WriteByte('.')
		}
	}
	r := DecodedRule{Mask: mask, Pattern: pattern.String(), Diagram: pattern.String()}
	switch {
	case action >= 0 && action <= 5:
		r.Direction = &action
		r.DirectionName = directionNames[action]
		r.Action = "move " + directionNames[action]
	case action == int(engine.ActionGetNew):
		r.Action = "learn"
	case action == int(engine.ActionDoAI):
		r.Action = "ai"
	case action == int(engine.ActionDie):
		r.Action = "die"
	default:
		r.Action = fmt.Sprintf("invalid(%d)", action)
	}
	return r
}

// DecodeRules is the canonical six-direction codec used by API, SQLite and CLI.
// It accepts either a rules array or an object containing rules/rule_table/data.
func DecodeRules(raw json.RawMessage) ([]DecodedRule, error) {
	raw = payloadData(raw)
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("%w: rules payload: %v", ErrCorrupt, err)
		}
		for _, key := range []string{"rules", "rule_table", "entries"} {
			if b, ok := obj[key]; ok && json.Unmarshal(b, &values) == nil {
				break
			}
		}
		if len(values) == 0 {
			return nil, fmt.Errorf("%w: rules payload has no table", ErrCorrupt)
		}
	}
	out := make([]DecodedRule, 0, len(values))
	for i, item := range values {
		var n int
		if json.Unmarshal(item, &n) == nil {
			out = append(out, decodeRule(uint8(i), n))
			continue
		}
		var x struct {
			Mask      *uint8          `json:"mask"`
			Action    json.RawMessage `json:"action"`
			Direction *int            `json:"direction"`
		}
		if err := json.Unmarshal(item, &x); err != nil {
			return nil, fmt.Errorf("%w: rule %d: %v", ErrCorrupt, i, err)
		}
		mask := uint8(i)
		if x.Mask != nil {
			mask = *x.Mask
		}
		action := int(engine.ActionDie)
		if x.Direction != nil {
			action = *x.Direction
		} else if len(x.Action) != 0 {
			if json.Unmarshal(x.Action, &action) != nil {
				var s string
				if json.Unmarshal(x.Action, &s) == nil {
					for d, name := range directionNames {
						if strings.EqualFold(s, name) || strings.EqualFold(s, strings.ReplaceAll(name, "-", "")) {
							action = d
							break
						}
					}
				}
			}
		}
		out = append(out, decodeRule(mask, action))
	}
	return out, nil
}

// VerifyEvents validates the append-only event chain and game head. It does not
// mutate state and can be used independently of a CLI.
// VerifyEvents validates the append-only event chain and game head.
func VerifyEvents(g Game, events []Event) error {
	prev := ""
	for i, e := range events {
		wantSeq := int64(i + 1)
		if e.GameID != g.ID || e.Sequence != wantSeq {
			return fmt.Errorf("%w: sequence expected=%d actual=%d", ErrDivergence, wantSeq, e.Sequence)
		}
		wantHash := eventHash(e.Sequence, prev, e.Type, e.Payload)
		if e.PrevHash != prev || wantHash != e.Hash {
			return fmt.Errorf("%w: sequence=%d expected_prev=%s actual_prev=%s expected_hash=%s actual_hash=%s", ErrDivergence, e.Sequence, prev, e.PrevHash, wantHash, e.Hash)
		}
		prev = e.Hash
	}
	if int64(len(events)) != g.Sequence || prev != g.EventHash {
		return fmt.Errorf("%w: game_head expected_sequence=%d actual_sequence=%d expected_hash=%s actual_hash=%s", ErrDivergence, g.Sequence, len(events), g.EventHash, prev)
	}
	return nil
}

func payloadData(raw json.RawMessage) json.RawMessage {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Data) != 0 {
		return envelope.Data
	}
	return raw
}

type ReplayOptions struct {
	Until         int64
	Pattern       string
	StopOnBrain   string
	StopOnPattern string
}
type ReplayTrace struct {
	Sequence       int64           `json:"sequence"`
	EventType      string          `json:"event_type"`
	DecisionID     string          `json:"decision_id,omitempty"`
	BrainVersionID string          `json:"brain_version_id,omitempty"`
	Observation    json.RawMessage `json:"observation,omitempty"`
	Mask           *uint8          `json:"mask,omitempty"`
	Action         string          `json:"action,omitempty"`
	StateHash      string          `json:"state_hash,omitempty"`
}
type ReplayResult struct {
	Game      Game          `json:"game"`
	Events    []Event       `json:"events"`
	Decisions []Event       `json:"decisions,omitempty"`
	Captures  []Event       `json:"captures,omitempty"`
	Deaths    []Event       `json:"deaths,omitempty"`
	Trace     []ReplayTrace `json:"trace,omitempty"`
	Stopped   bool          `json:"stopped"`
	StateHash string        `json:"state_hash,omitempty"`
}

func traceEvent(e Event) ReplayTrace {
	t := ReplayTrace{Sequence: e.Sequence, EventType: e.Type}
	var obj map[string]json.RawMessage
	if json.Unmarshal(payloadData(e.Payload), &obj) != nil {
		return t
	}
	for key, dst := range map[string]*string{"decision_id": &t.DecisionID, "brain_version_id": &t.BrainVersionID, "state_hash": &t.StateHash} {
		_ = json.Unmarshal(obj[key], dst)
	}
	if raw, ok := obj["observation"]; ok {
		t.Observation = append([]byte(nil), raw...)
	}
	if raw, ok := obj["mask"]; ok {
		var n uint8
		if json.Unmarshal(raw, &n) == nil {
			t.Mask = &n
		}
	}
	if raw, ok := obj["action"]; ok {
		var n int
		if json.Unmarshal(raw, &n) == nil {
			t.Action, _ = DecodeDirection(n)
		} else {
			_ = json.Unmarshal(raw, &t.Action)
		}
	}
	return t
}

func Replay(ctx context.Context, r Reader, id string, o ReplayOptions) (ReplayResult, error) {
	g, e := r.Game(ctx, id)
	if e != nil {
		return ReplayResult{}, e
	}
	events, e := r.Events(ctx, id, 0)
	if e != nil {
		return ReplayResult{}, e
	}
	if e = VerifyEvents(g, events); e != nil {
		return ReplayResult{}, e
	}
	if o.Until > 0 && o.Until < int64(len(events)) {
		events = events[:int(o.Until)]
	}
	res := ReplayResult{Game: g, Events: events}
	if snap, se := r.Snapshot(ctx, id); se == nil {
		if snap.Sequence <= int64(len(events)) {
			snapshotData := payloadData(snap.Payload)
			var persisted struct {
				Engine json.RawMessage `json:"engine"`
				State  json.RawMessage `json:"state"`
			}
			if json.Unmarshal(snapshotData, &persisted) == nil {
				if len(persisted.Engine) > 0 {
					snapshotData = persisted.Engine
				} else if len(persisted.State) > 0 {
					snapshotData = payloadData(persisted.State)
				}
			}
			initial, ue := engine.UnmarshalSnapshot(snapshotData)
			if ue != nil {
				return ReplayResult{}, fmt.Errorf("%w: snapshot replay: %v", ErrCorrupt, ue)
			}
			var moves []engine.Event
			for _, recorded := range events {
				if recorded.Sequence <= snap.Sequence || recorded.Type != "worm_moved" {
					continue
				}
				var move engine.Event
				if ue = json.Unmarshal(payloadData(recorded.Payload), &move); ue != nil {
					return ReplayResult{}, fmt.Errorf("%w: event replay: %v", ErrCorrupt, ue)
				}
				moves = append(moves, move)
			}
			_, ue = engine.Replay(initial, moves)
			if ue != nil {
				return ReplayResult{}, fmt.Errorf("%w: replay: %v", ErrCorrupt, ue)
			}
		}
	} else if !errors.Is(se, ErrNotFound) {
		return ReplayResult{}, se
	}
	for _, x := range events {
		text := strings.ToLower(x.Type + " " + string(x.Payload))
		if (o.StopOnBrain != "" && strings.Contains(text, strings.ToLower(o.StopOnBrain))) ||
			(o.StopOnPattern != "" && strings.Contains(text, strings.ToLower(o.StopOnPattern))) {
			res.Stopped = true
			break
		}
		if o.Pattern != "" && !strings.Contains(text, strings.ToLower(o.Pattern)) {
			continue
		}
		res.Trace = append(res.Trace, traceEvent(x))
		lowerType := strings.ToLower(x.Type)
		switch {
		case strings.Contains(lowerType, "decision"):
			res.Decisions = append(res.Decisions, x)
		case strings.Contains(lowerType, "captur"):
			res.Captures = append(res.Captures, x)
		case strings.Contains(lowerType, "death"):
			res.Deaths = append(res.Deaths, x)
		}
	}
	return res, nil
}

// Versioned wraps all exported diagnostic JSON payloads.
func Versioned(data any) ([]byte, error) {
	return json.Marshal(struct {
		Version int `json:"version"`
		Data    any `json:"data"`
	}{1, data})
}
func DecodeVersioned(raw []byte, dst any) error {
	var x struct {
		Version int             `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&x); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if x.Version != 1 {
		return fmt.Errorf("%w: unsupported version", ErrInvalid)
	}
	if err := json.Unmarshal(x.Data, dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

type Diagnostic struct {
	Version    int                 `json:"version"`
	ExportedAt string              `json:"exported_at"`
	Schema     int                 `json:"schema"`
	Brains     []BrainInspection   `json:"brains,omitempty"`
	Games      []Game              `json:"games,omitempty"`
	Events     map[string][]Event  `json:"events,omitempty"`
	Snapshots  map[string]Snapshot `json:"snapshots,omitempty"`
	Hashes     map[string]string   `json:"hashes"`
	Redacted   bool                `json:"redacted"`
}

// RestoreDiagnostic writes a validated diagnostic into a fresh or empty SQLite
// database. It creates only the read-model tables used by SQLiteReader.

func ImportDiagnosticSQLite(ctx context.Context, filename string, raw []byte) error {
	d, err := ImportDiagnostic(raw)
	if err != nil {
		return err
	}
	return RestoreDiagnostic(ctx, filename, d)
}

func RedactJSON(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	redactValue(v)
	b, _ := json.Marshal(v)
	return b
}
func redactValue(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "token") || strings.Contains(lk, "secret") || strings.Contains(lk, "password") || strings.Contains(lk, "authorization") || strings.Contains(lk, "credential") || lk == "api_key" {
				x[k] = "[redacted]"
			} else {
				redactValue(value)
			}
		}
	case []any:
		for _, value := range x {
			redactValue(value)
		}
	}
}
func (d *Diagnostic) Redact() {
	for i := range d.Brains {
		for j := range d.Brains[i].Versions {
			v := &d.Brains[i].Versions[j]
			v.Payload = RedactJSON(v.Payload)
			v.Hash = hash(v.Payload)
			v.Rules.Payload = RedactJSON(v.Rules.Payload)
			v.Rules.Hash = hash(v.Rules.Payload)
			v.Lineage.Payload = RedactJSON(v.Lineage.Payload)
			v.Lineage.Hash = hash(v.Lineage.Payload)
			v.Provenance.Payload = RedactJSON(v.Provenance.Payload)
			v.Provenance.Hash = hash(v.Provenance.Payload)
		}
	}
	for i := range d.Games {
		d.Games[i].RulesPayload = RedactJSON(d.Games[i].RulesPayload)
		for j := range d.Games[i].Participants {
			d.Games[i].Participants[j].Payload = RedactJSON(d.Games[i].Participants[j].Payload)
		}
		if es, ok := d.Events[d.Games[i].ID]; ok {
			prev := ""
			for j := range es {
				es[j].Payload = RedactJSON(es[j].Payload)
				es[j].PrevHash = prev
				es[j].Hash = eventHash(es[j].Sequence, prev, es[j].Type, es[j].Payload)
				prev = es[j].Hash
			}
			d.Games[i].EventHash = prev
			d.Events[d.Games[i].ID] = es
		}
	}
	for id, es := range d.Events {
		if _, ok := d.GamesByID(id); !ok {
			prev := ""
			for i := range es {
				es[i].Payload = RedactJSON(es[i].Payload)
				es[i].PrevHash = prev
				es[i].Hash = eventHash(es[i].Sequence, prev, es[i].Type, es[i].Payload)
				prev = es[i].Hash
			}
			d.Events[id] = es
		}
	}
	for id, s := range d.Snapshots {
		s.Payload = RedactJSON(s.Payload)
		s.Hash = hash(s.Payload)
		d.Snapshots[id] = s
	}
	d.Redacted = true
}
func (d *Diagnostic) GamesByID(id string) (Game, bool) {
	for _, g := range d.Games {
		if g.ID == id {
			return g, true
		}
	}
	return Game{}, false
}
func (d Diagnostic) Rehash() map[string]string {
	h := map[string]string{}
	for i, b := range d.Brains {
		for j, v := range b.Versions {
			raw, _ := json.Marshal(v)
			h[fmt.Sprintf("brain/%d/version/%d", i, j)] = hash(raw)
		}
	}
	for i, g := range d.Games {
		raw, _ := json.Marshal(g)
		h[fmt.Sprintf("game/%d", i)] = hash(raw)
	}
	for id, es := range d.Events {
		raw, _ := json.Marshal(es)
		h["events/"+id] = hash(raw)
	}
	for id, s := range d.Snapshots {
		raw, _ := json.Marshal(s)
		h["snapshot/"+id] = hash(raw)
	}
	return h
}
func Export(ctx context.Context, r Reader, brainID, gameID string, redact bool) (Diagnostic, error) {
	d := Diagnostic{Version: 1, ExportedAt: time.Now().UTC().Format(time.RFC3339Nano), Hashes: map[string]string{}}
	v, e := r.SchemaVersion(ctx)
	if e != nil {
		return d, e
	}
	d.Schema = v
	if brainID != "" {
		b, e := r.Brain(ctx, brainID)
		if e != nil {
			return d, e
		}
		d.Brains = []BrainInspection{b}
	}
	if gameID != "" {
		g, e := r.Game(ctx, gameID)
		if e != nil {
			return d, e
		}
		d.Games = []Game{g}
		es, e := r.Events(ctx, gameID, 0)
		if e == nil {
			d.Events = map[string][]Event{gameID: es}
		} else if !errors.Is(e, ErrNotFound) {
			return d, e
		}
		s, e := r.Snapshot(ctx, gameID)
		if e == nil {
			d.Snapshots = map[string]Snapshot{gameID: s}
		} else if !errors.Is(e, ErrNotFound) {
			return d, e
		}
	}
	if redact {
		d.Redact()
	}
	d.Hashes = d.Rehash()
	return d, nil
}
func ValidateDiagnostic(d Diagnostic) error {
	if d.Version != 1 {
		return fmt.Errorf("%w: diagnostic version", ErrInvalid)
	}
	for _, b := range d.Brains {
		for _, v := range b.Versions {
			for name, p := range map[string]json.RawMessage{
				"payload": v.Payload, "rules": v.Rules.Payload, "lineage": v.Lineage.Payload, "provenance": v.Provenance.Payload,
			} {
				if len(p) != 0 && (!json.Valid(p) || !isEnvelope(p)) {
					return fmt.Errorf("%w: brain %s payload", ErrCorrupt, name)
				}
			}
		}
	}
	for id, es := range d.Events {
		var g Game
		for _, x := range d.Games {
			if x.ID == id {
				g = x
			}
		}
		if g.ID != "" {
			if e := VerifyEvents(g, es); e != nil {
				return e
			}
		}
	}
	if len(d.Hashes) != 0 {
		got := d.Rehash()
		for k, want := range d.Hashes {
			if got[k] != want {
				return fmt.Errorf("%w: diagnostic hash %s", ErrCorrupt, k)
			}
		}
	}
	for id, s := range d.Snapshots {
		if !isEnvelope(s.Payload) || hash(s.Payload) != s.Hash {
			return fmt.Errorf("%w: snapshot %s", ErrCorrupt, id)
		}
	}
	return nil
}

// RestoreDiagnostic writes a validated diagnostic into a fresh or empty SQLite
// database. It refuses populated destinations and never replaces existing rows.
func RestoreDiagnostic(ctx context.Context, filename string, d Diagnostic) error {
	if strings.TrimSpace(filename) == "" {
		return fmt.Errorf("%w: database path", ErrInvalid)
	}
	if err := ValidateDiagnostic(d); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnection, err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConnection, err)
	}
	var tables int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("%w: %v", ErrConnection, err)
	}
	if tables != 0 {
		_ = tx.Rollback()
		return fmt.Errorf("%w: import destination must be nonexistent or empty", ErrInvalid)
	}
	ddl := []string{
		`CREATE TABLE schema_metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL)`,
		`CREATE TABLE brains(id TEXT PRIMARY KEY,name TEXT NOT NULL,description TEXT NOT NULL,created_at TEXT NOT NULL)`,
		`CREATE TABLE brain_rules(id TEXT PRIMARY KEY,payload BLOB NOT NULL,hash TEXT NOT NULL,created_at TEXT NOT NULL)`,
		`CREATE TABLE brain_lineages(id TEXT PRIMARY KEY,parent_version_id TEXT,payload BLOB NOT NULL,hash TEXT NOT NULL,created_at TEXT NOT NULL)`,
		`CREATE TABLE brain_provenance(id TEXT PRIMARY KEY,payload BLOB NOT NULL,hash TEXT NOT NULL,created_at TEXT NOT NULL)`,
		`CREATE TABLE brain_versions(id TEXT PRIMARY KEY,brain_id TEXT NOT NULL,version INTEGER NOT NULL,rules_id TEXT NOT NULL,lineage_id TEXT NOT NULL,provenance_id TEXT NOT NULL,payload BLOB NOT NULL,hash TEXT NOT NULL,created_at TEXT NOT NULL)`,
		`CREATE TABLE games(id TEXT PRIMARY KEY,brain_version_id TEXT,rules_payload BLOB NOT NULL,status TEXT NOT NULL,seed INTEGER NOT NULL,sequence INTEGER NOT NULL,event_hash TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
		`CREATE TABLE participants(game_id TEXT NOT NULL,id TEXT NOT NULL,name TEXT NOT NULL,brain_version_id TEXT,kind TEXT NOT NULL,score INTEGER NOT NULL,payload BLOB NOT NULL,slot INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(game_id,id))`,
		`CREATE TABLE game_events(game_id TEXT NOT NULL,sequence INTEGER NOT NULL,type TEXT NOT NULL,payload BLOB NOT NULL,prev_hash TEXT NOT NULL,hash TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(game_id,sequence))`,
		`CREATE TABLE game_snapshots(game_id TEXT NOT NULL,sequence INTEGER NOT NULL,payload BLOB NOT NULL,hash TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(game_id,sequence))`,
	}
	for _, stmt := range ddl {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%w: %v", ErrConnection, err)
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO schema_metadata(key,value) VALUES('schema_version',?)", strconv.Itoa(d.Schema)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("%w: %v", ErrConnection, err)
	}
	for _, bi := range d.Brains {
		if _, err = tx.ExecContext(ctx, "INSERT INTO brains(id,name,description,created_at) VALUES(?,?,?,?)", bi.Brain.ID, bi.Brain.Name, bi.Brain.Description, bi.Brain.CreatedAt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%w: %v", ErrConnection, err)
		}
		for _, v := range bi.Versions {
			if _, err = tx.ExecContext(ctx, "INSERT INTO brain_rules(id,payload,hash,created_at) VALUES(?,?,?,?)", v.Rules.ID, v.Rules.Payload, v.Rules.Hash, v.Rules.CreatedAt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("%w: %v", ErrConnection, err)
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO brain_lineages(id,parent_version_id,payload,hash,created_at) VALUES(?,?,?,?,?)", v.Lineage.ID, v.Lineage.ParentVersionID, v.Lineage.Payload, v.Lineage.Hash, v.Lineage.CreatedAt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("%w: %v", ErrConnection, err)
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO brain_provenance(id,payload,hash,created_at) VALUES(?,?,?,?)", v.Provenance.ID, v.Provenance.Payload, v.Provenance.Hash, v.Provenance.CreatedAt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("%w: %v", ErrConnection, err)
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO brain_versions(id,brain_id,version,rules_id,lineage_id,provenance_id,payload,hash,created_at) VALUES(?,?,?,?,?,?,?,?,?)", v.ID, v.BrainID, v.Version, v.Rules.ID, v.Lineage.ID, v.Provenance.ID, v.Payload, v.Hash, v.CreatedAt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("%w: %v", ErrConnection, err)
			}
		}
	}
	for _, g := range d.Games {
		if _, err = tx.ExecContext(ctx, "INSERT INTO games(id,brain_version_id,rules_payload,status,seed,sequence,event_hash,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)", g.ID, g.BrainVersionID, g.RulesPayload, g.Status, g.Seed, g.Sequence, g.EventHash, g.CreatedAt, g.UpdatedAt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%w: %v", ErrConnection, err)
		}
		for slot, p := range g.Participants {
			if _, err = tx.ExecContext(ctx, "INSERT INTO participants(game_id,id,name,brain_version_id,kind,score,payload,slot) VALUES(?,?,?,?,?,?,?,?)", g.ID, p.ID, p.Name, p.BrainVersionID, p.Kind, p.Score, p.Payload, slot); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("%w: %v", ErrConnection, err)
			}
		}
	}
	for id, events := range d.Events {
		for _, e := range events {
			if _, err = tx.ExecContext(ctx, "INSERT INTO game_events(game_id,sequence,type,payload,prev_hash,hash,created_at) VALUES(?,?,?,?,?,?,?)", id, e.Sequence, e.Type, e.Payload, e.PrevHash, e.Hash, e.CreatedAt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("%w: %v", ErrConnection, err)
			}
		}
	}
	for id, s := range d.Snapshots {
		if _, err = tx.ExecContext(ctx, "INSERT INTO game_snapshots(game_id,sequence,payload,hash,created_at) VALUES(?,?,?,?,?)", id, s.Sequence, s.Payload, s.Hash, s.CreatedAt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%w: %v", ErrConnection, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("%w: %v", ErrConnection, err)
	}
	return nil
}
func ImportDiagnostic(raw []byte) (Diagnostic, error) {
	var d Diagnostic
	var envelope struct {
		Version int             `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Data != nil {
		if envelope.Version != 1 {
			return d, fmt.Errorf("%w: diagnostic version", ErrInvalid)
		}
		raw = envelope.Data
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := ValidateDiagnostic(d); err != nil {
		return d, err
	}
	return d, nil
}
func WriteDiagnostic(w io.Writer, d Diagnostic) error {
	raw, e := Versioned(d)
	if e != nil {
		return e
	}
	_, e = w.Write(raw)
	return e
}
func ReadDiagnostic(r io.Reader) (Diagnostic, error) {
	raw, e := io.ReadAll(r)
	if e != nil {
		return Diagnostic{}, e
	}
	return ImportDiagnostic(raw)
}

// APIReader talks only to GET endpoints and has the same Reader contract as
// SQLiteReader. A server may expose only a subset; unsupported paths are
// returned as connection/API errors rather than silently falling back to SQL.
type APIReader struct {
	BaseURL string
	Client  *http.Client
}

func NewAPIReader(base string) *APIReader {
	base = strings.TrimRight(base, "/")
	return &APIReader{BaseURL: base, Client: &http.Client{Timeout: 15 * time.Second}}
}
func (a *APIReader) Close() error { return nil }
func (a *APIReader) SchemaVersion(ctx context.Context) (int, error) {
	var x struct {
		SchemaVersion int             `json:"schema_version"`
		Version       json.RawMessage `json:"version"`
	}
	e := a.get(ctx, "/api/v1/health", &x)
	if e != nil {
		return 0, e
	}
	if x.SchemaVersion > maxSchemaVersion {
		return 0, fmt.Errorf("%w: schema version %d", ErrSchema, x.SchemaVersion)
	}
	if x.SchemaVersion > 0 {
		return x.SchemaVersion, nil
	}
	var n int
	if len(x.Version) != 0 && json.Unmarshal(x.Version, &n) == nil {
		if n > maxSchemaVersion {
			return 0, fmt.Errorf("%w: schema version %d", ErrSchema, n)
		}
		return n, nil
	}
	return 1, nil
}
func (a *APIReader) get(ctx context.Context, path string, dst any) (err error) {
	if a == nil || a.BaseURL == "" {
		return fmt.Errorf("%w: empty API URL", ErrInvalid)
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+path, nil)
	if e != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, e)
	}
	resp, e := a.Client.Do(req)
	if e != nil {
		return fmt.Errorf("%w: %v", ErrConnection, e)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: HTTP %d", ErrConnection, resp.StatusCode)
	}
	raw, e := io.ReadAll(resp.Body)
	if e != nil {
		return fmt.Errorf("%w: %v", ErrConnection, e)
	}
	// API resources may be returned directly or in the common versioned
	// {version,data} envelope. Accepting both keeps diagnostics compatible with
	// older proof endpoints while retaining version checks for new endpoints.
	var envelope struct {
		Version json.RawMessage `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Data) != 0 {
		var version string
		var n int
		if json.Unmarshal(envelope.Version, &n) == nil {
			if n != 1 {
				return fmt.Errorf("%w: unsupported API version %d", ErrSchema, n)
			}
		} else if json.Unmarshal(envelope.Version, &version) == nil && version != "v1" && version != "1" {
			return fmt.Errorf("%w: unsupported API version %q", ErrSchema, version)
		}
		raw = envelope.Data
	}
	var resources map[string]json.RawMessage
	if json.Unmarshal(raw, &resources) == nil {
		for key, value := range resources {
			switch key {
			case "brain", "game", "games", "events", "snapshot", "diff", "brain_version":
				raw = value
			}
		}
	}
	if e = json.NewDecoder(bytes.NewReader(raw)).Decode(dst); e != nil {
		return fmt.Errorf("%w: %v", ErrCorrupt, e)
	}
	return nil
}
func (a *APIReader) Brain(ctx context.Context, id string) (BrainInspection, error) {
	if !validID(id) {
		return BrainInspection{}, fmt.Errorf("%w: brain id", ErrInvalid)
	}
	var b Brain
	if err := a.get(ctx, "/api/v1/brains/"+url.PathEscape(id), &b); err != nil {
		return BrainInspection{}, err
	}
	var x struct {
		Versions []BrainVersion `json:"versions"`
		Total    int            `json:"total"`
	}
	if err := a.get(ctx, "/api/v1/brains/"+url.PathEscape(id)+"/inspect", &x); err != nil {
		return BrainInspection{}, err
	}
	if len(x.Versions) == 0 {
		return BrainInspection{}, fmt.Errorf("%w: brain %s has no versions", ErrEmptyBrain, id)
	}
	for i := range x.Versions {
		enrichVersionMetadata(&x.Versions[i])
	}
	return BrainInspection{Brain: b, Versions: x.Versions}, nil
}
func (a *APIReader) BrainPage(ctx context.Context, id string, limit, offset int) (BrainPage, error) {
	if !validID(id) || limit < 1 || offset < 0 {
		return BrainPage{}, fmt.Errorf("%w: brain pagination", ErrInvalid)
	}
	var b Brain
	if err := a.get(ctx, "/api/v1/brains/"+url.PathEscape(id), &b); err != nil {
		return BrainPage{}, err
	}
	var page BrainPage
	p := "/api/v1/brains/" + url.PathEscape(id) + "/inspect?limit=" + strconv.Itoa(limit) + "&offset=" + strconv.Itoa(offset)
	if err := a.get(ctx, p, &page); err != nil {
		return BrainPage{}, err
	}
	if page.Total == 0 && len(page.Versions) == 0 && offset == 0 {
		return BrainPage{}, fmt.Errorf("%w: brain %s has no versions", ErrEmptyBrain, id)
	}
	page.Brain = b
	if page.Limit == 0 {
		page.Limit = limit
	}
	page.Offset = offset
	for i := range page.Versions {
		enrichVersionMetadata(&page.Versions[i])
	}
	if page.NextOffset == offset && len(page.Versions) == 0 {
		page.NextOffset = 0
	}
	return page, nil
}
func (a *APIReader) BrainVersion(ctx context.Context, id string) (BrainVersion, error) {
	if !validID(id) {
		return BrainVersion{}, fmt.Errorf("%w: version id", ErrInvalid)
	}
	var x BrainVersion
	e := a.get(ctx, "/api/v1/brain-versions/"+url.PathEscape(id), &x)
	if e == nil {
		enrichVersionMetadata(&x)
	}
	return x, e
}
func (a *APIReader) Diff(ctx context.Context, x, y string) (BrainDiff, error) {
	if !validID(x) || !validID(y) {
		return BrainDiff{}, fmt.Errorf("%w: brain version id", ErrInvalid)
	}
	from, err := a.BrainVersion(ctx, x)
	if err != nil {
		return BrainDiff{}, err
	}
	to, err := a.BrainVersion(ctx, y)
	if err != nil {
		return BrainDiff{}, err
	}
	if from.BrainID == "" || to.BrainID == "" || from.BrainID != to.BrainID {
		return BrainDiff{}, fmt.Errorf("%w: versions belong to different brains", ErrInvalid)
	}
	var d BrainDiff
	p := "/api/v1/brains/" + url.PathEscape(from.BrainID) + "/diff?from=" + url.QueryEscape(x) + "&to=" + url.QueryEscape(y)
	if err = a.get(ctx, p, &d); err != nil {
		return BrainDiff{}, err
	}
	return d, nil
}
func (a *APIReader) Games(ctx context.Context, brain string) ([]Game, error) {
	var x []Game
	p := "/api/v1/games"
	if brain != "" {
		p += "?brain=" + url.QueryEscape(brain)
	}
	e := a.get(ctx, p, &x)
	return x, e
}
func (a *APIReader) Game(ctx context.Context, id string) (Game, error) {
	if !validID(id) {
		return Game{}, fmt.Errorf("%w: game id", ErrInvalid)
	}
	var x Game
	e := a.get(ctx, "/api/v1/games/"+url.PathEscape(id), &x)
	return x, e
}
func (a *APIReader) Events(ctx context.Context, id string, after int64) ([]Event, error) {
	if !validID(id) || after < 0 {
		return nil, fmt.Errorf("%w: events query", ErrInvalid)
	}
	var x []Event
	p := "/api/v1/games/" + url.PathEscape(id) + "/events?after=" + strconv.FormatInt(after, 10)
	e := a.get(ctx, p, &x)
	return x, e
}
func (a *APIReader) Snapshot(ctx context.Context, id string) (Snapshot, error) {
	if !validID(id) {
		return Snapshot{}, fmt.Errorf("%w: game id", ErrInvalid)
	}
	var x Snapshot
	e := a.get(ctx, "/api/v1/games/"+url.PathEscape(id)+"/snapshot", &x)
	return x, e
}

func IsError(err, errorType error) bool { return errors.Is(err, errorType) }
