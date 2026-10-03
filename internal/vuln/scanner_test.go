package vuln

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeylking/repo-steward/internal/toolchain"
)

// Every toolchain image has a scanner, and the table has nothing else.
func TestScanners_CoverToolchainTable(t *testing.T) {
	if len(Scanners) != len(toolchain.Table) {
		t.Fatalf("%d pins for %d images", len(Scanners), len(toolchain.Table))
	}
	for _, img := range toolchain.Table {
		p, err := ScannerFor(img.GoMinor)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(p.ModuleSum, "h1:") || len(p.ObservedSHA256) != 64 {
			t.Fatalf("pin %+v", p)
		}
	}
	if _, err := ScannerFor("1.21"); err == nil {
		t.Fatal("pin for an unsupported minor")
	}
}

func TestArgv(t *testing.T) {
	p, _ := ScannerFor("1.22")
	if got := strings.Join(p.InstallArgv("/tools/x"), " "); got != "env GOBIN=/tools/x go install -trimpath golang.org/x/vuln/cmd/govulncheck@v1.1.4" {
		t.Fatalf("install = %s", got)
	}
	if got := strings.Join(ScanArgv("/tools/govulncheck", "file:///vulndb"), " "); got != "/tools/govulncheck -db file:///vulndb -format json ./..." {
		t.Fatalf("scan = %s", got)
	}
}

func TestInspectBinary_RefusesOtherPrograms(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBinary(exe); err == nil || !strings.Contains(err.Error(), "not golang.org/x/vuln/cmd/govulncheck") {
		t.Fatalf("err = %v", err)
	}
	notGo := filepath.Join(t.TempDir(), "script")
	os.WriteFile(notGo, []byte("#!/bin/sh\n"), 0o755)
	if _, err := InspectBinary(notGo); err == nil {
		t.Fatal("inspected a non-Go file")
	}
}

// VULN_SCANNER_DIR points at a directory of built scanners named
// go<minor>-<version>/govulncheck, as the manual pin procedure leaves them.
// Without it the test only checks the pin logic.
func TestInspectBinary_BuiltScanners(t *testing.T) {
	dir := os.Getenv("VULN_SCANNER_DIR")
	if dir == "" {
		t.Skip("VULN_SCANNER_DIR not set")
	}
	images := map[string]string{"1.22": "go1.22.12", "1.23": "go1.23.12", "1.24": "go1.24.13", "1.25": "go1.25.14", "1.26": "go1.26.8", "1.27": "go1.27.1"}
	for _, p := range Scanners {
		id, err := InspectBinary(filepath.Join(dir, "go"+p.GoMinor+"-"+p.Version, "govulncheck"))
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Check(id, images[p.GoMinor]); err != nil {
			t.Fatal(err)
		}
		if id.GOARCH == "arm64" && id.SHA256 != p.ObservedSHA256 {
			t.Errorf("go%s: sha256 %s, observed %s", p.GoMinor, id.SHA256, p.ObservedSHA256)
		}
	}
}

func TestCheck_Mismatches(t *testing.T) {
	p, _ := ScannerFor("1.27")
	good := ScannerIdentity{Version: p.Version, ModuleSum: p.ModuleSum, GoVersion: "go1.27.1", GOOS: "linux", GOARCH: "arm64"}
	if err := p.Check(good, "go1.27.1"); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*ScannerIdentity, *string){
		"version":       func(id *ScannerIdentity, _ *string) { id.Version = "v1.7.0" },
		"module sum":    func(id *ScannerIdentity, _ *string) { id.ModuleSum = "h1:x" },
		"other image":   func(_ *ScannerIdentity, img *string) { *img = "go1.27.2" },
		"other minor":   func(id *ScannerIdentity, img *string) { id.GoVersion, *img = "go1.26.8", "go1.26.8" },
		"not for linux": func(id *ScannerIdentity, _ *string) { id.GOOS = "darwin" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			id, img := good, "go1.27.1"
			mutate(&id, &img)
			if err := p.Check(id, img); !errors.Is(err, ErrScannerMismatch) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
