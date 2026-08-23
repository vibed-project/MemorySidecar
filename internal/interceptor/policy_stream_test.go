package interceptor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/vibed-project/mindD/internal/policy"
)

// scanReq stands in for a streaming method's request message: the namespace and
// the cost field the policy engine needs both live here, in the stream's first
// message rather than in the interceptor's arguments.
type scanReq struct {
	ns    string
	limit uint32
}

func (r scanReq) GetNamespace() string { return r.ns }
func (r scanReq) GetLimit() uint32     { return r.limit }

// fakeStream delivers a single request message, the way a server-streaming
// handler receives one before producing its responses.
type fakeStream struct {
	ctx      context.Context
	msg      any
	received int
	recvErr  error
}

func (s *fakeStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeStream) SetTrailer(metadata.MD)       {}
func (s *fakeStream) Context() context.Context     { return s.ctx }
func (s *fakeStream) SendMsg(any) error            { return nil }

func (s *fakeStream) RecvMsg(m any) error {
	if s.recvErr != nil {
		return s.recvErr
	}
	s.received++
	if p, ok := m.(*scanReq); ok {
		if src, ok := s.msg.(scanReq); ok {
			*p = src
		}
	}
	return nil
}

func denyNamespaceEngine(t *testing.T) policy.Engine {
	t.Helper()
	eng, err := policy.NewRuleEngine(policy.Spec{
		Rules: []policy.Rule{{
			Name:   "block-secret-namespaces",
			Effect: policy.EffectDeny,
			Match:  policy.Match{Namespaces: []string{"secret-*"}},
		}},
	})
	require.NoError(t, err)
	return eng
}

// A namespace-scoped deny rule must apply to streaming methods. It did not:
// PolicyStream built a HookCtx with an empty Namespace, so "secret-*" never
// matched and the call fell through to the default effect. The shipped example
// config relies on exactly this rule shape.
func TestPolicyStream_DenyRuleAppliesToStreamingMethods(t *testing.T) {
	streamingMethods := []string{
		"/mindd.kv.v1.KV/Scan",
		"/mindd.episodic.v1.Episodic/Range",
		"/mindd.episodic.v1.Episodic/Tail",
		"/mindd.artifact.v1.Artifact/Get",
		"/mindd.artifact.v1.Artifact/List",
		"/mindd.artifact.v1.Artifact/Put",
	}

	for _, method := range streamingMethods {
		t.Run(method, func(t *testing.T) {
			intercept := PolicyStream(denyNamespaceEngine(t))
			ss := &fakeStream{ctx: context.Background(), msg: scanReq{ns: "secret-vault"}}

			err := intercept(nil, ss, &grpc.StreamServerInfo{FullMethod: method},
				func(_ any, stream grpc.ServerStream) error {
					// Every generated streaming handler receives the request
					// before doing anything else.
					var req scanReq
					return stream.RecvMsg(&req)
				})

			require.Error(t, err, "a denied namespace must not reach the handler's work")
			assert.Equal(t, codes.PermissionDenied, status.Code(err))
		})
	}
}

func TestPolicyStream_AllowedNamespacePassesThrough(t *testing.T) {
	intercept := PolicyStream(denyNamespaceEngine(t))
	ss := &fakeStream{ctx: context.Background(), msg: scanReq{ns: "notes"}}

	handlerRan := false
	err := intercept(nil, ss, &grpc.StreamServerInfo{FullMethod: "/mindd.kv.v1.KV/Scan"},
		func(_ any, stream grpc.ServerStream) error {
			var req scanReq
			if err := stream.RecvMsg(&req); err != nil {
				return err
			}
			handlerRan = true
			return nil
		})

	require.NoError(t, err)
	assert.True(t, handlerRan, "an allowed namespace must reach the handler")
}

