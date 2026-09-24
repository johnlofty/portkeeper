package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventLog records the lifecycle calls made against a master, in order. reconcile's
// contract is as much about SEQUENCE as about outcome — a restart that starts before it
// stops, or a replay that runs before the master is ready, produces exactly the same end
// state in a fake and a dead tunnel in reality.
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, s)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

func (l *eventLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = nil
}

func (l *eventLog) count(s string) int {
	n := 0
	for _, e := range l.all() {
		if e == s {
			n++
		}
	}
	return n
}

// inOrder reports whether the wanted events appear in this relative order, ignoring
// anything in between.
func (l *eventLog) inOrder(want ...string) bool {
	ev := l.all()
	i := 0
	for _, e := range ev {
		if i < len(want) && e == want[i] {
			i++
		}
	}
	return i == len(want)
}

// scriptedMaster is a masterCtl whose health the test drives directly. Unlike fakeMaster
// it records what was done to it, and it can refuse to start — which is the case the
// backoff schedule exists for.
type scriptedMaster struct {
	mu        sync.Mutex
	host      string
	up        bool
	failStart bool
	log       *eventLog
}

func (s *scriptedMaster) check() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log.add(s.host + ":check")
	if s.up {
		return nil
	}
	return errors.New("no master")
}

func (s *scriptedMaster) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log.add(s.host + ":start")
	if s.failStart {
		return errors.New("ssh: connect to host code port 22: Connection refused")
	}
	s.up = true
	return nil
}

func (s *scriptedMaster) stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log.add(s.host + ":stop")
	s.up = false
	return nil
}

func (s *scriptedMaster) set(up, failStart bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up, s.failStart = up, failStart
}

// reconcileFixture is a manager wired to a scripted master and a frozen clock.
func reconcileFixture(t *testing.T) (*manager, *fakeRunner, *scriptedMaster, *eventLog, func(time.Duration)) {
	t.Helper()
	m, fr := testManager(t)

	log := &eventLog{}
	mc := &scriptedMaster{host: "code", up: true, log: log}
	m.masters = map[string]masterCtl{"code": mc}

	t0 := time.Now()
	clock := t0
	var mu sync.Mutex
	m.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		mu.Lock()
		clock = clock.Add(d)
		mu.Unlock()
	}
	return m, fr, mc, log, advance
}

func sawSpec(fr *fakeRunner, sub string) bool {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, argv := range fr.calls {
		if strings.Contains(strings.Join(argv, " "), sub) {
			return true
		}
	}
	return false
}

// The whole heal path in one assertion: a failed check must stop the old master, start a
// new one, wait for it to answer, and replay every forward with the exact spec it was
// created with.
func TestReconcileRestartsAndReplaysWithExactSpecs(t *testing.T) {
	m, fr, mc, log, _ := reconcileFixture(t)

	if _, _, err := m.Open(openReq{host: "code", direction: dirLocal, remotePort: 8530}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Open(openReq{host: "code", direction: dirLocal, remoteHost: "db",
		remotePort: 5432, localPort: 15432}); err != nil {
		t.Fatal(err)
	}

	mc.set(false, false) // wifi flap
	log.reset()
	fr.calls = nil

	m.reconcile()

	if !log.inOrder("code:check", "code:stop", "code:start", "code:check") {
		t.Fatalf("heal did not stop, start and then wait for the master: %v", log.all())
	}
	// The replayed specs must be byte-identical to the originals, remote target and all:
	// ssh matches a later cancel against the string it was given.
	if !sawSpec(fr, "forward -L 127.0.0.1:8530:localhost:8530") {
		t.Fatalf("plain forward not replayed: %v", fr.calls)
	}
	if !sawSpec(fr, "forward -L 127.0.0.1:15432:db:5432") {
		t.Fatalf("forward to a third host not replayed with its target: %v", fr.calls)
	}
	if n := len(m.List()); n != 2 {
		t.Fatalf("%d forwards survived the restart, want 2", n)
	}
}

// The control channel is gone, and with it the daemon's reason to send an unsolicited
// `-O forward -R` for its own listen port on every tick. Asserted rather than assumed,
// because that request is what raced OpenSSH's startup handlers and it must not come back
// by accident.
func TestReconcileNeverAssertsAControlChannel(t *testing.T) {
	m, fr, mc, _, _ := reconcileFixture(t)
	fr.calls = nil

	m.reconcile() // healthy master

	mc.set(false, false) // and again through the restart path
	m.reconcile()

	if sawSpec(fr, "9996:localhost:9996") {
		t.Fatalf("reconcile still requests a forward for its own listen port: %v", fr.calls)
	}
	for _, argv := range fr.calls {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "-O forward -R") {
			t.Fatalf("reconcile issued an unrequested remote-forward: %s", joined)
		}
	}
}

