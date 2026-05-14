package policy

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

type fixedEngine struct{ allow bool }

func (f fixedEngine) PreRead(context.Context, HookCtx) Decision {
	return Decision{Allow: f.allow, Reason: "fixed"}
}
func (f fixedEngine) PreWrite(context.Context, HookCtx) Decision {
	return Decision{Allow: f.allow, Reason: "fixed"}
}

func TestHolder_InitialAndSwap(t *testing.T) {
	h := NewHolder(fixedEngine{allow: true})
	d := h.PreRead(context.Background(), HookCtx{})
	assert.True(t, d.Allow)

	h.Swap(fixedEngine{allow: false})
	d = h.PreRead(context.Background(), HookCtx{})
	assert.False(t, d.Allow)
}

func TestHolder_SwapConcurrent(t *testing.T) {
	h := NewHolder(fixedEngine{allow: true})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.Swap(fixedEngine{allow: j%2 == 0})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = h.PreRead(context.Background(), HookCtx{})
			}
		}()
	}
	wg.Wait()
	// No panic / race = success; -race in `go test` will surface conflicts.
}
