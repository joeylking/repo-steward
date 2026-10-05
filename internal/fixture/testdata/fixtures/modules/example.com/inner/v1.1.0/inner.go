// Package inner v1.1.0 is a minor release with no API changes.
package inner

import "strings"

// Do normalizes s.
func Do(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Version names the release.
const Version = "v1.1.0"
