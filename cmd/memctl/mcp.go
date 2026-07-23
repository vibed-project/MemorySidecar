package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"

	"memsidecar/internal/interceptor"
	"memsidecar/internal/version"

	episodicv1 "memsidecar/gen/memsidecar/episodic/v1"
	graphv1 "memsidecar/gen/memsidecar/graph/v1"
	kvv1 "memsidecar/gen/memsidecar/kv/v1"
	semanticv1 "memsidecar/gen/memsidecar/semantic/v1"
)

// cmdMCP runs an MCP (Model Context Protocol) server over stdio that exposes a
// curated memory-tool set to LLM hosts (Claude Desktop, Cursor, …). It's pure
// protocol translation: every tool proxies to the memsidecar gRPC API under a
// single capability token, so the capability check + policy engine run
// server-side unchanged. The LLM's judgement stays client-side; the sidecar
// still just does CRUD.
func cmdMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	addr := fs.String("addr", mcpEnvOr("MEMSIDECAR_ADDR", "127.0.0.1:7777"), "server address host:port")
	token := fs.String("token", os.Getenv("MEMSIDECAR_TOKEN"), "capability token (defaults to $MEMSIDECAR_TOKEN)")
	useTLS := fs.Bool("tls", false, "dial the server over TLS")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "mcp: no token: pass --token or set $MEMSIDECAR_TOKEN")
		return 2
	}

	conn, err := mcpDial(*addr, *token, *useTLS)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp: dial:", err)
		return 1
	}
	defer func() { _ = conn.Close() }()

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "memsidecar",
		Title:   "memsidecar memory",
		Version: version.Version,
	}, nil)
	registerMemoryTools(server, conn)

	// Run over stdio; blocks until the host disconnects (EOF) or the process is
	// signalled. stdout is the JSON-RPC channel, so all diagnostics go to stderr.
	ctx, stop := signalContext()
	defer stop()
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "mcp: server:", err)
		return 1
	}
	return 0
}

func mcpDial(addr, token string, useTLS bool) (*grpc.ClientConn, error) {
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	inject := func(ctx context.Context) context.Context {
		return metadata.AppendToOutgoingContext(ctx, interceptor.CapabilityHeader, "Bearer "+token)
	}
	return grpc.NewClient(addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(inject(ctx), method, req, reply, cc, opts...)
		}),
		grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(inject(ctx), desc, cc, method, opts...)
		}),
	)
}

func mcpEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// signalContext cancels on Ctrl-C / SIGTERM so the stdio server shuts down
// cleanly when the host goes away.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func durationSeconds(n int) *durationpb.Duration {
	return durationpb.New(time.Duration(n) * time.Second)
}

// callCtx bounds a single tool's proxied gRPC call.
func callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Second)
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return textResult(string(b)), nil
}

// ---------------------------------------------------------------------------
// Curated tool set. Binary artifacts are intentionally omitted — blobs don't
// fit the LLM-tool model; use the SDK/gRPC for the artifact block.

