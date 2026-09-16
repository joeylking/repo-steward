package main

import (
	"testing"

	"example.com/lib"
)

func TestGreet(t *testing.T) {
	if got := lib.Greet("x"); got == "" {
		t.Fatal("empty greeting")
	}
}
