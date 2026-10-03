package vuln

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Output is one captured scanner run. It mirrors the fields of
// sandbox.ExecResult that decide conclusiveness, so parsing needs no engine.
type Output struct {
	Stdout, Stderr  []byte
	ExitCode        int
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
}

// Expect states what the caller knows about the run. Every non-empty field
// must match the scanner's config message, or the scan is inconclusive:
// evidence is bound to the scanner, database, and toolchain it claims.
type Expect struct {
	ScannerVersion string    // such as v1.8.0
	DB             string    // such as file:///vulndb
	DBModified     time.Time // index/db.json modified of the mounted snapshot
	GoVersion      string    // the image's go version, such as go1.27.1
}

// Level is how far the scanner traced a finding. Module means the module
// is required at an affected version; package means a vulnerable package is
// imported; symbol means a vulnerable symbol is reachable by calls from the
// scanned packages.
type Level string

const (
	LevelModule  Level = "module"
	LevelPackage Level = "package"
	LevelSymbol  Level = "symbol"
)

func (l Level) rank() int {
	switch l {
	case LevelModule:
		return 1
	case LevelPackage:
		return 2
	case LevelSymbol:
		return 3
	}
	return 0
}

// Frame is one step of a call trace, as the scanner reports it.
type Frame struct {
	Module   string `json:"module"`
	Version  string `json:"version,omitempty"`
	Package  string `json:"package,omitempty"`
	Function string `json:"function,omitempty"`
	Receiver string `json:"receiver,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// Symbol is the frame's qualified function, such as "lib.Greet" or
// "os.(*File).Read".
func (f Frame) Symbol() string {
	name := f.Function
	if f.Receiver != "" {
		r := f.Receiver
		if strings.HasPrefix(r, "*") {
			r = "(" + r + ")"
		}
		name = r + "." + name
	}
	pkg := f.Package
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// Finding is one advisory affecting one module, merged from every message
// the scanner emitted for that pair.
type Finding struct {
	// Key is "<OSV id> <module>", stable across runs and scanner versions.
	Key     string   `json:"key"`
	OSV     string   `json:"osv"`
	Aliases []string `json:"aliases,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Module  string   `json:"module"`
	// Stdlib is true for the standard library (and the toolchain), whose
	// version is the image's Go version and does not change with a
	// dependency upgrade.
	Stdlib       bool   `json:"stdlib"`
	FoundVersion string `json:"found_version"`
	// FixedVersion is the scanner's fix for the found version; empty means
	// no fix exists.
	FixedVersion string `json:"fixed_version,omitempty"`
	Level        Level  `json:"level"`
	// Packages are the vulnerable packages imported (package and symbol level).
	Packages []string `json:"packages,omitempty"`
	// Symbols are the vulnerable symbols reached (symbol level).
	Symbols []string `json:"symbols,omitempty"`
	// Trace is one representative call path, vulnerable symbol first, as
	// the scanner orders it; the shortest one reported.
	Trace []Frame `json:"trace,omitempty"`
	// Traces counts the symbol-level traces the scanner reported.
	Traces int `json:"traces,omitempty"`
}

