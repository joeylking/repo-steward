// Package vuln is the groundwork for vulnerability-driven remediation: the
// pinned govulncheck build for each toolchain image, a fail-closed parser
// for its JSON output, an offline snapshot of the Go vulnerability
// database with a content identity, and a synthetic database for the
// fixture modules. It runs nothing itself; callers run the scanner in the
// sandbox and hand the captured output to Parse.
//
// The scanner is govulncheck built from golang.org/x/vuln inside each
// toolchain image, because the images do not ship it and containers run
// with GOTOOLCHAIN=local, so each image can build only the releases whose
// go directive it satisfies. internal/vulnscan builds it in the sandbox's
// tool profile through the public module proxy and checksum database, and
// runs it in the execute profile with no network, with the scanner and the
// database mounted read-only.
package vuln

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ScannerModule and ScannerPackage name what is built.
const (
	ScannerModule  = "golang.org/x/vuln"
	ScannerPackage = ScannerModule + "/cmd/govulncheck"
)

// ScannerPin is the govulncheck release built for one toolchain image.
type ScannerPin struct {
	GoMinor string `json:"go_minor"`
	Version string `json:"version"`
	// ModuleSum is the go.sum hash of the golang.org/x/vuln module zip at
	// Version, as recorded in the binary's build information. The build
	// verifies it against sum.golang.org; Check verifies the binary carries it.
	ModuleSum string `json:"module_sum"`
	// ObservedSHA256 is the binary digest observed on linux/arm64 when the
	// table was made. Builds are reproducible for a fixed image, version,
	// and architecture, but the digest differs per architecture, so evidence
	// binds to the digest measured at build time, not to this value.
	ObservedSHA256 string `json:"observed_sha256_linux_arm64"`
}

// Scanners pins, for each Go minor in the toolchain table, the newest
// golang.org/x/vuln release that builds in that image under
// GOTOOLCHAIN=local. Determined on 2026-10-03 by building every release
// from v1.8.0 down in each image pinned in internal/toolchain:
// v1.2.0 through v1.7.0 declare go 1.25.0 and v1.8.0 declares go 1.26.0,
// so images before 1.25 stop at v1.1.4 (go 1.22.0).
var Scanners = []ScannerPin{
	{GoMinor: "1.22", Version: "v1.1.4", ModuleSum: "h1:Ju8QsuyhX3Hk8ma3CesTbO8vfJD9EvUBgHvkxHBzj0I=", ObservedSHA256: "58e04a97a0fe37056f8e64c4cc4ecf4cec023c6eaef05b5ff0b7491f2948de21"},
	{GoMinor: "1.23", Version: "v1.1.4", ModuleSum: "h1:Ju8QsuyhX3Hk8ma3CesTbO8vfJD9EvUBgHvkxHBzj0I=", ObservedSHA256: "6737a2b3a2cac645673984e44c69a2f3e58aaecd43b370f7e8416697f4490a21"},
	{GoMinor: "1.24", Version: "v1.1.4", ModuleSum: "h1:Ju8QsuyhX3Hk8ma3CesTbO8vfJD9EvUBgHvkxHBzj0I=", ObservedSHA256: "d51a40c0ac3ae39d95354355653ee90d0fb642862b6e0aa6c05449705153bdea"},
	{GoMinor: "1.25", Version: "v1.7.0", ModuleSum: "h1:4MQBuhmXbz2uepNJrf3v+aaZLGDqw1JluwYboegA1qg=", ObservedSHA256: "b148ad5a3b5713795d363eb7aa5093eaba629a2a46b180e6852af7f24e765945"},
	{GoMinor: "1.26", Version: "v1.8.0", ModuleSum: "h1:clG4qBU6zH5VKjti8n5j8BBuYzoSha392xXMkXS351U=", ObservedSHA256: "e4e652554b75f81d96c745706c68486cfc7016f042e1fe254abff14e7ccdc571"},
	{GoMinor: "1.27", Version: "v1.8.0", ModuleSum: "h1:clG4qBU6zH5VKjti8n5j8BBuYzoSha392xXMkXS351U=", ObservedSHA256: "2de63175e53875246484794fb483bab3615525650b329e76f6648f2ea9f1e4ee"},
}

// ScannerFor returns the pin for a Go minor such as "1.22".
func ScannerFor(goMinor string) (ScannerPin, error) {
	for _, p := range Scanners {
		if p.GoMinor == goMinor {
			return p, nil
		}
	}
	return ScannerPin{}, fmt.Errorf("vuln: no scanner pinned for go %s", goMinor)
}

