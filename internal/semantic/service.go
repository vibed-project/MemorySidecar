package semantic

import (
	"context"
	"errors"
	"time"

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
	reg   *Registry
	instr *instruments
}

func NewService(reg *Registry) *Service {
	return &Service{reg: reg, instr: newInstruments()}
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
			ID:         r.GetId(),
			Content:    r.GetContent(),
			Payload:    r.GetPayload(),
			Vector:     r.GetVector(),
			Metadata:   r.GetMetadata(),
			ValidFrom:  tsToTime(r.GetValidFrom()),
			ValidTo:    tsToTime(r.GetValidTo()),
			DeletedAt:  tsToTime(r.GetDeletedAt()),
			Supersedes: r.GetSupersedes(),
			Source:     r.GetSource(),
			IfVersion:  r.IfVersion,
		}
	}
	for j, idx := range toEmbedIdx {
		recs[idx].Vector = embedded[j]
	}

	upStart := time.Now()
	upErr := b.Driver.Upsert(ctx, recs)
	s.instr.backend(ctx, auth.OpSemanticUpsert, req.GetNamespace(), time.Since(upStart))
	if upErr != nil {
		if errors.Is(upErr, ErrVersionMismatch) {
			return nil, status.Error(codes.FailedPrecondition, "version mismatch")
		}
		return nil, status.Errorf(codes.Internal, "upsert: %v", upErr)
	}
	s.instr.result(ctx, auth.OpSemanticUpsert, req.GetNamespace(), len(recs))

	ids := make([]string, len(recs))
	versions := make([]uint64, len(recs))
	for i, r := range recs {
		ids[i] = r.ID
		versions[i] = r.Version
	}
	return &semanticv1.UpsertResponse{Ids: ids, Versions: versions}, nil
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

	preds, err := predicatesFromProto(req.GetPredicates())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	searchStart := time.Now()
	hits, err := b.Driver.Search(ctx, SearchOptions{
		QueryVector:        queryVec,
		TopK:               topK,
		Filter:             req.GetFilter(),
		Predicates:         preds,
		CreatedAfter:       tsToTime(req.GetCreatedAfter()),
		CreatedBefore:      tsToTime(req.GetCreatedBefore()),
		IncludePayload:     req.GetIncludePayload(),
		IncludeVector:      req.GetIncludeVector(),
		AsOf:               tsToTime(req.GetAsOf()),
		IncludeInvalidated: req.GetIncludeInvalidated(),
		IDsOnly:            req.GetIdsOnly(),
	})
	s.instr.backend(ctx, auth.OpSemanticSearch, req.GetNamespace(), time.Since(searchStart))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "search: %v", err)
	}
	s.instr.result(ctx, auth.OpSemanticSearch, req.GetNamespace(), len(hits))
	if len(hits) > 0 {
		s.instr.searchTopScore(ctx, req.GetNamespace(), hits[0].Score)
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
	delStart := time.Now()
	existed, err := b.Driver.Delete(ctx, req.GetId(), DeleteOptions{Hard: req.GetHard()})
	s.instr.backend(ctx, auth.OpSemanticDelete, req.GetNamespace(), time.Since(delStart))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &semanticv1.DeleteResponse{Existed: existed}, nil
}

func (s *Service) Expire(ctx context.Context, req *semanticv1.ExpireRequest) (*semanticv1.ExpireResponse, error) {
	action, ok := expireActionFromProto(req.GetAction())
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "action must be one of invalidate, soft_delete, hard_delete")
	}
	if req.GetMaxRows() == 0 {
		return nil, status.Error(codes.InvalidArgument, "max_rows must be > 0")
	}
	b, err := s.resolve(ctx, req.GetNamespace(), auth.OpSemanticExpire)
	if err != nil {
		return nil, err
	}
	expStart := time.Now()
	affected, err := b.Driver.Expire(ctx, ExpireOptions{
		Filter:  req.GetFilter(),
		Action:  action,
		MaxRows: req.GetMaxRows(),
	})
	s.instr.backend(ctx, auth.OpSemanticExpire, req.GetNamespace(), time.Since(expStart))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "expire: %v", err)
	}
	s.instr.result(ctx, auth.OpSemanticExpire, req.GetNamespace(), int(affected))
	return &semanticv1.ExpireResponse{Affected: affected}, nil
}

// expireActionFromProto maps the wire enum to the driver action, reporting
// false for the unspecified/unknown value.
func expireActionFromProto(a semanticv1.ExpireAction) (ExpireAction, bool) {
	switch a {
	case semanticv1.ExpireAction_EXPIRE_ACTION_INVALIDATE:
		return ExpireInvalidate, true
	case semanticv1.ExpireAction_EXPIRE_ACTION_SOFT_DELETE:
		return ExpireSoftDelete, true
	case semanticv1.ExpireAction_EXPIRE_ACTION_HARD_DELETE:
		return ExpireHardDelete, true
	default:
		return ExpireActionUnspecified, false
	}
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
	if !r.ValidFrom.IsZero() {
		out.ValidFrom = timestamppb.New(r.ValidFrom)
	}
	if !r.ValidTo.IsZero() {
		out.ValidTo = timestamppb.New(r.ValidTo)
	}
	if !r.DeletedAt.IsZero() {
		out.DeletedAt = timestamppb.New(r.DeletedAt)
	}
	out.Supersedes = r.Supersedes
	out.Source = r.Source
	out.Version = r.Version
	return out
}

// tsToTime converts an optional protobuf timestamp to a Go time, returning the
// zero time when the timestamp is nil.
func tsToTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// predicatesFromProto converts and validates the wire predicates. A validation
// error is returned for the caller to surface as InvalidArgument.
func predicatesFromProto(pb []*semanticv1.FieldPredicate) ([]FieldPredicate, error) {
	if len(pb) == 0 {
		return nil, nil
	}
	out := make([]FieldPredicate, len(pb))
	for i, p := range pb {
		fp := FieldPredicate{
			Key:    p.GetKey(),
			Op:     predicateOpFromProto(p.GetOp()),
			Values: p.GetValues(),
		}
		if err := fp.Validate(); err != nil {
			return nil, err
		}
		out[i] = fp
	}
	return out, nil
}

func predicateOpFromProto(op semanticv1.PredicateOp) PredicateOp {
	switch op {
	case semanticv1.PredicateOp_PREDICATE_OP_EQ:
		return PredEQ
	case semanticv1.PredicateOp_PREDICATE_OP_NEQ:
		return PredNEQ
	case semanticv1.PredicateOp_PREDICATE_OP_GT:
		return PredGT
	case semanticv1.PredicateOp_PREDICATE_OP_GTE:
		return PredGTE
	case semanticv1.PredicateOp_PREDICATE_OP_LT:
		return PredLT
	case semanticv1.PredicateOp_PREDICATE_OP_LTE:
		return PredLTE
	case semanticv1.PredicateOp_PREDICATE_OP_IN:
		return PredIN
	default:
		return PredUnspecified
	}
}
