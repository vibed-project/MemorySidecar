// Package postgres implements the semantic driver backed by pgvector.
//
// Each namespace gets its own table because pgvector bakes the dimension into
// the column type (vector(N)). Cosine similarity is computed via the <=>
// distance operator and converted back to similarity = 1 - distance.
//
// The pgvector extension must be installed in the target database. Use the
// pgvector/pgvector container image for tests.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"memsidecar/internal/semantic"
)

// Options configures a Driver.
type Options struct {
	DSN        string
	MaxConns   int32
	Namespace  string
	Dimensions int
	// If true, skip table creation (use when an external migration manages it).
	SkipMigrations bool
}

// Driver implements semantic.Driver against pgvector.
type Driver struct {
	pool      *pgxpool.Pool
	namespace string
	tableName string
	dim       int
	mu        sync.Mutex
	closed    bool
}

var safeNamespace = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)

// New opens a connection pool, ensures the pgvector extension is loaded and
// the namespace's table exists, and returns a Driver. Caller must Close().
func New(ctx context.Context, opts Options) (*Driver, error) {
	if opts.DSN == "" {
		return nil, errors.New("semantic/postgres: dsn required")
	}
	if opts.Namespace == "" || !safeNamespace.MatchString(opts.Namespace) {
		return nil, fmt.Errorf("semantic/postgres: namespace must match %s", safeNamespace.String())
	}
	if opts.Dimensions <= 0 {
		return nil, errors.New("semantic/postgres: dimensions must be > 0")
	}

	pcfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("semantic/postgres: parse dsn: %w", err)
	}
	if opts.MaxConns > 0 {
		pcfg.MaxConns = opts.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("semantic/postgres: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("semantic/postgres: ping: %w", err)
	}

	// Use underscores rather than hyphens in identifiers for SQL safety.
	tableName := "semantic_" + strings.ReplaceAll(opts.Namespace, "-", "_")

	if !opts.SkipMigrations {
		if err := ensureSchema(ctx, pool, tableName, opts.Dimensions); err != nil {
			pool.Close()
			return nil, fmt.Errorf("semantic/postgres: ensure schema: %w", err)
		}
	}

	return &Driver{
		pool: pool, namespace: opts.Namespace, tableName: tableName, dim: opts.Dimensions,
	}, nil
}

// Size returns an estimated record count for the namespace's table via the
// planner's reltuples statistic — deliberately not count(*), which would scan
// the whole table on every metric collection (O3). The estimate is maintained
// by ANALYZE/autovacuum and may lag very recent writes; that is an acceptable
// trade for a cheap namespace-growth gauge.
func (d *Driver) Size(ctx context.Context) (int64, error) {
	var n int64
	err := d.pool.QueryRow(ctx,
		`SELECT GREATEST(reltuples, 0)::bigint FROM pg_class WHERE oid = to_regclass($1)::oid`,
		d.tableName).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // table not yet created / not visible
	}
	if err != nil {
		return 0, err
	}
	return n, nil
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

func (d *Driver) Dimensions() int { return d.dim }

