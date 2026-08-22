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

	"github.com/vibed-project/mindD/internal/semantic"
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
			if existing, ok := d.byID[ck(r.Tenant, r.ID)]; ok {
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
		existing, exists := d.byID[ck(r.Tenant, r.ID)]
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
			d.order = append(d.order, ck(stored.Tenant, stored.ID))
		}
		d.byID[ck(stored.Tenant, stored.ID)] = &stored
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
			target, ok := d.byID[ck(stored.Tenant, sid)]
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
		if !ok || r.Tenant != opts.Tenant || !matchesFilter(r.Metadata, opts.Filter) {
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

// candidate is a filter-passing record with its per-lane relevance signals and
// its response copy (snapshotted under the lock to avoid holding map pointers).
type candidate struct {
	rec     semantic.Record // response copy (already stripped per opts)
	cosine  float32
	overlap int
}

func (d *Driver) Search(_ context.Context, opts semantic.SearchOptions) ([]semantic.Hit, error) {
	dense := opts.Mode == semantic.ModeDense || opts.Mode == semantic.ModeHybrid
	sparse := opts.Mode == semantic.ModeSparse || opts.Mode == semantic.ModeHybrid
	if dense && len(opts.QueryVector) != d.dim {
		return nil, fmt.Errorf("semantic/memory: query dim %d != namespace dim %d", len(opts.QueryVector), d.dim)
	}

	var q []float32
	if dense {
		q = normalised(opts.QueryVector)
	}
	var queryTerms map[string]struct{}
	if sparse {
		queryTerms = semantic.Tokenize(opts.QueryText)
	}

	asOf := opts.AsOf
	if asOf.IsZero() {
		asOf = d.now()
	}

	d.mu.RLock()
	cands := make([]candidate, 0, len(d.byID))
	for _, r := range d.byID {
		if r.Tenant != opts.Tenant {
			continue // storage isolation: only this tenant's records
		}
		if !matchesFilter(r.Metadata, opts.Filter) {
			continue
		}
		if !matchesPredicates(r.Metadata, opts.Predicates) {
			continue
		}
		if !opts.CreatedAfter.IsZero() && !r.CreatedAt.After(opts.CreatedAfter) {
			continue // created_at <= CreatedAfter → excluded (exclusive lower)
		}
		if !opts.CreatedBefore.IsZero() && !r.CreatedAt.Before(opts.CreatedBefore) {
			continue // created_at >= CreatedBefore → excluded (exclusive upper)
		}
		if !opts.IncludeInvalidated && !liveAt(r, asOf) {
			continue
		}
		c := candidate{rec: shallowCopyForResponse(*r, opts)}
		if dense {
			c.cosine = cosine(q, r.Vector)
		}
		if sparse {
			c.overlap = semantic.TermOverlap(queryTerms, r.Content)
		}
		cands = append(cands, c)
	}
	d.mu.RUnlock()

	topK := int(opts.TopK)
	switch opts.Mode {
	case semantic.ModeSparse:
		return rankSparse(cands, topK), nil
	case semantic.ModeHybrid:
		return rankHybrid(cands, rerankK(opts), topK), nil
	default:
		return rankDense(cands, topK), nil
	}
}

func rerankK(opts semantic.SearchOptions) int {
	if opts.RerankCandidateK > 0 {
		return int(opts.RerankCandidateK)
	}
	return semantic.DefaultRerankCandidateK
}

// rankDense preserves the original dense behaviour: order by cosine descending,
// then truncate to topK.
func rankDense(cands []candidate, topK int) []semantic.Hit {
	sort.Slice(cands, func(i, j int) bool { return cands[i].cosine > cands[j].cosine })
	if topK > 0 && topK < len(cands) {
		cands = cands[:topK]
	}
	hits := make([]semantic.Hit, len(cands))
	for i, c := range cands {
		hits[i] = semantic.Hit{Record: c.rec, Score: c.cosine}
	}
	return hits
}

// rankSparse keeps only term-overlapping records, ordered by overlap descending
// (ties broken by id for determinism), and scores hits by the overlap count.
func rankSparse(cands []candidate, topK int) []semantic.Hit {
	matched := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if c.overlap > 0 {
			matched = append(matched, c)
		}
	}
	sortByOverlap(matched)
	if topK > 0 && topK < len(matched) {
		matched = matched[:topK]
	}
	hits := make([]semantic.Hit, len(matched))
	for i, c := range matched {
		hits[i] = semantic.Hit{Record: c.rec, Score: float32(c.overlap)}
	}
	return hits
}

// rankHybrid fuses the dense and sparse lanes with Reciprocal Rank Fusion.
func rankHybrid(cands []candidate, rerank, topK int) []semantic.Hit {
	dense := make([]candidate, len(cands))
	copy(dense, cands)
	sort.Slice(dense, func(i, j int) bool {
		if dense[i].cosine != dense[j].cosine {
			return dense[i].cosine > dense[j].cosine
		}
		return dense[i].rec.ID < dense[j].rec.ID
	})
	denseIDs := topIDs(dense, rerank)

	sparse := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if c.overlap > 0 {
			sparse = append(sparse, c)
		}
	}
	sortByOverlap(sparse)
	sparseIDs := topIDs(sparse, rerank)

	fusedIDs, scores := semantic.RRFFuse([][]string{denseIDs, sparseIDs}, topK)
	byID := make(map[string]semantic.Record, len(cands))
	for _, c := range cands {
		byID[c.rec.ID] = c.rec
	}
	hits := make([]semantic.Hit, 0, len(fusedIDs))
	for _, id := range fusedIDs {
		if rec, ok := byID[id]; ok {
			hits = append(hits, semantic.Hit{Record: rec, Score: float32(scores[id])})
		}
	}
	return hits
}

func sortByOverlap(cs []candidate) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].overlap != cs[j].overlap {
			return cs[i].overlap > cs[j].overlap
		}
		return cs[i].rec.ID < cs[j].rec.ID
	})
}

func topIDs(cs []candidate, k int) []string {
	if k > 0 && k < len(cs) {
		cs = cs[:k]
	}
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.rec.ID
	}
	return ids
}

func (d *Driver) Delete(_ context.Context, id string, opts semantic.DeleteOptions) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := ck(opts.Tenant, id)
	r, ok := d.byID[key]
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
	delete(d.byID, key)
	for i, o := range d.order {
		if o == key {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
	return true, nil
}

// ck is the composite storage key: (tenant, id) is a record's identity, so two
// tenants can reuse the same id without colliding.
func ck(tenant, id string) string {
	return tenant + "\x1f" + id
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
