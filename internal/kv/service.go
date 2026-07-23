package kv

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"memsidecar/internal/auth"

	kvv1 "memsidecar/gen/memsidecar/kv/v1"
)

const Block = "kv"

// Service implements the generated KVServer.
type Service struct {
	kvv1.UnimplementedKVServer
	reg             *Registry
	tenantIsolation bool
	// quotas maps a config namespace name to its live-key cap (0/absent =
	// unlimited). Enforced in the driver against the tenant-qualified storage
	// namespace, so with tenant isolation on it is a per-tenant cap.
	quotas map[string]int
}

// NewService builds a KV service. quotas, keyed by config namespace name, caps
// the live keys a namespace may hold (0/absent = unlimited); pass nil for none.
func NewService(reg *Registry, tenantIsolation bool, quotas map[string]int) *Service {
	return &Service{reg: reg, tenantIsolation: tenantIsolation, quotas: quotas}
}

// namespaceDriver authorizes the request and returns the backing driver plus
// the storage namespace to address it by. The storage namespace is the
// requested (config) namespace qualified with the caller's tenant when tenant
// isolation is enabled, so drivers never see cross-tenant data. Resolution and
// the capability scope check both use the config namespace.
func (s *Service) namespaceDriver(ctx context.Context, requested string, op auth.Op) (Driver, string, error) {
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

func (s *Service) Get(ctx context.Context, req *kvv1.GetRequest) (*kvv1.GetResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key required")
	}
	d, ns, err := s.namespaceDriver(ctx, req.GetNamespace(), auth.OpKVGet)
	if err != nil {
		return nil, err
	}
	rec, err := d.Get(ctx, ns, req.GetKey())
	if errors.Is(err, ErrNotFound) {
		return &kvv1.GetResponse{Found: false}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get: %v", err)
	}
	return recordToGetResponse(rec), nil
}

func (s *Service) MultiGet(ctx context.Context, req *kvv1.MultiGetRequest) (*kvv1.MultiGetResponse, error) {
	d, ns, err := s.namespaceDriver(ctx, req.GetNamespace(), auth.OpKVGet)
	if err != nil {
		return nil, err
	}
	recs, err := d.MultiGet(ctx, ns, req.GetKeys())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "multiget: %v", err)
	}
	resp := &kvv1.MultiGetResponse{Items: make([]*kvv1.KVItem, 0, len(recs))}
	for _, r := range recs {
		resp.Items = append(resp.Items, recordToItem(r))
	}
	return resp, nil
}

func (s *Service) Put(ctx context.Context, req *kvv1.PutRequest) (*kvv1.PutResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key required")
	}
	d, ns, err := s.namespaceDriver(ctx, req.GetNamespace(), auth.OpKVPut)
	if err != nil {
		return nil, err
	}
	opts := PutOptions{
		Value:       req.GetValue(),
		ContentType: req.GetContentType(),
		Metadata:    req.GetMetadata(),
		MaxItems:    s.quotas[req.GetNamespace()],
	}
	if req.Ttl != nil {
		opts.TTL = req.GetTtl().AsDuration()
	}
	if req.IfVersion != nil {
		v := req.GetIfVersion()
		opts.IfVersion = &v
	}
	rec, err := d.Put(ctx, ns, req.GetKey(), opts)
	if errors.Is(err, ErrVersionMismatch) {
		return nil, status.Error(codes.FailedPrecondition, "version mismatch")
	}
	if errors.Is(err, ErrQuotaExceeded) {
		return nil, status.Errorf(codes.ResourceExhausted, "namespace %q item quota exceeded", req.GetNamespace())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "put: %v", err)
	}
	return &kvv1.PutResponse{
		Version:   rec.Version,
		CreatedAt: tsOrNil(rec.CreatedAt),
		ExpiresAt: tsOrNil(rec.ExpiresAt),
	}, nil
}

func (s *Service) Delete(ctx context.Context, req *kvv1.DeleteRequest) (*kvv1.DeleteResponse, error) {
	if req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "key required")
	}
	d, ns, err := s.namespaceDriver(ctx, req.GetNamespace(), auth.OpKVDelete)
	if err != nil {
		return nil, err
	}
	opts := DeleteOptions{}
	if req.IfVersion != nil {
		v := req.GetIfVersion()
		opts.IfVersion = &v
	}
	existed, err := d.Delete(ctx, ns, req.GetKey(), opts)
	if errors.Is(err, ErrVersionMismatch) {
		return nil, status.Error(codes.FailedPrecondition, "version mismatch")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &kvv1.DeleteResponse{Existed: existed}, nil
}

func (s *Service) Scan(req *kvv1.ScanRequest, stream kvv1.KV_ScanServer) error {
	ctx := stream.Context()
	d, ns, err := s.namespaceDriver(ctx, req.GetNamespace(), auth.OpKVScan)
	if err != nil {
		return err
	}
	opts := ScanOptions{
		KeyPrefix:     req.GetKeyPrefix(),
		Limit:         req.GetLimit(),
		IncludeValues: req.GetIncludeValues(),
		StartAfter:    req.GetStartAfter(),
	}
	err = d.Scan(ctx, ns, opts, func(r Record) error {
		return stream.Send(recordToItem(r))
	})
	if err != nil {
		return status.Errorf(codes.Internal, "scan: %v", err)
	}
	return nil
}

func recordToGetResponse(r Record) *kvv1.GetResponse {
	return &kvv1.GetResponse{
		Found:       true,
		Value:       r.Value,
		ContentType: r.ContentType,
		CreatedAt:   tsOrNil(r.CreatedAt),
		ExpiresAt:   tsOrNil(r.ExpiresAt),
		Version:     r.Version,
		Metadata:    r.Metadata,
	}
}

func recordToItem(r Record) *kvv1.KVItem {
	return &kvv1.KVItem{
		Key:         r.Key,
		Value:       r.Value,
		CreatedAt:   tsOrNil(r.CreatedAt),
		ExpiresAt:   tsOrNil(r.ExpiresAt),
		Version:     r.Version,
		Metadata:    r.Metadata,
		ContentType: r.ContentType,
	}
}

func tsOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
