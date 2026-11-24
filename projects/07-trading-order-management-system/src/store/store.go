// Package store is the PostgreSQL persistence layer: the event log, its
// projections, the transactional outbox, the broker-event inbox, idempotency
// records and the reconciliation exception queue.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"oms/src/clock"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	// ErrNotFound is returned when a row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrVersionConflict is returned when an order changed underneath a writer.
	// Writers lock the order row first, so seeing this means a bug or a write
	// that bypassed the store.
	ErrVersionConflict = errors.New("order version conflict")
)

// Options configures a Store.
type Options struct {
	// MaxConns bounds the connection pool. Zero uses the pgx default.
	MaxConns int32
	// LockTimeout bounds how long a statement waits for a row or table lock
	// before failing. A failed write is reported to the caller, who retries;
	// it is never silently dropped. Default 5s.
	LockTimeout time.Duration
	// StatementTimeout bounds any single statement. Default 30s.
	StatementTimeout time.Duration
	// Clock stamps every write. Default is the system clock.
	Clock clock.Clock
}

// Store is a handle on the OMS database.
type Store struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// Open connects to the database. It does not migrate; call Migrate.
func Open(ctx context.Context, url string, opts Options) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = 5 * time.Second
	}
	if opts.StatementTimeout <= 0 {
		opts.StatementTimeout = 30 * time.Second
	}
	if opts.Clock == nil {
		opts.Clock = clock.System{}
	}
	rp := cfg.ConnConfig.RuntimeParams
	rp["timezone"] = "UTC"
	rp["application_name"] = "oms"
	rp["lock_timeout"] = strconv.FormatInt(opts.LockTimeout.Milliseconds(), 10)
	rp["statement_timeout"] = strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool, clock: opts.Clock}, nil
}

// Close releases every connection.
func (s *Store) Close() { s.pool.Close() }

// Ping checks that the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool exposes the connection pool for tests and diagnostics.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Now is the store's clock.
func (s *Store) Now() time.Time { return s.clock.Now() }

// migrationLockKey is an arbitrary constant for pg_advisory_lock so that
// several processes starting together apply migrations one at a time.
const migrationLockKey = 0x6f6d73_0001

// Migrate applies every embedded migration that has not been applied yet.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(migrationLockKey)); err != nil {
		return fmt.Errorf("store: migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(migrationLockKey))

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		var applied bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name, applied_at) VALUES ($1, $2)`, name, s.clock.Now()); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Tx is one database transaction. Every write the OMS makes goes through a
// Tx, so that events, projections and outbox rows commit or roll back as one.
type Tx struct {
	tx pgx.Tx
	// Now is the single timestamp used for every row this transaction writes.
	Now time.Time
}

// Tx runs fn in a transaction. It commits if fn returns nil and rolls back
// otherwise, returning fn's error unchanged.
func (s *Store) Tx(ctx context.Context, fn func(tx *Tx) error) error {
	ptx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	// Roll back even if the request was cancelled.
	defer ptx.Rollback(context.WithoutCancel(ctx))
	if err := fn(&Tx{tx: ptx, Now: s.clock.Now()}); err != nil {
		return err
	}
	if err := ptx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// querier is satisfied by both the pool and a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// IsUniqueViolation reports whether err is a unique-constraint failure on the
// named constraint (any constraint when name is empty).
func IsUniqueViolation(err error, name string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	return name == "" || pgErr.ConstraintName == name
}

func utc(t time.Time) time.Time { return t.UTC() }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
