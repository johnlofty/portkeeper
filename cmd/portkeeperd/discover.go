package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// discoverTimeout bounds the one ssh call that runs real programs on the far side.
// `docker ps` against a wedged daemon does not return, and an admin clicking Discover
// must get an answer rather than a handler that never finishes.
const discoverTimeout = 15 * time.Second

// dockerMarker separates the socket listing from the container listing in one round
// trip. A marker rather than two calls: the two halves describe the same instant, and
// a container's published port shows up in both, so they have to be merged anyway.
const dockerMarker = "__LG_DOCKER__"

// dockerRangeCap stops a pathological `-p 1-65535` publish from turning one container
// into a listing nobody can read. The count is what matters, not which ports are lost.
const dockerRangeCap = 64

// listenEntry is one thing listening on a remote host, as far as we can tell from
// whatever combination of ss, netstat, sudo and docker that host actually had.
//
// Forwarded/MappingID/URL are filled in by the handler, not the parser: they describe
// this daemon's table, not the remote. An entry that is already forwarded is MARKED
// rather than hidden — "why is 3000 missing from this list" is a worse question to be
// left with than one row saying it is already patched.
type listenEntry struct {
	Port      int    `json:"port"`
	Address   string `json:"address"`
	Process   string `json:"process"`
	PID       int    `json:"pid"`
	Container string `json:"container"`

	Forwarded bool   `json:"forwarded"`
	MappingID string `json:"mapping_id,omitempty"`
	URL       string `json:"url,omitempty"`
}

var (
	// `users:(("gatewayd",pid=4210,fd=7))`
	ssProcess = regexp.MustCompile(`\(\("([^"]+)",pid=(\d+)`)
	// netstat's `4210/gatewayd`, or `-` when it may not say.
	netstatProcess = regexp.MustCompile(`^(\d+)/(\S+)$`)
	// `0.0.0.0:8080->80/tcp`, `127.0.0.1:9000-9001->9000-9001/tcp`, `:::8080->80/tcp`.
	dockerBinding = regexp.MustCompile(
		`^(?:(\[[0-9A-Fa-f:]+\]|[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+|::|\*):)?(\d+)(?:-(\d+))?->\d+(?:-\d+)?/(\w+)$`)
)

// parseListenerDiscovery turns the output of discoverCmd into a deduplicated listing.
//
// It is deliberately tolerant. The command is a chain of optional stages, so the input
// may be ss rows, netstat rows, both (the sudo retry repeats what the unprivileged run
// already printed, with process names filled in), or neither, with or without a docker
// section. Anything a line cannot be made sense of is skipped rather than guessed at.
func parseListenerDiscovery(out string) []listenEntry {
	var entries []listenEntry
	at := map[string]int{}

	add := func(e listenEntry) {
		if e.Port < 1 || e.Port > maxPort {
			return
		}
		// The identity of a listener is where it listens, not which tool reported it.
		// The sudo retry re-reports every row the first pass produced, and docker
		// re-reports a published port the socket listing has already named.
		k := e.Address + "/" + strconv.Itoa(e.Port)
		if i, ok := at[k]; ok {
			mergeEntry(&entries[i], e)
			return
		}
		at[k] = len(entries)
		entries = append(entries, e)
	}

	inDocker := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, dockerMarker) {
			inDocker = true
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if inDocker {
			for _, e := range parseDockerLine(line) {
				add(e)
			}
			continue
		}
		if e, ok := parseSocketLine(line); ok {
			add(e)
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Port != entries[j].Port {
			return entries[i].Port < entries[j].Port
		}
		return entries[i].Address < entries[j].Address
	})
	return entries
}

// mergeEntry keeps what the first sighting knew and fills in what it did not. A later
// line is never allowed to blank a field: the unprivileged pass and the sudo pass print
// the same rows, and only one of them can name the process.
func mergeEntry(dst *listenEntry, src listenEntry) {
	if dst.Process == "" {
		dst.Process = src.Process
	}
	if dst.PID == 0 {
		dst.PID = src.PID
	}
	if dst.Container == "" {
		dst.Container = src.Container
	}
}

