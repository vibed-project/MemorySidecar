// Package memory implements an in-memory semantic driver that brute-forces
// cosine similarity over every stored vector. Suitable for tests and small
// dev workloads; production should use pgvector or a dedicated vector DB.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"memsidecar/internal/semantic"
)

// Options configures a Driver.
type Options struct {
	Dimensions int
	NowFunc    func() time.Time
	NewID      func() string
}

// Driver is an in-memory semantic driver. Safe for concurrent use.
type Driver struct {
	mu         sync.RWMutex
	dim        int
	byID       map[string]*semantic.Record
	order      []string // insertion order; not stable across Delete
	now        func() time.Time
	newID      func() string
	normalised bool // vectors are stored already L2-normalised
}

// New builds a Driver.
func New(opts Options) (*Driver, error) {
	if opts.Dimensions <= 0 {
		return nil, errors.New("semantic/memory: dimensions must be > 0")
	}
	d := &Driver{
		dim:        opts.Dimensions,
		byID:       make(map[string]*semantic.Record),
		now:        time.Now,
		newID:      randomID,
		normalised: true,
	}
	if opts.NowFunc != nil {
		d.now = opts.NowFunc
	}
	if opts.NewID != nil {
		d.newID = opts.NewID
	}
	return d, nil
}

func (d *Driver) Close() error { return nil }

// Size returns the number of records held in the namespace, including
// soft-deleted tombstones and invalidated rows that still occupy memory.
func (d *Driver) Size(_ context.Context) (int64, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return int64(len(d.byID)), nil
}

func (d *Driver) Dimensions() int { return d.dim }

func (d *Driver) Upsert(_ context.Context, records []semantic.Record) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// U4: validate every if_version precondition before mutating, so a failing
	// CAS leaves the whole batch unapplied (parity with the pg tx).
	for i := range records {
		r := records[i]
		if r.IfVersion == nil {
			continue
		}
		var current uint64
		if r.ID != "" {
			if existing, ok := d.byID[r.ID]; ok {
				current = existing.Version
			}
		}
		if *r.IfVersion != current {
			return semantic.ErrVersionMismatch
		}
	}

	for i := range records {
		r := records[i]
		if len(r.Vector) != d.dim {
			return fmt.Errorf("semantic/memory: vector dim %d != namespace dim %d", len(r.Vector), d.dim)
		}
		if r.ID == "" {
			r.ID = d.newID()
		}
		existing, exists := d.byID[r.ID]
		var current uint64
		if exists {
			current = existing.Version
		}
		if r.CreatedAt.IsZero() {
			r.CreatedAt = d.now().UTC()
		}
		if r.ValidFrom.IsZero() {
			r.ValidFrom = d.now().UTC()
		}
		r.Version = current + 1
		stored := r // copy
		stored.IfVersion = nil
		stored.Vector = normalised(r.Vector)
		stored.Payload = cloneBytes(r.Payload)
		stored.Metadata = cloneMeta(r.Metadata)
		stored.Supersedes = cloneStrings(r.Supersedes)
		if !exists {
			d.order = append(d.order, stored.ID)
		}
		d.byID[stored.ID] = &stored
		// Reflect server-assigned id and new version back to the caller.
		records[i].ID = stored.ID
		records[i].Version = stored.Version

		// U2 (ADR-0003): bind a later fact to what it revises — invalidate each
		// named live record as of this record's valid_from. Localized: touches
		// only the named ids; self-references are ignored.
		for _, sid := range stored.Supersedes {
			if sid == stored.ID {
				continue
			}
			target, ok := d.byID[sid]
			if !ok || !target.DeletedAt.IsZero() {
				continue
			}
			if target.ValidTo.IsZero() || target.ValidTo.After(stored.ValidFrom) {
				target.ValidTo = stored.ValidFrom
			}
		}
	}
	return nil
}

