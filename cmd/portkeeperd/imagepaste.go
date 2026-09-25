package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Image paste hands the Mac's clipboard image to a Claude Code session on a remote host.
//
// Claude Code on Linux has no clipboard code of its own: on Ctrl+V it runs
// `xclip -selection clipboard -t TARGETS -o || wl-paste -l` to ask whether the clipboard
// holds an image, then `xclip … -t image/png -o || wl-paste --type image/png` to fetch it.
// A dev box reached over ssh has neither program and no display, so nothing is attached.
//
// The daemon closes that gap with three pieces:
//
//   - it reads the pasteboard itself (pasteboard_darwin.go) and serves the image on a
//     unix socket of its own, one per host, with a single read-only route;
//   - it forwards that socket to the host over the host's existing ControlMaster, as
//     `-R <remote sock>:<local sock>`, so the remote end is a mode-0600 socket only the
//     same account can open;
//   - it installs a small `wl-paste` stand-in on the host that fetches from that socket.
//
// It is `wl-paste`, not `xclip`, because Claude Code tries xclip first: the stand-in only
// runs when xclip is missing or fails, so it can never shadow a working X11 clipboard.
//
// This is deliberately NOT a route on the control listener. The README's promise is that
// nothing on the remote can reach the daemon; the clipboard socket is a second, separate
// server that answers one GET and knows nothing else. It never serves clipboard text,
// which is where passwords and tokens live.

const (
	clipRoute    = "/v1/clipboard/image"
	maxClipImage = 20 << 20

	// sunPathMax is the usable length of a unix socket path on macOS (sun_path is 104
	// bytes, NUL included). Linux allows 108; the smaller limit covers both ends.
	sunPathMax = 103

	// shimMarker identifies the stand-in as ours. Install refuses to overwrite a
	// wl-paste without it, and uninstall only ever removes a file that carries it.
	shimMarker = "portkeeper-shim"
)

var errPasteNoRunner = errors.New("this daemon cannot send files to a host")

// safeRemotePath is what a remote socket path may look like before it is spliced into a
// remote command or a forward spec. The path comes from the remote's own shell, so it is
// data, and it is checked like any other value that becomes part of a command: no quote,
// no space, no colon (a colon would re-cut the -R spec), no leading dash.
var safeRemotePath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)

// pasteboard is the Mac's clipboard, as far as the daemon needs it.
type pasteboard interface {
	changeCount() int64
	imagePNG() ([]byte, int64)
}

// clipSource caches the last image read, keyed by the pasteboard's change count. One
// paste asks twice (does it hold an image, then give it to me), and a TIFF-only image
// is re-encoded to PNG on every read, so the second ask should not pay for it again.
type clipSource struct {
	pb pasteboard

	mu    sync.Mutex
	cc    int64
	png   []byte
	valid bool
}

func (c *clipSource) image() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.pb.changeCount() == c.cc {
		return c.png
	}
	png, cc := c.pb.imagePNG()
	c.png, c.cc, c.valid = png, cc, true
	return png
}

// clipHandler is the whole of what a remote host can ask the Mac for.
func clipHandler(host string, src *clipSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != clipRoute {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		png := src.image()
		switch {
		case len(png) == 0:
			w.WriteHeader(http.StatusNoContent)
			return
		case len(png) > maxClipImage:
			http.Error(w, "clipboard image is larger than 20 MB", http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", strconv.Itoa(len(png)))
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		w.Write(png)
		logf("image paste %s: served %d bytes", host, len(png))
	})
}

// clipServer is one host's local clipboard socket.
type clipServer struct {
	path string
	ln   net.Listener
	srv  *http.Server
}

func startClipServer(path, host string, src *clipSource) (*clipServer, error) {
	if len(path) > sunPathMax {
		return nil, fmt.Errorf("clipboard socket path %s is longer than %d bytes", path, sunPathMax)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// A socket left by a previous daemon refuses connections and blocks the bind. Only a
	// socket is removed; anything else at that path is not ours to delete.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	s := &clipServer{path: path, ln: ln, srv: &http.Server{
		Handler:           clipHandler(host, src),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
	}}
	go s.srv.Serve(ln)
	return s, nil
}