// cap rules were inert on streaming methods for the same reason: the cost
// fields were never extracted from the request message.
func TestPolicyStream_CapAppliesToStreamingMethods(t *testing.T) {
	eng, err := policy.NewRuleEngine(policy.Spec{
		Rules: []policy.Rule{{
			Name:   "scan-limit-cap",
			Effect: policy.EffectCap,
			Match:  policy.Match{Ops: []string{"scan"}},
			Max:    policy.Cap{Limit: 100},
		}},
	})
	require.NoError(t, err)

	call := func(limit uint32) error {
		intercept := PolicyStream(eng)
		ss := &fakeStream{ctx: context.Background(), msg: scanReq{ns: "notes", limit: limit}}
		return intercept(nil, ss, &grpc.StreamServerInfo{FullMethod: "/mindd.kv.v1.KV/Scan"},
			func(_ any, stream grpc.ServerStream) error {
				var req scanReq
				return stream.RecvMsg(&req)
			})
	}

	require.NoError(t, call(50), "within the cap must pass")

	err = call(5000)
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
}

// A rate_limit rule consumes a token per evaluation, so the decision must be
// made exactly once per RPC no matter how many messages the stream carries.
func TestPolicyStream_EvaluatesOncePerRPC(t *testing.T) {
	eng, err := policy.NewRuleEngine(policy.Spec{
		Rules: []policy.Rule{{
			Name:   "one-per-rpc",
			Effect: policy.EffectRateLimit,
			Match:  policy.Match{Ops: []string{"put"}},
			Bucket: policy.Bucket{RatePerSecond: 1, Burst: 1},
		}},
	})
	require.NoError(t, err)

	intercept := PolicyStream(eng)
	ss := &fakeStream{ctx: context.Background(), msg: scanReq{ns: "blobs"}}

	// A client-streaming Put receives many messages within one RPC. If the
	// check ran per message, the second Recv would exhaust the burst of 1.
	err = intercept(nil, ss, &grpc.StreamServerInfo{FullMethod: "/mindd.artifact.v1.Artifact/Put"},
		func(_ any, stream grpc.ServerStream) error {
			for i := 0; i < 5; i++ {
				var req scanReq
				if err := stream.RecvMsg(&req); err != nil {
					return err
				}
			}
			return nil
		})

	require.NoError(t, err, "policy must be evaluated once per RPC, not once per message")
	assert.Equal(t, 5, ss.received)
}

// A handler that reports success without ever receiving a message would never
// have triggered the deferred check. That must not read as an allow.
func TestPolicyStream_HandlerThatNeverReceivesFailsClosed(t *testing.T) {
	intercept := PolicyStream(denyNamespaceEngine(t))
	ss := &fakeStream{ctx: context.Background(), msg: scanReq{ns: "secret-vault"}}

	err := intercept(nil, ss, &grpc.StreamServerInfo{FullMethod: "/mindd.kv.v1.KV/Scan"},
		func(_ any, _ grpc.ServerStream) error { return nil })

	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

// A client that disconnects before sending anything should surface its own
// error, not the never-evaluated guard.
func TestPolicyStream_RecvErrorIsNotMasked(t *testing.T) {
	intercept := PolicyStream(denyNamespaceEngine(t))
	sentinel := status.Error(codes.Canceled, "client went away")
	ss := &fakeStream{ctx: context.Background(), recvErr: sentinel}

	err := intercept(nil, ss, &grpc.StreamServerInfo{FullMethod: "/mindd.kv.v1.KV/Scan"},
		func(_ any, stream grpc.ServerStream) error {
			var req scanReq
			return stream.RecvMsg(&req)
		})

	require.Error(t, err)
	assert.Equal(t, codes.Canceled, status.Code(err))
}

// Unmapped methods (health, reflection) still pass through untouched.
func TestPolicyStream_UnmappedMethodPassesThrough(t *testing.T) {
	intercept := PolicyStream(denyNamespaceEngine(t))
	ss := &fakeStream{ctx: context.Background(), msg: scanReq{ns: "secret-vault"}}

	err := intercept(nil, ss, &grpc.StreamServerInfo{FullMethod: "/grpc.health.v1.Health/Watch"},
		func(_ any, _ grpc.ServerStream) error { return nil })

	require.NoError(t, err)
}
