package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func stubFingerprint(t *testing.T, fp string) {
	t.Helper()
	orig := fingerprintFor
	fingerprintFor = func([]string, string) string { return fp }
	t.Cleanup(func() { fingerprintFor = orig })
}

// bookIn builds a book whose every file lives in one temp dir, with a real key file to
// point IdentityFile at.
func bookIn(t *testing.T, sshConfig string) (*hostBook, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		EagerHosts:     []string{"code"},
		SSHConfigPath:  filepath.Join(dir, "user_ssh_config"),
		HostsFile:      filepath.Join(dir, "portkeeper", "hosts.conf"),
		KnownHostsFile: filepath.Join(dir, "portkeeper", "known_hosts"),
	}
	if sshConfig != "" {
		writeFile(t, dir, "user_ssh_config", sshConfig)
	}
	writeFile(t, dir, "id_test", "not really a key\n")
	return newHostBook(cfg, cfg.SSHConfigPath, cfg.HostsFile), dir
}

// What is written is what is read back: every field survives the file, and nothing but
// the fields plus the two fixed lines is in it.
func TestHostsFileRoundTrip(t *testing.T) {
	book, dir := bookIn(t, "")
	want := hostEntry{
		Alias: "pi2", HostName: "192.168.31.250", User: "pi", Port: 2222,
		IdentityFile: filepath.Join(dir, "id_test"), IdentitiesOnly: true,
		ProxyJump: "jump@bastion:2200,edge", Source: srcManual,
	}
	if err := book.Add(want); err != nil {
		t.Fatal(err)
	}

	got := parseSSHConfig(book.file)
	if len(got) != 1 {
		t.Fatalf("hosts file holds %d hosts, want 1", len(got))
	}
	got[0].Source = srcManual
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("round trip changed the host:\n got %+v\nwant %+v", got[0], want)
	}

	raw, _ := os.ReadFile(book.file)
	body := string(raw)
	for _, line := range []string{
		"  StrictHostKeyChecking accept-new\n",
		"  UserKnownHostsFile " + book.cfg.KnownHostsFile + " ~/.ssh/known_hosts\n",
	} {
		if !strings.Contains(body, line) {
			t.Errorf("hosts file is missing %q:\n%s", line, body)
		}
	}
	if fi, _ := os.Stat(book.file); fi.Mode().Perm() != 0o600 {
		t.Errorf("hosts file mode %v, want 0600: ssh refuses a config others can write", fi.Mode().Perm())
	}

	reloaded := newHostBook(book.cfg, book.sshConfig, book.file)
	if e := reloaded.List()[1]; e.ProxyJump != want.ProxyJump || e.Port != 2222 {
		t.Fatalf("a restart lost settings: %+v", e)
	}
}

// Every field refuses what could change the file's meaning: a newline would start a new
// directive, a quote would end the value, and a leading dash would be read as a flag.
func TestHostFieldsRefuseInjection(t *testing.T) {
	book, dir := bookIn(t, "")
	key := filepath.Join(dir, "id_test")
	ok := hostEntry{Alias: "box", HostName: "10.0.0.2"}

	cases := []struct {
		field string
		edit  func(*hostEntry)
	}{
		{"alias", func(e *hostEntry) { e.Alias = "box\nProxyCommand evil" }},
		{"alias", func(e *hostEntry) { e.Alias = "-oProxyCommand=evil" }},
		{"hostname", func(e *hostEntry) { e.HostName = "" }},
		{"hostname", func(e *hostEntry) { e.HostName = "10.0.0.2\nProxyCommand evil" }},
		{"hostname", func(e *hostEntry) { e.HostName = "-oProxyCommand=evil" }},
		{"hostname", func(e *hostEntry) { e.HostName = "a host" }},
		{"hostname", func(e *hostEntry) { e.HostName = `a"b` }},
		{"user", func(e *hostEntry) { e.User = "pi\nProxyCommand evil" }},
		{"user", func(e *hostEntry) { e.User = "-l" }},
		{"port", func(e *hostEntry) { e.Port = 70000 }},
		{"port", func(e *hostEntry) { e.Port = -1 }},
		{"identity_file", func(e *hostEntry) { e.IdentityFile = key + "\nProxyCommand evil" }},
		{"identity_file", func(e *hostEntry) { e.IdentityFile = key + `"` }},
		{"identity_file", func(e *hostEntry) { e.IdentityFile = "id_test" }},
		{"identity_file", func(e *hostEntry) { e.IdentityFile = filepath.Join(dir, "no_such_key") }},
		{"identity_file", func(e *hostEntry) { e.IdentityFile = dir }},
		{"proxy_jump", func(e *hostEntry) { e.ProxyJump = "bastion\nProxyCommand evil" }},
		{"proxy_jump", func(e *hostEntry) { e.ProxyJump = "-oProxyCommand=evil" }},
		{"proxy_jump", func(e *hostEntry) { e.ProxyJump = "bastion:99999" }},
	}
	for _, c := range cases {
		e := ok
		c.edit(&e)
		err := book.Add(e)
		var fe *fieldError
		if !errors.As(err, &fe) || fe.Field != c.field {
			t.Errorf("%+v: got %v, want a refusal on %s", e, err, c.field)
		}
	}
	if list := book.List(); len(list) != 1 {
		t.Fatalf("a refused host was stored: %+v", list)
	}
	if _, err := os.Stat(book.file); !os.IsNotExist(err) {
		t.Fatalf("a refused host wrote the hosts file")
	}

	for _, h := range []string{"10.0.0.2", "box.example.com", "fe80::1", "my_box"} {
		e := ok
		e.Alias, e.HostName = "ok"+strings.NewReplacer(".", "", ":", "").Replace(h), h
		if err := book.Add(e); err != nil {
			t.Errorf("host name %q was refused: %v", h, err)
		}
	}
}

