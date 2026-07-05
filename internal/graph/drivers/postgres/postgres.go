// Package postgres implements the graph driver over Postgres — the first
// production graph backend (ADR-0002 §6). It stores nodes and edges in two
// shared tables keyed by (namespace, id) and answers Neighbors/Traverse by
// fetching adjacency from the (namespace, from_id) / (namespace, to_id) indexes
// and running the bounded walk in Go, mirroring the in-memory reference driver.
// It trades traversal throughput (per-frontier-node queries, no server-side
// recursive CTE) for a faithful, portable implementation of the same
// hard-capped contract.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"memsidecar/internal/graph"
)

// Options configures a Driver.
type Options struct {
	DSN            string
	MaxConns       int32
	SkipMigrations bool // skip table creation (external migration manages it)
}

// Driver implements graph.Driver against Postgres. It is shared across
// namespaces (the namespace is a column, not a table prefix).
type Driver struct {
	pool *pgxpool.Pool
}

// New opens a pool and ensures the schema, returning a Driver. Caller Closes.
func New(ctx context.Context, opts Options) (*Driver, error) {
	if opts.DSN == "" {
		return nil, errors.New("graph/postgres: dsn required")
	}
	pcfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("graph/postgres: parse dsn: %w", err)
	}
	if opts.MaxConns > 0 {
		pcfg.MaxConns = opts.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("graph/postgres: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("graph/postgres: ping: %w", err)
	}
	if !opts.SkipMigrations {
		if err := ensureSchema(ctx, pool); err != nil {
			pool.Close()
			return nil, fmt.Errorf("graph/postgres: ensure schema: %w", err)
		}
	}
	return &Driver{pool: pool}, nil
}

func (d *Driver) Close() error {
	d.pool.Close()
	return nil
}

func ensureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS graph_nodes (
			namespace  text        NOT NULL,
			id         text        NOT NULL,
			labels     text[]      NOT NULL DEFAULT '{}',
			props      jsonb       NOT NULL DEFAULT '{}',
			created_at timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (namespace, id)
		)`,
		`CREATE TABLE IF NOT EXISTS graph_edges (
			namespace  text        NOT NULL,
			id         text        NOT NULL,
			type       text        NOT NULL,
			from_id    text        NOT NULL,
			to_id      text        NOT NULL,
			props      jsonb       NOT NULL DEFAULT '{}',
			created_at timestamptz NOT NULL DEFAULT now(),
			valid_from timestamptz,
			valid_to   timestamptz,
			PRIMARY KEY (namespace, id)
		)`,
		`CREATE INDEX IF NOT EXISTS graph_edges_from ON graph_edges (namespace, from_id)`,
		`CREATE INDEX IF NOT EXISTS graph_edges_to ON graph_edges (namespace, to_id)`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) UpsertNodes(ctx context.Context, namespace string, nodes []graph.Node) error {
	if len(nodes) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, n := range nodes {
		labels := n.Labels
		if labels == nil {
			labels = []string{}
		}
		batch.Queue(`
			INSERT INTO graph_nodes (namespace, id, labels, props, created_at)
			     VALUES ($1, $2, $3, $4, COALESCE($5, now()))
			ON CONFLICT (namespace, id) DO UPDATE
			  SET labels = EXCLUDED.labels, props = EXCLUDED.props`,
			namespace, n.ID, labels, mustJSON(n.Props), nullTime(n.CreatedAt))
	}
	return sendBatch(ctx, d.pool, batch, len(nodes))
}

func (d *Driver) UpsertEdges(ctx context.Context, namespace string, edges []graph.Edge) error {
	if len(edges) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range edges {
		batch.Queue(`
			INSERT INTO graph_edges (namespace, id, type, from_id, to_id, props, created_at, valid_from, valid_to)
			     VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()), $8, $9)
			ON CONFLICT (namespace, id) DO UPDATE
			  SET type = EXCLUDED.type, from_id = EXCLUDED.from_id,
			      to_id = EXCLUDED.to_id, props = EXCLUDED.props,
			      valid_from = EXCLUDED.valid_from, valid_to = EXCLUDED.valid_to`,
			namespace, e.ID, e.Type, e.From, e.To, mustJSON(e.Props),
			nullTime(e.CreatedAt), nullTime(e.ValidFrom), nullTime(e.ValidTo))
	}
	return sendBatch(ctx, d.pool, batch, len(edges))
}

func (d *Driver) GetNode(ctx context.Context, namespace, id string) (graph.Node, error) {
	n, ok, err := getNode(ctx, d.pool, namespace, id)
	if err != nil {
		return graph.Node{}, err
	}
	if !ok {
		return graph.Node{}, graph.ErrNotFound
	}
	return n, nil
}

func (d *Driver) Neighbors(ctx context.Context, namespace string, opts graph.NeighborOptions) ([]graph.Node, []graph.Edge, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("graph/postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, ok, err := getNode(ctx, tx, namespace, opts.NodeID); err != nil {
		return nil, nil, err
	} else if !ok {
		return nil, nil, graph.ErrNotFound
	}

	edges, err := incidentEdges(ctx, tx, namespace, opts.NodeID, opts.Direction)
	if err != nil {
		return nil, nil, err
	}
	// Batch-load the candidate neighbour nodes.
	ids := make([]string, 0, len(edges))
	for _, e := range edges {
		ids = append(ids, otherEndpoint(e, opts.NodeID))
	}
	byID, err := getNodes(ctx, tx, namespace, ids)
	if err != nil {
		return nil, nil, err
	}

	typeSet := toSet(opts.EdgeTypes)
	labelSet := toSet(opts.NodeLabels)
	asOf := opts.AsOf
	if asOf.IsZero() {
		asOf = time.Now()
	}
	seen := map[string]struct{}{}
	var outNodes []graph.Node
	var outEdges []graph.Edge
	for _, e := range edges {
		if opts.Limit > 0 && uint32(len(outNodes)) >= opts.Limit {
			break
		}
		if len(typeSet) > 0 {
			if _, ok := typeSet[e.Type]; !ok {
				continue
			}
		}
		if !e.ValidAt(asOf) {
			continue
		}
		neighborID := otherEndpoint(e, opts.NodeID)
		if neighborID == opts.NodeID {
			continue // self-loop: a node is not its own neighbour
		}
		n, ok := byID[neighborID]
		if !ok {
			continue
		}
		if len(labelSet) > 0 && !hasAnyLabel(n.Labels, labelSet) {
			continue
		}
		outEdges = append(outEdges, e)
		if _, dup := seen[neighborID]; !dup {
			seen[neighborID] = struct{}{}
			outNodes = append(outNodes, n)
		}
	}
	return outNodes, outEdges, tx.Commit(ctx)
}

func (d *Driver) Traverse(ctx context.Context, namespace string, opts graph.TraverseOptions) (graph.Subgraph, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return graph.Subgraph{}, fmt.Errorf("graph/postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	start, ok, err := getNode(ctx, tx, namespace, opts.StartID)
	if err != nil {
		return graph.Subgraph{}, err
	}
	if !ok {
		return graph.Subgraph{}, graph.ErrNotFound
	}

	typeSet := toSet(opts.EdgeTypes)
	asOf := opts.AsOf
	if asOf.IsZero() {
		asOf = time.Now()
	}
	maxNodes := opts.MaxNodes
	if maxNodes == 0 {
		maxNodes = 1
	}

	visited := map[string]struct{}{opts.StartID: {}}
	seenEdge := map[string]struct{}{}
	sub := graph.Subgraph{Nodes: []graph.Node{start}}
	frontier := []string{opts.StartID}

	for hop := uint32(0); hop < opts.MaxDepth && len(frontier) > 0; hop++ {
		var next []string
		for _, nodeID := range frontier {
			edges, err := incidentEdges(ctx, tx, namespace, nodeID, opts.Direction)
			if err != nil {
				return graph.Subgraph{}, err
			}
			var fan uint32
			for _, e := range edges {
				if opts.FanOut > 0 && fan >= opts.FanOut {
					break // per-node work cap: bound the adjacency scan
				}
				fan++
				if len(typeSet) > 0 {
					if _, ok := typeSet[e.Type]; !ok {
						continue
					}
				}
				if !e.ValidAt(asOf) {
					continue
				}
				neighborID := otherEndpoint(e, nodeID)
				if _, done := visited[neighborID]; !done {
					if uint32(len(sub.Nodes)) >= maxNodes {
						continue // node budget spent; don't add a dangling edge
					}
					n, ok, err := getNode(ctx, tx, namespace, neighborID)
					if err != nil {
						return graph.Subgraph{}, err
					}
					if !ok {
						continue
					}
					visited[neighborID] = struct{}{}
					sub.Nodes = append(sub.Nodes, n)
					next = append(next, neighborID)
				}
				if opts.MaxEdges > 0 && uint32(len(sub.Edges)) >= opts.MaxEdges {
					continue // edge result budget spent
				}
				if _, dup := seenEdge[e.ID]; !dup {
					seenEdge[e.ID] = struct{}{}
					sub.Edges = append(sub.Edges, e)
				}
			}
		}
		frontier = next
	}
	return sub, tx.Commit(ctx)
}

func (d *Driver) DeleteNode(ctx context.Context, namespace, id string, cascade bool) (bool, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("graph/postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	exists, err := nodeExists(ctx, tx, namespace, id)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}

	var incident int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM graph_edges WHERE namespace = $1 AND (from_id = $2 OR to_id = $2)`,
		namespace, id).Scan(&incident); err != nil {
		return false, fmt.Errorf("graph/postgres: count edges: %w", err)
	}
	if incident > 0 && !cascade {
		return false, graph.ErrEdgesExist
	}
	if incident > 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM graph_edges WHERE namespace = $1 AND (from_id = $2 OR to_id = $2)`,
			namespace, id); err != nil {
			return false, fmt.Errorf("graph/postgres: cascade edges: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM graph_nodes WHERE namespace = $1 AND id = $2`, namespace, id); err != nil {
		return false, fmt.Errorf("graph/postgres: delete node: %w", err)
	}
	return true, tx.Commit(ctx)
}