func registerMemoryTools(s *mcp.Server, conn *grpc.ClientConn) {
	kv := kvv1.NewKVClient(conn)
	sem := semanticv1.NewSemanticClient(conn)
	ep := episodicv1.NewEpisodicClient(conn)
	gr := graphv1.NewGraphClient(conn)

	type kvGetIn struct {
		Namespace string `json:"namespace" jsonschema:"the kv namespace"`
		Key       string `json:"key" jsonschema:"the key to fetch"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "kv_get", Description: "Fetch a value from the key-value store."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in kvGetIn) (*mcp.CallToolResult, any, error) {
			c, cancel := callCtx()
			defer cancel()
			resp, err := kv.Get(c, &kvv1.GetRequest{Namespace: in.Namespace, Key: in.Key})
			if err != nil {
				return nil, nil, err
			}
			if !resp.GetFound() {
				return textResult("(not found)"), nil, nil
			}
			return textResult(string(resp.GetValue())), nil, nil
		})

	type kvPutIn struct {
		Namespace  string `json:"namespace" jsonschema:"the kv namespace"`
		Key        string `json:"key" jsonschema:"the key to write"`
		Value      string `json:"value" jsonschema:"the value to store"`
		TTLSeconds int    `json:"ttl_seconds,omitempty" jsonschema:"optional time-to-live in seconds (0 = no expiry)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "kv_put", Description: "Store a value in the key-value store, optionally with a TTL."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in kvPutIn) (*mcp.CallToolResult, any, error) {
			c, cancel := callCtx()
			defer cancel()
			req := &kvv1.PutRequest{Namespace: in.Namespace, Key: in.Key, Value: []byte(in.Value)}
			if in.TTLSeconds > 0 {
				req.Ttl = durationSeconds(in.TTLSeconds)
			}
			resp, err := kv.Put(c, req)
			if err != nil {
				return nil, nil, err
			}
			return textResult(fmt.Sprintf("ok (version %d)", resp.GetVersion())), nil, nil
		})

	type semSearchIn struct {
		Namespace string `json:"namespace" jsonschema:"the semantic namespace"`
		Query     string `json:"query" jsonschema:"natural-language query text"`
		TopK      int    `json:"top_k,omitempty" jsonschema:"number of hits to return (default 10)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "semantic_search", Description: "Semantic (vector) search over stored records; returns matching ids and scores."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in semSearchIn) (*mcp.CallToolResult, any, error) {
			c, cancel := callCtx()
			defer cancel()
			resp, err := sem.Search(c, &semanticv1.SearchRequest{
				Namespace: in.Namespace, QueryText: in.Query, TopK: uint32(in.TopK), IncludePayload: true,
			})
			if err != nil {
				return nil, nil, err
			}
			type hit struct {
				ID      string  `json:"id"`
				Score   float32 `json:"score"`
				Content string  `json:"content,omitempty"`
			}
			hits := make([]hit, 0, len(resp.GetHits()))
			for _, hpb := range resp.GetHits() {
				hits = append(hits, hit{ID: hpb.GetRecord().GetId(), Score: hpb.GetScore(), Content: hpb.GetRecord().GetContent()})
			}
			return mustJSON(hits), nil, nil
		})

	type semUpsertIn struct {
		Namespace string `json:"namespace" jsonschema:"the semantic namespace"`
		ID        string `json:"id,omitempty" jsonschema:"optional record id (server assigns one if empty)"`
		Content   string `json:"content" jsonschema:"the text to embed and store"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "semantic_upsert", Description: "Store (embed) a text record for later semantic recall."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in semUpsertIn) (*mcp.CallToolResult, any, error) {
			c, cancel := callCtx()
			defer cancel()
			resp, err := sem.Upsert(c, &semanticv1.UpsertRequest{
				Namespace: in.Namespace,
				Records:   []*semanticv1.Record{{Id: in.ID, Content: in.Content}},
			})
			if err != nil {
				return nil, nil, err
			}
			ids := resp.GetIds()
			if len(ids) == 0 {
				return textResult("ok"), nil, nil
			}
			return textResult("stored id " + ids[0]), nil, nil
		})

	type epAppendIn struct {
		Namespace string `json:"namespace" jsonschema:"the episodic namespace"`
		Type      string `json:"type" jsonschema:"event type, e.g. message / tool_call / observation"`
		Payload   string `json:"payload,omitempty" jsonschema:"event body"`
		Role      string `json:"role,omitempty" jsonschema:"speaker/actor, e.g. user / assistant / tool"`
		SessionID string `json:"session_id,omitempty" jsonschema:"conversation grouping key"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "episodic_append", Description: "Append an event to the episodic log."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in epAppendIn) (*mcp.CallToolResult, any, error) {
			c, cancel := callCtx()
			defer cancel()
			resp, err := ep.Append(c, &episodicv1.AppendRequest{
				Namespace: in.Namespace, Type: in.Type, Payload: []byte(in.Payload),
				Role: in.Role, SessionId: in.SessionID,
			})
			if err != nil {
				return nil, nil, err
			}
			return textResult(fmt.Sprintf("appended cursor %d", resp.GetEvent().GetCursor())), nil, nil
		})

	type epRangeIn struct {
		Namespace   string `json:"namespace" jsonschema:"the episodic namespace"`
		AfterCursor uint64 `json:"after_cursor,omitempty" jsonschema:"exclusive lower bound; 0 = from the start"`
		Limit       int    `json:"limit,omitempty" jsonschema:"max events to return (0 = unbounded)"`
		SessionID   string `json:"session_id,omitempty" jsonschema:"filter to one conversation"`
		Role        string `json:"role,omitempty" jsonschema:"filter by speaker/actor"`
		Type        string `json:"type,omitempty" jsonschema:"filter by event type"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "episodic_range", Description: "Replay episodic events by cursor, optionally filtered by session/role/type."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in epRangeIn) (*mcp.CallToolResult, any, error) {
			c, cancel := callCtx()
			defer cancel()
			stream, err := ep.Range(c, &episodicv1.RangeRequest{
				Namespace: in.Namespace, AfterCursor: in.AfterCursor, Limit: uint32(in.Limit),
				SessionId: in.SessionID, Role: in.Role, Type: in.Type,
			})
			if err != nil {
				return nil, nil, err
			}
			type event struct {
				Cursor    uint64 `json:"cursor"`
				Type      string `json:"type"`
				Role      string `json:"role,omitempty"`
				SessionID string `json:"session_id,omitempty"`
				Payload   string `json:"payload,omitempty"`
			}
			var events []event
			for {
				ev, err := stream.Recv()
				if err != nil {
					break
				}
				events = append(events, event{
					Cursor: ev.GetCursor(), Type: ev.GetType(), Role: ev.GetRole(),
					SessionID: ev.GetSessionId(), Payload: string(ev.GetPayload()),
				})
			}
			return mustJSON(events), nil, nil
		})

	type graphNeighborsIn struct {
		Namespace string   `json:"namespace" jsonschema:"the graph namespace"`
		NodeID    string   `json:"node_id" jsonschema:"the node to expand from"`
		EdgeTypes []string `json:"edge_types,omitempty" jsonschema:"restrict to these edge types"`
		Limit     int      `json:"limit,omitempty" jsonschema:"max neighbors (0 = server default)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "graph_neighbors", Description: "One-hop neighbors of a graph node."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in graphNeighborsIn) (*mcp.CallToolResult, any, error) {
			c, cancel := callCtx()
			defer cancel()
			resp, err := gr.Neighbors(c, &graphv1.NeighborsRequest{
				Namespace: in.Namespace, NodeId: in.NodeID, EdgeTypes: in.EdgeTypes, Limit: uint32(in.Limit),
			})
			if err != nil {
				return nil, nil, err
			}
			return mustJSON(subgraphJSON(resp.GetNodes(), resp.GetEdges())), nil, nil
		})

	type graphTraverseIn struct {
		Namespace string `json:"namespace" jsonschema:"the graph namespace"`
		StartID   string `json:"start_id" jsonschema:"the node to start from"`
		Depth     int    `json:"depth,omitempty" jsonschema:"max hops (0 = server default; hard-capped server-side)"`
		MaxNodes  int    `json:"max_nodes,omitempty" jsonschema:"max nodes to visit (0 = server default)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "graph_traverse", Description: "Bounded multi-hop traversal from a graph node."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in graphTraverseIn) (*mcp.CallToolResult, any, error) {
			c, cancel := callCtx()
			defer cancel()
			resp, err := gr.Traverse(c, &graphv1.TraverseRequest{
				Namespace: in.Namespace, StartId: in.StartID, Depth: uint32(in.Depth), MaxNodes: uint32(in.MaxNodes),
			})
			if err != nil {
				return nil, nil, err
			}
			return mustJSON(subgraphJSON(resp.GetNodes(), resp.GetEdges())), nil, nil
		})
}

type nodeJSON struct {
	ID     string   `json:"id"`
	Labels []string `json:"labels,omitempty"`
}
type edgeJSON struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	From string `json:"from"`
	To   string `json:"to"`
}

func subgraphJSON(nodes []*graphv1.Node, edges []*graphv1.Edge) map[string]any {
	ns := make([]nodeJSON, 0, len(nodes))
	for _, n := range nodes {
		ns = append(ns, nodeJSON{ID: n.GetId(), Labels: n.GetLabels()})
	}
	es := make([]edgeJSON, 0, len(edges))
	for _, e := range edges {
		es = append(es, edgeJSON{ID: e.GetId(), Type: e.GetType(), From: e.GetFrom(), To: e.GetTo()})
	}
	return map[string]any{"nodes": ns, "edges": es}
}

func mustJSON(v any) *mcp.CallToolResult {
	r, err := jsonResult(v)
	if err != nil {
		return textResult(fmt.Sprintf("error marshaling result: %v", err))
	}
	return r
}
