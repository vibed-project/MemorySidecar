package fs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/artifact"
)

func newDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(Options{BaseDir: t.TempDir()})
	require.NoError(t, err)
	return d
}

func TestPutGetRound(t *testing.T) {
	d := newDriver(t)
	_, err := d.Put(context.Background(), "ns", artifact.PutHeader{
		ID: "obj1", ContentType: "text/plain", SHA256: "sha", Size: 11,
	}, strings.NewReader("hello world"))
	require.NoError(t, err)

	_, rc, err := d.Open(context.Background(), "ns", "obj1", artifact.GetOptions{})
	require.NoError(t, err)
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	assert.Equal(t, "hello world", string(b))
}

func TestStat(t *testing.T) {
	d := newDriver(t)
	_, _ = d.Put(context.Background(), "ns", artifact.PutHeader{ID: "obj1", Size: 5, SHA256: "x"}, strings.NewReader("hello"))
	meta, err := d.Stat(context.Background(), "ns", "obj1")
	require.NoError(t, err)
	assert.Equal(t, uint64(5), meta.Size)
	assert.Equal(t, "x", meta.SHA256)
}

func TestDelete(t *testing.T) {
	d := newDriver(t)
	_, _ = d.Put(context.Background(), "ns", artifact.PutHeader{ID: "a"}, strings.NewReader("x"))
	existed, _ := d.Delete(context.Background(), "ns", "a")
	assert.True(t, existed)
	_, err := d.Stat(context.Background(), "ns", "a")
	require.ErrorIs(t, err, artifact.ErrNotFound)
}

func TestPut_ShardsLayout(t *testing.T) {
	dir := t.TempDir()
	d, _ := New(Options{BaseDir: dir})
	_, _ = d.Put(context.Background(), "ns", artifact.PutHeader{ID: "abcdef"}, strings.NewReader("x"))
	// Expect <base>/ns/ab/abcdef and <base>/ns/ab/abcdef.json
	_, err := os.Stat(filepath.Join(dir, "ns", "ab", "abcdef"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "ns", "ab", "abcdef.json"))
	require.NoError(t, err)
}

func TestPut_RejectsUnsafeID(t *testing.T) {
	d := newDriver(t)
	_, err := d.Put(context.Background(), "ns", artifact.PutHeader{ID: "../etc/passwd"}, strings.NewReader("x"))
	require.Error(t, err)
}

func TestOpen_OffsetLength(t *testing.T) {
	d := newDriver(t)
	_, _ = d.Put(context.Background(), "ns", artifact.PutHeader{ID: "a"}, strings.NewReader("abcdefghij"))
	_, rc, err := d.Open(context.Background(), "ns", "a", artifact.GetOptions{Offset: 3, Length: 4})
	require.NoError(t, err)
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	assert.Equal(t, "defg", string(b))
}
