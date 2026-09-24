package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Listen carries the console and the /api routes behind it. It is loopback-only
	// and nothing off this Mac can reach it; requireLoopback enforces that. Its port is
	// also one no forward may use locally; see errSelfForward.
	Listen string

	// EagerHosts (LG_HOSTS) is the set whose masters are opened at startup rather than
	// on first use. It says nothing about authority — every host the book knows is
	// equally reachable from the console — only about when the connection is
	// made. These are the boxes in daily use, so paying for the connection up front
	// means the first forward of the day does not wait for one.
	//
	// Hosts discovered from ssh_config are NOT added here: dialling every alias in an
	// ssh config at startup would be absurd. They are connected on demand instead.
	EagerHosts []string

	// SSHConfigPath is the user's own ssh config. It is read — for discovery, and by
	// ssh through the wrapper's Include — and never written.
	SSHConfigPath string

	// HostsFile holds the hosts added in the console, as ssh_config Host blocks the
	// daemon alone writes. LegacyHostsFile is where the old console kept bare aliases;
	// it is read once, for migration.
	HostsFile       string
	LegacyHostsFile string

	// SSHWrapper is the config every daemon ssh invocation is given with -F. It
	// includes HostsFile, then SSHConfigPath, then the system config, so a stanza added
	// here wins and everything it leaves out still comes from the user's config.
	// Empty means no -F, which only tests use.
	SSHWrapper string

	// KnownHostsFile is where host keys for console-added hosts are recorded, so that
	// nothing new is ever written under ~/.ssh/.
	KnownHostsFile string

	// PinnedFile holds the mappings that are wanted across restarts. Like HostsFile it
	// is configuration, not a cache of ssh's internals — see the comment on `pin`.
	PinnedFile string

	MaxForwards int
	DefaultTTL  time.Duration
	ControlPath string
}

func loadConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	c := &Config{
		Listen:        env("LG_LISTEN", "127.0.0.1:9996"),
		EagerHosts:    splitHosts(env("LG_HOSTS", "")),
		SSHConfigPath: expandHome(env("LG_SSH_CONFIG", "~/.ssh/config"), home),
		HostsFile: expandHome(env("LG_HOSTS_FILE",
			filepath.Join(home, ".config", "portkeeper", "hosts.conf")), home),
		LegacyHostsFile: filepath.Join(home, ".config", "portkeeper", "hosts"),
		SSHWrapper: expandHome(env("LG_SSH_WRAPPER",
			filepath.Join(home, ".config", "portkeeper", "ssh_config")), home),
		KnownHostsFile: expandHome(env("LG_KNOWN_HOSTS",
			filepath.Join(home, ".config", "portkeeper", "known_hosts")), home),
		PinnedFile: expandHome(env("LG_PINNED_FILE",
			filepath.Join(home, ".config", "portkeeper", "pinned")), home),
		MaxForwards: envInt("LG_MAX_FORWARDS", 20),
		DefaultTTL:  time.Duration(envInt("LG_DEFAULT_TTL", 28800)) * time.Second,
		ControlPath: expandHome(env("LG_CONTROL_PATH", "~/.ssh/sockets/portkeeper-%r@%h-%p"), home),
	}

	// No eager hosts is a fine configuration, and the one a downloaded copy starts with:
	// every Host in ssh_config is offered and dialled on first use, and a pin brings its
	// host up at startup anyway. The daemon used to insist on LG_HOSTS, which shipped a
	// phantom "code" to everyone who was not its author.
	if err := requireLoopback(c.Listen); err != nil {
		return nil, err
	}
	return c, nil
}

// requireLoopback refuses any non-loopback listen address. The origin guard refuses
// cross-site browser requests and nothing else — there is no login — so this socket must
// never be offered anywhere but this machine.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address %q is not loopback", addr)
	}
	return nil
}

// defaultHost fills in the host for a request that did not name one. It is only
// meaningful when exactly one host is configured; with several, a request must say which
// one it means.
func (c *Config) defaultHost() string {
	if len(c.EagerHosts) == 1 {
		return c.EagerHosts[0]
	}
	return ""
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v, err := strconv.Atoi(env(k, ""))
	if err != nil {
		return def
	}
	return v
}

func splitHosts(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
