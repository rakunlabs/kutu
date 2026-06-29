// Package storage is kutu's relational config persistence layer. Each
// config entity (registry namespace/repository, raw mount, proxy
// listener/server, hook) lives in its own table; document-shaped fields
// (proxy graph nodes/edges, hook targets, polymorphic mount config,
// registry policy) are JSONB columns. SQL is built with goqu and list
// endpoints are filtered/sorted/paged via rakunlabs/query.
//
// Registry ARTIFACTS are NOT stored here — they live on raw mounts via
// the blobstore/rawfs abstraction. These tables hold configuration only.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/doug-martin/goqu/v9"
	_ "github.com/doug-martin/goqu/v9/dialect/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/rakunlabs/muz"

	"github.com/rakunlabs/kutu/internal/secret/keymgr"
	"github.com/rakunlabs/kutu/internal/service"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// dialect builds postgres SQL; datasets are executed against the *sql.DB.
var dialect = goqu.Dialect("postgres")

// Config selects and tunes the PostgreSQL backend.
type Config struct {
	// DSN is the PostgreSQL connection string, e.g.
	// "postgres://user:pass@localhost:5432/kutu?sslmode=disable".
	DSN string `cfg:"dsn"`

	MaxOpenConns    int           `cfg:"max_open_conns" default:"10"`
	MaxIdleConns    int           `cfg:"max_idle_conns" default:"5"`
	ConnMaxLifetime time.Duration `cfg:"conn_max_lifetime" default:"1h"`
}

// Store is the PostgreSQL-backed relational config store. It implements
// service.Storage.
type Store struct {
	db *sql.DB
	// mgr drives at-rest sealing of secret columns (registry repo auth).
	// nil-safe everywhere it's read.
	mgr *keymgr.Manager
}

var _ service.Storage = (*Store)(nil)

// New opens the database, verifies connectivity and applies migrations.
// mgr (may be nil in tests) drives per-row sealing of secret columns.
func New(ctx context.Context, cfg *Config, mgr *keymgr.Manager) (*Store, error) {
	if cfg == nil || cfg.DSN == "" {
		return nil, errors.New("storage: postgres DSN is required (set KUTU_STORAGE_DSN)")
	}

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	slog.Info("storage ready", "backend", "postgres")
	return &Store{db: db, mgr: mgr}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	m := muz.Migrate{Path: "migrations", FS: migrationsFS, Extension: ".sql"}
	driver := &muz.SQLDriver{
		DB:      db,
		Dialect: muz.DialectPostgres,
		Table:   "kutu_migrations",
		LockKey: "kutu:migrations",
		Logger:  slog.Default(),
	}
	return m.Migrate(ctx, driver)
}

// Close releases the connection pool.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the underlying *sql.DB.
func (s *Store) DB() *sql.DB { return s.db }

// ── meta (singletons) ──

// GetMeta unmarshals the JSONB value stored under key into dest. The
// bool reports whether a row existed.
func (s *Store) GetMeta(ctx context.Context, key string, dest any) (bool, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kutu_meta WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get meta %q: %w", key, err)
	}
	if len(raw) == 0 {
		return true, nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return true, fmt.Errorf("decode meta %q: %w", key, err)
	}
	return true, nil
}

// SetMeta upserts the JSONB value under key.
func (s *Store) SetMeta(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode meta %q: %w", key, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO kutu_meta (key, value, updated_at, updated_by) VALUES ($1, $2, now(), $3)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by`,
		key, string(raw), actor(ctx),
	)
	if err != nil {
		return fmt.Errorf("set meta %q: %w", key, err)
	}
	return nil
}

// ── helpers ──

// actor returns the request actor (from the X-User header, threaded via
// context) for the updated_by audit column. Empty when unset.
func actor(ctx context.Context) string {
	return service.ActorFromContext(ctx)
}

// jsonbArg marshals v to a JSON string suitable for a JSONB column
// parameter (pgx encodes string → jsonb). Returns nil → SQL NULL when v
// is a nil slice/pointer/map so empty documents stay NULL.
func jsonbArg(v any) (any, error) {
	if isNilish(v) {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func isNilish(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []string:
		return len(t) == 0
	case []byte:
		return len(t) == 0
	default:
		return false
	}
}

// unmarshalJSONB decodes a JSONB/[]byte column into dest, tolerating
// NULL/empty.
func unmarshalJSONB(raw []byte, dest any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dest)
}

// execDS builds SQL from a goqu dataset (insert/update/delete) and runs it.
func (s *Store) execDS(ctx context.Context, ds interface {
	ToSQL() (string, []any, error)
}) (sql.Result, error) {
	q, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build sql: %w", err)
	}
	return s.db.ExecContext(ctx, q, args...)
}
