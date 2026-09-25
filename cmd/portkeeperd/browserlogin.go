package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Browser login opens a URL from a remote host in the Mac's browser, and when the URL is
// a loopback login -- its redirect_uri points at 127.0.0.1:<port> on the host -- first
// forwards that port back to the host for the few minutes the login takes.
//
// Nearly every CLI login works this way (RFC 8252): aws sso login, gcloud, az, terraform,
// kubectl oidc-login. The tool listens on a random loopback port, sends that address to
// the provider as redirect_uri, and opens the provider's page. On a headless host nothing
// opens; opening the page on the Mac by hand fails too, because the provider redirects
// the Mac's browser to a port on the Mac where nothing listens.
//
// The request arrives from the host's stand-in (xdg-open, www-browser, portkeeper-open)
// over the host channel socket. That makes this a remote-to-Mac capability, which
// DESIGN.md says only comes back as a decision; the policy below is that decision:
// https only, providers on a trusted list, the callback port bound exactly, local-forward
// only, ten minutes at most, closed as soon as the tool stops listening.

const (
	openRoute     = "/v1/open"
	loginTTL      = 10 * time.Minute
	loginRequest  = "browser-login"
	maxLoginsHost = 3

	// loginSettle is how long a callback forward is left alone before the reaper may
	// close it for having nothing listening behind it: the tool's server is up before it
	// opens the URL, but give a slow host a moment.
	loginSettle = 20 * time.Second
)

var errPortBusy = errors.New("port busy")

// defaultProviders are the sign-in hosts that open without asking. An entry is a host,
// or "*." plus a domain for any subdomain, optionally followed by a path prefix.
var defaultProviders = []string{
	"oidc.*.amazonaws.com", // AWS IAM Identity Center authorize
	"device.sso.*.amazonaws.com",
	"*.awsapps.com",
	"accounts.google.com",
	"login.microsoftonline.com",
	"microsoft.com/devicelogin",
	"github.com/login",
	"app.terraform.io",
}

type openResult struct {
	Port int `json:"port,omitempty"`
}

