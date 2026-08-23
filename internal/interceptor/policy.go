package interceptor

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vibed-project/mindD/internal/auth"
	"github.com/vibed-project/mindD/internal/policy"
)

// methodToOp resolves a fully-qualified gRPC method name to a building-block op
// for policy hooks. Methods not listed here bypass policy.
var methodToOp = map[string]struct {
	op    auth.Op
	block string
	write bool
}{
	"/mindd.kv.v1.KV/Get":                {auth.OpKVGet, "kv", false},
	"/mindd.kv.v1.KV/MultiGet":           {auth.OpKVGet, "kv", false},
	"/mindd.kv.v1.KV/Put":                {auth.OpKVPut, "kv", true},
	"/mindd.kv.v1.KV/Delete":             {auth.OpKVDelete, "kv", true},
	"/mindd.kv.v1.KV/Scan":               {auth.OpKVScan, "kv", false},
	"/mindd.episodic.v1.Episodic/Append": {auth.OpEpisodicAppend, "episodic", true},
	"/mindd.episodic.v1.Episodic/Range":  {auth.OpEpisodicRange, "episodic", false},
	"/mindd.episodic.v1.Episodic/Tail":   {auth.OpEpisodicTail, "episodic", false},
	"/mindd.episodic.v1.Episodic/Expire": {auth.OpEpisodicExpire, "episodic", true},
	"/mindd.semantic.v1.Semantic/Upsert": {auth.OpSemanticUpsert, "semantic", true},
	"/mindd.semantic.v1.Semantic/Search": {auth.OpSemanticSearch, "semantic", false},
	"/mindd.semantic.v1.Semantic/Delete": {auth.OpSemanticDelete, "semantic", true},
	"/mindd.semantic.v1.Semantic/Expire": {auth.OpSemanticExpire, "semantic", true},
	"/mindd.artifact.v1.Artifact/Put":    {auth.OpArtifactPut, "artifact", true},
	"/mindd.artifact.v1.Artifact/Get":    {auth.OpArtifactGet, "artifact", false},
	"/mindd.artifact.v1.Artifact/Stat":   {auth.OpArtifactStat, "artifact", false},
	"/mindd.artifact.v1.Artifact/Delete": {auth.OpArtifactDelete, "artifact", true},
	"/mindd.artifact.v1.Artifact/List":   {auth.OpArtifactList, "artifact", false},
	"/mindd.lease.v1.Lease/Acquire":      {auth.OpLeaseAcquire, "lease", true},
	"/mindd.lease.v1.Lease/Renew":        {auth.OpLeaseRenew, "lease", true},
	"/mindd.lease.v1.Lease/Release":      {auth.OpLeaseRelease, "lease", true},
	"/mindd.lease.v1.Lease/Inspect":      {auth.OpLeaseInspect, "lease", false},
	"/mindd.lease.v1.Lease/List":         {auth.OpLeaseList, "lease", false},
	"/mindd.graph.v1.Graph/UpsertNodes":  {auth.OpGraphUpsert, "graph", true},
	"/mindd.graph.v1.Graph/UpsertEdges":  {auth.OpGraphUpsert, "graph", true},
	"/mindd.graph.v1.Graph/GetNode":      {auth.OpGraphGet, "graph", false},
	"/mindd.graph.v1.Graph/Neighbors":    {auth.OpGraphQuery, "graph", false},
	"/mindd.graph.v1.Graph/Traverse":     {auth.OpGraphQuery, "graph", false},
	"/mindd.graph.v1.Graph/DeleteNode":   {auth.OpGraphDelete, "graph", true},
	"/mindd.graph.v1.Graph/DeleteEdge":   {auth.OpGraphDelete, "graph", true},
	"/mindd.admin.v1.Admin/ListNamespaces": {auth.OpAdminInspect, "admin", false},
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
//
// The namespace and the cost fields live in the stream's first message, which
// has not arrived when the interceptor is entered. This previously built a
// HookCtx with Block and Op only, leaving Namespace empty -- so a rule matching
// on namespace (e.g. `deny namespace: ["secret-*"]`) never matched a streaming
// method and fell through to the default effect. KV/Scan, Episodic/Range,
// Episodic/Tail and all three Artifact streams were unfiltered by any
// namespace-scoped rule, and every `cap` rule was inert on them.
//
// The decision is therefore deferred to the first RecvMsg, where the request
// message is available and the hook can be populated exactly as PolicyUnary
// does. It is evaluated once per RPC and not once per message: a rate_limit
// rule consumes a token from its bucket on every evaluation, so checking up
// front *and* on the first message would charge each streaming call twice.
func PolicyStream(eng policy.Engine) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		mapping, ok := methodToOp[info.FullMethod]
		if !ok {
			return handler(srv, ss)
		}

		ps := &policyStream{
			ServerStream: ss,
			check: func(msg any) error {
				cap, _ := auth.FromContext(ss.Context())
				cost := costFromRequest(msg)
				hook := policy.HookCtx{
					Capability:       cap,
					Block:            mapping.block,
					Op:               mapping.op,
					Namespace:        namespaceFromRequest(msg),
					TopK:             cost.topK,
					Limit:            cost.limit,
					Depth:            cost.depth,
					FanOut:           cost.fanOut,
					RerankCandidateK: cost.rerankK,
				}
				dec := decide(ss.Context(), eng, mapping.write, hook)
				if !dec.Allow {
					return status.Errorf(policyCode(dec), "policy: %s", dec.Reason)
				}
				return nil
			},
		}

		err := handler(srv, ps)

		// A handler that streamed successfully without ever receiving a
		// message would never have triggered the check above. No generated
		// handler for the six mapped streaming methods behaves that way -- all
		// of them receive the request before doing anything -- but a silent
		// policy bypass is not something to leave resting on that. Fail loudly
		// instead. Only when the handler otherwise succeeded, so a client that
		// disconnects before sending still reports its own error.
		if err == nil && !ps.checked {
			return status.Error(codes.Internal,
				"policy: stream completed without evaluating policy; refusing to report success")
		}
		return err
	}
}

// policyStream defers the policy decision to the first message of a stream.
//
// RecvMsg is documented as not safe for concurrent use, and gRPC calls it
// sequentially from the handler's goroutine, so `checked` needs no
// synchronization.
type policyStream struct {
	grpc.ServerStream
	check   func(msg any) error
	checked bool
}

func (s *policyStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	if !s.checked {
		s.checked = true
		if err := s.check(m); err != nil {
			return err
		}
	}
	return nil
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
