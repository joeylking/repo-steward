// Package needsgo is a fixture dependency whose next version requires a
// newer Go toolchain than the pinned image provides.
package needsgo

// Value returns a constant.
func Value() int { return 1 }
