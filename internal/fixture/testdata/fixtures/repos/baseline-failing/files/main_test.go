package main

import (
	"testing"

	"example.com/lib"
)

func TestGreet(t *testing.T) {
	if got := lib.Greet("x"); got != "goodbye, x" {
		t.Fatalf("greeting = %q", got) // fails at the baseline on purpose
	}
}
