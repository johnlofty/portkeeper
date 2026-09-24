package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// A pin is a mapping the operator wants to exist. It is not a mapping that exists.
//
// That distinction is what keeps this from contradicting "State — in memory only". The
// forward table mirrors what ssh currently holds, and persisting it would be persisting
// a claim about another process's internals. A pin persists INTENT, the same category as
// a host typed into the console: it is configuration, it is written by an admin, and the
// reconcile loop's job is to make the live table match it again after a restart.
//
// A pin carries no TTL. "Keep this across restarts" and "drop this in eight hours" are
// contradictory instructions, so pinning clears the lease rather than racing it.
type pin struct {
	Host       string `json:"host"`
	Direction  string `json:"direction"`
	LocalPort  int    `json:"local_port"`
	RemotePort int    `json:"remote_port"`
	RemoteHost string `json:"remote_host,omitempty"`
	Label      string `json:"label,omitempty"`
}

func (p pin) key() fwdKey {
	return fwdKey{
		host:       p.Host,
		direction:  direction(p.Direction),
		remoteHost: p.RemoteHost,
		remotePort: p.RemotePort,
	}
}

func (p pin) id() string {
	return forwardID(p.Host, direction(p.Direction), p.RemoteHost, p.RemotePort)
}

func (p pin) sameAs(q pin) bool { return p == q }

// pinBook is the persisted set of pins, at ~/.config/local-gateway/pinned.
type pinBook struct {
	mu   sync.Mutex
	file string
	pins []pin
}

func newPinBook(file string) *pinBook {
	return &pinBook{file: file, pins: readPins(file)}
}

// readPins re-validates everything it loads. The file is written by this daemon, but it
// is an ordinary file in the user's home directory and its contents become ssh argv and
// forward specs; trusting it because we wrote it last time is the wrong instinct.
func readPins(path string) []pin {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil // absent is the normal case
	}
	var loaded []pin
	if err := json.Unmarshal(b, &loaded); err != nil {
		logf("ignoring %s: %v", path, err)
		return nil
	}

	var out []pin
	for _, p := range loaded {
		switch {
		case !safeAlias.MatchString(p.Host):
		case !direction(p.Direction).valid():
		case p.RemoteHost != "" && !safeAlias.MatchString(p.RemoteHost):
		case p.RemoteHost != "" && direction(p.Direction) != dirLocal:
		case !inRange(p.RemotePort) && !(direction(p.Direction) == dirRemote && p.RemotePort == 0):
		case direction(p.Direction) == dirRemote && !inRange(p.LocalPort):
		default:
			out = append(out, p)
			continue
		}
		logf("ignoring a malformed pin in %s: %+v", path, p)
	}
	return out
}

func (b *pinBook) save() error {
	if err := os.MkdirAll(filepath.Dir(b.file), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(b.pins, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(b.file, append(body, '\n'), 0o600)
}

func (b *pinBook) List() []pin {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]pin(nil), b.pins...)
}

func (b *pinBook) Has(k fwdKey) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.pins {
		if p.key() == k {
			return true
		}
	}
	return false
}

// Add records a pin, replacing any existing one with the same key. An identical pin is
// not rewritten: reconcile re-asserts pins every tick, and rewriting the file thirty
// times a minute to no effect is the kind of thing that eventually loses a file.
func (b *pinBook) Add(p pin) error {
	b.mu.Lock()
	for i, q := range b.pins {
		if q.key() != p.key() {
			continue
		}
		if q.sameAs(p) {
			b.mu.Unlock()
			return nil
		}
		b.pins[i] = p
		b.mu.Unlock()
		return b.save()
	}
	b.pins = append(b.pins, p)
	b.mu.Unlock()
	return b.save()
}

// Remove drops a pin and reports whether there was one.
func (b *pinBook) Remove(k fwdKey) (bool, error) {
	b.mu.Lock()
	kept := make([]pin, 0, len(b.pins))
	found := false
	for _, p := range b.pins {
		if p.key() == k {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		b.mu.Unlock()
		return false, nil
	}
	b.pins = kept
	b.mu.Unlock()
	return true, b.save()
}
