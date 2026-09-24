package main

import (
	"encoding/json"
	"testing"
)

// Captured from the shapes the three boxes this runs against actually produce. The
// parser is the part of discovery most likely to be handed something it has never seen,
// so the cases are kept verbatim rather than idealised.
const ssOutput = `LISTEN 0      4096       127.0.0.1:9996       0.0.0.0:*    users:(("gatewayd",pid=4210,fd=7))
LISTEN 0      128          0.0.0.0:22          0.0.0.0:*    users:(("sshd",pid=911,fd=3))
LISTEN 0      511            [::1]:19321          [::]:*
LISTEN 0      4096               *:3030          *:*        users:(("orbstack",pid=77,fd=9))`

const netstatOutput = `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State       PID/Program name
tcp        0      0 127.0.0.1:9996          0.0.0.0:*               LISTEN      4210/gatewayd
tcp        0      0 0.0.0.0:22              0.0.0.0:*               LISTEN      -
tcp6       0      0 :::8080                 :::*                    LISTEN      512/node`

func byPort(entries []listenEntry) map[int]listenEntry {
	out := map[int]listenEntry{}
	for _, e := range entries {
		out[e.Port] = e
	}
	return out
}

func TestParseDiscoverySS(t *testing.T) {
	got := byPort(parseListenerDiscovery(ssOutput))

	if e := got[9996]; e.Address != "127.0.0.1" || e.Process != "gatewayd" || e.PID != 4210 {
		t.Errorf("ss -p detail lost: %+v", e)
	}
	// Brackets are ss's syntax, not part of the address. Keeping them would make the
	// same listener look like two, depending on which tool reported it.
	if e := got[19321]; e.Address != "::1" {
		t.Errorf("IPv6 brackets not trimmed: %+v", e)
	}
	if e := got[3030]; e.Address != "*" || e.Process != "orbstack" {
		t.Errorf("wildcard address row: %+v", e)
	}
	if len(got) != 4 {
		t.Errorf("parsed %d entries, want 4: %+v", len(got), got)
	}
}

func TestParseDiscoveryNetstatFallback(t *testing.T) {
	got := byPort(parseListenerDiscovery(netstatOutput))

	if e := got[9996]; e.Process != "gatewayd" || e.PID != 4210 {
		t.Errorf("netstat pid/program not parsed: %+v", e)
	}
	if e := got[8080]; e.Address != "::" || e.Process != "node" {
		t.Errorf("netstat tcp6 row: %+v", e)
	}
	// `-` is netstat saying it may not tell us, not a process called "-".
	if e := got[22]; e.Process != "" || e.PID != 0 {
		t.Errorf("netstat's `-` became a process: %+v", e)
	}
	// The header lines are not listeners.
	if len(got) != 3 {
		t.Errorf("parsed %d entries, want 3: %+v", len(got), got)
	}
}

// The sudo retry re-prints every row the unprivileged pass produced, this time with
// process names. Merging must fill the gaps without duplicating the rows.
func TestParseDiscoveryMergesRicherDetail(t *testing.T) {
	out := "LISTEN 0 4096 127.0.0.1:9996 0.0.0.0:*\n" +
		"LISTEN 0 4096 127.0.0.1:9996 0.0.0.0:* users:((\"gatewayd\",pid=4210,fd=7))\n"
	entries := parseListenerDiscovery(out)
	if len(entries) != 1 {
		t.Fatalf("the same listener was listed %d times: %+v", len(entries), entries)
	}
	if entries[0].Process != "gatewayd" || entries[0].PID != 4210 {
		t.Fatalf("the later, richer row did not fill in the detail: %+v", entries[0])
	}
}

// ...and a first sighting that knew the process must not be blanked by a second that
// did not.
func TestParseDiscoveryMergeNeverBlanksAField(t *testing.T) {
	out := "LISTEN 0 4096 127.0.0.1:9996 0.0.0.0:* users:((\"gatewayd\",pid=4210,fd=7))\n" +
		"LISTEN 0 4096 127.0.0.1:9996 0.0.0.0:*\n"
	entries := parseListenerDiscovery(out)
	if len(entries) != 1 || entries[0].Process != "gatewayd" {
		t.Fatalf("merge lost detail it already had: %+v", entries)
	}
}

