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
	// Listen carries the console and the /admin routes behind it. It is loopback-only
	// and nothing off this Mac can reach it; requireLoopback enforces that.
	Listen string

	// EagerHosts (LG_HOSTS) is the set whose masters are opened at startup rather than
	// on first use. It says nothing about authority — every host the book knows is
	// equally reachable to an authenticated caller — only about when the connection is
	// made. These are the boxes in daily use, so paying for the connection up front
	// means the first forward of the day does not wait for one.
	//
	// Hosts discovered from ssh_config are NOT added here: dialling every alias in an
	// ssh config at startup would be absurd. They are connected on demand instead.
	EagerHosts []string

	SSHConfigPath string
	HostsFile     string

	// PinnedFile holds the mappings that are wanted across restarts. Like HostsFile it
	// is configuration, not a cache of ssh's internals — see the comment on `pin`.
	PinnedFile string

	MaxForwards int
	DefaultTTL  time.Duration
	ControlPath string

	// AdminPassword guards everything. Empty means admin is disabled, every route but
	// `GET /` fails closed, and the daemon can do nothing at all until one is set.
	//
	// Neither source here is the right long-term home: an environment variable is
	// visible to anything that can read the process environment, and a file is only as
	// good as its mode. The macOS Keychain is where this belongs once the shape settles.
	AdminPassword string
}

func (c *Config) adminEnabled() bool { return c.AdminPassword != "" }

// loadAdminPassword takes the secret from the environment, or failing that from a file.
//
// The file exists because the launchd plist is tracked in git: putting the password in
// the plist's EnvironmentVariables dict would leave it one `git add` away from being
// committed. The env var still wins, for `make run` and for tests.
//
// Nothing here logs the password, a prefix of it, or its length.
func loadAdminPassword(path string) string {
	if v := os.Getenv("LG_ADMIN_PASSWORD"); v != "" {
		return v
	}

	fi, err := os.Stat(path)
	if err != nil {
		return "" // absent is the normal case; adminEnabled() reports it once at startup
	}
	// On a machine whose threat model is "other local processes", a group- or
	// world-readable password file is worse than no password, because it looks like
	// protection. Refuse it loudly rather than quietly accepting it.
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		logf("ignoring %s: mode %04o is readable beyond the owner; run: chmod 600 %s", path, perm, path)
		return ""
	}

	b, err := os.ReadFile(path)
	if err != nil {
		logf("ignoring %s: %v", path, err)
		return ""
	}
	return trimOneNewline(string(b))
}

// trimOneNewline removes exactly one trailing line ending, because these files get made
// with `echo`. Nothing else is trimmed: leading or inner whitespace may be deliberate,
// and silently eating it would make a correct password fail for no visible reason.
func trimOneNewline(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}

func loadConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	c := &Config{
		Listen:        env("LG_LISTEN", "127.0.0.1:9996"),
		EagerHosts:    splitHosts(env("LG_HOSTS", "code")),
		SSHConfigPath: expandHome(env("LG_SSH_CONFIG", "~/.ssh/config"), home),
		HostsFile: expandHome(env("LG_HOSTS_FILE",
			filepath.Join(home, ".config", "local-gateway", "hosts")), home),
		PinnedFile: expandHome(env("LG_PINNED_FILE",
			filepath.Join(home, ".config", "local-gateway", "pinned")), home),
		MaxForwards: envInt("LG_MAX_FORWARDS", 20),
		DefaultTTL:  time.Duration(envInt("LG_DEFAULT_TTL", 28800)) * time.Second,
		ControlPath: expandHome(env("LG_CONTROL_PATH", "~/.ssh/sockets/local-gateway-%r@%h-%p"), home),
		AdminPassword: loadAdminPassword(
			expandHome(env("LG_ADMIN_PASSWORD_FILE",
				filepath.Join(home, ".config", "local-gateway", "admin-password")), home)),
	}

	if len(c.EagerHosts) == 0 {
		return nil, fmt.Errorf("LG_HOSTS is empty: nothing to forward to")
	}
	if err := requireLoopback(c.Listen); err != nil {
		return nil, err
	}
	return c, nil
}

// requireLoopback refuses any non-loopback listen address. A session cookie is the only
// thing guarding this socket, so it must never be offered anywhere but this machine.
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
