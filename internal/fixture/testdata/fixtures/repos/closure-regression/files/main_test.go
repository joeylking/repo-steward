package main

import "testing"

func TestClean(t *testing.T) {
	if clean("  a ") != "a" {
		t.Fatal("clean")
	}
}
