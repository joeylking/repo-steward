package deps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var published = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestCooldownReason(t *testing.T) {
	day := 24 * time.Hour
	if r := CooldownReason(published, true, published.Add(4*day), 3*day); r != "" {
		t.Fatalf("old enough: %q", r)
	}
	if r := CooldownReason(published, true, published.Add(3*day), 3*day); r != "" {
		t.Fatalf("exactly the cooldown: %q", r)
	}
	if r := CooldownReason(published, true, published.Add(5*time.Hour+30*time.Second), 72*time.Hour); r != "published 5h0m0s ago (2026-10-01T12:00:00Z), within the 72h0m0s version cooldown (-min-age)" {
		t.Fatalf("too young: %q", r)
	}
	if r := CooldownReason(time.Time{}, false, published, 72*time.Hour); !strings.Contains(r, "publish time unknown") {
		t.Fatalf("unknown: %q", r)
	}
	if r := CooldownReason(published, true, published.Add(-time.Hour), time.Hour); !strings.Contains(r, "after the current time") {
		t.Fatalf("future: %q", r)
	}
}

const libVersions = `{"Path":"example.com/lib","Versions":["v1.2.1","v1.2.4","v1.3.0","v2.0.0+incompatible"]}`

func discoverWith(t *testing.T, times string, pol Policy, now time.Time) ([]Candidate, *fakeSandbox) {
	t.Helper()
	outputs := map[string]string{
		"go list -m -u -json all":                    `{"Path":"example.com/app","Main":true}` + "\n" + `{"Path":"example.com/lib","Version":"v1.2.1"}`,
		"go list -m -versions -json example.com/lib": libVersions,
	}
	if times != "" {
		outputs["go list -e -m -json example.com/lib@v1.2.4 example.com/lib@v1.3.0"] = times
	}
	fs := &fakeSandbox{outputs: outputs}
	cands, err := DiscoverAt(context.Background(), fs, pol, now)
	if err != nil {
		t.Fatal(err)
	}
	return cands, fs
}

func TestDiscover_Cooldown(t *testing.T) {
	times := `{"Path":"example.com/lib","Version":"v1.2.4","Time":"2026-09-01T00:00:00Z"}
{"Path":"example.com/lib","Version":"v1.3.0","Time":"2026-10-01T12:00:00Z"}`
	pol := DefaultPolicy()
	pol.MinAge = 72 * time.Hour
	cands, _ := discoverWith(t, times, pol, published.Add(time.Hour))
	got := map[string]Target{}
	for _, tg := range cands[0].Targets {
		got[tg.Version] = tg
	}
	if !got["v1.2.4"].Eligible || len(got["v1.2.4"].Reasons) != 0 {
		t.Fatalf("old version: %+v", got["v1.2.4"])
	}
	if got["v1.3.0"].Eligible || strings.Join(got["v1.3.0"].Reasons, ";") != "published 1h0m0s ago (2026-10-01T12:00:00Z), within the 72h0m0s version cooldown (-min-age)" {
		t.Fatalf("young version: %+v", got["v1.3.0"])
	}
	// Only otherwise eligible versions were looked up; the major was not.
	if r := got["v2.0.0+incompatible"].Reasons; len(r) != 1 {
		t.Fatalf("major reasons %v", r)
	}

	// A version the toolchain gives no time for fails closed.
	cands, _ = discoverWith(t, `{"Path":"example.com/lib","Version":"v1.2.4","Time":"2026-09-01T00:00:00Z"}
{"Path":"example.com/lib","Version":"v1.3.0","Error":{"Err":"no info"}}`, pol, published.Add(10*24*time.Hour))
	if tg := cands[0].Targets[1]; tg.Version != "v1.3.0" || tg.Eligible || !strings.Contains(tg.Reasons[0], "publish time unknown") {
		t.Fatalf("unknown time: %+v", tg)
	}
}

// With the cooldown off nothing is looked up and the candidate JSON, which
// the model sees through list_candidates, is what it was. With it on and
// every version old enough, the JSON is the same too.
func TestDiscover_CooldownLeavesOldVersionsByteIdentical(t *testing.T) {
	off, fs := discoverWith(t, "", DefaultPolicy(), published)
	if len(fs.calls) != 2 {
		t.Fatalf("calls with the cooldown off = %d", len(fs.calls))
	}
	pol := DefaultPolicy()
	pol.MinAge = 72 * time.Hour
	on, _ := discoverWith(t, `{"Path":"example.com/lib","Version":"v1.2.4","Time":"2023-11-14T22:13:20Z"}
{"Path":"example.com/lib","Version":"v1.3.0","Time":"2023-11-14T22:13:20Z"}`, pol, published)
	a, _ := json.Marshal(off)
	b, _ := json.Marshal(on)
	if string(a) != string(b) {
		t.Fatalf("candidate JSON changed:\n%s\n%s", a, b)
	}
}