// The rule the whole design rests on: the user's ssh config is read, never written.
func TestUserSSHConfigIsNeverWritten(t *testing.T) {
	book, _ := bookIn(t, "Host pi\n  HostName 192.168.31.250\n\nHost *\n  User me\n")
	path := book.sshConfig
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	if err := book.Add(hostEntry{Alias: "pi2", HostName: "192.168.31.250"}); err != nil {
		t.Fatal(err)
	}
	if err := book.Update("pi2", hostEntry{HostName: "192.168.31.251", User: "pi"}); err != nil {
		t.Fatal(err)
	}
	if err := book.Remove("pi2"); err != nil {
		t.Fatal(err)
	}

	after, _ := os.ReadFile(path)
	fi, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !fi.ModTime().Equal(old) {
		t.Fatalf("the user's ssh config was touched (modified %v, was %v)", fi.ModTime(), old)
	}
}

// Real ssh, reading the real wrapper: a console host's settings win over the user's
// `Host *` defaults, and whatever the host leaves out still comes from their config.
func TestWrapperPrecedenceWithRealSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh on PATH")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "with space") // config paths with spaces must survive quoting
	cfg := &Config{
		SSHConfigPath:  filepath.Join(dir, "user_config"),
		HostsFile:      filepath.Join(dir, "portkeeper", "hosts.conf"),
		KnownHostsFile: filepath.Join(dir, "portkeeper", "known_hosts"),
		SSHWrapper:     filepath.Join(dir, "portkeeper", "ssh_config"),
		ControlPath:    filepath.Join(base, "cp-%r@%h-%p"), // an -o value, which ssh splits at spaces
	}
	if err := writeSSHWrapper(cfg); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "user_config", "Host *\n  User defaultuser\n  ServerAliveInterval 17\n")
	book := newHostBook(cfg, cfg.SSHConfigPath, cfg.HostsFile)
	if err := book.Add(hostEntry{Alias: "box2", HostName: "10.9.8.7", User: "pk", Port: 2222}); err != nil {
		t.Fatal(err)
	}

	resolve := func(host string) map[string]string {
		out, err := exec.Command("ssh", cfg.sshArgv("-G", host)...).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh -G %s: %v: %s", host, err, out)
		}
		got := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, " "); ok {
				got[k] = v
			}
		}
		return got
	}

	box := resolve("box2")
	for k, want := range map[string]string{
		"hostname": "10.9.8.7", "user": "pk", "port": "2222",
		"serveraliveinterval":   "17",
		"stricthostkeychecking": "accept-new",
	} {
		if box[k] != want {
			t.Errorf("box2 %s = %q, want %q", k, box[k], want)
		}
	}
	if !strings.HasPrefix(box["userknownhostsfile"], cfg.KnownHostsFile+" ") {
		t.Errorf("box2 would record new keys in %q, want portkeeper's own file first", box["userknownhostsfile"])
	}
	if other := resolve("elsewhere"); other["user"] != "defaultuser" {
		t.Errorf("a host the console does not own lost the user's defaults: user = %q", other["user"])
	}
}

