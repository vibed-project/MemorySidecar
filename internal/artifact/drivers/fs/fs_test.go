package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vibed-project/mindD/internal/artifact"
	"github.com/vibed-project/mindD/internal/artifact/artifacttest"
)

type harness struct{}

func (harness) New(t *testing.T) artifact.Driver {
	t.Helper()
	d, err := New(Options{BaseDir: t.TempDir()})
	require.NoError(t, err)
	return d
}

func TestConformance(t *testing.T) {
	artifacttest.RunConformance(t, harness{})
}

// Driver-specific: the fs driver shards files by ID prefix and rejects unsafe
// path components. These assertions are about on-disk layout and path safety,
// not the cross-driver contract.
func TestPut_ShardsLayout(t *testing.T) {
	dir := t.TempDir()
	d, err := New(Options{BaseDir: dir})
	require.NoError(t, err)
	_, err = d.Put(context.Background(), "ns", artifact.PutHeader{ID: "abcdef"}, strings.NewReader("x"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "ns", "ab", "abcdef"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "ns", "ab", "abcdef.json"))
	require.NoError(t, err)
}

func TestPut_RejectsUnsafeID(t *testing.T) {
	d, err := New(Options{BaseDir: t.TempDir()})
	require.NoError(t, err)
	_, err = d.Put(context.Background(), "ns", artifact.PutHeader{ID: "../etc/passwd"}, strings.NewReader("x"))
	require.Error(t, err)
}
