// Package util is a fixture dependency pulled up transitively by core.
package util

import "strings"

// Trim removes surrounding whitespace.
func Trim(s string) string { return strings.TrimSpace(s) }
