package redis

import "testing"

func TestLexSuccessor(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"a/", "a0", true},   // '/' (0x2f) -> '0' (0x30)
		{"abc", "abd", true}, // last byte incremented
		{"", "", false},      // empty has no finite successor
		{"\xff", "", false},  // all-0xFF has no successor
		{"a\xff", "b", true}, // carry: trailing 0xFF dropped, prior byte bumped
	}
	for _, c := range cases {
		got, ok := lexSuccessor(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("lexSuccessor(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLexRange(t *testing.T) {
	cases := []struct {
		prefix, startAfter string
		lo, hi             string
	}{
		// No prefix, no cursor: whole set.
		{"", "", "[", "+"},
		// Prefix only: inclusive start, exclusive successor upper bound.
		{"a/", "", "[a/", "(a0"},
		// Cursor within the prefix: exclusive lower bound, prefix upper bound.
		{"a/", "a/2", "(a/2", "(a0"},
		// Cursor before the prefix start: prefix start still binds the lower bound.
		{"a/", "0", "[a/", "(a0"},
		// Cursor with no prefix.
		{"", "m", "(m", "+"},
	}
	for _, c := range cases {
		lo, hi := lexRange(c.prefix, c.startAfter)
		if lo != c.lo || hi != c.hi {
			t.Errorf("lexRange(%q, %q) = (%q, %q), want (%q, %q)",
				c.prefix, c.startAfter, lo, hi, c.lo, c.hi)
		}
	}
}
