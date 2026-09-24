package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The value is spliced into `-L 127.0.0.1:L:HOST:R`. There is nothing to escape it with,
// so it is validated instead, and the colon rule is the load-bearing one: `db:80:other`
// would silently re-cut the spec into a different target and a different port.
func TestRemoteHostRejectsInjection(t *testing.T) {
	m, _ := testManager(t)
	for _, bad := range []string{
		"db:80:other",
		"-oProxyCommand=x",
		"a b",
		"host\n",
		"\nhost",
		"a;b",
		"$(whoami)",
		"::1",     // IPv6 is out of scope precisely because of the colon rule
		"[::1]",   //
		"a/b",     //
		"a\tb",    //
		"host\r",  //
		"-vvv",    //
		"'quoted", //
	} {
		_, _, err := m.Open(openReq{host: "code", direction: dirLocal, remotePort: 8530,
			remoteHost: bad})
		if !errors.Is(err, errBadRemoteHost) {
			t.Errorf("remote_host %q: got %v, want errBadRemoteHost", bad, err)
		}
	}

	// IPv4 dotted quads and ordinary names pass the same class an ssh alias does.
	for _, ok := range []string{"db", "10.0.0.5", "db.internal", "db-1_a"} {
		r := openReq{host: "code", direction: dirLocal, remotePort: 8530, remoteHost: ok}
		if err := m.validate(&r); err != nil {
			t.Errorf("remote_host %q rejected: %v", ok, err)
		}
	}
}

// A remote-forward's target is this Mac, which is always localhost. Accepting the field
// there would look like it meant something.
func TestRemoteHostRejectedOnARemoteForward(t *testing.T) {
	m, _ := testManager(t)
	_, _, err := m.Open(openReq{host: "code", direction: dirRemote, localPort: 3000,
		remoteHost: "db"})
	if !errors.Is(err, errRemoteHostNotLocal) {
		t.Fatalf("got %v, want errRemoteHostNotLocal", err)
	}
}

// The forward and the cancel must present ssh with the identical string, or the cancel
// matches nothing and the mapping is orphaned.
func TestRemoteHostSpecIsIdenticalOnForwardAndCancel(t *testing.T) {
	m, fr := testManager(t)
	v, _, err := m.Open(openReq{host: "code", direction: dirLocal, remoteHost: "db",
		remotePort: 5432, localPort: 15432})
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != "code:local-forward:db:5432" {
		t.Fatalf("id %q, want the four-part form", v.ID)
	}
	if v.RemoteHost != "db" {
		t.Fatalf("view lost the remote host: %+v", v)
	}

	if err := m.CloseID(v.ID); err != nil {
		t.Fatal(err)
	}

	var forwardSpec, cancelSpec string
	fr.mu.Lock()
	for _, argv := range fr.calls {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "-O forward -L"):
			forwardSpec = specOf(argv)
		case strings.Contains(joined, "-O cancel -L"):
			cancelSpec = specOf(argv)
		}
	}
	fr.mu.Unlock()

	if forwardSpec != "127.0.0.1:15432:db:5432" {
		t.Fatalf("forward spec %q", forwardSpec)
	}
	if cancelSpec != forwardSpec {
		t.Fatalf("cancel spec %q does not match the forward spec %q", cancelSpec, forwardSpec)
	}
}

