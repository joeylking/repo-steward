//go:build !faultinject

// Package faultpoint lets crash-recovery tests terminate the process at a
// named point. Without the faultinject build tag every point is a no-op,
// so production binaries carry no fault behaviour.
package faultpoint

// Hit is a no-op in production builds.
func Hit(name string) {}

// Enabled reports whether fault injection is compiled in.
const Enabled = false