// InstallArgv is the tool-profile command that builds the scanner into
// gobin, an absolute path inside the container that the profile mounts
// writable. The tool environment already sets GOTOOLCHAIN=local,
// CGO_ENABLED=0, the public proxy without direct fallback, and the
// checksum database; -trimpath makes the binary independent of container
// paths, so two builds in the same image are byte-identical.
func (p ScannerPin) InstallArgv(gobin string) []string {
	return []string{"env", "GOBIN=" + gobin, "go", "install", "-trimpath", ScannerPackage + "@" + p.Version}
}

// ScanArgv is the execute-profile command that scans the module at the
// working directory. dbURL is a file:// URL of the mounted snapshot,
// such as file:///vulndb. Test files are not analysed: reachability is
// judged from the code that ships. Every pinned release accepts -format
// json; the legacy -json flag is not used.
func ScanArgv(scanner, dbURL string) []string {
	return []string{scanner, "-db", dbURL, "-format", "json", "./..."}
}

// ScannerIdentity is what scan evidence binds to for the scanner.
type ScannerIdentity struct {
	SHA256    string `json:"sha256"`
	Version   string `json:"version"`
	ModuleSum string `json:"module_sum"`
	// GoVersion is the toolchain that built the binary, such as go1.22.12.
	GoVersion string `json:"go_version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
}

// InspectBinary reads the identity of a built scanner on the host: its
// digest and the build information the Go linker embeds, which needs no
// container and works for any target platform.
func InspectBinary(path string) (ScannerIdentity, error) {
	var id ScannerIdentity
	f, err := os.Open(path)
	if err != nil {
		return id, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return id, err
	}
	id.SHA256 = hex.EncodeToString(h.Sum(nil))
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return id, fmt.Errorf("vuln: %s: %w", path, err)
	}
	if bi.Path != ScannerPackage || bi.Main.Path != ScannerModule {
		return id, fmt.Errorf("vuln: %s is %s from %s, not %s", path, bi.Path, bi.Main.Path, ScannerPackage)
	}
	id.Version, id.ModuleSum, id.GoVersion = bi.Main.Version, bi.Main.Sum, bi.GoVersion
	settings := map[string]string{}
	for _, s := range bi.Settings {
		settings[s.Key] = s.Value
	}
	id.GOOS, id.GOARCH = settings["GOOS"], settings["GOARCH"]
	var problems []string
	if settings["-trimpath"] != "true" {
		problems = append(problems, "not built with -trimpath")
	}
	if settings["CGO_ENABLED"] != "0" {
		problems = append(problems, "not built with CGO_ENABLED=0")
	}
	if bi.Main.Replace != nil {
		problems = append(problems, "main module is replaced")
	}
	for _, d := range bi.Deps {
		if d.Replace != nil {
			problems = append(problems, "dependency "+d.Path+" is replaced")
		}
	}
	if len(problems) > 0 {
		return id, fmt.Errorf("vuln: %s: %s", path, strings.Join(problems, "; "))
	}
	return id, nil
}

// ErrScannerMismatch means a binary is not the pinned scanner.
var ErrScannerMismatch = errors.New("vuln: scanner binary does not match its pin")

// Check verifies a binary's identity against the pin and the image's Go
// version as reported by go version inside it, such as "go1.22.12".
func (p ScannerPin) Check(id ScannerIdentity, imageGoVersion string) error {
	var problems []string
	if id.Version != p.Version {
		problems = append(problems, fmt.Sprintf("version %s, pinned %s", id.Version, p.Version))
	}
	if id.ModuleSum != p.ModuleSum {
		problems = append(problems, fmt.Sprintf("module sum %s, pinned %s", id.ModuleSum, p.ModuleSum))
	}
	if id.GoVersion != imageGoVersion {
		problems = append(problems, fmt.Sprintf("built by %s, image has %s", id.GoVersion, imageGoVersion))
	}
	if !strings.HasPrefix(id.GoVersion, "go"+p.GoMinor+".") && id.GoVersion != "go"+p.GoMinor {
		problems = append(problems, fmt.Sprintf("built by %s for a go %s pin", id.GoVersion, p.GoMinor))
	}
	if id.GOOS != "linux" {
		problems = append(problems, "GOOS "+id.GOOS)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrScannerMismatch, strings.Join(problems, "; "))
	}
	return nil
}