// CallPath renders Trace entry point first, for a pull request body:
// "main (main.go:10) -> lib.Greet".
func (f Finding) CallPath() string {
	if len(f.Trace) == 0 {
		return ""
	}
	parts := make([]string, 0, len(f.Trace))
	for i := len(f.Trace) - 1; i >= 0; i-- {
		fr := f.Trace[i]
		s := fr.Symbol()
		if i > 0 && fr.File != "" && fr.Line > 0 {
			s += fmt.Sprintf(" (%s:%d)", fr.File, fr.Line)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " -> ")
}

// Module is one entry of the scanner's dependency listing.
type Module struct {
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}

// Scan is a parsed scanner run.
type Scan struct {
	// Conclusive is true only when the run completed, its output was
	// captured whole and parsed completely, it identified itself as
	// expected, and it reported finishing the analysis. An inconclusive
	// scan says nothing about vulnerabilities, including that there are none.
	Conclusive bool   `json:"conclusive"`
	Reason     string `json:"reason,omitempty"`

	ProtocolVersion string    `json:"protocol_version,omitempty"`
	ScannerName     string    `json:"scanner_name,omitempty"`
	ScannerVersion  string    `json:"scanner_version,omitempty"`
	DB              string    `json:"db,omitempty"`
	DBModified      time.Time `json:"db_modified,omitempty"`
	GoVersion       string    `json:"go_version,omitempty"`
	ScanLevel       string    `json:"scan_level,omitempty"`
	ScanMode        string    `json:"scan_mode,omitempty"`

	Roots   []string `json:"roots,omitempty"`
	Modules []Module `json:"modules,omitempty"`
	// Findings are sorted by Key.
	Findings []Finding `json:"findings"`
	// Notes record oddities that did not make the scan inconclusive.
	Notes []string `json:"notes,omitempty"`
}

// Finding returns the finding for an advisory and module.
func (s *Scan) Finding(osv, module string) (Finding, bool) {
	key := osv + " " + module
	i := sort.Search(len(s.Findings), func(i int) bool { return s.Findings[i].Key >= key })
	if i < len(s.Findings) && s.Findings[i].Key == key {
		return s.Findings[i], true
	}
	return Finding{}, false
}

// The wire format: a stream of indented JSON objects, each with exactly
// one of the keys config, progress, SBOM, osv, finding. The schema is
// identical in v1.1.4, v1.7.0, and v1.8.0 (internal/govulncheck in
// golang.org/x/vuln, protocol v1.0.0).
type message struct {
	Config   *wireConfig
	Progress *wireProgress
	SBOM     *wireSBOM
	OSV      *wireOSV
	Finding  *wireFinding
}

type wireConfig struct {
	ProtocolVersion string     `json:"protocol_version"`
	ScannerName     string     `json:"scanner_name"`
	ScannerVersion  string     `json:"scanner_version"`
	DB              string     `json:"db"`
	DBLastModified  *time.Time `json:"db_last_modified"`
	GoVersion       string     `json:"go_version"`
	ScanLevel       string     `json:"scan_level"`
	ScanMode        string     `json:"scan_mode"`
}

type wireProgress struct {
	Message string `json:"message"`
}

type wireSBOM struct {
	GoVersion string   `json:"go_version"`
	Modules   []Module `json:"modules"`
	Roots     []string `json:"roots"`
}

// wireOSV keeps only what a finding needs; the entry is otherwise opaque.
type wireOSV struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases"`
	Summary string   `json:"summary"`
}

type wireFinding struct {
	OSV          string       `json:"osv"`
	FixedVersion string       `json:"fixed_version"`
	Trace        []*wireFrame `json:"trace"`
}

type wireFrame struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Package  string `json:"package"`
	Function string `json:"function"`
	Receiver string `json:"receiver"`
	Position *struct {
		Filename string `json:"filename"`
		Line     int    `json:"line"`
	} `json:"position"`
}

// progressDone is the last progress message of a completed source scan in
// every pinned release. Its presence is the only positive sign that the
// analysis ran: v1.1.4 exits 0 after printing only its config when the
// package patterns match nothing.
const progressDone = "Checking the code against the vulnerabilities..."

// StdlibModule is the module path the scanner uses for the standard library.
const StdlibModule = "stdlib"

