package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

type Config struct {
	Server        ServerConfig        `koanf:"server"`
	Observability ObservabilityConfig `koanf:"observability"`
	Auth          AuthConfig          `koanf:"auth"`
	Policy        PolicyConfig        `koanf:"policy"`
	Backends      []BackendConfig     `koanf:"backends"`
	Namespaces    []NamespaceConfig   `koanf:"namespaces"`
	// TenantIsolation, when true, scopes every block's storage to the caller's
	// capability tenant, so two tenants sharing a namespace name get physically
	// separate data. Off by default: a single-tenant deployment behaves exactly
	// as before and existing data is unaffected. Enable it for multi-tenant
	// deployments (all tokens must then carry a tenant).
	TenantIsolation bool `koanf:"tenant_isolation"`
}

// PolicyConfig mirrors policy.Spec under a config-friendly tag. It is parsed
// here so config validation can catch typos before the engine spins up.
type PolicyConfig struct {
	Default string             `koanf:"default"`
	Rules   []PolicyRuleConfig `koanf:"rules"`
}

type PolicyRuleConfig struct {
	Name   string             `koanf:"name"`
	Effect string             `koanf:"effect"`
	Reason string             `koanf:"reason"`
	Match  PolicyMatchConfig  `koanf:"match"`
	Bucket PolicyBucketConfig `koanf:"bucket"`
	Max    PolicyCapConfig    `koanf:"max"`
}

type PolicyMatchConfig struct {
	Tenant    []string `koanf:"tenant"`
	Agent     []string `koanf:"agent"`
	Block     []string `koanf:"block"`
	Namespace []string `koanf:"namespace"`
	Op        []string `koanf:"op"`
}

type PolicyBucketConfig struct {
	PerTenant     bool    `koanf:"per_tenant"`
	PerAgent      bool    `koanf:"per_agent"`
	PerNamespace  bool    `koanf:"per_namespace"`
	PerOp         bool    `koanf:"per_op"`
	RatePerSecond float64 `koanf:"rate_per_second"`
	Burst         int     `koanf:"burst"`
}

// PolicyCapConfig mirrors policy.Cap: per-dimension upper bounds for an
// effect: cap rule. A zero field imposes no bound on that dimension.
type PolicyCapConfig struct {
	TopK             uint32 `koanf:"top_k"`
	Limit            uint32 `koanf:"limit"`
	Depth            uint32 `koanf:"depth"`
	FanOut           uint32 `koanf:"fan_out"`
	RerankCandidateK uint32 `koanf:"rerank_candidate_k"`
}

type ServerConfig struct {
	GRPC            GRPCConfig    `koanf:"grpc"`
	HTTP            HTTPConfig    `koanf:"http"`
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout"`
}

type GRPCConfig struct {
	TCP string     `koanf:"tcp"`
	UDS string     `koanf:"uds"`
	TLS *TLSConfig `koanf:"tls"`
}

// TLSConfig enables TLS on the gRPC TCP listener. UDS is always plaintext
// since access is gated by filesystem permissions.
//
// Setting ClientCAFile turns on mTLS: every client must present a certificate
// chained to that CA. Capability tokens still apply on top — mTLS authenticates
// the transport peer, the token still scopes what they can do.
type TLSConfig struct {
	CertFile          string `koanf:"cert_file"`
	KeyFile           string `koanf:"key_file"`
	ClientCAFile      string `koanf:"client_ca_file"`
	RequireClientCert bool   `koanf:"require_client_cert"`
}

// HTTPConfig configures the grpc-gateway HTTP/JSON listener.
// An empty Addr disables it. Methods are reachable at
//
//	POST /<package>.<service>/<method>
//
// with the JSON request body, matching gRPC's URL scheme.
type HTTPConfig struct {
	Addr string `koanf:"addr"`
}

type ObservabilityConfig struct {
	Tracing TracingConfig `koanf:"tracing"`
	Metrics MetricsConfig `koanf:"metrics"`
	Logging LoggingConfig `koanf:"logging"`
}

type TracingConfig struct {
	Exporter    string     `koanf:"exporter"` // "stdout" | "otlp" | "none"
	SampleRatio float64    `koanf:"sample_ratio"`
	OTLP        OTLPConfig `koanf:"otlp"`
}

// OTLPConfig configures the OTLP/gRPC trace exporter. Headers may carry an
// API key for managed backends (Honeycomb, Lightstep, etc.).
type OTLPConfig struct {
	Endpoint    string            `koanf:"endpoint"`    // host:port (e.g. localhost:4317)
	Insecure    bool              `koanf:"insecure"`    // plaintext gRPC; default secure
	HeadersEnv  map[string]string `koanf:"headers_env"` // header → env var holding value
	Headers     map[string]string `koanf:"headers"`     // header → literal value
	Compression string            `koanf:"compression"` // "gzip" | ""
}