// The old console stored bare aliases. One the user's config already has is dropped; any
// other is listed as incomplete, refused as a target, and finished by an edit — at which
// point the old file goes.
func TestLegacyAliasesAreMigrated(t *testing.T) {
	book, dir := bookIn(t, "Host pi\n  HostName 192.168.31.250\n")
	legacy := writeFile(t, dir, "hosts", "pi\nbox9\n")

	if n := book.importLegacy(legacy); n != 1 {
		t.Fatalf("importLegacy = %d incomplete, want 1 (box9)", n)
	}
	var box9 *hostEntry
	for _, e := range book.List() {
		if e.Alias == "box9" {
			e := e
			box9 = &e
		}
		if e.Alias == "pi" && e.Source != srcSSHConfig {
			t.Errorf("pi should stay an ssh-config host, got %s", e.Source)
		}
	}
	if box9 == nil || !box9.Incomplete || box9.Source != srcManual {
		t.Fatalf("box9 should be listed as an incomplete manual host: %+v", box9)
	}
	if book.Known("box9") {
		t.Fatal("an incomplete host is a target; ssh has nowhere to send it")
	}

	if err := book.Update("box9", hostEntry{HostName: "10.0.0.9"}); err != nil {
		t.Fatal(err)
	}
	if !book.Known("box9") {
		t.Fatal("box9 is still not a target after it got a host name")
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("the old alias file should be gone once nothing is waiting in it")
	}
}

