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
	reg *Registry
}

func NewService(reg *Registry) *Service {
	return &Service{reg: reg}
}

func (s *Service) driverFor(ctx context.Context, namespace string, op auth.Op) (Driver, error) {
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

func (s *Service) Append(ctx context.Context, req *episodicv1.AppendRequest) (*episodicv1.AppendResponse, error) {
	if req.GetType() == "" {
		return nil, status.Error(codes.InvalidArgument, "type required")
	}
	d, err := s.driverFor(ctx, req.GetNamespace(), auth.OpEpisodicAppend)
	if err != nil {
		return nil, err
	}
	ev, err := d.Append(ctx, req.GetNamespace(), AppendOptions{
		Type:      req.GetType(),
		Payload:   req.GetPayload(),
		Metadata:  req.GetMetadata(),
		Role:      req.GetRole(),
		SessionID: req.GetSessionId(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "append: %v", err)
	}
	return &episodicv1.AppendResponse{Event: eventToProto(ev)}, nil
}

func (s *Service) Range(req *episodicv1.RangeRequest, stream episodicv1.Episodic_RangeServer) error {
	ctx := stream.Context()
	d, err := s.driverFor(ctx, req.GetNamespace(), auth.OpEpisodicRange)
	if err != nil {
		return err
	}
	err = d.Range(ctx, req.GetNamespace(), RangeOptions{
		AfterCursor:  req.GetAfterCursor(),
		BeforeCursor: req.GetBeforeCursor(),
		Limit:        req.GetLimit(),
		Reverse:      req.GetReverse(),
		AfterTime:    tsToTime(req.GetAfterTime()),
		BeforeTime:   tsToTime(req.GetBeforeTime()),
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
	d, err := s.driverFor(ctx, req.GetNamespace(), auth.OpEpisodicTail)
	if err != nil {
		return err
	}
	err = d.Tail(ctx, req.GetNamespace(), TailOptions{
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

func eventToProto(e Event) *episodicv1.Event {
	return &episodicv1.Event{
		Id:        e.ID,
		Cursor:    e.Cursor,
		Timestamp: timestamppb.New(e.Timestamp),
		Type:      e.Type,
		Payload:   e.Payload,
		Metadata:  e.Metadata,
		Role:      e.Role,
		SessionId: e.SessionID,
	}
}

// tsToTime converts an optional protobuf timestamp to a Go time, returning the
// zero time when the timestamp is nil.
func tsToTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}
