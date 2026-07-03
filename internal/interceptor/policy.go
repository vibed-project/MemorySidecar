package interceptor

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"memsidecar/internal/auth"
	"memsidecar/internal/policy"
)

// methodToOp resolves a fully-qualified gRPC method name to a building-block op
// for policy hooks. Methods not listed here bypass policy.
var methodToOp = map[string]struct {
	op    auth.Op
	block string
	write bool
}{
	"/memsidecar.kv.v1.KV/Get":                {auth.OpKVGet, "kv", false},
	"/memsidecar.kv.v1.KV/Put":                {auth.OpKVPut, "kv", true},
	"/memsidecar.kv.v1.KV/Delete":             {auth.OpKVDelete, "kv", true},
	"/memsidecar.kv.v1.KV/Scan":               {auth.OpKVScan, "kv", false},
	"/memsidecar.episodic.v1.Episodic/Append": {auth.OpEpisodicAppend, "episodic", true},
	"/memsidecar.episodic.v1.Episodic/Range":  {auth.OpEpisodicRange, "episodic", false},
	"/memsidecar.episodic.v1.Episodic/Tail":   {auth.OpEpisodicTail, "episodic", false},
	"/memsidecar.semantic.v1.Semantic/Upsert": {auth.OpSemanticUpsert, "semantic", true},
	"/memsidecar.semantic.v1.Semantic/Search": {auth.OpSemanticSearch, "semantic", false},
	"/memsidecar.semantic.v1.Semantic/Delete": {auth.OpSemanticDelete, "semantic", true},
	"/memsidecar.semantic.v1.Semantic/Expire": {auth.OpSemanticExpire, "semantic", true},
	"/memsidecar.artifact.v1.Artifact/Put":    {auth.OpArtifactPut, "artifact", true},
	"/memsidecar.artifact.v1.Artifact/Get":    {auth.OpArtifactGet, "artifact", false},
	"/memsidecar.artifact.v1.Artifact/Stat":   {auth.OpArtifactStat, "artifact", false},
	"/memsidecar.artifact.v1.Artifact/Delete": {auth.OpArtifactDelete, "artifact", true},
	"/memsidecar.lease.v1.Lease/Acquire":      {auth.OpLeaseAcquire, "lease", true},
	"/memsidecar.lease.v1.Lease/Renew":        {auth.OpLeaseRenew, "lease", true},
	"/memsidecar.lease.v1.Lease/Release":      {auth.OpLeaseRelease, "lease", true},
	"/memsidecar.lease.v1.Lease/Inspect":      {auth.OpLeaseInspect, "lease", false},
}

// PolicyUnary invokes the configured policy engine for every recognized method.
// Without a recognized method (e.g. health probes) it passes through.
func PolicyUnary(eng policy.Engine) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		mapping, ok := methodToOp[info.FullMethod]
		if !ok {
			return handler(ctx, req)
		}
		cap, _ := auth.FromContext(ctx)
		hook := policy.HookCtx{
			Capability: cap,
			Block:      mapping.block,
			Op:         mapping.op,
			Namespace:  namespaceFromRequest(req),
		}
		dec := decide(ctx, eng, mapping.write, hook)
		if !dec.Allow {
			return nil, status.Errorf(codes.PermissionDenied, "policy denied: %s", dec.Reason)
		}
		return handler(ctx, req)
	}
}

// PolicyStream is the streaming-call equivalent of PolicyUnary.
func PolicyStream(eng policy.Engine) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		mapping, ok := methodToOp[info.FullMethod]
		if !ok {
			return handler(srv, ss)
		}
		cap, _ := auth.FromContext(ss.Context())
		hook := policy.HookCtx{Capability: cap, Block: mapping.block, Op: mapping.op}
		dec := decide(ss.Context(), eng, mapping.write, hook)
		if !dec.Allow {
			return status.Errorf(codes.PermissionDenied, "policy denied: %s", dec.Reason)
		}
		return handler(srv, ss)
	}
}

func decide(ctx context.Context, eng policy.Engine, write bool, hook policy.HookCtx) policy.Decision {
	if write {
		return eng.PreWrite(ctx, hook)
	}
	return eng.PreRead(ctx, hook)
}

// namespaceFromRequest extracts the namespace field from request types that
// expose it. Returns "" for types that don't.
func namespaceFromRequest(req any) string {
	type nsGetter interface{ GetNamespace() string }
	if r, ok := req.(nsGetter); ok {
		return r.GetNamespace()
	}
	return ""
}
