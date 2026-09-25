package main

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	minPort = 1024
	maxPort = 65535

	// A local-forward is called dead once the reconcile loop has failed to reach it
	// for longer than a couple of intervals.
	staleAfter = 90 * time.Second

	// A remote-forward's listener lives on the far side, so checking it costs an ssh
	// round trip. It is checked every remoteCheckEvery reconciles, and therefore needs
	// a staleness window wide enough to survive the gaps between those checks.
	remoteCheckEvery = 4
	remoteStaleAfter = 5 * time.Minute

	// maxRangePorts caps one ranged request. A range is a convenience for the handful
	// of ports a dev stack actually uses; anything larger is a mistake worth refusing
	// before it spends a minute installing forwards nobody wanted.
	maxRangePorts = 32
)

var (
	errPortRange    = fmt.Errorf("port must be an integer in %d-%d", minPort, maxPort)
	errUnknownHost  = errors.New("host is not in the allowlist")
	errAtCapacity   = errors.New("too many active forwards")
	errNotFound     = errors.New("no such forward")
	errBadDirection = fmt.Errorf("direction must be %q or %q", dirLocal, dirRemote)
	errLocalNeeded  = errors.New("local_port is required for a remote-forward: it names the Mac service being published")
	errOpenNotLocal = errors.New("open only applies to a local-forward; a remote-forward has no URL to open on the Mac")

	errRemoteHostNotLocal = errors.New("remote_host applies only to a local-forward: a remote-forward's target is this Mac, which is always localhost")
	errBadRemoteHost      = errors.New("remote_host must start with a letter, digit or underscore and contain only letters, digits, dot, dash or underscore; " +
		"an IPv6 literal cannot be used because the value is spliced into a colon-delimited ssh forward spec")
	errRangeTooBig = fmt.Errorf("a port range may cover at most %d ports", maxRangePorts)
	errRangeOrder  = errors.New("the end of a port range must not be below its start")
	errRangeLength = errors.New("the local and remote port ranges must cover the same number of ports")

	// errSelfForward refuses a local port that is this daemon's own listen port. As a
	// remote-forward it would publish the console to the remote, where nothing gates it;
	// as a local-forward it names a port the daemon already holds, and the fallback
	// allocator would quietly substitute another one instead of saying so.
	errSelfForward = errors.New("local_port is portkeeper's own listen port: a remote-forward of it would publish " +
		"this console to the remote, and a local-forward cannot bind it; choose another port")
)

// callerError marks a failure the caller can fix by asking for something different — a
// port already in use, a mapping that already exists. It exists so the HTTP layer can
// answer 400 rather than 502 without matching on message text, which is the kind of
// coupling that quietly stops working the day a message is reworded.
type callerError struct{ err error }

func (e callerError) Error() string { return e.err.Error() }
func (e callerError) Unwrap() error { return e.err }

func callerErrf(format string, a ...any) error {
	return callerError{fmt.Errorf(format, a...)}
}

type forward struct {
	host      string
	direction direction
	// remoteHost is the TARGET of a local-forward on the far side. Empty means the
	// remote's own localhost, which is what every mapping meant before the field
	// existed. Never set on a remote-forward.
	remoteHost string
	remotePort int
	localPort  int
	label      string
	createdAt  time.Time
	lastSeen   time.Time
	ttl        time.Duration
	requester  string
	autoOpened bool
	// pinned marks a mapping that a persisted pin asks for. The pin is the record that
	// outlives the daemon; this flag is only how the live table remembers which of its
	// rows are answering to one.
	pinned bool
}

// forwardID is the stable handle the API and console address a mapping by.
//
// Direction is part of it because the same host and port can legitimately carry one of
// each. The remote host appears only when it is set, so a mapping to the remote itself
// keeps the short two-colon form. The value cannot itself contain a colon (safeAlias
// forbids it), so the longer form stays unambiguous, and find() compares whole ids
// rather than splitting them anyway.
func forwardID(host string, d direction, remoteHost string, remotePort int) string {
	if remoteHost == "" {
		return fmt.Sprintf("%s:%s:%d", host, d, remotePort)
	}
	return fmt.Sprintf("%s:%s:%s:%d", host, d, remoteHost, remotePort)
}

func (f *forward) id() string {
	return forwardID(f.host, f.direction, f.remoteHost, f.remotePort)
}

func (f *forward) key() fwdKey {
	return fwdKey{f.host, f.direction, f.remoteHost, f.remotePort}
}

func (f *forward) pin() pin {
	return pin{
		Host:       f.host,
		Direction:  string(f.direction),
		LocalPort:  f.localPort,
		RemotePort: f.remotePort,
		RemoteHost: f.remoteHost,
		Label:      f.label,
	}
}

func (f *forward) listenPort() int {
	return f.direction.listenPort(f.localPort, f.remotePort)
}

