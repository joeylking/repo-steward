// Package lib is a fixture dependency. v1.2.4 is a patch release with no
// API changes; scenario S1 upgrades to it.
package lib

// Greet returns a greeting for name.
func Greet(name string) string {
	if name == "" {
		name = "world"
	}
	return "hello, " + name
}
