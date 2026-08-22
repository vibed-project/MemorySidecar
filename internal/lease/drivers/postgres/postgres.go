// Package postgres implements the lease driver backed by PostgreSQL.
//
// Acquisition is atomic via INSERT … ON CONFLICT DO UPDATE WHERE the existing
// lease has expired. This avoids the per-session limitation of advisory
// locks while still keeping the whole protocol observable through SQL.
package postgres

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
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

	"github.com/vibed-project/mindD/internal/lease"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Options configures the driver.
type Options struct {
	DSN            string
	MaxConns       int32
	PollInterval   time.Duration // wait_for poll cadence; default 100ms
	SkipMigrations bool
}

// Driver implements lease.Driver against Postgres.
type Driver struct {
	pool   *pgxpool.Pool
	poll   time.Duration
	mu     sync.Mutex
	closed bool
}

func New(ctx context.Context, opts Options) (*Driver, error) {
	if opts.DSN == "" {
		return nil, errors.New("lease/postgres: dsn required")
	}
	pcfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("lease/postgres: parse dsn: %w", err)
	}
	if opts.MaxConns > 0 {
		pcfg.MaxConns = opts.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("lease/postgres: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("lease/postgres: ping: %w", err)
	}
	if !opts.SkipMigrations {
		if err := runMigrations(ctx, pool); err != nil {
			pool.Close()
			return nil, fmt.Errorf("lease/postgres: migrate: %w", err)
		}
	}
	poll := opts.PollInterval
	if poll == 0 {
		poll = 100 * time.Millisecond
	}
	return &Driver{pool: pool, poll: poll}, nil
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

func (d *Driver) Acquire(ctx context.Context, namespace, key string, opts lease.AcquireOptions) (lease.Lease, error) {
	deadline := time.Now().Add(opts.WaitFor)
	for {
		l, err := d.tryAcquire(ctx, namespace, key, opts)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, lease.ErrAlreadyHeld) {
			return lease.Lease{}, err
		}
		if opts.WaitFor <= 0 || !time.Now().Before(deadline) {
			return lease.Lease{}, lease.ErrAlreadyHeld
		}
		wait := d.poll
		if remain := time.Until(deadline); remain < wait {
			wait = remain
		}
		select {
		case <-ctx.Done():
			return lease.Lease{}, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (d *Driver) tryAcquire(ctx context.Context, namespace, key string, opts lease.AcquireOptions) (lease.Lease, error) {
	metaBytes, err := json.Marshal(opts.Metadata)
	if err != nil {
		return lease.Lease{}, fmt.Errorf("lease/postgres: marshal metadata: %w", err)
	}
	holderID := randomID()
	expires := time.Now().Add(opts.TTL).UTC()

	var (
		outHolder    string
		outAcquired  time.Time
		outExpires   time.Time
		outMeta      []byte
	)
	err = d.pool.QueryRow(ctx, `
		INSERT INTO leases (namespace, key, holder_id, acquired_at, expires_at, metadata)
		     VALUES ($1, $2, $3, now(), $4, $5)
		ON CONFLICT (namespace, key) DO UPDATE
		    SET holder_id  = EXCLUDED.holder_id,
		        acquired_at = now(),
		        expires_at = EXCLUDED.expires_at,
		        metadata   = EXCLUDED.metadata
		   WHERE leases.expires_at <= now()
		RETURNING holder_id, acquired_at, expires_at, metadata`,
		namespace, key, holderID, expires, metaBytes,
	).Scan(&outHolder, &outAcquired, &outExpires, &outMeta)
	if errors.Is(err, pgx.ErrNoRows) {
		return lease.Lease{}, lease.ErrAlreadyHeld
	}
	if err != nil {
		return lease.Lease{}, fmt.Errorf("lease/postgres: acquire: %w", err)
	}
	l := lease.Lease{
		HolderID:   outHolder,
		Namespace:  namespace,
		Key:        key,
		AcquiredAt: outAcquired,
		ExpiresAt:  outExpires,
	}
	if len(outMeta) > 0 {
		_ = json.Unmarshal(outMeta, &l.Metadata)
	}
	return l, nil
}

func (d *Driver) Renew(ctx context.Context, namespace, key, holderID string, ttl time.Duration) (lease.Lease, error) {
	newExp := time.Now().Add(ttl).UTC()
	var (
		acquired  time.Time
		expires   time.Time
		metaBytes []byte
	)
	err := d.pool.QueryRow(ctx, `
		UPDATE leases
		   SET expires_at = $1
		 WHERE namespace = $2 AND key = $3 AND holder_id = $4
		   AND expires_at > now()
		RETURNING acquired_at, expires_at, metadata`,
		newExp, namespace, key, holderID,
	).Scan(&acquired, &expires, &metaBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return lease.Lease{}, lease.ErrNotHeld
	}
	if err != nil {
		return lease.Lease{}, fmt.Errorf("lease/postgres: renew: %w", err)
	}
	l := lease.Lease{
		HolderID: holderID, Namespace: namespace, Key: key,
		AcquiredAt: acquired, ExpiresAt: expires,
	}
	if len(metaBytes) > 0 {
		_ = json.Unmarshal(metaBytes, &l.Metadata)
	}
	return l, nil
}

func (d *Driver) Release(ctx context.Context, namespace, key, holderID string) (bool, error) {
	// First check existence and ownership atomically so we can distinguish
	// "wrong holder" (ErrNotHeld) from "not present" (existed=false).
	var dbHolder string
	err := d.pool.QueryRow(ctx,
		`SELECT holder_id FROM leases WHERE namespace=$1 AND key=$2`,
		namespace, key,
	).Scan(&dbHolder)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lease/postgres: probe: %w", err)
	}
	if dbHolder != holderID {
		return false, lease.ErrNotHeld
	}
	ct, err := d.pool.Exec(ctx,
		`DELETE FROM leases WHERE namespace=$1 AND key=$2 AND holder_id=$3`,
		namespace, key, holderID)
	if err != nil {
		return false, fmt.Errorf("lease/postgres: release: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

func (d *Driver) Inspect(ctx context.Context, namespace, key string) (lease.Lease, bool, error) {
	var (
		holder    string
		acquired  time.Time
		expires   time.Time
		metaBytes []byte
	)
	err := d.pool.QueryRow(ctx, `
		SELECT holder_id, acquired_at, expires_at, metadata
		  FROM leases
		 WHERE namespace = $1 AND key = $2 AND expires_at > now()`,
		namespace, key,
	).Scan(&holder, &acquired, &expires, &metaBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return lease.Lease{}, false, nil
	}
	if err != nil {
		return lease.Lease{}, false, fmt.Errorf("lease/postgres: inspect: %w", err)
	}
	l := lease.Lease{
		HolderID: holder, Namespace: namespace, Key: key,
		AcquiredAt: acquired, ExpiresAt: expires,
	}
	if len(metaBytes) > 0 {
		_ = json.Unmarshal(metaBytes, &l.Metadata)
	}
	return l, true, nil
}

func (d *Driver) List(ctx context.Context, namespace string) ([]lease.Lease, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT key, holder_id, acquired_at, expires_at, metadata
		  FROM leases
		 WHERE namespace = $1 AND expires_at > now()
		 ORDER BY key`,
		namespace)
	if err != nil {
		return nil, fmt.Errorf("lease/postgres: list: %w", err)
	}
	defer rows.Close()

	var out []lease.Lease
	for rows.Next() {
		var (
			key       string
			holder    string
			acquired  time.Time
			expires   time.Time
			metaBytes []byte
		)
		if err := rows.Scan(&key, &holder, &acquired, &expires, &metaBytes); err != nil {
			return nil, fmt.Errorf("lease/postgres: scan: %w", err)
		}
		l := lease.Lease{
			HolderID: holder, Namespace: namespace, Key: key,
			AcquiredAt: acquired, ExpiresAt: expires,
		}
		if len(metaBytes) > 0 {
			_ = json.Unmarshal(metaBytes, &l.Metadata)
		}
		out = append(out, l)
	}
	return out, rows.Err()
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
		version := "lease_" + strings.TrimSuffix(name, ".up.sql")
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

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}
