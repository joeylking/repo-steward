// Package extra is the part of wrapx that uses example.com/inner.
package extra

import "example.com/inner"

// Norm normalizes s through inner.
func Norm(s string) string { return inner.Do(s) }
