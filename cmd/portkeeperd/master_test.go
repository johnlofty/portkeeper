package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The settle is real time spent doing nothing, which is the point in production and
// pure cost in a test suite that restarts fake masters dozens of times.
func init() { masterSettle = 0 }

// shortDir is a temp dir with a short path. t.TempDir() embeds the test name, and on
// macOS a Unix socket path must fit in 104 bytes.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// gRunner answers `ssh -G` with a canned config dump and refuses every mux command,
// which is exactly what ssh does when the control socket is stale.
type gRunner struct {
	sock  string
	calls []string
}

func (r *gRunner) run(argv []string) (string, error) {
	r.calls = append(r.calls, strings.Join(argv, " "))
	for _, a := range argv {
		if a == "-G" {
			return "user dev\nhostname 203.0.113.7\ncontrolpath " + r.sock + "\ncontrolpersist 600\n", nil
		}
	}
	return "", errors.New("Control socket connect(" + r.sock + "): Connection refused")
}

// leftBehind creates a Unix socket file with nothing listening on it: what a SIGKILLed
// master leaves at its ControlPath.
func leftBehind(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("expected a leftover socket file: %v", err)
	}
}

func TestParseControlPathReadsTheExpandedPath(t *testing.T) {
	out := "user dev\nhostname 203.0.113.7\nport 22\n" +
		"controlpath /Users/dev/.ssh/sockets/portkeeper-dev@203.0.113.7-22\ncontrolpersist 600\n"
	want := "/Users/dev/.ssh/sockets/portkeeper-dev@203.0.113.7-22"
	if got := parseControlPath(out); got != want {
		t.Fatalf("parseControlPath = %q, want %q", got, want)
	}
	if got := parseControlPath("user dev\n"); got != "" {
		t.Fatalf("parseControlPath without a controlpath line = %q, want empty", got)
	}
}

func TestSocketStaleDistinguishesLiveLeftoverAndAbsent(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "s")

	if socketStale(path) {
		t.Fatal("an absent path is not stale")
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if socketStale(path) {
		t.Fatal("a socket with a listener is live, not stale")
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if !socketStale(path) {
		t.Fatal("a socket file nobody serves is stale")
	}

	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if socketStale(plain) {
		t.Fatal("a regular file is never treated as a stale socket")
	}
}

// The live failure of 2026-09-23: a leftover socket, and a daemon that stop()s and
// start()s forever because ssh -M will not bind over it. stop() with no process of its
// own, which is what the daemon's Start() does, must clear the leftover so the master
// that follows can bind.
func TestStopClearsALeftoverSocket(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "lg-dev@host-22")
	leftBehind(t, sock)

	r := &gRunner{sock: sock}
	m := &sshMaster{cfg: &Config{ControlPath: filepath.Join(dir, "lg-%r@%h-%p")}, host: "code", run: r}

	if err := m.stop(); err == nil {
		t.Fatal("stop should report that -O exit failed against a stale socket")
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("stale socket still present after stop: %v", err)
	}

	// The expanded path is learned once and remembered.
	m.stop()
	gs := 0
	for _, c := range r.calls {
		if strings.Contains(c, " -G ") {
			gs++
		}
	}
	if gs != 1 {
		t.Fatalf("ssh -G ran %d times, want once", gs)
	}
}

func TestStopLeavesALiveSocketAlone(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "live")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	m := &sshMaster{cfg: &Config{ControlPath: sock}, host: "code", run: &gRunner{sock: sock}}
	m.stop()
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("a socket someone is serving must not be removed: %v", err)
	}
}

func TestStopWithoutAPathLearnedDoesNothingDestructive(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s")
	leftBehind(t, sock)

	// A runner that cannot even answer -G: ssh missing or broken. Nothing is removed,
	// because the daemon does not know which file is its own.
	m := &sshMaster{cfg: &Config{ControlPath: sock}, host: "code", run: &gRunner{}}
	m.run = failingRunner{}
	m.stop()
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("with no path learned, nothing may be removed: %v", err)
	}
}

type failingRunner struct{}

func (failingRunner) run([]string) (string, error) { return "", errors.New("ssh: not found") }
