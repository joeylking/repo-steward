package main

import "example.com/util"

// clean trims its input using util directly.
func clean(s string) string {
	return util.Trim(s)
}
