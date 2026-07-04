// Package memory implements an in-memory graph driver: nodes and edges in maps
// with out/in adjacency for bounded traversal. It is the reference driver and
// conformance baseline; production deployments use a real graph backend.
package memory

import (
	"context"
	"sync"
	"time"

	"memsidecar/internal/graph"
)

// nsData holds one namespace's nodes, edges, and adjacency. out[n] is the set
// of edge ids whose From is n; in[n] is the set whose To is n.
type nsData struct {
	nodes map[string]graph.Node
	edges map[string]graph.Edge
	out   map[string]map[string]struct{}
	in    map[string]map[string]struct{}
}

func newNSData() *nsData {
	return &nsData{
		nodes: map[string]graph.Node{},
		edges: map[string]graph.Edge{},
		out:   map[string]map[string]struct{}{},
		in:    map[string]map[string]struct{}{},
	}
}

// Options configures a Driver.
type Options struct {
	NowFunc func() time.Time
}

// Driver is an in-memory graph driver, partitioned by namespace. Safe for
// concurrent use.
type Driver struct {
	mu  sync.RWMutex
	ns  map[string]*nsData
	now func() time.Time
}

// New builds a Driver.
func New(opts Options) *Driver {
	d := &Driver{ns: map[string]*nsData{}, now: time.Now}
	if opts.NowFunc != nil {
		d.now = opts.NowFunc
	}
	return d
}

func (d *Driver) Close() error { return nil }

// space returns the namespace partition, creating it if absent. Caller holds
// the write lock.
func (d *Driver) space(namespace string) *nsData {
	s := d.ns[namespace]
	if s == nil {
		s = newNSData()
		d.ns[namespace] = s
	}
	return s
}

func (d *Driver) UpsertNodes(_ context.Context, namespace string, nodes []graph.Node) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.space(namespace)
	for _, n := range nodes {
		stored := graph.Node{
			ID:        n.ID,
			Labels:    cloneStrings(n.Labels),
			Props:     cloneMap(n.Props),
			CreatedAt: n.CreatedAt,
		}
		if stored.CreatedAt.IsZero() {
			if existing, ok := s.nodes[n.ID]; ok {
				stored.CreatedAt = existing.CreatedAt // preserve on update
			} else {
				stored.CreatedAt = d.now().UTC()
			}
		}
		s.nodes[n.ID] = stored
	}
	return nil
}

func (d *Driver) UpsertEdges(_ context.Context, namespace string, edges []graph.Edge) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.space(namespace)
	for _, e := range edges {
		if old, ok := s.edges[e.ID]; ok {
			detach(s, old) // replacing: drop stale adjacency
		}
		stored := graph.Edge{
			ID: e.ID, Type: e.Type, From: e.From, To: e.To,
			Props: cloneMap(e.Props), CreatedAt: e.CreatedAt,
		}
		if stored.CreatedAt.IsZero() {
			stored.CreatedAt = d.now().UTC()
		}
		s.edges[e.ID] = stored
		addAdj(s.out, e.From, e.ID)
		addAdj(s.in, e.To, e.ID)
	}
	return nil
}

func (d *Driver) GetNode(_ context.Context, namespace, id string) (graph.Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s := d.ns[namespace]
	if s == nil {
		return graph.Node{}, graph.ErrNotFound
	}
	n, ok := s.nodes[id]
	if !ok {
		return graph.Node{}, graph.ErrNotFound
	}
	return cloneNode(n), nil
}

func (d *Driver) Neighbors(_ context.Context, namespace string, opts graph.NeighborOptions) ([]graph.Node, []graph.Edge, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s := d.ns[namespace]
	if s == nil {
		return nil, nil, graph.ErrNotFound
	}
	if _, ok := s.nodes[opts.NodeID]; !ok {
		return nil, nil, graph.ErrNotFound
	}

	typeSet := toSet(opts.EdgeTypes)
	labelSet := toSet(opts.NodeLabels)
	seen := map[string]struct{}{}
	var outNodes []graph.Node
	var outEdges []graph.Edge

	for _, edgeID := range incident(s, opts.NodeID, opts.Direction) {
		if opts.Limit > 0 && uint32(len(outNodes)) >= opts.Limit {
			break
		}
		e := s.edges[edgeID]
		if len(typeSet) > 0 {
			if _, ok := typeSet[e.Type]; !ok {
				continue
			}
		}
		neighborID := otherEndpoint(e, opts.NodeID)
		if neighborID == opts.NodeID {
			continue // self-loop: a node is not its own neighbor (and would
			// otherwise be emitted twice under DirectionBoth)
		}
		n, ok := s.nodes[neighborID]
		if !ok {
			continue
		}
		if len(labelSet) > 0 && !hasAnyLabel(n.Labels, labelSet) {
			continue
		}
		outEdges = append(outEdges, cloneEdge(e))
		if _, dup := seen[neighborID]; !dup {
			seen[neighborID] = struct{}{}
			outNodes = append(outNodes, cloneNode(n))
		}
	}
	return outNodes, outEdges, nil
}

