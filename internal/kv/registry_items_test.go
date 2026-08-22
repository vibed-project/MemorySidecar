package kv_test

import (
	"context"
	"testing"

	"github.com/vibed-project/mindD/internal/kv"
	kvmem "github.com/vibed-project/mindD/internal/kv/drivers/memory"
)

// noSizeDriver satisfies kv.Driver but does NOT implement a Size method, so
// NamespaceItems must skip its namespace.
type noSizeDriver struct{}

func (noSizeDriver) Get(context.Context, string, string) (kv.Record, error) {
	return kv.Record{}, kv.ErrNotFound
}
func (noSizeDriver) MultiGet(context.Context, string, []string) ([]kv.Record, error) {
	return nil, nil
}
func (noSizeDriver) Put(context.Context, string, string, kv.PutOptions) (kv.Record, error) {
	return kv.Record{}, nil
}
func (noSizeDriver) Delete(context.Context, string, string, kv.DeleteOptions) (bool, error) {
	return false, nil
}
func (noSizeDriver) Scan(context.Context, string, kv.ScanOptions, func(kv.Record) error) error {
	return nil
}
func (noSizeDriver) Close() error { return nil }

func TestRegistryNamespaceItems(t *testing.T) {
	ctx := context.Background()
	reg := kv.NewRegistry()
	mem := kvmem.New(kvmem.Options{SweeperInterval: -1})
	if err := reg.Bind("live", mem); err != nil {
		t.Fatal(err)
	}
	if err := reg.Bind("opaque", noSizeDriver{}); err != nil {
		t.Fatal(err)
	}

	_, _ = mem.Put(ctx, "live", "k1", kv.PutOptions{Value: []byte("v")})
	_, _ = mem.Put(ctx, "live", "k2", kv.PutOptions{Value: []byte("v")})

	items := reg.NamespaceItems(ctx)
	if items["live"] != 2 {
		t.Fatalf("items[live] = %d, want 2 (items=%v)", items["live"], items)
	}
	if _, ok := items["opaque"]; ok {
		t.Fatalf("a driver without Size must be omitted (items=%v)", items)
	}
}