// serveOpen is the route handler: parse, hand to the manager, map its answer to a status.
func serveOpen(w http.ResponseWriter, r *http.Request, host string, open func(host, url string) (openResult, error)) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		URL string `json:"url"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil || json.Unmarshal(body, &in) != nil || in.URL == "" {
		http.Error(w, `body must be {"url": "..."}`, http.StatusBadRequest)
		return
	}
	res, err := open(host, in.URL)
	var ce callerError
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, res)
	case errors.Is(err, errPortBusy):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.As(err, &ce):
		http.Error(w, err.Error(), http.StatusForbidden)
	default:
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
}

// loopbackPort finds a loopback redirect_uri in a login URL and returns its port, or 0.
// It looks at redirect_uri itself, then at any parameter whose value is itself an https
// URL (a "continue" or "return_to" wrapping the real authorize URL), two levels deep.
func loopbackPort(u *url.URL, depth int) int {
	q := u.Query()
	if p := loopbackRedirect(q.Get("redirect_uri")); p > 0 {
		return p
	}
	if depth == 0 {
		return 0
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the same URL always gives the same answer
	for _, k := range keys {
		for _, v := range q[k] {
			inner, err := url.Parse(v)
			if err != nil || inner.Scheme != "https" || inner.Host == "" {
				continue
			}
			if p := loopbackPort(inner, depth-1); p > 0 {
				return p
			}
		}
	}
	return 0
}

// loopbackRedirect is the port of an http redirect to this machine's loopback, with an
// explicit port. Anything else -- https, another host, no port -- is not one.
func loopbackRedirect(raw string) int {
	if raw == "" {
		return 0
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return 0
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return 0
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil || !inRange(p) {
		return 0
	}
	return p
}

// providerTrusted matches a URL against the trusted list.
func providerTrusted(u *url.URL, list []string) bool {
	host := strings.ToLower(u.Hostname())
	for _, entry := range list {
		pat, path, _ := strings.Cut(strings.ToLower(entry), "/")
		if !hostMatches(host, pat) {
			continue
		}
		if path == "" || strings.HasPrefix(strings.TrimPrefix(strings.ToLower(u.Path), "/"), path) {
			return true
		}
	}
	return false
}

// hostMatches supports "*" as one whole label anywhere ("oidc.*.amazonaws.com") and a
// leading "*." for any number of subdomains ("*.awsapps.com").
func hostMatches(host, pat string) bool {
	if rest, ok := strings.CutPrefix(pat, "*."); ok {
		return host == rest || strings.HasSuffix(host, "."+rest)
	}
	hl, pl := strings.Split(host, "."), strings.Split(pat, ".")
	if len(hl) != len(pl) {
		return false
	}
	for i := range pl {
		if pl[i] != "*" && pl[i] != hl[i] {
			return false
		}
	}
	return true
}

// openLogin is POST /v1/open, from the host's stand-in.
func (m *manager) openLogin(host, raw string) (openResult, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		logf("browser login %s: refused a non-https URL", host)
		return openResult{}, callerErrf("only https URLs are opened on the Mac")
	}
	if !providerTrusted(u, m.loginProviders()) {
		logf("browser login %s: refused %s (not a trusted sign-in provider)", host, u.Hostname())
		return openResult{}, callerErrf("%s is not a trusted sign-in provider in Portkeeper", u.Hostname())
	}

	port := loopbackPort(u, 2)
	if port > 0 {
		if err := m.placeLoginForward(host, u.Hostname(), port); err != nil {
			logf("browser login %s: %s port %d: %v", host, u.Hostname(), port, err)
			return openResult{}, err
		}
	}
	m.openURL(raw)
	if port > 0 {
		logf("browser login %s: opened %s, callback port %d forwarded", host, u.Hostname(), port)
	} else {
		logf("browser login %s: opened %s", host, u.Hostname())
	}
	return openResult{Port: port}, nil
}

func (m *manager) loginProviders() []string { return defaultProviders }

// placeLoginForward maps the Mac's port to the same port on the host. The number is
// written into the URL the browser will be sent to, so no other port will do.
func (m *manager) placeLoginForward(host, provider string, port int) error {
	m.dropOldestLogins(host)
	_, _, err := m.Open(openReq{
		host:       host,
		direction:  dirLocal,
		remotePort: port,
		localPort:  port,
		exactLocal: true,
		ttl:        loginTTL,
		label:      "login: " + provider,
		requester:  loginRequest,
	})
	return err
}

// dropOldestLogins keeps a host under maxLoginsHost callback forwards, oldest first out,
// so a new login always gets its port.
func (m *manager) dropOldestLogins(host string) {
	m.mu.Lock()
	var fs []*forward
	for _, f := range m.fwds {
		if f.host == host && f.requester == loginRequest {
			fs = append(fs, f)
		}
	}
	m.mu.Unlock()
	if len(fs) < maxLoginsHost {
		return
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].createdAt.Before(fs[j].createdAt) })
	for _, f := range fs[:len(fs)-maxLoginsHost+1] {
		m.CloseID(f.id())
	}
}

// reapLogins closes callback forwards whose tool has stopped listening on the host: the
// login finished (or was abandoned), and the port should not stay mapped for the rest of
// its ten minutes. One listener probe per host that has any.
func (m *manager) reapLogins() {
	now := m.now()
	m.mu.Lock()
	byHost := map[string][]*forward{}
	for _, f := range m.fwds {
		if f.requester == loginRequest && now.Sub(f.createdAt) >= loginSettle {
			byHost[f.host] = append(byHost[f.host], f)
		}
	}
	m.mu.Unlock()

	for host, fs := range byHost {
		if !m.isHealthy(host) {
			continue
		}
		listening, ok := m.remoteListeners(host)
		if !ok {
			continue
		}
		for _, f := range fs {
			if !listening[f.remotePort] {
				logf("browser login %s: port %d closed on the host; dropping its forward", host, f.remotePort)
				m.CloseID(f.id())
			}
		}
	}
}

// exactLocalFree is Open's check for a request that must bind one particular port.
func exactLocalFree(port int, taken map[int]bool, free func(int) bool) error {
	if taken[port] || !free(port) {
		return fmt.Errorf("%w: port %d is already in use on the Mac", errPortBusy, port)
	}
	return nil
}

// The stand-in. One script under three names: xdg-open (Go, Node, Rust tools and
// scripts), www-browser (Python's webbrowser with no display, sensible-browser) and
// portkeeper-open (for BROWSER=portkeeper-open). It must return promptly: callers such
// as Python's webbrowser wait for it.
var openShimNames = []string{"portkeeper-open", "xdg-open", "www-browser"}

const openShimScript = `#!/bin/sh
# ` + shimMarker + ` v1 (open)
# Installed by Portkeeper. Opens http(s) links from this host in your Mac's browser, and
# forwards a login's loopback callback port back here for the few minutes it takes. Turn
# Browser login off in Portkeeper to remove this file.
SOCK='@SOCK@'
URL=http://portkeeper` + openRoute + `
name=$(basename "$0")

