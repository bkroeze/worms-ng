// Package store provides the native SQLite persistence layer. It deliberately stores
// engine values as versioned JSON so persistence does not depend on engine internals.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"worms.ng/internal/extension"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	ErrNotFound        = errors.New("store: not found")
	ErrConflict        = errors.New("store: optimistic concurrency conflict")
	ErrImmutable       = errors.New("store: immutable record")
	ErrInvalidPayload  = errors.New("store: invalid versioned JSON payload")
	ErrCorruptPayload  = errors.New("store: corrupt persisted payload")
	ErrCorruptEvent    = errors.New("store: corrupt event chain")
	ErrConstraint      = errors.New("store: constraint violation")
	ErrMigration       = errors.New("store: migration failed")
	ErrCanceled        = errors.New("store: operation canceled")
	ErrInvalidArgument = errors.New("store: invalid argument")
)

// Compatibility aliases keep the error vocabulary explicit at call sites.
var (
	ErrCancelled    = ErrCanceled
	ErrCancellation = ErrCanceled
	ErrCorrupt      = ErrCorruptPayload
)

// ConstraintError preserves the stable repository category while retaining the
// driver error for diagnostics.
type ConstraintError struct {
	Resource string
	Err      error
}

func (e *ConstraintError) Error() string {
	return fmt.Sprintf("%v: %s: %v", ErrConstraint, e.Resource, e.Err)
}
func (e *ConstraintError) Unwrap() error { return ErrConstraint }

type MigrationError struct {
	Name string
	Err  error
}

func (e *MigrationError) Error() string {
	return fmt.Sprintf("%v: %s: %v", ErrMigration, e.Name, e.Err)
}
func (e *MigrationError) Unwrap() error { return ErrMigration }
func (e *MigrationError) Is(target error) bool {
	if target == ErrCanceled || target == context.Canceled || target == context.DeadlineExceeded {
		return errors.Is(e.Err, target)
	}
	return target == ErrMigration
}

type CancellationError struct{ Err error }

func (e *CancellationError) Error() string { return fmt.Sprintf("%v: %v", ErrCanceled, e.Err) }
func (e *CancellationError) Unwrap() error { return e.Err }
func (e *CancellationError) Is(target error) bool {
	return target == ErrCanceled || errors.Is(e.Err, target)
}

// classify maps driver cancellation and transient SQLite errors to the stable
// repository vocabulary. Every database boundary uses this helper, including
// row scans and rows.Err.
func classify(err error, resource string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrCanceled) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &CancellationError{Err: err}
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "locked") || strings.Contains(msg, "busy") {
		return fmt.Errorf("%w: %s writer busy: %v", ErrConflict, resource, err)
	}
	if strings.Contains(msg, "constraint") || strings.Contains(msg, "unique") ||
		strings.Contains(msg, "foreign key") || strings.Contains(msg, "not null") {
		return &ConstraintError{Resource: resource, Err: err}
	}
	return err
}

func classifyScan(err error, resource string) error { return classify(err, resource) }

type PayloadEnvelope struct {
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
}

// DecodePayload validates the canonical versioned envelope and decodes its
// data without coupling persistence to an engine package.
func DecodePayload(raw []byte, out any) error {
	if err := validatePayload(raw); err != nil {
		return err
	}
	var env PayloadEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

func PayloadData(raw []byte) (json.RawMessage, error) {
	if err := validatePayload(raw); err != nil {
		return nil, err
	}
	var env PayloadEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	return copyJSON(env.Data), nil
}

type ConflictError struct {
	Resource, ID                     string
	ExpectedSequence, ActualSequence int64
	ExpectedHash, ActualHash         string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%v: %s %s (expected sequence %d/%s, actual %d/%s)", ErrConflict, e.Resource, e.ID, e.ExpectedSequence, e.ExpectedHash, e.ActualSequence, e.ActualHash)
}
func (e *ConflictError) Unwrap() error { return ErrConflict }

type PayloadError struct {
	Err          error
	Resource, ID string
}

func (e *PayloadError) Error() string {
	return fmt.Sprintf("%v: %s %s: %v", ErrCorruptPayload, e.Resource, e.ID, e.Err)
}
func (e *PayloadError) Unwrap() error        { return ErrCorruptPayload }
func (e *PayloadError) Is(target error) bool { return target == ErrCorruptPayload || target == e.Err }

// Store is safe for concurrent use. SQLite serializes writers, and one
// connection keeps connection-local PRAGMAs (notably foreign_keys) consistent.
type Store struct{ db *sql.DB }

type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func Open(ctx context.Context, filename string) (*Store, error) {
	if filename == "" {
		return nil, fmt.Errorf("%w: empty database filename", ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, classify(err, "open")
	}
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err = s.configure(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = s.ensureGameMoveCount(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func OpenMemory(ctx context.Context) (*Store, error) {
	return Open(ctx, "file:worms-store-"+newID()+"?mode=memory&cache=shared")
}
func (s *Store) DB() *sql.DB { return s.db }
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) configure(ctx context.Context) error {
	for _, q := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", "PRAGMA busy_timeout = 5000", "PRAGMA synchronous = NORMAL"} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return classify(err, "configure")
		}
	}
	return nil
}

type migrationSpec struct {
	name string
	sql  string
}

func (s *Store) migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return &MigrationError{Name: "read", Err: err}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	files := make([]migrationSpec, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		text, readErr := migrationFS.ReadFile("migrations/" + entry.Name())
		if readErr != nil {
			return &MigrationError{Name: entry.Name(), Err: readErr}
		}
		files = append(files, migrationSpec{name: entry.Name(), sql: string(text)})
	}
	return s.migrateFiles(ctx, files)
}

func (s *Store) ensureGameMoveCount(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(games)")
	if err != nil {
		return classify(err, "games schema")
	}
	defer func() { _ = rows.Close() }()
	var found bool
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err = rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return classifyScan(err, "games schema")
		}
		if name == "move_count" {
			found = true
		}
	}
	if err = rows.Err(); err != nil {
		return classify(err, "games schema")
	}
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE games ADD COLUMN move_count INTEGER NOT NULL DEFAULT 0")
	return classify(err, "games schema")
}

// migrateFiles is deliberately injectable for migration rollback tests. Each
// migration, including its bookkeeping writes, executes in one transaction.
func (s *Store) migrateFiles(ctx context.Context, files []migrationSpec) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY NOT NULL, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		return classify(err, "schema_migrations")
	}
	for _, file := range files {
		version, err := strconv.Atoi(strings.SplitN(file.name, "_", 2)[0])
		if err != nil {
			return &MigrationError{Name: file.name, Err: err}
		}
		var exists int
		if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version = ?", version).Scan(&exists); err != nil {
			return &MigrationError{Name: file.name, Err: err}
		}
		if exists != 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return &MigrationError{Name: file.name, Err: err}
		}
		if _, err = tx.ExecContext(ctx, file.sql); err != nil {
			_ = tx.Rollback()
			return &MigrationError{Name: file.name, Err: err}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)", version, file.name, now()); err != nil {
			_ = tx.Rollback()
			return &MigrationError{Name: file.name, Err: err}
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO schema_metadata(key,value) VALUES('schema_version',?)", strconv.Itoa(version)); err != nil {
			_ = tx.Rollback()
			return &MigrationError{Name: file.name, Err: err}
		}
		if err = tx.Commit(); err != nil {
			return &MigrationError{Name: file.name, Err: err}
		}
	}
	return nil
}

// SchemaVersion and Metadata expose migration metadata without exposing write
// access to the schema tables.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var value string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM schema_metadata WHERE key='schema_version'").Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: schema version", ErrNotFound)
		}
		return 0, classifyScan(err, "schema metadata")
	}
	v, err := strconv.Atoi(value)
	return v, err
}

