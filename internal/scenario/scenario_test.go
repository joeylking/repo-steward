package scenario

import (
	"strings"
	"testing"
)

func TestOracle_Check(t *testing.T) {
	o := Oracle{
		File:                 "main.go",
		MustContain:          []string{`"city"`},
		MustNotContain:       []string{"Params.Arguments["},
		MustMatch:            []string{`GetArguments\(\)|,\s*ok\s*:?=`},
		MustNotMatch:         []string{`panic\(`},
		NoUncheckedAssertion: []string{"map[string]any", "map[string]interface{}"},
	}
	pass := map[string]string{
		"GetArguments": `package main

func h(r R) {
	args := r.GetArguments()
	city, ok := args["city"].(string)
	_, _ = city, ok
}
`,
		"comma-ok, parenthesized, interface{}": `package main

func h(r R) {
	args, ok := (r.Params.Arguments.(map[string]interface{}))
	if !ok {
		return
	}
	_ = args["city"]
}
`,
		"var comma-ok and a type switch": `package main

func h(r R) {
	var args, ok = r.Params.Arguments.(map[string]any)
	switch v := r.Params.Arguments.(type) {
	case map[string]any:
		_ = v["city"]
	}
	_, _ = args, ok
}
`,
	}
	for name, src := range pass {
		if got := o.Check([]byte(src)); len(got) != 0 {
			t.Errorf("%s: %v", name, got)
		}
	}
	fail := map[string]struct{ src, want string }{
		"unchecked assertion": {`package main

func h(r R) {
	args := r.Params.Arguments.(map[string]any)
	city, ok := args["city"].(string)
	_, _ = city, ok
}
`, "unchecked type assertion to map[string]any at line 4"},
		"unchecked inline": {`package main

func h(r R) {
	city, ok := r.Params.Arguments.(map[string]any)["city"].(string)
	_, _ = city, ok
}
`, "unchecked type assertion to map[string]any at line 4"},
		"old expression": {`package main

func h(r R) {
	city, ok := r.Params.Arguments["city"].(string)
	_, _ = city, ok
}
`, `contains "Params.Arguments["`},
		"not Go": {"city, ok := r.GetArguments()", "does not parse as Go"},
		"reads removed": {`package main

func h(r R) { _ = r.GetArguments() }
`, `missing "\"city\""`},
		"forbidden match": {`package main

func h(r R) { city, ok := r.GetArguments()["city"].(string); _, _ = city, ok; panic("x") }
`, `matches "panic\\("`},
	}
	for name, c := range fail {
		got := strings.Join(o.Check([]byte(c.src)), "\n")
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q does not report %q", name, got, c.want)
		}
	}
	if got := (Oracle{MustMatch: []string{"("}}).Check(nil); len(got) != 1 || !strings.Contains(got[0], "invalid expression") {
		t.Errorf("invalid expression: %v", got)
	}
}
