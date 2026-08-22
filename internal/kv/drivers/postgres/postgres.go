// Package postgres implements the KV driver backed by PostgreSQL via pgx/v5.
//
// Schema: a single table kv_items keyed by (namespace, key). TTL is enforced
// by a partial index on expires_at, lazy filtering on read, and a background
// sweeper that deletes expired rows on a configurable interval.
//
// Tenancy is enforced by capability scope above this driver — this driver
// does not partition data by tenant.
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

	"github.com/vibed-project/mindD/internal/kv"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Options configures the driver.
type Options struct {
	DSN             string
	MaxConns        int32
	SweeperInterval time.Duration // 0 → 5 min; negative → disabled
	SkipMigrations  bool
}

// Driver implements kv.Driver against Postgres.
type Driver struct {
	pool      *pgxpool.Pool
	stopSweep chan struct{}
	wg        sync.WaitGroup
}

// New opens a connection pool, runs embedded migrations (unless disabled), and
// starts the background sweeper. Caller owns the returned Driver and must
// Close() it.
func New(ctx context.Context, opts Options) (*Driver, error) {
	if opts.DSN == "" {
		return nil, errors.New("postgres: dsn required")
	}
	pcfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	if opts.MaxConns > 0 {
		pcfg.MaxConns = opts.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	if !opts.SkipMigrations {
		if err := runMigrations(ctx, pool); err != nil {
			pool.Close()
			return nil, fmt.Errorf("postgres: migrate: %w", err)
		}
	}

	sweep := opts.SweeperInterval
	if sweep == 0 {
		sweep = 5 * time.Minute
	}

	d := &Driver{pool: pool, stopSweep: make(chan struct{})}
	if sweep > 0 {
		d.wg.Add(1)
		go d.sweepLoop(sweep) //nolint:gosec // long-lived background sweeper; deliberately not request-scoped
	}
	return d, nil
}

func (d *Driver) Close() error {
	select {
	case <-d.stopSweep:
	default:
		close(d.stopSweep)
	}
	d.wg.Wait()
	d.pool.Close()
	return nil
}

func (d *Driver) sweepLoop(interval time.Duration) {
	defer d.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-d.stopSweep:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, _ = d.pool.Exec(ctx, `DELETE FROM kv_items WHERE expires_at IS NOT NULL AND expires_at <= now()`)
			cancel()
		}
	}
}

func (d *Driver) Get(ctx context.Context, namespace, key string) (kv.Record, error) {
	row := d.pool.QueryRow(ctx, `
		SELECT key, value, content_type, metadata, version, created_at, expires_at
		  FROM kv_items
		 WHERE namespace = $1 AND key = $2
		   AND (expires_at IS NULL OR expires_at > now())`,
		namespace, key)

	var (
		r          kv.Record
		metaBytes  []byte
		expires    *time.Time
	)
	err := row.Scan(&r.Key, &r.Value, &r.ContentType, &metaBytes, &r.Version, &r.CreatedAt, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return kv.Record{}, kv.ErrNotFound
	}
	if err != nil {
		return kv.Record{}, fmt.Errorf("postgres: get: %w", err)
	}
	if expires != nil {
		r.ExpiresAt = *expires
	}
	if len(metaBytes) > 0 {
		_ = json.Unmarshal(metaBytes, &r.Metadata)
	}
	return r, nil
}

