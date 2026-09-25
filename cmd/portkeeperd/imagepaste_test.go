package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakePasteboard struct {
	mu    sync.Mutex
	png   []byte
	cc    int64
	reads int
}

func (f *fakePasteboard) changeCount() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cc
}

func (f *fakePasteboard) imagePNG() ([]byte, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.png, f.cc
}

func (f *fakePasteboard) set(png []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.png = png
	f.cc++
}

var testPNG = []byte("\x89PNG\r\n\x1a\nnot really a png, but bytes are bytes")

func TestClipHandler(t *testing.T) {
	pb := &fakePasteboard{}
	h := clipHandler("code", &clipSource{pb: pb})
	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}

	if rec := do("GET", clipRoute); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("empty pasteboard: got %d with %d bytes, want 204 and nothing", rec.Code, rec.Body.Len())
	}

	pb.set(testPNG)
	rec := do("GET", clipRoute)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), testPNG) {
		t.Fatalf("GET: got %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type %q", ct)
	}
	rec = do("HEAD", clipRoute)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD: got %d with %d bytes, length %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}

	// The socket is reachable from the remote, so everything but one read is refused.
	if rec := do("POST", clipRoute); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: got %d, want 405", rec.Code)
	}
	for _, p := range []string{"/", "/api/forwards", "/v1/clipboard/text", clipRoute + "/x"} {
		if rec := do("GET", p); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s: got %d, want 404", p, rec.Code)
		}
	}

	pb.set(make([]byte, maxClipImage+1))
	if rec := do("GET", clipRoute); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: got %d, want 413", rec.Code)
	}
}

// One paste asks twice; the second ask must not read (and re-encode) the pasteboard again.
func TestClipSourceCachesByChangeCount(t *testing.T) {
	pb := &fakePasteboard{}
	pb.set(testPNG)
	src := &clipSource{pb: pb}
	src.image()
	src.image()
	if pb.reads != 1 {
		t.Fatalf("read the pasteboard %d times for one change, want 1", pb.reads)
	}
	pb.set([]byte("\x89PNGother"))
	if got := src.image(); string(got) != "\x89PNGother" {
		t.Fatalf("after a change: got %q", got)
	}
}

// scriptRunner answers each ssh invocation by what it asks for, and records the order.
type scriptRunner struct {
	mu    sync.Mutex
	calls []string
	base  string // what the remote says its socket directory is
	ls    string // what `ls -ld` prints for the socket
	fwd   string // output of -O forward
	fwdOK bool
	found string // where a login shell finds wl-paste
	stdin string
}

func (s *scriptRunner) run(argv []string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := strings.Join(argv, " ")
	s.calls = append(s.calls, line)
	switch {
	case strings.Contains(line, "XDG_RUNTIME_DIR"):
		return s.base, nil
	case strings.Contains(line, "-O forward"):
		if s.fwdOK {
			return s.fwd, nil
		}
		return s.fwd, errors.New("exit status 255")
	case strings.Contains(line, "ls -ld"):
		return s.ls, nil
	}
	return "", nil
}

func (s *scriptRunner) runInput(argv []string, stdin string) (string, error) {
	s.mu.Lock()
	s.stdin = stdin
	s.mu.Unlock()
	s.run(argv)
	return "shim=/home/u/.local/bin/wl-paste\nfound=" + s.found + "\n", nil
}

