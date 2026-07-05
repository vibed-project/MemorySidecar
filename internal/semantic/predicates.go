package semantic

import (
	"fmt"
	"strconv"
)

// PredicateOp is the comparison in a FieldPredicate.
type PredicateOp int

const (
	PredUnspecified PredicateOp = iota
	PredEQ
	PredNEQ
	PredGT
	PredGTE
	PredLT
	PredLTE
	PredIN
)

// FieldPredicate filters Search on a single metadata key (Q3). It complements
// the exact-match SearchOptions.Filter map with ranges and set membership.
type FieldPredicate struct {
	Key    string
	Op     PredicateOp
	Values []string
}

// IsNumeric reports whether the op compares values numerically.
func (op PredicateOp) IsNumeric() bool {
	switch op {
	case PredGT, PredGTE, PredLT, PredLTE:
		return true
	}
	return false
}

// Validate checks the predicate is well-formed: known op, non-empty key,
// correct value count, and a numeric value for numeric ops. The error is
// suitable for surfacing as InvalidArgument.
func (p FieldPredicate) Validate() error {
	if p.Key == "" {
		return fmt.Errorf("predicate: empty key")
	}
	switch p.Op {
	case PredEQ, PredNEQ, PredGT, PredGTE, PredLT, PredLTE:
		if len(p.Values) != 1 {
			return fmt.Errorf("predicate %q: op requires exactly one value, got %d", p.Key, len(p.Values))
		}
	case PredIN:
		if len(p.Values) == 0 {
			return fmt.Errorf("predicate %q: IN requires at least one value", p.Key)
		}
	default:
		return fmt.Errorf("predicate %q: unspecified or unknown op", p.Key)
	}
	if p.Op.IsNumeric() {
		if _, err := strconv.ParseFloat(p.Values[0], 64); err != nil {
			return fmt.Errorf("predicate %q: value %q is not numeric", p.Key, p.Values[0])
		}
	}
	return nil
}

// Matches evaluates the predicate against a record's metadata. A missing key
// never matches. For numeric ops a non-numeric stored value never matches
// (rather than erroring), so malformed data can't fail the whole query.
func (p FieldPredicate) Matches(meta map[string]string) bool {
	v, ok := meta[p.Key]
	if !ok {
		return false
	}
	switch p.Op {
	case PredEQ:
		return v == p.Values[0]
	case PredNEQ:
		return v != p.Values[0]
	case PredIN:
		for _, want := range p.Values {
			if v == want {
				return true
			}
		}
		return false
	case PredGT, PredGTE, PredLT, PredLTE:
		fv, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return false
		}
		tv, err := strconv.ParseFloat(p.Values[0], 64)
		if err != nil {
			return false
		}
		switch p.Op {
		case PredGT:
			return fv > tv
		case PredGTE:
			return fv >= tv
		case PredLT:
			return fv < tv
		case PredLTE:
			return fv <= tv
		}
	}
	return false
}
