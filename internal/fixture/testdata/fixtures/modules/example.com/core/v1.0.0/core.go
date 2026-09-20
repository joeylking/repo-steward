// Package core is a fixture dependency that depends on util.
package core

import "example.com/util"

// Run trims its input.
func Run(s string) string { return util.Trim(s) }
