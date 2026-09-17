package main

import "testing"

func TestShout(t *testing.T) {
	if got := shout("x"); got != "X!" {
		t.Fatalf("shout = %q", got)
	}
}