func (s *scriptRunner) indexOf(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

func testPaste(t *testing.T, sr *scriptRunner) *imagePaste {
	t.Helper()
	cfg := testCfg()
	dir := shortDir(t)
	cfg.ClipDir = filepath.Join(dir, "clip")
	cfg.ImagePasteFile = filepath.Join(dir, "image-paste")
	p := newImagePaste(cfg, sr, &fakePasteboard{})
	t.Cleanup(p.shutdown)
	return p
}

func goodRunner() *scriptRunner {
	return &scriptRunner{
		base:  "/run/user/1000",
		ls:    "srw------- 1 u u 0 Sep 25 18:00 /run/user/1000/portkeeper/clip.sock",
		fwdOK: true,
		found: "/home/u/.local/bin/wl-paste",
	}
}

func TestEnablePlacesForwardInOrder(t *testing.T) {
	sr := goodRunner()
	p := testPaste(t, sr)
	if err := p.enable("code"); err != nil {
		t.Fatal(err)
	}
	v := p.view("code")
	if !v.Enabled || !v.Active || v.Error != "" || v.Warning != "" {
		t.Fatalf("view %+v", v)
	}

	sock := "/run/user/1000/portkeeper/clip.sock"
	if !strings.Contains(sr.stdin, "SOCK='"+sock+"'") || !strings.Contains(sr.stdin, shimMarker) {
		t.Fatalf("the shim was not written with the resolved socket:\n%s", sr.stdin)
	}
	// Cancel before clearing the remote socket, clear before forwarding, check after.
	// Clearing under a live forward and forwarding again binds nothing; see placeLocked.
	order := []string{"-O cancel -R " + sock, "rm -f '" + sock + "'", "-O forward -R " + sock + ":", "ls -ld '" + sock + "'"}
	last := -1
	for _, step := range order {
		i := sr.indexOf(step)
		if i < 0 || i < last {
			t.Fatalf("step %q at %d, after %d; calls:\n%s", step, i, last, strings.Join(sr.calls, "\n"))
		}
		last = i
	}
	fi, err := os.Stat(p.localSock("code"))
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("local socket: %v %v", fi, err)
	}

	// The choice survives a restart.
	again := newImagePaste(p.cfg, sr, &fakePasteboard{})
	if !again.isEnabled("code") {
		t.Fatal("a new daemon forgot that image paste is on for code")
	}
}

func TestEnableTwiceStaysActive(t *testing.T) {
	sr := goodRunner()
	p := testPaste(t, sr)
	if err := p.enable("code"); err != nil {
		t.Fatal(err)
	}
	if err := p.enable("code"); err != nil {
		t.Fatal(err)
	}
	if !p.view("code").Active {
		t.Fatal("second enable left the host inactive")
	}
}

func TestPlaceRefusesSocketOthersCanOpen(t *testing.T) {
	sr := goodRunner()
	sr.ls = "srwxrwxrwx 1 u u 0 Sep 25 18:00 /run/user/1000/portkeeper/clip.sock"
	p := testPaste(t, sr)
	err := p.enable("code")
	if err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if p.view("code").Active {
		t.Fatal("a world-readable socket was left active")
	}
	if sr.indexOf("-O cancel") < 0 || sr.indexOf("rm -f '/run/user") < 0 {
		t.Fatalf("the forward and socket were not taken down:\n%s", strings.Join(sr.calls, "\n"))
	}
}

func TestForwardFailureNamingOurPathIsReal(t *testing.T) {
	sr := goodRunner()
	sr.fwdOK = false
	sr.fwd = "remote port forwarding failed for listen path /run/user/1000/portkeeper/clip.sock"
	p := testPaste(t, sr)
	if err := p.enable("code"); err == nil {
		t.Fatal("a forward that failed for our own path was taken as working")
	}
}

// ssh_config's RemoteForward lines replay on every -O forward and fail when their ports
// are held elsewhere. That noise must not read as our forward failing.
func TestForwardReplayNoiseIsTolerated(t *testing.T) {
	sr := goodRunner()
	sr.fwdOK = false
	sr.fwd = "Warning: remote port forwarding failed for listen port 9998"
	p := testPaste(t, sr)
	if err := p.enable("code"); err != nil {
		t.Fatalf("replay noise was taken as a failure: %v", err)
	}
}