func specOf(argv []string) string {
	for i, a := range argv {
		if (a == "-L" || a == "-R") && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// The id only grows the extra field when it is actually used, so ids without a target
// host keep the shape the console and the pin file have always stored.
func TestIDFormatIsUnchangedWithoutARemoteHost(t *testing.T) {
	m, _ := testManager(t)
	v, _, err := openTest(m, 8530)
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != "code:local-forward:8530" {
		t.Fatalf("id %q changed shape for a mapping with no remote host", v.ID)
	}
	if v.RemoteHost != "" {
		t.Fatalf("remote_host %q, want empty", v.RemoteHost)
	}
	// Same host and port, different target: two mappings, two ids, no collision.
	if _, _, err := m.Open(openReq{host: "code", direction: dirLocal, remotePort: 8530,
		remoteHost: "db", localPort: 18530}); err != nil {
		t.Fatal(err)
	}
	if n := len(m.List()); n != 2 {
		t.Fatalf("%d mappings, want the two targets to coexist", n)
	}
}

// A target host rides the ordinary admin route: it lands in the id and in the reply, and
// the mapping exists afterwards. There is no anonymous route left for it to be refused on.
func TestRemoteHostIsCreatedThroughTheAdminRoute(t *testing.T) {
	h, m, _ := testServer(t)
	w := post(t, h, `{"remote_port":5432,"remote_host":"db"}`)
	if w.Code != 200 {
		t.Fatalf("code %d, want 200: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"id":"code:local-forward:db:5432"`) {
		t.Fatalf("id missing the target host: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), `"remote_host":"db"`) {
		t.Fatalf("reply does not echo the target host: %s", w.Body)
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("%d mapping(s), want 1", n)
	}
}

func TestAdminMayEditTheRemoteHost(t *testing.T) {
	h, m, _ := testServer(t)
	c := adminCookie(t, h)
	post(t, h, `{"remote_port":5432}`)
	id := m.List()[0].ID

	if w := do(t, h, "PATCH", "/admin/forward/"+id, `{"remote_host":"db"}`, c); w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body)
	}
	if got := m.List()[0].ID; got != "code:local-forward:db:5432" {
		t.Fatalf("id after the edit: %q", got)
	}
	// And the same validation applies on the way in.
	if w := do(t, h, "PATCH", "/admin/forward/code:local-forward:db:5432",
		`{"remote_host":"db:80:other"}`, c); w.Code != 400 {
		t.Fatalf("injection through edit: %d %s", w.Code, w.Body)
	}
}

/* ---- ranges ---------------------------------------------------------- */

type rangeResult struct {
	Forwards []struct {
		ID         string `json:"id"`
		LocalPort  int    `json:"local_port"`
		RemotePort int    `json:"remote_port"`
		URL        string `json:"url"`
	} `json:"forwards"`
}

func TestRangeExpandsIntoIndependentForwards(t *testing.T) {
	h, m, _ := testServer(t)
	m.cfg.MaxForwards = 20

	w := post(t, h, `{"remote_port":8000,"remote_port_end":8004}`)
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	var got rangeResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Forwards) != 5 {
		t.Fatalf("%d forwards, want 5: %+v", len(got.Forwards), got)
	}
	for i, f := range got.Forwards {
		if f.RemotePort != 8000+i || f.LocalPort != 8000+i {
			t.Fatalf("forward %d: %+v", i, f)
		}
		if f.ID == "" || f.URL == "" {
			t.Fatalf("forward %d has no id or url: %+v", i, f)
		}
	}
	if n := len(m.List()); n != 5 {
		t.Fatalf("the table holds %d, want 5 separate records", n)
	}
	// Each one is its own mapping, closable on its own.
	if err := m.CloseID("code:local-forward:8002"); err != nil {
		t.Fatalf("closing one member of a range: %v", err)
	}
	if n := len(m.List()); n != 4 {
		t.Fatalf("%d left after closing one", n)
	}
}

// The single-object shape is what the console reads for a plain request. A range must
// not change it for requests that did not ask for one.
func TestSinglePortKeepsTheOldResponseShape(t *testing.T) {
	h, _, _ := testServer(t)
	w := post(t, h, `{"remote_port":8530}`)
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ranged := got["forwards"]; ranged {
		t.Fatalf("a single-port request answered with a range envelope: %s", w.Body)
	}
	if got["url"] != "http://127.0.0.1:8530" {
		t.Fatalf("url missing from the top level: %s", w.Body)
	}
}

// A range is installed sequentially, so a failure part way through would otherwise leave
// the caller with mappings they never got told about.
func TestRangeRollsBackWhatItCreated(t *testing.T) {
	h, m, fr := testServer(t)
	m.cfg.MaxForwards = 20

	// One install fails: the fourth, which is 8003. Everything before it has already
	// been put in place, and the cancels that undo them must still work.
	fr.failAt(4, "bind [127.0.0.1]:8003: Address already in use")

	w := post(t, h, `{"remote_port":8000,"remote_port_end":8004}`)
	if w.Code != 400 && w.Code != 502 {
		t.Fatalf("a failed range reported success: %d %s", w.Code, w.Body)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("%d forwards were left behind by a failed range: %+v", n, m.List())
	}
}

// ...but a mapping that already existed before the call is not ours to tear down.
func TestRangeRollbackLeavesPreexistingMappingsAlone(t *testing.T) {
	h, m, fr := testServer(t)
	m.cfg.MaxForwards = 20
	post(t, h, `{"remote_port":8001}`)

	// Call 1 created 8001. The range then installs 8000 (call 2), reuses 8001 without
	// an ssh call at all, installs 8002 (call 3), and fails on 8003 (call 4).
	fr.failAt(4, "bind [127.0.0.1]:8003: Address already in use")
	if w := post(t, h, `{"remote_port":8000,"remote_port_end":8004}`); w.Code == 200 {
		t.Fatal("a failed range reported success")
	}

	list := m.List()
	if len(list) != 1 || list[0].RemotePort != 8001 {
		t.Fatalf("rollback destroyed a mapping it did not create: %+v", list)
	}
}

func TestRangeLimits(t *testing.T) {
	h, m, _ := testServer(t)
	m.cfg.MaxForwards = 200

	cases := []struct{ name, body string }{
		{"backwards", `{"remote_port":8010,"remote_port_end":8000}`},
		{"too many", `{"remote_port":8000,"remote_port_end":8100}`},
		{"mismatched lengths", `{"remote_port":8000,"remote_port_end":8004,"local_port":9000,"local_port_end":9001}`},
		{"end without a start", `{"remote_port":8000,"local_port_end":9001}`},
		{"open with a range", `{"remote_port":8000,"remote_port_end":8002,"open":true}`},
	}
	for _, c := range cases {
		if w := post(t, h, c.body); w.Code != 400 {
			t.Errorf("%s: code %d, want a refusal: %s", c.name, w.Code, w.Body)
		}
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("a refused range still created %d mapping(s)", n)
	}
}

// The cap on concurrent forwards is not bypassed by asking for them all at once.
func TestRangeStillRespectsMaxForwards(t *testing.T) {
	h, m, _ := testServer(t)
	m.cfg.MaxForwards = 3

	w := post(t, h, `{"remote_port":8000,"remote_port_end":8009}`)
	if w.Code != 429 {
		t.Fatalf("code %d, want 429: %s", w.Code, w.Body)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("%d forwards survived a range that hit the cap", n)
	}
}

// An explicit local range is checked before anything is installed.
func TestRangePreflightsExplicitLocalPorts(t *testing.T) {
	h, m, _ := testServer(t)
	m.cfg.MaxForwards = 20
	m.free = func(p int) bool { return p != 9002 }

	w := post(t, h, `{"remote_port":8000,"remote_port_end":8004,"local_port":9000,"local_port_end":9004}`)
	if w.Code != 400 {
		t.Fatalf("code %d, want 400: %s", w.Code, w.Body)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("preflight ran too late: %d forwards installed", n)
	}
}

func TestRangeMirrorsTheLocalSideWhenGiven(t *testing.T) {
	h, m, _ := testServer(t)
	m.cfg.MaxForwards = 20

	w := post(t, h, `{"remote_port":8000,"remote_port_end":8002,"local_port":9000}`)
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	var got rangeResult
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Forwards) != 3 {
		t.Fatalf("%+v", got)
	}
	for i, f := range got.Forwards {
		if f.LocalPort != 9000+i {
			t.Fatalf("forward %d local port %d, want the local side to advance too", i, f.LocalPort)
		}
	}
}
