package memory

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/artifact"
)

func TestPut_Get_Round(t *testing.T) {
	d := New(Options{})
	defer d.Close()

	body := strings.NewReader("hello world")
	meta, err := d.Put(context.Background(), "ns", artifact.PutHeader{
		ID: "obj1", ContentType: "text/plain", SHA256: "sha-fake", Size: 11,
	}, body)
	require.NoError(t, err)
	assert.Equal(t, "obj1", meta.ID)
	assert.Equal(t, uint64(11), meta.Size)

	got, rc, err := d.Open(context.Background(), "ns", "obj1", artifact.GetOptions{})
	require.NoError(t, err)
	defer rc.Close()
	all, _ := io.ReadAll(rc)
	assert.Equal(t, "hello world", string(all))
	assert.Equal(t, "obj1", got.ID)
}

func TestPut_AssignsID(t *testing.T) {
	d := New(Options{})
	meta, err := d.Put(context.Background(), "ns", artifact.PutHeader{}, strings.NewReader("x"))
	require.NoError(t, err)
	assert.NotEmpty(t, meta.ID)
}

func TestOpen_OffsetLength(t *testing.T) {
	d := New(Options{})
	_, err := d.Put(context.Background(), "ns", artifact.PutHeader{ID: "a"}, strings.NewReader("abcdefghij"))
	require.NoError(t, err)

	_, rc, err := d.Open(context.Background(), "ns", "a", artifact.GetOptions{Offset: 3, Length: 4})
	require.NoError(t, err)
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	assert.Equal(t, "defg", string(b))
}

func TestOpen_Missing(t *testing.T) {
	d := New(Options{})
	_, _, err := d.Open(context.Background(), "ns", "missing", artifact.GetOptions{})
	require.ErrorIs(t, err, artifact.ErrNotFound)
}

func TestDelete(t *testing.T) {
	d := New(Options{})
	_, _ = d.Put(context.Background(), "ns", artifact.PutHeader{ID: "a"}, strings.NewReader("x"))
	existed, err := d.Delete(context.Background(), "ns", "a")
	require.NoError(t, err)
	assert.True(t, existed)
	existed, _ = d.Delete(context.Background(), "ns", "a")
	assert.False(t, existed)
}