// Expire applies opts.Action to at most opts.MaxRows records matching
// opts.Filter (ADR-0003, U3). Iterates in insertion order for determinism.
func (d *Driver) Expire(_ context.Context, opts semantic.ExpireOptions) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now().UTC()
	var affected uint64
	var hardDeleted bool
	for _, id := range d.order {
		if affected >= uint64(opts.MaxRows) {
			break
		}
		r, ok := d.byID[id]
		if !ok || !matchesFilter(r.Metadata, opts.Filter) {
			continue
		}
		switch opts.Action {
		case semantic.ExpireInvalidate:
			if r.DeletedAt.IsZero() && (r.ValidTo.IsZero() || r.ValidTo.After(now)) {
				r.ValidTo = now
				affected++
			}
		case semantic.ExpireSoftDelete:
			if r.DeletedAt.IsZero() {
				r.DeletedAt = now
				affected++
			}
		case semantic.ExpireHardDelete:
			delete(d.byID, id)
			hardDeleted = true
			affected++
		default:
			return 0, fmt.Errorf("semantic/memory: unknown expire action %d", opts.Action)
		}
	}
	if hardDeleted {
		kept := d.order[:0]
		for _, id := range d.order {
			if _, ok := d.byID[id]; ok {
				kept = append(kept, id)
			}
		}
		d.order = kept
	}
	return affected, nil
}

func (d *Driver) Search(_ context.Context, opts semantic.SearchOptions) ([]semantic.Hit, error) {
	if len(opts.QueryVector) != d.dim {
		return nil, fmt.Errorf("semantic/memory: query dim %d != namespace dim %d", len(opts.QueryVector), d.dim)
	}
	q := normalised(opts.QueryVector)

	asOf := opts.AsOf
	if asOf.IsZero() {
		asOf = d.now()
	}

	d.mu.RLock()
	hits := make([]semantic.Hit, 0, len(d.byID))
	for _, r := range d.byID {
		if !matchesFilter(r.Metadata, opts.Filter) {
			continue
		}
		if !matchesPredicates(r.Metadata, opts.Predicates) {
			continue
		}
		if !opts.IncludeInvalidated && !liveAt(r, asOf) {
			continue
		}
		hits = append(hits, semantic.Hit{
			Record: shallowCopyForResponse(*r, opts),
			Score:  cosine(q, r.Vector),
		})
	}
	d.mu.RUnlock()

	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	k := int(opts.TopK)
	if k > 0 && k < len(hits) {
		hits = hits[:k]
	}
	return hits, nil
}

func (d *Driver) Delete(_ context.Context, id string, opts semantic.DeleteOptions) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.byID[id]
	if !ok {
		return false, nil
	}
	if !opts.Hard {
		// Soft delete: tombstone a live row; already-retracted rows are a no-op.
		if !r.DeletedAt.IsZero() {
			return false, nil
		}
		r.DeletedAt = d.now().UTC()
		return true, nil
	}
	delete(d.byID, id)
	for i, o := range d.order {
		if o == id {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
	return true, nil
}

func matchesFilter(meta, filter map[string]string) bool {
	for k, v := range filter {
		if meta[k] != v {
			return false
		}
	}
	return true
}

func matchesPredicates(meta map[string]string, preds []semantic.FieldPredicate) bool {
	for _, p := range preds {
		if !p.Matches(meta) {
			return false
		}
	}
	return true
}

// liveAt reports whether a record is valid and not tombstoned at instant asOf:
// not soft-deleted, valid_from <= asOf, and valid_to unset or > asOf.
func liveAt(r *semantic.Record, asOf time.Time) bool {
	if !r.DeletedAt.IsZero() {
		return false
	}
	if !r.ValidFrom.IsZero() && r.ValidFrom.After(asOf) {
		return false
	}
	if !r.ValidTo.IsZero() && !r.ValidTo.After(asOf) {
		return false
	}
	return true
}

func shallowCopyForResponse(r semantic.Record, opts semantic.SearchOptions) semantic.Record {
	if opts.IDsOnly {
		return semantic.Record{ID: r.ID}
	}
	out := r
	if !opts.IncludePayload {
		out.Payload = nil
	}
	if !opts.IncludeVector {
		out.Vector = nil
	} else {
		v := make([]float32, len(r.Vector))
		copy(v, r.Vector)
		out.Vector = v
	}
	out.Metadata = cloneMeta(r.Metadata)
	out.Supersedes = cloneStrings(r.Supersedes)
	return out
}

// cosine returns the cosine similarity of two equal-length vectors. Inputs
// MUST already be L2-normalised (the driver normalises on write and search).
func cosine(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func normalised(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	var sum float64
	for _, x := range out {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return out
	}
	n := float32(math.Sqrt(sum))
	for i := range out {
		out[i] /= n
	}
	return out
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
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

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
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
