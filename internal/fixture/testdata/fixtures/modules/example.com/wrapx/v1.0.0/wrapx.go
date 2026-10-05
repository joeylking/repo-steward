// Package wrapx is a fixture dependency whose root package does not use
// example.com/inner; only its extra package does.
package wrapx

import "strings"

// Shout upper-cases s.
func Shout(s string) string { return strings.ToUpper(s) }
