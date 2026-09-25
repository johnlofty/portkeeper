package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The URL shapes real CLIs send, and the ones that must not count as a loopback login.
func TestLoopbackPort(t *testing.T) {
	cases := []struct {
		url  string
		want int
	}{
		// aws-cli 2.35 auth-code flow
		{"https://oidc.us-east-1.amazonaws.com/authorize?response_type=code&client_id=x&redirect_uri=http%3A%2F%2F127.0.0.1%3A43817%2Foauth%2Fcallback&state=s", 43817},
		// gcloud
		{"https://accounts.google.com/o/oauth2/auth?redirect_uri=http%3A%2F%2Flocalhost%3A8085%2F&client_id=y", 8085},
		{"https://login.microsoftonline.com/common/oauth2/v2.0/authorize?redirect_uri=http://[::1]:5555/", 5555},
		// wrapped: the real authorize URL rides in "continue"
		{"https://accounts.google.com/signin?continue=" + url.QueryEscape("https://accounts.google.com/o/oauth2/auth?redirect_uri=http://localhost:9000/cb"), 9000},
		// not loopback logins
		{"https://oidc.us-east-1.amazonaws.com/authorize?redirect_uri=https://example.com/cb", 0},
		{"https://oidc.us-east-1.amazonaws.com/authorize?redirect_uri=http://127.0.0.1/oauth/callback", 0}, // no port
		{"https://oidc.us-east-1.amazonaws.com/authorize?redirect_uri=http://10.0.0.5:8080/", 0},
		{"https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH", 0},
	}
	for _, c := range cases {
		if got := loopbackPort(mustURL(t, c.url), 2); got != c.want {
			t.Errorf("%s: got %d, want %d", c.url, got, c.want)
		}
	}
}

func TestProviderTrusted(t *testing.T) {
	yes := []string{
		"https://oidc.us-east-1.amazonaws.com/authorize",
		"https://device.sso.eu-west-1.amazonaws.com/",
		"https://d-1234567890.awsapps.com/start",
		"https://accounts.google.com/o/oauth2/auth",
		"https://github.com/login/device",
		"https://microsoft.com/devicelogin",
	}
	no := []string{
		"https://evil.com/?redirect_uri=http://127.0.0.1:1234/",
		"https://oidc.us-east-1.amazonaws.com.evil.com/authorize",
		"https://github.com/torvalds/linux",
		"https://amazonaws.com/",
		"https://s3.amazonaws.com/bucket",
	}
	for _, s := range yes {
		if !providerTrusted(mustURL(t, s), defaultProviders) {
			t.Errorf("%s: not trusted, want trusted", s)
		}
	}
	for _, s := range no {
		if providerTrusted(mustURL(t, s), defaultProviders) {
			t.Errorf("%s: trusted, want refused", s)
		}
	}
}

func loginManager(t *testing.T) (*manager, *fakeRunner, *[]string) {
	t.Helper()
	m, fr := testManager(t)
	m.cfg.MaxForwards = 10
	var opened []string
	m.openURL = func(u string) { opened = append(opened, u) }
	return m, fr, &opened
}

const awsLogin = "https://oidc.us-east-1.amazonaws.com/authorize?response_type=code&redirect_uri=http%3A%2F%2F127.0.0.1%3A43817%2Foauth%2Fcallback"

func TestOpenLoginForwardsTheCallbackPortFirst(t *testing.T) {
	m, fr, opened := loginManager(t)
	res, err := m.openLogin("code", awsLogin)
	if err != nil {
		t.Fatal(err)
	}
	if res.Port != 43817 {
		t.Fatalf("port %d", res.Port)
	}
	if len(*opened) != 1 || (*opened)[0] != awsLogin {
		t.Fatalf("opened %v", *opened)
	}
	var fwd *forward
	for _, f := range m.fwds {
		fwd = f
	}
	if fwd == nil || fwd.localPort != 43817 || fwd.remotePort != 43817 || fwd.direction != dirLocal ||
		fwd.requester != loginRequest || fwd.ttl != loginTTL || fwd.pinned {
		t.Fatalf("forward %+v", fwd)
	}
	if !fr.sawSubcommand("-L") {
		t.Fatal("no -L was placed")
	}
}

