package graph

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"memsidecar/internal/auth"

	graphv1 "memsidecar/gen/memsidecar/graph/v1"
)

const Block = "graph"

// Server-side traversal bounds (ADR-0002 §4/§8). Cost is always capped here,
// independent of policy; policy `cap` rules can additionally reject earlier
// with ResourceExhausted.
const (
	defaultNeighborLimit uint32 = 50
	maxNeighborLimit     uint32 = 1000
	defaultTraverseDepth uint32 = 1
	maxTraverseDepth     uint32 = 5
	defaultTraverseNodes uint32 = 1000
	maxTraverseNodes     uint32 = 10000
	// Fixed hard bounds on traversal result/work — not caller-tunable, so a
	// dense or high-degree region can never blow up the response or the scan.
	maxTraverseEdges  uint32 = 20000
	maxTraverseFanOut uint32 = 1000
)

// Service implements the generated GraphServer.
type Service struct {
	graphv1.UnimplementedGraphServer
	reg *Registry
}

func NewService(reg *Registry) *Service { return &Service{reg: reg} }

func (s *Service) resolve(ctx context.Context, namespace string, op auth.Op) (Driver, error) {
	cap, ok := auth.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing capability")
	}
	if !cap.PermitsNamespace(Block, namespace) {
		return nil, status.Errorf(codes.PermissionDenied, "namespace %s/%s not in capability scope", Block, namespace)
	}
	if !cap.PermitsOp(op) {
		return nil, status.Errorf(codes.PermissionDenied, "op %s not in capability scope", op)
	}
	d, ok := s.reg.Resolve(namespace)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "namespace %q not configured", namespace)
	}
	return d, nil
}

func (s *Service) UpsertNodes(ctx context.Context, req *graphv1.UpsertNodesRequest) (*graphv1.UpsertNodesResponse, error) {
	d, err := s.resolve(ctx, req.GetNamespace(), auth.OpGraphUpsert)
	if err != nil {
		return nil, err
	}
	pn := req.GetNodes()
	nodes := make([]Node, len(pn))
	ids := make([]string, len(pn))
	for i, n := range pn {
		if n.GetId() == "" {
			return nil, status.Errorf(codes.InvalidArgument, "nodes[%d]: id required", i)
		}
		nodes[i] = Node{
			ID:        n.GetId(),
			Labels:    n.GetLabels(),
			Props:     n.GetProps(),
			CreatedAt: tsToTime(n.GetCreatedAt()),
		}
		ids[i] = n.GetId()
	}
	if err := d.UpsertNodes(ctx, req.GetNamespace(), nodes); err != nil {
		return nil, status.Errorf(codes.Internal, "upsert nodes: %v", err)
	}
	return &graphv1.UpsertNodesResponse{Ids: ids}, nil
}

func (s *Service) UpsertEdges(ctx context.Context, req *graphv1.UpsertEdgesRequest) (*graphv1.UpsertEdgesResponse, error) {
	d, err := s.resolve(ctx, req.GetNamespace(), auth.OpGraphUpsert)
	if err != nil {
		return nil, err
	}
	pe := req.GetEdges()
	edges := make([]Edge, len(pe))
	ids := make([]string, len(pe))
	for i, e := range pe {
		if e.GetId() == "" || e.GetType() == "" || e.GetFrom() == "" || e.GetTo() == "" {
			return nil, status.Errorf(codes.InvalidArgument, "edges[%d]: id, type, from, to are all required", i)
		}
		edges[i] = Edge{
			ID:        e.GetId(),
			Type:      e.GetType(),
			From:      e.GetFrom(),
			To:        e.GetTo(),
			Props:     e.GetProps(),
			CreatedAt: tsToTime(e.GetCreatedAt()),
		}
		ids[i] = e.GetId()
	}
	if err := d.UpsertEdges(ctx, req.GetNamespace(), edges); err != nil {
		return nil, status.Errorf(codes.Internal, "upsert edges: %v", err)
	}
	return &graphv1.UpsertEdgesResponse{Ids: ids}, nil
}

func (s *Service) GetNode(ctx context.Context, req *graphv1.GetNodeRequest) (*graphv1.Node, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	d, err := s.resolve(ctx, req.GetNamespace(), auth.OpGraphGet)
	if err != nil {
		return nil, err
	}
	n, err := d.GetNode(ctx, req.GetNamespace(), req.GetId())
	if errors.Is(err, ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "node %q not found", req.GetId())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get node: %v", err)
	}
	return nodeToProto(n), nil
}

