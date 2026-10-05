// Excerpt written for the oracle test: the shape of two parameter helpers
// in awsoremod/mcp's pkg/aw/server.go at eda957c, not a copy of it.
package aw

import (
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func requiredParam[T comparable](r mcp.CallToolRequest, p string) (T, error) {
	var zero T
	if _, ok := r.Params.Arguments[p]; !ok {
		return zero, fmt.Errorf("missing required parameter: %s", p)
	}
	if _, ok := r.Params.Arguments[p].(T); !ok {
		return zero, fmt.Errorf("parameter %s is not of type %T", p, zero)
	}
	if r.Params.Arguments[p].(T) == zero {
		return zero, fmt.Errorf("missing required parameter: %s", p)
	}
	return r.Params.Arguments[p].(T), nil
}

func OptionalParam[T any](r mcp.CallToolRequest, p string) (T, error) {
	var zero T
	if _, ok := r.Params.Arguments[p]; !ok {
		return zero, nil
	}
	if _, ok := r.Params.Arguments[p].(T); !ok {
		return zero, fmt.Errorf("parameter %s is not of type %T, is %T", p, zero, r.Params.Arguments[p])
	}
	return r.Params.Arguments[p].(T), nil
}
