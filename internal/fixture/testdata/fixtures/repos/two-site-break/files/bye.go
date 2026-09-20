package main

import "example.com/lib"

// farewell is a second call site in a second file.
func farewell(name string) string {
	return "bye, " + lib.Greet(name)
}
