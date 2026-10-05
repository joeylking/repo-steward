// Package inner v1.0.1 is a patch release with no API changes.
package inner

import "strings"

// Do normalizes s.
func Do(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
