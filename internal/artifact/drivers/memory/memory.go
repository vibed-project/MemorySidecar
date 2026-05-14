// Package memory is an in-memory artifact driver. Suitable for tests; loses
// data on restart. Bytes live in a per-namespace map and are copied on read
// so callers can't mutate stored content.
package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync"
	"time"

	"memsidecar/internal/artifact"
)

type stored struct {
	meta artifact.Meta
	data []byte
}

// Driver is an in-memory artifact driver. Safe for concurrent use.
type Driver struct {
	mu    sync.RWMutex
	items map[string]map[string]*stored // namespace -> id -> stored
	now   func() time.Time
}

// Options configures a Driver.
type Options struct {
	NowFunc func() time.Time
}

func New(opts Options) *Driver {
	d := &Driver{items: make(map[string]map[string]*stored), now: time.Now}
	if opts.NowFunc != nil {
		d.now = opts.NowFunc
	}
	return d
}

func (d *Driver) Close() error { return nil }

func (d *Driver) Put(_ context.Context, namespace string, header artifact.PutHeader, r io.Reader) (artifact.Meta, error) {
	buf, err := io.ReadAll(r)
	if err != nil {
		return artifact.Meta{}, err
	}
	id := header.ID
	if id == "" {
		id = randomID()
	}
	meta := artifact.Meta{
		ID:          id,
		Size:        header.Size,
		ContentType: header.ContentType,
		SHA256:      header.SHA256,
		Metadata:    cloneMeta(header.Metadata),
		CreatedAt:   d.now().UTC(),
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	bucket := d.items[namespace]
	if bucket == nil {
		bucket = make(map[string]*stored)
		d.items[namespace] = bucket
	}
	bucket[id] = &stored{meta: meta, data: buf}
	return meta, nil
}

func (d *Driver) Open(_ context.Context, namespace, id string, opts artifact.GetOptions) (artifact.Meta, io.ReadCloser, error) {
	d.mu.RLock()
	bucket := d.items[namespace]
	s, ok := bucket[id]
	d.mu.RUnlock()
	if !ok {
		return artifact.Meta{}, nil, artifact.ErrNotFound
	}
	from := int(opts.Offset)
	if from > len(s.data) {
		from = len(s.data)
	}
	to := len(s.data)
	if opts.Length > 0 && from+int(opts.Length) < to {
		to = from + int(opts.Length)
	}
	slice := make([]byte, to-from)
	copy(slice, s.data[from:to])
	return s.meta, io.NopCloser(bytes.NewReader(slice)), nil
}

func (d *Driver) Stat(_ context.Context, namespace, id string) (artifact.Meta, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.items[namespace][id]
	if !ok {
		return artifact.Meta{}, artifact.ErrNotFound
	}
	return s.meta, nil
}

// PatchMeta updates the stored meta with the size and sha256 computed by the
// service after the upload stream completes. Implements the optional
// metaPatcher interface declared in internal/artifact/service.go.
func (d *Driver) PatchMeta(_ context.Context, namespace, id, sha256Hex string, size uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.items[namespace][id]
	if !ok {
		return nil
	}
	s.meta.SHA256 = sha256Hex
	s.meta.Size = size
	return nil
}

func (d *Driver) Delete(_ context.Context, namespace, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	bucket := d.items[namespace]
	if bucket == nil {
		return false, nil
	}
	if _, ok := bucket[id]; !ok {
		return false, nil
	}
	delete(bucket, id)
	if len(bucket) == 0 {
		delete(d.items, namespace)
	}
	return true, nil
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