func (d *Driver) Traverse(_ context.Context, namespace string, opts graph.TraverseOptions) (graph.Subgraph, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s := d.ns[namespace]
	if s == nil {
		return graph.Subgraph{}, graph.ErrNotFound
	}
	start, ok := s.nodes[opts.StartID]
	if !ok {
		return graph.Subgraph{}, graph.ErrNotFound
	}

	typeSet := toSet(opts.EdgeTypes)
	maxNodes := opts.MaxNodes
	if maxNodes == 0 {
		maxNodes = 1
	}

	visited := map[string]struct{}{opts.StartID: {}}
	seenEdge := map[string]struct{}{}
	sub := graph.Subgraph{Nodes: []graph.Node{cloneNode(start)}}
	frontier := []string{opts.StartID}

	for hop := uint32(0); hop < opts.MaxDepth && len(frontier) > 0; hop++ {
		var next []string
		for _, nodeID := range frontier {
			var fan uint32
			for _, edgeID := range incident(s, nodeID, opts.Direction) {
				if opts.FanOut > 0 && fan >= opts.FanOut {
					break // per-node work cap: bound the adjacency scan
				}
				fan++
				e := s.edges[edgeID]
				if len(typeSet) > 0 {
					if _, ok := typeSet[e.Type]; !ok {
						continue
					}
				}
				neighborID := otherEndpoint(e, nodeID)
				if _, done := visited[neighborID]; !done {
					if uint32(len(sub.Nodes)) >= maxNodes {
						continue // node budget spent; don't add a dangling edge
					}
					n, ok := s.nodes[neighborID]
					if !ok {
						continue
					}
					visited[neighborID] = struct{}{}
					sub.Nodes = append(sub.Nodes, cloneNode(n))
					next = append(next, neighborID)
				}
				if opts.MaxEdges > 0 && uint32(len(sub.Edges)) >= opts.MaxEdges {
					continue // edge result budget spent
				}
				if _, dup := seenEdge[edgeID]; !dup {
					seenEdge[edgeID] = struct{}{}
					sub.Edges = append(sub.Edges, cloneEdge(e))
				}
			}
		}
		frontier = next
	}
	return sub, nil
}

func (d *Driver) DeleteNode(_ context.Context, namespace, id string, cascade bool) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.ns[namespace]
	if s == nil {
		return false, nil
	}
	if _, ok := s.nodes[id]; !ok {
		return false, nil
	}
	incidentEdges := map[string]struct{}{}
	for eid := range s.out[id] {
		incidentEdges[eid] = struct{}{}
	}
	for eid := range s.in[id] {
		incidentEdges[eid] = struct{}{}
	}
	if len(incidentEdges) > 0 && !cascade {
		return false, graph.ErrEdgesExist
	}
	for eid := range incidentEdges {
		detach(s, s.edges[eid])
		delete(s.edges, eid)
	}
	delete(s.nodes, id)
	return true, nil
}

func (d *Driver) DeleteEdge(_ context.Context, namespace, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.ns[namespace]
	if s == nil {
		return false, nil
	}
	e, ok := s.edges[id]
	if !ok {
		return false, nil
	}
	detach(s, e)
	delete(s.edges, id)
	return true, nil
}

// --- helpers ---

func incident(s *nsData, nodeID string, dir graph.Direction) []string {
	var ids []string
	if dir == graph.DirectionOut || dir == graph.DirectionBoth {
		for id := range s.out[nodeID] {
			ids = append(ids, id)
		}
	}
	if dir == graph.DirectionIn || dir == graph.DirectionBoth {
		for id := range s.in[nodeID] {
			ids = append(ids, id)
		}
	}
	return ids
}

func otherEndpoint(e graph.Edge, nodeID string) string {
	if e.From == nodeID {
		return e.To
	}
	return e.From
}

func addAdj(m map[string]map[string]struct{}, node, edgeID string) {
	set := m[node]
	if set == nil {
		set = map[string]struct{}{}
		m[node] = set
	}
	set[edgeID] = struct{}{}
}

func detach(s *nsData, e graph.Edge) {
	if set := s.out[e.From]; set != nil {
		delete(set, e.ID)
		if len(set) == 0 {
			delete(s.out, e.From)
		}
	}
	if set := s.in[e.To]; set != nil {
		delete(set, e.ID)
		if len(set) == 0 {
			delete(s.in, e.To)
		}
	}
}

func toSet(vals []string) map[string]struct{} {
	if len(vals) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		m[v] = struct{}{}
	}
	return m
}

func hasAnyLabel(labels []string, want map[string]struct{}) bool {
	for _, l := range labels {
		if _, ok := want[l]; ok {
			return true
		}
	}
	return false
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneNode(n graph.Node) graph.Node {
	n.Labels = cloneStrings(n.Labels)
	n.Props = cloneMap(n.Props)
	return n
}

func cloneEdge(e graph.Edge) graph.Edge {
	e.Props = cloneMap(e.Props)
	return e
}
