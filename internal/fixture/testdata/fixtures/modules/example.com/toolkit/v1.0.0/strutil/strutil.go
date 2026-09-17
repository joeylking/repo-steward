// Package strutil is a fixture package. In v1.1.0 it moves to
// example.com/toolkit/text/strutil.
package strutil

import "strings"

// Upper returns s in upper case.
func Upper(s string) string { return strings.ToUpper(s) }
