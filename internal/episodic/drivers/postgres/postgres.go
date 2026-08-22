// Package postgres implements the episodic driver backed by PostgreSQL.
//
// Cursors are assigned per-namespace from a counter row in episodic_cursors,
// bumped atomically with INSERT ... ON CONFLICT DO UPDATE ... RETURNING. That
// takes a row-level lock on just the namespace's counter, so concurrent
// appenders to the same namespace serialize while different namespaces stay
// independent — all without the illegal `SELECT max(cursor) ... FOR UPDATE`
// (Postgres forbids FOR UPDATE with aggregate functions). Tail is implemented
// as polling — LISTEN/NOTIFY is the natural upgrade path.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vibed-project/mindD/internal/episodic"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const defaultTailInterval = 250 * time.Millisecond

// Options configures the driver.
type Options struct {
	DSN            string
	MaxConns       int32
	TailInterval   time.Duration // 0 → 250ms; minimum 50ms
	SkipMigrations bool
}

// Driver implements episodic.Driver against Postgres.
type Driver struct {
	pool         *pgxpool.Pool
	tailInterval time.Duration

	mu     sync.Mutex
	closed bool
}

// New opens a connection pool, runs embedded migrations (unless disabled), and
// returns a Driver. Caller owns the returned Driver and must Close() it.
func New(ctx context.Context, opts Options) (*Driver, error) {
	if opts.DSN == "" {
		return nil, errors.New("episodic/postgres: dsn required")
	}
	pcfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("episodic/postgres: parse dsn: %w", err)
	}
	if opts.MaxConns > 0 {
		pcfg.MaxConns = opts.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("episodic/postgres: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("episodic/postgres: ping: %w", err)
	}

	if !opts.SkipMigrations {
		if err := runMigrations(ctx, pool); err != nil {
			pool.Close()
			return nil, fmt.Errorf("episodic/postgres: migrate: %w", err)
		}
	}

	interval := opts.TailInterval
	if interval == 0 {
		interval = defaultTailInterval
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}

	return &Driver{pool: pool, tailInterval: interval}, nil
}

func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	d.pool.Close()
	return nil
}

// querier is the subset of pgxpool.Pool / pgx.Tx used for single-row reads, so
// selectByDedup can run either inside a tx or on the pool.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// eventColumns is the ordered projection scanEvent expects. source/supersedes
// are coalesced so a NULL on a pre-lifecycle row reads as "" / empty slice;
// deleted_at stays nullable (NULL = live).
const eventColumns = `id, cursor, timestamp, type, payload, metadata, role, session_id, ` +
	`COALESCE(source, ''), COALESCE(supersedes, '{}'), deleted_at`

func (d *Driver) selectByDedup(ctx context.Context, q querier, namespace, dedupKey string) (episodic.Event, bool, error) {
	row := q.QueryRow(ctx,
		`SELECT `+eventColumns+` FROM episodic_events WHERE namespace = $1 AND dedup_key = $2`,
		namespace, dedupKey)
	ev, err := scanEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return episodic.Event{}, false, nil
	}
	if err != nil {
		return episodic.Event{}, false, err
	}
	return ev, true, nil
}

