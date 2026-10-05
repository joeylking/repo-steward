// Excerpt written for the oracle test: the shape of the GenerateRequest in
// allof-dev/dictionary's cmd/translate/main.go at 192b5dc, not a copy of it.
package main

import (
	"context"
	"encoding/json"

	"github.com/ollama/ollama/api"
)

var _ = json.Valid

func translate(ctx context.Context, c *api.Client, b []byte, fn api.GenerateResponseFunc) error {
	F := false
	return c.Generate(ctx, &api.GenerateRequest{
		Model:  "gemma2:9b",
		System: "Translate the values of the given JSON.",
		Format: json.RawMessage(`"json"`),
		Prompt: string(b),
		Stream: &F,
	}, fn)
}
