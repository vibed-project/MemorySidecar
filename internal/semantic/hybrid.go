package semantic

import (
	"sort"
	"strings"
	"unicode"
)

// SearchMode selects the retrieval lanes for a Search (Q4).
type SearchMode int

const (
	ModeDense  SearchMode = iota // vector ANN only (default)
	ModeSparse                   // lexical only
	ModeHybrid                   // Reciprocal Rank Fusion of dense + sparse
)

// DefaultRerankCandidateK is the per-lane candidate depth used when a
// SPARSE/HYBRID request leaves rerank_candidate_k at 0.
const DefaultRerankCandidateK = 50

// rrfK is the Reciprocal Rank Fusion constant. 60 is the value from the
// original RRF paper and the de-facto default.
const rrfK = 60

// RRFFuse fuses per-lane rankings — each a slice of ids, best-first — into one
// ranking by summed reciprocal rank, score(id) = Σ 1/(rrfK + rank). It returns
// the ids ordered by fused score descending (ties broken by id ascending, so
// the result is deterministic across drivers) and the fused scores. topK <= 0
// returns every fused id.
func RRFFuse(lanes [][]string, topK int) ([]string, map[string]float64) {
	scores := make(map[string]float64)
	for _, lane := range lanes {
		for rank, id := range lane {
			scores[id] += 1.0 / float64(rrfK+rank+1) // rank is 0-based
		}
	}
	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if topK > 0 && topK < len(ids) {
		ids = ids[:topK]
	}
	return ids, scores
}

// Tokenize lowercases text and splits on non-alphanumeric runes, returning the
// distinct terms. It backs the in-memory sparse lane's term-overlap scoring —
// a deliberately simple, language-agnostic lexer (the pgvector driver uses
// Postgres full-text search instead).
func Tokenize(text string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, f := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		out[f] = struct{}{}
	}
	return out
}

// TermOverlap counts how many distinct query terms appear in content — the
// in-memory sparse relevance signal.
func TermOverlap(queryTerms map[string]struct{}, content string) int {
	if len(queryTerms) == 0 {
		return 0
	}
	docTerms := Tokenize(content)
	n := 0
	for t := range queryTerms {
		if _, ok := docTerms[t]; ok {
			n++
		}
	}
	return n
}
