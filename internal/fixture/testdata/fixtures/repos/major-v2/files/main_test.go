package main

import "testing"

func TestTotal(t *testing.T) {
	if total() != 22 {
		t.Fatalf("total = %d", total())
	}
}