type MetricsConfig struct {
	Exporter   string           `koanf:"exporter"` // "prometheus" | "otlp" | "none"
	Prometheus PrometheusConfig `koanf:"prometheus"`
	OTLP       OTLPConfig       `koanf:"otlp"` // only when exporter=otlp
}

type PrometheusConfig struct {
	Addr string `koanf:"addr"` // e.g. ":9090"
	Path string `koanf:"path"` // default "/metrics"
}

type LoggingConfig struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

type AuthConfig struct {
	Verifier string       `koanf:"verifier"` // "paseto" | "jwt"
	PASETO   PASETOConfig `koanf:"paseto"`
	JWT      JWTConfig    `koanf:"jwt"`
}

type PASETOConfig struct {
	PublicKeyHex   string   `koanf:"public_key_hex"`   // singular form (back-compat)
	PublicKeyHexes []string `koanf:"public_key_hexes"` // rotation list; tried in order
	PrivateKeyHex  string   `koanf:"private_key_hex"`  // dev issuer only
}

// AllKeys returns the union of PublicKeyHex (if set) and PublicKeyHexes.
// The result is deduplicated and ordered: explicit list first, then the
// singular value if not already present.
func (p PASETOConfig) AllKeys() []string {
	out := make([]string, 0, len(p.PublicKeyHexes)+1)
	seen := make(map[string]bool, len(p.PublicKeyHexes)+1)
	for _, k := range p.PublicKeyHexes {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	if p.PublicKeyHex != "" && !seen[p.PublicKeyHex] {
		out = append(out, p.PublicKeyHex)
	}
	return out
}

type JWTConfig struct {
	Alg        string   `koanf:"alg"`         // HS256 | RS256
	SecretEnv  string   `koanf:"secret_env"`  // env var holding HMAC secret (HS256)
	PublicPEM  string   `koanf:"public_pem"`  // singular RS256 public key (back-compat)
	PublicPEMs []string `koanf:"public_pems"` // rotation list (RS256)
}

// AllPublicPEMs returns the union of PublicPEM and PublicPEMs.
func (j JWTConfig) AllPublicPEMs() []string {
	out := make([]string, 0, len(j.PublicPEMs)+1)
	seen := make(map[string]bool, len(j.PublicPEMs)+1)
	for _, k := range j.PublicPEMs {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	if j.PublicPEM != "" && !seen[j.PublicPEM] {
		out = append(out, j.PublicPEM)
	}
	return out
}

type BackendConfig struct {
	Name    string                 `koanf:"name"`
	Driver  string                 `koanf:"driver"` // "memory" | "postgres"
	Options map[string]interface{} `koanf:"options"`
}

type NamespaceConfig struct {
	Block    string         `koanf:"block"` // "kv" | "episodic" | "semantic"
	Name     string         `koanf:"name"`
	Backend  string         `koanf:"backend"`
	Embedder EmbedderConfig `koanf:"embedder"` // semantic only
	// TextSearch is the Postgres full-text config for the sparse lane of hybrid
	// search (semantic only; e.g. "simple" or "english"). Empty = "simple".
	TextSearch string `koanf:"text_search"`
	// Access is the opt-in KV cache-tier policy (U5). Only honoured by the
	// in-memory driver. Durations are seconds (koanf here doesn't decode
	// duration strings).
	Access AccessConfig `koanf:"access"`
}

// AccessConfig is the per-namespace KV cache-tier policy (U5). The zero value
// is fully disabled.
type AccessConfig struct {
	Track               bool `koanf:"track"`                  // record access counters on Get
	SlideTTLSeconds     int  `koanf:"slide_ttl_seconds"`      // >0: extend a TTL'd key's expiry on Get
	Capacity            int  `koanf:"capacity"`               // >0: cap live keys; evict coldest by heat
	HeatHalfLifeSeconds int  `koanf:"heat_half_life_seconds"` // heat decay half-life (0 = driver default)
}

// EmbedderConfig configures the embedder bound to a semantic namespace.
type EmbedderConfig struct {
	Provider   string                 `koanf:"provider"` // "fake" | "ollama" | "openai"
	Model      string                 `koanf:"model"`
	Dimensions int                    `koanf:"dimensions"`
	Options    map[string]interface{} `koanf:"options"`
	// CacheSize bounds the per-namespace embedding cache (identical content is
	// embedded once). 0 (unset) enables a default-sized cache; a negative value
	// disables caching for the namespace.
	CacheSize int `koanf:"cache_size"`
}

// Load reads a YAML config file and overlays MINDD_* environment vars.
// Env vars use double-underscore as section separator: MINDD_SERVER__GRPC__TCP=":8080".
func Load(path string) (*Config, error) {
	k := koanf.New(".")

	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("load config %q: %w", path, err)
		}
	}

	envProvider := env.Provider("MINDD_", ".", func(s string) string {
		s = strings.TrimPrefix(s, "MINDD_")
		s = strings.ToLower(s)
		return strings.ReplaceAll(s, "__", ".")
	})
	if err := k.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("load env: %w", err)
	}

	cfg := &Config{}
	if err := k.UnmarshalWithConf("", cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.GRPC.TCP == "" {
		c.Server.GRPC.TCP = "127.0.0.1:7777"
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = 10 * time.Second
	}
	if c.Observability.Tracing.Exporter == "" {
		c.Observability.Tracing.Exporter = "stdout"
	}
	if c.Observability.Tracing.SampleRatio == 0 {
		c.Observability.Tracing.SampleRatio = 1.0
	}
	if c.Observability.Logging.Level == "" {
		c.Observability.Logging.Level = "info"
	}
	if c.Observability.Logging.Format == "" {
		c.Observability.Logging.Format = "json"
	}
	if c.Auth.Verifier == "" {
		c.Auth.Verifier = "paseto"
	}
}