func (d *Driver) DeleteEdge(ctx context.Context, namespace, id string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM graph_edges WHERE namespace = $1 AND id = $2`, namespace, id)
	if err != nil {
		return false, fmt.Errorf("graph/postgres: delete edge: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// --- helpers ---------------------------------------------------------------

// rowQuerier is satisfied by both *pgxpool.Pool and pgx.Tx.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func getNode(ctx context.Context, q rowQuerier, namespace, id string) (graph.Node, bool, error) {
	var (
		n     graph.Node
		props []byte
	)
	err := q.QueryRow(ctx,
		`SELECT id, labels, props, created_at FROM graph_nodes WHERE namespace = $1 AND id = $2`,
		namespace, id).Scan(&n.ID, &n.Labels, &props, &n.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return graph.Node{}, false, nil
	}
	if err != nil {
		return graph.Node{}, false, fmt.Errorf("graph/postgres: get node: %w", err)
	}
	n.Props = unmarshalProps(props)
	return n, true, nil
}

func getNodes(ctx context.Context, q rowQuerier, namespace string, ids []string) (map[string]graph.Node, error) {
	out := make(map[string]graph.Node, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx,
		`SELECT id, labels, props, created_at FROM graph_nodes WHERE namespace = $1 AND id = ANY($2)`,
		namespace, ids)
	if err != nil {
		return nil, fmt.Errorf("graph/postgres: get nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			n     graph.Node
			props []byte
		)
		if err := rows.Scan(&n.ID, &n.Labels, &props, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("graph/postgres: scan node: %w", err)
		}
		n.Props = unmarshalProps(props)
		out[n.ID] = n
	}
	return out, rows.Err()
}

func nodeExists(ctx context.Context, q rowQuerier, namespace, id string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM graph_nodes WHERE namespace = $1 AND id = $2)`,
		namespace, id).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("graph/postgres: node exists: %w", err)
	}
	return exists, nil
}

// incidentEdges returns the edges incident to nodeID in the given direction,
// ordered by id for determinism. Type filtering is applied by the caller (after
// the fan-out cap), matching the in-memory driver.
func incidentEdges(ctx context.Context, q rowQuerier, namespace, nodeID string, dir graph.Direction) ([]graph.Edge, error) {
	var pred string
	switch dir {
	case graph.DirectionOut:
		pred = "from_id = $2"
	case graph.DirectionIn:
		pred = "to_id = $2"
	default: // both
		pred = "(from_id = $2 OR to_id = $2)"
	}
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT id, type, from_id, to_id, props, created_at, valid_from, valid_to
		   FROM graph_edges WHERE namespace = $1 AND %s ORDER BY id`, pred),
		namespace, nodeID)
	if err != nil {
		return nil, fmt.Errorf("graph/postgres: incident edges: %w", err)
	}
	defer rows.Close()
	var out []graph.Edge
	for rows.Next() {
		var (
			e         graph.Edge
			props     []byte
			validFrom *time.Time
			validTo   *time.Time
		)
		if err := rows.Scan(&e.ID, &e.Type, &e.From, &e.To, &props, &e.CreatedAt, &validFrom, &validTo); err != nil {
			return nil, fmt.Errorf("graph/postgres: scan edge: %w", err)
		}
		e.Props = unmarshalProps(props)
		if validFrom != nil {
			e.ValidFrom = *validFrom
		}
		if validTo != nil {
			e.ValidTo = *validTo
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func otherEndpoint(e graph.Edge, nodeID string) string {
	if e.From == nodeID {
		return e.To
	}
	return e.From
}

func toSet(vals []string) map[string]struct{} {
	if len(vals) == 0 {
		return nil
	}
	s := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		s[v] = struct{}{}
	}
	return s
}

func hasAnyLabel(labels []string, want map[string]struct{}) bool {
	for _, l := range labels {
		if _, ok := want[l]; ok {
			return true
		}
	}
	return false
}

func mustJSON(m map[string]string) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	b, _ := json.Marshal(m)
	return b
}

func unmarshalProps(b []byte) map[string]string {
	if len(b) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func sendBatch(ctx context.Context, pool *pgxpool.Pool, batch *pgx.Batch, n int) error {
	br := pool.SendBatch(ctx, batch)
	for i := 0; i < n; i++ {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("graph/postgres: upsert: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("graph/postgres: upsert close: %w", err)
	}
	return nil
}
