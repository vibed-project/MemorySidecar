package semantic_test

import (
	"testing"

	"memsidecar/internal/semantic"
)

func TestFieldPredicateValidate(t *testing.T) {
	valid := []semantic.FieldPredicate{
		{Key: "s", Op: semantic.PredEQ, Values: []string{"x"}},
		{Key: "s", Op: semantic.PredNEQ, Values: []string{"x"}},
		{Key: "n", Op: semantic.PredGT, Values: []string{"3.5"}},
		{Key: "n", Op: semantic.PredLTE, Values: []string{"-2"}},
		{Key: "s", Op: semantic.PredIN, Values: []string{"a", "b"}},
	}
	for _, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", p, err)
		}
	}

	invalid := map[string]semantic.FieldPredicate{
		"empty key":   {Key: "", Op: semantic.PredEQ, Values: []string{"x"}},
		"unknown op":  {Key: "s", Op: semantic.PredUnspecified, Values: []string{"x"}},
		"no value":    {Key: "s", Op: semantic.PredEQ, Values: nil},
		"too many":    {Key: "s", Op: semantic.PredEQ, Values: []string{"a", "b"}},
		"non-numeric": {Key: "n", Op: semantic.PredGT, Values: []string{"abc"}},
		"empty IN":    {Key: "s", Op: semantic.PredIN, Values: nil},
	}
	for name, p := range invalid {
		if err := p.Validate(); err == nil {
			t.Errorf("Validate(%s) = nil, want error", name)
		}
	}
}

func TestFieldPredicateMatches(t *testing.T) {
	meta := map[string]string{"status": "open", "priority": "3", "score": "not-a-number"}
	cases := []struct {
		name string
		p    semantic.FieldPredicate
		want bool
	}{
		{"eq hit", semantic.FieldPredicate{Key: "status", Op: semantic.PredEQ, Values: []string{"open"}}, true},
		{"eq miss", semantic.FieldPredicate{Key: "status", Op: semantic.PredEQ, Values: []string{"closed"}}, false},
		{"neq hit", semantic.FieldPredicate{Key: "status", Op: semantic.PredNEQ, Values: []string{"closed"}}, true},
		{"missing key eq", semantic.FieldPredicate{Key: "nope", Op: semantic.PredEQ, Values: []string{"x"}}, false},
		{"missing key neq", semantic.FieldPredicate{Key: "nope", Op: semantic.PredNEQ, Values: []string{"x"}}, false},
		{"in hit", semantic.FieldPredicate{Key: "status", Op: semantic.PredIN, Values: []string{"open", "blocked"}}, true},
		{"in miss", semantic.FieldPredicate{Key: "status", Op: semantic.PredIN, Values: []string{"done"}}, false},
		{"gte hit", semantic.FieldPredicate{Key: "priority", Op: semantic.PredGTE, Values: []string{"3"}}, true},
		{"gt miss", semantic.FieldPredicate{Key: "priority", Op: semantic.PredGT, Values: []string{"3"}}, false},
		{"lt hit", semantic.FieldPredicate{Key: "priority", Op: semantic.PredLT, Values: []string{"5"}}, true},
		{"non-numeric stored", semantic.FieldPredicate{Key: "score", Op: semantic.PredGT, Values: []string{"1"}}, false},
	}
	for _, c := range cases {
		if got := c.p.Matches(meta); got != c.want {
			t.Errorf("%s: Matches = %v, want %v", c.name, got, c.want)
		}
	}
}