func TestShadowedHostIsFlagged(t *testing.T) {
	book, dir := bookIn(t, "")
	if err := book.Add(hostEntry{Alias: "pi2", HostName: "10.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "user_ssh_config", "Host pi2\n  HostName 10.0.0.99\n")
	for _, e := range book.List() {
		if e.Alias == "pi2" && (!e.Shadowed || e.HostName != "10.0.0.2") {
			t.Fatalf("pi2 should be flagged as shadowed and keep its own host name: %+v", e)
		}
	}
	if err := book.Add(hostEntry{Alias: "pi2", HostName: "10.0.0.3"}); err == nil {
		t.Fatal("added a second definition of an alias")
	}
}

func TestOnlyConsoleHostsCanBeEdited(t *testing.T) {
	book, _ := bookIn(t, "Host pi\n")
	for _, alias := range []string{"code", "pi"} {
		if err := book.Update(alias, hostEntry{HostName: "10.0.0.2"}); !errors.Is(err, errNotManual) {
			t.Errorf("Update(%s) = %v, want errNotManual", alias, err)
		}
	}
	if err := book.Update("nosuch", hostEntry{HostName: "10.0.0.2"}); !errors.Is(err, errHostMissing) {
		t.Errorf("Update(nosuch) = %v, want errHostMissing", err)
	}
}

// A host with mappings or pins cannot be removed: a pin would retry against a host the
// book no longer knows, forever.
func TestRemoveRefusesAHostInUse(t *testing.T) {
	h, m, _ := testServer(t)
	if w := do(t, h, "POST", "/api/hosts", `{"alias":"box2","hostname":"10.0.0.2"}`); w.Code != 200 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	m.masters["box2"] = &fakeMaster{up: true}
	m.setHealthy("box2", true)
	if w := post(t, h, `{"host":"box2","remote_port":8530}`); w.Code != 200 {
		t.Fatalf("open: %d %s", w.Code, w.Body)
	}

	w := do(t, h, "DELETE", "/api/hosts/box2", "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "1 mapping") {
		t.Fatalf("remove with a live mapping: %d %s, want 409 naming it", w.Code, w.Body)
	}

	if w := do(t, h, "DELETE", "/api/forward/box2:local-forward:8530", ""); w.Code != 200 {
		t.Fatalf("close: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "DELETE", "/api/hosts/box2", ""); w.Code != 200 {
		t.Fatalf("remove once unused: %d %s", w.Code, w.Body)
	}
	if _, ok := m.masters["box2"]; ok {
		t.Fatal("a removed host kept its master")
	}
}

// An edit drops the host's connection, so the next one is made with the new settings.
func TestEditRestartsTheHostsMaster(t *testing.T) {
	h, m, _ := testServer(t)
	do(t, h, "POST", "/api/hosts", `{"alias":"box2","hostname":"10.0.0.2"}`)
	fm := &fakeMaster{up: true}
	m.masters["box2"] = fm

	if w := do(t, h, "PUT", "/api/hosts/box2", `{"hostname":"10.0.0.3"}`); w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body)
	}
	if fm.up {
		t.Fatal("the master kept running with the old settings")
	}
	var e hostEntry
	w := do(t, h, "GET", "/api/hosts", "")
	var list []hostEntry
	json.Unmarshal(w.Body.Bytes(), &list)
	for _, x := range list {
		if x.Alias == "box2" {
			e = x
		}
	}
	if e.HostName != "10.0.0.3" || !e.IdentitiesOnly == (e.IdentityFile != "") {
		t.Fatalf("edit did not take: %+v", e)
	}

	if w := do(t, h, "PUT", "/api/hosts/box2", `{"alias":"other","hostname":"10.0.0.3"}`); w.Code != 400 {
		t.Fatalf("renaming through an edit: %d, want 400", w.Code)
	}
}

func TestFieldErrorNamesTheField(t *testing.T) {
	h, _, _ := testServer(t)
	w := do(t, h, "POST", "/api/hosts", `{"alias":"box2","hostname":"10.0.0.2","port":70000}`)
	var got struct{ Error, Field string }
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 400 || got.Field != "port" || got.Error == "" {
		t.Fatalf("got %d %s, want 400 with field port", w.Code, w.Body)
	}
}

// ssh's own last line is the answer, and a key refusal says what the daemon cannot do.
func TestConnectionTestReportsSSHsReason(t *testing.T) {
	h, _, fr := testServer(t)
	stubFingerprint(t, "SHA256:abc")
	do(t, h, "POST", "/api/hosts", `{"alias":"box2","hostname":"10.0.0.2"}`)

	w := do(t, h, "POST", "/api/hosts/box2/test", "")
	if !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), "SHA256:abc") {
		t.Fatalf("a working host: %s", w.Body)
	}
	last := fr.calls[len(fr.calls)-1]
	if !hasOpt(last, "ControlPath=none") {
		t.Fatalf("the test ran through a master: %v", last)
	}

	fr.err, fr.out = errors.New("exit status 255"), "debug noise\nbox2: Permission denied (publickey).\n"
	w = do(t, h, "POST", "/api/hosts/box2/test", "")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "Permission denied (publickey)") || !strings.Contains(body, "agent") {
		t.Fatalf("a refused key: %d %s", w.Code, body)
	}
}

func TestListIdentitiesNamesKeysOnly(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"id_ed25519", "id_ed25519.pub", "work", "work.pub", "config", "known_hosts", "id_rsa"} {
		writeFile(t, dir, n, "x")
	}
	os.Mkdir(filepath.Join(dir, "sockets"), 0o700)
	got := listIdentities(dir)
	want := []string{"~/.ssh/id_ed25519", "~/.ssh/id_rsa", "~/.ssh/work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listIdentities = %v, want %v", got, want)
	}
}

// LG_HOSTS_FILE pointed at the old default must not migrate the file onto itself: that
// would write hosts.conf and then overwrite it with the leftover aliases.
func TestHostsFileAtTheOldPathIsNotMigratedOntoItself(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LG_HOSTS_FILE", filepath.Join(dir, "hosts"))
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LegacyHostsFile != "" {
		t.Fatalf("legacy file = %q, want none when it is the hosts file", cfg.LegacyHostsFile)
	}

	book := newHostBook(cfg, filepath.Join(dir, "none"), cfg.HostsFile)
	if err := book.Add(hostEntry{Alias: "box2", HostName: "10.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	book.importLegacy(cfg.HostsFile) // a caller getting it wrong anyway
	if err := book.Update("box2", hostEntry{HostName: "10.0.0.3"}); err != nil {
		t.Fatal(err)
	}
	if !newHostBook(cfg, filepath.Join(dir, "none"), cfg.HostsFile).Known("box2") {
		t.Fatal("the host was lost: the hosts file was overwritten by a migration")
	}
}
