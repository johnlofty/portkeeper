package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// direction names the two kinds of mapping in the vocabulary the UI and API use.
// It maps one-to-one onto ssh's own flags, so there is never a question of which
// end is which.
type direction string

const (
	dirLocal  direction = "local-forward"  // ssh -L: remote's port reachable on the Mac
	dirRemote direction = "remote-forward" // ssh -R: Mac's port reachable on the remote
)

func (d direction) valid() bool { return d == dirLocal || d == dirRemote }

func (d direction) flag() string {
	if d == dirRemote {
		return "-R"
	}
	return "-L"
}

// spec is always listen-side:target-side. Both sides pin the listener to 127.0.0.1:
// a bare port would make ssh (or sshd) bind every interface and publish the service
// to whatever network that host is on.
//
// The explicit remote bind address is verified to work against this sshd, which runs
// the default GatewayPorts=no; that setting confines a remote forward to loopback
// anyway, so naming 127.0.0.1 agrees with it rather than fighting it.
func (d direction) spec(local, remote int) string {
	if d == dirRemote {
		return fmt.Sprintf("127.0.0.1:%d:localhost:%d", remote, local)
	}
	return fmt.Sprintf("127.0.0.1:%d:localhost:%d", local, remote)
}

// listenPort is the side that actually binds a socket, and so the only port ssh can
// report a failure for. For a local-forward that is the Mac's port; for a
// remote-forward it is the remote's.
func (d direction) listenPort(local, remote int) int {
	if d == dirRemote {
		return remote
	}
	return local
}

// sshArgv prefixes every ssh invocation with an explicit ControlPath.
//
// SAFETY: the user's own interactive master lives at ssh_config's default
// ControlPath. `ssh -O exit` against that path would tear down their live
// sessions, so the daemon never relies on the default — not once, anywhere.
func sshArgv(controlPath string, rest ...string) []string {
	argv := make([]string, 0, len(rest)+2)
	argv = append(argv, "-o", "ControlPath="+controlPath)
	return append(argv, rest...)
}

func (c *Config) argvExit(host string) []string {
	return sshArgv(c.ControlPath, "-O", "exit", host)
}

func (c *Config) argvCheck(host string) []string {
	return sshArgv(c.ControlPath, "-O", "check", host)
}

func (c *Config) argvMaster(host string) []string {
	return sshArgv(c.ControlPath, "-M", "-N",
		"-o", "ControlMaster=yes", "-o", "ControlPersist=no", host)
}

func (c *Config) argvForward(host string, d direction, local, remote int) []string {
	return sshArgv(c.ControlPath, "-O", "forward", d.flag(), d.spec(local, remote), host)
}

func (c *Config) argvCancel(host string, d direction, local, remote int) []string {
	return sshArgv(c.ControlPath, "-O", "cancel", d.flag(), d.spec(local, remote), host)
}

// argvListeners asks the remote what it is listening on. This is the only way to check
// a remote-forward: its socket lives on the far side, so there is nothing local to dial.
// No filter expression is passed, because that would have to survive the remote shell's
// word splitting; parsing a short listing here is cheaper than getting quoting right.
func (c *Config) argvListeners(host string) []string {
	return sshArgv(c.ControlPath, host, "ss", "-ltnH")
}

// portFailure matches the ports ssh names when a forward cannot be set up, e.g.
// "remote port forwarding failed for listen port 9996" or "cannot listen to port: 8530".
var portFailure = regexp.MustCompile(`\b(\d{2,5})\b`)

var failureWords = []string{"fail", "cannot", "error", "refused", "in use", "bind", "denied"}

