package episodic

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"memsidecar/internal/auth"

	episodicv1 "memsidecar/gen/memsidecar/episodic/v1"
)

const Block = "episodic"

// Service implements the generated EpisodicServer.
type Service struct {
	episodicv1.UnimplementedEpisodicServer
	reg             *Registry
	tenantIsolation bool
}

func NewService(reg *Registry, tenantIsolation bool) *Service {
	return &Service{reg: reg, tenantIsolation: tenantIsolation}
}

// driverFor authorizes the request and returns the backing driver plus the
// storage namespace to address it by. The storage namespace is the requested
// (config) namespace qualified with the caller's tenant when tenant isolation
// is enabled, so drivers never see cross-tenant data. Resolution and the
// capability scope check both use the config namespace.
func (s *Service) driverFor(ctx context.Context, requested string, op auth.Op) (Driver, string, error) {
	cap, ok := auth.FromContext(ctx)
	if !ok {
		return nil, "", status.Error(codes.Unauthenticated, "missing capability")
	}
	if !cap.PermitsNamespace(Block, requested) {
		return nil, "", status.Errorf(codes.PermissionDenied, "namespace %s/%s not in capability scope", Block, requested)
	}
	if !cap.PermitsOp(op) {
		return nil, "", status.Errorf(codes.PermissionDenied, "op %s not in capability scope", op)
	}
	d, ok := s.reg.Resolve(requested)
	if !ok {
		return nil, "", status.Errorf(codes.NotFound, "namespace %q not configured", requested)
	}
	storageNS := auth.QualifyNamespace(cap.Scope.Tenant, requested, s.tenantIsolation)
	return d, storageNS, nil
}

func (s *Service) Append(ctx context.Context, req *episodicv1.AppendRequest) (*episodicv1.AppendResponse, error) {
	if req.GetType() == "" {
		return nil, status.Error(codes.InvalidArgument, "type required")
	}
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpEpisodicAppend)
	if err != nil {
		return nil, err
	}
	ev, err := d.Append(ctx, ns, AppendOptions{
		Type:       req.GetType(),
		Payload:    req.GetPayload(),
		Metadata:   req.GetMetadata(),
		Role:       req.GetRole(),
		SessionID:  req.GetSessionId(),
		DedupKey:   req.GetDedupKey(),
		Supersedes: req.GetSupersedes(),
		Source:     req.GetSource(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "append: %v", err)
	}
	return &episodicv1.AppendResponse{Event: eventToProto(ev)}, nil
}

func (s *Service) Range(req *episodicv1.RangeRequest, stream episodicv1.Episodic_RangeServer) error {
	ctx := stream.Context()
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpEpisodicRange)
	if err != nil {
		return err
	}
	err = d.Range(ctx, ns, RangeOptions{
		AfterCursor:    req.GetAfterCursor(),
		BeforeCursor:   req.GetBeforeCursor(),
		Limit:          req.GetLimit(),
		Reverse:        req.GetReverse(),
		AfterTime:      tsToTime(req.GetAfterTime()),
		BeforeTime:     tsToTime(req.GetBeforeTime()),
		IncludeDeleted: req.GetIncludeDeleted(),
		SessionID:      req.GetSessionId(),
		Role:           req.GetRole(),
		Type:           req.GetType(),
	}, func(ev Event) error {
		return stream.Send(eventToProto(ev))
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return status.Errorf(codes.Internal, "range: %v", err)
	}
	return nil
}

func (s *Service) Tail(req *episodicv1.TailRequest, stream episodicv1.Episodic_TailServer) error {
	ctx := stream.Context()
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpEpisodicTail)
	if err != nil {
		return err
	}
	err = d.Tail(ctx, ns, TailOptions{
		AfterCursor:       req.GetAfterCursor(),
		IncludeHistorical: req.GetIncludeHistorical(),
	}, func(ev Event) error {
		return stream.Send(eventToProto(ev))
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return status.Errorf(codes.Internal, "tail: %v", err)
	}
	return nil
}

func (s *Service) Expire(ctx context.Context, req *episodicv1.ExpireRequest) (*episodicv1.ExpireResponse, error) {
	action, ok := expireActionFromProto(req.GetAction())
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "action must be one of soft_delete, hard_delete")
	}
	if req.GetMaxRows() == 0 {
		return nil, status.Error(codes.InvalidArgument, "max_rows must be > 0")
	}
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpEpisodicExpire)
	if err != nil {
		return nil, err
	}
	affected, err := d.Expire(ctx, ns, ExpireOptions{
		BeforeCursor: req.GetBeforeCursor(),
		BeforeTime:   tsToTime(req.GetBeforeTime()),
		Action:       action,
		MaxRows:      req.GetMaxRows(),
	})
	if err != nil {
		if errors.Is(err, ErrExpireWindowRequired) {
			return nil, status.Error(codes.InvalidArgument, "before_cursor or before_time required")
		}
		return nil, status.Errorf(codes.Internal, "expire: %v", err)
	}
	return &episodicv1.ExpireResponse{Affected: affected}, nil
}

func expireActionFromProto(a episodicv1.ExpireAction) (ExpireAction, bool) {
	switch a {
	case episodicv1.ExpireAction_EXPIRE_ACTION_SOFT_DELETE:
		return ExpireSoftDelete, true
	case episodicv1.ExpireAction_EXPIRE_ACTION_HARD_DELETE:
		return ExpireHardDelete, true
	default:
		return ExpireActionUnspecified, false
	}
}

func eventToProto(e Event) *episodicv1.Event {
	out := &episodicv1.Event{
		Id:         e.ID,
		Cursor:     e.Cursor,
		Timestamp:  timestamppb.New(e.Timestamp),
		Type:       e.Type,
		Payload:    e.Payload,
		Metadata:   e.Metadata,
		Role:       e.Role,
		SessionId:  e.SessionID,
		Supersedes: e.Supersedes,
		Source:     e.Source,
	}
	if !e.DeletedAt.IsZero() {
		out.DeletedAt = timestamppb.New(e.DeletedAt)
	}
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