// Parse turns one scanner run into a Scan. It never returns an error: every
// failure is an inconclusive Scan with a reason.
func Parse(out Output, want Expect) *Scan {
	s := &Scan{Findings: []Finding{}}
	fail := func(format string, args ...any) *Scan {
		s.Conclusive = false
		s.Reason = fmt.Sprintf(format, args...)
		s.Findings = []Finding{}
		return s
	}
	switch {
	case out.TimedOut:
		return fail("scanner timed out")
	case out.StdoutTruncated || out.StderrTruncated:
		return fail("scanner output exceeded the capture limit")
	case out.ExitCode != 0:
		// In JSON mode the scanner exits 0 whether or not it finds
		// anything; any other status is a failure to scan.
		return fail("scanner exited %d: %s", out.ExitCode, firstLines(out.Stderr, 3))
	}
	if len(bytes.TrimSpace(out.Stderr)) > 0 {
		s.Notes = append(s.Notes, "stderr: "+firstLines(out.Stderr, 3))
	}

	dec := json.NewDecoder(bytes.NewReader(out.Stdout))
	var (
		sawConfig, sawSBOM, sawDone bool
		osvs                        = map[string]*wireOSV{}
		byKey                       = map[string]*Finding{}
		bestTrace                   = map[string][]Frame{}
		sbomVersions                = map[string]string{}
		index                       int
	)
	for {
		var raw map[string]json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		index++
		if err != nil {
			return fail("message %d: malformed JSON: %v", index, err)
		}
		// Each message has exactly one known key. An unknown kind is schema
		// drift in a pinned scanner, so it fails the scan rather than being
		// skipped.
		if len(raw) != 1 {
			return fail("message %d carries %d keys, want exactly one", index, len(raw))
		}
		var m message
		for kind, body := range raw {
			var target any
			switch kind {
			case "config":
				m.Config = new(wireConfig)
				target = m.Config
			case "progress":
				m.Progress = new(wireProgress)
				target = m.Progress
			case "SBOM":
				m.SBOM = new(wireSBOM)
				target = m.SBOM
			case "osv":
				m.OSV = new(wireOSV)
				target = m.OSV
			case "finding":
				m.Finding = new(wireFinding)
				target = m.Finding
			default:
				return fail("message %d: unknown kind %q", index, kind)
			}
			if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
				return fail("message %d: %s is null", index, kind)
			}
			if err := json.Unmarshal(body, target); err != nil {
				return fail("message %d: %s: %v", index, kind, err)
			}
		}
		if index == 1 && m.Config == nil {
			return fail("first message is not the scanner config")
		}
		switch {
		case m.Config != nil:
			if sawConfig {
				return fail("message %d: second config message", index)
			}
			sawConfig = true
			if reason := s.setConfig(m.Config, want); reason != "" {
				return fail("%s", reason)
			}
		case m.SBOM != nil:
			if sawSBOM {
				return fail("message %d: second SBOM message", index)
			}
			sawSBOM = true
			if m.SBOM.GoVersion != s.GoVersion {
				return fail("SBOM go version %q differs from config %q", m.SBOM.GoVersion, s.GoVersion)
			}
			s.Modules = m.SBOM.Modules
			s.Roots = append([]string(nil), m.SBOM.Roots...)
			sort.Strings(s.Roots)
			for _, mod := range m.SBOM.Modules {
				if mod.Path == "" {
					return fail("SBOM lists a module with no path")
				}
				sbomVersions[mod.Path] = mod.Version
			}
		case m.Progress != nil:
			if m.Progress.Message == progressDone {
				sawDone = true
			}
		case m.OSV != nil:
			if m.OSV.ID == "" {
				return fail("message %d: OSV entry with no id", index)
			}
			osvs[m.OSV.ID] = m.OSV
		case m.Finding != nil:
			if reason := s.addFinding(m.Finding, osvs, sbomVersions, byKey, bestTrace); reason != "" {
				return fail("message %d: %s", index, reason)
			}
		}
	}
	switch {
	case !sawConfig:
		return fail("no scanner config message")
	case !sawSBOM:
		return fail("no SBOM message: the scanner did not load the module")
	case len(s.Roots) == 0:
		return fail("SBOM has no root module")
	case !sawDone:
		return fail("the scanner never reported checking the code")
	}
	for key, f := range byKey {
		f.Trace = bestTrace[key]
		sort.Strings(f.Packages)
		sort.Strings(f.Symbols)
		s.Findings = append(s.Findings, *f)
	}
	sort.Slice(s.Findings, func(i, j int) bool { return s.Findings[i].Key < s.Findings[j].Key })
	s.Conclusive = true
	return s
}

