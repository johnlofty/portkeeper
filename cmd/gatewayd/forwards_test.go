package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	err   error
	out   string
}

func (f *fakeRunner) run(argv []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argv)
	return f.out, f.err
}

func (f *fakeRunner) sawSubcommand(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Join(c, " ") != "" && contains(c, sub) {
			return true
		}
	}
	return false
}

func contains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

type fakeMaster struct{ up bool }

func (f *fakeMaster) check() error {
	if f.up {
		return nil
	}
	return errors.New("no master")
}
func (f *fakeMaster) start() error { f.up = true; return nil }
func (f *fakeMaster) stop() error  { f.up = false; return nil }

func testManager(t *testing.T) (*manager, *fakeRunner) {
	t.Helper()
	cfg := testCfg()
	cfg.DefaultTTL = time.Hour
	fr := &fakeRunner{}
	m := newManager(cfg, fr, map[string]masterCtl{"code": &fakeMaster{up: true}})
	m.free = func(int) bool { return true }
	m.pick = func() (int, error) { return 40000, nil }
	m.alive = func(int) bool { return true }
	m.openURL = func(string) {}
	m.setHealthy("code", true)
	return m, fr
}

func openTest(m *manager, port int) (forwardView, bool, error) {
	return m.Open(openReq{host: "code", direction: dirLocal, remotePort: port, ttl: time.Hour})
}

func TestAllocMirrorsWhenFree(t *testing.T) {
	got, err := allocLocalPort(8530, func(int) bool { return true }, func() (int, error) { return 1, nil })
	if err != nil || got != 8530 {
		t.Fatalf("got %d %v, want 8530", got, err)
	}
}

func TestAllocFallsBackWhenBusy(t *testing.T) {
	got, err := allocLocalPort(8530, func(int) bool { return false }, func() (int, error) { return 40001, nil })
	if err != nil || got != 40001 {
		t.Fatalf("got %d %v, want 40001", got, err)
	}
}

func TestOpenReturnsAllocatedPortNotPreferred(t *testing.T) {
	m, _ := testManager(t)
	m.free = func(int) bool { return false } // mirror unavailable

	v, _, err := openTest(m, 8530)
	if err != nil {
		t.Fatal(err)
	}
	if v.LocalPort != 40000 {
		t.Fatalf("local port %d, want the allocated 40000", v.LocalPort)
	}
	if v.URL != "http://127.0.0.1:40000" {
		t.Fatalf("url %q must reflect the allocated port", v.URL)
	}
}

func TestDedupReturnsExistingMapping(t *testing.T) {
	m, fr := testManager(t)

	first, reused, err := openTest(m, 8530)
	if err != nil || reused {
		t.Fatalf("first open: %v reused=%v", err, reused)
	}
	second, reused, err := openTest(m, 8530)
	if err != nil || !reused {
		t.Fatalf("second open: %v reused=%v, want reused", err, reused)
	}
	if second.LocalPort != first.LocalPort {
		t.Fatalf("reused mapping changed port: %d -> %d", first.LocalPort, second.LocalPort)
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("registry has %d entries, want 1", n)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.calls) != 1 {
		t.Fatalf("issued %d ssh calls, want 1 (dedup must not re-forward)", len(fr.calls))
	}
}

func TestRejectsPortOutOfRange(t *testing.T) {
	m, _ := testManager(t)
	for _, p := range []int{0, 80, 1023, 65536, 99999, -1} {
		if _, _, err := openTest(m, p); !errors.Is(err, errPortRange) {
			t.Errorf("port %d: got %v, want errPortRange", p, err)
		}
	}
}

func TestRejectsHostOutsideAllowlist(t *testing.T) {
	m, _ := testManager(t)
	_, _, err := m.Open(openReq{host: "evil", remotePort: 8530})
	if !errors.Is(err, errUnknownHost) {
		t.Fatalf("got %v, want errUnknownHost", err)
	}
}

func TestCapacityCap(t *testing.T) {
	m, _ := testManager(t)
	for i := 0; i < m.cfg.MaxForwards; i++ {
		if _, _, err := openTest(m, 9000+i); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	if _, _, err := openTest(m, 9999); !errors.Is(err, errAtCapacity) {
		t.Fatalf("got %v, want errAtCapacity", err)
	}
}

func TestTTLExpiryCancelsAndRemoves(t *testing.T) {
	m, fr := testManager(t)
	t0 := time.Now()
	m.now = func() time.Time { return t0 }

	if _, _, err := m.Open(openReq{host: "code", remotePort: 8530, ttl: time.Minute}); err != nil {
		t.Fatal(err)
	}

	m.now = func() time.Time { return t0.Add(2 * time.Minute) }
	m.reap(true)

	if n := len(m.List()); n != 0 {
		t.Fatalf("expired forward still present (%d entries)", n)
	}
	if !fr.sawSubcommand("cancel") {
		t.Fatal("expiry did not cancel the forward")
	}
}

func TestTTLZeroNeverExpires(t *testing.T) {
	m, _ := testManager(t)
	t0 := time.Now()
	m.now = func() time.Time { return t0 }

	if _, _, err := m.Open(openReq{host: "code", remotePort: 8530, ttl: 0}); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return t0.Add(500 * time.Hour) }
	m.reap(true)

	if n := len(m.List()); n != 1 {
		t.Fatalf("ttl=0 forward was reaped (%d entries)", n)
	}
}

