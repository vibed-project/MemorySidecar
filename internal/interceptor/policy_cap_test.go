package interceptor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"memsidecar/internal/policy"
)

type topKReq struct {
	ns   string
	topK uint32
}

func (r topKReq) GetNamespace() string { return r.ns }
func (r topKReq) GetTopK() uint32      { return r.topK }

// TestPolicyUnary_CapExceeded verifies the interceptor extracts top_k, enforces
// a cap rule, and maps the rejection to ResourceExhausted (ADR-0002 §8), while
// a within-cap request passes through.
func TestPolicyUnary_CapExceeded(t *testing.T) {
	eng, err := policy.NewRuleEngine(policy.Spec{
		Rules: []policy.Rule{{
			Name:   "search-topk-cap",
			Effect: policy.EffectCap,
			Match:  policy.Match{Ops: []string{"search"}},
			Max:    policy.Cap{TopK: 100},
		}},
	})
	require.NoError(t, err)
	intercept := PolicyUnary(eng)

	call := func(topK uint32) error {
		_, err := intercept(context.Background(), topKReq{ns: "notes", topK: topK},
			&grpc.UnaryServerInfo{FullMethod: "/memsidecar.semantic.v1.Semantic/Search"},
			func(context.Context, any) (any, error) { return "ok", nil })
		return err
	}

	require.NoError(t, call(50), "within the cap must pass")

	err = call(500)
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "top_k 500 exceeds cap 100")
}
