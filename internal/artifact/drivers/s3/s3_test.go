//go:build integration

package s3_test

import (
	"context"
	"io"
	"strings"
	"testing"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"memsidecar/internal/artifact"
	s3drv "memsidecar/internal/artifact/drivers/s3"
)

func newDriver(t *testing.T) *s3drv.Driver {
	t.Helper()
	ctx := context.Background()
	c, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername("minioadmin"),
		tcminio.WithPassword("minioadmin"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	endpoint, err := c.ConnectionString(ctx)
	require.NoError(t, err)
	endpoint = strings.TrimPrefix(endpoint, "http://")

	// Pre-create the bucket via a raw client; the driver requires it.
	admin, err := miniogo.New(endpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4("minioadmin", "minioadmin", ""),
		Secure: false,
	})
	require.NoError(t, err)
	require.NoError(t, admin.MakeBucket(ctx, "test", miniogo.MakeBucketOptions{}))

	d, err := s3drv.New(ctx, s3drv.Options{
		Endpoint: endpoint, UseSSL: false, Bucket: "test",
		AccessKey: "minioadmin", SecretKey: "minioadmin",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestS3_PutGetRound(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	_, err := d.Put(ctx, "ns", artifact.PutHeader{
		ID: "obj1", ContentType: "text/plain", SHA256: "sha-fake", Size: 11,
		Metadata: map[string]string{"tag": "v1"},
	}, strings.NewReader("hello world"))
	require.NoError(t, err)

	meta, rc, err := d.Open(ctx, "ns", "obj1", artifact.GetOptions{})
	require.NoError(t, err)
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	assert.Equal(t, "hello world", string(body))
	assert.Equal(t, "sha-fake", meta.SHA256)
	assert.Equal(t, "v1", meta.Metadata["tag"])
}

func TestS3_StatDelete(t *testing.T) {
	d := newDriver(t)
	ctx := context.Background()
	_, _ = d.Put(ctx, "ns", artifact.PutHeader{ID: "obj1", Size: 1}, strings.NewReader("x"))
	meta, err := d.Stat(ctx, "ns", "obj1")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), meta.Size)

	existed, err := d.Delete(ctx, "ns", "obj1")
	require.NoError(t, err)
	assert.True(t, existed)
	_, err = d.Stat(ctx, "ns", "obj1")
	require.ErrorIs(t, err, artifact.ErrNotFound)
}
