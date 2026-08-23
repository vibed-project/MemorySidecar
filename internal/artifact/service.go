package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/vibed-project/mindD/internal/auth"

	artifactv1 "github.com/vibed-project/mindD/gen/mindd/artifact/v1"
)

const Block = "artifact"

// chunkBytes is the per-message payload size used when streaming Get responses.
const chunkBytes = 64 << 10 // 64 KiB

// Service implements the generated ArtifactServer.
type Service struct {
	artifactv1.UnimplementedArtifactServer
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

// Put consumes the client stream. The first message must carry PutInit; all
// later messages must carry PutChunk. The service hashes bytes and counts
// them as they flow through to the driver, then writes the final sha256+size
// back into the meta.
func (s *Service) Put(stream artifactv1.Artifact_PutServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "expected PutInit: %v", err)
	}
	init := first.GetInit()
	if init == nil {
		return status.Error(codes.InvalidArgument, "first message must be PutInit")
	}
	d, ns, err := s.driverFor(ctx, init.GetNamespace(), auth.OpArtifactPut)
	if err != nil {
		return err
	}

	// Stream the rest of the inbound chunks through an io.Pipe, with a
	// TeeReader feeding a sha256 + byte-counter as the driver pulls bytes.
	pr, pw := io.Pipe()
	streamErrCh := make(chan error, 1)
	go func() {
		defer func() { _ = pw.Close() }()
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				streamErrCh <- nil
				return
			}
			if err != nil {
				_ = pw.CloseWithError(err)
				streamErrCh <- err
				return
			}
			chunk := msg.GetChunk()
			if chunk == nil {
				err := status.Error(codes.InvalidArgument, "non-first message must be PutChunk")
				_ = pw.CloseWithError(err)
				streamErrCh <- err
				return
			}
			if _, err := pw.Write(chunk.GetData()); err != nil {
				streamErrCh <- err
				return
			}
		}
	}()

	hasher := sha256.New()
	counter := &countingReader{R: io.TeeReader(pr, hasher)}

	// The driver computes nothing about the bytes itself; it just stores
	// what we hand it. We patch size+sha256 into the returned meta after.
	meta, err := d.Put(ctx, ns, PutHeader{
		ID:          init.GetId(),
		ContentType: init.GetContentType(),
		Metadata:    init.GetMetadata(),
		// Size and SHA256 unknown at the driver's call site; the FS/memory
		// drivers persist them blindly so we hand back the post-hash values.
	}, counter)
	if err != nil {
		return status.Errorf(codes.Internal, "put: %v", err)
	}
	if recvErr := <-streamErrCh; recvErr != nil && !errors.Is(recvErr, io.EOF) {
		return status.Errorf(codes.Internal, "recv: %v", recvErr)
	}

	meta.Size = counter.N
	meta.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	// Best-effort patch: writeback for FS driver if it supports it. Without
	// a separate WriteMeta interface this is a known limitation: the meta
	// JSON on disk has empty SHA256/Size until the next Stat is overlaid by
	// the service. To keep the walking-slice simple, we re-Stat then patch.
	if statter, ok := d.(metaPatcher); ok {
		_ = statter.PatchMeta(ctx, ns, meta.ID, meta.SHA256, meta.Size)
	}

	return stream.SendAndClose(&artifactv1.PutResponse{
		Ref: &artifactv1.ArtifactRef{
			Id:          meta.ID,
			Size:        meta.Size,
			ContentType: meta.ContentType,
			Sha256:      meta.SHA256,
			CreatedAt:   timestamppb.New(meta.CreatedAt),
		},
	})
}

// metaPatcher is an optional driver-level hook that lets the service
// retroactively write sha256+size into stored metadata once the stream
// completes. Drivers that don't implement it simply lose those fields.
type metaPatcher interface {
	PatchMeta(ctx context.Context, namespace, id, sha256 string, size uint64) error
}

func (s *Service) Get(req *artifactv1.GetRequest, stream artifactv1.Artifact_GetServer) error {
	ctx := stream.Context()
	if req.GetId() == "" {
		return status.Error(codes.InvalidArgument, "id required")
	}
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpArtifactGet)
	if err != nil {
		return err
	}

	if err := ValidateID(req.GetId()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	meta, rc, err := d.Open(ctx, ns, req.GetId(), GetOptions{
		Offset: req.GetOffset(),
		Length: req.GetLength(),
	})
	if errors.Is(err, ErrNotFound) {
		return status.Errorf(codes.NotFound, "artifact %q not found", req.GetId())
	}
	if err != nil {
		return status.Errorf(codes.Internal, "open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if err := stream.Send(&artifactv1.GetResponse{
		Body: &artifactv1.GetResponse_Header{Header: &artifactv1.GetHeader{Meta: metaToProto(meta)}},
	}); err != nil {
		return err
	}

	buf := make([]byte, chunkBytes)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&artifactv1.GetResponse{
				Body: &artifactv1.GetResponse_Chunk{Chunk: &artifactv1.GetChunk{Data: buf[:n]}},
			}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Internal, "read: %v", err)
		}
	}
}

func (s *Service) Stat(ctx context.Context, req *artifactv1.StatRequest) (*artifactv1.StatResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpArtifactStat)
	if err != nil {
		return nil, err
	}
	if err := ValidateID(req.GetId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	meta, err := d.Stat(ctx, ns, req.GetId())
	if errors.Is(err, ErrNotFound) {
		return &artifactv1.StatResponse{Found: false}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "stat: %v", err)
	}
	return &artifactv1.StatResponse{Found: true, Meta: metaToProto(meta)}, nil
}

func (s *Service) Delete(ctx context.Context, req *artifactv1.DeleteRequest) (*artifactv1.DeleteResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpArtifactDelete)
	if err != nil {
		return nil, err
	}
	if err := ValidateID(req.GetId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	existed, err := d.Delete(ctx, ns, req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &artifactv1.DeleteResponse{Existed: existed}, nil
}

func (s *Service) List(req *artifactv1.ListRequest, stream artifactv1.Artifact_ListServer) error {
	ctx := stream.Context()
	d, ns, err := s.driverFor(ctx, req.GetNamespace(), auth.OpArtifactList)
	if err != nil {
		return err
	}
	err = d.List(ctx, ns, ListOptions{
		Filter:     req.GetFilter(),
		StartAfter: req.GetStartAfter(),
		Limit:      req.GetLimit(),
	}, func(m Meta) error {
		return stream.Send(metaToProto(m))
	})
	if err != nil {
		return status.Errorf(codes.Internal, "list: %v", err)
	}
	return nil
}

func metaToProto(m Meta) *artifactv1.ArtifactMeta {
	out := &artifactv1.ArtifactMeta{
		Id:          m.ID,
		Size:        m.Size,
		ContentType: m.ContentType,
		Sha256:      m.SHA256,
		Metadata:    m.Metadata,
	}
	if !m.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(m.CreatedAt)
	}
	return out
}

type countingReader struct {
	R io.Reader
	N uint64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.R.Read(p)
	c.N += uint64(n)
	return n, err
}