func TestResolveRejectsUnsafePaths(t *testing.T) {
	for _, base := range []string{"", "relative/dir", "/tmp/a b", "/tmp/a:b", "/tmp/'x'", "/tmp/../etc", "/" + strings.Repeat("d", 120)} {
		sr := goodRunner()
		sr.base = base
		p := testPaste(t, sr)
		if _, err := p.resolveRemoteSock("code"); err == nil {
			t.Errorf("base %q was accepted", base)
		}
	}
}

func TestShimWarnsWhenLoginShellMissesIt(t *testing.T) {
	sr := goodRunner()
	sr.found = "/usr/bin/wl-paste"
	p := testPaste(t, sr)
	if err := p.enable("code"); err != nil {
		t.Fatal(err)
	}
	if w := p.view("code").Warning; !strings.Contains(w, "/usr/bin/wl-paste") {
		t.Fatalf("warning %q", w)
	}
}

func TestDisableCleansUp(t *testing.T) {
	sr := goodRunner()
	p := testPaste(t, sr)
	if err := p.enable("code"); err != nil {
		t.Fatal(err)
	}
	local := p.localSock("code")
	if err := p.disable("code", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("local socket still there: %v", err)
	}
	if p.isEnabled("code") || newImagePaste(p.cfg, sr, &fakePasteboard{}).isEnabled("code") {
		t.Fatal("still enabled after disable")
	}
	if sr.indexOf("grep -q "+shimMarker) < 0 {
		t.Fatal("the stand-in was not removed")
	}
}

// A new master carries no forwards, so a host coming back up must be placed again.
func TestMasterUpMarksForwardGone(t *testing.T) {
	m, _ := testManager(t)
	sr := goodRunner()
	m.paste = testPaste(t, sr)
	if err := m.paste.enable("code"); err != nil {
		t.Fatal(err)
	}
	m.setHealthy("code", false)
	m.setHealthy("code", true)
	if m.paste.isPlaced("code") {
		t.Fatal("still marked placed after the master came back")
	}
	m.ensureImagePaste()
	if !m.paste.isPlaced("code") {
		t.Fatal("ensureImagePaste did not place it again")
	}
}

func TestReadPasteHostsValidates(t *testing.T) {
	f := filepath.Join(t.TempDir(), "image-paste")
	os.WriteFile(f, []byte(`["code", "-oProxyCommand=x", "a b", "dev.box"]`), 0o600)
	got := readPasteHosts(f)
	if strings.Join(got, ",") != "code,dev.box" {
		t.Fatalf("got %v", got)
	}
}

// The shim itself, run by a real sh against a real socket served by clipHandler: the
// exact commands Claude Code runs, with and without an image on the pasteboard.
func TestShimAgainstRealSocket(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	dir := shortDir(t)
	pb := &fakePasteboard{}
	srv, err := startClipServer(filepath.Join(dir, "c.sock"), "code", &clipSource{pb: pb})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	shim := filepath.Join(dir, "wl-paste")
	if err := os.WriteFile(shim, []byte(shimFor(srv.path)), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command("/bin/sh", append([]string{shim}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir} // no WAYLAND_DISPLAY
		return cmd.Output()
	}

	if out, err := run("-l"); err == nil {
		t.Fatalf("-l with no image: exit 0, printed %q", out)
	}
	pb.set(testPNG)
	if out, err := run("-l"); err != nil || strings.TrimSpace(string(out)) != "image/png" {
		t.Fatalf("-l: %q %v", out, err)
	}
	if out, err := run("--type", "image/png"); err != nil || !bytes.Equal(out, testPNG) {
		t.Fatalf("--type image/png: %q %v", out, err)
	}
	if out, err := run("--type=image/png"); err != nil || !bytes.Equal(out, testPNG) {
		t.Fatalf("--type=image/png: %q %v", out, err)
	}
	// Text is never served, and a type we do not serve fails like a missing wl-paste.
	for _, args := range [][]string{{}, {"--type", "text/plain"}, {"--type", "image/bmp"}} {
		if out, err := run(args...); err == nil || len(out) != 0 {
			t.Fatalf("%v: exit 0 or output %q", args, out)
		}
	}
}
