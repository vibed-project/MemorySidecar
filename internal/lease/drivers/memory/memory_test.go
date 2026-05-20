package memory

import (
	"testing"
	"time"

	"memsidecar/internal/lease"
	"memsidecar/internal/lease/leasetest"
)

type harness struct{}

func (harness) New(t *testing.T) lease.Driver {
	t.Helper()
	d := New(Options{PollInterval: 10 * time.Millisecond})
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestConformance(t *testing.T) {
	leasetest.RunConformance(t, harness{})
}