// parseSocketLine reads one row of either `ss -ltnpH` or `netstat -tlnp`, which differ
// in column count and in where the process ends up. The first field tells them apart:
// ss with -H leads with the state, netstat leads with the protocol.
func parseSocketLine(line string) (listenEntry, bool) {
	f := strings.Fields(line)
	if len(f) < 4 {
		return listenEntry{}, false
	}

	var addr, proc string
	switch {
	case strings.EqualFold(f[0], "LISTEN"): // ss
		addr = f[3]
		if len(f) > 5 {
			proc = strings.Join(f[5:], " ")
		}
	case strings.HasPrefix(strings.ToLower(f[0]), "tcp"): // netstat
		if len(f) < 6 || !strings.EqualFold(f[5], "LISTEN") {
			return listenEntry{}, false
		}
		addr = f[3]
		if len(f) > 6 {
			proc = f[6]
		}
	default:
		return listenEntry{}, false // headers, blank lines, sudo's own complaints
	}

	host, port, ok := splitListenAddr(addr)
	if !ok {
		return listenEntry{}, false
	}
	name, pid := parseProcessField(proc)
	return listenEntry{Port: port, Address: host, Process: name, PID: pid}, true
}

func parseProcessField(s string) (string, int) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return "", 0
	}
	if m := ssProcess.FindStringSubmatch(s); m != nil {
		pid, _ := strconv.Atoi(m[2])
		return m[1], pid
	}
	if m := netstatProcess.FindStringSubmatch(s); m != nil {
		pid, _ := strconv.Atoi(m[1])
		return m[2], pid
	}
	return "", 0
}

// splitListenAddr splits `127.0.0.1:9996`, `*:22`, `[::1]:19321` and netstat's `:::22`.
// The brackets go: they are ss's disambiguation syntax, and keeping them would make the
// same listener look like two different addresses depending on which tool saw it.
func splitListenAddr(s string) (string, int, bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, false
	}
	port, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	host := strings.TrimSuffix(strings.TrimPrefix(s[:i], "["), "]")
	if host == "" {
		host = "*"
	}
	return host, port, true
}

// parseDockerLine reads `NAME<TAB>PORTS`, where PORTS is a comma-separated list of
// bindings. Only published tcp bindings count: an unpublished `80/tcp` is not something
// the host is listening on, so forwarding to it would fail.
func parseDockerLine(line string) []listenEntry {
	name, ports, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "\t")
	if !ok {
		return nil
	}
	name = strings.TrimSpace(name)

	var out []listenEntry
	for _, part := range strings.Split(ports, ",") {
		m := dockerBinding.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil || !strings.EqualFold(m[4], "tcp") {
			continue
		}
		host := strings.TrimSuffix(strings.TrimPrefix(m[1], "["), "]")
		// One `-p 8080:80` publish is printed twice, as `0.0.0.0:8080->80/tcp` and
		// `:::8080->80/tcp`. They are the same socket seen from two families, so the
		// wildcard forms are folded together; otherwise every published container port
		// appears in the console as two identical rows with two identical buttons.
		if host == "" || host == "::" || host == "*" {
			host = "0.0.0.0"
		}
		start, _ := strconv.Atoi(m[2])
		end := start
		if m[3] != "" {
			end, _ = strconv.Atoi(m[3])
		}
		if end < start || end-start >= dockerRangeCap {
			end = start
		}
		for p := start; p <= end; p++ {
			out = append(out, listenEntry{Port: p, Address: host, Container: name})
		}
	}
	return out
}

// wildcardAddress reports whether an address means "reachable on the remote's own
// loopback", which is what a forward with no explicit remote_host targets. Anything
// else is a specific interface and has to be named on the spec to be reached.
func wildcardAddress(addr string) bool {
	switch addr {
	case "", "0.0.0.0", "*", "::", "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// discoverListeners asks one host what it is listening on.
func (m *manager) discoverListeners(host string) ([]listenEntry, error) {
	argv := m.cfg.argvDiscover(host)

	var out string
	var err error
	if br, ok := m.run.(boundedRunner); ok {
		out, err = br.runBounded(argv, discoverTimeout)
	} else {
		out, err = m.run.run(argv)
	}
	// The command ends in `|| true` and every stage swallows its own errors, so a
	// non-zero exit means ssh itself failed. Output that parsed is still worth showing.
	if err != nil {
		if entries := parseListenerDiscovery(out); len(entries) > 0 {
			return entries, nil
		}
		return nil, err
	}
	return parseListenerDiscovery(out), nil
}

// markForwarded annotates entries this daemon already has a local-forward for.
func (m *manager) markForwarded(host string, entries []listenEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range entries {
		for _, f := range m.fwds {
			if f.host != host || f.direction != dirLocal || f.remotePort != entries[i].Port {
				continue
			}
			// An empty remote_host means the remote's own localhost, which is what a
			// wildcard or loopback listener answers on.
			if f.remoteHost == "" {
				if !wildcardAddress(entries[i].Address) {
					continue
				}
			} else if f.remoteHost != entries[i].Address {
				continue
			}
			entries[i].Forwarded = true
			entries[i].MappingID = f.id()
			entries[i].URL = f.url()
			break
		}
	}
}
