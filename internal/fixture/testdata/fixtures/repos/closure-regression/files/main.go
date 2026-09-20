package main

import (
	"fmt"

	"example.com/core"
)

func main() {
	fmt.Println(core.Run("  steward  "), clean("  x  "))
}
