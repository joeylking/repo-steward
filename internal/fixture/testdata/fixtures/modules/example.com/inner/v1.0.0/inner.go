// Package inner is a fixture dependency that applications reach only
// through another module. v1.0.0 is affected by the indirect-target
// advisory used in tests; v1.0.1 fixes it.
package inner

import "strings"

// Do normalizes s.
func Do(s string) string { return strings.ToLower(s) }