// failureNamesPort reports whether ssh blamed this specific port.
//
// Every `ssh -O forward` replays ssh_config's RemoteForward lines to the master, and
// ours are already bound, so a non-zero exit is the normal case and says nothing on its
// own. But ssh names the offending port in each failure line, and the replay noise only
// ever names the control ports. So the question "did MY forward fail" has a precise
// answer: did any failure line mention my listen port.
//
// The previous rule — tolerate any error if the port answers — is what let a forward
// resolve to an unrelated process that happened to hold the port.
func failureNamesPort(out string, port int) bool {
	want := strconv.Itoa(port)
	for _, line := range strings.Split(out, "\n") {
		low := strings.ToLower(line)
		if !containsAny(low, failureWords) {
			continue
		}
		for _, m := range portFailure.FindAllStringSubmatch(line, -1) {
			if m[1] == want {
				return true
			}
		}
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// The only failure shapes the RemoteForward replay can produce. Anything else in the
// output is a different problem wearing the same non-zero exit status.
var benignForwardFailure = []string{
	"forwarding failed for listen port",
	"port not forwarded",
	"forwarding request failed",
	"master forward request failed",
	"master cancel forward request failed",
}

// forwardErrTolerable reports whether a non-zero `ssh -O forward|cancel` exit is fully
// explained by that replay noise.
//
// Tolerating requires positive evidence rather than merely the absence of bad news: at
// least one forwarding-failure line, none of them naming our own listen port, and no
// line of any other kind (a dead control socket, a refused connection, a bind clash).
// Anything unrecognised counts as a real failure, because a false success here yields a
// mapping that quietly points somewhere nobody asked for.
func forwardErrTolerable(out string, listenPort int) bool {
	want := strconv.Itoa(listenPort)
	sawFailure := false

	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		low := strings.ToLower(t)
		if !containsAny(low, failureWords) {
			continue
		}
		if !containsAny(low, benignForwardFailure) {
			return false
		}
		sawFailure = true
		for _, m := range portFailure.FindAllStringSubmatch(t, -1) {
			if m[1] == want {
				return false
			}
		}
	}
	return sawFailure
}

// parseListeners pulls the listening ports out of `ss -ltnH` output, whose rows look
// like: "LISTEN 0 128 127.0.0.1:9996 0.0.0.0:*".
func parseListeners(out string) map[int]bool {
	ports := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		addr := f[3]
		i := strings.LastIndex(addr, ":")
		if i < 0 {
			continue
		}
		if p, err := strconv.Atoi(addr[i+1:]); err == nil {
			ports[p] = true
		}
	}
	return ports
}

// runner executes a one-shot ssh control command and hands back what it printed,
// because the output — not the exit status — is what says whether it worked.
type runner interface {
	run(argv []string) (string, error)
}

type execRunner struct{}

func (execRunner) run(argv []string) (string, error) {
	out, err := exec.Command("ssh", argv...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("ssh %s: %w: %s",
			strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// masterCtl is the lifecycle of one host's ControlMaster.
type masterCtl interface {
	check() error
	start() error
	stop() error
}

type sshMaster struct {
	cfg  *Config
	host string
	run  runner

	mu  sync.Mutex
	cmd *exec.Cmd
}

func (m *sshMaster) check() error {
	_, err := m.run.run(m.cfg.argvCheck(m.host))
	return err
}

func (m *sshMaster) start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cmd := exec.Command("ssh", m.cfg.argvMaster(m.host)...)
	// A writer rather than StderrPipe: Wait closes a pipe as soon as the process
	// exits, which would race the reader. With a writer, Wait drains it first.
	cmd.Stderr = &masterLog{host: m.host}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap; liveness is judged by `ssh -O check`, not by this
	m.cmd = cmd
	return nil
}

func (m *sshMaster) stop() error {
	_, err := m.run.run(m.cfg.argvExit(m.host))

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil {
		m.cmd.Process.Kill()
		m.cmd = nil
	}
	return err
}

// waitReady polls until the master answers, because a freshly started master needs
// a moment to authenticate and bind its socket.
func waitReady(m masterCtl, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = m.check(); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// masterLog forwards the master's stderr, minus the one warning we expect and cannot
// avoid: ssh_config's RemoteForward lines are shared with the user's interactive
// master, so whichever connection lands second cannot bind those remote ports. It is
// harmless, and it would otherwise reappear in the log on every reconnect.
type masterLog struct {
	host string
	buf  []byte
}

func (l *masterLog) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := strings.TrimSpace(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
		if line != "" && !strings.Contains(line, "remote port forwarding failed") {
			fmt.Fprintf(os.Stderr, "ssh[%s]: %s\n", l.host, line)
		}
	}
}

func logf(format string, args ...any) { log.Printf(format, args...) }
