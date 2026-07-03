package memory_test

import (
	"testing"

	"memsidecar/internal/graph"
	memdrv "memsidecar/internal/graph/drivers/memory"
	"memsidecar/internal/graph/graphtest"
)

type harness struct{}

func (harness) New(_ *testing.T) graph.Driver {
	return memdrv.New(memdrv.Options{})
}

func TestConformance(t *testing.T) {
	graphtest.RunConformance(t, harness{})
}