func (s *Store) Metadata(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM schema_metadata WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: metadata %s", ErrNotFound, key)
	}
	return value, classifyScan(err, "schema metadata")
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		copy(b, h[:16])
	}
	return hex.EncodeToString(b)
}
func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func eventHash(sequence int64, previous, typ string, payload []byte) string {
	return hashBytes([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s", sequence, previous, typ, string(payload))))
}

// EncodePayload wraps data in the stable v1 envelope used by this package.
func EncodePayload(data any) (json.RawMessage, error) {
	b, err := json.Marshal(struct {
		Version int `json:"version"`
		Data    any `json:"data"`
	}{1, data})
	return b, err
}
func validatePayload(raw []byte) error {
	if len(raw) == 0 || !json.Valid(raw) {
		return ErrInvalidPayload
	}
	var e struct {
		Version *int            `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Version == nil || *e.Version != 1 || e.Data == nil || !json.Valid(e.Data) {
		return ErrInvalidPayload
	}
	return nil
}

func EncodeSnapshot(data any) (json.RawMessage, error) { return EncodePayload(data) }
func DecodeSnapshot(raw []byte, out any) error         { return DecodePayload(raw, out) }
func ValidateSnapshot(raw []byte) error                { return validatePayload(raw) }
func copyJSON(raw []byte) json.RawMessage              { return append(json.RawMessage(nil), raw...) }

type Brain struct {
	ID, Name, Description, Type, CreatedAt string
	Frozen                                 bool
}
type Rules struct {
	ID              string
	Payload         json.RawMessage
	Hash, CreatedAt string
}
type Lineage struct {
	ID, ParentVersionID string
	Payload             json.RawMessage
	Hash, CreatedAt     string
}
type Provenance struct {
	ID              string
	Payload         json.RawMessage
	Hash, CreatedAt string
}
type BrainVersion struct {
	ID, BrainID     string
	Version         int64
	Rules           Rules
	Lineage         Lineage
	Provenance      Provenance
	Payload         json.RawMessage
	Hash, CreatedAt string
}
type CreateBrainInput struct {
	ID, Name, Description, Type string
	Frozen                      bool
}
type CreateBrainVersionInput struct {
	ID, BrainID                         string
	Version                             int64
	Rules, Lineage, Provenance, Payload json.RawMessage
	ParentVersionID                     string
}
type UpdateBrainVersionInput struct {
	BrainID, BaseVersionID              string
	Rules, Lineage, Provenance, Payload json.RawMessage
}
type BrainListOptions struct {
	Limit, Offset int
	Name, Type    string
	Frozen        *bool
}

func (s *Store) CreateBrain(ctx context.Context, in CreateBrainInput) (Brain, error) {
	if in.Name == "" {
		return Brain{}, fmt.Errorf("%w: brain name", ErrInvalidArgument)
	}
	id := in.ID
	if id == "" {
		id = newID()
	}
	t := now()
	_, err := s.db.ExecContext(ctx, "INSERT INTO brains(id,name,description,type,frozen,created_at) VALUES(?,?,?,?,?,?)", id, in.Name, in.Description, in.Type, boolInt(in.Frozen), t)
	if err != nil {
		return Brain{}, classify(err, "brain")
	}
	return Brain{ID: id, Name: in.Name, Description: in.Description, Type: in.Type, Frozen: in.Frozen, CreatedAt: t}, nil
}
func (s *Store) GetBrain(ctx context.Context, id string) (Brain, error) {
	var b Brain
	var frozen int
	err := s.db.QueryRowContext(ctx, "SELECT id,name,description,type,frozen,created_at FROM brains WHERE id=?", id).Scan(&b.ID, &b.Name, &b.Description, &b.Type, &frozen, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("%w: brain %s", ErrNotFound, id)
	}
	if err != nil {
		return b, classify(err, "brain")
	}
	b.Frozen = frozen != 0
	return b, nil
}
func (s *Store) LoadBrain(ctx context.Context, id string) (Brain, error) { return s.GetBrain(ctx, id) }
func (s *Store) ListBrains(ctx context.Context, o BrainListOptions) ([]Brain, error) {
	limit, offset := page(o.Limit, o.Offset)
	q := "SELECT id,name,description,type,frozen,created_at FROM brains"
	args := []any{}
	where := []string{}
	if o.Name != "" {
		where = append(where, "name=?")
		args = append(args, o.Name)
	}
	if o.Type != "" {
		where = append(where, "type=?")
		args = append(args, o.Type)
	}
	if o.Frozen != nil {
		where = append(where, "frozen=?")
		args = append(args, boolInt(*o.Frozen))
	}
	if len(where) != 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at,id LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, classify(err, "brain list")
	}
	defer func() { _ = rows.Close() }()
	var out []Brain
	for rows.Next() {
		var b Brain
		var frozen int
		if err = rows.Scan(&b.ID, &b.Name, &b.Description, &b.Type, &frozen, &b.CreatedAt); err != nil {
			return nil, classifyScan(err, "brain list")
		}
		b.Frozen = frozen != 0
		out = append(out, b)
	}
	if err = rows.Err(); err != nil {
		return nil, classify(err, "brain list")
	}
	return out, nil
}
func (s *Store) CreateBrainVersion(ctx context.Context, in CreateBrainVersionInput) (BrainVersion, error) {
	if in.BrainID == "" || in.Version <= 0 {
		return BrainVersion{}, fmt.Errorf("%w: brain/version", ErrInvalidArgument)
	}
	for _, p := range [][]byte{in.Rules, in.Lineage, in.Provenance, in.Payload} {
		if err := validatePayload(p); err != nil {
			return BrainVersion{}, err
		}
	}
	id := in.ID
	if id == "" {
		id = newID()
	}
	t := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BrainVersion{}, classify(err, "brain version")
	}
	defer func() { _ = tx.Rollback() }()
	var brainExists string
	if err = tx.QueryRowContext(ctx, "SELECT id FROM brains WHERE id=?", in.BrainID).Scan(&brainExists); errors.Is(err, sql.ErrNoRows) {
		return BrainVersion{}, fmt.Errorf("%w: brain %s", ErrNotFound, in.BrainID)
	} else if err != nil {
		return BrainVersion{}, classify(err, "brain")
	}
	if in.ParentVersionID != "" {
		var parentBrain string
		err = tx.QueryRowContext(ctx, "SELECT brain_id FROM brain_versions WHERE id=?", in.ParentVersionID).Scan(&parentBrain)
		if errors.Is(err, sql.ErrNoRows) {
			return BrainVersion{}, fmt.Errorf("%w: brain version %s", ErrNotFound, in.ParentVersionID)
		}
		if err != nil {
			return BrainVersion{}, classify(err, "brain lineage")
		}
		if parentBrain != in.BrainID {
			return BrainVersion{}, fmt.Errorf("%w: parent brain mismatch", ErrConflict)
		}
	}
	rh, lh, ph := hashBytes(in.Rules), hashBytes(in.Lineage), hashBytes(in.Provenance)
	vh := hashBytes(in.Payload)
	var rid, ruleCreated string
	err = tx.QueryRowContext(ctx, "SELECT id,created_at FROM brain_rules WHERE hash=? AND payload=?", rh, in.Rules).Scan(&rid, &ruleCreated)
	if errors.Is(err, sql.ErrNoRows) {
		rid, ruleCreated = newID(), t
		if _, err = tx.ExecContext(ctx, "INSERT INTO brain_rules(id,payload,hash,created_at) VALUES(?,?,?,?)", rid, in.Rules, rh, t); err != nil {
			return BrainVersion{}, classify(err, "brain rule")
		}
	} else if err != nil {
		return BrainVersion{}, classify(err, "brain rule")
	}
	lid, pid := newID(), newID()
	if _, err = tx.ExecContext(ctx, "INSERT INTO brain_lineages(id,parent_version_id,payload,hash,created_at) VALUES(?,?,?,?,?)", lid, nullString(in.ParentVersionID), in.Lineage, lh, t); err != nil {
		return BrainVersion{}, classify(err, "brain lineage")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO brain_provenance(id,payload,hash,created_at) VALUES(?,?,?,?)", pid, in.Provenance, ph, t); err != nil {
		return BrainVersion{}, classify(err, "brain provenance")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO brain_versions(id,brain_id,version,rules_id,lineage_id,provenance_id,payload,hash,created_at) VALUES(?,?,?,?,?,?,?,?,?)", id, in.BrainID, in.Version, rid, lid, pid, in.Payload, vh, t); err != nil {
		return BrainVersion{}, classify(err, "brain version")
	}
	if err = tx.Commit(); err != nil {
		return BrainVersion{}, classify(err, "brain version")
	}
	return BrainVersion{ID: id, BrainID: in.BrainID, Version: in.Version,
		Rules:      Rules{ID: rid, Payload: copyJSON(in.Rules), Hash: rh, CreatedAt: ruleCreated},
		Lineage:    Lineage{ID: lid, ParentVersionID: in.ParentVersionID, Payload: copyJSON(in.Lineage), Hash: lh, CreatedAt: t},
		Provenance: Provenance{ID: pid, Payload: copyJSON(in.Provenance), Hash: ph, CreatedAt: t},
		Payload:    copyJSON(in.Payload), Hash: vh, CreatedAt: t}, nil
}

// UpdateBrainVersion appends an immutable child only when BaseVersionID is the
// current version for BrainID. The previous version is never modified.
func (s *Store) UpdateBrainVersion(ctx context.Context, in UpdateBrainVersionInput) (BrainVersion, error) {
	if in.BrainID == "" || in.BaseVersionID == "" {
		return BrainVersion{}, fmt.Errorf("%w: brain/base version", ErrInvalidArgument)
	}
	var baseBrain string
	var baseVersion, currentVersion int64
	err := s.db.QueryRowContext(ctx, "SELECT brain_id,version FROM brain_versions WHERE id=?", in.BaseVersionID).Scan(&baseBrain, &baseVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return BrainVersion{}, fmt.Errorf("%w: brain version %s", ErrNotFound, in.BaseVersionID)
	}
	if err != nil {
		return BrainVersion{}, classify(err, "brain version")
	}
	if baseBrain != in.BrainID {
		return BrainVersion{}, fmt.Errorf("%w: base brain mismatch", ErrConflict)
	}
	err = s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM brain_versions WHERE brain_id=?", in.BrainID).Scan(&currentVersion)
	if err != nil {
		return BrainVersion{}, classify(err, "brain version")
	}
	if baseVersion != currentVersion {
		return BrainVersion{}, &ConflictError{Resource: "brain version", ID: in.BrainID, ExpectedSequence: baseVersion, ActualSequence: currentVersion}
	}
	return s.CreateBrainVersion(ctx, CreateBrainVersionInput{
		BrainID: in.BrainID, Version: currentVersion + 1, ParentVersionID: in.BaseVersionID,
		Rules: in.Rules, Lineage: in.Lineage, Provenance: in.Provenance, Payload: in.Payload,
	})
}
func (s *Store) GetBrainVersion(ctx context.Context, id string) (BrainVersion, error) {
	return s.scanBrainVersion(s.db.QueryRowContext(ctx, `SELECT v.id,v.brain_id,v.version,v.payload,v.hash,v.created_at,r.id,r.payload,r.hash,r.created_at,l.id,COALESCE(l.parent_version_id,''),l.payload,l.hash,l.created_at,p.id,p.payload,p.hash,p.created_at FROM brain_versions v JOIN brain_rules r ON r.id=v.rules_id JOIN brain_lineages l ON l.id=v.lineage_id JOIN brain_provenance p ON p.id=v.provenance_id WHERE v.id=?`, id), id)
}
func (s *Store) LoadBrainVersion(ctx context.Context, id string) (BrainVersion, error) {
	return s.GetBrainVersion(ctx, id)
}
func (s *Store) ListBrainVersions(ctx context.Context, brainID string, o BrainListOptions) ([]BrainVersion, error) {
	limit, offset := page(o.Limit, o.Offset)
	rows, err := s.db.QueryContext(ctx, `SELECT v.id,v.brain_id,v.version,v.payload,v.hash,v.created_at,r.id,r.payload,r.hash,r.created_at,l.id,COALESCE(l.parent_version_id,''),l.payload,l.hash,l.created_at,p.id,p.payload,p.hash,p.created_at FROM brain_versions v JOIN brain_rules r ON r.id=v.rules_id JOIN brain_lineages l ON l.id=v.lineage_id JOIN brain_provenance p ON p.id=v.provenance_id WHERE v.brain_id=? ORDER BY v.version LIMIT ? OFFSET ?`, brainID, limit, offset)
	if err != nil {
		return nil, classify(err, "brain version list")
	}
	defer func() { _ = rows.Close() }()
	out := []BrainVersion{}
	for rows.Next() {
		v, scanErr := s.scanBrainVersion(rows, "")
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, classify(err, "brain version list")
	}
	return out, nil
}
func (s *Store) scanBrainVersion(row interface{ Scan(...any) error }, id string) (BrainVersion, error) {
	var v BrainVersion
	var payload, rules, lineage, prov []byte
	var parent string
	err := row.Scan(&v.ID, &v.BrainID, &v.Version, &payload, &v.Hash, &v.CreatedAt, &v.Rules.ID, &rules, &v.Rules.Hash, &v.Rules.CreatedAt, &v.Lineage.ID, &parent, &lineage, &v.Lineage.Hash, &v.Lineage.CreatedAt, &v.Provenance.ID, &prov, &v.Provenance.Hash, &v.Provenance.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return v, fmt.Errorf("%w: brain version %s", ErrNotFound, id)
	}
	if err != nil {
		return v, classifyScan(err, "brain version")
	}
	for _, x := range []struct {
		p []byte
		n string
	}{{payload, "brain version"}, {rules, "rules"}, {lineage, "lineage"}, {prov, "provenance"}} {
		if err := validatePayload(x.p); err != nil {
			return v, &PayloadError{Err: err, Resource: x.n, ID: v.ID}
		}
	}
	if hashBytes(payload) != v.Hash || hashBytes(rules) != v.Rules.Hash ||
		hashBytes(lineage) != v.Lineage.Hash || hashBytes(prov) != v.Provenance.Hash {
		return v, &PayloadError{Err: fmt.Errorf("payload hash mismatch"), Resource: "brain version", ID: v.ID}
	}
	v.Payload = copyJSON(payload)
	v.Rules.Payload = copyJSON(rules)
	v.Lineage.ParentVersionID = parent
	v.Lineage.Payload = copyJSON(lineage)
	v.Provenance.Payload = copyJSON(prov)
	return v, nil
}

type BrainInspection struct {
	Brain    Brain
	Versions []BrainVersion
}

func (s *Store) InspectBrain(ctx context.Context, id string) (BrainInspection, error) {
	b, err := s.GetBrain(ctx, id)
	if err != nil {
		return BrainInspection{}, err
	}
	var versions []BrainVersion
	for offset := 0; ; offset += 1000 {
		page, err := s.ListBrainVersions(ctx, id, BrainListOptions{Limit: 1000, Offset: offset})
		if err != nil {
			return BrainInspection{}, err
		}
		versions = append(versions, page...)
		if len(page) < 1000 {
			break
		}
	}
	return BrainInspection{b, versions}, nil
}

type BrainUsage struct {
	BrainID        string
	VersionID      string
	GameID         string
	ParticipantID  string
	ReferenceCount int64
}

// BrainUsageCount counts persisted game and participant references without
// hydrating game/event payloads.
func (s *Store) BrainUsageCount(ctx context.Context, brainVersionID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM games WHERE brain_version_id=?)
		+ (SELECT count(*) FROM participants WHERE brain_version_id=?)`, brainVersionID, brainVersionID).Scan(&n)
	return n, classify(err, "brain usage")
}

func (s *Store) ListGamesReferencingBrain(ctx context.Context, brainID string, o GameListOptions) (out []Game, err error) {
	limit, offset := page(o.Limit, o.Offset)
	rows, err := s.db.QueryContext(ctx, `SELECT g.id FROM games g JOIN brain_versions v ON v.id=g.brain_version_id
		WHERE v.brain_id=? ORDER BY g.updated_at DESC,g.id LIMIT ? OFFSET ?`, brainID, limit, offset)
	if err != nil {
		return nil, classify(err, "brain references")
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			out = nil
			err = classify(closeErr, "brain references")
		}
	}()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, classifyScan(err, "brain references")
		}
		g, getErr := s.GetGame(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		out = append(out, g)
	}
	if err = rows.Err(); err != nil {
		return nil, classify(err, "brain references")
	}
	return out, nil
}