func (s *clipServer) close() {
	s.srv.Close()
	os.Remove(s.path)
}

// inputRunner runs an ssh command with something on its stdin, which is how a file is
// written on a host without scp and without interpolating its contents into a command.
type inputRunner interface {
	runInput(argv []string, stdin string) (string, error)
}

func (execRunner) runInput(argv []string, stdin string) (string, error) {
	cmd := exec.Command("ssh", argv...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("ssh %s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// The remote commands. Each is a fixed string; the only values ever spliced in are a
// socket path that has passed safeRemotePath, single-quoted.

// remoteBaseCmd names the directory the host's socket goes under. XDG_RUNTIME_DIR is
// per user and mode 0700 where it exists; ~/.cache is the fallback, never /tmp, where
// another user could squat the name first.
const remoteBaseCmd = `printf '%s' "${XDG_RUNTIME_DIR:-$HOME/.cache}"`

func remotePrepareCmd(sock string) string {
	return fmt.Sprintf(`mkdir -p -m 700 '%s' && rm -f '%s'`, filepath.Dir(sock), sock)
}

func remoteStatCmd(sock string) string { return fmt.Sprintf(`ls -ld '%s'`, sock) }

func remoteRemoveCmd(sock string) string {
	return fmt.Sprintf(`rm -f '%s'; f="$HOME/.local/bin/wl-paste"; `+
		`if [ -f "$f" ] && grep -q %s "$f"; then rm -f "$f"; fi`, sock, shimMarker)
}

// installShimCmd writes the stand-in from stdin. It refuses to replace a wl-paste that
// is not ours, then reports where a login shell finds wl-paste, so a PATH that misses
// ~/.local/bin is caught at install time rather than as a paste that silently does nothing.
const installShimCmd = `set -e; d="$HOME/.local/bin"; f="$d/wl-paste"; ` +
	`if [ -e "$f" ] && ! grep -q ` + shimMarker + ` "$f"; then ` +
	`echo "$f exists and is not Portkeeper's; leaving it alone" >&2; exit 3; fi; ` +
	`mkdir -p "$d"; cat > "$f.portkeeper-tmp"; chmod 755 "$f.portkeeper-tmp"; mv "$f.portkeeper-tmp" "$f"; ` +
	`echo "shim=$f"; echo "found=$("${SHELL:-/bin/sh}" -lc 'command -v wl-paste' 2>/dev/null </dev/null | tail -n 1)"`

// shimScript is the stand-in. POSIX sh and curl only, so it runs on any box that has
// both. Anything that is not a question about a PNG goes to a real wl-paste when there is
// a Wayland session to ask, and fails otherwise, the way a missing wl-paste would.
const shimScript = `#!/bin/sh
# ` + shimMarker + ` v1
# Installed by Portkeeper. It hands the Mac's clipboard image to programs that read the
# clipboard with wl-paste, such as Claude Code. Only a PNG image is served; clipboard
# text never leaves the Mac. Turn image paste off in Portkeeper to remove this file.
SOCK='@SOCK@'
URL=http://portkeeper` + clipRoute + `

real() {
	[ -n "$WAYLAND_DISPLAY" ] || exit 1
	self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")
	IFS=:
	for d in $PATH; do
		c="$d/wl-paste"
		if [ -x "$c" ] && [ "$c" != "$self" ] && ! grep -q ` + shimMarker + ` "$c" 2>/dev/null; then
			exec "$c" "$@"
		fi
	done
	exit 1
}

list=0 type= want=
for a in "$@"; do
	if [ -n "$want" ]; then type=$a; want=; continue; fi
	case $a in
	-l|--list-types) list=1 ;;
	-t|--type) want=1 ;;
	--type=*) type=${a#--type=} ;;
	-n|--no-newline) ;;
	*) real "$@" ;;
	esac
done

[ -S "$SOCK" ] && command -v curl >/dev/null 2>&1 || real "$@"

if [ "$list" = 1 ]; then
	code=$(curl -s -I -o /dev/null -w '%{http_code}' --max-time 5 --unix-socket "$SOCK" "$URL")
	[ "$code" = 200 ] || real "$@"
	echo image/png
	exit 0
fi

[ "$type" = image/png ] || real "$@"
t=$(mktemp) || exit 1
code=$(curl -s -o "$t" -w '%{http_code}' --max-time 30 --unix-socket "$SOCK" "$URL")
if [ "$code" = 200 ]; then
	cat "$t"
	rm -f "$t"
	exit 0
fi
rm -f "$t"
real "$@"
`

func shimFor(sock string) string { return strings.Replace(shimScript, "@SOCK@", sock, 1) }

// pasteHost is what the daemon knows about one host's image paste.
type pasteHost struct {
	srv        *clipServer
	remoteSock string // resolved once per daemon run; empty until then
	placed     bool   // the forward is on the current master
	err        string
	warn       string
}

// imagePaste owns every host's clipboard socket and forward.
type imagePaste struct {
	cfg *Config
	run runner
	src *clipSource

	// opMu serialises the slow work (ssh round trips) so that the reconcile loop and a
	// click in the console never place the same forward twice at once.
	opMu sync.Mutex

	mu      sync.Mutex
	enabled map[string]bool // persisted in cfg.ImagePasteFile
	hosts   map[string]*pasteHost
}

func newImagePaste(cfg *Config, run runner, pb pasteboard) *imagePaste {
	p := &imagePaste{
		cfg:     cfg,
		run:     run,
		src:     &clipSource{pb: pb},
		enabled: map[string]bool{},
		hosts:   map[string]*pasteHost{},
	}
	for _, h := range readPasteHosts(cfg.ImagePasteFile) {
		p.enabled[h] = true
	}
	return p
}

// readPasteHosts loads the persisted set. Like pins, every entry is re-validated: the
// names become ssh argv.
func readPasteHosts(path string) []string {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var loaded []string
	if err := json.Unmarshal(b, &loaded); err != nil {
		logf("ignoring %s: %v", path, err)
		return nil
	}
	var out []string
	for _, h := range loaded {
		if safeAlias.MatchString(h) {
			out = append(out, h)
		}
	}
	return out
}

func (p *imagePaste) saveLocked() error {
	if p.cfg.ImagePasteFile == "" {
		return nil
	}
	list := make([]string, 0, len(p.enabled))
	for h := range p.enabled {
		list = append(list, h)
	}
	sort.Strings(list)
	b, _ := json.MarshalIndent(list, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p.cfg.ImagePasteFile), 0o700); err != nil {
		return err
	}
	tmp := p.cfg.ImagePasteFile + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.cfg.ImagePasteFile)
}

func (p *imagePaste) hostLocked(host string) *pasteHost {
	ph, ok := p.hosts[host]
	if !ok {
		ph = &pasteHost{}
		p.hosts[host] = ph
	}
	return ph
}

// localSock is where a host's clipboard socket lives on the Mac. The alias is hashed so
// that a long one cannot push the path past the unix socket limit.
func (p *imagePaste) localSock(host string) string {
	sum := sha256.Sum256([]byte(host))
	return filepath.Join(p.cfg.ClipDir, hex.EncodeToString(sum[:6])+".sock")
}

func (p *imagePaste) isEnabled(host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled[host]
}

func (p *imagePaste) enabledHosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.enabled))
	for h := range p.enabled {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// masterUp is told whenever a host's master (re)connects. A forward lives on a master,
// so a new master carries none, and the next ensure places it again.
func (p *imagePaste) masterUp(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ph, ok := p.hosts[host]; ok {
		ph.placed = false
	}
}

func (p *imagePaste) isPlaced(host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	ph, ok := p.hosts[host]
	return ok && ph.placed
}

func (p *imagePaste) setErr(host string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ph := p.hostLocked(host)
	if err == nil {
		ph.err = ""
		return
	}
	ph.err = err.Error()
}

// enable turns image paste on for a host whose master is up: it installs the stand-in,
// places the forward, and remembers the choice. Every step is safe to repeat.
func (p *imagePaste) enable(host string) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()

	sock, err := p.resolveRemoteSock(host)
	if err != nil {
		p.setErr(host, err)
		return err
	}
	warn, err := p.installShim(host, sock)
	if err != nil {
		p.setErr(host, err)
		return err
	}
	p.mu.Lock()
	p.hostLocked(host).warn = warn
	p.enabled[host] = true
	saveErr := p.saveLocked()
	p.mu.Unlock()
	if saveErr != nil {
		logf("image paste: could not save %s: %v", p.cfg.ImagePasteFile, saveErr)
	}
	err = p.placeLocked(host)
	p.setErr(host, err)
	return err
}

