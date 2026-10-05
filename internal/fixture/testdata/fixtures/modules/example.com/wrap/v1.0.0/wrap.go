// Package wrap is a fixture dependency that calls example.com/inner, so
// applications reach inner only indirectly.
package wrap

import "example.com/inner"

// Run normalizes s through inner.
func Run(s string) string { return inner.Do(s) }
