package graph

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a node or edge id is absent.
var ErrNotFound = errors.New("graph: not found")

// ErrEdgesExist is returned by DeleteNode when the node still has incident
// edges and cascade was not requested.
var ErrEdgesExist = errors.New("graph: node has incident edges")

// Direction selects which edges a read follows. The zero value is "both".
type Direction uint8

const (
	DirectionBoth Direction = iota
	DirectionOut
	DirectionIn
)

// Node is a unit within a graph namespace. The id is caller-supplied so it can
// be shared with a semantic record id for agent-orchestrated hybrid recall.
type Node struct {
	ID        string
	Labels    []string
	Props     map[string]string
	CreatedAt time.Time
}

// Edge is a typed, directed relationship between two nodes in the same namespace.
type Edge struct {
	ID        string
	Type      string
	From      string
	To        string
	Props     map[string]string
	CreatedAt time.Time
}

// NeighborOptions narrows a 1-hop Neighbors read. Limit is a bounded value the
// service supplies (never unbounded).
type NeighborOptions struct {
	NodeID     string
	EdgeTypes  []string // empty = any type
	Direction  Direction
	NodeLabels []string // filter returned neighbors; empty = any
	Limit      uint32
}

// TraverseOptions narrows a bounded multi-hop Traverse read. All four bounds
// are hard-capped by the service before they reach the driver. A zero bound
// means "unbounded" at the driver level (the service always supplies a value).
type TraverseOptions struct {
	StartID   string
	EdgeTypes []string
	Direction Direction
	MaxDepth  uint32
	MaxNodes  uint32 // result cap: total nodes
	MaxEdges  uint32 // result cap: total edges
	FanOut    uint32 // work cap: incident edges examined per frontier node
}

// Subgraph is a Traverse result: the reached nodes and the edges among them.
type Subgraph struct {
	Nodes []Node
	Edges []Edge
}

// Driver is the contract every graph backend implements. Implementations must
// be safe for concurrent use. It FRONTS a graph engine — it does not implement
// a query language or any reasoning.
type Driver interface {
	UpsertNodes(ctx context.Context, namespace string, nodes []Node) error
	UpsertEdges(ctx context.Context, namespace string, edges []Edge) error
	GetNode(ctx context.Context, namespace, id string) (Node, error)
	Neighbors(ctx context.Context, namespace string, opts NeighborOptions) (nodes []Node, edges []Edge, err error)
	Traverse(ctx context.Context, namespace string, opts TraverseOptions) (Subgraph, error)
	DeleteNode(ctx context.Context, namespace, id string, cascade bool) (existed bool, err error)
	DeleteEdge(ctx context.Context, namespace, id string) (existed bool, err error)
	Close() error
}
