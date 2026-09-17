package repo

import (
	"path"
	"strings"
)

// IsProtected reports whether p (slash-separated, repository-relative)
// matches any protected glob. A glob is matched with path.Match against
// the whole path; a leading "**/" also matches at the root, and a trailing
// "/**" matches everything beneath a directory.
func IsProtected(p string, globs []string) bool {
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	for _, g := range globs {
		if matchGlob(g, p) {
			return true
		}
	}
	return false
}

func matchGlob(g, p string) bool {
	switch {
	case strings.HasSuffix(g, "/**"):
		dir := strings.TrimSuffix(g, "/**")
		if strings.HasPrefix(dir, "**/") {
			base := strings.TrimPrefix(dir, "**/")
			for _, seg := range prefixes(p) {
				if ok, _ := path.Match(base, seg); ok {
					return true
				}
			}
			return false
		}
		return p == dir || strings.HasPrefix(p, dir+"/")
	case strings.HasPrefix(g, "**/"):
		base := strings.TrimPrefix(g, "**/")
		if ok, _ := path.Match(base, p); ok {
			return true
		}
		ok, _ := path.Match(base, path.Base(p))
		return ok && !strings.Contains(base, "/")
	default:
		ok, _ := path.Match(g, p)
		return ok
	}
}

// prefixes returns every directory component of p, e.g. a/b/c.go -> a, b.
func prefixes(p string) []string {
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return nil
	}
	return parts[:len(parts)-1]
}