func (s *Store) ListRules(ctx context.Context, o PageOptions) (out []Rules, err error) {
	limit, offset := page(o.Limit, o.Offset)
	rows, err := s.db.QueryContext(ctx, "SELECT id,payload,hash,created_at FROM brain_rules ORDER BY created_at,id LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, classify(err, "rules list")
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			out = nil
			err = classify(closeErr, "rules list")
		}
	}()
	for rows.Next() {
		var r Rules
		var p []byte
		if err = rows.Scan(&r.ID, &p, &r.Hash, &r.CreatedAt); err != nil {
			return nil, classifyScan(err, "rules list")
		}
		if err = validatePayload(p); err != nil || hashBytes(p) != r.Hash {
			return nil, &PayloadError{Err: ErrCorruptPayload, Resource: "rules", ID: r.ID}
		}
		r.Payload = copyJSON(p)
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		return nil, classify(err, "rules list")
	}
	return out, nil
}

func (s *Store) ListProvenance(ctx context.Context, o PageOptions) (out []Provenance, err error) {
	limit, offset := page(o.Limit, o.Offset)
	rows, err := s.db.QueryContext(ctx, "SELECT id,payload,hash,created_at FROM brain_provenance ORDER BY created_at,id LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, classify(err, "provenance list")
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			out = nil
			err = classify(closeErr, "provenance list")
		}
	}()
	for rows.Next() {
		var p Provenance
		var raw []byte
		if err = rows.Scan(&p.ID, &raw, &p.Hash, &p.CreatedAt); err != nil {
			return nil, classifyScan(err, "provenance list")
		}
		if err = validatePayload(raw); err != nil || hashBytes(raw) != p.Hash {
			return nil, &PayloadError{Err: ErrCorruptPayload, Resource: "provenance", ID: p.ID}
		}
		p.Payload = copyJSON(raw)
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, classify(err, "provenance list")
	}
	return out, nil
}

