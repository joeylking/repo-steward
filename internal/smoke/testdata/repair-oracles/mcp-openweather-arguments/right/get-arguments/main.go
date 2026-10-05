// Excerpt written for the oracle test: the shape of weatherHandler in
// mschneider82/mcp-openweather's main.go at e032683, not a copy of it.
package main

import (
	"context"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"
)

func weatherHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	city, ok := args["city"].(string)
	if !ok {
		return nil, errors.New("city must be a string")
	}
	units, ok := args["units"].(string)
	if !ok || units == "" {
		units = "c"
	}
	lang, ok := args["lang"].(string)
	if !ok || lang == "" {
		lang = "en"
	}
	return weather(ctx, city, units, lang)
}
