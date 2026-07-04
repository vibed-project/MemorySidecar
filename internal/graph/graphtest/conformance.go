// Package graphtest provides a driver-agnostic conformance suite that every
// graph.Driver implementation must pass.
package graphtest

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/graph"
)

const ns = "g"

// Harness adapts a concrete driver to the conformance suite. New must return a
// freshly isolated driver.
type Harness interface {
	New(t *testing.T) graph.Driver
}

// RunConformance runs the full battery against the driver produced by h.
func RunConformance(t *testing.T, h Harness) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"UpsertNode_Get", testUpsertNodeGet},
		{"Neighbors_Direction", testNeighborsDirection},
		{"Neighbors_EdgeTypeFilter", testNeighborsEdgeTypeFilter},
		{"Neighbors_LabelFilter", testNeighborsLabelFilter},
		{"Neighbors_Limit", testNeighborsLimit},
		{"Traverse_Depth", testTraverseDepth},
		{"Traverse_MaxNodes", testTraverseMaxNodes},
		{"Traverse_ResultCaps", testTraverseResultCaps},
		{"Traverse_EdgeTypeFilter", testTraverseEdgeTypeFilter},
		{"Neighbors_SelfLoop", testNeighborsSelfLoop},
		{"DeleteEdge", testDeleteEdge},
		{"DeleteNode_RejectWithEdges", testDeleteNodeRejectWithEdges},
		{"DeleteNode_Cascade", testDeleteNodeCascade},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, h) })
	}
}

// fixture builds a small graph:
//
//	a(Person) -KNOWS-> b(Person) -KNOWS-> d(Person)
//	a(Person) -LIVES_IN-> c(City) <-LIVES_IN- d
func fixture(t *testing.T, d graph.Driver) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, d.UpsertNodes(ctx, ns, []graph.Node{
		{ID: "a", Labels: []string{"Person"}},
		{ID: "b", Labels: []string{"Person"}},
		{ID: "c", Labels: []string{"City"}},
		{ID: "d", Labels: []string{"Person"}},
	}))
	require.NoError(t, d.UpsertEdges(ctx, ns, []graph.Edge{
		{ID: "e1", Type: "KNOWS", From: "a", To: "b"},
		{ID: "e2", Type: "LIVES_IN", From: "a", To: "c"},
		{ID: "e3", Type: "KNOWS", From: "b", To: "d"},
		{ID: "e4", Type: "LIVES_IN", From: "d", To: "c"},
	}))
}

func nodeIDs(nodes []graph.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	sort.Strings(out)
	return out
}

func edgeIDs(edges []graph.Edge) []string {
	out := make([]string, len(edges))
	for i, e := range edges {
		out[i] = e.ID
	}
	sort.Strings(out)
	return out
}

func testUpsertNodeGet(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertNodes(ctx, ns, []graph.Node{
		{ID: "x", Labels: []string{"Person"}, Props: map[string]string{"name": "Kim"}},
	}))
	got, err := d.GetNode(ctx, ns, "x")
	require.NoError(t, err)
	assert.Equal(t, "x", got.ID)
	assert.Equal(t, []string{"Person"}, got.Labels)
	assert.Equal(t, "Kim", got.Props["name"])
	assert.False(t, got.CreatedAt.IsZero(), "created_at should be assigned")

	_, err = d.GetNode(ctx, ns, "missing")
	assert.ErrorIs(t, err, graph.ErrNotFound)
}

