package main

import (
	"testing"

	"example.com/wrap"
)

func TestRun(t *testing.T) {
	if got := wrap.Run("X"); got == "" {
		t.Fatal("empty result")
	}
}
