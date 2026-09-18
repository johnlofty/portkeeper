package main

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// safeAlias is deliberately strict. An alias becomes an argv element handed to ssh, so a
// value like "-oProxyCommand=..." would be read as an OPTION rather than a destination —
// argv already rules out a shell, but it does not stop ssh parsing its own flags. Leading
// dashes are therefore rejected outright, not escaped.
var safeAlias = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]*$`)

var (
	errBadAlias   = errors.New("host must start with a letter, digit or underscore and contain only letters, digits, dot, dash or underscore")
	errHostExists = errors.New("that host is already known")
	errNotManual  = errors.New("only manually added hosts can be removed")
)

type hostSource string

const (
	srcPublic    hostSource = "public"     // LG_HOSTS: reachable by the public /api
	srcSSHConfig hostSource = "ssh-config" // discovered in ~/.ssh/config
	srcManual    hostSource = "manual"     // typed into the console, persisted
)

type hostEntry struct {
	Alias    string     `json:"alias"`
	HostName string     `json:"hostname"`
	User     string     `json:"user"`
	Source   hostSource `json:"source"`
	// Public marks a host the unauthenticated /api may target. Discovery deliberately
	// does not grant this: a process on a remote VM could otherwise ask for a tunnel to
	// anything in the ssh config, routers and all.
	Public bool `json:"public"`
}

// parseSSHConfig pulls connectable aliases out of an ssh_config.
//
// Patterns are skipped rather than listed: `Host *` and friends are matching rules, not
// machines, and offering "*" as somewhere to forward to would be nonsense. Include is
// followed because split configs are common, with a depth bound so a cycle cannot hang us.
func parseSSHConfig(path string) []hostEntry {
	return parseSSHConfigDepth(path, 0, map[string]bool{})
}

func parseSSHConfigDepth(path string, depth int, seen map[string]bool) []hostEntry {
	if depth > 4 || seen[path] {
		return nil
	}
	seen[path] = true

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []hostEntry
	var current []int // indexes into out that the current Host block applies to

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val := splitKeyword(line)
		switch strings.ToLower(key) {
		case "host":
			current = nil
			for _, pat := range strings.Fields(val) {
				if !safeAlias.MatchString(pat) {
					continue // wildcards, negations, and anything ssh might read as a flag
				}
				current = append(current, len(out))
				out = append(out, hostEntry{Alias: pat, Source: srcSSHConfig})
			}
		case "hostname":
			for _, i := range current {
				out[i].HostName = val
			}
		case "user":
			for _, i := range current {
				out[i].User = val
			}
		case "include":
			for _, pat := range strings.Fields(val) {
				if !filepath.IsAbs(pat) {
					pat = filepath.Join(filepath.Dir(path), pat)
				}
				matches, _ := filepath.Glob(pat)
				for _, inc := range matches {
					out = append(out, parseSSHConfigDepth(inc, depth+1, seen)...)
				}
			}
			current = nil
		}
	}
	return out
}

// splitKeyword handles both "Key value" and ssh_config's "Key=value" form.
func splitKeyword(line string) (string, string) {
	if i := strings.IndexAny(line, " \t="); i >= 0 {
		return line[:i], strings.TrimSpace(strings.TrimLeft(line[i:], " \t="))
	}
	return line, ""
}

// hostBook is the set of hosts the console may offer. It is configuration rather than
// runtime state — unlike forwards, which mirror ssh's own internals and are deliberately
// not persisted, a host someone typed in should still be there after a restart.
type hostBook struct {
	cfg       *Config
	mu        sync.Mutex
	manual    []string
	sshConfig string
	file      string
}

func newHostBook(cfg *Config, sshConfig, file string) *hostBook {
	b := &hostBook{cfg: cfg, sshConfig: sshConfig, file: file}
	b.manual = readLines(file)
	return b
}

func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") || !safeAlias.MatchString(s) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (b *hostBook) save() error {
	if err := os.MkdirAll(filepath.Dir(b.file), 0o700); err != nil {
		return err
	}
	return os.WriteFile(b.file, []byte(strings.Join(b.manual, "\n")+"\n"), 0o600)
}

// List merges the three sources, newest information winning per alias: a host named in
// LG_HOSTS is public even if ssh_config also describes it, and ssh_config supplies the
// hostname and user that make the picker readable.
func (b *hostBook) List() []hostEntry {
	b.mu.Lock()
	manual := append([]string(nil), b.manual...)
	b.mu.Unlock()

	byAlias := map[string]*hostEntry{}
	order := []string{}
	add := func(e hostEntry) {
		if prev, ok := byAlias[e.Alias]; ok {
			if prev.HostName == "" {
				prev.HostName = e.HostName
			}
			if prev.User == "" {
				prev.User = e.User
			}
			return
		}
		cp := e
		byAlias[e.Alias] = &cp
		order = append(order, e.Alias)
	}

	for _, h := range b.cfg.PublicHosts {
		add(hostEntry{Alias: h, Source: srcPublic, Public: true})
	}
	for _, h := range manual {
		add(hostEntry{Alias: h, Source: srcManual})
	}
	for _, e := range parseSSHConfig(b.sshConfig) {
		add(e)
	}

	sort.Slice(order, func(i, j int) bool {
		a, c := byAlias[order[i]], byAlias[order[j]]
		if a.Public != c.Public {
			return a.Public // the hosts that actually work from the remote come first
		}
		return a.Alias < c.Alias
	})

	out := make([]hostEntry, 0, len(order))
	for _, a := range order {
		out = append(out, *byAlias[a])
	}
	return out
}

// Known reports whether an authenticated caller may target this host.
func (b *hostBook) Known(alias string) bool {
	for _, e := range b.List() {
		if e.Alias == alias {
			return true
		}
	}
	return false
}

func (b *hostBook) Add(alias string) error {
	if !safeAlias.MatchString(alias) {
		return errBadAlias
	}
	if b.Known(alias) {
		return errHostExists
	}
	b.mu.Lock()
	b.manual = append(b.manual, alias)
	b.mu.Unlock()
	return b.save()
}

func (b *hostBook) Remove(alias string) error {
	b.mu.Lock()
	kept := make([]string, 0, len(b.manual))
	found := false
	for _, h := range b.manual {
		if h == alias {
			found = true
			continue
		}
		kept = append(kept, h)
	}
	if found {
		b.manual = kept
	}
	b.mu.Unlock()

	if !found {
		// Discovered and LG_HOSTS entries are not ours to delete: one lives in the ssh
		// config, the other in the daemon's environment.
		return errNotManual
	}
	return b.save()
}
