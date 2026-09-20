// Package legacy is a fixture dependency whose v2 is an incompatible
// release without a go.mod and with a renamed API.
package legacy

// Old returns a fixed value.
func Old() int { return 1 }