func (s *Store) ListBrainUsage(ctx context.Context, versionID string, o PageOptions) ([]BrainUsage, error) {
	limit, offset := page(o.Limit, o.Offset)
	rows, err := s.db.QueryContext(ctx, `SELECT v.brain_id,v.id,g.id,'' FROM brain_versions v JOIN games g ON g.brain_version_id=v.id WHERE v.id=?
		UNION ALL SELECT v.brain_id,v.id,p.game_id,p.id FROM brain_versions v JOIN participants p ON p.brain_version_id=v.id WHERE v.id=?
		ORDER BY 3,4 LIMIT ? OFFSET ?`, versionID, versionID, limit, offset)
	if err != nil {
		return nil, classify(err, "brain usage list")
	}
	defer func() { _ = rows.Close() }()
	var out []BrainUsage
	for rows.Next() {
		var u BrainUsage
		if err = rows.Scan(&u.BrainID, &u.VersionID, &u.GameID, &u.ParticipantID); err != nil {
			return nil, classifyScan(err, "brain usage list")
		}
		u.ReferenceCount = 1
		out = append(out, u)
	}
	if err = rows.Err(); err != nil {
		return nil, classify(err, "brain usage list")
	}
	return out, nil
}

type BrainDiff struct {
	From, To                                                        BrainVersion
	RulesChanged, LineageChanged, ProvenanceChanged, PayloadChanged bool
}

func (s *Store) DiffBrainVersions(ctx context.Context, a, b string) (BrainDiff, error) {
	x, e := s.GetBrainVersion(ctx, a)
	if e != nil {
		return BrainDiff{}, e
	}
	y, e := s.GetBrainVersion(ctx, b)
	if e != nil {
		return BrainDiff{}, e
	}
	return BrainDiff{x, y, x.Rules.Hash != y.Rules.Hash, x.Lineage.Hash != y.Lineage.Hash, x.Provenance.Hash != y.Provenance.Hash, x.Hash != y.Hash}, nil
}

// Game persistence.
type Game struct {
	ID, BrainVersionID, Status, EventHash, CreatedAt, UpdatedAt string
	RulesPayload                                                json.RawMessage
	Seed, Sequence, MoveCount                                   int64
	Participants                                                []Participant
}
type Participant struct {
	GameID, ID, Name, BrainVersionID, Kind string
	Score                                  int64
	Slot                                   int
	Payload                                json.RawMessage
}
type CreateGameInput struct {
	ID, BrainVersionID, Status string
	RulesPayload               json.RawMessage
	Seed                       int64
	Participants               []Participant
}
type ParticipantUpdate struct {
	ID      string
	Score   int64
	Payload json.RawMessage
}
type UpdateGameInput struct {
	ID, Status       string
	ExpectedSequence int64
	ExpectedHash     string
	Participants     []ParticipantUpdate
	MoveCount        *int64
}
type GameListOptions struct {
	Status        string
	Limit, Offset int
}
type Event struct {
	GameID                    string
	Sequence                  int64
	Type                      string
	Payload                   json.RawMessage
	PrevHash, Hash, CreatedAt string
}
type EventInput struct {
	Type    string
	Payload json.RawMessage
}
type Snapshot struct {
	GameID          string
	Sequence        int64
	Payload         json.RawMessage
	Hash, CreatedAt string
}

