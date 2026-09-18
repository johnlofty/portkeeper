package main

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
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
)

var (
	errPortRange    = fmt.Errorf("port must be an integer in %d-%d", minPort, maxPort)
	errUnknownHost  = errors.New("host is not in the allowlist")
	errAtCapacity   = errors.New("too many active forwards")
	errNotFound     = errors.New("no such forward")
	errBadDirection = fmt.Errorf("direction must be %q or %q", dirLocal, dirRemote)
	errLocalNeeded  = errors.New("local_port is required for a remote-forward: it names the Mac service being published")
	errOpenNotLocal = errors.New("open only applies to a local-forward; a remote-forward has no URL to open on the Mac")
)

type forward struct {
	host       string
	direction  direction
	remotePort int
	localPort  int
	label      string
	createdAt  time.Time
	lastSeen   time.Time
	ttl        time.Duration
	requester  string
	autoOpened bool
}

// id is the stable handle the API and console address a mapping by. Direction is part
// of it because the same host and port can legitimately carry one of each.
func (f *forward) id() string {
	return fmt.Sprintf("%s:%s:%d", f.host, f.direction, f.remotePort)
}

func (f *forward) key() fwdKey {
	return fwdKey{f.host, f.direction, f.remotePort}
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
}

func (f *forward) view(now time.Time) forwardView {
	state := "alive"
	if now.Sub(f.lastSeen) > f.staleWindow() {
		state = "dead"
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
	}
}

type fwdKey struct {
	host       string
	direction  direction
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
	remotePort int
	localPort  int // 0 means "unset"; required for a remote-forward
	label      string
	ttl        time.Duration
	requester  string
	open       bool
	// admin is set by the authenticated handler. It widens which hosts are reachable —
	// never what the forward itself does.
	admin bool
}

type editReq struct {
	label      *string
	ttl        *time.Duration
	localPort  *int
	remotePort *int
}

type manager struct {
	cfg  *Config
	run  runner
	book *hostBook

	// newMaster builds a master for a host not seen before. Masters for public hosts are
	// opened at startup because they carry the control channel; a host discovered from
	// ssh_config is dialled only when someone actually forwards to it, since holding a
	// connection open to every alias in an ssh config would be absurd.
	newMaster func(host string) masterCtl

	alive   func(int) bool
	free    func(int) bool
	pick    func() (int, error)
	openURL func(string)
	now     func() time.Time

	mu      sync.Mutex
	masters map[string]masterCtl
	fwds    map[fwdKey]*forward
	healthy map[string]bool
	cycles  int
}

func newManager(cfg *Config, run runner, masters map[string]masterCtl) *manager {
	if masters == nil {
		masters = map[string]masterCtl{}
	}
	return &manager{
		cfg:     cfg,
		run:     run,
		masters: masters,
		alive:   dialAlive,
		free:    probeFree,
		pick:    ephemeralPort,
		openURL: openBrowser,
		now:     time.Now,
		fwds:    map[fwdKey]*forward{},
		healthy: map[string]bool{},
	}
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
// healing and shutdown must walk — not the config, which now only lists the public ones.
func (m *manager) liveMasters() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.masters))
	for h := range m.masters {
		out = append(out, h)
	}
	return out
}

// ensureUp brings a host's master up if this is the first forward to it. A public host is
// already up from Start(); a discovered host is connected here, on demand.
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
		return fmt.Errorf("could not open an ssh connection to %s: %w", host, err)
	}
	if err := waitReady(mc, 10*time.Second); err != nil {
		return fmt.Errorf("ssh connection to %s did not come up: %w", host, err)
	}
	m.setHealthy(host, true)
	logf("master %s: up", host)
	return nil
}

// ensureControlChannel re-requests the RemoteForward the remote reaches us through.
//
// ssh_config already asks for it at connect time, but that bind FAILS whenever another
// connection is holding the remote port — typically the user's own interactive session,
// which carries the same RemoteForward line. ssh reports it as
// "remote port forwarding failed for listen port N", which masterLog deliberately
// filters as noise, because when someone else holds the port the tunnel still works.
//
// The trap is what happens next: that other session disconnects, the remote port falls
// free, and nothing re-requests it. From the Mac everything looks healthy — our master is
// up, our listener is up — while `expose` on the remote gets connection-refused. So ask
// explicitly every time a master comes up, and ignore the error when it is already bound.
func (m *manager) ensureControlChannel(host string) {
	_, portStr, err := net.SplitHostPort(m.cfg.Listen)
	if err != nil {
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return
	}
	// Same port on both ends: the remote reaches us at the address it would use locally.
	m.run.run(m.cfg.argvForward(host, dirRemote, port, port))
}

// Start clears any master left by a previous daemon before opening a fresh one.
// This is what makes in-memory state sound: an empty table alongside an empty master
// is consistent by construction, and the daemon never inherits forwards that OpenSSH
// would not let it enumerate or cancel.
func (m *manager) Start() {
	for _, h := range m.cfg.PublicHosts {
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
		m.ensureControlChannel(h)
		logf("master %s: up", h)
	}
}

func (m *manager) setHealthy(host string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthy[host] = ok
}