func TestParseDiscoveryDocker(t *testing.T) {
	out := ssOutput + "\n\n__LG_DOCKER__\n" +
		"web\t0.0.0.0:8080->80/tcp, :::8080->80/tcp\n" +
		"pg\t127.0.0.1:9000-9001->9000-9001/tcp\n" +
		"quiet\t80/tcp\n" +
		"udponly\t0.0.0.0:5353->53/udp\n"
	entries := parseListenerDiscovery(out)
	got := byPort(entries)

	if e := got[8080]; e.Container != "web" || e.Address != "0.0.0.0" {
		t.Errorf("docker binding not parsed: %+v", e)
	}
	// docker prints one publish twice, once per address family. That is one socket.
	n := 0
	for _, e := range entries {
		if e.Port == 8080 {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the v4 and v6 halves of one publish became %d rows", n)
	}
	// A published range is N listeners, not one row with a dash in it.
	for _, p := range []int{9000, 9001} {
		if e := got[p]; e.Container != "pg" || e.Address != "127.0.0.1" {
			t.Errorf("docker range port %d: %+v", p, e)
		}
	}
	// An unpublished container port is not something the host is listening on, and
	// forwarding to it would simply fail.
	if _, ok := got[80]; ok {
		t.Error("an unpublished container port was offered as a listener")
	}
	if _, ok := got[5353]; ok {
		t.Error("a udp publish was offered as something to forward")
	}
}

// A container's published port shows up in both halves of the output. It is one thing
// listening, and the row that names the container is the more useful of the two.
func TestParseDiscoveryMergesDockerOntoSocket(t *testing.T) {
	out := "LISTEN 0 4096 0.0.0.0:8080 0.0.0.0:* users:((\"docker-proxy\",pid=99,fd=4))\n" +
		"__LG_DOCKER__\nweb\t0.0.0.0:8080->80/tcp\n"
	entries := parseListenerDiscovery(out)
	if len(entries) != 1 {
		t.Fatalf("docker and ss rows for one port did not merge: %+v", entries)
	}
	if entries[0].Process != "docker-proxy" || entries[0].Container != "web" {
		t.Fatalf("merged entry lost half its detail: %+v", entries[0])
	}
}

// docker is the stage most likely to be missing. Everything before the marker must
// still be reported.
func TestParseDiscoveryWithoutADockerSection(t *testing.T) {
	if got := parseListenerDiscovery(ssOutput); len(got) != 4 {
		t.Fatalf("no marker in the output cost us the socket listing: %+v", got)
	}
	got := parseListenerDiscovery(ssOutput + "\n__LG_DOCKER__\n")
	if len(got) != 4 {
		t.Fatalf("an empty docker section cost us the socket listing: %+v", got)
	}
}

func TestParseDiscoveryIgnoresNonsense(t *testing.T) {
	out := "sudo: a password is required\nWelcome to Ubuntu 22.04\nLISTEN 0 4096 127.0.0.1:9996 0.0.0.0:*\n"
	got := parseListenerDiscovery(out)
	if len(got) != 1 || got[0].Port != 9996 {
		t.Fatalf("banner and error lines were read as listeners: %+v", got)
	}
}

/* ---- the endpoint ---------------------------------------------------- */

func TestListenersEndpointRejectsUnknownAndUnsafeAliases(t *testing.T) {
	h, _, _ := testServer(t)

	// A host that is not in the book is not ours to enumerate.
	if w := do(t, h, "GET", "/admin/hosts/nosuchbox/listeners", ""); w.Code != 400 {
		t.Errorf("unknown host: %d, want 400", w.Code)
	}
	// And an alias that ssh would read as an option never gets that far.
	if w := do(t, h, "GET", "/admin/hosts/-oProxyCommand=x/listeners", ""); w.Code != 400 {
		t.Errorf("flag-shaped alias: %d, want 400", w.Code)
	}
}

func TestListenersEndpointMarksWhatIsAlreadyForwarded(t *testing.T) {
	h, m, fr := testServer(t)

	if w := do(t, h, "POST", "/admin/forward", `{"remote_port":3030}`); w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	fr.out = ssOutput

	w := do(t, h, "GET", "/admin/hosts/code/listeners", "")
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	var got struct {
		Host      string        `json:"host"`
		Listeners []listenEntry `json:"listeners"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Host != "code" || len(got.Listeners) != 4 {
		t.Fatalf("unexpected payload: %+v", got)
	}

	byP := byPort(got.Listeners)
	// Already forwarded, so it is MARKED rather than dropped from the list: leaving the
	// operator to wonder where 3030 went is worse than one row saying it is patched.
	if e := byP[3030]; !e.Forwarded || e.MappingID != m.List()[0].ID || e.URL == "" {
		t.Errorf("an already-forwarded listener was not marked: %+v", e)
	}
	if e := byP[22]; e.Forwarded {
		t.Errorf("an unforwarded listener was marked: %+v", e)
	}
}

// A mapping that targets a third host must not claim the remote's own listener on the
// same port, and vice versa: they are different services that happen to share a number.
func TestListenersMarkingRespectsTheRemoteTarget(t *testing.T) {
	m, fr := testManager(t)
	if _, _, err := m.Open(openReq{host: "code", direction: dirLocal, remoteHost: "db",
		remotePort: 5432, localPort: 15432}); err != nil {
		t.Fatal(err)
	}
	fr.out = "LISTEN 0 4096 127.0.0.1:5432 0.0.0.0:*"

	entries, err := m.discoverListeners("code")
	if err != nil {
		t.Fatal(err)
	}
	m.markForwarded("code", entries)
	if len(entries) != 1 {
		t.Fatalf("%+v", entries)
	}
	if entries[0].Forwarded {
		t.Fatal("a forward to db:5432 was credited with the remote's own 127.0.0.1:5432")
	}
}
