// Package admin implements the cross-namespace introspection service: what
// namespaces exist, how they're configured, and their live item counts. It
// reads the loaded config for the topology and each block registry's cheap
// per-namespace count for stats — no driver-specific work of its own.
package admin

import (
	"context"
	"sort"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminv1 "memsidecar/gen/memsidecar/admin/v1"
	"memsidecar/internal/auth"
	"memsidecar/internal/config"
	"memsidecar/internal/obs"
)

const Block = "admin"

// Service implements the generated AdminServer.
type Service struct {
	adminv1.UnimplementedAdminServer
	cfg     *config.Config
	counts  map[string]func(context.Context) map[string]int64 // block -> items fn
	version string
	commit  string
}

// NewService builds the Admin service. sources are the same per-block
// namespace-count functions the metrics gauge uses (reused here for stats).
func NewService(cfg *config.Config, sources []obs.NamespaceItemSource, version, commit string) *Service {
	counts := make(map[string]func(context.Context) map[string]int64, len(sources))
	for _, s := range sources {
		counts[s.Block] = s.Items
	}
	return &Service{cfg: cfg, counts: counts, version: version, commit: commit}
}

func (s *Service) ListNamespaces(ctx context.Context, _ *adminv1.ListNamespacesRequest) (*adminv1.ListNamespacesResponse, error) {
	cap, ok := auth.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing capability")
	}
	// Admin is cross-namespace: gate on the op only, not a namespace scope.
	if !cap.PermitsOp(auth.OpAdminInspect) {
		return nil, status.Errorf(codes.PermissionDenied, "op %s not in capability scope", auth.OpAdminInspect)
	}

	driverByBackend := make(map[string]string, len(s.cfg.Backends))
	for _, b := range s.cfg.Backends {
		driverByBackend[b.Name] = b.Driver
	}

	// Snapshot each block's counts once (the funcs are cheap: map length or a
	// reltuples estimate) rather than per-namespace.
	countsByBlock := make(map[string]map[string]int64, len(s.counts))
	for block, fn := range s.counts {
		if fn != nil {
			countsByBlock[block] = fn(ctx)
		}
	}

	resp := &adminv1.ListNamespacesResponse{
		Server:     &adminv1.ServerInfo{Version: s.version, Commit: s.commit},
		Namespaces: make([]*adminv1.NamespaceInfo, 0, len(s.cfg.Namespaces)),
	}
	for _, ns := range s.cfg.Namespaces {
		info := &adminv1.NamespaceInfo{
			Block:   ns.Block,
			Name:    ns.Name,
			Backend: ns.Backend,
			Driver:  driverByBackend[ns.Backend],
		}
		if counts, ok := countsByBlock[ns.Block]; ok {
			if n, ok := counts[ns.Name]; ok {
				info.ItemCount = n
				info.HasCount = true
			}
		}
		if ns.Embedder.Provider != "" {
			info.Embedder = &adminv1.EmbedderInfo{
				Provider:   ns.Embedder.Provider,
				Model:      ns.Embedder.Model,
				Dimensions: uint32(ns.Embedder.Dimensions),
			}
		}
		resp.Namespaces = append(resp.Namespaces, info)
	}
	sort.Slice(resp.Namespaces, func(i, j int) bool {
		a, b := resp.Namespaces[i], resp.Namespaces[j]
		if a.GetBlock() != b.GetBlock() {
			return a.GetBlock() < b.GetBlock()
		}
		return a.GetName() < b.GetName()
	})
	return resp, nil
}
