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
	// TextSearch is the Postgres full-text config for the sparse lane of hybrid
	// search (e.g. "simple", "english"). Empty defaults to "simple".
	TextSearch string
	// If true, skip table creation (use when an external migration manages it).
	SkipMigrations bool
}

// Driver implements semantic.Driver against pgvector.
type Driver struct {
	pool       *pgxpool.Pool
	namespace  string
	tableName  string
	dim        int
	textSearch string
	mu         sync.Mutex
	closed     bool
}

var safeNamespace = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)

// safeTextSearch bounds the full-text config to a plain lowercase identifier so
// it can be inlined into the FTS expression (which an index can't parameterize)
// without any injection surface.
var safeTextSearch = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

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
	textSearch := opts.TextSearch
	if textSearch == "" {
		textSearch = "simple"
	}
	if !safeTextSearch.MatchString(textSearch) {
		return nil, fmt.Errorf("semantic/postgres: text_search must match %s", safeTextSearch.String())
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
		if err := ensureSchema(ctx, pool, tableName, opts.Dimensions, textSearch); err != nil {
			pool.Close()
			return nil, fmt.Errorf("semantic/postgres: ensure schema: %w", err)
		}
	}

	return &Driver{
		pool: pool, namespace: opts.Namespace, tableName: tableName,
		dim: opts.Dimensions, textSearch: textSearch,
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
		INSERT INTO %[1]s (id, content, payload, vector, metadata, valid_from, valid_to, deleted_at, supersedes, source, tenant)
		     VALUES (COALESCE(NULLIF($1, ''), gen_random_uuid()::text), $2, $3, $4, $5,
		             COALESCE($6, now()), $7, $8, $9, $10, $11)
		ON CONFLICT (tenant, id) DO UPDATE
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
		 WHERE tenant = $4 AND id = ANY($2) AND id <> $3 AND deleted_at IS NULL
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
					fmt.Sprintf(`SELECT version FROM %s WHERE tenant = $2 AND id = $1 FOR UPDATE`, d.tableName), r.ID, r.Tenant,
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
			r.Supersedes, nullString(r.Source), r.Tenant,
		).Scan(&outID, &outCreated, &outValidFrom, &outVersion)
		if err != nil {
			return fmt.Errorf("semantic/postgres: upsert: %w", err)
		}
		records[i].ID = outID
		records[i].CreatedAt = outCreated
		records[i].ValidFrom = outValidFrom
		records[i].Version = outVersion

		if len(r.Supersedes) > 0 {
			if _, err := tx.Exec(ctx, supersedeStmt, outValidFrom, r.Supersedes, outID, r.Tenant); err != nil {
				return fmt.Errorf("semantic/postgres: supersede: %w", err)
			}
		}
	}
	return tx.Commit(ctx)
}

func (d *Driver) Search(ctx context.Context, opts semantic.SearchOptions) ([]semantic.Hit, error) {
	dense := opts.Mode == semantic.ModeDense || opts.Mode == semantic.ModeHybrid
	if dense && len(opts.QueryVector) != d.dim {
		return nil, fmt.Errorf("semantic/postgres: query dim %d != %d", len(opts.QueryVector), d.dim)
	}
	topK := opts.TopK
	if topK == 0 {
		topK = 10
	}
	switch opts.Mode {
	case semantic.ModeSparse:
		return d.searchSparse(ctx, opts, int(topK))
	case semantic.ModeHybrid:
		return d.searchHybrid(ctx, opts, int(topK))
	default:
		return d.searchDense(ctx, opts, topK)
	}
}

