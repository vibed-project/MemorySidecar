package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"memsidecar/internal/auth"
	"memsidecar/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "token":
		if len(os.Args) < 3 {
			usage(os.Stderr)
			os.Exit(2)
		}
		switch os.Args[2] {
		case "issue":
			os.Exit(cmdTokenIssue(os.Args[3:]))
		case "gen-keypair":
			os.Exit(cmdGenKeypair(os.Args[3:]))
		default:
			usage(os.Stderr)
			os.Exit(2)
		}
	case "mcp":
		os.Exit(cmdMCP(os.Args[2:]))
	case "version", "-v", "--version":
		fmt.Println("memctl", version.String())
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		// Data-plane verbs (kv / semantic / episodic / graph / ns) talk to a
		// running server using the capability token memctl issues.
		if handled, code := dispatchData(os.Args[1], os.Args[2:]); handled {
			os.Exit(code)
		}
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	_, _ = fmt.Fprintf(w, `memctl — memsidecar CLI

Control plane:
  memctl token gen-keypair                Mint a PASETO v4.public keypair (hex)
  memctl token issue [flags]              Mint a dev capability token
  memctl mcp [flags]                      Run an MCP (stdio) server for LLM hosts
  memctl version                          Print version

Data plane (needs a running server + a token):
  memctl kv put <ns> <key> <value> [--ttl 5m]
  memctl kv get <ns> <key>
  memctl semantic search <ns> <query> [--top-k 10] [--filter k=v,...]
  memctl episodic tail <ns> [--historical] [--after-cursor N]
  memctl graph neighbors <ns> <node-id> [--limit N] [--edge-types a,b]
  memctl ns ls

Data-plane verbs share --addr (default $MEMSIDECAR_ADDR or 127.0.0.1:7777),
--token (default $MEMSIDECAR_TOKEN), and --tls.

Examples:
  memctl token gen-keypair
  memctl token issue \
      --secret-key-hex $MEMSIDECAR_PASETO_SECRET_HEX \
      --tenant acme --agent agent-1 \
      --ns 'kv/scratchpad' --ops get,put,delete,scan --ttl 1h

  export MEMSIDECAR_TOKEN=$(memctl token issue --tenant acme --ns 'kv/*' --ops '*')
  memctl kv put scratchpad hello world
  memctl kv get scratchpad hello
  memctl ns ls
`)
}

func cmdGenKeypair(args []string) int {
	fs := flag.NewFlagSet("gen-keypair", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sec, pub, err := auth.GeneratePASETOKeyPair()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-keypair:", err)
		return 1
	}
	fmt.Printf("paseto:\n  secret_key_hex: %s\n  public_key_hex: %s\n", sec, pub)
	return 0
}

func cmdTokenIssue(args []string) int {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	var (
		format       = fs.String("format", "paseto", "paseto|jwt")
		secretHex    = fs.String("secret-key-hex", os.Getenv("MEMSIDECAR_PASETO_SECRET_HEX"), "PASETO v4 secret (hex). Defaults to $MEMSIDECAR_PASETO_SECRET_HEX")
		jwtSecret    = fs.String("jwt-secret", os.Getenv("MEMSIDECAR_JWT_SECRET"), "HS256 secret. Defaults to $MEMSIDECAR_JWT_SECRET")
		tenant       = fs.String("tenant", "", "tenant id (required)")
		agent        = fs.String("agent", "", "agent id")
		nsList       = fs.String("ns", "", "comma-separated namespace patterns, e.g. 'kv/scratchpad,kv/tool-*' (required)")
		opsList      = fs.String("ops", "get,put,delete,scan", "comma-separated allowed ops")
		ttl          = fs.Duration("ttl", time.Hour, "lifetime")
		tokenID      = fs.String("jti", "", "optional token id")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tenant == "" || *nsList == "" {
		fmt.Fprintln(os.Stderr, "--tenant and --ns are required")
		return 2
	}
	scope := auth.Scope{
		Tenant:           *tenant,
		Agent:            *agent,
		NamespacePattern: splitCSV(*nsList),
		AllowedOps:       splitCSV(*opsList),
	}
	var (
		tok string
		err error
	)
	switch *format {
	case "paseto":
		if *secretHex == "" {
			fmt.Fprintln(os.Stderr, "--secret-key-hex required (or set $MEMSIDECAR_PASETO_SECRET_HEX)")
			return 2
		}
		tok, err = auth.IssuePASETO(*secretHex, scope, *ttl, *tokenID)
	case "jwt":
		if *jwtSecret == "" {
			fmt.Fprintln(os.Stderr, "--jwt-secret required (or set $MEMSIDECAR_JWT_SECRET)")
			return 2
		}
		tok, err = auth.IssueJWT([]byte(*jwtSecret), scope, *ttl, *tokenID)
	default:
		fmt.Fprintln(os.Stderr, "unknown --format", *format)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue:", err)
		return 1
	}
	fmt.Println(tok)
	return 0
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
