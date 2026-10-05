// Excerpt written for the oracle test: the shape of weatherHandler in
// mschneider82/mcp-openweather's main.go at e032683, not a copy of it.
package main

import (
	"context"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"
)

func weatherHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	city, units, lang := "Berlin", "c", "en"
	return weather(ctx, city, units, lang)
}