func (c *Config) Validate() error {
	if c.Server.GRPC.TCP == "" && c.Server.GRPC.UDS == "" {
		return fmt.Errorf("at least one of server.grpc.tcp or server.grpc.uds must be set")
	}
	switch c.Auth.Verifier {
	case "paseto":
		if len(c.Auth.PASETO.AllKeys()) == 0 {
			return fmt.Errorf("auth.paseto: public_key_hex or public_key_hexes required when verifier=paseto")
		}
	case "jwt":
		if c.Auth.JWT.Alg == "" {
			return fmt.Errorf("auth.jwt.alg required when verifier=jwt")
		}
	default:
		return fmt.Errorf("unknown auth.verifier %q (expected paseto|jwt)", c.Auth.Verifier)
	}

	backendByName := make(map[string]BackendConfig, len(c.Backends))
	for _, b := range c.Backends {
		if b.Name == "" {
			return fmt.Errorf("backend with empty name")
		}
		if _, dup := backendByName[b.Name]; dup {
			return fmt.Errorf("duplicate backend name %q", b.Name)
		}
		switch b.Driver {
		case "memory", "postgres", "fs", "s3":
		default:
			return fmt.Errorf("backend %q: unknown driver %q (expected memory|postgres|fs|s3)", b.Name, b.Driver)
		}
		backendByName[b.Name] = b
	}

	seen := make(map[string]bool, len(c.Namespaces))
	for _, ns := range c.Namespaces {
		switch ns.Block {
		case "kv", "episodic", "artifact", "lease", "graph":
		case "semantic":
			if err := validateSemanticEmbedder(ns); err != nil {
				return err
			}
		default:
			return fmt.Errorf("namespace %q: unsupported block %q (expected kv|episodic|semantic|artifact|lease|graph)", ns.Name, ns.Block)
		}
		if ns.Name == "" {
			return fmt.Errorf("namespace with empty name")
		}
		key := ns.Block + "/" + ns.Name
		if seen[key] {
			return fmt.Errorf("duplicate namespace %q", key)
		}
		seen[key] = true
		if _, ok := backendByName[ns.Backend]; !ok {
			return fmt.Errorf("namespace %q references unknown backend %q", key, ns.Backend)
		}
	}
	return nil
}

func validateSemanticEmbedder(ns NamespaceConfig) error {
	switch ns.Embedder.Provider {
	case "fake", "ollama":
	case "openai":
		if ns.Embedder.Model == "" {
			return fmt.Errorf("namespace %q: embedder.model required for openai", ns.Name)
		}
	case "":
		return fmt.Errorf("namespace %q: embedder.provider required for semantic", ns.Name)
	default:
		return fmt.Errorf("namespace %q: unsupported embedder provider %q (expected fake|ollama|openai)", ns.Name, ns.Embedder.Provider)
	}
	if ns.Embedder.Provider == "ollama" && ns.Embedder.Model == "" {
		return fmt.Errorf("namespace %q: embedder.model required for ollama", ns.Name)
	}
	if ns.Embedder.Dimensions <= 0 {
		return fmt.Errorf("namespace %q: embedder.dimensions must be > 0", ns.Name)
	}
	return nil
}
