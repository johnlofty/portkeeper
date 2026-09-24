package main

import (
	"strings"
	"testing"
)

const testCP = "/tmp/lg-sockets/local-gateway-%r@%h-%p"

func testCfg() *Config {
	return &Config{
		Listen:      "127.0.0.1:9996",
		EagerHosts:  []string{"code"},
		MaxForwards: 3,
		ControlPath: testCP,
	}
}

// The daemon must never touch the user's interactive master, which lives at
// ssh_config's default ControlPath. Every argv it builds carries its own path.
func TestEverySSHArgvCarriesControlPath(t *testing.T) {
	c := testCfg()
	argvs := map[string][]string{
		"exit":           c.argvExit("code"),
		"check":          c.argvCheck("code"),
		"master":         c.argvMaster("code"),
		"listeners":      c.argvListeners("code"),
		"discover":       c.argvDiscover("code"),
		"forward-local":  c.argvForward("code", dirLocal, 8530, 8530, ""),
		"cancel-local":   c.argvCancel("code", dirLocal, 8530, 8530, ""),
		"forward-target": c.argvForward("code", dirLocal, 8530, 5432, "db"),
		"cancel-target":  c.argvCancel("code", dirLocal, 8530, 5432, "db"),
		"forward-remote": c.argvForward("code", dirRemote, 3000, 3000, ""),
		"cancel-remote":  c.argvCancel("code", dirRemote, 3000, 3000, ""),
	}

	for name, argv := range argvs {
		if !hasOpt(argv, "ControlPath="+testCP) {
			t.Errorf("%s: argv has no explicit -o ControlPath=%s: %v", name, testCP, argv)
		}
		for _, a := range argv {
			if strings.Contains(a, "ControlPath=") && a != "ControlPath="+testCP {
				t.Errorf("%s: unexpected ControlPath %q", name, a)
			}
		}
	}
}

// -o and its value must be adjacent, or ssh reads the value as the destination host.
func hasOpt(argv []string, want string) bool {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "-o" && argv[i+1] == want {
			return true
		}
	}
	return false
}

// Both directions pin their listener to loopback, and each puts the LISTENING side
// first -- getting that backwards would forward the wrong way round.
func TestSpecPinsLoopbackAndOrdersSides(t *testing.T) {
	if got, want := dirLocal.spec(8530, 9000, ""), "127.0.0.1:8530:localhost:9000"; got != want {
		t.Errorf("local-forward: got %q want %q", got, want)
	}
	if got, want := dirRemote.spec(8530, 9000, ""), "127.0.0.1:9000:localhost:8530"; got != want {
		t.Errorf("remote-forward: got %q want %q", got, want)
	}
	if got := dirLocal.listenPort(8530, 9000); got != 8530 {
		t.Errorf("local-forward listens on the Mac port, got %d", got)
	}
	if got := dirRemote.listenPort(8530, 9000); got != 9000 {
		t.Errorf("remote-forward listens on the remote port, got %d", got)
	}
}

func TestArgvUsesCorrectFlagPerDirection(t *testing.T) {
	c := testCfg()
	if !contains(c.argvForward("code", dirLocal, 1100, 1200, ""), "-L") {
		t.Error("local-forward must use -L")
	}
	if !contains(c.argvForward("code", dirRemote, 1100, 1200, ""), "-R") {
		t.Error("remote-forward must use -R")
	}
}

func TestArgvOrderPutsHostLast(t *testing.T) {
	c := testCfg()
	for _, argv := range [][]string{
		c.argvExit("code"), c.argvCheck("code"), c.argvMaster("code"),
		c.argvForward("code", dirLocal, 1100, 1200, ""), c.argvCancel("code", dirLocal, 1100, 1200, ""),
		c.argvForward("code", dirLocal, 1100, 1200, "db"), c.argvCancel("code", dirLocal, 1100, 1200, "db"),
		c.argvForward("code", dirRemote, 1100, 1200, ""), c.argvCancel("code", dirRemote, 1100, 1200, ""),
	} {
		if argv[len(argv)-1] != "code" {
			t.Errorf("host is not the final argument: %v", argv)
		}
	}
}

// Captured from the live host. Every `ssh -O forward` replays ssh_config's RemoteForward
// lines, so this noise accompanies a forward that in fact succeeded. 9997 and 9998 are
// notify-relay and ccimgd, which have nothing to do with this daemon and are still in the
// config; the 9996 line the capture also had went with the control channel.
const replayNoise = `mux_client_forward: forwarding request failed: remote port forwarding failed for listen port 9998
mux_client_forward: forwarding request failed: remote port forwarding failed for listen port 9997
muxclient: master forward request failed`

func TestForwardErrTolerance(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		listenPort int
		tolerable  bool
	}{
		{"replay noise for other ports", replayNoise, 8530, true},
		{"noise names our port", replayNoise, 9997, false},
		{"cancel noise", "mux_client_forward: forwarding request failed: port not forwarded", 8530, true},
		{
			// The OrbStack case: ssh could not bind our port because another process
			// held the wildcard. Tolerating this produced a mapping to the wrong service.
			"our own bind failed",
			replayNoise + "\nbind [127.0.0.1]:3030: Address already in use",
			3030, false,
		},
		{"dead control socket", "Control socket connect(/x): No such file or directory", 8530, false},
		{"connection refused", "ssh: connect to host code port 22: Connection refused", 8530, false},
		{"no failure at all", "", 8530, false},
	}
	for _, c := range cases {
		if got := forwardErrTolerable(c.out, c.listenPort); got != c.tolerable {
			t.Errorf("%s: forwardErrTolerable = %v, want %v", c.name, got, c.tolerable)
		}
	}
}

func TestParseListeners(t *testing.T) {
	out := `LISTEN 0      128        127.0.0.1:3000      0.0.0.0:*
LISTEN 0      128            [::1]:19321         [::]:*
LISTEN 0      4096         0.0.0.0:3030       0.0.0.0:*`
	got := parseListeners(out)
	for _, p := range []int{3000, 19321, 3030} {
		if !got[p] {
			t.Errorf("port %d not parsed from ss output: %v", p, got)
		}
	}
	if got[22] {
		t.Error("parsed a port that was not listening")
	}
}

// Shared ssh_config RemoteForward lines mean the second master to connect always
// fails to bind them. 9997 and 9998 are still in that config for other tools, so the
// warning is still produced on every reconnect. Suppressed so a reconnect loop cannot
// spam the log.
func TestMasterLogDropsExpectedBindWarning(t *testing.T) {
	l := &masterLog{host: "code"}
	n, err := l.Write([]byte("Warning: remote port forwarding failed for listen port 9997\nreal problem\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("writer reported no bytes consumed")
	}
	if len(l.buf) != 0 {
		t.Fatalf("complete lines left buffered: %q", l.buf)
	}
}

func TestMasterLogBuffersPartialLines(t *testing.T) {
	l := &masterLog{host: "code"}
	l.Write([]byte("half a li"))
	if string(l.buf) != "half a li" {
		t.Fatalf("partial line not retained: %q", l.buf)
	}
	l.Write([]byte("ne\n"))
	if len(l.buf) != 0 {
		t.Fatalf("buffer not drained after newline: %q", l.buf)
	}
}
