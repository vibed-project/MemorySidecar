package artifact_test

import (
	"testing"

	"github.com/vibed-project/mindD/internal/artifact"
)

func TestValidateID_RejectsTraversal(t *testing.T) {
	// Every one of these used to reach filepath.Join in the fs driver and
	// resolve outside the caller's namespace, because only Put validated ids.
	bad := []string{
		"",
		".",
		"..",
		"./../../other/secret",
		"../../../../etc/passwd",
		"../etc/passwd",
		"a/b",
		`a\b`,
		"/etc/passwd",
		"foo/../bar",
		"\x1ftenant",       // the tenant separator must not be smuggled in
		"a\x00b",           // NUL truncation
		string(make([]byte, 129)), // over the length bound
	}
	for _, id := range bad {
		if err := artifact.ValidateID(id); err == nil {
			t.Errorf("ValidateID(%q) = nil, want an error", id)
		}
	}
}

func TestValidateID_AcceptsOrdinaryIDs(t *testing.T) {
	good := []string{
		"a",
		"ab",
		"report.pdf",
		"a-b_c.d",
		"2026-08-23T12-00-00Z",
		"01HQ8G7Z9K0000000000000000",
		"...", // three dots is a legal filename, unlike "." and ".."
	}
	for _, id := range good {
		if err := artifact.ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q) = %v, want nil", id, err)
		}
	}
}

func TestValidateNamespace(t *testing.T) {
	good := []string{
		"blobs",
		"my-ns",
		"tenant-a\x1fblobs", // the QualifyNamespace form when isolation is on
	}
	for _, ns := range good {
		if err := artifact.ValidateNamespace(ns); err != nil {
			t.Errorf("ValidateNamespace(%q) = %v, want nil", ns, err)
		}
	}

	bad := []string{
		"",
		"..",
		"../other",
		"a/b",
		"\x1fblobs",              // empty tenant half
		"tenant\x1f",             // empty namespace half
		"a\x1fb\x1fc",            // more separators than the format allows
		"tenant\x1f../other",     // traversal hidden in the namespace half
	}
	for _, ns := range bad {
		if err := artifact.ValidateNamespace(ns); err == nil {
			t.Errorf("ValidateNamespace(%q) = nil, want an error", ns)
		}
	}
}
