// Package fs is a local-filesystem artifact driver.
//
// Layout: <base>/<namespace>/<id> for the bytes, plus <base>/<namespace>/<id>.json
// for metadata. Atomic writes use a temp file + rename so partially-written
// artifacts never become readable. Two-character sharding (the first two hex
// chars of the id) keeps any one directory shallow.
package fs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"memsidecar/internal/artifact"
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

// Options configures a Driver.
type Options struct {
	BaseDir string
	NowFunc func() time.Time
}

// Driver persists artifacts to a local directory tree.
type Driver struct {
	base string
	now  func() time.Time
	mu   sync.Mutex // serialises shard mkdir
}

func New(opts Options) (*Driver, error) {
	if opts.BaseDir == "" {
		return nil, errors.New("artifact/fs: base_dir required")
	}
	if err := os.MkdirAll(opts.BaseDir, 0o750); err != nil {
		return nil, fmt.Errorf("artifact/fs: mkdir base: %w", err)
	}
	d := &Driver{base: opts.BaseDir, now: time.Now}
	if opts.NowFunc != nil {
		d.now = opts.NowFunc
	}
	return d, nil
}

func (d *Driver) Close() error { return nil }

func (d *Driver) Put(_ context.Context, namespace string, header artifact.PutHeader, r io.Reader) (artifact.Meta, error) {
	id := header.ID
	if id == "" {
		id = randomID()
	}
	if !safeID.MatchString(id) {
		return artifact.Meta{}, fmt.Errorf("artifact/fs: id must match %s", safeID.String())
	}
	if !safeID.MatchString(namespace) {
		return artifact.Meta{}, fmt.Errorf("artifact/fs: namespace must match %s", safeID.String())
	}

	dir, dataPath, metaPath := d.paths(namespace, id)
	d.mu.Lock()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		d.mu.Unlock()
		return artifact.Meta{}, err
	}
	d.mu.Unlock()

	tmp, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return artifact.Meta{}, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		cleanup()
		return artifact.Meta{}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return artifact.Meta{}, err
	}
	if err := os.Rename(tmpName, dataPath); err != nil {
		cleanup()
		return artifact.Meta{}, err
	}

	meta := artifact.Meta{
		ID:          id,
		Size:        header.Size,
		ContentType: header.ContentType,
		SHA256:      header.SHA256,
		Metadata:    cloneMeta(header.Metadata),
		CreatedAt:   d.now().UTC(),
	}
	if err := writeMeta(metaPath, meta); err != nil {
		_ = os.Remove(dataPath)
		return artifact.Meta{}, err
	}
	return meta, nil
}

func (d *Driver) Open(ctx context.Context, namespace, id string, opts artifact.GetOptions) (artifact.Meta, io.ReadCloser, error) {
	meta, err := d.Stat(ctx, namespace, id)
	if err != nil {
		return artifact.Meta{}, nil, err
	}
	_, dataPath, _ := d.paths(namespace, id)
	f, err := os.Open(dataPath) //nolint:gosec // base is operator-controlled, id is regex-validated
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return artifact.Meta{}, nil, artifact.ErrNotFound
		}
		return artifact.Meta{}, nil, err
	}
	if opts.Offset > 0 {
		if _, err := f.Seek(int64(opts.Offset), io.SeekStart); err != nil {
			_ = f.Close()
			return artifact.Meta{}, nil, err
		}
	}
	var rc io.ReadCloser = f
	if opts.Length > 0 {
		rc = &limitedReadCloser{R: io.LimitReader(f, int64(opts.Length)), C: f}
	}
	return meta, rc, nil
}

func (d *Driver) Stat(_ context.Context, namespace, id string) (artifact.Meta, error) {
	_, _, metaPath := d.paths(namespace, id)
	body, err := os.ReadFile(metaPath) //nolint:gosec // id is regex-validated
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return artifact.Meta{}, artifact.ErrNotFound
		}
		return artifact.Meta{}, err
	}
	var meta artifact.Meta
	if err := json.Unmarshal(body, &meta); err != nil {
		return artifact.Meta{}, fmt.Errorf("artifact/fs: unmarshal meta: %w", err)
	}
	return meta, nil
}

// PatchMeta rewrites the metadata JSON to include the size and sha256 the
// service computed after the upload stream completed. Implements the
// optional metaPatcher interface in internal/artifact/service.go.
func (d *Driver) PatchMeta(ctx context.Context, namespace, id, sha256Hex string, size uint64) error {
	meta, err := d.Stat(ctx, namespace, id)
	if err != nil {
		return err
	}
	meta.SHA256 = sha256Hex
	meta.Size = size
	_, _, metaPath := d.paths(namespace, id)
	return writeMeta(metaPath, meta)
}

func (d *Driver) Delete(_ context.Context, namespace, id string) (bool, error) {
	_, dataPath, metaPath := d.paths(namespace, id)
	if _, err := os.Stat(dataPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := os.Remove(dataPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// paths computes (dir, data, meta) on disk. Two-char sharding keeps any one
// dir reasonably shallow when ids are UUIDs.
func (d *Driver) paths(namespace, id string) (dir, data, meta string) {
	shard := id
	if len(shard) >= 2 {
		shard = shard[:2]
	}
	dir = filepath.Join(d.base, namespace, shard)
	data = filepath.Join(dir, id)
	meta = filepath.Join(dir, id+".json")
	return
}

func writeMeta(path string, m artifact.Meta) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o640); err != nil { //nolint:gosec // metadata sidecar file; 0640 (group-readable) is intentional
		return err
	}
	return os.Rename(tmp, path)
}

type limitedReadCloser struct {
	R io.Reader
	C io.Closer
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.R.Read(p) }
func (l *limitedReadCloser) Close() error               { return l.C.Close() }

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