// install runs one `ssh -O forward` and decides whether it worked. See
// forwardErrTolerable: the exit status alone is not evidence either way.
func (m *manager) install(host string, d direction, local, remote int) error {
	out, err := m.run.run(m.cfg.argvForward(host, d, local, remote))
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
	out, err := m.run.run(m.cfg.argvCancel(f.host, f.direction, f.localPort, f.remotePort))
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
	if r.admin {
		// An authenticated caller may reach anything the console offers: discovered in
		// ssh_config, added by hand, or public.
		if m.book == nil || !m.book.Known(r.host) {
			return errUnknownHost
		}
	} else if !m.cfg.allows(r.host) {
		return errUnknownHost
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
	}
	return nil
}

func inRange(p int) bool { return p >= minPort && p <= maxPort }

func (m *manager) Open(r openReq) (forwardView, bool, error) {
	if err := m.validate(&r); err != nil {
		return forwardView{}, false, err
	}

	now := m.now()
	key := fwdKey{r.host, r.direction, r.remotePort}

	m.mu.Lock()
	if f, ok := m.fwds[key]; ok {
		f.lastSeen = now
		v := f.view(now)
		m.mu.Unlock()
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

	// A discovered host has no master until now; a public one is already up and this is
	// a cheap check. Either way the connection must exist before a forward can ride it.
	if err := m.ensureUp(r.host); err != nil {
		return forwardView{}, false, err
	}

	if err := m.install(r.host, r.direction, local, r.remotePort); err != nil {
		return forwardView{}, false, err
	}

	f := &forward{
		host:       r.host,
		direction:  r.direction,
		remotePort: r.remotePort,
		localPort:  local,
		label:      r.label,
		createdAt:  now,
		lastSeen:   now,
		ttl:        r.ttl,
		requester:  r.requester,
		autoOpened: r.open,
	}

	m.mu.Lock()
	m.fwds[key] = f
	v := f.view(now) // built under the lock; reap may touch lastSeen the moment we let go
	m.mu.Unlock()

	if r.open {
		m.openURL(v.URL)
	}
	return v, false, nil
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
	if e.localPort != nil {
		newLocal = *e.localPort
	}
	if e.remotePort != nil {
		newRemote = *e.remotePort
	}
	if !inRange(newLocal) || !inRange(newRemote) {
		return forwardView{}, errPortRange
	}

	portsChanged := newLocal != f.localPort || newRemote != f.remotePort
	if portsChanged {
		if _, clash := m.find(fmt.Sprintf("%s:%s:%d", f.host, f.direction, newRemote)); clash && newRemote != f.remotePort {
			return forwardView{}, fmt.Errorf("a %s for %s:%d already exists", f.direction, f.host, newRemote)
		}
		if f.direction == dirLocal && newLocal != f.localPort && !m.free(newLocal) {
			return forwardView{}, fmt.Errorf("local port %d is already in use on the Mac", newLocal)
		}
		if err := m.install(f.host, f.direction, newLocal, newRemote); err != nil {
			return forwardView{}, err
		}
		old := *f // cancel the previous spec only now that the replacement stands
		if err := m.uninstall(&old); err != nil {
			logf("edit %s: new mapping is up but the old one would not cancel: %v", id, err)
		}
	}

	now := m.now()
	m.mu.Lock()
	delete(m.fwds, f.key())
	if e.label != nil {
		f.label = *e.label
	}
	if e.ttl != nil {
		f.ttl = *e.ttl
	}
	f.localPort, f.remotePort = newLocal, newRemote
	f.lastSeen = now
	m.fwds[f.key()] = f
	v := f.view(now)
	m.mu.Unlock()
	return v, nil
}

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
	m.mu.Lock()
	delete(m.fwds, f.key())
	m.mu.Unlock()
	return nil
}

// localForwardID names a local-forward the way callers that predate ids address one:
// by host and remote port alone.
func localForwardID(host string, remotePort int) string {
	return fmt.Sprintf("%s:%s:%d", host, dirLocal, remotePort)
}

// Close is the pre-direction entry point, kept so the deployed `bin/expose` keeps
// working. It addresses a local-forward, which is all that client ever created.
func (m *manager) Close(host string, remotePort int) error {
	return m.CloseID(localForwardID(host, remotePort))
}

func (m *manager) List() []forwardView {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]forwardView, 0, len(m.fwds))
	for _, f := range m.fwds {
		out = append(out, f.view(now))
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
			// Re-assert it even on a healthy master: the remote port can fall free at
			// any time, when whoever else was holding it disconnects. The request is a
			// local mux round trip and is idempotent, so asking every cycle is cheaper
			// than discovering the control channel is dead from a user's failed expose.
			if m.cfg.allows(h) {
				m.ensureControlChannel(h)
			}
			continue
		}
		m.setHealthy(h, false)
		logf("master %s: gone, restarting", h)
		mc.stop()
		if err := mc.start(); err != nil {
			logf("master %s: restart failed: %v", h, err)
			continue
		}
		if err := waitReady(mc, 10*time.Second); err != nil {
			logf("master %s: still not ready: %v", h, err)
			continue
		}
		m.setHealthy(h, true)
		m.ensureControlChannel(h)
		m.replay(h)
	}
	m.reap(checkRemote)
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
		if err := m.install(f.host, f.direction, f.localPort, f.remotePort); err != nil {
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
		if !m.healthy[f.host] {
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
	for _, h := range m.liveMasters() {
		m.masterFor(h).stop()
	}
}