# Hand over to a real command of the same name, later on PATH, if there is one.
real() {
	[ "$name" = portkeeper-open ] && exit 1
	self=$(cd "$(dirname "$0")" && pwd)/$name
	IFS=:
	for d in $PATH; do
		c="$d/$name"
		if [ -x "$c" ] && [ "$c" != "$self" ] && ! grep -q ` + shimMarker + ` "$c" 2>/dev/null; then
			exec "$c" "$@"
		fi
	done
	exit 1
}

case $1 in
http://*|https://*) ;;
*) real "$@" ;;
esac
# A desktop session on this host opens its own browser.
if [ -n "$DISPLAY$WAYLAND_DISPLAY" ] && [ "$name" != portkeeper-open ]; then
	real "$@"
fi
[ -S "$SOCK" ] && command -v curl >/dev/null 2>&1 || real "$@"

esc=$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')
out=$(curl -s --max-time 5 -w '\n%{http_code}' --unix-socket "$SOCK" \
	-H 'Content-Type: application/json' --data-binary "{\"url\":\"$esc\"}" "$URL")
code=$(printf '%s\n' "$out" | tail -n 1)
case $code in
2??) exit 0 ;;
esac
msg=$(printf '%s\n' "$out" | sed '$d')
echo "portkeeper: ${msg:-no answer from the Mac}" >&2
real "$@"
`

func openShimFor(sock string) string { return strings.Replace(openShimScript, "@SOCK@", sock, 1) }

// enableLogin turns browser login on for a host whose master is up.
func (p *imagePaste) enableLogin(host string) error {
	p.opMu.Lock()
	defer p.opMu.Unlock()

	sock, err := p.resolveRemoteSock(host)
	if err != nil {
		p.setErr(host, err)
		return err
	}
	var warns []string
	for _, name := range openShimNames {
		warn, err := p.installShim(host, name, openShimFor(sock))
		if err != nil {
			p.setErr(host, err)
			return err
		}
		if warn != "" && name != "portkeeper-open" {
			warns = append(warns, warn)
		}
	}
	p.mu.Lock()
	p.hostLocked(host).loginWarn = strings.Join(warns, " ")
	p.login[host] = true
	saveErr := p.saveLocked()
	placed := p.hostLocked(host).placed
	p.mu.Unlock()
	if saveErr != nil {
		logf("browser login: could not save settings: %v", saveErr)
	}
	if placed {
		return nil // the channel is already up for image paste
	}
	err = p.placeLocked(host)
	p.setErr(host, err)
	return err
}

func (p *imagePaste) disableLogin(host string, reachable bool) error {
	return p.turnOff(host, reachable, p.login, openShimNames, "browser login")
}

type loginView struct {
	Enabled bool   `json:"enabled"`
	Active  bool   `json:"active"`
	Error   string `json:"error,omitempty"`
	Warning string `json:"warning,omitempty"`
}

func (p *imagePaste) loginViews() map[string]loginView {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]loginView{}
	for h := range p.login {
		v := loginView{Enabled: true}
		if ph, ok := p.hosts[h]; ok {
			v.Active, v.Error, v.Warning = ph.placed, ph.err, ph.loginWarn
		}
		out[h] = v
	}
	return out
}

func (p *imagePaste) loginView(host string) loginView {
	if v, ok := p.loginViews()[host]; ok {
		return v
	}
	return loginView{}
}
