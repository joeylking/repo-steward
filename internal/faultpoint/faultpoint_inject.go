//go:build faultinject

package faultpoint

import (
	"fmt"
	"os"
)

// Enabled reports whether fault injection is compiled in.
const Enabled = true

// EnvVar names the fault point at which the process exits.
const EnvVar = "REPO_STEWARD_FAULT"

// Hit exits the process with status 3 when name matches EnvVar, simulating
// a crash between two persisted operations.
func Hit(name string) {
	if os.Getenv(EnvVar) == name {
		fmt.Fprintf(os.Stderr, "faultpoint: crashing at %s\n", name)
		os.Exit(3)
	}
}