func testNeighborsDirection(t *testing.T, h Harness) {
	d := h.New(t)
	fixture(t, d)
	ctx := context.Background()

	// Out from a → b, c.
	nodes, edges, err := d.Neighbors(ctx, ns, graph.NeighborOptions{NodeID: "a", Direction: graph.DirectionOut})
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "c"}, nodeIDs(nodes))
	assert.Equal(t, []string{"e1", "e2"}, edgeIDs(edges))

	// In to c → a, d.
	nodes, _, err = d.Neighbors(ctx, ns, graph.NeighborOptions{NodeID: "c", Direction: graph.DirectionIn})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "d"}, nodeIDs(nodes))

	// Out from c → none (c has no outgoing edges).
	nodes, _, err = d.Neighbors(ctx, ns, graph.NeighborOptions{NodeID: "c", Direction: graph.DirectionOut})
	require.NoError(t, err)
	assert.Empty(t, nodes)

	// Both from b → a (incoming e1) and d (outgoing e3).
	nodes, _, err = d.Neighbors(ctx, ns, graph.NeighborOptions{NodeID: "b", Direction: graph.DirectionBoth})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "d"}, nodeIDs(nodes))

	// Missing node.
	_, _, err = d.Neighbors(ctx, ns, graph.NeighborOptions{NodeID: "nope", Direction: graph.DirectionBoth})
	assert.ErrorIs(t, err, graph.ErrNotFound)
}

