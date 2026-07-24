package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"

	"memsidecar/internal/interceptor"

	adminv1 "memsidecar/gen/memsidecar/admin/v1"
	episodicv1 "memsidecar/gen/memsidecar/episodic/v1"
	graphv1 "memsidecar/gen/memsidecar/graph/v1"
	kvv1 "memsidecar/gen/memsidecar/kv/v1"
	semanticv1 "memsidecar/gen/memsidecar/semantic/v1"
)

// connFlags adds the connection flags every data-plane verb shares and returns
// accessors evaluated after fs.Parse. Defaults come from the environment so the
// common case (`memctl kv get ns key`) needs no flags.
func connFlags(fs *flag.FlagSet) func() (*grpc.ClientConn, error) {
	addr := fs.String("addr", envOr("MEMSIDECAR_ADDR", "127.0.0.1:7777"), "server address host:port")
	token := fs.String("token", os.Getenv("MEMSIDECAR_TOKEN"), "capability token (defaults to $MEMSIDECAR_TOKEN)")
	useTLS := fs.Bool("tls", false, "dial over TLS")
	return func() (*grpc.ClientConn, error) {
		if *token == "" {
			return nil, fmt.Errorf("no token: pass --token or set $MEMSIDECAR_TOKEN")
		}
		creds := insecure.NewCredentials()
		if *useTLS {
			creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
		}
		return grpc.NewClient(*addr,
			grpc.WithTransportCredentials(creds),
			grpc.WithUnaryInterceptor(capUnary(*token)),
			grpc.WithStreamInterceptor(capStream(*token)),
		)
	}
}

func capUnary(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(withCap(ctx, token), method, req, reply, cc, opts...)
	}
}

func capStream(token string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(withCap(ctx, token), desc, cc, method, opts...)
	}
}

func withCap(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, interceptor.CapabilityHeader, "Bearer "+token)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// dispatchData routes the data-plane verbs. Returns (handled, exitcode).
func dispatchData(block string, args []string) (bool, int) {
	switch block {
	case "kv":
		return true, cmdKV(args)
	case "semantic":
		return true, cmdSemantic(args)
	case "episodic":
		return true, cmdEpisodic(args)
	case "graph":
		return true, cmdGraph(args)
	case "ns":
		return true, cmdNS(args)
	}
	return false, 0
}

func cmdKV(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: memctl kv (get|put) ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "get":
		fs := flag.NewFlagSet("kv get", flag.ContinueOnError)
		dial := connFlags(fs)
		if err := parseArgs(fs, rest); err != nil {
			return 2
		}
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "usage: memctl kv get <namespace> <key>")
			return 2
		}
		conn, err := dial()
		if err != nil {
			return fail(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := ctxWithTimeout()
		defer cancel()
		resp, err := kvv1.NewKVClient(conn).Get(ctx, &kvv1.GetRequest{Namespace: fs.Arg(0), Key: fs.Arg(1)})
		if err != nil {
			return fail(err)
		}
		if !resp.GetFound() {
			fmt.Fprintln(os.Stderr, "not found")
			return 1
		}
		_, _ = os.Stdout.Write(resp.GetValue())
		fmt.Println()
		return 0
	case "put":
		fs := flag.NewFlagSet("kv put", flag.ContinueOnError)
		dial := connFlags(fs)
		ttl := fs.Duration("ttl", 0, "time-to-live (0 = no expiry)")
		if err := parseArgs(fs, rest); err != nil {
			return 2
		}
		if fs.NArg() != 3 {
			fmt.Fprintln(os.Stderr, "usage: memctl kv put <namespace> <key> <value> [--ttl 5m]")
			return 2
		}
		conn, err := dial()
		if err != nil {
			return fail(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := ctxWithTimeout()
		defer cancel()
		req := &kvv1.PutRequest{Namespace: fs.Arg(0), Key: fs.Arg(1), Value: []byte(fs.Arg(2))}
		if *ttl > 0 {
			req.Ttl = durationpb.New(*ttl)
		}
		resp, err := kvv1.NewKVClient(conn).Put(ctx, req)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("ok version=%d\n", resp.GetVersion())
		return 0
	default:
		fmt.Fprintln(os.Stderr, "usage: memctl kv (get|put) ...")
		return 2
	}
}

func cmdSemantic(args []string) int {
	if len(args) < 1 || args[0] != "search" {
		fmt.Fprintln(os.Stderr, "usage: memctl semantic search <namespace> <query> [--top-k 10] [--filter k=v,...]")
		return 2
	}
	fs := flag.NewFlagSet("semantic search", flag.ContinueOnError)
	dial := connFlags(fs)
	topK := fs.Uint("top-k", 10, "number of hits")
	filter := fs.String("filter", "", "exact-match metadata filter, e.g. topic=food,lang=en")
	if err := parseArgs(fs, args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: memctl semantic search <namespace> <query> [--top-k 10] [--filter k=v,...]")
		return 2
	}
	conn, err := dial()
	if err != nil {
		return fail(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	resp, err := semanticv1.NewSemanticClient(conn).Search(ctx, &semanticv1.SearchRequest{
		Namespace: fs.Arg(0), QueryText: fs.Arg(1), TopK: uint32(*topK), Filter: parseKV(*filter),
	})
	if err != nil {
		return fail(err)
	}
	for _, hit := range resp.GetHits() {
		fmt.Printf("%s\t%.4f\n", hit.GetRecord().GetId(), hit.GetScore())
	}
	return 0
}

func cmdEpisodic(args []string) int {
	if len(args) < 1 || args[0] != "tail" {
		fmt.Fprintln(os.Stderr, "usage: memctl episodic tail <namespace> [--historical] [--after-cursor N]")
		return 2
	}
	fs := flag.NewFlagSet("episodic tail", flag.ContinueOnError)
	dial := connFlags(fs)
	historical := fs.Bool("historical", false, "replay history before going live")
	after := fs.Uint64("after-cursor", 0, "start after this cursor")
	if err := parseArgs(fs, args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: memctl episodic tail <namespace> [--historical] [--after-cursor N]")
		return 2
	}
	conn, err := dial()
	if err != nil {
		return fail(err)
	}
	defer func() { _ = conn.Close() }()
	// Tail is a live stream; run until interrupted.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stream, err := episodicv1.NewEpisodicClient(conn).Tail(ctx, &episodicv1.TailRequest{
		Namespace: fs.Arg(0), IncludeHistorical: *historical, AfterCursor: *after,
	})
	if err != nil {
		return fail(err)
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return 0 // clean interrupt
			}
			return fail(err)
		}
		fmt.Printf("%d\t%s\t%s\t%s\t%s\n",
			ev.GetCursor(), ev.GetTimestamp().AsTime().Format(time.RFC3339),
			ev.GetType(), ev.GetRole(), string(ev.GetPayload()))
	}
}

