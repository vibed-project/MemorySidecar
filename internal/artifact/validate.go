package artifact

import (
	"fmt"
	"regexp"
	"strings"
)

// tenantSep mirrors auth.tenantSep. Namespaces reaching a driver have already
// been through auth.QualifyNamespace, so with tenant isolation enabled they
// are "<tenant>\x1f<namespace>". Validation has to accept that shape without
// letting either half smuggle in path syntax.
const tenantSep = "\x1f"

// idPattern is the set of characters an artifact id or namespace component may
// contain. It excludes "/" and "\" so a value can never contribute a path
// separator, but note that it does allow "." — see ValidateID for why that is
// not sufficient on its own.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

// ValidateID rejects any artifact id that could escape its namespace directory
// or object prefix.
//
// Drivers build filesystem paths and object keys by joining the id onto a base
// (see the fs driver's paths() and the s3 driver's key()). Historically only
// Put validated the id, so Stat, Open and Delete accepted values like
// "./../../other/secret" and resolved them outside the caller's namespace —
// a capability-scoped caller could read and delete arbitrary files.
//
// The character class alone is not enough: "." and ".." match it and are
// exactly the two values filepath.Join treats as directory traversal, so they
// are rejected explicitly.
func ValidateID(id string) error {
	if id == "" {
		return fmt.Errorf("artifact: id is required")
	}
	if id == "." || id == ".." {
		return fmt.Errorf("artifact: id %q is not a valid artifact id", id)
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("artifact: id must match %s", idPattern.String())
	}
	return nil
}

// ValidateNamespace applies the same rule to a storage namespace, allowing for
// the tenant-qualified "<tenant>\x1f<namespace>" form that QualifyNamespace
// produces. Each side is validated independently; the separator itself is a
// legal filename byte and is left intact so the storage layout still reflects
// the tenant boundary.
func ValidateNamespace(namespace string) error {
	if namespace == "" {
		return fmt.Errorf("artifact: namespace is required")
	}
	parts := strings.Split(namespace, tenantSep)
	if len(parts) > 2 {
		return fmt.Errorf("artifact: namespace has too many tenant separators")
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return fmt.Errorf("artifact: namespace %q is not a valid namespace", namespace)
		}
		if !idPattern.MatchString(p) {
			return fmt.Errorf("artifact: namespace must match %s", idPattern.String())
		}
	}
	return nil
}
