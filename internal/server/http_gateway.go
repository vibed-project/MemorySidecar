package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	artifactv1 "github.com/vibed-project/mindD/gen/mindd/artifact/v1"
	episodicv1 "github.com/vibed-project/mindD/gen/mindd/episodic/v1"
	kvv1 "github.com/vibed-project/mindD/gen/mindd/kv/v1"
	leasev1 "github.com/vibed-project/mindD/gen/mindd/lease/v1"
	semanticv1 "github.com/vibed-project/mindD/gen/mindd/semantic/v1"
	"github.com/vibed-project/mindD/internal/interceptor"
)

// NewHTTPGateway builds an http.Handler that translates JSON-over-HTTP to
// gRPC and forwards it to the loopback gRPC server at grpcAddr. The default
// URL scheme matches gRPC: POST /<package>.<service>/<method> with the JSON
// request body.
//
// The Authorization header and the capability header
// "x-mindd-capability" are forwarded as gRPC metadata so the server-side
// auth interceptor sees them.
func NewHTTPGateway(ctx context.Context, grpcAddr string) (http.Handler, error) {
	if grpcAddr == "" {
		return nil, fmt.Errorf("http gateway: grpc addr required")
	}

	mux := runtime.NewServeMux(
		runtime.WithIncomingHeaderMatcher(func(key string) (string, bool) {
			switch strings.ToLower(key) {
			case interceptor.CapabilityHeader, "authorization":
				return strings.ToLower(key), true
			}
			return runtime.DefaultHeaderMatcher(key)
		}),
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{}),
	)

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	// gRPC dial target. Bare host:port works as-is; a filesystem path is
	// rewritten to a unix:// URI.
	target := grpcAddr
	if strings.HasPrefix(grpcAddr, "/") {
		target = "unix://" + grpcAddr
	}

	for _, register := range []func(context.Context, *runtime.ServeMux, string, []grpc.DialOption) error{
		kvv1.RegisterKVHandlerFromEndpoint,
		episodicv1.RegisterEpisodicHandlerFromEndpoint,
		semanticv1.RegisterSemanticHandlerFromEndpoint,
		artifactv1.RegisterArtifactHandlerFromEndpoint,
		leasev1.RegisterLeaseHandlerFromEndpoint,
	} {
		if err := register(ctx, mux, target, dialOpts); err != nil {
			return nil, fmt.Errorf("http gateway: register: %w", err)
		}
	}

	return mux, nil
}
