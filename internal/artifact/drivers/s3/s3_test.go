//go:build integration

package s3_test

import (
	"context"
	"strings"
	"testing"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/vibed-project/mindD/internal/artifact"
	"github.com/vibed-project/mindD/internal/artifact/artifacttest"
	s3drv "github.com/vibed-project/mindD/internal/artifact/drivers/s3"
)

type harness struct{}

func (harness) New(t *testing.T) artifact.Driver {
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

	// The driver requires the bucket to exist; pre-create via a raw client.
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

func TestConformance(t *testing.T) {
	artifacttest.RunConformance(t, harness{})
}