func testNeighborsEdgeTypeFilter(t *testing.T, h Harness) {
	d := h.New(t)
	fixture(t, d)
	nodes, _, err := d.Neighbors(context.Background(), ns, graph.NeighborOptions{
		NodeID: "a", Direction: graph.DirectionOut, EdgeTypes: []string{"KNOWS"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, nodeIDs(nodes))
}

func testNeighborsLabelFilter(t *testing.T, h Harness) {
	d := h.New(t)
	fixture(t, d)
	// a's out neighbors are b(Person) and c(City); filter to City.
	nodes, _, err := d.Neighbors(context.Background(), ns, graph.NeighborOptions{
		NodeID: "a", Direction: graph.DirectionOut, NodeLabels: []string{"City"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"c"}, nodeIDs(nodes))
}

func testNeighborsLimit(t *testing.T, h Harness) {
	d := h.New(t)
	fixture(t, d)
	// a has two out neighbors; limit to one.
	nodes, _, err := d.Neighbors(context.Background(), ns, graph.NeighborOptions{
		NodeID: "a", Direction: graph.DirectionOut, Limit: 1,
	})
	require.NoError(t, err)
	assert.Len(t, nodes, 1)
}

func testTraverseDepth(t *testing.T, h Harness) {
	d := h.New(t)
	fixture(t, d)
	ctx := context.Background()

	// Depth 1 out from a → a, b, c.
	sub, err := d.Traverse(ctx, ns, graph.TraverseOptions{StartID: "a", Direction: graph.DirectionOut, MaxDepth: 1, MaxNodes: 100})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, nodeIDs(sub.Nodes))
	assert.Equal(t, []string{"e1", "e2"}, edgeIDs(sub.Edges))

	// Depth 2 reaches d via b.
	sub, err = d.Traverse(ctx, ns, graph.TraverseOptions{StartID: "a", Direction: graph.DirectionOut, MaxDepth: 2, MaxNodes: 100})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c", "d"}, nodeIDs(sub.Nodes))

	// Missing start.
	_, err = d.Traverse(ctx, ns, graph.TraverseOptions{StartID: "nope", MaxDepth: 2, MaxNodes: 100})
	assert.ErrorIs(t, err, graph.ErrNotFound)
}

func testTraverseMaxNodes(t *testing.T, h Harness) {
	d := h.New(t)
	fixture(t, d)
	// max_nodes caps the result including the start node.
	sub, err := d.Traverse(context.Background(), ns, graph.TraverseOptions{
		StartID: "a", Direction: graph.DirectionOut, MaxDepth: 5, MaxNodes: 2,
	})
	require.NoError(t, err)
	assert.Len(t, sub.Nodes, 2)
}

// testTraverseResultCaps checks that a high-degree region cannot blow up the
// result: MaxEdges bounds the returned edges and FanOut bounds how many
// incident edges of a node are examined.
func testTraverseResultCaps(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()

	// Star: hub -> leaf0 .. leaf9 (10 out-edges).
	nodes := []graph.Node{{ID: "hub"}}
	edges := make([]graph.Edge, 0, 10)
	for i := 0; i < 10; i++ {
		lid := fmt.Sprintf("leaf%d", i)
		nodes = append(nodes, graph.Node{ID: lid})
		edges = append(edges, graph.Edge{ID: "e" + lid, Type: "L", From: "hub", To: lid})
	}
	require.NoError(t, d.UpsertNodes(ctx, ns, nodes))
	require.NoError(t, d.UpsertEdges(ctx, ns, edges))

	// Edge result cap: at most MaxEdges edges regardless of degree.
	sub, err := d.Traverse(ctx, ns, graph.TraverseOptions{
		StartID: "hub", Direction: graph.DirectionOut, MaxDepth: 1, MaxNodes: 100, MaxEdges: 3,
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(sub.Edges), 3, "edge result must be capped")

	// Per-node fan-out cap bounds neighbours examined (hub + at most FanOut).
	sub, err = d.Traverse(ctx, ns, graph.TraverseOptions{
		StartID: "hub", Direction: graph.DirectionOut, MaxDepth: 1, MaxNodes: 100, FanOut: 2,
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(sub.Nodes), 3, "fan-out must bound neighbours examined")
}

// testNeighborsSelfLoop checks a self-loop is not reported as a node being its
// own neighbour, and is not duplicated under DirectionBoth.
func testNeighborsSelfLoop(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertNodes(ctx, ns, []graph.Node{{ID: "a"}}))
	require.NoError(t, d.UpsertEdges(ctx, ns, []graph.Edge{{ID: "loop", Type: "SELF", From: "a", To: "a"}}))

	nodes, edges, err := d.Neighbors(ctx, ns, graph.NeighborOptions{NodeID: "a", Direction: graph.DirectionBoth})
	require.NoError(t, err)
	assert.Empty(t, nodes, "a node is not its own neighbour")
	assert.Empty(t, edges, "self-loop yields no neighbour edges (and no duplicate)")
}

func testTraverseEdgeTypeFilter(t *testing.T, h Harness) {
	d := h.New(t)
	fixture(t, d)
	// Follow only KNOWS out of a: a → b → d (c is reached only via LIVES_IN).
	sub, err := d.Traverse(context.Background(), ns, graph.TraverseOptions{
		StartID: "a", Direction: graph.DirectionOut, EdgeTypes: []string{"KNOWS"}, MaxDepth: 5, MaxNodes: 100,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "d"}, nodeIDs(sub.Nodes))
	assert.Equal(t, []string{"e1", "e3"}, edgeIDs(sub.Edges))
}

func testDeleteEdge(t *testing.T, h Harness) {
	d := h.New(t)
	fixture(t, d)
	ctx := context.Background()

	existed, err := d.DeleteEdge(ctx, ns, "e1")
	require.NoError(t, err)
	assert.True(t, existed)

	// a no longer reaches b via the deleted edge.
	nodes, _, err := d.Neighbors(ctx, ns, graph.NeighborOptions{NodeID: "a", Direction: graph.DirectionOut})
	require.NoError(t, err)
	assert.Equal(t, []string{"c"}, nodeIDs(nodes))

	existed, err = d.DeleteEdge(ctx, ns, "e1")
	require.NoError(t, err)
	assert.False(t, existed, "second delete is a no-op")
}

func testDeleteNodeRejectWithEdges(t *testing.T, h Harness) {
	d := h.New(t)
	fixture(t, d)
	_, err := d.DeleteNode(context.Background(), ns, "a", false)
	assert.ErrorIs(t, err, graph.ErrEdgesExist)
}

func testDeleteNodeCascade(t *testing.T, h Harness) {
	d := h.New(t)
	fixture(t, d)
	ctx := context.Background()

	existed, err := d.DeleteNode(ctx, ns, "a", true)
	require.NoError(t, err)
	assert.True(t, existed)

	_, err = d.GetNode(ctx, ns, "a")
	assert.ErrorIs(t, err, graph.ErrNotFound)

	// a's incident edges (e1, e2) are gone: c no longer has a as an in-neighbor.
	nodes, _, err := d.Neighbors(ctx, ns, graph.NeighborOptions{NodeID: "c", Direction: graph.DirectionIn})
	require.NoError(t, err)
	assert.Equal(t, []string{"d"}, nodeIDs(nodes))
}
