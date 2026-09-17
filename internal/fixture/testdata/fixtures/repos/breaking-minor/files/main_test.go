package main

import "testing"

func TestGreeting(t *testing.T) {
	if got := greeting("x"); got != "hello, x" {
		t.Fatalf("greeting = %q", got)
	}
}
