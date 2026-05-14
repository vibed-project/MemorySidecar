package lease

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"memsidecar/internal/auth"

	leasev1 "memsidecar/gen/memsidecar/lease/v1"
)

const Block = "lease"

// Service implements the generated LeaseServer.
type Service struct {
	leasev1.UnimplementedLeaseServer
	reg *Registry
}

func NewService(reg *Registry) *Service { return &Service{reg: reg} }

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

func (s *Service) Acquire(ctx context.Context, req *leasev1.AcquireRequest) (*leasev1.AcquireResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key required")
	}
	if req.GetTtl() == nil || req.GetTtl().AsDuration() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "ttl required and must be > 0")
	}
	d, err := s.driverFor(ctx, req.GetNamespace(), auth.OpLeaseAcquire)
	if err != nil {
		return nil, err
	}
	opts := AcquireOptions{
		TTL:      req.GetTtl().AsDuration(),
		Metadata: req.GetMetadata(),
	}
	if req.WaitFor != nil {
		opts.WaitFor = req.GetWaitFor().AsDuration()
	}
	l, err := d.Acquire(ctx, req.GetNamespace(), req.GetKey(), opts)
	if errors.Is(err, ErrAlreadyHeld) {
		return nil, status.Error(codes.FailedPrecondition, "already held")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, status.FromContextError(err).Err()
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "acquire: %v", err)
	}
	return &leasev1.AcquireResponse{Handle: leaseToProto(l)}, nil
}

func (s *Service) Renew(ctx context.Context, req *leasev1.RenewRequest) (*leasev1.RenewResponse, error) {
	if req.GetKey() == "" || req.GetHolderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "key and holder_id required")
	}
	if req.GetTtl() == nil || req.GetTtl().AsDuration() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "ttl required and must be > 0")
	}
	d, err := s.driverFor(ctx, req.GetNamespace(), auth.OpLeaseRenew)
	if err != nil {
		return nil, err
	}
	l, err := d.Renew(ctx, req.GetNamespace(), req.GetKey(), req.GetHolderId(), req.GetTtl().AsDuration())
	if errors.Is(err, ErrNotHeld) {
		return nil, status.Error(codes.FailedPrecondition, "not held by this holder")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "renew: %v", err)
	}
	return &leasev1.RenewResponse{Handle: leaseToProto(l)}, nil
}

func (s *Service) Release(ctx context.Context, req *leasev1.ReleaseRequest) (*leasev1.ReleaseResponse, error) {
	if req.GetKey() == "" || req.GetHolderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "key and holder_id required")
	}
	d, err := s.driverFor(ctx, req.GetNamespace(), auth.OpLeaseRelease)
	if err != nil {
		return nil, err
	}
	existed, err := d.Release(ctx, req.GetNamespace(), req.GetKey(), req.GetHolderId())
	if errors.Is(err, ErrNotHeld) {
		return nil, status.Error(codes.FailedPrecondition, "not held by this holder")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "release: %v", err)
	}
	return &leasev1.ReleaseResponse{Existed: existed}, nil
}

func (s *Service) Inspect(ctx context.Context, req *leasev1.InspectRequest) (*leasev1.InspectResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key required")
	}
	d, err := s.driverFor(ctx, req.GetNamespace(), auth.OpLeaseInspect)
	if err != nil {
		return nil, err
	}
	l, held, err := d.Inspect(ctx, req.GetNamespace(), req.GetKey())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "inspect: %v", err)
	}
	resp := &leasev1.InspectResponse{Held: held}
	if held {
		resp.Handle = leaseToProto(l)
	}
	return resp, nil
}

func leaseToProto(l Lease) *leasev1.LeaseHandle {
	out := &leasev1.LeaseHandle{
		HolderId:  l.HolderID,
		Namespace: l.Namespace,
		Key:       l.Key,
		Metadata:  l.Metadata,
	}
	if !l.AcquiredAt.IsZero() {
		out.AcquiredAt = timestamppb.New(l.AcquiredAt)
	}
	if !l.ExpiresAt.IsZero() {
		out.ExpiresAt = timestamppb.New(l.ExpiresAt)
	}
	return out
}
