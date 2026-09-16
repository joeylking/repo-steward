// Package toolchain pins the container image used to validate a repository,
// selected from the repository's go directive. Images are pinned by digest;
// the table is updated deliberately, never resolved at run time.
//
// The host compiler that builds repo-steward is unrelated to this table.
package toolchain

import (
	"fmt"
	"strings"
)

// Image is one pinned toolchain image.
type Image struct {
	GoMinor string `json:"go_minor"`
	Tag     string `json:"tag"`
	Digest  string `json:"digest"`
}

// Ref returns the digest-pinned reference used to run containers.
func (i Image) Ref() string { return "golang@" + i.Digest }

// Table lists supported Go minor versions. Digests were resolved from Docker
// Hub on 2026-09-16 for the linux/arm64 and linux/amd64 manifest lists.
var Table = []Image{
	{GoMinor: "1.22", Tag: "golang:1.22-bookworm", Digest: "sha256:3d699e4d15d0f8f13c9195c0632a16702b8cbdece2955af1c23b37ae5d55a253"},
	{GoMinor: "1.23", Tag: "golang:1.23-bookworm", Digest: "sha256:167053a2bb901972bf2c1611f8f52c44d5fe7e762e5cab213708d82c421614db"},
	{GoMinor: "1.24", Tag: "golang:1.24-bookworm", Digest: "sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac"},
	{GoMinor: "1.25", Tag: "golang:1.25-bookworm", Digest: "sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437"},
	{GoMinor: "1.26", Tag: "golang:1.26-bookworm", Digest: "sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81"},
	{GoMinor: "1.27", Tag: "golang:1.27-bookworm", Digest: "sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b"},
}

// Minor reduces a go directive such as "1.22.5" or "1.22" to "1.22".
func Minor(goDirective string) (string, error) {
	parts := strings.Split(strings.TrimSpace(goDirective), ".")
	if len(parts) < 2 || parts[0] != "1" {
		return "", fmt.Errorf("toolchain: unsupported go directive %q", goDirective)
	}
	for _, p := range parts[:2] {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return "", fmt.Errorf("toolchain: unsupported go directive %q", goDirective)
		}
	}
	return parts[0] + "." + parts[1], nil
}

// ForGoDirective returns the pinned image for a go directive.
func ForGoDirective(goDirective string) (Image, error) {
	minor, err := Minor(goDirective)
	if err != nil {
		return Image{}, err
	}
	for _, img := range Table {
		if img.GoMinor == minor {
			return img, nil
		}
	}
	return Image{}, fmt.Errorf("toolchain: no pinned image for go %s", minor)
}
