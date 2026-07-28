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

func (s *Service) Acquire(ctx context.Context, req *leasev1.AcquireRequest) (*leasev1.AcquireResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key required")
	}
	if req.GetTtl() == nil || req.GetTtl().AsDuration() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "ttl required and must be > 0")
	}
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpLeaseAcquire)
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
	l, err := d.Acquire(ctx, ns, req.GetKey(), opts)
	if errors.Is(err, ErrAlreadyHeld) {
		return nil, status.Error(codes.FailedPrecondition, "already held")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, status.FromContextError(err).Err()
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "acquire: %v", err)
	}
	return &leasev1.AcquireResponse{Handle: leaseToProto(l, req.GetNamespace())}, nil
}

func (s *Service) Renew(ctx context.Context, req *leasev1.RenewRequest) (*leasev1.RenewResponse, error) {
	if req.GetKey() == "" || req.GetHolderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "key and holder_id required")
	}
	if req.GetTtl() == nil || req.GetTtl().AsDuration() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "ttl required and must be > 0")
	}
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpLeaseRenew)
	if err != nil {
		return nil, err
	}
	l, err := d.Renew(ctx, ns, req.GetKey(), req.GetHolderId(), req.GetTtl().AsDuration())
	if errors.Is(err, ErrNotHeld) {
		return nil, status.Error(codes.FailedPrecondition, "not held by this holder")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "renew: %v", err)
	}
	return &leasev1.RenewResponse{Handle: leaseToProto(l, req.GetNamespace())}, nil
}

func (s *Service) Release(ctx context.Context, req *leasev1.ReleaseRequest) (*leasev1.ReleaseResponse, error) {
	if req.GetKey() == "" || req.GetHolderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "key and holder_id required")
	}
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpLeaseRelease)
	if err != nil {
		return nil, err
	}
	existed, err := d.Release(ctx, ns, req.GetKey(), req.GetHolderId())
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
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpLeaseInspect)
	if err != nil {
		return nil, err
	}
	l, held, err := d.Inspect(ctx, ns, req.GetKey())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "inspect: %v", err)
	}
	resp := &leasev1.InspectResponse{Held: held}
	if held {
		resp.Handle = leaseToProto(l, req.GetNamespace())
	}
	return resp, nil
}

func (s *Service) List(ctx context.Context, req *leasev1.ListRequest) (*leasev1.ListResponse, error) {
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpLeaseList)
	if err != nil {
		return nil, err
	}
	leases, err := d.List(ctx, ns)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	resp := &leasev1.ListResponse{Leases: make([]*leasev1.LeaseHandle, 0, len(leases))}
	for _, l := range leases {
		resp.Leases = append(resp.Leases, leaseToProto(l, req.GetNamespace()))
	}
	return resp, nil
}

// leaseToProto maps a stored lease to the wire handle. displayNamespace is the
// config namespace the caller asked for — returned instead of l.Namespace so the
// internal tenant-qualified storage namespace never leaks to the client.
func leaseToProto(l Lease, displayNamespace string) *leasev1.LeaseHandle {
	out := &leasev1.LeaseHandle{
		HolderId:  l.HolderID,
		Namespace: displayNamespace,
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