// disable takes image paste off a host: the forward, the remote socket, the stand-in and
// the local socket all go. A host that cannot be reached right now is still turned off
// here; its master will not carry the forward again, and the stand-in without a socket
// behind it just fails the way a missing wl-paste would.
func (p *imagePaste) disable(host string, reachable bool) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()

	p.mu.Lock()
	delete(p.enabled, host)
	saveErr := p.saveLocked()
	ph := p.hosts[host]
	delete(p.hosts, host)
	p.mu.Unlock()
	if saveErr != nil {
		logf("image paste: could not save %s: %v", p.cfg.ImagePasteFile, saveErr)
	}
	if ph == nil {
		return nil
	}
	var err error
	if reachable && ph.remoteSock != "" {
		if ph.placed && ph.srv != nil {
			p.run.run(p.cfg.sshArgv("-O", "cancel", "-R", ph.remoteSock+":"+ph.srv.path, host))
		}
		if _, rerr := p.run.run(p.cfg.sshArgv(host, remoteRemoveCmd(ph.remoteSock))); rerr != nil {
			err = fmt.Errorf("image paste is off, but cleaning up on %s failed: %w", host, rerr)
		}
	}
	if ph.srv != nil {
		ph.srv.close()
	}
	return err
}

// ensure places a host's forward if its master is up and does not carry one. It is what
// the reconcile loop calls after a master comes back.
func (p *imagePaste) ensure(host string) {
	if !p.isEnabled(host) || p.isPlaced(host) {
		return
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()
	if !p.isEnabled(host) || p.isPlaced(host) {
		return
	}
	err := p.placeLocked(host)
	p.setErr(host, err)
	if err != nil {
		logf("image paste %s: %v", host, err)
	}
}

// resolveRemoteSock asks the host where its socket should go, once per daemon run.
func (p *imagePaste) resolveRemoteSock(host string) (string, error) {
	p.mu.Lock()
	if ph, ok := p.hosts[host]; ok && ph.remoteSock != "" {
		s := ph.remoteSock
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()

	out, err := p.run.run(p.cfg.sshArgv(host, remoteBaseCmd))
	if err != nil {
		return "", fmt.Errorf("could not ask %s where to put the clipboard socket: %w", host, err)
	}
	base := strings.TrimRight(strings.TrimSpace(lastLine(out)), "/")
	sock := base + "/portkeeper/clip.sock"
	if !safeRemotePath.MatchString(base) || strings.Contains(base, "..") {
		return "", fmt.Errorf("%s reported an unusable socket directory %q", host, base)
	}
	if len(sock) > sunPathMax {
		return "", fmt.Errorf("the clipboard socket path on %s (%s) is longer than %d bytes", host, sock, sunPathMax)
	}
	p.mu.Lock()
	p.hostLocked(host).remoteSock = sock
	p.mu.Unlock()
	return sock, nil
}

// installShim writes the stand-in and returns a warning when a login shell on the host
// would not find it.
func (p *imagePaste) installShim(host, sock string) (string, error) {
	ir, ok := p.run.(inputRunner)
	if !ok {
		return "", errPasteNoRunner
	}
	out, err := ir.runInput(p.cfg.sshArgv(host, installShimCmd), shimFor(sock))
	if err != nil {
		if msg := lastLine(out); msg != "" {
			return "", fmt.Errorf("could not install wl-paste on %s: %s", host, msg)
		}
		return "", fmt.Errorf("could not install wl-paste on %s: %w", host, err)
	}
	var shim, found string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "shim="); ok {
			shim = v
		} else if v, ok := strings.CutPrefix(line, "found="); ok {
			found = v
		}
	}
	if shim != "" && found != shim {
		if found == "" {
			found = "nothing"
		}
		return fmt.Sprintf("A login shell on %s finds %s for wl-paste, not %s. Put ~/.local/bin on PATH before other directories.", host, found, shim), nil
	}
	return "", nil
}

