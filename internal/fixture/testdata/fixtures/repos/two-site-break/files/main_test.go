package main

import "testing"

func TestBoth(t *testing.T) {
	if greeting("x") != "hello, x" || farewell("x") != "bye, hello, x" {
		t.Fatal("greeting or farewell")
	}
}