func TestOpenLoginDeviceCodeOpensWithoutForward(t *testing.T) {
	m, _, opened := loginManager(t)
	if _, err := m.openLogin("code", "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH"); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 1 || len(m.fwds) != 0 {
		t.Fatalf("opened %v, forwards %d", *opened, len(m.fwds))
	}
}

func TestOpenLoginRefusals(t *testing.T) {
	m, _, opened := loginManager(t)
	for _, s := range []string{
		"http://oidc.us-east-1.amazonaws.com/authorize",         // not https
		"file:///etc/passwd",                                    // not https
		"https://evil.com/?redirect_uri=http://127.0.0.1:2222/", // untrusted, fake redirect
		"https://example.com/",                                  // untrusted plain link
		"https://oidc.us-east-1.amazonaws.com.evil.com/?redirect_uri=http://127.0.0.1:2223/",
	} {
		_, err := m.openLogin("code", s)
		var ce callerError
		if !errors.As(err, &ce) {
			t.Errorf("%s: got %v, want a refusal", s, err)
		}
	}
	if len(*opened) != 0 || len(m.fwds) != 0 {
		t.Fatalf("refused URLs opened %v / forwarded %d", *opened, len(m.fwds))
	}
}

// The port is written into the URL, so a busy Mac port must fail, not move.
func TestOpenLoginBusyPortIsRefusedNotMoved(t *testing.T) {
	m, _, opened := loginManager(t)
	m.free = func(p int) bool { return p != 43817 }
	_, err := m.openLogin("code", awsLogin)
	if !errors.Is(err, errPortBusy) {
		t.Fatalf("got %v, want errPortBusy", err)
	}
	if len(*opened) != 0 || len(m.fwds) != 0 {
		t.Fatal("a busy port still opened or forwarded")
	}
}

func TestOpenLoginKeepsAtMostThreePerHost(t *testing.T) {
	m, _, _ := loginManager(t)
	base := time.Now()
	for i, p := range []int{41001, 41002, 41003, 41004} {
		m.now = func() time.Time { return base.Add(time.Duration(i) * time.Second) }
		u := "https://oidc.us-east-1.amazonaws.com/authorize?redirect_uri=http://127.0.0.1:" + itoa(p) + "/oauth/callback"
		if _, err := m.openLogin("code", u); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.fwds) != maxLoginsHost {
		t.Fatalf("%d forwards, want %d", len(m.fwds), maxLoginsHost)
	}
	for _, f := range m.fwds {
		if f.remotePort == 41001 {
			t.Fatal("the oldest login forward was kept")
		}
	}
}

// After the tool's callback server exits, its forward goes on the next tick.
func TestReapLoginsClosesWhenNothingListens(t *testing.T) {
	m, fr, _ := loginManager(t)
	base := time.Now()
	m.now = func() time.Time { return base }
	if _, err := m.openLogin("code", awsLogin); err != nil {
		t.Fatal(err)
	}
	// Still listening, and past the settle time: kept.
	m.now = func() time.Time { return base.Add(loginSettle + time.Second) }
	fr.out = "LISTEN 0 5 0.0.0.0:43817 0.0.0.0:*\n"
	m.reapLogins()
	if len(m.fwds) != 1 {
		t.Fatal("dropped while the tool was still listening")
	}
	fr.out = "LISTEN 0 128 127.0.0.1:22 0.0.0.0:*\n"
	m.reapLogins()
	if len(m.fwds) != 0 {
		t.Fatal("kept after the tool stopped listening")
	}
}

func TestChannelRoutesFollowTheirToggles(t *testing.T) {
	sr := goodRunner()
	p := testPaste(t, sr)
	p.openLogin = func(host, u string) (openResult, error) { return openResult{Port: 1}, nil }
	h := p.channelHandler("code")
	do := func(method, path, body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec.Code
	}
	if c := do("POST", openRoute, `{"url":"https://x"}`); c != http.StatusNotFound {
		t.Fatalf("open with browser login off: %d", c)
	}
	if c := do("GET", clipRoute, ""); c != http.StatusNotFound {
		t.Fatalf("clipboard with image paste off: %d", c)
	}
	p.login["code"] = true
	if c := do("POST", openRoute, `{"url":"https://x"}`); c != http.StatusAccepted {
		t.Fatalf("open with browser login on: %d", c)
	}
	if c := do("GET", clipRoute, ""); c != http.StatusNotFound {
		t.Fatalf("clipboard still off: %d", c)
	}
}

