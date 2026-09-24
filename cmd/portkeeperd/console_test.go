package main

import (
	"regexp"
	"strings"
	"testing"
)

var (
	jsByID    = regexp.MustCompile(`getElementById\("([^"]+)"\)`)
	jsByName  = regexp.MustCompile(`\.elements\["([^"]+)"\]`)
	jsBySel   = regexp.MustCompile(`querySelector\("\.([A-Za-z0-9_-]+)"\)`)
	markupID  = regexp.MustCompile(`\bid="([^"]+)"`)
	markupNm  = regexp.MustCompile(`\bname="([^"]+)"`)
	markupCls = regexp.MustCompile(`\bclass="([^"]+)"`)
)

func setOf(body string, re *regexp.Regexp, split bool) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		if !split {
			out[m[1]] = true
			continue
		}
		for _, f := range strings.Fields(m[1]) {
			out[f] = true
		}
	}
	return out
}

// The console is one file with no build step and no test runner, so the failure mode it
// is actually exposed to is a rename: JS reaching for an element the markup no longer
// has, which is silent until someone clicks the thing. This walks the page the daemon
// really serves and checks that every hook the script reaches for exists.
func TestConsoleMarkupHasEveryElementTheScriptUses(t *testing.T) {
	h, _, _ := testServer(t)
	body := do(t, h, "GET", "/", "").Body.String()
	if !strings.Contains(body, "<script>") {
		t.Fatal("the served console has no script block")
	}

	// Guard against the check quietly becoming vacuous if the page is ever restructured
	// in a way these patterns no longer match.
	wantIDs := setOf(body, jsByID, false)
	if len(wantIDs) < 20 {
		t.Fatalf("only %d element lookups found; the check has stopped matching the page", len(wantIDs))
	}

	ids := setOf(body, markupID, false)
	for want := range wantIDs {
		if !ids[want] {
			t.Errorf("the script looks up #%s, which the markup does not define", want)
		}
	}

	names := setOf(body, markupNm, false)
	for want := range setOf(body, jsByName, false) {
		if !names[want] {
			t.Errorf("the script reads the form field %q, which the markup does not define", want)
		}
	}

	classes := setOf(body, markupCls, true)
	for want := range setOf(body, jsBySel, false) {
		if !classes[want] {
			t.Errorf("the script selects .%s, which the markup does not define", want)
		}
	}
}

// The features added alongside the daemon's have to be visible in the page the daemon
// ships, not only in the code that would build them.
func TestConsoleCarriesTheNewControls(t *testing.T) {
	h, _, _ := testServer(t)
	body := do(t, h, "GET", "/", "").Body.String()

	for _, want := range []string{
		`name="pinned"`,      // keep across restarts
		`name="remote_host"`, // target host on the remote
		`id="probe"`,         // the discovery section
		`id="probe-run"`,
		`id="probe-rows"`,
		"reconnecting", // the third row state
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the console does not carry %s", want)
		}
	}
}

// The console's vocabulary is the design's: mappings are added, forwarded and kept across
// restarts. The retired words must not creep back, and neither may any trace of the login
// the daemon no longer has.
func TestConsoleVocabulary(t *testing.T) {
	h, _, _ := testServer(t)
	body := do(t, h, "GET", "/", "").Body.String()

	for _, gone := range []string{"Patch", "password", "login", "credentials", "expose"} {
		if strings.Contains(body, gone) {
			t.Errorf("the console still says %q", gone)
		}
	}
	for _, want := range []string{
		"Add mapping",
		"Forward",
		"already mapped",
		"Keep across restarts",
		"The arrow points where the port appears",
		`"#add"`,                               // a native app links straight to the add sheet
		`"Content-Type"] = "application/json"`, // every write declares JSON
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the console does not carry %s", want)
		}
	}
}