func (s *Service) Neighbors(ctx context.Context, req *graphv1.NeighborsRequest) (*graphv1.NeighborsResponse, error) {
	if req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id required")
	}
	d, err := s.resolve(ctx, req.GetNamespace(), auth.OpGraphQuery)
	if err != nil {
		return nil, err
	}
	nodes, edges, err := d.Neighbors(ctx, req.GetNamespace(), NeighborOptions{
		NodeID:     req.GetNodeId(),
		EdgeTypes:  req.GetEdgeTypes(),
		Direction:  directionFromProto(req.GetDirection()),
		NodeLabels: req.GetNodeLabels(),
		Limit:      clamp(req.GetLimit(), defaultNeighborLimit, maxNeighborLimit),
	})
	if errors.Is(err, ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "node %q not found", req.GetNodeId())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "neighbors: %v", err)
	}
	return &graphv1.NeighborsResponse{Nodes: nodesToProto(nodes), Edges: edgesToProto(edges)}, nil
}

func (s *Service) Traverse(ctx context.Context, req *graphv1.TraverseRequest) (*graphv1.Subgraph, error) {
	if req.GetStartId() == "" {
		return nil, status.Error(codes.InvalidArgument, "start_id required")
	}
	d, err := s.resolve(ctx, req.GetNamespace(), auth.OpGraphQuery)
	if err != nil {
		return nil, err
	}
	sub, err := d.Traverse(ctx, req.GetNamespace(), TraverseOptions{
		StartID:   req.GetStartId(),
		EdgeTypes: req.GetEdgeTypes(),
		Direction: directionFromProto(req.GetDirection()),
		MaxDepth:  clamp(req.GetDepth(), defaultTraverseDepth, maxTraverseDepth),
		MaxNodes:  clamp(req.GetMaxNodes(), defaultTraverseNodes, maxTraverseNodes),
		MaxEdges:  maxTraverseEdges,
		FanOut:    maxTraverseFanOut,
	})
	if errors.Is(err, ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "node %q not found", req.GetStartId())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "traverse: %v", err)
	}
	return &graphv1.Subgraph{Nodes: nodesToProto(sub.Nodes), Edges: edgesToProto(sub.Edges)}, nil
}

func (s *Service) DeleteNode(ctx context.Context, req *graphv1.DeleteNodeRequest) (*graphv1.DeleteNodeResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	d, err := s.resolve(ctx, req.GetNamespace(), auth.OpGraphDelete)
	if err != nil {
		return nil, err
	}
	existed, err := d.DeleteNode(ctx, req.GetNamespace(), req.GetId(), req.GetCascade())
	if errors.Is(err, ErrEdgesExist) {
		return nil, status.Errorf(codes.FailedPrecondition, "node %q has incident edges; set cascade=true to remove them", req.GetId())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete node: %v", err)
	}
	return &graphv1.DeleteNodeResponse{Existed: existed}, nil
}

func (s *Service) DeleteEdge(ctx context.Context, req *graphv1.DeleteEdgeRequest) (*graphv1.DeleteEdgeResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	d, err := s.resolve(ctx, req.GetNamespace(), auth.OpGraphDelete)
	if err != nil {
		return nil, err
	}
	existed, err := d.DeleteEdge(ctx, req.GetNamespace(), req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete edge: %v", err)
	}
	return &graphv1.DeleteEdgeResponse{Existed: existed}, nil
}

// clamp applies a server-side default (when v==0) and hard maximum to a
// caller-supplied bound.
func clamp(v, def, max uint32) uint32 {
	if v == 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func directionFromProto(d graphv1.Direction) Direction {
	switch d {
	case graphv1.Direction_DIRECTION_OUT:
		return DirectionOut
	case graphv1.Direction_DIRECTION_IN:
		return DirectionIn
	default:
		return DirectionBoth
	}
}

func tsToTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

func nodeToProto(n Node) *graphv1.Node {
	out := &graphv1.Node{Id: n.ID, Labels: n.Labels, Props: n.Props}
	if !n.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(n.CreatedAt)
	}
	return out
}

func edgeToProto(e Edge) *graphv1.Edge {
	out := &graphv1.Edge{Id: e.ID, Type: e.Type, From: e.From, To: e.To, Props: e.Props}
	if !e.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(e.CreatedAt)
	}
	return out
}

func nodesToProto(ns []Node) []*graphv1.Node {
	out := make([]*graphv1.Node, len(ns))
	for i, n := range ns {
		out[i] = nodeToProto(n)
	}
	return out
}

func edgesToProto(es []Edge) []*graphv1.Edge {
	out := make([]*graphv1.Edge, len(es))
	for i, e := range es {
		out[i] = edgeToProto(e)
	}
	return out
}
