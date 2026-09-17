// Package lib is a fixture dependency. v1.3.0 changes the Greet signature
// to take a context; scenario S2 upgrades to it and must repair callers.
package lib

import "context"

// Greet returns a greeting for name. It honours ctx cancellation.
func Greet(ctx context.Context, name string) string {
	if err := ctx.Err(); err != nil {
		return ""
	}
	if name == "" {
		name = "world"
	}
	return "hello, " + name
}
