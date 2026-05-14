package interceptor

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"memsidecar/internal/auth"
)

// CapabilityHeader is the gRPC metadata key carrying the bearer token.
const CapabilityHeader = "x-memsidecar-capability"

// AuthUnary builds a unary interceptor that verifies the bearer token in the
// capability header and attaches the parsed Capability to the request context.
func AuthUnary(v auth.TokenVerifier) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		newCtx, err := authenticate(ctx, v, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(newCtx, req)
	}
}

// AuthStream is the streaming-call equivalent of AuthUnary.
func AuthStream(v auth.TokenVerifier) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		newCtx, err := authenticate(ss.Context(), v, info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: newCtx})
	}
}

func authenticate(ctx context.Context, v auth.TokenVerifier, method string) (context.Context, error) {
	if isPublicMethod(method) {
		return ctx, nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get(CapabilityHeader)
	if len(values) == 0 {
		return nil, status.Errorf(codes.Unauthenticated, "missing %s header", CapabilityHeader)
	}
	tok := strings.TrimSpace(values[0])
	if strings.HasPrefix(strings.ToLower(tok), "bearer ") {
		tok = strings.TrimSpace(tok[7:])
	}
	if tok == "" {
		return nil, status.Error(codes.Unauthenticated, "empty bearer token")
	}
	cap, err := v.Verify(ctx, tok)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid token: %v", err)
	}
	if cap.Expired(time.Now()) {
		return nil, status.Error(codes.Unauthenticated, "token expired")
	}
	return auth.WithCapability(ctx, cap), nil
}

func isPublicMethod(fullMethod string) bool {
	switch {
	case strings.HasPrefix(fullMethod, "/grpc.health."):
		return true
	case strings.HasPrefix(fullMethod, "/grpc.reflection."):
		return true
	}
	return false
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }
