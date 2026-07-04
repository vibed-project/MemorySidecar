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

	"memsidecar/internal/episodic"
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

	var (
		outID        string
		outCursor    int64
		outTimestamp time.Time
		outType      string
		outPayload   []byte
		outMeta      []byte
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO episodic_events (namespace, cursor, type, payload, metadata)
		     VALUES ($1, $2, $3, $4, $5)
		RETURNING id, cursor, timestamp, type, payload, metadata`,
		namespace, next, opts.Type, payload, metaBytes,
	).Scan(&outID, &outCursor, &outTimestamp, &outType, &outPayload, &outMeta)
	if err != nil {
		return episodic.Event{}, fmt.Errorf("episodic/postgres: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return episodic.Event{}, fmt.Errorf("episodic/postgres: commit: %w", err)
	}

	ev := episodic.Event{
		ID:        outID,
		Cursor:    uint64(outCursor),
		Timestamp: outTimestamp,
		Type:      outType,
		Payload:   outPayload,
	}
	if len(outMeta) > 0 {
		_ = json.Unmarshal(outMeta, &ev.Metadata)
	}
	return ev, nil
}

func (d *Driver) Range(ctx context.Context, namespace string, opts episodic.RangeOptions, yield func(episodic.Event) error) error {
	args := []any{namespace}
	q := strings.Builder{}
	q.WriteString(`SELECT id, cursor, timestamp, type, payload, metadata
	                 FROM episodic_events
	                WHERE namespace = $1`)
	if opts.AfterCursor > 0 {
		args = append(args, int64(opts.AfterCursor))
		fmt.Fprintf(&q, " AND cursor > $%d", len(args))
	}
	if opts.BeforeCursor > 0 {
		args = append(args, int64(opts.BeforeCursor))
		fmt.Fprintf(&q, " AND cursor < $%d", len(args))
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

func scanEvent(rows pgx.Rows) (episodic.Event, error) {
	var (
		ev        episodic.Event
		cursor    int64
		metaBytes []byte
	)
	err := rows.Scan(&ev.ID, &cursor, &ev.Timestamp, &ev.Type, &ev.Payload, &metaBytes)
	if err != nil {
		return episodic.Event{}, fmt.Errorf("episodic/postgres: scan: %w", err)
	}
	ev.Cursor = uint64(cursor)
	if len(metaBytes) > 0 {
		_ = json.Unmarshal(metaBytes, &ev.Metadata)
	}
	return ev, nil
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