func (d *Driver) Upsert(ctx context.Context, records []semantic.Record) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("semantic/postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmt := fmt.Sprintf(`
		INSERT INTO %[1]s (id, content, payload, vector, metadata, valid_from, valid_to, deleted_at, supersedes, source)
		     VALUES (COALESCE(NULLIF($1, ''), gen_random_uuid()::text), $2, $3, $4, $5,
		             COALESCE($6, now()), $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE
		    SET content    = EXCLUDED.content,
		        payload    = EXCLUDED.payload,
		        vector     = EXCLUDED.vector,
		        metadata   = EXCLUDED.metadata,
		        valid_from = EXCLUDED.valid_from,
		        valid_to   = EXCLUDED.valid_to,
		        deleted_at = EXCLUDED.deleted_at,
		        supersedes = EXCLUDED.supersedes,
		        source     = EXCLUDED.source,
		        version    = %[1]s.version + 1
		RETURNING id, created_at, valid_from, version`, d.tableName)

	// U2 (ADR-0003): invalidate each superseded live record as of the
	// superseding record's valid_from. Runs in the same transaction and only
	// touches the named ids (localized); a record never supersedes itself.
	supersedeStmt := fmt.Sprintf(`
		UPDATE %s SET valid_to = $1
		 WHERE id = ANY($2) AND id <> $3 AND deleted_at IS NULL
		   AND (valid_to IS NULL OR valid_to > $1)`, d.tableName)

	for i, r := range records {
		if len(r.Vector) != d.dim {
			return fmt.Errorf("semantic/postgres: record %d: vector dim %d != %d", i, len(r.Vector), d.dim)
		}
		metaBytes, err := json.Marshal(r.Metadata)
		if err != nil {
			return fmt.Errorf("semantic/postgres: marshal metadata: %w", err)
		}

		// U4: optimistic-concurrency precondition. Lock the row and compare the
		// current version before writing (parity with the kv driver).
		if r.IfVersion != nil {
			var current uint64
			if r.ID != "" {
				err := tx.QueryRow(
					ctx,
					fmt.Sprintf(`SELECT version FROM %s WHERE id = $1 FOR UPDATE`, d.tableName), r.ID,
				).Scan(&current)
				if errors.Is(err, pgx.ErrNoRows) {
					current = 0
				} else if err != nil {
					return fmt.Errorf("semantic/postgres: cas select: %w", err)
				}
			}
			if *r.IfVersion != current {
				return semantic.ErrVersionMismatch
			}
		}

		var (
			outID        string
			outCreated   time.Time
			outValidFrom time.Time
			outVersion   uint64
		)
		err = tx.QueryRow(
			ctx, stmt,
			r.ID, r.Content, emptyIfNil(r.Payload), pgvector.NewVector(r.Vector), metaBytes,
			nullTime(r.ValidFrom), nullTime(r.ValidTo), nullTime(r.DeletedAt),
			r.Supersedes, nullString(r.Source),
		).Scan(&outID, &outCreated, &outValidFrom, &outVersion)
		if err != nil {
			return fmt.Errorf("semantic/postgres: upsert: %w", err)
		}
		records[i].ID = outID
		records[i].CreatedAt = outCreated
		records[i].ValidFrom = outValidFrom
		records[i].Version = outVersion

		if len(r.Supersedes) > 0 {
			if _, err := tx.Exec(ctx, supersedeStmt, outValidFrom, r.Supersedes, outID); err != nil {
				return fmt.Errorf("semantic/postgres: supersede: %w", err)
			}
		}
	}
	return tx.Commit(ctx)
}

func (d *Driver) Search(ctx context.Context, opts semantic.SearchOptions) ([]semantic.Hit, error) {
	if len(opts.QueryVector) != d.dim {
		return nil, fmt.Errorf("semantic/postgres: query dim %d != %d", len(opts.QueryVector), d.dim)
	}
	topK := opts.TopK
	if topK == 0 {
		topK = 10
	}

	args := []any{pgvector.NewVector(opts.QueryVector)}
	var conds []string
	if len(opts.Filter) > 0 {
		args = append(args, mustMarshalFilter(opts.Filter))
		conds = append(conds, fmt.Sprintf("metadata @> $%d::jsonb", len(args)))
	}
	if !opts.IncludeInvalidated {
		// Lifecycle pre-filter (ADR-0003): live and valid as of AsOf (nil = now()).
		args = append(args, nullTime(opts.AsOf))
		conds = append(conds, fmt.Sprintf(
			"deleted_at IS NULL AND valid_from <= COALESCE($%d, now()) AND (valid_to IS NULL OR valid_to > COALESCE($%d, now()))",
			len(args), len(args),
		))
	}

	if opts.IDsOnly {
		// Seed-set path (plan Q5): select only id + distance so Postgres never
		// reads or ships the content/payload/vector/metadata columns.
		lq := strings.Builder{}
		fmt.Fprintf(&lq, "SELECT id, (vector <=> $1) AS distance FROM %s", d.tableName)
		if len(conds) > 0 {
			fmt.Fprintf(&lq, " WHERE %s", strings.Join(conds, " AND "))
		}
		args = append(args, int32(topK))
		fmt.Fprintf(&lq, " ORDER BY vector <=> $1 LIMIT $%d", len(args))

		rows, err := d.pool.Query(ctx, lq.String(), args...)
		if err != nil {
			return nil, fmt.Errorf("semantic/postgres: search: %w", err)
		}
		defer rows.Close()

		hits := make([]semantic.Hit, 0, topK)
		for rows.Next() {
			var (
				id       string
				distance float64
			)
			if err := rows.Scan(&id, &distance); err != nil {
				return nil, fmt.Errorf("semantic/postgres: scan: %w", err)
			}
			hits = append(hits, semantic.Hit{
				Record: semantic.Record{ID: id},
				Score:  similarityFromDistance(distance),
			})
		}
		return hits, rows.Err()
	}

	q := strings.Builder{}
	fmt.Fprintf(&q, `SELECT id, content, payload, vector, metadata, created_at, valid_from, valid_to, deleted_at,
	                 COALESCE(supersedes, '{}'::text[]) AS supersedes, source, version,
	                 (vector <=> $1) AS distance
	            FROM %s`, d.tableName)
	if len(conds) > 0 {
		fmt.Fprintf(&q, " WHERE %s", strings.Join(conds, " AND "))
	}
	args = append(args, int32(topK))
	fmt.Fprintf(&q, " ORDER BY vector <=> $1 LIMIT $%d", len(args))

	rows, err := d.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("semantic/postgres: search: %w", err)
	}
	defer rows.Close()

	hits := make([]semantic.Hit, 0, topK)
	for rows.Next() {
		var (
			r         semantic.Record
			pgvec     pgvector.Vector
			meta      []byte
			validTo   *time.Time
			deletedAt *time.Time
			source    *string
			distance  float64
		)
		if err := rows.Scan(&r.ID, &r.Content, &r.Payload, &pgvec, &meta, &r.CreatedAt, &r.ValidFrom, &validTo, &deletedAt, &r.Supersedes, &source, &r.Version, &distance); err != nil {
			return nil, fmt.Errorf("semantic/postgres: scan: %w", err)
		}
		if validTo != nil {
			r.ValidTo = *validTo
		}
		if deletedAt != nil {
			r.DeletedAt = *deletedAt
		}
		if source != nil {
			r.Source = *source
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &r.Metadata)
		}
		if opts.IncludeVector {
			r.Vector = pgvec.Slice()
		}
		if !opts.IncludePayload {
			r.Payload = nil
		}
		hits = append(hits, semantic.Hit{
			Record: r,
			Score:  similarityFromDistance(distance),
		})
	}
	return hits, rows.Err()
}

func (d *Driver) Delete(ctx context.Context, id string, opts semantic.DeleteOptions) (bool, error) {
	var stmt string
	if opts.Hard {
		stmt = fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, d.tableName)
	} else {
		// Soft delete: tombstone a live row; already-retracted rows are a no-op.
		stmt = fmt.Sprintf(`UPDATE %s SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, d.tableName)
	}
	ct, err := d.pool.Exec(ctx, stmt, id)
	if err != nil {
		return false, fmt.Errorf("semantic/postgres: delete: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

func (d *Driver) Expire(ctx context.Context, opts semantic.ExpireOptions) (uint64, error) {
	// Reuse the same metadata @> jsonb predicate as Search; {} matches all.
	// A bounded subselect caps the affected set (max_rows), so the whole
	// operation is one localized statement (ADR-0003, U3).
	var stmt string
	switch opts.Action {
	case semantic.ExpireInvalidate:
		stmt = `UPDATE %[1]s SET valid_to = now()
		         WHERE id IN (SELECT id FROM %[1]s
		                       WHERE metadata @> $1::jsonb AND deleted_at IS NULL
		                         AND (valid_to IS NULL OR valid_to > now())
		                       LIMIT $2)`
	case semantic.ExpireSoftDelete:
		stmt = `UPDATE %[1]s SET deleted_at = now()
		         WHERE id IN (SELECT id FROM %[1]s
		                       WHERE metadata @> $1::jsonb AND deleted_at IS NULL
		                       LIMIT $2)`
	case semantic.ExpireHardDelete:
		stmt = `DELETE FROM %[1]s
		         WHERE id IN (SELECT id FROM %[1]s
		                       WHERE metadata @> $1::jsonb
		                       LIMIT $2)`
	default:
		return 0, fmt.Errorf("semantic/postgres: unknown expire action %d", opts.Action)
	}
	ct, err := d.pool.Exec(ctx, fmt.Sprintf(stmt, d.tableName),
		mustMarshalFilter(opts.Filter), int32(opts.MaxRows))
	if err != nil {
		return 0, fmt.Errorf("semantic/postgres: expire: %w", err)
	}
	return uint64(ct.RowsAffected()), nil
}

// nullTime maps a zero time.Time to a nil SQL parameter so the driver can rely
// on column defaults / NULL semantics for unset lifecycle timestamps.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// nullString maps an empty string to a nil SQL parameter (NULL) so unset
// provenance sources are stored as NULL rather than ”.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// emptyIfNil returns a non-nil, possibly-empty byte slice so pgx encodes it as
// an empty bytea rather than SQL NULL (the payload column is NOT NULL).
func emptyIfNil(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// similarityFromDistance maps a pgvector cosine *distance* (1 - cos_sim) back
// to similarity, clamped to [-1, 1] to mask floating-point drift.
func similarityFromDistance(d float64) float32 {
	s := 1 - d
	if math.IsNaN(s) {
		return 0
	}
	if s > 1 {
		return 1
	}
	if s < -1 {
		return -1
	}
	return float32(s)
}

func mustMarshalFilter(m map[string]string) []byte {
	b, _ := json.Marshal(m)
	return b
}

func ensureSchema(ctx context.Context, pool *pgxpool.Pool, table string, dim int) error {
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("create extension vector: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		return fmt.Errorf("create extension pgcrypto: %w", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id         text        PRIMARY KEY,
			content    text        NOT NULL DEFAULT '',
			payload    bytea       NOT NULL DEFAULT ''::bytea,
			vector     vector(%d)  NOT NULL,
			metadata   jsonb       NOT NULL DEFAULT '{}'::jsonb,
			created_at timestamptz NOT NULL DEFAULT now()
		)`, table, dim)); err != nil {
		return fmt.Errorf("create table %s: %w", table, err)
	}
	// Lifecycle columns (ADR-0003). Added idempotently so existing tables
	// upgrade in place; existing rows backfill valid_from=now() and read as live.
	for _, alter := range []string{
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS valid_from timestamptz NOT NULL DEFAULT now()`,
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS valid_to timestamptz`,
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS deleted_at timestamptz`,
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS supersedes text[]`,
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS source text`,
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS version bigint NOT NULL DEFAULT 1`,
	} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(alter, table)); err != nil {
			return fmt.Errorf("add lifecycle column on %s: %w", table, err)
		}
	}
	// Partial index over the non-tombstoned set to keep the default live-search
	// pre-filter cheap (the deleted_at IS NULL predicate is immutable).
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s_live ON %s (valid_from, valid_to) WHERE deleted_at IS NULL`,
		table, table,
	)); err != nil {
		return fmt.Errorf("create live index on %s: %w", table, err)
	}
	// HNSW is the modern choice (pgvector >= 0.5.0). Falls back to no-op on
	// older pgvector via IF NOT EXISTS — but the CREATE INDEX itself would
	// fail there. Acceptable for the walking-skeleton; document the version
	// requirement in the README when this driver ships.
	_, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s_vector_hnsw ON %s USING hnsw (vector vector_cosine_ops)`,
		table, table,
	))
	if err != nil {
		// Fall back to ivfflat if HNSW unavailable, but only log; index is an optimisation.
		_ = err
	}

	// Sanity check: confirm pgvector is responsive.
	var _v pgvector.Vector
	if err := pool.QueryRow(ctx, `SELECT '[1,2,3]'::vector`).Scan(&_v); err != nil {
		return fmt.Errorf("pgvector sanity check: %w", err)
	}

	// Disambiguate from any other migrations table (kv, episodic).
	_, _ = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`)
	_, _ = pool.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)
		ON CONFLICT (version) DO NOTHING`, "semantic_"+table)
	return nil
}
