package semantic

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"memsidecar/internal/auth"

	semanticv1 "memsidecar/gen/memsidecar/semantic/v1"
)

const Block = "semantic"

const defaultTopK = 10

// Service implements the generated SemanticServer.
type Service struct {
	semanticv1.UnimplementedSemanticServer
	reg *Registry
}

func NewService(reg *Registry) *Service {
	return &Service{reg: reg}
}

func (s *Service) resolve(ctx context.Context, namespace string, op auth.Op) (BoundNamespace, error) {
	cap, ok := auth.FromContext(ctx)
	if !ok {
		return BoundNamespace{}, status.Error(codes.Unauthenticated, "missing capability")
	}
	if !cap.PermitsNamespace(Block, namespace) {
		return BoundNamespace{}, status.Errorf(codes.PermissionDenied, "namespace %s/%s not in capability scope", Block, namespace)
	}
	if !cap.PermitsOp(op) {
		return BoundNamespace{}, status.Errorf(codes.PermissionDenied, "op %s not in capability scope", op)
	}
	b, ok := s.reg.Resolve(namespace)
	if !ok {
		return BoundNamespace{}, status.Errorf(codes.NotFound, "namespace %q not configured", namespace)
	}
	return b, nil
}

func (s *Service) Upsert(ctx context.Context, req *semanticv1.UpsertRequest) (*semanticv1.UpsertResponse, error) {
	b, err := s.resolve(ctx, req.GetNamespace(), auth.OpSemanticUpsert)
	if err != nil {
		return nil, err
	}
	pbRecs := req.GetRecords()
	if len(pbRecs) == 0 {
		return &semanticv1.UpsertResponse{}, nil
	}

	dim := b.Embedder.Dimensions()

	// First pass: identify which records need embedding (have content but no
	// vector). Validate dimensions on records that already carry vectors.
	toEmbedIdx := make([]int, 0, len(pbRecs))
	toEmbedText := make([]string, 0, len(pbRecs))
	for i, r := range pbRecs {
		if len(r.GetVector()) > 0 {
			if len(r.GetVector()) != dim {
				return nil, status.Errorf(codes.InvalidArgument,
					"records[%d].vector dim %d != namespace dim %d", i, len(r.GetVector()), dim)
			}
			continue
		}
		if r.GetContent() == "" {
			return nil, status.Errorf(codes.InvalidArgument,
				"records[%d]: at least one of content or vector required", i)
		}
		toEmbedIdx = append(toEmbedIdx, i)
		toEmbedText = append(toEmbedText, r.GetContent())
	}

	var embedded [][]float32
	if len(toEmbedText) > 0 {
		embedded, err = b.Embedder.Embed(ctx, toEmbedText)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "embed: %v", err)
		}
		if len(embedded) != len(toEmbedText) {
			return nil, status.Errorf(codes.Internal,
				"embedder returned %d vectors for %d inputs", len(embedded), len(toEmbedText))
		}
		for i, v := range embedded {
			if len(v) != dim {
				return nil, status.Errorf(codes.Internal,
					"embedder returned dim %d, expected %d (vector %d)", len(v), dim, i)
			}
		}
	}

	recs := make([]Record, len(pbRecs))
	for i, r := range pbRecs {
		recs[i] = Record{
			ID:       r.GetId(),
			Content:  r.GetContent(),
			Payload:  r.GetPayload(),
			Vector:   r.GetVector(),
			Metadata: r.GetMetadata(),
		}
	}
	for j, idx := range toEmbedIdx {
		recs[idx].Vector = embedded[j]
	}

	if err := b.Driver.Upsert(ctx, recs); err != nil {
		return nil, status.Errorf(codes.Internal, "upsert: %v", err)
	}

	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.ID
	}
	return &semanticv1.UpsertResponse{Ids: ids}, nil
}

func (s *Service) Search(ctx context.Context, req *semanticv1.SearchRequest) (*semanticv1.SearchResponse, error) {
	b, err := s.resolve(ctx, req.GetNamespace(), auth.OpSemanticSearch)
	if err != nil {
		return nil, err
	}

	queryVec := req.GetQueryVector()
	switch {
	case req.GetQueryText() != "" && len(queryVec) > 0:
		return nil, status.Error(codes.InvalidArgument, "set query_text OR query_vector, not both")
	case req.GetQueryText() == "" && len(queryVec) == 0:
		return nil, status.Error(codes.InvalidArgument, "query_text or query_vector required")
	case req.GetQueryText() != "":
		vs, err := b.Embedder.Embed(ctx, []string{req.GetQueryText()})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "embed query: %v", err)
		}
		queryVec = vs[0]
	}
	if dim := b.Embedder.Dimensions(); len(queryVec) != dim {
		return nil, status.Errorf(codes.InvalidArgument,
			"query vector dim %d != namespace dim %d", len(queryVec), dim)
	}

	topK := req.GetTopK()
	if topK == 0 {
		topK = defaultTopK
	}

	hits, err := b.Driver.Search(ctx, SearchOptions{
		QueryVector:    queryVec,
		TopK:           topK,
		Filter:         req.GetFilter(),
		IncludePayload: req.GetIncludePayload(),
		IncludeVector:  req.GetIncludeVector(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "search: %v", err)
	}

	resp := &semanticv1.SearchResponse{Hits: make([]*semanticv1.Hit, len(hits))}
	for i, h := range hits {
		resp.Hits[i] = &semanticv1.Hit{
			Record: recordToProto(h.Record),
			Score:  h.Score,
		}
	}
	return resp, nil
}

func (s *Service) Delete(ctx context.Context, req *semanticv1.DeleteRequest) (*semanticv1.DeleteResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	b, err := s.resolve(ctx, req.GetNamespace(), auth.OpSemanticDelete)
	if err != nil {
		return nil, err
	}
	existed, err := b.Driver.Delete(ctx, req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &semanticv1.DeleteResponse{Existed: existed}, nil
}

func recordToProto(r Record) *semanticv1.Record {
	out := &semanticv1.Record{
		Id:       r.ID,
		Content:  r.Content,
		Payload:  r.Payload,
		Vector:   r.Vector,
		Metadata: r.Metadata,
	}
	if !r.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(r.CreatedAt)
	}
	return out
}