func (s *Store) CreateGame(ctx context.Context, in CreateGameInput) (Game, error) {
	if in.RulesPayload == nil {
		in.RulesPayload = []byte(`{"version":1,"data":{}}`)
	}
	if err := validatePayload(in.RulesPayload); err != nil {
		return Game{}, err
	}
	id := in.ID
	if id == "" {
		id = newID()
	}
	st := in.Status
	if st == "" {
		st = "active"
	}
	t := now()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return Game{}, classify(e, "game")
	}
	defer func() { _ = tx.Rollback() }()
	if _, e = tx.ExecContext(ctx, "INSERT INTO games(id,brain_version_id,rules_payload,status,seed,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", id, nullString(in.BrainVersionID), in.RulesPayload, st, in.Seed, t, t); e != nil {
		return Game{}, classify(e, "game")
	}
	for slot, p := range in.Participants {
		if p.ID == "" {
			return Game{}, fmt.Errorf("%w: participant id", ErrInvalidArgument)
		}
		if p.Payload == nil {
			p.Payload = []byte(`{"version":1,"data":{}}`)
		}
		if e = validatePayload(p.Payload); e != nil {
			return Game{}, e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO participants(game_id,id,name,brain_version_id,kind,score,payload,slot) VALUES(?,?,?,?,?,?,?,?)", id, p.ID, p.Name, nullString(p.BrainVersionID), p.Kind, p.Score, p.Payload, slot); e != nil {
			return Game{}, classify(e, "participant")
		}
	}
	if e = tx.Commit(); e != nil {
		return Game{}, classify(e, "game")
	}
	return s.GetGame(ctx, id)
}
func (s *Store) GetGame(ctx context.Context, id string) (Game, error) {
	var g Game
	var rules []byte
	var brain sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT id,brain_version_id,rules_payload,status,seed,sequence,move_count,event_hash,created_at,updated_at FROM games WHERE id=?", id).Scan(&g.ID, &brain, &rules, &g.Status, &g.Seed, &g.Sequence, &g.MoveCount, &g.EventHash, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return g, fmt.Errorf("%w: game %s", ErrNotFound, id)
	}
	if err != nil {
		return g, classifyScan(err, "game")
	}
	g.BrainVersionID = brain.String
	g.RulesPayload = copyJSON(rules)
	if err = validatePayload(rules); err != nil {
		return g, &PayloadError{Err: err, Resource: "game", ID: id}
	}
	rows, e := s.db.QueryContext(ctx, "SELECT game_id,id,name,brain_version_id,kind,score,payload,slot FROM participants WHERE game_id=? ORDER BY slot,id", id)
	if e != nil {
		return g, classify(e, "participant list")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p Participant
		var bp sql.NullString
		var pl []byte
		if e = rows.Scan(&p.GameID, &p.ID, &p.Name, &bp, &p.Kind, &p.Score, &pl, &p.Slot); e != nil {
			return g, classifyScan(e, "participant")
		}
		p.BrainVersionID = bp.String
		if e = validatePayload(pl); e != nil {
			return g, &PayloadError{Err: e, Resource: "participant", ID: p.ID}
		}
		p.Payload = copyJSON(pl)
		g.Participants = append(g.Participants, p)
	}
	if e = rows.Err(); e != nil {
		return g, classify(e, "participant list")
	}
	return g, nil
}
func (s *Store) LoadGame(ctx context.Context, id string) (Game, error)   { return s.GetGame(ctx, id) }
func (s *Store) ResumeGame(ctx context.Context, id string) (Game, error) { return s.GetGame(ctx, id) }
func (s *Store) UpdateGame(ctx context.Context, in UpdateGameInput) error {
	if in.ID == "" {
		return fmt.Errorf("%w: game id", ErrInvalidArgument)
	}
	var status string
	if e := s.db.QueryRowContext(ctx, "SELECT status FROM games WHERE id=?", in.ID).Scan(&status); errors.Is(e, sql.ErrNoRows) {
		return fmt.Errorf("%w: game %s", ErrNotFound, in.ID)
	} else if e != nil {
		return classify(e, "game")
	}
	if status == "completed" || status == "cancelled" {
		return fmt.Errorf("%w: game %s", ErrImmutable, in.ID)
	}
	r, e := s.db.ExecContext(ctx, "UPDATE games SET status=?,updated_at=? WHERE id=? AND sequence=? AND event_hash=?", in.Status, now(), in.ID, in.ExpectedSequence, in.ExpectedHash)
	if e != nil {
		return classify(e, "game")
	}
	n, _ := r.RowsAffected()
	if n == 1 {
		return nil
	}
	g, e := s.GetGame(ctx, in.ID)
	if e != nil {
		return e
	}
	return &ConflictError{"game", in.ID, in.ExpectedSequence, g.Sequence, in.ExpectedHash, g.EventHash}
}

// CompleteGame atomically advances the optimistic game head, persists the
// authoritative participant scores, and records the actual move count.
func (s *Store) CompleteGame(ctx context.Context, id, status string, expectedSequence int64, expectedHash string, scores map[string]int64, moveCount int64) error {
	if id == "" || status == "" {
		return fmt.Errorf("%w: game completion", ErrInvalidArgument)
	}
	if moveCount < 0 {
		return fmt.Errorf("%w: move count", ErrInvalidArgument)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classify(err, "game completion")
	}
	defer func() { _ = tx.Rollback() }()
	var seq int64
	var hash, current string
	if err = tx.QueryRowContext(ctx, "SELECT sequence,event_hash,status FROM games WHERE id=?", id).Scan(&seq, &hash, &current); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: game %s", ErrNotFound, id)
	} else if err != nil {
		return classifyScan(err, "game completion")
	}
	if current == "completed" || current == "cancelled" {
		return fmt.Errorf("%w: game %s", ErrImmutable, id)
	}
	if seq != expectedSequence || hash != expectedHash {
		return &ConflictError{"game", id, expectedSequence, seq, expectedHash, hash}
	}
	for participantID, score := range scores {
		if participantID == "" || score < 0 {
			return fmt.Errorf("%w: participant score", ErrInvalidArgument)
		}
		result, execErr := tx.ExecContext(ctx, "UPDATE participants SET score=? WHERE game_id=? AND id=?", score, id, participantID)
		if execErr != nil {
			return classify(execErr, "participant score")
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return fmt.Errorf("%w: participant %s", ErrNotFound, participantID)
		}
	}
	result, err := tx.ExecContext(ctx, "UPDATE games SET status=?,move_count=?,updated_at=? WHERE id=? AND sequence=? AND event_hash=?", status, moveCount, now(), id, expectedSequence, expectedHash)
	if err != nil {
		return classify(err, "game completion")
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return &ConflictError{"game", id, expectedSequence, seq, expectedHash, hash}
	}
	if err = tx.Commit(); err != nil {
		return classify(err, "game completion")
	}
	return nil
}
func (s *Store) ListGames(ctx context.Context, o GameListOptions) ([]Game, error) {
	limit, offset := page(o.Limit, o.Offset)
	q := "SELECT id FROM games"
	args := []any{}
	if o.Status != "" {
		q += " WHERE status=?"
		args = append(args, o.Status)
	}
	q += " ORDER BY updated_at DESC,id LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, classify(e, "game list")
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			_ = rows.Close()
			return nil, classifyScan(e, "game list")
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		_ = rows.Close()
		return nil, classify(e, "game list")
	}
	if e = rows.Close(); e != nil {
		return nil, classify(e, "game list")
	}
	out := make([]Game, 0, len(ids))
	for _, id := range ids {
		g, getErr := s.GetGame(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *Store) AppendGameEvents(ctx context.Context, gameID string, expectedSequence int64, expectedHash string, inputs []EventInput) ([]Event, error) {
	if len(inputs) == 0 {
		return []Event{}, nil
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, classify(e, "event")
	}
	defer func() { _ = tx.Rollback() }()
	var seq int64
	var prev, status string
	if e = tx.QueryRowContext(ctx, "SELECT sequence,event_hash,status FROM games WHERE id=?", gameID).Scan(&seq, &prev, &status); errors.Is(e, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: game %s", ErrNotFound, gameID)
	} else if e != nil {
		return nil, classify(e, "game")
	}
	if status == "completed" || status == "cancelled" {
		return nil, fmt.Errorf("%w: game %s", ErrImmutable, gameID)
	}
	if seq != expectedSequence || prev != expectedHash {
		return nil, &ConflictError{"game", gameID, expectedSequence, seq, expectedHash, prev}
	}
	out := make([]Event, 0, len(inputs))
	for _, in := range inputs {
		if in.Type == "" {
			return nil, fmt.Errorf("%w: event type", ErrInvalidArgument)
		}
		if e = validatePayload(in.Payload); e != nil {
			return nil, e
		}
		seq++
		t := now()
		h := eventHash(seq, prev, in.Type, in.Payload)
		if _, e = tx.ExecContext(ctx, "INSERT INTO game_events(game_id,sequence,type,payload,prev_hash,hash,created_at) VALUES(?,?,?,?,?,?,?)", gameID, seq, in.Type, in.Payload, prev, h, t); e != nil {
			return nil, classify(e, "event")
		}
		out = append(out, Event{gameID, seq, in.Type, copyJSON(in.Payload), prev, h, t})
		prev = h
	}
	r, e := tx.ExecContext(ctx, "UPDATE games SET sequence=?,event_hash=?,updated_at=? WHERE id=? AND sequence=? AND event_hash=?", seq, prev, now(), gameID, expectedSequence, expectedHash)
	if e != nil {
		return nil, classify(e, "game")
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return nil, &ConflictError{"game", gameID, expectedSequence, seq, expectedHash, prev}
	}
	if e = tx.Commit(); e != nil {
		return nil, classify(e, "event")
	}
	return out, nil
}

// AppendGameEventsWithSnapshot atomically appends events, advances the game
// head, and records the resulting snapshot.
func (s *Store) AppendGameEventsWithSnapshot(ctx context.Context, gameID string, expectedSequence int64, expectedHash string, inputs []EventInput, snap Snapshot) ([]Event, error) {
	if err := validateSnapshot(gameID, &snap); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, classify(err, "event")
	}
	defer func() { _ = tx.Rollback() }()
	var seq int64
	var prev, status string
	if err = tx.QueryRowContext(ctx, "SELECT sequence,event_hash,status FROM games WHERE id=?", gameID).Scan(&seq, &prev, &status); errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: game %s", ErrNotFound, gameID)
	} else if err != nil {
		return nil, classify(err, "game")
	}
	if status == "completed" || status == "cancelled" {
		return nil, fmt.Errorf("%w: game %s", ErrImmutable, gameID)
	}
	if seq != expectedSequence || prev != expectedHash {
		return nil, &ConflictError{"game", gameID, expectedSequence, seq, expectedHash, prev}
	}
	if snap.Sequence != seq+int64(len(inputs)) {
		return nil, fmt.Errorf("%w: snapshot sequence", ErrInvalidArgument)
	}
	out := make([]Event, 0, len(inputs))
	for _, in := range inputs {
		if in.Type == "" {
			return nil, fmt.Errorf("%w: event type", ErrInvalidArgument)
		}
		if err = validatePayload(in.Payload); err != nil {
			return nil, err
		}
		seq++
		created := now()
		hash := eventHash(seq, prev, in.Type, in.Payload)
		if _, err = tx.ExecContext(ctx, "INSERT INTO game_events(game_id,sequence,type,payload,prev_hash,hash,created_at) VALUES(?,?,?,?,?,?,?)", gameID, seq, in.Type, in.Payload, prev, hash, created); err != nil {
			return nil, classify(err, "event")
		}
		out = append(out, Event{gameID, seq, in.Type, copyJSON(in.Payload), prev, hash, created})
		prev = hash
	}
	if len(inputs) != 0 {
		result, err := tx.ExecContext(ctx, "UPDATE games SET sequence=?,event_hash=?,updated_at=? WHERE id=? AND sequence=? AND event_hash=?", seq, prev, now(), gameID, expectedSequence, expectedHash)
		if err != nil {
			return nil, classify(err, "game")
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return nil, &ConflictError{"game", gameID, expectedSequence, seq, expectedHash, prev}
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO game_snapshots(game_id,sequence,payload,hash,created_at) VALUES(?,?,?,?,?)", gameID, snap.Sequence, snap.Payload, snap.Hash, now()); err != nil {
		return nil, classify(err, "snapshot")
	}
	if err = tx.Commit(); err != nil {
		return nil, classify(err, "event")
	}
	return out, nil
}