func (s *Scan) setConfig(c *wireConfig, want Expect) string {
	s.ProtocolVersion, s.ScannerName, s.ScannerVersion = c.ProtocolVersion, c.ScannerName, c.ScannerVersion
	s.DB, s.GoVersion, s.ScanLevel, s.ScanMode = c.DB, c.GoVersion, c.ScanLevel, c.ScanMode
	if c.DBLastModified != nil {
		s.DBModified = c.DBLastModified.UTC()
	}
	switch {
	case semver.Major(c.ProtocolVersion) != "v1":
		return fmt.Sprintf("unsupported protocol version %q", c.ProtocolVersion)
	case c.ScannerName != "govulncheck":
		return fmt.Sprintf("scanner name %q, want govulncheck", c.ScannerName)
	case c.ScanMode != "source":
		return fmt.Sprintf("scan mode %q, want source", c.ScanMode)
	case c.ScanLevel != "symbol":
		return fmt.Sprintf("scan level %q, want symbol", c.ScanLevel)
	case c.DBLastModified == nil || c.DBLastModified.IsZero():
		return "config has no database modified time"
	case c.GoVersion == "":
		return "config has no go version"
	case want.ScannerVersion != "" && c.ScannerVersion != want.ScannerVersion:
		return fmt.Sprintf("scanner version %q, want %q", c.ScannerVersion, want.ScannerVersion)
	case want.DB != "" && c.DB != want.DB:
		return fmt.Sprintf("database %q, want %q", c.DB, want.DB)
	case !want.DBModified.IsZero() && !c.DBLastModified.Equal(want.DBModified):
		return fmt.Sprintf("database modified %s, want %s", c.DBLastModified.UTC().Format(time.RFC3339), want.DBModified.UTC().Format(time.RFC3339))
	case want.GoVersion != "" && c.GoVersion != want.GoVersion:
		return fmt.Sprintf("go version %q, want %q", c.GoVersion, want.GoVersion)
	}
	return ""
}

func (s *Scan) addFinding(w *wireFinding, osvs map[string]*wireOSV, sbom map[string]string, byKey map[string]*Finding, best map[string][]Frame) string {
	if w.OSV == "" {
		return "finding with no OSV id"
	}
	entry, ok := osvs[w.OSV]
	if !ok {
		return fmt.Sprintf("finding for %s precedes its OSV entry", w.OSV)
	}
	if len(w.Trace) == 0 || w.Trace[0] == nil || w.Trace[0].Module == "" {
		return fmt.Sprintf("finding for %s has no module in its trace", w.OSV)
	}
	trace := make([]Frame, 0, len(w.Trace))
	for i, wf := range w.Trace {
		if wf == nil || wf.Module == "" {
			return fmt.Sprintf("finding for %s has an empty frame %d", w.OSV, i)
		}
		f := Frame{Module: wf.Module, Version: wf.Version, Package: wf.Package, Function: wf.Function, Receiver: wf.Receiver}
		if wf.Position != nil {
			f.File, f.Line = wf.Position.Filename, wf.Position.Line
		}
		trace = append(trace, f)
	}
	head := trace[0]
	// The level is read from the first frame, as the scanner documents:
	// module findings carry no package, package findings no function.
	level := LevelModule
	switch {
	case head.Function != "":
		if head.Package == "" {
			return fmt.Sprintf("finding for %s names a function with no package", w.OSV)
		}
		level = LevelSymbol
	case head.Package != "":
		level = LevelPackage
	}
	if level != LevelSymbol && len(trace) > 1 {
		return fmt.Sprintf("%s-level finding for %s has %d frames", level, w.OSV, len(trace))
	}
	if level == LevelSymbol && len(trace) < 2 {
		// A source-mode symbol finding runs from the vulnerable symbol to
		// an entry point in the scanned module.
		return fmt.Sprintf("symbol-level finding for %s has no caller", w.OSV)
	}
	sbomVersion, listed := sbom[head.Module]
	if !listed {
		return fmt.Sprintf("finding for %s names module %s, which the SBOM does not list", w.OSV, head.Module)
	}
	if head.Version != sbomVersion {
		return fmt.Sprintf("finding for %s has %s at %s, SBOM says %s", w.OSV, head.Module, head.Version, sbomVersion)
	}
	if w.FixedVersion != "" && !semver.IsValid(w.FixedVersion) {
		return fmt.Sprintf("finding for %s has invalid fixed version %q", w.OSV, w.FixedVersion)
	}
	key := w.OSV + " " + head.Module
	f := byKey[key]
	if f == nil {
		f = &Finding{Key: key, OSV: w.OSV, Aliases: entry.Aliases, Summary: entry.Summary, Module: head.Module, Stdlib: head.Module == StdlibModule || head.Module == "toolchain", FoundVersion: head.Version, FixedVersion: w.FixedVersion, Level: level}
		byKey[key] = f
	} else {
		if f.FoundVersion != head.Version {
			return fmt.Sprintf("findings for %s disagree on the version of %s: %s and %s", w.OSV, head.Module, f.FoundVersion, head.Version)
		}
		if f.FixedVersion != w.FixedVersion {
			return fmt.Sprintf("findings for %s disagree on the fixed version: %q and %q", w.OSV, f.FixedVersion, w.FixedVersion)
		}
		if level.rank() > f.Level.rank() {
			f.Level = level
		}
	}
	if head.Package != "" && !contains(f.Packages, head.Package) {
		f.Packages = append(f.Packages, head.Package)
	}
	if level == LevelSymbol {
		f.Traces++
		if sym := head.Symbol(); !contains(f.Symbols, sym) {
			f.Symbols = append(f.Symbols, sym)
		}
		if cur, ok := best[key]; !ok || shorter(trace, cur) {
			best[key] = trace
		}
	}
	return ""
}