// A dead master makes every forward unreachable. Those forwards are about to be
// replayed, so reaping them would destroy the table the replay depends on.
func TestReapSkipsForwardsWhenMasterIsDown(t *testing.T) {
	m, _ := testManager(t)
	if _, _, err := openTest(m, 8530); err != nil {
		t.Fatal(err)
	}

	m.setHealthy("code", false)
	m.alive = func(int) bool { return false }
	m.reap(true)

	if n := len(m.List()); n != 1 {
		t.Fatalf("forward dropped while master was down (%d entries)", n)
	}
}

func TestReapDropsVanishedForwardWhenMasterIsUp(t *testing.T) {
	m, _ := testManager(t)
	if _, _, err := openTest(m, 8530); err != nil {
		t.Fatal(err)
	}

	m.alive = func(int) bool { return false }
	m.reap(true)

	if n := len(m.List()); n != 0 {
		t.Fatalf("vanished forward kept (%d entries)", n)
	}
}

func TestReplayReissuesForwards(t *testing.T) {
	m, fr := testManager(t)
	if _, _, err := openTest(m, 8530); err != nil {
		t.Fatal(err)
	}
	fr.mu.Lock()
	before := len(fr.calls)
	fr.mu.Unlock()

	m.replay("code")

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.calls) != before+1 {
		t.Fatalf("replay issued %d calls, want 1", len(fr.calls)-before)
	}
	if !contains(fr.calls[len(fr.calls)-1], "forward") {
		t.Fatalf("replay did not re-add the forward: %v", fr.calls[len(fr.calls)-1])
	}
}

func TestCloseCancelsAndForgets(t *testing.T) {
	m, fr := testManager(t)
	if _, _, err := openTest(m, 8530); err != nil {
		t.Fatal(err)
	}
	if err := m.Close("code", 8530); err != nil {
		t.Fatal(err)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("closed forward still listed (%d)", n)
	}
	if !fr.sawSubcommand("cancel") {
		t.Fatal("close did not issue ssh -O cancel")
	}
	if err := m.Close("code", 8530); !errors.Is(err, errNotFound) {
		t.Fatalf("second close: got %v, want errNotFound", err)
	}
}

func TestViewStateGoesDeadWhenStale(t *testing.T) {
	m, _ := testManager(t)
	t0 := time.Now()
	m.now = func() time.Time { return t0 }
	if _, _, err := openTest(m, 8530); err != nil {
		t.Fatal(err)
	}

	m.now = func() time.Time { return t0.Add(staleAfter + time.Second) }
	if got := m.List()[0].State; got != "dead" {
		t.Fatalf("state %q, want dead", got)
	}
}

// Open, List and reap all touch the same entries; this is the check that the
// lock discipline holds rather than merely looking right.
func TestConcurrentOpenListReap(t *testing.T) {
	m, _ := testManager(t)
	m.cfg.MaxForwards = 1000

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			openTest(m, 10000+i)
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.List() }()
		wg.Add(1)
		go func() { defer wg.Done(); m.reap(true) }()
	}
	wg.Wait()
}

func TestRemoteForwardRequiresLocalPort(t *testing.T) {
	m, _ := testManager(t)
	_, _, err := m.Open(openReq{host: "code", direction: dirRemote, remotePort: 3000})
	if !errors.Is(err, errLocalNeeded) {
		t.Fatalf("got %v, want errLocalNeeded", err)
	}
}

