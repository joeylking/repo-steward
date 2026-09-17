package main

import (
	"fmt"

	"example.com/toolkit/strutil"
)

// shout wraps the library call so tests do not depend on its location.
func shout(s string) string {
	return strutil.Upper(s) + "!"
}

func main() {
	fmt.Println(shout("steward"))
}
