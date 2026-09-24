package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
//
// remoteHost names the TARGET of a local-forward on the far side, and defaults to
// localhost. It is only ever a value that has passed safeAlias, because it is spliced
// into a colon-delimited spec: a colon in it would silently re-cut the whole string
// into different fields, and a leading dash would be read by ssh as an option.
// A remote-forward ignores it — its target is this Mac, which is always localhost.
func (d direction) spec(local, remote int, remoteHost string) string {
	if d == dirRemote {
		return fmt.Sprintf("127.0.0.1:%d:localhost:%d", remote, local)
	}
	target := remoteHost
	if target == "" {
		target = "localhost"
	}
	return fmt.Sprintf("127.0.0.1:%d:%s:%d", local, target, remote)
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

// sshArgv prefixes every ssh invocation with the daemon's own config and an explicit
// ControlPath.
//
// SAFETY: the user's own interactive master lives at ssh_config's default
// ControlPath. `ssh -O exit` against that path would tear down their live
// sessions, so the daemon never relies on the default — not once, anywhere.
//
// -F points at the generated wrapper (see writeSSHWrapper), which is how a host added in
// the console exists for ssh at all without the user's config being touched.
func (c *Config) sshArgv(rest ...string) []string {
	argv := make([]string, 0, len(rest)+4)
	if c.SSHWrapper != "" {
		argv = append(argv, "-F", c.SSHWrapper)
	}
	argv = append(argv, "-o", "ControlPath="+c.ControlPath)
	return append(argv, rest...)
}

func (c *Config) argvExit(host string) []string {
	return c.sshArgv("-O", "exit", host)
}

func (c *Config) argvCheck(host string) []string {
	return c.sshArgv("-O", "check", host)
}

func (c *Config) argvMaster(host string) []string {
	return c.sshArgv("-M", "-N",
		"-o", "ControlMaster=yes", "-o", "ControlPersist=no", host)
}

// argvTest is a one-shot connection that proves a host's settings work, independently
// of whether a master happens to be up.
//
// It is built by hand, NOT through sshArgv: ssh keeps the first -o value it sees, so a
// ControlPath=none after sshArgv's would be ignored and the test would ride on the
// daemon's master. With ControlMaster=no and no socket it cannot touch any master, the
// user's or ours. BatchMode stops it waiting on a prompt nobody can answer.
func (c *Config) argvTest(host string) []string {
	argv := []string{}
	if c.SSHWrapper != "" {
		argv = append(argv, "-F", c.SSHWrapper)
	}
	return append(argv,
		"-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=8",
		host, "true")
}

// argvForward and argvCancel must build the SAME spec for the same mapping: ssh matches
// a cancel against the string it was given, so a remote host that is present on one and
// absent on the other leaves a forward nothing can take down.
func (c *Config) argvForward(host string, d direction, local, remote int, remoteHost string) []string {
	return c.sshArgv("-O", "forward", d.flag(), d.spec(local, remote, remoteHost), host)
}

func (c *Config) argvCancel(host string, d direction, local, remote int, remoteHost string) []string {
	return c.sshArgv("-O", "cancel", d.flag(), d.spec(local, remote, remoteHost), host)
}

// argvListeners asks the remote what it is listening on. This is the only way to check
// a remote-forward: its socket lives on the far side, so there is nothing local to dial.
// No filter expression is passed, because that would have to survive the remote shell's
// word splitting; parsing a short listing here is cheaper than getting quoting right.
func (c *Config) argvListeners(host string) []string {
	return c.sshArgv(host, "ss", "-ltnH")
}

// discoverCmd is what a host is asked when the console wants to SHOW what is listening
// there, as opposed to argvListeners' cheap yes/no liveness probe.
//
// It is a fixed string. Nothing from a request is ever interpolated into it — the only
// user-supplied value in the whole invocation is the host alias, which is a separate
// argv element and has passed safeAlias. That is the rule that keeps a remote command
// string safe without any quoting to get right.
//
// Every stage is independently optional, because the three boxes this runs against do
// not agree on what is installed: ss where it exists, netstat where it does not, an
// unprivileged sudo retry that fills in process names when it is permitted and prints
// nothing when it is not, and docker last. A missing tool must leave the rest working,
// which is why each stage swallows its own errors rather than failing the command.
const discoverCmd = `ss -ltnpH 2>/dev/null || netstat -tlnp 2>/dev/null; ` +
	`sudo -n ss -ltnpH 2>/dev/null; ` +
	`printf '\n__LG_DOCKER__\n'; ` +
	`docker ps --format '{{.Names}}\t{{.Ports}}' 2>/dev/null || true`

func (c *Config) argvDiscover(host string) []string {
	return c.sshArgv(host, discoverCmd)
}

// portFailure matches the ports ssh names when a forward cannot be set up, e.g.
// "remote port forwarding failed for listen port 9997" or "cannot listen to port: 8530".
var portFailure = regexp.MustCompile(`\b(\d{2,5})\b`)

var failureWords = []string{"fail", "cannot", "error", "refused", "in use", "bind", "denied"}

// failureNamesPort reports whether ssh blamed this specific port.
//
// Every `ssh -O forward` replays ssh_config's RemoteForward lines to the master, and
// those ports are already bound, so a non-zero exit is the normal case and says nothing
// on its own. The lines are still there — 9997 and 9998 carry notify-relay and ccimgd,
// independently of this daemon — so the noise is still real. But ssh names the offending
// port in each failure line, and the replay noise only ever names those config ports. So
// the question "did MY forward fail" has a precise answer: did any failure line mention
// my listen port.
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
// like: "LISTEN 0 128 127.0.0.1:3000 0.0.0.0:*".
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

// boundedRunner is the escape hatch for the one command that can genuinely hang.
// Everything else here is a local mux round trip that returns in milliseconds; listener
// discovery runs real programs on the far side, and a wedged docker daemon there must
// not hold an HTTP handler on this Mac open indefinitely.
//
// It is a separate optional interface rather than a wider `runner` so that every test
// fake keeps working unchanged.
type boundedRunner interface {
	runBounded(argv []string, d time.Duration) (string, error)
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

func (execRunner) runBounded(argv []string, d time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ssh", argv...).CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("ssh %s: gave up after %s", strings.Join(argv, " "), d)
	}
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

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{} // closed once cmd has been reaped
	sock string        // the expanded ControlPath, learned from `ssh -G` on first use
}

func (m *sshMaster) check() error {
	_, err := m.run.run(m.cfg.argvCheck(m.host))
	return err
}

func (m *sshMaster) start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.clearStaleSocketLocked()

	cmd := exec.Command("ssh", m.cfg.argvMaster(m.host)...)
	// A writer rather than StderrPipe: Wait closes a pipe as soon as the process
	// exits, which would race the reader. With a writer, Wait drains it first.
	cmd.Stderr = &masterLog{host: m.host}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { // reap; liveness is judged by `ssh -O check`, not by this
		cmd.Wait()
		close(done)
	}()
	m.cmd, m.done = cmd, done
	return nil
}

