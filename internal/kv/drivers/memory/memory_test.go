package memory_test

import (
	"testing"

	"memsidecar/internal/kv"
	"memsidecar/internal/kv/drivers/memory"
	"memsidecar/internal/kv/kvtest"
)

type harness struct{}

func (harness) New(t *testing.T) kv.Driver {
	t.Helper()
	d := memory.New(memory.Options{SweeperInterval: -1})
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func (harness) NewWithClock(t *testing.T, clock *kvtest.FakeClock) (kv.Driver, bool) {
	t.Helper()
	d := memory.New(memory.Options{SweeperInterval: -1, NowFunc: clock.Now})
	t.Cleanup(func() { _ = d.Close() })
	return d, true
}

func TestConformance(t *testing.T) {
	kvtest.RunConformance(t, harness{})
}