// filterConds builds the shared WHERE conditions (metadata filter, structured
// predicates, time window, lifecycle) and the args they introduce, numbering
// placeholders from $1. Callers append mode-specific args (vector, query text)
// afterward.
func (d *Driver) filterConds(opts semantic.SearchOptions) ([]string, []any) {
	var conds []string
	var args []any
	// Storage isolation: every search lane funnels through here, so scoping the
	// tenant once covers dense/sparse/hybrid. Always applied (tenant is "" when
	// isolation is off, matching the un-scoped rows).
	args = append(args, opts.Tenant)
	conds = append(conds, fmt.Sprintf("tenant = $%d", len(args)))
	if len(opts.Filter) > 0 {
		args = append(args, mustMarshalFilter(opts.Filter))
		conds = append(conds, fmt.Sprintf("metadata @> $%d::jsonb", len(args)))
	}
	for _, p := range opts.Predicates {
		cond, pargs := predicateCond(p, len(args))
		conds = append(conds, cond)
		args = append(args, pargs...)
	}
	if !opts.CreatedAfter.IsZero() {
		args = append(args, opts.CreatedAfter)
		conds = append(conds, fmt.Sprintf("created_at > $%d", len(args)))
	}
	if !opts.CreatedBefore.IsZero() {
		args = append(args, opts.CreatedBefore)
		conds = append(conds, fmt.Sprintf("created_at < $%d", len(args)))
	}
	if !opts.IncludeInvalidated {
		// Lifecycle pre-filter (ADR-0003): live and valid as of AsOf (nil = now()).
		args = append(args, nullTime(opts.AsOf))
		conds = append(conds, fmt.Sprintf(
			"deleted_at IS NULL AND valid_from <= COALESCE($%d, now()) AND (valid_to IS NULL OR valid_to > COALESCE($%d, now()))",
			len(args), len(args),
		))
	}
	return conds, args
}

func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

const fullRecordColumns = `id, content, payload, vector, metadata, created_at, valid_from, valid_to, deleted_at,
	COALESCE(supersedes, '{}'::text[]) AS supersedes, source, version`

func scanFullRecord(rows pgx.Rows) (semantic.Record, float64, error) {
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
		return semantic.Record{}, 0, fmt.Errorf("semantic/postgres: scan: %w", err)
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
	r.Vector = pgvec.Slice()
	return r, distance, nil
}

func applyIncludeFlags(r *semantic.Record, opts semantic.SearchOptions) {
	if !opts.IncludeVector {
		r.Vector = nil
	}
	if !opts.IncludePayload {
		r.Payload = nil
	}
}

func (d *Driver) searchDense(ctx context.Context, opts semantic.SearchOptions, topK uint32) ([]semantic.Hit, error) {
	conds, args := d.filterConds(opts)
	args = append(args, pgvector.NewVector(opts.QueryVector))
	vecIdx := len(args)
	args = append(args, int32(topK))
	limIdx := len(args)
	where := whereClause(conds)

	if opts.IDsOnly {
		// Seed-set path (plan Q5): select only id + distance.
		q := fmt.Sprintf("SELECT id, (vector <=> $%d) AS distance FROM %s%s ORDER BY vector <=> $%d LIMIT $%d",
			vecIdx, d.tableName, where, vecIdx, limIdx)
		rows, err := d.pool.Query(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("semantic/postgres: search: %w", err)
		}
		defer rows.Close()
		hits := make([]semantic.Hit, 0, topK)
		for rows.Next() {
			var id string
			var distance float64
			if err := rows.Scan(&id, &distance); err != nil {
				return nil, fmt.Errorf("semantic/postgres: scan: %w", err)
			}
			hits = append(hits, semantic.Hit{Record: semantic.Record{ID: id}, Score: similarityFromDistance(distance)})
		}
		return hits, rows.Err()
	}

	q := fmt.Sprintf("SELECT %s, (vector <=> $%d) AS distance FROM %s%s ORDER BY vector <=> $%d LIMIT $%d",
		fullRecordColumns, vecIdx, d.tableName, where, vecIdx, limIdx)
	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic/postgres: search: %w", err)
	}
	defer rows.Close()
	hits := make([]semantic.Hit, 0, topK)
	for rows.Next() {
		r, distance, err := scanFullRecord(rows)
		if err != nil {
			return nil, err
		}
		applyIncludeFlags(&r, opts)
		hits = append(hits, semantic.Hit{Record: r, Score: similarityFromDistance(distance)})
	}
	return hits, rows.Err()
}

type sparseHit struct {
	id   string
	rank float64
}

