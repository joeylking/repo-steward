package main

import (
	"testing"

	"example.com/wrapx"
)

func TestShout(t *testing.T) {
	if got := wrapx.Shout("x"); got != "X" {
		t.Fatalf("got %q", got)
	}
}