// A remote-forward publishes an existing Mac service, so the remote port mirrors the
// local one unless asked otherwise, and there is no Mac URL to hand back.
func TestRemoteForwardMirrorsAndHasNoURL(t *testing.T) {
	m, _ := testManager(t)
	v, _, err := m.Open(openReq{host: "code", direction: dirRemote, localPort: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if v.RemotePort != 3000 || v.LocalPort != 3000 {
		t.Fatalf("ports %d -> %d, want 3000 both", v.LocalPort, v.RemotePort)
	}
	if v.URL != "" {
		t.Fatalf("url %q: a remote-forward has nothing to open on the Mac", v.URL)
	}
	if v.ID != "code:remote-forward:3000" {
		t.Fatalf("id %q", v.ID)
	}
}

func TestRemoteForwardRejectsOpen(t *testing.T) {
	m, _ := testManager(t)
	_, _, err := m.Open(openReq{host: "code", direction: dirRemote, localPort: 3000, open: true})
	if !errors.Is(err, errOpenNotLocal) {
		t.Fatalf("got %v, want errOpenNotLocal", err)
	}
}

// Same host and port in each direction are different mappings and must coexist.
func TestDirectionsAreSeparateKeys(t *testing.T) {
	m, _ := testManager(t)
	if _, _, err := m.Open(openReq{host: "code", direction: dirLocal, remotePort: 3000}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Open(openReq{host: "code", direction: dirRemote, localPort: 3000}); err != nil {
		t.Fatal(err)
	}
	if n := len(m.List()); n != 2 {
		t.Fatalf("%d entries, want both directions to coexist", n)
	}
}

func TestRemoteForwardUsesDashR(t *testing.T) {
	m, fr := testManager(t)
	if _, _, err := m.Open(openReq{host: "code", direction: dirRemote, localPort: 3000}); err != nil {
		t.Fatal(err)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	last := fr.calls[len(fr.calls)-1]
	if !contains(last, "-R") {
		t.Fatalf("remote-forward did not use -R: %v", last)
	}
}

// The replay noise accompanies a forward that worked; a failure naming our own port
// does not. The manager must tell those apart from the ssh output alone.
func TestOpenToleratesReplayNoiseButNotOurOwnFailure(t *testing.T) {
	m, fr := testManager(t)
	fr.err = errors.New("exit status 255")
	fr.out = replayNoise

	if _, _, err := openTest(m, 8530); err != nil {
		t.Fatalf("replay noise treated as failure: %v", err)
	}

	fr.out = replayNoise + "\nbind [127.0.0.1]:8531: Address already in use"
	if _, _, err := openTest(m, 8531); err == nil {
		t.Fatal("a failure naming our own port was accepted")
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("%d entries, want only the successful one", n)
	}
}

// A dead control socket is not replay noise, and must not be tolerated even though it
// never names a port.
func TestOpenRejectsNonForwardingErrors(t *testing.T) {
	m, fr := testManager(t)
	fr.err = errors.New("exit status 255")
	fr.out = "Control socket connect(/tmp/x): No such file or directory"

	if _, _, err := openTest(m, 8530); err == nil {
		t.Fatal("a dead control socket was treated as success")
	}
}

// Edit must install the replacement before cancelling the original, so a failure
// leaves the working mapping standing rather than tearing it down for nothing.
func TestEditKeepsOldMappingWhenNewOneFails(t *testing.T) {
	m, fr := testManager(t)
	if _, _, err := openTest(m, 8530); err != nil {
		t.Fatal(err)
	}

	fr.err = errors.New("exit status 255")
	fr.out = "bind [127.0.0.1]:9100: Address already in use"
	newLocal := 9100
	if _, err := m.Edit("code:local-forward:8530", editReq{localPort: &newLocal}); err == nil {
		t.Fatal("edit reported success despite the new mapping failing")
	}

	list := m.List()
	if len(list) != 1 || list[0].LocalPort != 8530 {
		t.Fatalf("original mapping lost after a failed edit: %+v", list)
	}
}

func TestEditUpdatesTTLAndLabelWithoutReforwarding(t *testing.T) {
	m, fr := testManager(t)
	if _, _, err := openTest(m, 8530); err != nil {
		t.Fatal(err)
	}
	fr.mu.Lock()
	before := len(fr.calls)
	fr.mu.Unlock()

	label := "renamed"
	ttl := 5 * time.Minute
	v, err := m.Edit("code:local-forward:8530", editReq{label: &label, ttl: &ttl})
	if err != nil {
		t.Fatal(err)
	}
	if v.Label != "renamed" || v.TTL != 300 {
		t.Fatalf("edit did not apply: %+v", v)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.calls) != before {
		t.Fatalf("metadata-only edit issued %d ssh calls", len(fr.calls)-before)
	}
}

// A remote-forward's listener is on the far side, so reap must consult the remote's
// listening set rather than dialling anything locally.
func TestReapUsesRemoteListenersForRemoteForwards(t *testing.T) {
	m, fr := testManager(t)
	if _, _, err := m.Open(openReq{host: "code", direction: dirRemote, localPort: 3000}); err != nil {
		t.Fatal(err)
	}
	m.alive = func(int) bool { return false } // would condemn it if reap dialled locally

	fr.out = "LISTEN 0 128 127.0.0.1:3000 0.0.0.0:*"
	m.reap(true)
	if n := len(m.List()); n != 1 {
		t.Fatalf("remote-forward reaped while the remote was still listening (%d)", n)
	}

	fr.out = "LISTEN 0 128 127.0.0.1:22 0.0.0.0:*"
	m.reap(true)
	if n := len(m.List()); n != 0 {
		t.Fatalf("remote-forward kept after the remote stopped listening (%d)", n)
	}
}

// Off-cycle, a remote-forward is left alone rather than guessed at.
func TestReapSkipsRemoteForwardsWhenNotChecking(t *testing.T) {
	m, _ := testManager(t)
	if _, _, err := m.Open(openReq{host: "code", direction: dirRemote, localPort: 3000}); err != nil {
		t.Fatal(err)
	}
	m.alive = func(int) bool { return false }
	m.reap(false)
	if n := len(m.List()); n != 1 {
		t.Fatalf("remote-forward reaped on a cycle that did not check it (%d)", n)
	}
}