func TestEnableLoginInstallsThreeStandInsAndKeepsPasteChannel(t *testing.T) {
	sr := goodRunner()
	sr.found = "/home/u/.local/bin/wl-paste" // login shell finds ours for every name in this fake
	p := testPaste(t, sr)
	if err := p.enableLogin("code"); err != nil {
		t.Fatal(err)
	}
	for _, name := range openShimNames {
		if sr.indexOf(`f="$d/`+name+`"`) < 0 {
			t.Errorf("%s was not installed", name)
		}
	}
	if !p.loginView("code").Active {
		t.Fatal("channel not placed")
	}
	// Turning image paste on and off again must leave browser login's channel alone.
	if err := p.enable("code"); err != nil {
		t.Fatal(err)
	}
	if err := p.disable("code", true); err != nil {
		t.Fatal(err)
	}
	if !p.loginOn("code") || !p.loginView("code").Active {
		t.Fatal("turning image paste off took browser login down with it")
	}
	if _, err := os.Stat(p.localSock("code")); err != nil {
		t.Fatalf("local socket gone: %v", err)
	}
}

// The stand-in itself, run by a real sh against a real socket, as each caller would.
func TestOpenShimAgainstRealSocket(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	dir := shortDir(t)
	var got []string
	h := http.NewServeMux()
	h.HandleFunc(openRoute, func(w http.ResponseWriter, r *http.Request) {
		serveOpen(w, r, "code", func(host, u string) (openResult, error) {
			got = append(got, u)
			if strings.Contains(u, "evil") {
				return openResult{}, callerErrf("evil.com is not a trusted sign-in provider in Portkeeper")
			}
			return openResult{}, nil
		})
	})
	srv, err := startClipServer(filepath.Join(dir, "c.sock"), h)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	for _, name := range openShimNames {
		os.WriteFile(filepath.Join(bin, name), []byte(openShimFor(srv.path)), 0o755)
	}
	run := func(name string, args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(bin, name), args...)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	quoted := `https://oidc.us-east-1.amazonaws.com/authorize?a="b"&c=\d`
	for _, name := range openShimNames {
		if out, err := run(name, quoted); err != nil {
			t.Fatalf("%s: %v %s", name, err, out)
		}
	}
	if len(got) != 3 || got[0] != quoted {
		t.Fatalf("daemon got %q", got)
	}
	out, err := run("xdg-open", "https://evil.com/")
	if err == nil || !strings.Contains(out, "not a trusted sign-in provider") {
		t.Fatalf("refusal: err %v, out %q", err, out)
	}
	// Not a URL: handed to a real xdg-open, of which there is none, so it fails.
	if _, err := run("xdg-open", "/tmp/file.txt"); err == nil {
		t.Fatal("a file path was accepted")
	}
	if len(got) != 4 {
		t.Fatalf("a non-URL reached the daemon: %q", got)
	}
}

func TestServeOpenStatuses(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusAccepted},
		{callerErrf("nope"), http.StatusForbidden},
		{errPortBusy, http.StatusConflict},
		{errors.New("ssh broke"), http.StatusBadGateway},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		serveOpen(rec, httptest.NewRequest("POST", openRoute, strings.NewReader(`{"url":"https://x"}`)), "code",
			func(string, string) (openResult, error) { return openResult{Port: 7}, c.err })
		if rec.Code != c.want {
			t.Errorf("%v: got %d, want %d", c.err, rec.Code, c.want)
		}
		if c.err == nil {
			var r openResult
			json.Unmarshal(rec.Body.Bytes(), &r)
			if r.Port != 7 {
				t.Errorf("body %q", rec.Body.String())
			}
		}
	}
}