func validateSnapshot(gameID string, snap *Snapshot) error {
	if err := validatePayload(snap.Payload); err != nil {
		return err
	}
	if snap.GameID != "" && snap.GameID != gameID {
		return fmt.Errorf("%w: snapshot game id", ErrInvalidArgument)
	}
	snap.GameID = gameID
	expected := hashBytes(snap.Payload)
	if snap.Hash != "" && snap.Hash != expected {
		return &PayloadError{Err: fmt.Errorf("snapshot hash mismatch"), Resource: "snapshot", ID: gameID}
	}
	snap.Hash = expected
	return nil
}

func (s *Store) AppendEventsWithSnapshot(ctx context.Context, id string, expectedSequence int64, expectedHash string, events []EventInput, snap Snapshot) ([]Event, error) {
	return s.AppendGameEventsWithSnapshot(ctx, id, expectedSequence, expectedHash, events, snap)
}

func (s *Store) AppendEvents(ctx context.Context, id string, expectedSequence int64, expectedHash string, events []EventInput) ([]Event, error) {
	return s.AppendGameEvents(ctx, id, expectedSequence, expectedHash, events)
}
func (s *Store) ListEvents(ctx context.Context, id string, after int64, limit int) ([]Event, error) {
	limit, _ = page(limit, 0)
	rows, e := s.db.QueryContext(ctx, "SELECT game_id,sequence,type,payload,prev_hash,hash,created_at FROM game_events WHERE game_id=? AND sequence>? ORDER BY sequence LIMIT ?", id, after, limit)
	if e != nil {
		return nil, classify(e, "event list")
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var x Event
		var p []byte
		if e = rows.Scan(&x.GameID, &x.Sequence, &x.Type, &p, &x.PrevHash, &x.Hash, &x.CreatedAt); e != nil {
			return nil, classifyScan(e, "event list")
		}
		if e = validatePayload(p); e != nil {
			return nil, &PayloadError{Err: e, Resource: "event", ID: id}
		}
		if eventHash(x.Sequence, x.PrevHash, x.Type, p) != x.Hash {
			return nil, fmt.Errorf("%w: game %s event %d", ErrCorruptEvent, id, x.Sequence)
		}
		x.Payload = copyJSON(p)
		out = append(out, x)
	}
	if e = rows.Err(); e != nil {
		return nil, classify(e, "event list")
	}
	return out, nil
}
func (s *Store) SaveSnapshot(ctx context.Context, gameID string, snap Snapshot) error {
	if err := validateSnapshot(gameID, &snap); err != nil {
		return err
	}
	var status string
	err := s.db.QueryRowContext(ctx, "SELECT status FROM games WHERE id=?", gameID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: game %s", ErrNotFound, gameID)
	}
	if err != nil {
		return classify(err, "game")
	}
	if status == "completed" || status == "cancelled" {
		return fmt.Errorf("%w: game %s", ErrImmutable, gameID)
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO game_snapshots(game_id,sequence,payload,hash,created_at) VALUES(?,?,?,?,?)", gameID, snap.Sequence, snap.Payload, snap.Hash, now())
	return classify(err, "snapshot")
}

// SaveSnapshotOptimistic atomically checks the game head before recording a
// snapshot, preventing a stale resume writer from replacing a newer state.
func (s *Store) SaveSnapshotOptimistic(ctx context.Context, gameID string, expectedSequence int64, expectedHash string, snap Snapshot) error {
	if err := validateSnapshot(gameID, &snap); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classify(err, "snapshot")
	}
	defer func() { _ = tx.Rollback() }()
	var seq int64
	var hash, status string
	if err = tx.QueryRowContext(ctx, "SELECT sequence,event_hash,status FROM games WHERE id=?", gameID).Scan(&seq, &hash, &status); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: game %s", ErrNotFound, gameID)
	} else if err != nil {
		return classify(err, "game")
	}
	if status == "completed" || status == "cancelled" {
		return fmt.Errorf("%w: game %s", ErrImmutable, gameID)
	}
	if seq != expectedSequence || hash != expectedHash {
		return &ConflictError{"game", gameID, expectedSequence, seq, expectedHash, hash}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO game_snapshots(game_id,sequence,payload,hash,created_at) VALUES(?,?,?,?,?)", gameID, snap.Sequence, snap.Payload, snap.Hash, now()); err != nil {
		return classify(err, "snapshot")
	}
	return classify(tx.Commit(), "snapshot")
}
func (s *Store) LoadLatestSnapshot(ctx context.Context, gameID string) (Snapshot, error) {
	var x Snapshot
	var p []byte
	e := s.db.QueryRowContext(ctx, "SELECT game_id,sequence,payload,hash,created_at FROM game_snapshots WHERE game_id=? ORDER BY sequence DESC LIMIT 1", gameID).Scan(&x.GameID, &x.Sequence, &p, &x.Hash, &x.CreatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return x, fmt.Errorf("%w: snapshot %s", ErrNotFound, gameID)
	}
	if e != nil {
		return x, classify(e, "snapshot")
	}
	if e = validatePayload(p); e != nil {
		return x, &PayloadError{Err: e, Resource: "snapshot", ID: gameID}
	}
	if hashBytes(p) != x.Hash {
		return x, &PayloadError{Err: fmt.Errorf("snapshot hash mismatch"), Resource: "snapshot", ID: gameID}
	}
	x.Payload = copyJSON(p)
	return x, nil
}