// sparseLane ranks content ids by Postgres full-text ts_rank (best-first),
// limited to n, honouring the shared filter/lifecycle conditions.
func (d *Driver) sparseLane(ctx context.Context, opts semantic.SearchOptions, n int) ([]sparseHit, error) {
	conds, args := d.filterConds(opts)
	args = append(args, opts.QueryText)
	qIdx := len(args)
	args = append(args, int32(n))
	limIdx := len(args)
	// The text-search config is a validated identifier (safe to inline); the
	// query text is a bound parameter.
	fts := fmt.Sprintf("to_tsvector('%s', content)", d.textSearch)
	tsq := fmt.Sprintf("websearch_to_tsquery('%s', $%d)", d.textSearch, qIdx)
	conds = append(conds, fmt.Sprintf("%s @@ %s", fts, tsq))
	q := fmt.Sprintf("SELECT id, ts_rank(%s, %s) AS rank FROM %s%s ORDER BY rank DESC, id LIMIT $%d",
		fts, tsq, d.tableName, whereClause(conds), limIdx)
	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic/postgres: sparse search: %w", err)
	}
	defer rows.Close()
	var out []sparseHit
	for rows.Next() {
		var sh sparseHit
		if err := rows.Scan(&sh.id, &sh.rank); err != nil {
			return nil, fmt.Errorf("semantic/postgres: scan: %w", err)
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// denseIDs returns ids ordered by vector distance (best-first), limited to n.
func (d *Driver) denseIDs(ctx context.Context, opts semantic.SearchOptions, n int) ([]string, error) {
	conds, args := d.filterConds(opts)
	args = append(args, pgvector.NewVector(opts.QueryVector))
	vecIdx := len(args)
	args = append(args, int32(n))
	limIdx := len(args)
	// id tiebreak keeps the fusion input deterministic on equal distances.
	q := fmt.Sprintf("SELECT id FROM %s%s ORDER BY vector <=> $%d, id LIMIT $%d",
		d.tableName, whereClause(conds), vecIdx, limIdx)
	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic/postgres: dense search: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("semantic/postgres: scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (d *Driver) searchSparse(ctx context.Context, opts semantic.SearchOptions, topK int) ([]semantic.Hit, error) {
	lane, err := d.sparseLane(ctx, opts, topK)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(lane))
	score := make(map[string]float64, len(lane))
	for i, sh := range lane {
		ids[i] = sh.id
		score[sh.id] = sh.rank
	}
	return d.materialize(ctx, opts, ids, score)
}

func (d *Driver) searchHybrid(ctx context.Context, opts semantic.SearchOptions, topK int) ([]semantic.Hit, error) {
	rerank := int(opts.RerankCandidateK)
	if rerank <= 0 {
		rerank = semantic.DefaultRerankCandidateK
	}
	denseIDs, err := d.denseIDs(ctx, opts, rerank)
	if err != nil {
		return nil, err
	}
	lane, err := d.sparseLane(ctx, opts, rerank)
	if err != nil {
		return nil, err
	}
	sparseIDs := make([]string, len(lane))
	for i, sh := range lane {
		sparseIDs[i] = sh.id
	}
	fusedIDs, scores := semantic.RRFFuse([][]string{denseIDs, sparseIDs}, topK)
	return d.materialize(ctx, opts, fusedIDs, scores)
}

// materialize fetches the full records for orderedIDs (or just the ids when
// IDsOnly) and returns hits in that order, scored by score[id].
func (d *Driver) materialize(ctx context.Context, opts semantic.SearchOptions, orderedIDs []string, score map[string]float64) ([]semantic.Hit, error) {
	if len(orderedIDs) == 0 {
		return nil, nil
	}
	if opts.IDsOnly {
		hits := make([]semantic.Hit, len(orderedIDs))
		for i, id := range orderedIDs {
			hits[i] = semantic.Hit{Record: semantic.Record{ID: id}, Score: float32(score[id])}
		}
		return hits, nil
	}
	// Scope by tenant too: ids from the ranking lanes are already this tenant's,
	// but ids can collide across tenants, so id = ANY alone could fetch another
	// tenant's colliding row.
	q := fmt.Sprintf("SELECT %s, 0::float8 AS distance FROM %s WHERE tenant = $2 AND id = ANY($1)", fullRecordColumns, d.tableName)
	rows, err := d.pool.Query(ctx, q, orderedIDs, opts.Tenant)
	if err != nil {
		return nil, fmt.Errorf("semantic/postgres: materialize: %w", err)
	}
	defer rows.Close()
	recs := make(map[string]semantic.Record, len(orderedIDs))
	for rows.Next() {
		r, _, err := scanFullRecord(rows)
		if err != nil {
			return nil, err
		}
		applyIncludeFlags(&r, opts)
		recs[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hits := make([]semantic.Hit, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		if r, ok := recs[id]; ok {
			hits = append(hits, semantic.Hit{Record: r, Score: float32(score[id])})
		}
	}
	return hits, nil
}

func (d *Driver) Delete(ctx context.Context, id string, opts semantic.DeleteOptions) (bool, error) {
	var stmt string
	if opts.Hard {
		stmt = fmt.Sprintf(`DELETE FROM %s WHERE tenant = $2 AND id = $1`, d.tableName)
	} else {
		// Soft delete: tombstone a live row; already-retracted rows are a no-op.
		stmt = fmt.Sprintf(`UPDATE %s SET deleted_at = now() WHERE tenant = $2 AND id = $1 AND deleted_at IS NULL`, d.tableName)
	}
	ct, err := d.pool.Exec(ctx, stmt, id, opts.Tenant)
	if err != nil {
		return false, fmt.Errorf("semantic/postgres: delete: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

func (d *Driver) Expire(ctx context.Context, opts semantic.ExpireOptions) (uint64, error) {
	// Reuse the same metadata @> jsonb predicate as Search; {} matches all.
	// A bounded subselect caps the affected set (max_rows), so the whole
	// operation is one localized statement (ADR-0003, U3).
	// Both the outer statement and the bounded subselect are tenant-scoped: ids
	// can collide across tenants, so id IN (…) alone could touch another
	// tenant's row.
	var stmt string
	switch opts.Action {
	case semantic.ExpireInvalidate:
		stmt = `UPDATE %[1]s SET valid_to = now()
		         WHERE tenant = $3 AND id IN (SELECT id FROM %[1]s
		                       WHERE tenant = $3 AND metadata @> $1::jsonb AND deleted_at IS NULL
		                         AND (valid_to IS NULL OR valid_to > now())
		                       LIMIT $2)`
	case semantic.ExpireSoftDelete:
		stmt = `UPDATE %[1]s SET deleted_at = now()
		         WHERE tenant = $3 AND id IN (SELECT id FROM %[1]s
		                       WHERE tenant = $3 AND metadata @> $1::jsonb AND deleted_at IS NULL
		                       LIMIT $2)`
	case semantic.ExpireHardDelete:
		stmt = `DELETE FROM %[1]s
		         WHERE tenant = $3 AND id IN (SELECT id FROM %[1]s
		                       WHERE tenant = $3 AND metadata @> $1::jsonb
		                       LIMIT $2)`
	default:
		return 0, fmt.Errorf("semantic/postgres: unknown expire action %d", opts.Action)
	}
	ct, err := d.pool.Exec(ctx, fmt.Sprintf(stmt, d.tableName),
		mustMarshalFilter(opts.Filter), int32(opts.MaxRows), opts.Tenant)
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

// numericTextRe matches an optionally-signed integer or decimal. Numeric
// predicates guard the ::numeric cast behind it so a non-numeric stored value
// is skipped rather than erroring the whole query.
const numericTextRe = `^-?[0-9]+(\.[0-9]+)?$`

// predicateCond compiles a FieldPredicate (Q3) into a parameterized SQL
// condition plus the ordered args it introduces. base is len(args) before the
// args are appended, so the first placeholder is $base+1. The metadata key and
// every value are bound as parameters (never string-interpolated) to avoid
// injection. Predicates are validated in the service, so the ops here are known
// and numeric values already parse.
func predicateCond(p semantic.FieldPredicate, base int) (string, []any) {
	keyIdx := base + 1
	valIdx := base + 2
	switch p.Op {
	case semantic.PredEQ:
		return fmt.Sprintf("metadata->>$%d = $%d", keyIdx, valIdx), []any{p.Key, p.Values[0]}
	case semantic.PredNEQ:
		return fmt.Sprintf("metadata->>$%d <> $%d", keyIdx, valIdx), []any{p.Key, p.Values[0]}
	case semantic.PredIN:
		return fmt.Sprintf("metadata->>$%d = ANY($%d)", keyIdx, valIdx), []any{p.Key, p.Values}
	case semantic.PredGT, semantic.PredGTE, semantic.PredLT, semantic.PredLTE:
		return fmt.Sprintf(
			"(metadata->>$%d ~ '%s' AND (metadata->>$%d)::numeric %s $%d::numeric)",
			keyIdx, numericTextRe, keyIdx, sqlNumericOp(p.Op), valIdx,
		), []any{p.Key, p.Values[0]}
	default:
		return "false", nil // unreachable: validated upstream
	}
}

func sqlNumericOp(op semantic.PredicateOp) string {
	switch op {
	case semantic.PredGT:
		return ">"
	case semantic.PredGTE:
		return ">="
	case semantic.PredLT:
		return "<"
	case semantic.PredLTE:
		return "<="
	}
	return ""
}

func ensureSchema(ctx context.Context, pool *pgxpool.Pool, table string, dim int, textSearch string) error {
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("create extension vector: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		return fmt.Errorf("create extension pgcrypto: %w", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			tenant     text        NOT NULL DEFAULT '',
			id         text        NOT NULL,
			content    text        NOT NULL DEFAULT '',
			payload    bytea       NOT NULL DEFAULT ''::bytea,
			vector     vector(%d)  NOT NULL,
			metadata   jsonb       NOT NULL DEFAULT '{}'::jsonb,
			created_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant, id)
		)`, table, dim)); err != nil {
		return fmt.Errorf("create table %s: %w", table, err)
	}
	// Lifecycle columns (ADR-0003) + the tenant column (storage isolation).
	// Added idempotently so existing tables upgrade in place; existing rows
	// backfill valid_from=now() and read as live, and tenant='' (the un-isolated
	// partition, matching pre-isolation behavior).
	for _, alter := range []string{
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS tenant text NOT NULL DEFAULT ''`,
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
	// Upgrade a pre-isolation table whose primary key is still (id) to the
	// composite (tenant, id). Idempotent: a single-column PK is the old shape.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		DO $$
		BEGIN
			IF EXISTS (
				SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid
				WHERE c.relname = '%[1]s' AND i.indisprimary
				  AND array_length(i.indkey::int[], 1) = 1
			) THEN
				ALTER TABLE %[1]s DROP CONSTRAINT %[1]s_pkey;
				ALTER TABLE %[1]s ADD PRIMARY KEY (tenant, id);
			END IF;
		END $$`, table)); err != nil {
		return fmt.Errorf("upgrade primary key on %s: %w", table, err)
	}
	// Partial index over the non-tombstoned set to keep the default live-search
	// pre-filter cheap (the deleted_at IS NULL predicate is immutable).
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s_live ON %s (valid_from, valid_to) WHERE deleted_at IS NULL`,
		table, table,
	)); err != nil {
		return fmt.Errorf("create live index on %s: %w", table, err)
	}
	// Btree on created_at backs the Q2 time-range pre-filter.
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s_created_at ON %s (created_at)`,
		table, table,
	)); err != nil {
		return fmt.Errorf("create created_at index on %s: %w", table, err)
	}
	// GIN over the full-text vector of content backs the Q4 sparse lane. The
	// config is a validated identifier, safe to inline (an index expression
	// can't be parameterized).
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s_content_fts ON %s USING gin (to_tsvector('%s', content))`,
		table, table, textSearch,
	)); err != nil {
		return fmt.Errorf("create fts index on %s: %w", table, err)
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