func cmdGraph(args []string) int {
	if len(args) < 1 || args[0] != "neighbors" {
		fmt.Fprintln(os.Stderr, "usage: memctl graph neighbors <namespace> <node-id> [--limit N] [--edge-types a,b]")
		return 2
	}
	fs := flag.NewFlagSet("graph neighbors", flag.ContinueOnError)
	dial := connFlags(fs)
	limit := fs.Uint("limit", 0, "max neighbors (0 = server default)")
	edgeTypes := fs.String("edge-types", "", "comma-separated edge types to follow")
	if err := parseArgs(fs, args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: memctl graph neighbors <namespace> <node-id> [--limit N] [--edge-types a,b]")
		return 2
	}
	conn, err := dial()
	if err != nil {
		return fail(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	resp, err := graphv1.NewGraphClient(conn).Neighbors(ctx, &graphv1.NeighborsRequest{
		Namespace: fs.Arg(0), NodeId: fs.Arg(1), Limit: uint32(*limit), EdgeTypes: splitCSV(*edgeTypes),
	})
	if err != nil {
		return fail(err)
	}
	for _, e := range resp.GetEdges() {
		fmt.Printf("edge\t%s\t%s\t%s->%s\n", e.GetId(), e.GetType(), e.GetFrom(), e.GetTo())
	}
	for _, n := range resp.GetNodes() {
		fmt.Printf("node\t%s\t%s\n", n.GetId(), strings.Join(n.GetLabels(), ","))
	}
	return 0
}

func cmdNS(args []string) int {
	if len(args) < 1 || args[0] != "ls" {
		fmt.Fprintln(os.Stderr, "usage: memctl ns ls")
		return 2
	}
	fs := flag.NewFlagSet("ns ls", flag.ContinueOnError)
	dial := connFlags(fs)
	if err := parseArgs(fs, args[1:]); err != nil {
		return 2
	}
	conn, err := dial()
	if err != nil {
		return fail(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	resp, err := adminv1.NewAdminClient(conn).ListNamespaces(ctx, &adminv1.ListNamespacesRequest{})
	if err != nil {
		return fail(err)
	}
	fmt.Printf("# memsidecar %s (%s)\n", resp.GetServer().GetVersion(), resp.GetServer().GetCommit())
	fmt.Printf("%-10s %-16s %-14s %-9s %s\n", "BLOCK", "NAME", "BACKEND", "DRIVER", "ITEMS")
	for _, ns := range resp.GetNamespaces() {
		items := "-"
		if ns.GetHasCount() {
			items = fmt.Sprintf("%d", ns.GetItemCount())
		}
		fmt.Printf("%-10s %-16s %-14s %-9s %s\n",
			ns.GetBlock(), ns.GetName(), ns.GetBackend(), ns.GetDriver(), items)
	}
	return 0
}

func ctxWithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}

// parseArgs parses a flag set while tolerating flags placed after the
// positional args (the stdlib flag package stops at the first positional). It
// moves the flag section — everything from the first "-"-prefixed token — ahead
// of the positionals, so both `kv put ns k v --ttl 5m` and `kv put --ttl 5m ns k
// v` work. fs.Args() afterwards holds just the positionals.
func parseArgs(fs *flag.FlagSet, args []string) error {
	var pos, flags []string
	seen := false
	for _, a := range args {
		if !seen && strings.HasPrefix(a, "-") {
			seen = true
		}
		if seen {
			flags = append(flags, a)
		} else {
			pos = append(pos, a)
		}
	}
	return fs.Parse(append(flags, pos...))
}

// parseKV parses "k=v,k2=v2" into a map. Empty input yields nil.
func parseKV(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
