// Package lib is a fixture dependency. v1.2.1 is the version fixture
// applications start from.
package lib

// Greet returns a greeting for name.
func Greet(name string) string {
	return "hello, " + name
}