// placeLocked starts the local socket if needed and puts the forward on the host's
// master. The caller holds opMu.
func (p *imagePaste) placeLocked(host string) error {
	sock, err := p.resolveRemoteSock(host)
	if err != nil {
		return err
	}

	p.mu.Lock()
	ph := p.hostLocked(host)
	srv := ph.srv
	p.mu.Unlock()
	if srv == nil {
		srv, err = startClipServer(p.localSock(host), host, p.src)
		if err != nil {
			return fmt.Errorf("could not open the local clipboard socket: %w", err)
		}
		p.mu.Lock()
		ph.srv = srv
		p.mu.Unlock()
	}

	p.mu.Lock()
	ph.placed = false
	p.mu.Unlock()

	// Cancel first, whether or not this master carries the forward. The master treats a
	// forward it already has as done and binds nothing, so removing the remote socket
	// under a live forward and asking again would leave no socket at all -- found live
	// by turning image paste on a second time. A cancel of a forward that is not there
	// fails harmlessly.
	spec := sock + ":" + srv.path
	p.run.run(p.cfg.sshArgv("-O", "cancel", "-R", spec, host))

	// sshd does not unlink a leftover socket, and a leftover blocks the bind, so clear
	// it next. The directory is created 0700 so nobody else can list or replace it.
	if _, err := p.run.run(p.cfg.sshArgv(host, remotePrepareCmd(sock))); err != nil {
		return fmt.Errorf("could not prepare %s on %s: %w", filepath.Dir(sock), host, err)
	}
	out, err := p.run.run(p.cfg.sshArgv("-O", "forward", "-R", spec, host))
	// ssh replays ssh_config's own RemoteForward lines on every -O forward, and those
	// fail when the ports are held elsewhere; only a failure that names our path counts.
	if err != nil && (strings.Contains(out, sock) || !forwardErrTolerable(out, 0)) {
		return fmt.Errorf("could not forward the clipboard socket to %s: %s", host, strings.TrimSpace(lastLine(out)))
	}

	// The remote end's mode comes from sshd's StreamLocalBindMask, which defaults to
	// 0177 but is the host's to change. Check it rather than trust it.
	out, err = p.run.run(p.cfg.sshArgv(host, remoteStatCmd(sock)))
	if err != nil {
		return fmt.Errorf("the clipboard socket did not appear on %s: %w", host, err)
	}
	if mode := strings.Fields(lastLine(out)); len(mode) == 0 || mode[0] != "srw-------" {
		p.run.run(p.cfg.sshArgv("-O", "cancel", "-R", spec, host))
		p.run.run(p.cfg.sshArgv(host, remoteRemoveCmd(sock)))
		return fmt.Errorf("%s created the clipboard socket readable by other users (%s); image paste stays off there", host, strings.TrimSpace(lastLine(out)))
	}

	p.mu.Lock()
	ph.placed = true
	p.mu.Unlock()
	logf("image paste %s: forwarded %s", host, sock)
	return nil
}

// forget drops a host that is being removed from the book, cleaning up on it when it
// can still be reached.
func (p *imagePaste) forget(host string, reachable bool) {
	if !p.isEnabled(host) {
		return
	}
	if err := p.disable(host, reachable); err != nil {
		logf("image paste %s: %v", host, err)
	}
}

// shutdown closes every local socket. The masters, and the forwards on them, are
// stopped by the manager.
func (p *imagePaste) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ph := range p.hosts {
		if ph.srv != nil {
			ph.srv.close()
			ph.srv = nil
		}
		ph.placed = false
	}
}

type pasteView struct {
	Enabled bool   `json:"enabled"`
	Active  bool   `json:"active"`
	Error   string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
}

func (p *imagePaste) views() map[string]pasteView {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]pasteView{}
	for h := range p.enabled {
		v := pasteView{Enabled: true}
		if ph, ok := p.hosts[h]; ok {
			v.Active, v.Error, v.Warning = ph.placed, ph.err, ph.warn
		}
		out[h] = v
	}
	return out
}

func (p *imagePaste) view(host string) pasteView {
	if v, ok := p.views()[host]; ok {
		return v
	}
	return pasteView{}
}
