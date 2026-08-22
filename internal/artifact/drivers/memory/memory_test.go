package memory

import (
	"testing"

	"github.com/vibed-project/mindD/internal/artifact"
	"github.com/vibed-project/mindD/internal/artifact/artifacttest"
)

type harness struct{}

func (harness) New(t *testing.T) artifact.Driver {
	t.Helper()
	d := New(Options{})
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestConformance(t *testing.T) {
	artifacttest.RunConformance(t, harness{})
}
