// Package util v0.2.0 renames Trim to TrimSpace.
package util

import "strings"

// TrimSpace removes surrounding whitespace.
func TrimSpace(s string) string { return strings.TrimSpace(s) }