func (d *Driver) Put(ctx context.Context, namespace, key string, opts kv.PutOptions) (kv.Record, error) {
	metaBytes, err := json.Marshal(opts.Metadata)
	if err != nil {
		return kv.Record{}, fmt.Errorf("postgres: marshal metadata: %w", err)
	}
	var expires *time.Time
	if opts.TTL > 0 {
		t := time.Now().Add(opts.TTL)
		expires = &t
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return kv.Record{}, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentVersion uint64
	var rowExists bool
	row := tx.QueryRow(ctx,
		`SELECT version FROM kv_items WHERE namespace=$1 AND key=$2 AND (expires_at IS NULL OR expires_at > now()) FOR UPDATE`,
		namespace, key)
	if err := row.Scan(&currentVersion); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return kv.Record{}, fmt.Errorf("postgres: select-for-update: %w", err)
		}
		rowExists = false
	} else {
		rowExists = true
	}

	if opts.IfVersion != nil {
		want := *opts.IfVersion
		got := currentVersion
		if !rowExists {
			got = 0
		}
		if want != got {
			return kv.Record{}, kv.ErrVersionMismatch
		}
	}

	nextVersion := currentVersion + 1
	if !rowExists {
		nextVersion = 1
	}

	var (
		outKey      string
		outValue    []byte
		outContent  string
		outMeta     []byte
		outVersion  uint64
		outCreated  time.Time
		outExpires  *time.Time
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO kv_items (namespace, key, value, content_type, metadata, version, expires_at)
		     VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (namespace, key) DO UPDATE
		    SET value = EXCLUDED.value,
		        content_type = EXCLUDED.content_type,
		        metadata = EXCLUDED.metadata,
		        version = EXCLUDED.version,
		        updated_at = now(),
		        expires_at = EXCLUDED.expires_at
		RETURNING key, value, content_type, metadata, version, created_at, expires_at`,
		namespace, key, opts.Value, opts.ContentType, metaBytes, nextVersion, expires,
	).Scan(&outKey, &outValue, &outContent, &outMeta, &outVersion, &outCreated, &outExpires)
	if err != nil {
		return kv.Record{}, fmt.Errorf("postgres: upsert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return kv.Record{}, fmt.Errorf("postgres: commit: %w", err)
	}

	rec := kv.Record{
		Key:         outKey,
		Value:       outValue,
		ContentType: outContent,
		Version:     outVersion,
		CreatedAt:   outCreated,
	}
	if outExpires != nil {
		rec.ExpiresAt = *outExpires
	}
	if len(outMeta) > 0 {
		_ = json.Unmarshal(outMeta, &rec.Metadata)
	}
	return rec, nil
}

func (d *Driver) Delete(ctx context.Context, namespace, key string, opts kv.DeleteOptions) (bool, error) {
	if opts.IfVersion == nil {
		ct, err := d.pool.Exec(ctx,
			`DELETE FROM kv_items WHERE namespace=$1 AND key=$2 AND (expires_at IS NULL OR expires_at > now())`,
			namespace, key)
		if err != nil {
			return false, fmt.Errorf("postgres: delete: %w", err)
		}
		return ct.RowsAffected() > 0, nil
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current uint64
	row := tx.QueryRow(ctx,
		`SELECT version FROM kv_items WHERE namespace=$1 AND key=$2 AND (expires_at IS NULL OR expires_at > now()) FOR UPDATE`,
		namespace, key)
	if err := row.Scan(&current); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("postgres: select-for-delete: %w", err)
		}
		if *opts.IfVersion != 0 {
			return false, kv.ErrVersionMismatch
		}
		return false, tx.Commit(ctx)
	}
	if *opts.IfVersion != current {
		return false, kv.ErrVersionMismatch
	}
	_, err = tx.Exec(ctx, `DELETE FROM kv_items WHERE namespace=$1 AND key=$2`, namespace, key)
	if err != nil {
		return false, fmt.Errorf("postgres: cas-delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("postgres: commit: %w", err)
	}
	return true, nil
}

func (d *Driver) MultiGet(ctx context.Context, namespace string, keys []string) ([]kv.Record, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	// key = ANY($2) fetches every requested key in one query and naturally
	// deduplicates repeats. ORDER BY key gives the caller a deterministic order.
	rows, err := d.pool.Query(ctx, `
		SELECT key, value, content_type, metadata, version, created_at, expires_at
		  FROM kv_items
		 WHERE namespace = $1 AND key = ANY($2)
		   AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY key COLLATE "C"`,
		namespace, keys)
	if err != nil {
		return nil, fmt.Errorf("postgres: multiget: %w", err)
	}
	defer rows.Close()

	var out []kv.Record
	for rows.Next() {
		var (
			r         kv.Record
			metaBytes []byte
			expires   *time.Time
		)
		if err := rows.Scan(&r.Key, &r.Value, &r.ContentType, &metaBytes, &r.Version, &r.CreatedAt, &expires); err != nil {
			return nil, fmt.Errorf("postgres: multiget row: %w", err)
		}
		if expires != nil {
			r.ExpiresAt = *expires
		}
		if len(metaBytes) > 0 {
			_ = json.Unmarshal(metaBytes, &r.Metadata)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *Driver) Scan(ctx context.Context, namespace string, opts kv.ScanOptions, yield func(kv.Record) error) error {
	args := []any{namespace}
	q := strings.Builder{}
	q.WriteString(`SELECT key, value, content_type, metadata, version, created_at, expires_at
	                 FROM kv_items
	                WHERE namespace = $1
	                  AND (expires_at IS NULL OR expires_at > now())`)
	if opts.KeyPrefix != "" {
		args = append(args, opts.KeyPrefix+"%")
		fmt.Fprintf(&q, " AND key LIKE $%d", len(args))
	}
	if opts.StartAfter != "" {
		// COLLATE "C" forces byte ordering so the keyset cursor is consistent
		// with the memory driver's sort.Strings regardless of the DB locale.
		args = append(args, opts.StartAfter)
		fmt.Fprintf(&q, ` AND key COLLATE "C" > $%d`, len(args))
	}
	q.WriteString(` ORDER BY key COLLATE "C"`)
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		fmt.Fprintf(&q, " LIMIT $%d", len(args))
	}

	rows, err := d.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return fmt.Errorf("postgres: scan: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			r         kv.Record
			metaBytes []byte
			expires   *time.Time
		)
		if err := rows.Scan(&r.Key, &r.Value, &r.ContentType, &metaBytes, &r.Version, &r.CreatedAt, &expires); err != nil {
			return fmt.Errorf("postgres: scan row: %w", err)
		}
		if expires != nil {
			r.ExpiresAt = *expires
		}
		if !opts.IncludeValues {
			r.Value = nil
		}
		if len(metaBytes) > 0 {
			_ = json.Unmarshal(metaBytes, &r.Metadata)
		}
		if err := yield(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// runMigrations applies any embedded migrations not yet recorded in
// schema_migrations. Kept intentionally minimal (no golang-migrate dep) so the
// walking-skeleton image stays small.
func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version   text PRIMARY KEY,
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
		version := strings.TrimSuffix(name, ".up.sql")
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
