// Package artifacttest provides a driver-agnostic conformance suite that
// every artifact.Driver implementation must pass.
package artifacttest

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"memsidecar/internal/artifact"
)

// Harness adapts a concrete driver to the conformance suite.
type Harness interface {
	New(t *testing.T) artifact.Driver
}

// RunConformance runs the full conformance battery against the driver
// produced by h.
func RunConformance(t *testing.T, h Harness) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"Put_Get_Round", testPutGetRound},
		{"Put_AssignsID", testPutAssignsID},
		{"Open_OffsetLength", testOpenOffsetLength},
		{"Open_Missing", testOpenMissing},
		{"Stat_RoundTrip", testStatRoundTrip},
		{"Delete", testDelete},
		{"Metadata_RoundTrip", testMetadataRoundTrip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, h) })
	}
}

func testPutGetRound(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	meta, err := d.Put(ctx, "ns", artifact.PutHeader{
		ID: "obj1", ContentType: "text/plain", SHA256: "sha-fake", Size: 11,
	}, strings.NewReader("hello world"))
	require.NoError(t, err)
	assert.Equal(t, "obj1", meta.ID)
	assert.Equal(t, uint64(11), meta.Size)

	got, rc, err := d.Open(ctx, "ns", "obj1", artifact.GetOptions{})
	require.NoError(t, err)
	defer rc.Close()
	all, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(all))
	assert.Equal(t, "obj1", got.ID)
}

func testPutAssignsID(t *testing.T, h Harness) {
	d := h.New(t)
	meta, err := d.Put(context.Background(), "ns", artifact.PutHeader{}, strings.NewReader("x"))
	require.NoError(t, err)
	assert.NotEmpty(t, meta.ID)
}

func testOpenOffsetLength(t *testing.T, h Harness) {
	d := h.New(t)
	_, err := d.Put(context.Background(), "ns", artifact.PutHeader{ID: "a"}, strings.NewReader("abcdefghij"))
	require.NoError(t, err)

	_, rc, err := d.Open(context.Background(), "ns", "a", artifact.GetOptions{Offset: 3, Length: 4})
	require.NoError(t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "defg", string(b))
}

func testOpenMissing(t *testing.T, h Harness) {
	d := h.New(t)
	_, _, err := d.Open(context.Background(), "ns", "missing", artifact.GetOptions{})
	require.ErrorIs(t, err, artifact.ErrNotFound)
}

func testStatRoundTrip(t *testing.T, h Harness) {
	d := h.New(t)
	_, err := d.Put(context.Background(), "ns", artifact.PutHeader{
		ID: "obj1", Size: 5, SHA256: "deadbeef",
	}, strings.NewReader("hello"))
	require.NoError(t, err)

	meta, err := d.Stat(context.Background(), "ns", "obj1")
	require.NoError(t, err)
	assert.Equal(t, uint64(5), meta.Size)
	assert.Equal(t, "deadbeef", meta.SHA256)
}

func testDelete(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	_, err := d.Put(ctx, "ns", artifact.PutHeader{ID: "a"}, strings.NewReader("x"))
	require.NoError(t, err)

	existed, err := d.Delete(ctx, "ns", "a")
	require.NoError(t, err)
	assert.True(t, existed)

	existed, err = d.Delete(ctx, "ns", "a")
	require.NoError(t, err)
	assert.False(t, existed)

	_, err = d.Stat(ctx, "ns", "a")
	require.ErrorIs(t, err, artifact.ErrNotFound)
}

func testMetadataRoundTrip(t *testing.T, h Harness) {
	d := h.New(t)
	ctx := context.Background()
	_, err := d.Put(ctx, "ns", artifact.PutHeader{
		ID: "obj1", Size: 1, SHA256: "sha",
		Metadata: map[string]string{"tag": "v1", "owner": "alice"},
	}, strings.NewReader("x"))
	require.NoError(t, err)

	meta, err := d.Stat(ctx, "ns", "obj1")
	require.NoError(t, err)
	assert.Equal(t, "v1", meta.Metadata["tag"])
	assert.Equal(t, "alice", meta.Metadata["owner"])
}
