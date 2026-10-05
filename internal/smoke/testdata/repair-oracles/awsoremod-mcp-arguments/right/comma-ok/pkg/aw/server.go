// Excerpt written for the oracle test: the shape of two parameter helpers
// in awsoremod/mcp's pkg/aw/server.go at eda957c, not a copy of it.
package aw

import (
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func requiredParam[T comparable](r mcp.CallToolRequest, p string) (T, error) {
	var zero T
	args, ok := r.Params.Arguments.(map[string]any)
	if !ok {
		return zero, fmt.Errorf("arguments are not an object")
	}
	if _, ok := args[p]; !ok {
		return zero, fmt.Errorf("missing required parameter: %s", p)
	}
	if _, ok := args[p].(T); !ok {
		return zero, fmt.Errorf("parameter %s is not of type %T", p, zero)
	}
	if args[p].(T) == zero {
		return zero, fmt.Errorf("missing required parameter: %s", p)
	}
	return args[p].(T), nil
}

func OptionalParam[T any](r mcp.CallToolRequest, p string) (T, error) {
	var zero T
	args, ok := r.Params.Arguments.(map[string]any)
	if !ok {
		return zero, fmt.Errorf("arguments are not an object")
	}
	if _, ok := args[p]; !ok {
		return zero, nil
	}
	if _, ok := args[p].(T); !ok {
		return zero, fmt.Errorf("parameter %s is not of type %T, is %T", p, zero, args[p])
	}
	return args[p].(T), nil
}
