// Package s3 is an artifact driver backed by any S3-compatible object store
// (AWS S3, MinIO, Cloudflare R2, ...). Each namespace lives under a key
// prefix inside one bucket.
//
// The bucket must exist; the driver does not create or delete it. Per-object
// metadata (content type, sha256, custom k/v) is stored in S3 user-metadata
// headers, which round-trip without an extra GET.
package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"memsidecar/internal/artifact"
)

// Options configures a Driver.
type Options struct {
	Endpoint  string // host[:port]; no scheme
	UseSSL    bool
	Region    string
	Bucket    string
	Prefix    string // object-key prefix (e.g. "memsidecar/")
	AccessKey string
	SecretKey string
	NowFunc   func() time.Time
}

// Driver persists artifacts to an S3-compatible store.
type Driver struct {
	c      *miniogo.Client
	bucket string
	prefix string
	now    func() time.Time
}

func New(ctx context.Context, opts Options) (*Driver, error) {
	if opts.Endpoint == "" {
		return nil, errors.New("artifact/s3: endpoint required")
	}
	if opts.Bucket == "" {
		return nil, errors.New("artifact/s3: bucket required")
	}
	if opts.AccessKey == "" || opts.SecretKey == "" {
		return nil, errors.New("artifact/s3: access_key and secret_key required")
	}

	c, err := miniogo.New(opts.Endpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, ""),
		Secure: opts.UseSSL,
		Region: opts.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("artifact/s3: client: %w", err)
	}

	exists, err := c.BucketExists(ctx, opts.Bucket)
	if err != nil {
		return nil, fmt.Errorf("artifact/s3: bucket check: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("artifact/s3: bucket %q does not exist", opts.Bucket)
	}

	d := &Driver{c: c, bucket: opts.Bucket, prefix: opts.Prefix, now: time.Now}
	if opts.NowFunc != nil {
		d.now = opts.NowFunc
	}
	return d, nil
}

func (d *Driver) Close() error { return nil }

func (d *Driver) key(namespace, id string) string {
	return d.prefix + namespace + "/" + id
}

const (
	metaSHA256      = "x-amz-meta-memsidecar-sha256"
	metaContentType = "x-amz-meta-memsidecar-content-type"
	metaCreatedAt   = "x-amz-meta-memsidecar-created-at"
	userMetaPrefix  = "memsidecar-user-"
)

func (d *Driver) Put(ctx context.Context, namespace string, header artifact.PutHeader, r io.Reader) (artifact.Meta, error) {
	id := header.ID
	if id == "" {
		id = randomID()
	}
	created := d.now().UTC()

	userMeta := map[string]string{
		"memsidecar-sha256":       header.SHA256,
		"memsidecar-content-type": header.ContentType,
		"memsidecar-created-at":   created.Format(time.RFC3339Nano),
	}
	for k, v := range header.Metadata {
		userMeta[userMetaPrefix+k] = v
	}

	objSize := int64(-1) // unknown → multipart
	if header.Size > 0 {
		objSize = int64(header.Size)
	}

	_, err := d.c.PutObject(ctx, d.bucket, d.key(namespace, id), r, objSize,
		miniogo.PutObjectOptions{
			ContentType:  header.ContentType,
			UserMetadata: userMeta,
		})
	if err != nil {
		return artifact.Meta{}, fmt.Errorf("artifact/s3: put: %w", err)
	}

	return artifact.Meta{
		ID:          id,
		Size:        header.Size,
		ContentType: header.ContentType,
		SHA256:      header.SHA256,
		Metadata:    cloneMeta(header.Metadata),
		CreatedAt:   created,
	}, nil
}

func (d *Driver) Open(ctx context.Context, namespace, id string, opts artifact.GetOptions) (artifact.Meta, io.ReadCloser, error) {
	meta, err := d.Stat(ctx, namespace, id)
	if err != nil {
		return artifact.Meta{}, nil, err
	}

	getOpts := miniogo.GetObjectOptions{}
	if opts.Length > 0 {
		_ = getOpts.SetRange(int64(opts.Offset), int64(opts.Offset+opts.Length-1))
	} else if opts.Offset > 0 {
		_ = getOpts.SetRange(int64(opts.Offset), 0)
	}

	obj, err := d.c.GetObject(ctx, d.bucket, d.key(namespace, id), getOpts)
	if err != nil {
		return artifact.Meta{}, nil, fmt.Errorf("artifact/s3: get: %w", err)
	}
	// Do NOT probe the object with obj.Stat() here. minio-go's Object is lazy:
	// the first Stat/Seek request strips the Range header off the pending GET
	// (api-get-object.go), so a probe would discard our offset/length range and
	// the following Read would return the whole object. Existence was already
	// confirmed by d.Stat above; any late fetch error surfaces on the first Read.
	return meta, obj, nil
}

func (d *Driver) Stat(ctx context.Context, namespace, id string) (artifact.Meta, error) {
	info, err := d.c.StatObject(ctx, d.bucket, d.key(namespace, id), miniogo.StatObjectOptions{})
	if err != nil {
		if isNotFoundErr(err) {
			return artifact.Meta{}, artifact.ErrNotFound
		}
		return artifact.Meta{}, fmt.Errorf("artifact/s3: stat: %w", err)
	}
	meta := artifact.Meta{
		ID:          id,
		Size:        uint64(info.Size),
		ContentType: info.ContentType,
	}
	for k, v := range info.UserMetadata {
		lk := strings.ToLower(k)
		switch lk {
		case "memsidecar-sha256":
			meta.SHA256 = v
		case "memsidecar-content-type":
			if meta.ContentType == "" {
				meta.ContentType = v
			}
		case "memsidecar-created-at":
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				meta.CreatedAt = t
			}
		default:
			if strings.HasPrefix(lk, userMetaPrefix) {
				if meta.Metadata == nil {
					meta.Metadata = make(map[string]string)
				}
				meta.Metadata[strings.TrimPrefix(lk, userMetaPrefix)] = v
			}
		}
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = info.LastModified.UTC()
	}
	_ = strconv.Itoa // keep import minimal even after future tweaks
	_ = metaSHA256
	_ = metaContentType
	_ = metaCreatedAt
	return meta, nil
}

func (d *Driver) Delete(ctx context.Context, namespace, id string) (bool, error) {
	// minio-go's RemoveObject succeeds when the object is absent, so probe first.
	if _, err := d.Stat(ctx, namespace, id); err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	err := d.c.RemoveObject(ctx, d.bucket, d.key(namespace, id), miniogo.RemoveObjectOptions{})
	if err != nil {
		return false, fmt.Errorf("artifact/s3: delete: %w", err)
	}
	return true, nil
}

func isNotFoundErr(err error) bool {
	var resp miniogo.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.StatusCode == 404
	}
	return false
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}

func cloneMeta(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
