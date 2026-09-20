// Package core v1.1.0 requires util v0.2.0, which renamed Trim. Its own
// API is unchanged; the closure bump is what breaks direct users of util.
package core

import "example.com/util"

// Run trims its input.
func Run(s string) string { return util.TrimSpace(s) }
