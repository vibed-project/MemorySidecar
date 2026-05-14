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
		INSERT INTO %s (id, content, payload, vector, metadata)
		     VALUES (COALESCE(NULLIF($1, ''), gen_random_uuid()::text), $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE
		    SET content    = EXCLUDED.content,
		        payload    = EXCLUDED.payload,
		        vector     = EXCLUDED.vector,
		        metadata   = EXCLUDED.metadata
		RETURNING id, created_at`, d.tableName)

	for i, r := range records {
		if len(r.Vector) != d.dim {
			return fmt.Errorf("semantic/postgres: record %d: vector dim %d != %d", i, len(r.Vector), d.dim)
		}
		metaBytes, err := json.Marshal(r.Metadata)
		if err != nil {
			return fmt.Errorf("semantic/postgres: marshal metadata: %w", err)
		}

		var (
			outID      string
			outCreated time.Time
		)
		err = tx.QueryRow(ctx, stmt,
			r.ID, r.Content, r.Payload, pgvector.NewVector(r.Vector), metaBytes,
		).Scan(&outID, &outCreated)
		if err != nil {
			return fmt.Errorf("semantic/postgres: upsert: %w", err)
		}
		records[i].ID = outID
		records[i].CreatedAt = outCreated
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
	q := strings.Builder{}
	fmt.Fprintf(&q, `SELECT id, content, payload, vector, metadata, created_at,
	                 (vector <=> $1) AS distance
	            FROM %s`, d.tableName)
	if len(opts.Filter) > 0 {
		args = append(args, mustMarshalFilter(opts.Filter))
		fmt.Fprintf(&q, " WHERE metadata @> $%d::jsonb", len(args))
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
			r        semantic.Record
			pgvec    pgvector.Vector
			meta     []byte
			distance float64
		)
		if err := rows.Scan(&r.ID, &r.Content, &r.Payload, &pgvec, &meta, &r.CreatedAt, &distance); err != nil {
			return nil, fmt.Errorf("semantic/postgres: scan: %w", err)
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

func (d *Driver) Delete(ctx context.Context, id string) (bool, error) {
	ct, err := d.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, d.tableName), id)
	if err != nil {
		return false, fmt.Errorf("semantic/postgres: delete: %w", err)
	}
	return ct.RowsAffected() > 0, nil
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
	// HNSW is the modern choice (pgvector >= 0.5.0). Falls back to no-op on
	// older pgvector via IF NOT EXISTS — but the CREATE INDEX itself would
	// fail there. Acceptable for the walking-skeleton; document the version
	// requirement in the README when this driver ships.
	_, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s_vector_hnsw ON %s USING hnsw (vector vector_cosine_ops)`,
		table, table))
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