// LoadLatestVerifiedSnapshot skips tampered snapshots and returns the newest
// snapshot whose envelope and content hash verify. It never writes recovery
// data, so callers can safely replay events when it returns ErrNotFound.
func (s *Store) LoadLatestVerifiedSnapshot(ctx context.Context, gameID string) (Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT game_id,sequence,payload,hash,created_at FROM game_snapshots WHERE game_id=? ORDER BY sequence DESC", gameID)
	if err != nil {
		return Snapshot{}, classify(err, "snapshot")
	}
	defer func() { _ = rows.Close() }()
	var saw bool
	for rows.Next() {
		saw = true
		var x Snapshot
		var p []byte
		if err = rows.Scan(&x.GameID, &x.Sequence, &p, &x.Hash, &x.CreatedAt); err != nil {
			return Snapshot{}, classifyScan(err, "snapshot")
		}
		if validatePayload(p) != nil || hashBytes(p) != x.Hash {
			continue
		}
		x.Payload = copyJSON(p)
		return x, nil
	}
	if err = rows.Err(); err != nil {
		return Snapshot{}, classify(err, "snapshot")
	}
	if saw {
		return Snapshot{}, &PayloadError{Err: ErrCorruptPayload, Resource: "snapshot", ID: gameID}
	}
	return Snapshot{}, fmt.Errorf("%w: snapshot %s", ErrNotFound, gameID)
}

// VerifyEventChain validates sequence continuity and hashes from the game's
// genesis through its persisted head.
func (s *Store) VerifyEventChain(ctx context.Context, gameID string) error {
	var headSequence int64
	var headHash string
	if err := s.db.QueryRowContext(ctx, "SELECT sequence,event_hash FROM games WHERE id=?", gameID).Scan(&headSequence, &headHash); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: game %s", ErrNotFound, gameID)
	} else if err != nil {
		return classifyScan(err, "game")
	}
	prev := ""
	var after int64
	for {
		events, err := s.ListEvents(ctx, gameID, after, 1000)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Sequence != after+1 || event.PrevHash != prev {
				return fmt.Errorf("%w: game %s event %d", ErrCorruptEvent, gameID, event.Sequence)
			}
			after, prev = event.Sequence, event.Hash
		}
		if len(events) < 1000 {
			break
		}
	}
	if after != headSequence || prev != headHash {
		return fmt.Errorf("%w: game %s head sequence/hash %d/%s, chain %d/%s", ErrCorruptEvent, gameID, headSequence, headHash, after, prev)
	}
	return nil
}

// Tournaments and matches.
type Tournament struct {
	ID, Name, Status, CreatedAt, UpdatedAt string
	RulesPayload                           json.RawMessage
}
type CreateTournamentInput struct {
	ID, Name, Status string
	RulesPayload     json.RawMessage
}
type Match struct {
	ID, TournamentID, GameID, Status, CreatedAt, UpdatedAt string
	Round                                                  int64
	Payload                                                json.RawMessage
}
type CreateMatchInput struct {
	ID, TournamentID, GameID, Status string
	Round                            int64
	Payload                          json.RawMessage
}
type TournamentListOptions struct{ Limit, Offset int }

func (s *Store) CreateTournament(ctx context.Context, in CreateTournamentInput) (Tournament, error) {
	if in.Name == "" {
		return Tournament{}, fmt.Errorf("%w: tournament name", ErrInvalidArgument)
	}
	if in.RulesPayload == nil {
		in.RulesPayload = []byte(`{"version":1,"data":{}}`)
	}
	if e := validatePayload(in.RulesPayload); e != nil {
		return Tournament{}, e
	}
	id := in.ID
	if id == "" {
		id = newID()
	}
	st := in.Status
	if st == "" {
		st = "pending"
	}
	t := now()
	_, e := s.db.ExecContext(ctx, "INSERT INTO tournaments(id,name,rules_payload,status,created_at,updated_at) VALUES(?,?,?,?,?,?)", id, in.Name, in.RulesPayload, st, t, t)
	return Tournament{id, in.Name, st, t, t, copyJSON(in.RulesPayload)}, classify(e, "tournament")
}
func (s *Store) GetTournament(ctx context.Context, id string) (Tournament, error) {
	var t Tournament
	var p []byte
	e := s.db.QueryRowContext(ctx, "SELECT id,name,rules_payload,status,created_at,updated_at FROM tournaments WHERE id=?", id).Scan(&t.ID, &t.Name, &p, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return t, fmt.Errorf("%w: tournament %s", ErrNotFound, id)
	}
	if e != nil {
		return t, classifyScan(e, "tournament")
	}
	if e = validatePayload(p); e != nil {
		return t, &PayloadError{Err: e, Resource: "tournament", ID: id}
	}
	t.RulesPayload = copyJSON(p)
	return t, nil
}
func (s *Store) ListTournaments(ctx context.Context, o TournamentListOptions) ([]Tournament, error) {
	limit, offset := page(o.Limit, o.Offset)
	rows, e := s.db.QueryContext(ctx, "SELECT id FROM tournaments ORDER BY updated_at DESC,id LIMIT ? OFFSET ?", limit, offset)
	if e != nil {
		return nil, classify(e, "tournament list")
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			_ = rows.Close()
			return nil, classifyScan(e, "tournament list")
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		_ = rows.Close()
		return nil, classify(e, "tournament list")
	}
	if e = rows.Close(); e != nil {
		return nil, classify(e, "tournament list")
	}
	out := make([]Tournament, 0, len(ids))
	for _, id := range ids {
		t, getErr := s.GetTournament(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		out = append(out, t)
	}
	return out, nil
}
func (s *Store) CreateMatch(ctx context.Context, in CreateMatchInput) (Match, error) {
	if in.TournamentID == "" {
		return Match{}, fmt.Errorf("%w: tournament id", ErrInvalidArgument)
	}
	if in.Payload == nil {
		in.Payload = []byte(`{"version":1,"data":{}}`)
	}
	if e := validatePayload(in.Payload); e != nil {
		return Match{}, e
	}
	id := in.ID
	if id == "" {
		id = newID()
	}
	st := in.Status
	if st == "" {
		st = "pending"
	}
	t := now()
	_, e := s.db.ExecContext(ctx, "INSERT INTO tournament_matches(id,tournament_id,game_id,round,status,payload,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)", id, in.TournamentID, nullString(in.GameID), in.Round, st, in.Payload, t, t)
	return Match{ID: id, TournamentID: in.TournamentID, GameID: in.GameID, Status: st, CreatedAt: t, UpdatedAt: t, Round: in.Round, Payload: copyJSON(in.Payload)}, classify(e, "match")
}
func (s *Store) GetMatch(ctx context.Context, id string) (Match, error) {
	var m Match
	var p []byte
	var g sql.NullString
	e := s.db.QueryRowContext(ctx, "SELECT id,tournament_id,game_id,round,status,payload,created_at,updated_at FROM tournament_matches WHERE id=?", id).Scan(&m.ID, &m.TournamentID, &g, &m.Round, &m.Status, &p, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return m, fmt.Errorf("%w: match %s", ErrNotFound, id)
	}
	if e != nil {
		return m, classifyScan(e, "match")
	}
	m.GameID = g.String
	if e = validatePayload(p); e != nil {
		return m, &PayloadError{Err: e, Resource: "match", ID: id}
	}
	m.Payload = copyJSON(p)
	return m, nil
}
func (s *Store) ListMatches(ctx context.Context, tournamentID string, o BrainListOptions) ([]Match, error) {
	limit, offset := page(o.Limit, o.Offset)
	rows, e := s.db.QueryContext(ctx, "SELECT id FROM tournament_matches WHERE tournament_id=? ORDER BY round,id LIMIT ? OFFSET ?", tournamentID, limit, offset)
	if e != nil {
		return nil, classify(e, "match list")
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, classifyScan(e, "match list")
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		return nil, classify(e, "match list")
	}
	out := make([]Match, 0, len(ids))
	for _, id := range ids {
		m, getErr := s.GetMatch(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		out = append(out, m)
	}
	return out, nil
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func page(limit, offset int) (int, int) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// WithTx runs fn atomically and rolls back on every error, including panics.
func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	if fn == nil {
		return fmt.Errorf("%w: nil transaction callback", ErrInvalidArgument)
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return classify(e, "transaction")
	}
	w := &Tx{tx: tx}
	defer func() {
		if v := recover(); v != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				panic(fmt.Errorf("transaction panic: %v; rollback: %w", v, rollbackErr))
			}
			panic(v)
		}
	}()
	if e = fn(w); e != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return classify(errors.Join(e, rollbackErr), "transaction")
		}
		return classify(e, "transaction")
	}
	return classify(tx.Commit(), "transaction")
}