// url is empty for a remote-forward: the service it publishes already runs on the Mac,
// so there is nothing here for the Mac's browser to open.
func (f *forward) url() string {
	if f.direction == dirRemote {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", f.localPort)
}

func (f *forward) staleWindow() time.Duration {
	if f.direction == dirRemote {
		return remoteStaleAfter
	}
	return staleAfter
}

func (f *forward) expired(now time.Time) bool {
	return f.ttl > 0 && now.Sub(f.createdAt) > f.ttl
}

type forwardView struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	Direction  string `json:"direction"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
	LocalPort  int    `json:"local_port"`
	Label      string `json:"label"`
	URL        string `json:"url"`
	CreatedAt  string `json:"created_at"`
	TTL        int    `json:"ttl"`
	ExpiresIn  int    `json:"expires_in"`
	Age        int    `json:"age"`
	State      string `json:"state"`
	Requester  string `json:"requester"`
	AutoOpened bool   `json:"auto_opened"`
	Pinned     bool   `json:"pinned"`
}

// view renders one mapping. healthy is the state of the host's master, which the caller
// holds the lock for, because it changes what this row MEANS: a forward on a host whose
// connection is being re-established has not vanished, and calling it dead would tell
// the operator to go and re-create something that is about to come back on its own.
func (f *forward) view(now time.Time, healthy bool) forwardView {
	state := "alive"
	if now.Sub(f.lastSeen) > f.staleWindow() {
		state = "dead"
	}
	if !healthy {
		state = "reconnecting"
	}
	expires := 0
	if f.ttl > 0 {
		if e := int((f.ttl - now.Sub(f.createdAt)).Seconds()); e > 0 {
			expires = e
		}
	}
	return forwardView{
		ID:         f.id(),
		Host:       f.host,
		Direction:  string(f.direction),
		RemoteHost: f.remoteHost,
		RemotePort: f.remotePort,
		LocalPort:  f.localPort,
		Label:      f.label,
		URL:        f.url(),
		CreatedAt:  f.createdAt.UTC().Format(time.RFC3339),
		TTL:        int(f.ttl.Seconds()),
		ExpiresIn:  expires,
		Age:        int(now.Sub(f.createdAt).Seconds()),
		State:      state,
		Requester:  f.requester,
		AutoOpened: f.autoOpened,
		Pinned:     f.pinned,
	}
}

type fwdKey struct {
	host       string
	direction  direction
	remoteHost string
	remotePort int
}

// allocLocalPort mirrors the preferred port when it is available, and falls back to a
// free one otherwise. Kept pure so the fallback path is testable without sockets.
func allocLocalPort(preferred int, free func(int) bool, ephemeral func() (int, error)) (int, error) {
	if free(preferred) {
		return preferred, nil
	}
	return ephemeral()
}

// probeFree is inherently TOCTOU — ssh binds the port a moment later — but the window
// is tiny and losing the race surfaces as a visible ssh error rather than silent wrongness.
//
// The dial is not redundant with the bind. Go sets SO_REUSEADDR on its listeners, so on
// macOS binding 127.0.0.1:p SUCCEEDS while another process holds the wildcard *:p — ssh,
// which does not set it, then fails to bind and the forward silently resolves to that
// other process instead. Observed with OrbStack on :3030. Anything that answers owns the
// port, whatever bind() is willing to claim.
func probeFree(port int) bool {
	if dialAlive(port) {
		return false
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func ephemeralPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// dialAlive reports whether our forward listener is still installed. ssh accepts the
// connection even when the remote service is down, which is exactly the question being
// asked here — OpenSSH offers no way to enumerate its forwards, so this is the only check.
func dialAlive(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

type openReq struct {
	host       string
	direction  direction
	remoteHost string // local-forward only; empty means the remote's localhost
	remotePort int
	localPort  int // 0 means "unset"; required for a remote-forward
	label      string
	ttl        time.Duration
	requester  string
	open       bool
	// pinned asks for a mapping that outlives the daemon, by writing it to the pin book
	// as well as installing it.
	pinned bool
}

type editReq struct {
	label      *string
	ttl        *time.Duration
	localPort  *int
	remotePort *int
	remoteHost *string
	pinned     *bool
}

// hostState is what the daemon knows about one host's master. It exists so that a
// forward on an unreachable host can be reported honestly, and so that a box that is
// simply switched off is not hammered with an ssh connection attempt every 30 seconds
// for as long as the laptop is awake.
type hostState struct {
	healthy   bool
	attempts  int       // consecutive failed restart attempts; reset by a success
	nextRetry time.Time // zero means "try now"
	lastErr   string
}

// backoffSteps is the retry schedule: 30s, 1m, 2m, 4m, 8m, then a 10 minute ceiling.
//
// It never gives up. A laptop sleeps for hours and wakes with the same forwards still
// wanted, so the terminal state of this machine is "keep trying, quietly" — the cap
// exists to stop the log and the process table filling up, not to declare defeat.
var backoffSteps = []time.Duration{
	30 * time.Second,
	60 * time.Second,
	120 * time.Second,
	240 * time.Second,
	480 * time.Second,
}

const backoffCap = 10 * time.Minute

func backoffFor(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > len(backoffSteps) {
		return backoffCap
	}
	if d := backoffSteps[attempts-1]; d < backoffCap {
		return d
	}
	return backoffCap
}

type hostHealthView struct {
	Healthy     bool   `json:"healthy"`
	Attempts    int    `json:"attempts"`
	NextRetryIn int    `json:"next_retry_in"`
	LastError   string `json:"last_error"`
}

type manager struct {
	cfg   *Config
	run   runner
	book  *hostBook
	pins  *pinBook
	paste *imagePaste // nil in tests that do not exercise image paste

	// selfPort is the daemon's own listen port, parsed from cfg.Listen once. No forward
	// may use it as its local port; see errSelfForward. 0 if Listen has no usable port,
	// which never matches since every forward port is at least minPort.
	selfPort int

	// newMaster builds a master for a host not seen before. Masters for the configured
	// hosts are opened at startup so the boxes actually in daily use are ready; a host
	// discovered from ssh_config is dialled only when someone forwards to it, since
	// holding a connection open to every alias in an ssh config would be absurd.
	newMaster func(host string) masterCtl

	alive   func(int) bool
	free    func(int) bool
	pick    func() (int, error)
	openURL func(string)
	now     func() time.Time

	mu      sync.Mutex
	masters map[string]masterCtl
	fwds    map[fwdKey]*forward
	hosts   map[string]*hostState
	cycles  int
}

func newManager(cfg *Config, run runner, masters map[string]masterCtl) *manager {
	if masters == nil {
		masters = map[string]masterCtl{}
	}
	var self int
	if _, port, err := net.SplitHostPort(cfg.Listen); err == nil {
		self, _ = strconv.Atoi(port)
	}
	return &manager{
		cfg:      cfg,
		run:      run,
		selfPort: self,
		masters:  masters,
		alive:    dialAlive,
		free:     probeFree,
		pick:     ephemeralPort,
		openURL:  openBrowser,
		now:      time.Now,
		fwds:     map[fwdKey]*forward{},
		hosts:    map[string]*hostState{},
	}
}

// hostStateLocked returns a host's record, creating it on first mention.
func (m *manager) hostStateLocked(host string) *hostState {
	hs, ok := m.hosts[host]
	if !ok {
		hs = &hostState{}
		m.hosts[host] = hs
	}
	return hs
}

// setHealthy records the outcome of a master check. A success clears the whole failure
// history: a connection that came back owes nothing to the attempts that preceded it.
func (m *manager) setHealthy(host string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hs := m.hostStateLocked(host)
	if ok && !hs.healthy && m.paste != nil {
		// Down (or never seen) to up is a new master, and a new master carries none of
		// the forwards the old one had; image paste places its own again.
		m.paste.masterUp(host)
	}
	hs.healthy = ok
	if ok {
		hs.attempts = 0
		hs.nextRetry = time.Time{}
		hs.lastErr = ""
	}
}

func (m *manager) isHealthy(host string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	hs, ok := m.hosts[host]
	return ok && hs.healthy
}

// noteFailure marks a host down and moves its retry deadline out. It returns the
// attempt number so the caller can log ONCE per attempt rather than once per tick.
func (m *manager) noteFailure(host string, err error) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	hs := m.hostStateLocked(host)
	hs.healthy = false
	hs.attempts++
	hs.nextRetry = m.now().Add(backoffFor(hs.attempts))
	if err != nil {
		hs.lastErr = err.Error()
	}
	return hs.attempts
}

// backingOff reports whether a failed host is inside its retry window. A tick that is
// backing off does nothing at all: no ssh process, no log line.
func (m *manager) backingOff(host string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	hs, ok := m.hosts[host]
	if !ok || hs.nextRetry.IsZero() {
		return false
	}
	return m.now().Before(hs.nextRetry)
}

func (m *manager) HostHealth() map[string]hostHealthView {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]hostHealthView, len(m.hosts))
	for h, hs := range m.hosts {
		retry := 0
		if !hs.nextRetry.IsZero() {
			if d := int(hs.nextRetry.Sub(now).Seconds()); d > 0 {
				retry = d
			}
		}
		out[h] = hostHealthView{
			Healthy:     hs.healthy,
			Attempts:    hs.attempts,
			NextRetryIn: retry,
			LastError:   hs.lastErr,
		}
	}
	return out
}

// masterFor returns a host's master, building it on first use.
func (m *manager) masterFor(host string) masterCtl {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mc, ok := m.masters[host]; ok {
		return mc
	}
	if m.newMaster == nil {
		return nil
	}
	mc := m.newMaster(host)
	m.masters[host] = mc
	return mc
}

// liveMasters snapshots the hosts a master has actually been opened for, which is what
// healing and shutdown must walk — not the config, which only lists the eager ones.
func (m *manager) liveMasters() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.masters))
	for h := range m.masters {
		out = append(out, h)
	}
	return out
}

// ensureUp brings a host's master up if this is the first forward to it. An eager host is
// already up from Start(); a discovered host is connected here, on demand.
//
// It ignores the backoff schedule deliberately. Backoff exists so that a background loop
// does not hammer a box that is switched off; a person who has just asked for a forward
// to that box has said something the loop did not know, and making them wait out a ten
// minute window would be the daemon arguing with its operator.
func (m *manager) ensureUp(host string) error {
	mc := m.masterFor(host)
	if mc == nil {
		return fmt.Errorf("no ssh master available for %s", host)
	}
	if err := mc.check(); err == nil {
		m.setHealthy(host, true)
		return nil
	}
	mc.stop()
	if err := mc.start(); err != nil {
		wrapped := fmt.Errorf("could not open an ssh connection to %s: %w", host, err)
		m.noteFailure(host, wrapped)
		return wrapped
	}
	if err := waitReady(mc, 10*time.Second); err != nil {
		wrapped := fmt.Errorf("ssh connection to %s did not come up: %w", host, err)
		m.noteFailure(host, wrapped)
		return wrapped
	}
	m.setHealthy(host, true)
	logf("master %s: up", host)
	return nil
}

// Start clears any master left by a previous daemon before opening a fresh one.
// This is what makes in-memory state sound: an empty table alongside an empty master
// is consistent by construction, and the daemon never inherits forwards that OpenSSH
// would not let it enumerate or cancel.
func (m *manager) Start() {
	for _, h := range m.cfg.EagerHosts {
		mc := m.masterFor(h)
		if mc == nil {
			continue
		}
		mc.stop()
		if err := mc.start(); err != nil {
			logf("master %s: start failed: %v", h, err)
			continue
		}
		if err := waitReady(mc, 10*time.Second); err != nil {
			logf("master %s: not ready: %v", h, err)
			continue
		}
		m.setHealthy(h, true)
		logf("master %s: up", h)
	}
	// Pins are asserted last, once the eager masters exist. A pin to a host that is
	// only in ssh_config dials it here, eagerly — which is a departure from "discovered
	// hosts are lazy", and the right one: the operator has said they want that mapping
	// to exist whether or not anyone asks for it today.
	m.ensurePins()
	m.ensureImagePaste()
}

// install runs one `ssh -O forward` and decides whether it worked. See
// forwardErrTolerable: the exit status alone is not evidence either way.
func (m *manager) install(host string, d direction, local, remote int, remoteHost string) error {
	out, err := m.run.run(m.cfg.argvForward(host, d, local, remote, remoteHost))
	if err == nil {
		return nil
	}
	if forwardErrTolerable(out, d.listenPort(local, remote)) {
		return nil
	}
	if d == dirRemote {
		return fmt.Errorf("remote port %d could not be bound on %s (already in use there?): %w", remote, host, err)
	}
	return err
}

func (m *manager) uninstall(f *forward) error {
	out, err := m.run.run(m.cfg.argvCancel(f.host, f.direction, f.localPort, f.remotePort, f.remoteHost))
	if err == nil || forwardErrTolerable(out, f.listenPort()) {
		return nil
	}
	// A local-forward that stopped answering is gone whatever ssh claims.
	if f.direction == dirLocal && !m.alive(f.localPort) {
		return nil
	}
	return err
}

func (m *manager) validate(r *openReq) error {
	if r.direction == "" {
		r.direction = dirLocal // what every caller meant before the field existed
	}
	if !r.direction.valid() {
		return errBadDirection
	}
	// One rule, because there is one authority level: a caller may reach anything the
	// host book knows — named in LG_HOSTS, discovered in ssh_config, or added by hand.
	// Getting this far already passed the origin guard.
	if m.book == nil || !m.book.Known(r.host) {
		return errUnknownHost
	}

	// The value is spliced into `-L 127.0.0.1:L:HOST:R`, so it is validated rather than
	// escaped — there is nothing to escape it WITH. A colon would re-cut the spec into
	// different fields entirely (`db:80:other` would name a different target and port),
	// and a leading dash would be read by ssh as one of its own options.
	if r.remoteHost != "" {
		if r.direction != dirLocal {
			return errRemoteHostNotLocal
		}
		if !safeAlias.MatchString(r.remoteHost) {
			return errBadRemoteHost
		}
	}

	// A pin is a standing instruction, and a TTL is an instruction to stop. Keeping both
	// would mean a mapping that expires and is immediately re-created by the next tick.
	if r.pinned {
		r.ttl = 0
	}

	switch r.direction {
	case dirRemote:
		if r.open {
			return errOpenNotLocal
		}
		if r.localPort == 0 {
			return errLocalNeeded
		}
		if !inRange(r.localPort) {
			return errPortRange
		}
		if r.localPort == m.selfPort {
			return errSelfForward
		}
		if r.remotePort == 0 {
			r.remotePort = r.localPort // mirror by default
		}
		if !inRange(r.remotePort) {
			return errPortRange
		}
	default:
		if !inRange(r.remotePort) {
			return errPortRange
		}
		if r.localPort != 0 && !inRange(r.localPort) {
			return errPortRange
		}
		// Only an explicit local port. A mirrored one (local_port unset, remote_port
		// equal to ours) goes through the ordinary fallback: nobody asked for this port.
		if r.localPort != 0 && r.localPort == m.selfPort {
			return errSelfForward
		}
	}
	return nil
}

func inRange(p int) bool { return p >= minPort && p <= maxPort }

func (m *manager) Open(r openReq) (forwardView, bool, error) {
	if err := m.validate(&r); err != nil {
		return forwardView{}, false, err
	}

	now := m.now()
	key := fwdKey{r.host, r.direction, r.remoteHost, r.remotePort}

	m.mu.Lock()
	if f, ok := m.fwds[key]; ok {
		f.lastSeen = now
		if r.pinned && !f.pinned {
			f.pinned = true
			f.ttl = 0
		}
		v := f.view(now, m.hostStateLocked(f.host).healthy)
		pinIt := r.pinned
		p := f.pin()
		m.mu.Unlock()
		if pinIt && m.pins != nil {
			if err := m.pins.Add(p); err != nil {
				logf("pin %s: %v", p.id(), err)
			}
		}
		return v, true, nil
	}
	if len(m.fwds) >= m.cfg.MaxForwards {
		m.mu.Unlock()
		return forwardView{}, false, errAtCapacity
	}
	taken := m.takenLocalPortsLocked()
	m.mu.Unlock()

	local := r.localPort
	if r.direction == dirLocal {
		preferred := r.remotePort
		if r.localPort != 0 {
			preferred = r.localPort
		}
		var err error
		local, err = allocLocalPort(preferred,
			func(p int) bool { return !taken[p] && m.free(p) },
			m.pick)
		if err != nil {
			return forwardView{}, false, fmt.Errorf("no local port available: %w", err)
		}
	}

	// A discovered host has no master until now; an eager one is already up and this is
	// a cheap check. Either way the connection must exist before a forward can ride it.
	if err := m.ensureUp(r.host); err != nil {
		return forwardView{}, false, err
	}

	if err := m.install(r.host, r.direction, local, r.remotePort, r.remoteHost); err != nil {
		return forwardView{}, false, err
	}

	f := &forward{
		host:       r.host,
		direction:  r.direction,
		remoteHost: r.remoteHost,
		remotePort: r.remotePort,
		localPort:  local,
		label:      r.label,
		createdAt:  now,
		lastSeen:   now,
		ttl:        r.ttl,
		requester:  r.requester,
		autoOpened: r.open,
		pinned:     r.pinned,
	}

	m.mu.Lock()
	m.fwds[key] = f
	// Built under the lock; reap may touch lastSeen the moment we let go.
	v := f.view(now, m.hostStateLocked(f.host).healthy)
	p := f.pin()
	m.mu.Unlock()

	if r.pinned && m.pins != nil {
		if err := m.pins.Add(p); err != nil {
			logf("pin %s: %v", p.id(), err)
		}
	}
	if r.open {
		m.openURL(v.URL)
	}
	return v, false, nil
}

// OpenRange installs several mappings as one unit.
//
// Sequential rather than concurrent: each one mutates the same ssh master, and the local
// port allocator reads a table the previous iteration just wrote. The rollback undoes
// only what THIS call created — a request that overlaps an existing mapping reuses it,
// and tearing that one down on an unrelated failure would break something that was
// working before the call arrived.
func (m *manager) OpenRange(reqs []openReq) ([]forwardView, error) {
	if len(reqs) == 0 {
		return nil, errPortRange
	}
	if len(reqs) > maxRangePorts {
		return nil, errRangeTooBig
	}

	// Preflight. Validating every member first turns "the eleventh port was a typo"
	// into a rejection instead of ten installed forwards and an error.
	wantLocal := map[int]bool{}
	for i := range reqs {
		probe := reqs[i]
		if err := m.validate(&probe); err != nil {
			return nil, err
		}
		if probe.direction != dirLocal || probe.localPort == 0 {
			continue
		}
		if wantLocal[probe.localPort] {
			return nil, callerErrf("local port %d appears twice in the same range", probe.localPort)
		}
		wantLocal[probe.localPort] = true
		if !m.free(probe.localPort) {
			return nil, callerErrf("local port %d is already in use on the Mac", probe.localPort)
		}
	}

	var views []forwardView
	var created []string
	for _, r := range reqs {
		v, reused, err := m.Open(r)
		if err != nil {
			for _, id := range created {
				if cerr := m.CloseID(id); cerr != nil {
					logf("rolling back %s: %v", id, cerr)
				}
			}
			return nil, err
		}
		if !reused {
			created = append(created, v.ID)
		}
		views = append(views, v)
	}
	return views, nil
}

// takenLocalPortsLocked lists the Mac ports this daemon already has listeners on. Only
// local-forwards bind locally; a remote-forward's local port names a service someone
// else runs, and several mappings may legitimately point at it.
func (m *manager) takenLocalPortsLocked() map[int]bool {
	taken := map[int]bool{}
	for _, f := range m.fwds {
		if f.direction == dirLocal {
			taken[f.localPort] = true
		}
	}
	return taken
}

func (m *manager) find(id string) (*forward, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.fwds {
		if f.id() == id {
			return f, true
		}
	}
	return nil, false
}

// Edit changes a mapping in place. A port change is a cancel plus a re-add, done in that
// order only when it cannot leave a gap: the new forward is installed FIRST, and the old
// one is cancelled only once the new one is confirmed. If the new one fails, the old is
// still standing and the caller gets an error — the alternative ordering would tear down
// a working mapping and then discover it cannot build its replacement.
func (m *manager) Edit(id string, e editReq) (forwardView, error) {
	f, ok := m.find(id)
	if !ok {
		return forwardView{}, errNotFound
	}

	newLocal, newRemote := f.localPort, f.remotePort
	newRemoteHost := f.remoteHost
	if e.localPort != nil {
		newLocal = *e.localPort
	}
	if e.remotePort != nil {
		newRemote = *e.remotePort
	}
	if e.remoteHost != nil {
		newRemoteHost = strings.TrimSpace(*e.remoteHost)
	}
	if !inRange(newLocal) || !inRange(newRemote) {
		return forwardView{}, errPortRange
	}
	if newLocal != f.localPort && newLocal == m.selfPort {
		return forwardView{}, errSelfForward
	}
	if newRemoteHost != "" {
		if f.direction != dirLocal {
			return forwardView{}, errRemoteHostNotLocal
		}
		if !safeAlias.MatchString(newRemoteHost) {
			return forwardView{}, errBadRemoteHost
		}
	}

	// The remote host is part of the spec ssh was given, so changing it is a re-install
	// exactly like a port change is — and the cancel that follows has to name the OLD
	// one or ssh will not recognise what it is being asked to drop.
	specChanged := newLocal != f.localPort || newRemote != f.remotePort || newRemoteHost != f.remoteHost
	if specChanged {
		newID := forwardID(f.host, f.direction, newRemoteHost, newRemote)
		if _, clash := m.find(newID); clash && newID != f.id() {
			return forwardView{}, callerErrf("a %s for %s already exists", f.direction, newID)
		}
		if f.direction == dirLocal && newLocal != f.localPort && !m.free(newLocal) {
			return forwardView{}, callerErrf("local port %d is already in use on the Mac", newLocal)
		}
		if err := m.install(f.host, f.direction, newLocal, newRemote, newRemoteHost); err != nil {
			return forwardView{}, err
		}
		old := *f // cancel the previous spec only now that the replacement stands
		if err := m.uninstall(&old); err != nil {
			logf("edit %s: new mapping is up but the old one would not cancel: %v", id, err)
		}
	}

	oldKey := f.key()

	now := m.now()
	m.mu.Lock()
	delete(m.fwds, oldKey)
	if e.label != nil {
		f.label = *e.label
	}
	if e.ttl != nil {
		f.ttl = *e.ttl
	}
	if e.pinned != nil {
		f.pinned = *e.pinned
		if f.pinned {
			f.ttl = 0 // pinning clears the lease; see validate()
		}
	}
	f.localPort, f.remotePort, f.remoteHost = newLocal, newRemote, newRemoteHost
	f.lastSeen = now
	m.fwds[f.key()] = f
	v := f.view(now, m.hostStateLocked(f.host).healthy)
	newPin, pinned := f.pin(), f.pinned
	m.mu.Unlock()

	// The pin follows the mapping. A pinned mapping whose ports moved must not leave the
	// old spec behind in the file, or the next restart re-creates the mapping the
	// operator has just edited away from.
	if m.pins != nil {
		if oldKey != newPin.key() || !pinned {
			if _, err := m.pins.Remove(oldKey); err != nil {
				logf("unpin %s: %v", id, err)
			}
		}
		if pinned {
			if err := m.pins.Add(newPin); err != nil {
				logf("pin %s: %v", newPin.id(), err)
			}
		}
	}
	return v, nil
}

// CloseID drops a mapping. Closing a PINNED mapping also removes its pin: leaving the
// pin behind would have reconcile re-create the mapping within thirty seconds, which
// reads as the daemon ignoring the operator rather than as a standing instruction being
// honoured. Unpin-and-close is one action because "close" can only sensibly mean that.
func (m *manager) CloseID(id string) error {
	f, ok := m.find(id)
	if !ok {
		return errNotFound
	}
	// The record is dropped only once the mapping is actually gone. Forgetting it first
	// would leave a forward that ssh still holds and nothing can list or retry —
	// precisely the orphan that in-memory state is supposed to make impossible.
	if err := m.uninstall(f); err != nil {
		return err
	}
	key := f.key()
	m.mu.Lock()
	delete(m.fwds, key)
	m.mu.Unlock()

	if m.pins != nil {
		if _, err := m.pins.Remove(key); err != nil {
			logf("unpin %s: %v", id, err)
		}
	}
	return nil
}

func (m *manager) List() []forwardView {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	// Host health is read under the same lock as the table, so a row's state and the
	// connection it depends on cannot disagree with each other.
	out := make([]forwardView, 0, len(m.fwds))
	for _, f := range m.fwds {
		out = append(out, f.view(now, m.hostStateLocked(f.host).healthy))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		if out[i].Direction != out[j].Direction {
			return out[i].Direction < out[j].Direction
		}
		return out[i].RemotePort < out[j].RemotePort
	})
	return out
}

// reconcile heals masters first, then reaps. Order matters: a dead master makes every
// forward look dead, and those forwards are about to be replayed rather than deleted.
func (m *manager) reconcile() {
	m.mu.Lock()
	m.cycles++
	checkRemote := m.cycles%remoteCheckEvery == 0
	m.mu.Unlock()

	for _, h := range m.liveMasters() {
		mc := m.masterFor(h)
		if mc == nil {
			continue
		}
		if err := mc.check(); err == nil {
			m.setHealthy(h, true)
			continue
		}
		// The master is gone. Whether we do anything about it THIS tick depends on how
		// long it has been gone: a box that is switched off or on the other side of a
		// closed laptop lid does not need a fresh ssh process every thirty seconds, and
		// the log line that accompanies one is what turns a quiet failure into noise.
		if m.backingOff(h) {
			m.markDown(h)
			continue
		}
		attempt := m.noteFailure(h, nil)
		logf("master %s: gone, restarting (attempt %d)", h, attempt)

		mc.stop()
		if err := mc.start(); err != nil {
			m.noteErr(h, err)
			logf("master %s: restart failed: %v", h, err)
			continue
		}
		if err := waitReady(mc, 10*time.Second); err != nil {
			m.noteErr(h, err)
			logf("master %s: still not ready: %v", h, err)
			continue
		}
		m.setHealthy(h, true)
		m.replay(h)
	}
	m.reap(checkRemote)

	// Last, and after the reap: a pin whose mapping was just dropped as vanished is
	// re-created here, in the same tick, rather than a further thirty seconds later.
	m.ensurePins()
	m.ensureImagePaste()
}

// ensureImagePaste brings up every host image paste is on for and puts its clipboard
// forward back on a master that lost it. Like a pin, turning image paste on says the
// operator wants that host connected, so a host with no master yet is dialled here --
// outside its backoff window, the same as the loop treats any other host.
func (m *manager) ensureImagePaste() {
	if m.paste == nil {
		return
	}
	for _, h := range m.paste.enabledHosts() {
		if !m.isHealthy(h) {
			if m.backingOff(h) {
				continue
			}
			if err := m.ensureUp(h); err != nil {
				m.paste.setErr(h, err)
				continue
			}
		}
		m.paste.ensure(h)
	}
}

// markDown records that a host is still unreachable without touching the retry
// schedule. A tick spent backing off is not an attempt.
func (m *manager) markDown(host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hostStateLocked(host).healthy = false
}

// noteErr attaches the reason a host is down, for /api/status, without counting a
// second attempt against a tick that only made one.
func (m *manager) noteErr(host string, err error) {
	if err == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hostStateLocked(host).lastErr = err.Error()
}

// ensurePins re-asserts every persisted pin whose mapping is not in the live table.
//
// A failure is logged and retried on the next tick rather than dropping the pin — the
// host being unreachable right now says nothing about whether the mapping is still wanted.
func (m *manager) ensurePins() {
	if m.pins == nil {
		return
	}
	for _, p := range m.pins.List() {
		key := p.key()
		m.mu.Lock()
		_, live := m.fwds[key]
		m.mu.Unlock()
		if live {
			continue
		}
		_, _, err := m.Open(openReq{
			host:       p.Host,
			direction:  direction(p.Direction),
			remoteHost: p.RemoteHost,
			remotePort: p.RemotePort,
			localPort:  p.LocalPort,
			label:      p.Label,
			requester:  "pinned",
			pinned:     true,
		})
		if errors.Is(err, errSelfForward) {
			// Retrying cannot help: the pin names the daemon's own port, so it will be
			// refused on every tick until someone edits the file. Drop it, say so once.
			if _, rmErr := m.pins.Remove(key); rmErr != nil {
				logf("pinned %s: %v; could not remove the pin: %v", p.id(), err, rmErr)
			} else {
				logf("pinned %s: %v; pin removed", p.id(), err)
			}
			continue
		}
		if err != nil {
			logf("pinned %s: %v (will retry)", p.id(), err)
		}
	}
}

func (m *manager) replay(host string) {
	m.mu.Lock()
	var fs []*forward
	for _, f := range m.fwds {
		if f.host == host {
			fs = append(fs, f)
		}
	}
	m.mu.Unlock()

	for _, f := range fs {
		// Trusting the raw exit status here would drop every forward on each master
		// restart, because the RemoteForward replay always reports failure.
		if err := m.install(f.host, f.direction, f.localPort, f.remotePort, f.remoteHost); err != nil {
			logf("replay %s %s %d: %v, dropping", f.host, f.direction, f.remotePort, err)
			m.mu.Lock()
			delete(m.fwds, f.key())
			m.mu.Unlock()
			continue
		}
		m.mu.Lock()
		f.lastSeen = m.now()
		m.mu.Unlock()
	}
}

// remoteListeners asks a host what it is listening on. One round trip covers every
// remote-forward to that host, which is why it is done per host rather than per forward.
func (m *manager) remoteListeners(host string) (map[int]bool, bool) {
	out, err := m.run.run(m.cfg.argvListeners(host))
	if err != nil {
		logf("listener probe %s: %v", host, err)
		return nil, false
	}
	return parseListeners(out), true
}

func (m *manager) reap(checkRemote bool) {
	now := m.now()

	// Snapshot first: each probe can take up to its timeout, and holding the lock
	// across twenty of them would stall every HTTP handler.
	m.mu.Lock()
	var cands []*forward
	hosts := map[string]bool{}
	for _, f := range m.fwds {
		if !m.hostStateLocked(f.host).healthy {
			continue // master is down; not this forward's fault
		}
		cands = append(cands, f)
		if f.direction == dirRemote && checkRemote {
			hosts[f.host] = true
		}
	}
	m.mu.Unlock()

	listeners := map[string]map[int]bool{}
	for h := range hosts {
		if set, ok := m.remoteListeners(h); ok {
			listeners[h] = set
		}
	}

	var expired, dead, live []*forward
	for _, f := range cands {
		switch {
		case f.expired(now):
			expired = append(expired, f)
		case f.direction == dirRemote:
			set, probed := listeners[f.host]
			if !probed {
				continue // not this cycle's business, or the probe failed
			}
			if set[f.remotePort] {
				live = append(live, f)
			} else {
				dead = append(dead, f)
			}
		case m.alive(f.localPort):
			live = append(live, f)
		default:
			dead = append(dead, f)
		}
	}

	m.mu.Lock()
	for _, f := range live {
		f.lastSeen = now
	}
	for _, f := range append(expired, dead...) {
		delete(m.fwds, f.key())
	}
	m.mu.Unlock()

	for _, f := range expired {
		logf("expired %s %s %d -> %d", f.host, f.direction, f.remotePort, f.localPort)
		m.uninstall(f)
	}
	for _, f := range dead {
		logf("vanished %s %s %d -> %d", f.host, f.direction, f.remotePort, f.localPort)
	}
}

func (m *manager) Shutdown() {
	m.mu.Lock()
	fs := make([]*forward, 0, len(m.fwds))
	for _, f := range m.fwds {
		fs = append(fs, f)
	}
	m.fwds = map[fwdKey]*forward{}
	m.mu.Unlock()

	for _, f := range fs {
		m.uninstall(f)
	}
	if m.paste != nil {
		m.paste.shutdown()
	}
	for _, h := range m.liveMasters() {
		m.masterFor(h).stop()
	}
}

// hostInUse counts what still depends on a host: live mappings and pins. A host with
// either cannot be removed — a pin would try to reopen its mapping every tick, against a
// host the book no longer knows, forever.
func (m *manager) hostInUse(host string) (mappings, pinned int) {
	m.mu.Lock()
	for _, f := range m.fwds {
		if f.host == host {
			mappings++
		}
	}
	m.mu.Unlock()
	if m.pins != nil {
		for _, p := range m.pins.List() {
			if p.Host == host {
				pinned++
			}
		}
	}
	return mappings, pinned
}

// restartHost drops a host's master after its settings changed, so the next connection
// is made with the new ones. The daemon keeps no copy of those settings: ssh reads them
// from the wrapper each time, and a restart is the whole of "apply".
//
// The failure history is cleared too, or a host that was backing off before the edit
// would sit out its window with settings that may now work. If mappings depend on the
// host they are replayed straight away rather than on the next reconcile tick.
func (m *manager) restartHost(host string) {
	m.mu.Lock()
	mc, ok := m.masters[host]
	if hs, seen := m.hosts[host]; seen {
		hs.attempts, hs.nextRetry, hs.lastErr = 0, time.Time{}, ""
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	mc.stop()
	m.setHealthy(host, false)
	if n, _ := m.hostInUse(host); n == 0 {
		return
	}
	go func() {
		if err := m.ensureUp(host); err != nil {
			logf("master %s: after a settings change: %v", host, err)
			return
		}
		m.replay(host)
	}()
}

// forgetHost stops a removed host's master and drops everything the manager knew about
// it. The caller has already made sure nothing depends on it.
func (m *manager) forgetHost(host string) {
	// Image paste first, while the master can still reach the host to remove the
	// wl-paste stand-in and the remote socket.
	if m.paste != nil {
		m.paste.forget(host, m.isHealthy(host))
	}
	m.mu.Lock()
	mc, ok := m.masters[host]
	delete(m.masters, host)
	delete(m.hosts, host)
	m.mu.Unlock()
	if ok {
		mc.stop()
	}
}

// testHost makes one throwaway connection with a host's current settings. It returns
// ssh's own words on failure, which say more than any paraphrase would.
func (m *manager) testHost(host string) error {
	argv := m.cfg.argvTest(host)
	var err error
	var out string
	if br, ok := m.run.(boundedRunner); ok {
		out, err = br.runBounded(argv, 20*time.Second)
	} else {
		out, err = m.run.run(argv)
	}
	if err == nil {
		return nil
	}
	if msg := lastLine(out); msg != "" {
		if strings.Contains(msg, "Permission denied") {
			msg += ". If the key has a passphrase, add it to the agent or Keychain first: the daemon has no terminal to ask for it."
		}
		return errors.New(msg)
	}
	return err
}

// lastLine is the last non-empty line ssh printed, which is where it puts the reason.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
