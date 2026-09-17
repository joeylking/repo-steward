package repo

import "testing"

func TestIsProtected(t *testing.T) {
	cases := map[string]bool{
		"main.go":                   false,
		"main_test.go":              true,
		"pkg/a/b_test.go":           true,
		"testdata/x.txt":            true,
		"pkg/testdata/deep/x.go":    true,
		".github/workflows/ci.yml":  true,
		".github":                   true,
		"go.mod":                    true,
		"go.sum":                    true,
		"vendor/x/y.go":             true,
		"internal/vendor.go":        false,
		"SECURITY.md":               true,
		"docs/SECURITY.md":          false,
		".repo-steward/config.yaml": true,
		"Jenkinsfile":               true,
		"cmd/tool/main.go":          false,
		"mytestdata/x":              false,
		"pkg/test.go":               false,
		"./pkg/../pkg/a_test.go":    true,
	}
	for p, want := range cases {
		if got := IsProtected(p, DefaultProtectedGlobs); got != want {
			t.Errorf("IsProtected(%q) = %v, want %v", p, got, want)
		}
	}
}