type Tx struct{ tx *sql.Tx }

func (t *Tx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, q, args...)
}

// PageOptions is shared by list methods. Offset is the number of rows to skip.
type PageOptions = BrainListOptions
type MatchListOptions = BrainListOptions

func (s *Store) QueryBrains(ctx context.Context, o PageOptions) ([]Brain, error) {
	return s.ListBrains(ctx, o)
}
func (s *Store) QueryBrainVersions(ctx context.Context, brainID string, o PageOptions) ([]BrainVersion, error) {
	return s.ListBrainVersions(ctx, brainID, o)
}
func (s *Store) QueryGames(ctx context.Context, o GameListOptions) ([]Game, error) {
	return s.ListGames(ctx, o)
}
func (s *Store) QueryEvents(ctx context.Context, gameID string, after int64, limit int) ([]Event, error) {
	return s.ListEvents(ctx, gameID, after, limit)
}
func (s *Store) CreateSnapshot(ctx context.Context, gameID string, snapshot Snapshot) error {
	return s.SaveSnapshot(ctx, gameID, snapshot)
}
func (s *Store) LoadSnapshot(ctx context.Context, gameID string) (Snapshot, error) {
	return s.LoadLatestSnapshot(ctx, gameID)
}
func (s *Store) QueryMatches(ctx context.Context, tournamentID string, o MatchListOptions) ([]Match, error) {
	return s.ListMatches(ctx, tournamentID, o)
}

func (s *Store) QueryRules(ctx context.Context, o PageOptions) ([]Rules, error) {
	return s.ListRules(ctx, o)
}
func (s *Store) QueryProvenance(ctx context.Context, o PageOptions) ([]Provenance, error) {
	return s.ListProvenance(ctx, o)
}
func (s *Store) QueryBrainUsage(ctx context.Context, versionID string, o PageOptions) ([]BrainUsage, error) {
	return s.ListBrainUsage(ctx, versionID, o)
}

// InspectBrainPage returns a bounded, server/debug-friendly version page.
// filter is matched against the canonical payload columns.
func (s *Store) InspectBrainPage(ctx context.Context, brainID, versionID, filter string, offset, limit int) ([]BrainVersion, error) {
	limit, offset = page(limit, offset)
	q := `SELECT v.id,v.brain_id,v.version,v.payload,v.hash,v.created_at,r.id,r.payload,r.hash,r.created_at,l.id,COALESCE(l.parent_version_id,''),l.payload,l.hash,l.created_at,p.id,p.payload,p.hash,p.created_at
		FROM brain_versions v JOIN brain_rules r ON r.id=v.rules_id JOIN brain_lineages l ON l.id=v.lineage_id JOIN brain_provenance p ON p.id=v.provenance_id WHERE v.brain_id=?`
	args := []any{brainID}
	if versionID != "" {
		q += " AND v.id=?"
		args = append(args, versionID)
	}
	if filter != "" {
		q += " AND (CAST(v.payload AS TEXT) LIKE ? OR CAST(r.payload AS TEXT) LIKE ? OR CAST(p.payload AS TEXT) LIKE ?)"
		needle := "%" + filter + "%"
		args = append(args, needle, needle, needle)
	}
	q += " ORDER BY v.version LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, classify(err, "brain inspection")
	}
	defer func() { _ = rows.Close() }()
	out := make([]BrainVersion, 0, limit)
	for rows.Next() {
		v, scanErr := s.scanBrainVersion(rows, versionID)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, classify(err, "brain inspection")
	}
	return out, nil
}

// EncodeExtensionSnapshot stores the canonical extension snapshot in the same
// validated v1 payload envelope used by game snapshots.
func EncodeExtensionSnapshot(state extension.State) (json.RawMessage, error) {
	b, err := state.MarshalSnapshot()
	if err != nil {
		return nil, err
	}
	return EncodeSnapshot(json.RawMessage(b))
}

func DecodeExtensionSnapshot(raw json.RawMessage) (extension.State, error) {
	data, err := PayloadData(raw)
	if err != nil {
		return extension.State{}, &PayloadError{Err: err}
	}
	state, err := extension.UnmarshalSnapshot(data)
	if err != nil {
		return extension.State{}, &PayloadError{Err: err}
	}
	return state, nil
}

func (s *Store) SaveExtensionSnapshot(ctx context.Context, gameID string, state extension.State) error {
	// The engine tick is not the game event cursor: a single move may append
	// several events. Capture the verified head and write optimistically so a
	// concurrent event writer cannot leave a stale snapshot behind.
	g, err := s.GetGame(ctx, gameID)
	if err != nil {
		return err
	}
	state.GameEventSequence = g.Sequence
	state.GameEventHash = g.EventHash
	raw, err := EncodeExtensionSnapshot(state)
	if err != nil {
		return err
	}
	return s.SaveSnapshotOptimistic(ctx, gameID, g.Sequence, g.EventHash, Snapshot{
		GameID: gameID, Sequence: g.Sequence, Payload: raw, Hash: hashBytes(raw),
	})
}

func (s *Store) LoadExtensionSnapshot(ctx context.Context, gameID string) (extension.State, error) {
	snap, err := s.LoadLatestVerifiedSnapshot(ctx, gameID)
	if err != nil {
		return extension.State{}, err
	}
	return DecodeExtensionSnapshot(snap.Payload)
}
