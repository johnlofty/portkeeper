package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// safeAlias is deliberately strict. An alias becomes an argv element handed to ssh, so a
// value like "-oProxyCommand=..." would be read as an OPTION rather than a destination —
// argv already rules out a shell, but it does not stop ssh parsing its own flags. Leading
// dashes are therefore rejected outright, not escaped.
var safeAlias = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]*$`)

var (
	errBadAlias    = errors.New("host must start with a letter, digit or underscore and contain only letters, digits, dot, dash or underscore")
	errHostExists  = errors.New("that host is already known")
	errNotManual   = errors.New("only hosts added in portkeeper can be changed or removed")
	errHostMissing = errors.New("no such host")
)

type hostSource string

const (
	srcEager     hostSource = "eager"      // LG_HOSTS: connected at startup, not on first use
	srcSSHConfig hostSource = "ssh-config" // discovered in ~/.ssh/config
	srcManual    hostSource = "manual"     // added in the console, kept in HostsFile
)

// hostEntry is one machine the console may offer. Source says where it came from, which
// is presentation only — every targetable entry here is equally reachable, and there is
// no other kind.
//
// For a manual host the fields ARE the host: they are what HostsFile says, and the only
// settings the console can write. For the other two they are read from the user's ssh
// config for display, and mean nothing to how ssh connects.
type hostEntry struct {
	Alias          string     `json:"alias"`
	HostName       string     `json:"hostname"`
	User           string     `json:"user"`
	Port           int        `json:"port,omitempty"`
	IdentityFile   string     `json:"identity_file,omitempty"`
	IdentitiesOnly bool       `json:"identities_only,omitempty"`
	ProxyJump      string     `json:"proxy_jump,omitempty"`
	Source         hostSource `json:"source"`

	// Incomplete marks a bare alias carried over from the old console, which never
	// stored where the host was. It is listed so it can be finished, and refused as a
	// target until it is.
	Incomplete bool `json:"incomplete,omitempty"`
	// Shadowed marks a manual host whose alias the user's own ssh config also defines.
	// The daemon uses the portkeeper entry (it comes first in the wrapper); a terminal
	// uses theirs. Said out loud so the two never disagree silently.
	Shadowed bool `json:"shadowed,omitempty"`
}

// fieldError is a refused value, with the field it belongs to so the console can put
// the message next to the right input.
type fieldError struct {
	Field string
	Msg   string
}

func (e *fieldError) Error() string { return e.Msg }

// parseSSHConfig pulls connectable aliases out of an ssh_config.
//
// Patterns are skipped rather than listed: `Host *` and friends are matching rules, not
// machines, and offering "*" as somewhere to forward to would be nonsense. Include is
// followed because split configs are common, with a depth bound so a cycle cannot hang us.
//
// The same parser reads HostsFile back, which is plain ssh_config by construction.
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
		case "match":
			current = nil
		case "hostname":
			for _, i := range current {
				out[i].HostName = val
			}
		case "user":
			for _, i := range current {
				out[i].User = val
			}
		case "port":
			if p, err := strconv.Atoi(val); err == nil {
				for _, i := range current {
					out[i].Port = p
				}
			}
		case "identityfile":
			for _, i := range current {
				if out[i].IdentityFile == "" { // ssh offers every one; show the first
					out[i].IdentityFile = unquote(val)
				}
			}
		case "identitiesonly":
			for _, i := range current {
				out[i].IdentitiesOnly = strings.EqualFold(val, "yes")
			}
		case "proxyjump":
			for _, i := range current {
				out[i].ProxyJump = val
			}
		case "include":
			for _, pat := range strings.Fields(val) {
				pat = unquote(pat)
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
	// A read error part-way through leaves what was parsed; a half-listed config is
	// better than none, and nothing here decides anything that matters on it.
	_ = sc.Err()
	return out
}

// splitKeyword handles both "Key value" and ssh_config's "Key=value" form.
func splitKeyword(line string) (string, string) {
	if i := strings.IndexAny(line, " \t="); i >= 0 {
		return line[:i], strings.TrimSpace(strings.TrimLeft(line[i:], " \t="))
	}
	return line, ""
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// ---- validation -----------------------------------------------------------------------

// Values are validated and never escaped, like aliases. A newline in a value would start
// a new directive in HostsFile, and a quote would end the one it is in, so either is
// refused outright rather than encoded.
func hasUnsafeChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return true
		}
	}
	return false
}

// validHostName accepts a DNS name, an IPv4 address or a bare IPv6 address.
func validHostName(h string) bool {
	if safeAlias.MatchString(h) {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && strings.Contains(h, ":")
}

// validJump checks one ProxyJump hop: [user@]host[:port].
func validJump(hop string) bool {
	if u, rest, ok := strings.Cut(hop, "@"); ok {
		if !safeAlias.MatchString(u) {
			return false
		}
		hop = rest
	}
	host := hop
	if i := strings.LastIndex(hop, ":"); i >= 0 && !strings.Contains(hop[:i], ":") {
		p, err := strconv.Atoi(hop[i+1:])
		if err != nil || p < 1 || p > 65535 {
			return false
		}
		host = hop[:i]
	}
	return safeAlias.MatchString(host)
}

// normalize trims every field and checks it. It returns the first problem found, naming
// its field, or nil. IdentitiesOnly is not decided here; see the API's default.
func (e *hostEntry) normalize(home string) *fieldError {
	e.Alias = strings.TrimSpace(e.Alias)
	e.HostName = strings.TrimSpace(e.HostName)
	e.User = strings.TrimSpace(e.User)
	e.IdentityFile = strings.TrimSpace(e.IdentityFile)
	e.ProxyJump = strings.TrimSpace(e.ProxyJump)

	if !safeAlias.MatchString(e.Alias) {
		return &fieldError{"alias", errBadAlias.Error()}
	}
	switch {
	case e.HostName == "":
		return &fieldError{"hostname", "enter the host's address or DNS name"}
	case hasUnsafeChar(e.HostName) || !validHostName(e.HostName):
		return &fieldError{"hostname", "host name must be a DNS name or an IP address, with no spaces and no leading dash"}
	}
	if e.User != "" && !safeAlias.MatchString(e.User) {
		return &fieldError{"user", "user must start with a letter, digit or underscore and contain only letters, digits, dot, dash or underscore"}
	}
	if e.Port < 0 || e.Port > 65535 {
		return &fieldError{"port", "port must be between 1 and 65535"}
	}
	if e.IdentityFile != "" {
		if hasUnsafeChar(e.IdentityFile) {
			return &fieldError{"identity_file", "identity file path cannot contain quotes, backslashes or control characters"}
		}
		path := e.IdentityFile
		switch {
		case strings.HasPrefix(path, "~/"):
			path = filepath.Join(home, path[2:])
		case !filepath.IsAbs(path):
			return &fieldError{"identity_file", "identity file must be a full path, or start with ~/"}
		}
		if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
			return &fieldError{"identity_file", "no key file at " + e.IdentityFile}
		}
	} else {
		e.IdentitiesOnly = false
	}
	if e.ProxyJump != "" {
		for _, hop := range strings.Split(e.ProxyJump, ",") {
			if !validJump(strings.TrimSpace(hop)) {
				return &fieldError{"proxy_jump", "jump host must be [user@]host[:port], or several separated by commas"}
			}
		}
		e.ProxyJump = strings.ReplaceAll(e.ProxyJump, " ", "")
	}
	return nil
}

// ---- HostsFile ------------------------------------------------------------------------

// renderHostsFile writes the console's hosts as ssh_config. It is the only writer of that
// file and it writes from the struct, keyword by keyword: nothing a request sends can
// become a keyword, which is what keeps ProxyCommand, LocalCommand, Match and Include
// out of it.
//
// Two lines are added to every host and cannot be changed from the console. The daemon
// has no terminal, so a first connection could never answer ssh's host key prompt;
// accept-new trusts the first key and still refuses one that changes. The known_hosts
// line records that key in portkeeper's own file — ssh writes to the first file listed
// and checks them all — so nothing new is ever written under ~/.ssh/.
func renderHostsFile(hosts []hostEntry, knownHosts string) string {
	var b strings.Builder
	b.WriteString("# Hosts added in the Portkeeper console. Written by portkeeperd; edit them in the\n")
	b.WriteString("# console, since changes made here are overwritten.\n")
	for _, h := range hosts {
		if h.Incomplete {
			continue
		}
		fmt.Fprintf(&b, "\nHost %s\n", h.Alias)
		fmt.Fprintf(&b, "  HostName %s\n", h.HostName)
		if h.User != "" {
			fmt.Fprintf(&b, "  User %s\n", h.User)
		}
		if h.Port != 0 {
			fmt.Fprintf(&b, "  Port %d\n", h.Port)
		}
		if h.IdentityFile != "" {
			fmt.Fprintf(&b, "  IdentityFile \"%s\"\n", h.IdentityFile)
			if h.IdentitiesOnly {
				b.WriteString("  IdentitiesOnly yes\n")
			}
		}
		if h.ProxyJump != "" {
			fmt.Fprintf(&b, "  ProxyJump %s\n", h.ProxyJump)
		}
		b.WriteString("  StrictHostKeyChecking accept-new\n")
		if knownHosts != "" {
			fmt.Fprintf(&b, "  UserKnownHostsFile %s ~/.ssh/known_hosts\n", sshQuote(knownHosts))
		}
	}
	return b.String()
}

// ---- the book -------------------------------------------------------------------------

// hostBook is the set of hosts the console may offer. It is configuration rather than
// runtime state — unlike forwards, which mirror ssh's own internals and are deliberately
// not persisted, a host someone added should still be there after a restart.
type hostBook struct {
	cfg       *Config
	mu        sync.Mutex
	manual    []hostEntry
	sshConfig string
	file      string
	legacy    string // the old bare-alias file, until the first save replaces it
}

func newHostBook(cfg *Config, sshConfig, file string) *hostBook {
	b := &hostBook{cfg: cfg, sshConfig: sshConfig, file: file}
	for _, e := range parseSSHConfig(file) {
		e.Source = srcManual
		b.manual = append(b.manual, e)
	}
	return b
}

// importLegacy reads the old console's list of bare aliases. An alias the user's ssh
// config or LG_HOSTS already covers is dropped: it is listed from there, and nothing of
// it was ever stored here. Any other alias is kept as incomplete, since the old console
// accepted it without asking where it was. It returns how many are incomplete.
func (b *hostBook) importLegacy(path string) int {
	aliases := readLines(path)
	if len(aliases) == 0 {
		return 0
	}
	elsewhere := map[string]bool{}
	for _, h := range b.cfg.EagerHosts {
		elsewhere[h] = true
	}
	for _, e := range parseSSHConfig(b.sshConfig) {
		elsewhere[e.Alias] = true
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.legacy = path
	have := map[string]bool{}
	for _, e := range b.manual {
		have[e.Alias] = true
	}
	n := 0
	for _, a := range aliases {
		if elsewhere[a] || have[a] {
			continue
		}
		have[a] = true
		b.manual = append(b.manual, hostEntry{Alias: a, Source: srcManual, Incomplete: true})
		n++
	}
	return n
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
	_ = sc.Err() // a short list is still a list; see parseSSHConfigDepth
	return out
}

// saveLocked writes HostsFile, and settles the old alias file: rewritten with only the
// aliases still waiting for a host name, or removed once none are.
func (b *hostBook) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(b.file), 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(b.file, []byte(renderHostsFile(b.manual, b.cfg.KnownHostsFile))); err != nil {
		return err
	}
	if b.legacy == "" {
		return nil
	}
	var left []string
	for _, e := range b.manual {
		if e.Incomplete {
			left = append(left, e.Alias)
		}
	}
	if len(left) == 0 {
		if err := os.Remove(b.legacy); err != nil && !os.IsNotExist(err) {
			return err
		}
		b.legacy = ""
		return nil
	}
	return writeFileAtomic(b.legacy, []byte(strings.Join(left, "\n")+"\n"))
}

// List merges the three sources, the first mention of an alias winning its source and
// later ones filling in what it left blank: a host named in LG_HOSTS stays an LG_HOSTS
// host even when ssh_config also describes it, and ssh_config supplies the hostname and
// user that make the picker readable.
func (b *hostBook) List() []hostEntry {
	b.mu.Lock()
	manual := append([]hostEntry(nil), b.manual...)
	b.mu.Unlock()

	fromSSH := parseSSHConfig(b.sshConfig)
	inSSH := map[string]bool{}
	for _, e := range fromSSH {
		inSSH[e.Alias] = true
	}

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
			if prev.Port == 0 {
				prev.Port = e.Port
			}
			if prev.IdentityFile == "" {
				prev.IdentityFile = e.IdentityFile
				prev.IdentitiesOnly = e.IdentitiesOnly
			}
			if prev.ProxyJump == "" {
				prev.ProxyJump = e.ProxyJump
			}
			return
		}
		cp := e
		byAlias[e.Alias] = &cp
		order = append(order, e.Alias)
	}

	for _, h := range b.cfg.EagerHosts {
		add(hostEntry{Alias: h, Source: srcEager})
	}
	for _, e := range manual {
		e.Shadowed = !e.Incomplete && inSSH[e.Alias]
		add(e)
	}
	for _, e := range fromSSH {
		add(e)
	}

	sort.Slice(order, func(i, j int) bool {
		a, c := byAlias[order[i]], byAlias[order[j]]
		if ae, ce := a.Source == srcEager, c.Source == srcEager; ae != ce {
			return ae // the configured hosts are the ones in daily use; they come first
		}
		return a.Alias < c.Alias
	})

	out := make([]hostEntry, 0, len(order))
	for _, a := range order {
		out = append(out, *byAlias[a])
	}
	return out
}

// Known reports whether this is a host the daemon may be asked to forward to. An
// incomplete host is listed but not known: ssh has nowhere to send it yet.
func (b *hostBook) Known(alias string) bool {
	for _, e := range b.List() {
		if e.Alias == alias {
			return !e.Incomplete
		}
	}
	return false
}

func (b *hostBook) listed(alias string) bool {
	for _, e := range b.List() {
		if e.Alias == alias {
			return true
		}
	}
	return false
}

// IsManual reports whether the console owns this host, and so may change or remove it.
func (b *hostBook) IsManual(alias string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.indexLocked(alias) >= 0
}

func (b *hostBook) indexLocked(alias string) int {
	for i, e := range b.manual {
		if e.Alias == alias {
			return i
		}
	}
	return -1
}

func (b *hostBook) home() string {
	h, _ := os.UserHomeDir()
	return h
}

// Add records a new host. Its alias must be new to every source: a second definition of a
// name the user's ssh config already has would make the terminal and the daemon mean two
// different machines by one word.
func (b *hostBook) Add(e hostEntry) error {
	if fe := e.normalize(b.home()); fe != nil {
		return fe
	}
	if b.listed(e.Alias) {
		return &fieldError{"alias", errHostExists.Error()}
	}
	e.Source, e.Incomplete, e.Shadowed = srcManual, false, false

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.indexLocked(e.Alias) >= 0 { // lost a race with another Add
		return &fieldError{"alias", errHostExists.Error()}
	}
	b.manual = append(b.manual, e)
	return b.saveLocked()
}

// Update replaces a manual host's settings. The alias is the key and cannot change.
func (b *hostBook) Update(alias string, e hostEntry) error {
	e.Alias = alias
	if fe := e.normalize(b.home()); fe != nil {
		return fe
	}
	e.Source, e.Incomplete, e.Shadowed = srcManual, false, false

	b.mu.Lock()
	defer b.mu.Unlock()
	i := b.indexLocked(alias)
	if i < 0 {
		if b.listedUnlocked(alias) {
			return errNotManual
		}
		return errHostMissing
	}
	b.manual[i] = e
	return b.saveLocked()
}

// listedUnlocked is listed() for a caller already holding b.mu, which List() would
// otherwise deadlock on.
func (b *hostBook) listedUnlocked(alias string) bool {
	for _, h := range b.cfg.EagerHosts {
		if h == alias {
			return true
		}
	}
	for _, e := range parseSSHConfig(b.sshConfig) {
		if e.Alias == alias {
			return true
		}
	}
	return false
}

func (b *hostBook) Remove(alias string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := b.indexLocked(alias)
	if i < 0 {
		// Discovered and LG_HOSTS entries are not ours to delete: one lives in the ssh
		// config, the other in the daemon's environment.
		return errNotManual
	}
	b.manual = append(b.manual[:i], b.manual[i+1:]...)
	return b.saveLocked()
}
