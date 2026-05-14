package policy

// Effect describes what a matching rule does.
type Effect string

const (
	EffectAllow     Effect = "allow"
	EffectDeny      Effect = "deny"
	EffectRateLimit Effect = "rate_limit"
)

// Default describes the engine's behaviour when no rule matches.
type Default string

const (
	DefaultAllow Default = "allow"
	DefaultDeny  Default = "deny"
)

// Match filters which requests a Rule applies to. An empty list on a field
// means "match anything"; a non-empty list means "the request's value must
// equal one of these".
//
// Namespace patterns support a single trailing "*" for prefix matching, e.g.
// "tool-*". Op values are the dotted form ("kv.put") or the verb-only form
// ("put") used in capability tokens.
type Match struct {
	Tenants    []string `koanf:"tenant"`
	Agents     []string `koanf:"agent"`
	Blocks     []string `koanf:"block"`
	Namespaces []string `koanf:"namespace"`
	Ops        []string `koanf:"op"`
}

// Bucket configures a token-bucket limiter for a rate_limit Effect.
type Bucket struct {
	PerTenant     bool    `koanf:"per_tenant"`
	PerAgent      bool    `koanf:"per_agent"`
	PerNamespace  bool    `koanf:"per_namespace"`
	PerOp         bool    `koanf:"per_op"`
	RatePerSecond float64 `koanf:"rate_per_second"`
	Burst         int     `koanf:"burst"`
}

// Rule is one entry in the policy engine's ordered list.
type Rule struct {
	Name   string `koanf:"name"`
	Effect Effect `koanf:"effect"`
	Reason string `koanf:"reason"`
	Match  Match  `koanf:"match"`
	Bucket Bucket `koanf:"bucket"`
}

// Spec is the full policy configuration.
type Spec struct {
	Default Default `koanf:"default"`
	Rules   []Rule  `koanf:"rules"`
}
