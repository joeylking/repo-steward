package main

import (
	"fmt"

	"example.com/lib"
)

// greeting wraps the library call so tests do not depend on its signature.
func greeting(name string) string {
	return lib.Greet(name)
}

func main() {
	fmt.Println(greeting("steward"), farewell("steward"))
}
