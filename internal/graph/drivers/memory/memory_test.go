package memory_test

import (
	"testing"

	"github.com/vibed-project/mindD/internal/graph"
	memdrv "github.com/vibed-project/mindD/internal/graph/drivers/memory"
	"github.com/vibed-project/mindD/internal/graph/graphtest"
)

type harness struct{}

func (harness) New(_ *testing.T) graph.Driver {
	return memdrv.New(memdrv.Options{})
}

func TestConformance(t *testing.T) {
	graphtest.RunConformance(t, harness{})
}
