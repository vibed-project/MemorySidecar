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
	"/memsidecar.kv.v1.KV/MultiGet":           {auth.OpKVGet, "kv", false},
	"/memsidecar.kv.v1.KV/Put":                {auth.OpKVPut, "kv", true},
	"/memsidecar.kv.v1.KV/Delete":             {auth.OpKVDelete, "kv", true},
	"/memsidecar.kv.v1.KV/Scan":               {auth.OpKVScan, "kv", false},
	"/memsidecar.episodic.v1.Episodic/Append": {auth.OpEpisodicAppend, "episodic", true},
	"/memsidecar.episodic.v1.Episodic/Range":  {auth.OpEpisodicRange, "episodic", false},
	"/memsidecar.episodic.v1.Episodic/Tail":   {auth.OpEpisodicTail, "episodic", false},
	"/memsidecar.episodic.v1.Episodic/Expire": {auth.OpEpisodicExpire, "episodic", true},
	"/memsidecar.semantic.v1.Semantic/Upsert": {auth.OpSemanticUpsert, "semantic", true},
	"/memsidecar.semantic.v1.Semantic/Search": {auth.OpSemanticSearch, "semantic", false},
	"/memsidecar.semantic.v1.Semantic/Delete": {auth.OpSemanticDelete, "semantic", true},
	"/memsidecar.semantic.v1.Semantic/Expire": {auth.OpSemanticExpire, "semantic", true},
	"/memsidecar.artifact.v1.Artifact/Put":    {auth.OpArtifactPut, "artifact", true},
	"/memsidecar.artifact.v1.Artifact/Get":    {auth.OpArtifactGet, "artifact", false},
	"/memsidecar.artifact.v1.Artifact/Stat":   {auth.OpArtifactStat, "artifact", false},
	"/memsidecar.artifact.v1.Artifact/Delete": {auth.OpArtifactDelete, "artifact", true},
	"/memsidecar.artifact.v1.Artifact/List":   {auth.OpArtifactList, "artifact", false},
	"/memsidecar.lease.v1.Lease/Acquire":      {auth.OpLeaseAcquire, "lease", true},
	"/memsidecar.lease.v1.Lease/Renew":        {auth.OpLeaseRenew, "lease", true},
	"/memsidecar.lease.v1.Lease/Release":      {auth.OpLeaseRelease, "lease", true},
	"/memsidecar.lease.v1.Lease/Inspect":      {auth.OpLeaseInspect, "lease", false},
	"/memsidecar.lease.v1.Lease/List":         {auth.OpLeaseList, "lease", false},
	"/memsidecar.graph.v1.Graph/UpsertNodes":  {auth.OpGraphUpsert, "graph", true},
	"/memsidecar.graph.v1.Graph/UpsertEdges":  {auth.OpGraphUpsert, "graph", true},
	"/memsidecar.graph.v1.Graph/GetNode":      {auth.OpGraphGet, "graph", false},
	"/memsidecar.graph.v1.Graph/Neighbors":    {auth.OpGraphQuery, "graph", false},
	"/memsidecar.graph.v1.Graph/Traverse":     {auth.OpGraphQuery, "graph", false},
	"/memsidecar.graph.v1.Graph/DeleteNode":   {auth.OpGraphDelete, "graph", true},
	"/memsidecar.graph.v1.Graph/DeleteEdge":   {auth.OpGraphDelete, "graph", true},
	"/memsidecar.admin.v1.Admin/ListNamespaces": {auth.OpAdminInspect, "admin", false},
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
		cost := costFromRequest(req)
		hook := policy.HookCtx{
			Capability:       cap,
			Block:            mapping.block,
			Op:               mapping.op,
			Namespace:        namespaceFromRequest(req),
			TopK:             cost.topK,
			Limit:            cost.limit,
			Depth:            cost.depth,
			FanOut:           cost.fanOut,
			RerankCandidateK: cost.rerankK,
		}
		dec := decide(ctx, eng, mapping.write, hook)
		if !dec.Allow {
			return nil, status.Errorf(policyCode(dec), "policy: %s", dec.Reason)
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
			return status.Errorf(policyCode(dec), "policy: %s", dec.Reason)
		}
		return handler(srv, ss)
	}
}

// policyCode maps a rejected decision to a gRPC status code: resource/cost
// limits (rate limit, cap) surface as ResourceExhausted, everything else
// (deny, default deny) as PermissionDenied.
func policyCode(dec policy.Decision) codes.Code {
	if dec.Exhausted {
		return codes.ResourceExhausted
	}
	return codes.PermissionDenied
}

type requestCost struct {
	topK, limit, depth, fanOut, rerankK uint32
}

// costFromRequest extracts request-magnitude fields for cap rules from request
// types that expose them. Missing getters yield zero (no bound applies).
func costFromRequest(req any) requestCost {
	var c requestCost
	if r, ok := req.(interface{ GetTopK() uint32 }); ok {
		c.topK = r.GetTopK()
	}
	if r, ok := req.(interface{ GetLimit() uint32 }); ok {
		c.limit = r.GetLimit()
	}
	if r, ok := req.(interface{ GetDepth() uint32 }); ok {
		c.depth = r.GetDepth()
	}
	if r, ok := req.(interface{ GetFanOut() uint32 }); ok {
		c.fanOut = r.GetFanOut()
	}
	if r, ok := req.(interface{ GetRerankCandidateK() uint32 }); ok {
		c.rerankK = r.GetRerankCandidateK()
	}
	return c
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