func (d *Driver) Append(ctx context.Context, namespace string, opts episodic.AppendOptions) (episodic.Event, error) {
	metaBytes, err := json.Marshal(opts.Metadata)
	if err != nil {
		return episodic.Event{}, fmt.Errorf("episodic/postgres: marshal metadata: %w", err)
	}

	// Read Committed is sufficient: the cursor bump below takes a row lock on the
	// namespace's counter, which fully serializes same-namespace appends without
	// risking serialization-failure retries under concurrency.
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return episodic.Event{}, fmt.Errorf("episodic/postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotent replay fast path: if this dedup key already claimed an event,
	// return it without burning a cursor.
	if opts.DedupKey != "" {
		if ev, found, derr := d.selectByDedup(ctx, tx, namespace, opts.DedupKey); derr != nil {
			return episodic.Event{}, fmt.Errorf("episodic/postgres: dedup lookup: %w", derr)
		} else if found {
			return ev, nil
		}
	}

	// Atomically allocate the next cursor for this namespace. INSERT ... ON
	// CONFLICT DO UPDATE locks the counter row, so concurrent appenders to the
	// same namespace block here rather than racing on a shared MAX. Running
	// inside the tx means an aborted append rolls the increment back, leaving no
	// cursor gap.
	var next int64
	err = tx.QueryRow(ctx, `
		INSERT INTO episodic_cursors (namespace, last_cursor)
		     VALUES ($1, 1)
		ON CONFLICT (namespace)
		  DO UPDATE SET last_cursor = episodic_cursors.last_cursor + 1
		  RETURNING last_cursor`,
		namespace).Scan(&next)
	if err != nil {
		return episodic.Event{}, fmt.Errorf("episodic/postgres: allocate cursor: %w", err)
	}

	// The payload column is NOT NULL; a nil []byte would be encoded as SQL NULL
	// (the column DEFAULT only applies when the column is omitted, not when NULL
	// is passed), so coalesce an absent payload to empty bytes.
	payload := opts.Payload
	if payload == nil {
		payload = []byte{}
	}

	// ON CONFLICT DO NOTHING guards the dedup-race window: if a concurrent append
	// committed the same (namespace, dedup_key) after our fast-path read, the
	// insert returns no row — we roll back (releasing the cursor) and read the
	// winner. Rows with no dedup_key aren't in the partial index and never
	// conflict.
	row := tx.QueryRow(ctx, `
		INSERT INTO episodic_events (namespace, cursor, type, payload, metadata, role, session_id, source, supersedes, dedup_key)
		     VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (namespace, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING
		RETURNING `+eventColumns,
		namespace, next, opts.Type, payload, metaBytes, opts.Role, opts.SessionID,
		nullString(opts.Source), opts.Supersedes, nullString(opts.DedupKey))
	ev, err := scanEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		existing, found, derr := d.selectByDedup(ctx, d.pool, namespace, opts.DedupKey)
		if derr != nil {
			return episodic.Event{}, fmt.Errorf("episodic/postgres: dedup lookup: %w", derr)
		}
		if !found {
			return episodic.Event{}, fmt.Errorf("episodic/postgres: insert conflicted but no existing event for dedup key")
		}
		return existing, nil
	}
	if err != nil {
		return episodic.Event{}, fmt.Errorf("episodic/postgres: insert: %w", err)
	}

	// Tombstone superseded live events in the same tx, so the revision and the
	// invalidation commit atomically. id::text comparison sidesteps uuid casts
	// and silently ignores non-matching / malformed ids; the self-reference and
	// already-tombstoned guards mirror the memory driver.
	if len(opts.Supersedes) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE episodic_events
			   SET deleted_at = $1
			 WHERE namespace = $2 AND id::text = ANY($3) AND id::text <> $4 AND deleted_at IS NULL`,
			ev.Timestamp, namespace, opts.Supersedes, ev.ID); err != nil {
			return episodic.Event{}, fmt.Errorf("episodic/postgres: supersede: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return episodic.Event{}, fmt.Errorf("episodic/postgres: commit: %w", err)
	}
	return ev, nil
}

func (d *Driver) Range(ctx context.Context, namespace string, opts episodic.RangeOptions, yield func(episodic.Event) error) error {
	args := []any{namespace}
	q := strings.Builder{}
	q.WriteString(`SELECT ` + eventColumns + `
	                 FROM episodic_events
	                WHERE namespace = $1`)
	if !opts.IncludeDeleted {
		q.WriteString(" AND deleted_at IS NULL")
	}
	if opts.AfterCursor > 0 {
		args = append(args, int64(opts.AfterCursor))
		fmt.Fprintf(&q, " AND cursor > $%d", len(args))
	}
	if opts.BeforeCursor > 0 {
		args = append(args, int64(opts.BeforeCursor))
		fmt.Fprintf(&q, " AND cursor < $%d", len(args))
	}
	if !opts.AfterTime.IsZero() {
		args = append(args, opts.AfterTime)
		fmt.Fprintf(&q, " AND timestamp > $%d", len(args))
	}
	if !opts.BeforeTime.IsZero() {
		args = append(args, opts.BeforeTime)
		fmt.Fprintf(&q, " AND timestamp < $%d", len(args))
	}
	if opts.SessionID != "" {
		args = append(args, opts.SessionID)
		fmt.Fprintf(&q, " AND session_id = $%d", len(args))
	}
	if opts.Role != "" {
		args = append(args, opts.Role)
		fmt.Fprintf(&q, " AND role = $%d", len(args))
	}
	if opts.Type != "" {
		args = append(args, opts.Type)
		fmt.Fprintf(&q, " AND type = $%d", len(args))
	}
	if opts.Reverse {
		q.WriteString(" ORDER BY cursor DESC")
	} else {
		q.WriteString(" ORDER BY cursor ASC")
	}
	if opts.Limit > 0 {
		args = append(args, int32(opts.Limit))
		fmt.Fprintf(&q, " LIMIT $%d", len(args))
	}

	rows, err := d.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return fmt.Errorf("episodic/postgres: range: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return err
		}
		if err := yield(ev); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (d *Driver) Tail(ctx context.Context, namespace string, opts episodic.TailOptions, yield func(episodic.Event) error) error {
	lastCursor := opts.AfterCursor

	if opts.IncludeHistorical {
		err := d.Range(ctx, namespace, episodic.RangeOptions{
			AfterCursor: lastCursor,
		}, func(ev episodic.Event) error {
			lastCursor = ev.Cursor
			return yield(ev)
		})
		if err != nil {
			return err
		}
	} else {
		// Skip directly to current head so we only stream new events.
		var head *int64
		err := d.pool.QueryRow(ctx,
			`SELECT MAX(cursor) FROM episodic_events WHERE namespace = $1`,
			namespace).Scan(&head)
		if err != nil {
			return fmt.Errorf("episodic/postgres: head: %w", err)
		}
		if head != nil {
			lastCursor = uint64(*head)
		}
	}

	ticker := time.NewTicker(d.tailInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			err := d.Range(ctx, namespace, episodic.RangeOptions{
				AfterCursor: lastCursor,
			}, func(ev episodic.Event) error {
				lastCursor = ev.Cursor
				return yield(ev)
			})
			if err != nil {
				return err
			}
		}
	}
}

// scannable is satisfied by both pgx.Row (single-row QueryRow) and pgx.Rows.
type scannable interface {
	Scan(dest ...any) error
}

func scanEvent(row scannable) (episodic.Event, error) {
	var (
		ev        episodic.Event
		cursor    int64
		metaBytes []byte
		deletedAt *time.Time
	)
	err := row.Scan(&ev.ID, &cursor, &ev.Timestamp, &ev.Type, &ev.Payload, &metaBytes,
		&ev.Role, &ev.SessionID, &ev.Source, &ev.Supersedes, &deletedAt)
	if err != nil {
		return episodic.Event{}, fmt.Errorf("episodic/postgres: scan: %w", err)
	}
	ev.Cursor = uint64(cursor)
	if len(metaBytes) > 0 {
		_ = json.Unmarshal(metaBytes, &ev.Metadata)
	}
	if deletedAt != nil {
		ev.DeletedAt = *deletedAt
	}
	return ev, nil
}

func (d *Driver) Expire(ctx context.Context, namespace string, opts episodic.ExpireOptions) (uint64, error) {
	if opts.Action != episodic.ExpireSoftDelete && opts.Action != episodic.ExpireHardDelete {
		return 0, fmt.Errorf("episodic/postgres: unknown expire action %d", opts.Action)
	}
	if opts.BeforeCursor == 0 && opts.BeforeTime.IsZero() {
		return 0, episodic.ErrExpireWindowRequired
	}

	// Retention window predicate, oldest-first and capped by max_rows via a
	// LIMIT subselect so the whole sweep stays one localized statement.
	where := strings.Builder{}
	where.WriteString("namespace = $1")
	args := []any{namespace}
	if opts.BeforeCursor > 0 {
		args = append(args, int64(opts.BeforeCursor))
		fmt.Fprintf(&where, " AND cursor < $%d", len(args))
	}
	if !opts.BeforeTime.IsZero() {
		args = append(args, opts.BeforeTime)
		fmt.Fprintf(&where, " AND timestamp < $%d", len(args))
	}

	var stmt string
	if opts.Action == episodic.ExpireSoftDelete {
		// Only tombstone currently-live rows so affected counts stay idempotent.
		args = append(args, int32(opts.MaxRows))
		stmt = fmt.Sprintf(`
			UPDATE episodic_events SET deleted_at = now()
			 WHERE id IN (
			     SELECT id FROM episodic_events
			      WHERE %s AND deleted_at IS NULL
			      ORDER BY cursor ASC
			      LIMIT $%d)`, where.String(), len(args))
	} else {
		args = append(args, int32(opts.MaxRows))
		stmt = fmt.Sprintf(`
			DELETE FROM episodic_events
			 WHERE id IN (
			     SELECT id FROM episodic_events
			      WHERE %s
			      ORDER BY cursor ASC
			      LIMIT $%d)`, where.String(), len(args))
	}

	ct, err := d.pool.Exec(ctx, stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("episodic/postgres: expire: %w", err)
	}
	return uint64(ct.RowsAffected()), nil
}

// nullString maps "" to a SQL NULL so optional text columns store "unset" as
// NULL rather than an empty string.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		version := "episodic_" + strings.TrimSuffix(name, ".up.sql")
		var applied bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrationsFS.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			return err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