// stop asks the master to exit and then makes sure nothing of it is left behind: not
// the process, and not its socket file.
//
// The order matters. `-O exit` makes the master unlink its own socket on the way out,
// but only if it is allowed to get there; a SIGKILL that lands first leaves the file in
// place, and OpenSSH will never reuse a leftover socket (see clearStaleSocketLocked).
// So the kill is a fallback after a grace period, not the first move.
func (m *sshMaster) stop() error {
	_, err := m.run.run(m.cfg.argvExit(m.host))

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil {
		select {
		case <-m.done:
		case <-time.After(2 * time.Second):
			m.cmd.Process.Kill()
			select {
			case <-m.done:
			case <-time.After(time.Second):
			}
		}
		m.cmd, m.done = nil, nil
	}
	m.clearStaleSocketLocked()
	return err
}

// socketPathLocked is the file ssh will actually use for this host's control socket.
// ControlPath carries %r/%h/%p tokens the daemon cannot expand itself, but `ssh -G`
// prints the resolved configuration without connecting anywhere, so ask it once.
func (m *sshMaster) socketPathLocked() string {
	if m.sock != "" {
		return m.sock
	}
	out, err := m.run.run(m.cfg.sshArgv("-G", m.host))
	if err != nil {
		return ""
	}
	m.sock = parseControlPath(out)
	return m.sock
}

