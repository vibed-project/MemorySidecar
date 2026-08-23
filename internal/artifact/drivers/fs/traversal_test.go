package fs_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/vibed-project/mindD/internal/artifact"
	fsdrv "github.com/vibed-project/mindD/internal/artifact/drivers/fs"
)

// A capability-scoped caller must not be able to address anything outside its
// own namespace directory. Before this was fixed, safeID was applied only in
// Put and List; Stat, Open, Delete and PatchMeta built paths straight from the
// caller's id, so "./../../other/secret" resolved outside the namespace and
// Delete removed arbitrary files the process could reach.
func TestNoPathTraversalOutsideNamespace(t *testing.T) {
	base := t.TempDir()

	outside := filepath.Join(base, "OUTSIDE")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret+".json", []byte(`{"id":"secret","size":16}`), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := fsdrv.New(fsdrv.Options{BaseDir: filepath.Join(base, "blobs")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	ids := []string{
		"./../../OUTSIDE/secret",
		"../../OUTSIDE/secret",
		"..",
		".",
		"../../../../etc/passwd",
	}

	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			if _, err := d.Stat(ctx, "ns", id); err == nil {
				t.Errorf("Stat(%q) succeeded; it must not resolve outside the namespace", id)
			}
			if _, rc, err := d.Open(ctx, "ns", id, artifact.GetOptions{}); err == nil {
				b, _ := io.ReadAll(rc)
				_ = rc.Close()
				t.Errorf("Open(%q) succeeded and returned %q", id, string(b))
			}
			if ok, err := d.Delete(ctx, "ns", id); err == nil && ok {
				t.Errorf("Delete(%q) reported a deletion outside the namespace", id)
			}
			if err := d.PatchMeta(ctx, "ns", id, "deadbeef", 4); err == nil {
				t.Errorf("PatchMeta(%q) succeeded", id)
			}
		})
	}

	// The bytes and the sidecar outside the namespace must still be there.
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("a file outside the namespace was deleted: %v", err)
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != "TOP-SECRET-BYTES" {
		t.Fatalf("a file outside the namespace was modified: %v", err)
	}
}

// With tenant isolation on, the namespace reaching the driver is
// "<tenant>\x1f<namespace>". The old regex rejected the separator outright, so
// Put and List failed for every tenant-isolated deployment while Stat, Open
// and Delete traversed happily -- broken and unsafe at the same time.
func TestTenantQualifiedNamespaceRoundTrips(t *testing.T) {
	d, err := fsdrv.New(fsdrv.Options{BaseDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	ns := "tenant-a\x1fblobs"

	meta, err := d.Put(ctx, ns, artifact.PutHeader{ID: "doc", SHA256: "abc", Size: 5},
		readerOf("hello"))
	if err != nil {
		t.Fatalf("Put into a tenant-qualified namespace: %v", err)
	}
	if meta.ID != "doc" {
		t.Fatalf("Put returned id %q, want doc", meta.ID)
	}
	if _, err := d.Stat(ctx, ns, "doc"); err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// A different tenant with the same namespace name must not see it.
	if _, err := d.Stat(ctx, "tenant-b\x1fblobs", "doc"); err == nil {
		t.Fatal("tenant-b could Stat tenant-a's artifact")
	}
}

func readerOf(s string) io.Reader { return &strReader{s: s} }

type strReader struct {
	s string
	i int
}

func (r *strReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