// shorter orders traces by length, then by rendering, so the choice of
// representative does not depend on message order.
func shorter(a, b []Frame) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return render(a) < render(b)
}

func render(t []Frame) string {
	var sb strings.Builder
	for _, f := range t {
		fmt.Fprintf(&sb, "%s|%s|%s|%s:%d;", f.Package, f.Receiver, f.Function, f.File, f.Line)
	}
	return sb.String()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func firstLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var keep []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
		if len(keep) == n {
			break
		}
	}
	s := strings.Join(keep, " | ")
	if len(s) > 400 {
		s = s[:400]
	}
	if s == "" {
		return "no stderr"
	}
	return s
}

// Delta compares a base scan with a candidate scan by finding key.
type Delta struct {
	// Resolved are base findings absent from the candidate.
	Resolved []Finding `json:"resolved"`
	// Introduced are candidate findings absent from the base.
	Introduced []Finding `json:"introduced"`
	// Escalated are findings present in both whose level rose, such as a
	// package that is now called into.
	Escalated []Finding `json:"escalated"`
	// Persisting are findings present in both at the same or a lower level.
	Persisting []Finding `json:"persisting"`
}

// ErrInconclusive is returned when either scan is inconclusive.
var ErrInconclusive = errors.New("vuln: scan is inconclusive")

// Compare diffs two conclusive scans.
func Compare(base, candidate *Scan) (Delta, error) {
	var d Delta
	for name, s := range map[string]*Scan{"base": base, "candidate": candidate} {
		if s == nil || !s.Conclusive {
			reason := "missing"
			if s != nil {
				reason = s.Reason
			}
			return d, fmt.Errorf("%w: %s: %s", ErrInconclusive, name, reason)
		}
	}
	for _, f := range base.Findings {
		if _, ok := candidate.Finding(f.OSV, f.Module); !ok {
			d.Resolved = append(d.Resolved, f)
		}
	}
	for _, f := range candidate.Findings {
		b, ok := base.Finding(f.OSV, f.Module)
		switch {
		case !ok:
			d.Introduced = append(d.Introduced, f)
		case f.Level.rank() > b.Level.rank():
			d.Escalated = append(d.Escalated, f)
		default:
			d.Persisting = append(d.Persisting, f)
		}
	}
	return d, nil
}