func parseControlPath(out string) string {
	for _, line := range strings.Split(out, "\n") {
		k, v := splitKeyword(strings.TrimSpace(line))
		if strings.EqualFold(k, "controlpath") {
			return v
		}
	}
	return ""
}

// clearStaleSocketLocked removes a control socket that no master is serving.
//
// OpenSSH does not do this itself. Started with -M against a path that already exists,
// it logs "ControlSocket ... already exists, disabling multiplexing" and carries on as a
// plain session that answers no `-O check`. The daemon then sees a master that never
// comes up, stops it, starts another, and gets the same result: a loop it cannot leave
// on its own. Found live on 2026-09-23: a `make install` two days earlier had SIGKILLed
// the previous daemon's master before it unlinked, and every tick since had hit this.
//
// Only a socket that exists AND refuses connections is removed. A live socket belongs
// to whoever is serving it, and a regular file at that path is somebody else's problem.
func (m *sshMaster) clearStaleSocketLocked() {
	path := m.socketPathLocked()
	if path == "" || !socketStale(path) {
		return
	}
	if err := os.Remove(path); err != nil {
		logf("master %s: stale control socket %s could not be removed: %v", m.host, path, err)
		return
	}
	logf("master %s: removed stale control socket %s", m.host, path)
}

// socketStale reports whether path is a Unix socket nothing is listening on.
func socketStale(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return false
	}
	c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err == nil {
		c.Close()
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

// masterSettle is how long a freshly answering master is left alone before the daemon
// sends it any `-O forward`. See waitReady. Tests set it to zero.
var masterSettle = 3 * time.Second

// waitReady polls until the master answers, because a freshly started master needs a
// moment to authenticate and bind its socket -- and then waits a little longer.
//
// The extra wait is not politeness. At startup the master sends one tcpip-forward
// request per RemoteForward line in ssh_config and registers a reply handler for each
// that points INTO its options.remote_forwards array. A `-O forward -R` arriving over
// the mux socket before those replies are back appends to that array, which may move
// it, and the pending handlers then read freed memory. Seen live on 2026-09-23 as
// "ssh_confirm_remote_forward: parse packet: incomplete message", a fatal that killed
// the master twice in a row, each time within a second of the daemon re-asserting the
// control channel; the third start survived on timing alone. The mux socket appears
// before those replies arrive, so "answers -O check" is not "ready for -O forward -R",
// and nothing in the mux protocol says when it is. So wait longer than a round trip to
// the remote could plausibly take.
//
// Retiring the control channel removed the one thing that reliably sent an `-O forward -R`
// within milliseconds of a master coming up, but it did not remove the hazard: a
// remote-forward pin asserted by Start(), or one made from the console against a host
// dialled on demand, arrives on the same path. The wait stays.
func waitReady(m masterCtl, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = m.check(); err == nil {
			time.Sleep(masterSettle)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// masterLog forwards the master's stderr, minus the one warning we expect and cannot
// avoid: ssh_config still carries `RemoteForward 9997` and `9998` for notify-relay and
// ccimgd, and those lines are shared with the user's interactive master, so whichever
// connection lands second cannot bind those remote ports. It is harmless — nothing here
// depends on them — and it would otherwise reappear in the log on every reconnect.
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