// A box that is switched off must not get a fresh ssh process every 30 seconds. The
// schedule backs off; it never gives up, because a laptop can sleep for hours.
func TestReconcileBacksOffAndResetsOnSuccess(t *testing.T) {
	m, _, mc, log, advance := reconcileFixture(t)
	mc.set(false, true)

	m.reconcile()
	if log.count("code:start") != 1 {
		t.Fatalf("first failure did not attempt a restart: %v", log.all())
	}

	// Still inside the 30s window: no ssh process at all this tick.
	advance(10 * time.Second)
	m.reconcile()
	if log.count("code:start") != 1 {
		t.Fatalf("a restart was attempted during the backoff window: %v", log.all())
	}

	advance(25 * time.Second) // 35s total, past the first step
	m.reconcile()
	if log.count("code:start") != 2 {
		t.Fatalf("no retry after the backoff window elapsed: %v", log.all())
	}

	h := m.HostHealth()["code"]
	if h.Healthy || h.Attempts != 2 {
		t.Fatalf("health after two failures: %+v", h)
	}
	if h.LastError == "" {
		t.Fatal("no last error recorded for a host that will not connect")
	}
	// The second step is 60s, so the next window must be longer than the first.
	if h.NextRetryIn <= 30 {
		t.Fatalf("next retry in %ds, want the schedule to have widened", h.NextRetryIn)
	}

	mc.set(false, false) // the box comes back
	advance(2 * time.Minute)
	m.reconcile()

	h = m.HostHealth()["code"]
	if !h.Healthy || h.Attempts != 0 || h.NextRetryIn != 0 || h.LastError != "" {
		t.Fatalf("a success did not clear the failure history: %+v", h)
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{
		30 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second,
		240 * time.Second, 480 * time.Second, 600 * time.Second, 600 * time.Second,
	}
	for i, w := range want {
		if got := backoffFor(i); got != w {
			t.Errorf("backoffFor(%d) = %s, want %s", i, got, w)
		}
	}
}

// A forward on a host whose master is down has not vanished; it is waiting. Reporting it
// as dead would send the operator off to re-create something that is coming back.
func TestForwardsOnADownHostReportReconnecting(t *testing.T) {
	m, _, mc, _, _ := reconcileFixture(t)
	if _, _, err := m.Open(openReq{host: "code", direction: dirLocal, remotePort: 8530}); err != nil {
		t.Fatal(err)
	}
	if got := m.List()[0].State; got != "alive" {
		t.Fatalf("state %q before the flap, want alive", got)
	}

	mc.set(false, true)
	m.reconcile()

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("the forward was dropped while its host was down: %+v", list)
	}
	if list[0].State != "reconnecting" {
		t.Fatalf("state %q, want reconnecting", list[0].State)
	}
}

func TestReconcileReapsDeadLocalForwards(t *testing.T) {
	m, _, _, _, _ := reconcileFixture(t)
	if _, _, err := m.Open(openReq{host: "code", direction: dirLocal, remotePort: 8530}); err != nil {
		t.Fatal(err)
	}
	m.alive = func(int) bool { return false } // nothing answers on the Mac side any more

	m.reconcile()

	if n := len(m.List()); n != 0 {
		t.Fatalf("a vanished local-forward survived reconcile (%d entries)", n)
	}
}

// The pin is the instruction; the table is what ssh holds. reconcile's last job is to
// make the second match the first.
func TestReconcileRecreatesPinnedMappings(t *testing.T) {
	m, fr, _, _, _ := reconcileFixture(t)
	if err := m.pins.Add(pin{Host: "code", Direction: string(dirLocal), RemotePort: 8530, Label: "kept"}); err != nil {
		t.Fatal(err)
	}

	m.reconcile()

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("the pinned mapping was not created: %+v", list)
	}
	if !list[0].Pinned || list[0].Label != "kept" || list[0].TTL != 0 {
		t.Fatalf("pinned mapping came back wrong: %+v", list[0])
	}
	if !sawSpec(fr, "forward -L 127.0.0.1:8530:localhost:8530") {
		t.Fatalf("no forward was installed for the pin: %v", fr.calls)
	}

	// A second tick must not re-install what is already standing.
	before := len(m.List())
	m.reconcile()
	if len(m.List()) != before {
		t.Fatal("a pin that was already live was created twice")
	}
}

// A pin that cannot be satisfied right now stays a pin. The host being unreachable says
// nothing about whether the mapping is still wanted.
func TestPinSurvivesAFailedAssertion(t *testing.T) {
	m, fr, _, _, _ := reconcileFixture(t)
	if err := m.pins.Add(pin{Host: "code", Direction: string(dirLocal), RemotePort: 8530}); err != nil {
		t.Fatal(err)
	}
	fr.err = errors.New("exit status 255")
	fr.out = "bind [127.0.0.1]:8530: Address already in use"

	m.reconcile()
	if n := len(m.List()); n != 0 {
		t.Fatalf("a failed pin produced %d mappings", n)
	}
	if len(m.pins.List()) != 1 {
		t.Fatal("the pin was discarded because it could not be asserted once")
	}

	fr.err, fr.out = nil, ""
	m.reconcile()
	if n := len(m.List()); n != 1 {
		t.Fatalf("the pin was not retried on the next tick (%d mappings)", n)
	}
}
