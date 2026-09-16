package toolchain

import "testing"

func TestForGoDirective(t *testing.T) {
	for _, in := range []string{"1.22", "1.22.5", " 1.22.0 "} {
		img, err := ForGoDirective(in)
		if err != nil || img.GoMinor != "1.22" || img.Ref() != "golang@"+img.Digest {
			t.Fatalf("%q: %+v %v", in, img, err)
		}
	}
	for _, in := range []string{"", "1", "2.0", "1.21", "1.x", "go1.22"} {
		if _, err := ForGoDirective(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
	seen := map[string]bool{}
	for _, img := range Table {
		if seen[img.GoMinor] || len(img.Digest) != len("sha256:")+64 {
			t.Fatalf("bad table entry %+v", img)
		}
		seen[img.GoMinor] = true
	}
}
