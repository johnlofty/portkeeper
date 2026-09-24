package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	localgateway "github.com/johnlofty/portkeeper"
)

const (
	maxBody = 64 << 10

	// maxLoggedHosts bounds the set of refused Host values remembered for log-once. A
	// page doing DNS rebinding can mint names as fast as it likes; the log must not grow
	// with it.
	maxLoggedHosts = 64
)

var (
	errOpenWithRange = errors.New("open cannot be combined with a port range: it would put one browser window on the screen per port")
	errRangeNeedsLow = errors.New("local_port_end needs local_port: a range has to start somewhere")
)

type forwardReq struct {
	Direction  string `json:"direction"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
	LocalPort  int    `json:"local_port"`
	// *_end turn one request into a range. Absent means "just the one port", which is
	// what every caller that predates the field sends.
	RemotePortEnd int    `json:"remote_port_end"`
	LocalPortEnd  int    `json:"local_port_end"`
	Host          string `json:"host"`
	Label         string `json:"label"`
	Open          bool   `json:"open"`
	Pinned        bool   `json:"pinned"`
	TTL           *int   `json:"ttl"`
	Requester     string `json:"requester"`
}

type editBody struct {
	Label      *string `json:"label"`
	TTL        *int    `json:"ttl"`
	LocalPort  *int    `json:"local_port"`
	RemotePort *int    `json:"remote_port"`
	RemoteHost *string `json:"remote_host"`
	Pinned     *bool   `json:"pinned"`
}

type server struct {
	m   *manager
	cfg *Config

	// hosts is every Host header this daemon answers to: its own listen address, and
	// the two loopback names for the same port. Anything else is refused before routing.
	hosts map[string]bool

	logMu     sync.Mutex
	loggedBad map[string]bool
}

// newServer wires one listener behind one guard.
//
// There is no login. The daemon is driven only from the owner's own browser on this
// Mac, nothing on the remote can reach it, and a process running as the owner was never
// defended against. The caller left worth refusing is a web page in that browser firing
// cross-site requests at this port, and originGuard is what refuses it; see "Dropping
// the login" in DESIGN.md.
//
// There is still no address-based trust. Loopback origin is not evidence of anything,
// so RemoteAddr is never consulted: what the guard asks is whether the request came from
// a page this daemon served, or from no browser at all.
//
// The /api prefix predates the login going and is kept only to avoid churn.
func newServer(m *manager, cfg *Config) http.Handler {
	s := &server{m: m, cfg: cfg, loggedBad: map[string]bool{}}
	s.hosts = map[string]bool{cfg.Listen: true}
	if _, port, err := net.SplitHostPort(cfg.Listen); err == nil {
		s.hosts["localhost:"+port] = true
		s.hosts["[::1]:"+port] = true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hosts", s.listHosts)
	mux.HandleFunc("POST /api/hosts", s.addHost)
	mux.HandleFunc("PUT /api/hosts/{alias}", s.updateHost)
	mux.HandleFunc("DELETE /api/hosts/{alias}", s.removeHost)
	mux.HandleFunc("POST /api/hosts/{alias}/test", s.testHost)
	mux.HandleFunc("GET /api/identities", s.identities)
	mux.HandleFunc("GET /api/hosts/{alias}/listeners", s.hostListeners)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/forwards", s.list)
	mux.HandleFunc("POST /api/forward", s.open)
	mux.HandleFunc("PATCH /api/forward/{ref}", s.edit)
	mux.HandleFunc("DELETE /api/forward/{ref}", s.close)

	mux.HandleFunc("GET /{$}", s.root)
	return s.originGuard(mux)
}

// originGuard refuses what a browser says is cross-site, and lets through what no
// browser sent. Three checks, in this order:
//
//  1. Host must name this daemon. Anything else is misconfiguration or DNS rebinding —
//     a hostile page whose name has been re-pointed at 127.0.0.1, which the browser
//     then treats as same-origin with that page. 400, and the value is logged once.
//  2. Sec-Fetch-Site, when present, must be same-origin or none, and Origin, when
//     present, must be this daemon's own origin for the Host that arrived. same-site is
//     refused on purpose: a dev server on localhost:3000, or one of this daemon's own
//     local-forwards on 127.0.0.1:<port>, is same-site with the console and is exactly
//     the page this guard exists for. Absence of both is allowed, so curl from a
//     terminal keeps working; that caller runs as the owner and was never defended
//     against. 403.
//  3. A mutating request with a body must say application/json. That is not a
//     CORS-safelisted type, so a cross-origin page cannot send it without a preflight,
//     and this daemon answers no preflight. 415.
//
// GET / gets check 1 only. Following a link to the console from another site is a
// top-level navigation with Sec-Fetch-Site: cross-site, the shell carries no data, and
// refusing it would only make the console look broken.
func (s *server) originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hosts[r.Host] {
			s.logBadHost(r.Host)
			writeErr(w, http.StatusBadRequest, "unexpected Host header")
			return
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeErr(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		if origin, ok := r.Header["Origin"]; ok && (len(origin) != 1 || origin[0] != "http://"+r.Host) {
			writeErr(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.ContentLength != 0 && !isJSON(r.Header.Get("Content-Type")) {
			writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isJSON accepts application/json, optionally with a charset parameter and nothing else.
func isJSON(ct string) bool {
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || mt != "application/json" {
		return false
	}
	for k := range params {
		if k != "charset" {
			return false
		}
	}
	return true
}

// logBadHost records each refused Host value once. A hit means either the listen
// address and the URL in use disagree, or something is rebinding a name to this port,
// and either is worth one line — not one per request.
func (s *server) logBadHost(host string) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.loggedBad[host] || len(s.loggedBad) >= maxLoggedHosts {
		return
	}
	s.loggedBad[host] = true
	logf("refused a request for Host %s: not this daemon's address (misconfiguration, or DNS rebinding)", strconv.Quote(host))
}

// root serves the console shell. It carries no mappings and no config; the page fetches
// those itself, through the guarded routes.
func (s *server) root(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(localgateway.Console)
}

// status reports what the daemon is doing.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		// api_version is for clients other than the console, which ships with the daemon
		// and so can never be out of step with it. The menu-bar app is built separately
		// and reads this to tell "old daemon" from "daemon is broken". Bump it when a
		// field a client reads changes meaning or goes away; adding one does not.
		"api_version": 1,
		"listen":      s.cfg.Listen,
		// `hosts` is now the per-host connection state, keyed by alias; the flat list of
		// configured names it used to be moved to `eager_hosts`. What an operator wants
		// from this route is "is the link to that box up, and when does it try again",
		// which a list of names cannot answer.
		"hosts":        s.m.HostHealth(),
		"eager_hosts":  s.cfg.EagerHosts,
		"max_forwards": s.cfg.MaxForwards,
		"default_ttl":  int(s.cfg.DefaultTTL.Seconds()),
		"forwards":     len(s.m.List()),
		"pinned":       s.pinCount(),
	})
}

func (s *server) pinCount() int {
	if s.m.pins == nil {
		return 0
	}
	return len(s.m.pins.List())
}

// hostListeners shows what a host is listening on. It is the route the console's
// Discover button calls, and it runs real programs on the far side, which is one reason
// the guard refuses a cross-site GET rather than only cross-site writes.
func (s *server) hostListeners(w http.ResponseWriter, r *http.Request) {
	alias := strings.TrimSpace(r.PathValue("alias"))
	if !safeAlias.MatchString(alias) {
		writeErr(w, http.StatusBadRequest, errBadAlias.Error())
		return
	}
	if s.m.book == nil || !s.m.book.Known(alias) {
		writeErr(w, http.StatusBadRequest, errUnknownHost.Error())
		return
	}
	// A host discovered in ssh_config has no master until something needs one. Dialling
	// it here is the lazy-connection rule working as intended: an admin asked.
	if err := s.m.ensureUp(alias); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	entries, err := s.m.discoverListeners(alias)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if entries == nil {
		entries = []listenEntry{}
	}
	s.m.markForwarded(alias, entries)
	writeJSON(w, http.StatusOK, map[string]any{"host": alias, "listeners": entries})
}

// listHosts describes the machines this Mac can reach, which is what the console's host
// picker is built from.
func (s *server) listHosts(w http.ResponseWriter, r *http.Request) {
	if s.m.book == nil {
		writeJSON(w, http.StatusOK, []hostEntry{})
		return
	}
	writeJSON(w, http.StatusOK, s.m.book.List())
}

// hostReq is a host as the console sends it. identities_only is a pointer because its
// absence means "yes when there is an identity file", which is what almost everyone
// wants and what ssh does not default to.
type hostReq struct {
	Alias          string `json:"alias"`
	HostName       string `json:"hostname"`
	User           string `json:"user"`
	Port           int    `json:"port"`
	IdentityFile   string `json:"identity_file"`
	IdentitiesOnly *bool  `json:"identities_only"`
	ProxyJump      string `json:"proxy_jump"`
}

func (h hostReq) entry() hostEntry {
	e := hostEntry{
		Alias: h.Alias, HostName: h.HostName, User: h.User, Port: h.Port,
		IdentityFile: h.IdentityFile, ProxyJump: h.ProxyJump,
	}
	e.IdentitiesOnly = h.IdentitiesOnly == nil || *h.IdentitiesOnly
	return e
}

func (s *server) readHost(w http.ResponseWriter, r *http.Request) (hostReq, bool) {
	var in hostReq
	body, err := readAndRestore(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "body too large")
		return in, false
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return in, false
	}
	if s.m.book == nil {
		writeErr(w, http.StatusServiceUnavailable, "no host book")
		return in, false
	}
	return in, true
}

// writeHostErr names the field a refusal is about, when there is one, so the console
// can mark the right input rather than showing a message at the top of the form.
func writeHostErr(w http.ResponseWriter, err error) {
	var fe *fieldError
	switch {
	case errors.As(err, &fe):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fe.Msg, "field": fe.Field})
	case errors.Is(err, errHostMissing):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errNotManual):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *server) hostView(alias string) any {
	for _, e := range s.m.book.List() {
		if e.Alias == alias {
			return e
		}
	}
	return map[string]any{"ok": true}
}

func (s *server) addHost(w http.ResponseWriter, r *http.Request) {
	in, ok := s.readHost(w, r)
	if !ok {
		return
	}
	e := in.entry()
	if err := s.m.book.Add(e); err != nil {
		writeHostErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.hostView(strings.TrimSpace(e.Alias)))
}

// updateHost replaces the settings of a host added in the console. The alias is the key
// and comes from the path; the running connection is dropped so the next one uses the
// new settings, and its mappings come back on their own.
func (s *server) updateHost(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	in, ok := s.readHost(w, r)
	if !ok {
		return
	}
	if in.Alias != "" && strings.TrimSpace(in.Alias) != alias {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a host's alias cannot be changed; remove it and add it again", "field": "alias"})
		return
	}
	if err := s.m.book.Update(alias, in.entry()); err != nil {
		writeHostErr(w, err)
		return
	}
	s.m.restartHost(alias)
	writeJSON(w, http.StatusOK, s.hostView(alias))
}

func (s *server) removeHost(w http.ResponseWriter, r *http.Request) {
	if s.m.book == nil {
		writeErr(w, http.StatusServiceUnavailable, "no host book")
		return
	}
	alias := r.PathValue("alias")
	if !s.m.book.IsManual(alias) {
		writeErr(w, http.StatusBadRequest, errNotManual.Error())
		return
	}
	if n, p := s.m.hostInUse(alias); n+p > 0 {
		writeErr(w, http.StatusConflict, inUseMsg(alias, n, p))
		return
	}
	if err := s.m.book.Remove(alias); err != nil {
		writeHostErr(w, err)
		return
	}
	s.m.forgetHost(alias)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func inUseMsg(alias string, mappings, pinned int) string {
	plural := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + many
	}
	switch {
	case mappings > 0 && pinned > 0:
		return alias + " still has " + plural(mappings, "mapping", "mappings") + " and " +
			plural(pinned, "pinned mapping", "pinned mappings") + ". Close them first."
	case pinned > 0:
		return alias + " still has " + plural(pinned, "pinned mapping", "pinned mappings") + ". Close them first."
	default:
		return alias + " still has " + plural(mappings, "mapping", "mappings") + ". Close them first."
	}
}

// testHost connects once with a host's saved settings. A failure is still a 200: the
// request worked, and the answer is that the host did not.
func (s *server) testHost(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if s.m.book == nil || !s.m.book.Known(alias) {
		writeErr(w, http.StatusBadRequest, errUnknownHost.Error())
		return
	}
	if err := s.m.testHost(alias); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	out := map[string]any{"ok": true}
	if fp := hostFingerprint(s.cfg, s.hostEntry(alias)); fp != "" {
		out["fingerprint"] = fp
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) hostEntry(alias string) hostEntry {
	for _, e := range s.m.book.List() {
		if e.Alias == alias {
			return e
		}
	}
	return hostEntry{Alias: alias}
}

// identities lists the private keys in ~/.ssh, for the dialog's suggestions. Names
// only: the daemon never reads, let alone returns, a key's contents.
func (s *server) identities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, listIdentities(sshDir()))
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.m.List())
}

func (s *server) open(w http.ResponseWriter, r *http.Request) {
	body, err := readAndRestore(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "body too large")
		return
	}
	var req forwardReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}

	dir := direction(strings.TrimSpace(req.Direction))
	if dir == "" {
		dir = dirLocal // what every caller meant before the field existed
	}

	host, ok := s.resolveHost(w, req.Host)
	if !ok {
		return
	}

	ttl := s.cfg.DefaultTTL
	if req.TTL != nil {
		if *req.TTL < 0 {
			writeErr(w, http.StatusBadRequest, "ttl must not be negative")
			return
		}
		ttl = time.Duration(*req.TTL) * time.Second // an explicit 0 means no expiry
	}

	reqs, ranged, err := expandForward(req, dir, host, ttl)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// A request with no range keeps the single-object response: the console reads `.url`
	// off the top level, and a range envelope for a caller that asked for one port would
	// be a shape change in service of a feature it never used.
	if !ranged {
		view, reused, err := s.m.Open(reqs[0])
		if err != nil {
			writeErr(w, forwardErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, forwardResult(view, reused))
		return
	}

	views, err := s.m.OpenRange(reqs)
	if err != nil {
		writeErr(w, forwardErrStatus(err), err.Error())
		return
	}
	out := make([]map[string]any, 0, len(views))
	for _, v := range views {
		out = append(out, forwardResult(v, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"forwards": out})
}

func forwardResult(view forwardView, reused bool) map[string]any {
	return map[string]any{
		"id":          view.ID,
		"direction":   view.Direction,
		"local_port":  view.LocalPort,
		"remote_port": view.RemotePort,
		"remote_host": view.RemoteHost,
		"pinned":      view.Pinned,
		"url":         view.URL, // always server-constructed; never echoed from the request
		"reused":      reused,
	}
}

// forwardErrStatus separates "you asked for something impossible" from "the ssh side
// would not do it", which are 400 and 502 respectively and read very differently to
// whoever is looking at the failure.
func forwardErrStatus(err error) int {
	var caller callerError
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, errPortRange), errors.Is(err, errUnknownHost),
		errors.Is(err, errBadDirection), errors.Is(err, errLocalNeeded),
		errors.Is(err, errOpenNotLocal), errors.Is(err, errRemoteHostNotLocal),
		errors.Is(err, errBadRemoteHost), errors.Is(err, errRangeTooBig),
		errors.Is(err, errRangeOrder), errors.Is(err, errRangeLength),
		errors.Is(err, errSelfForward), errors.As(err, &caller):
		return http.StatusBadRequest
	case errors.Is(err, errAtCapacity):
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

// expandForward turns one request into the mappings it asks for, and reports whether a
// range was requested at all.
//
// The expansion happens HERE rather than in the manager because a range is a property of
// the request, not of a mapping: each port ends up as its own independent record with its
// own id, which is what makes closing or editing one of them mean anything.
func expandForward(req forwardReq, dir direction, host string, ttl time.Duration) ([]openReq, bool, error) {
	rStart, rEnd := req.RemotePort, req.RemotePortEnd
	lStart, lEnd := req.LocalPort, req.LocalPortEnd
	rangedR, rangedL := rEnd != 0, lEnd != 0

	n := 1
	if rangedR {
		if rEnd < rStart {
			return nil, false, errRangeOrder
		}
		n = rEnd - rStart + 1
	}
	if rangedL {
		if lStart == 0 {
			return nil, false, errRangeNeedsLow
		}
		if lEnd < lStart {
			return nil, false, errRangeOrder
		}
		if nl := lEnd - lStart + 1; rangedR && nl != n {
			return nil, false, errRangeLength
		} else {
			n = nl
		}
	}
	ranged := rangedR || rangedL
	if n > maxRangePorts {
		return nil, false, errRangeTooBig
	}
	if ranged && req.Open {
		return nil, false, errOpenWithRange
	}

	reqs := make([]openReq, 0, n)
	for i := 0; i < n; i++ {
		o := openReq{
			host:       host,
			direction:  dir,
			remoteHost: strings.TrimSpace(req.RemoteHost),
			label:      req.Label,
			ttl:        ttl,
			requester:  req.Requester,
			open:       req.Open,
			pinned:     req.Pinned,
		}
		// A zero start stays zero: it means "unset", and the manager's own defaulting
		// (mirror the other side, or allocate) is what should decide, not this loop.
		if rStart != 0 {
			o.remotePort = rStart + i
		}
		if lStart != 0 {
			o.localPort = lStart + i
		}
		reqs = append(reqs, o)
	}
	return reqs, ranged, nil
}

// close drops the mapping named by a forward id. Closing a pinned mapping removes its
// pin too, which CloseID handles: "close" can only sensibly mean that.
func (s *server) close(w http.ResponseWriter, r *http.Request) {
	target, ok := s.m.find(r.PathValue("ref"))
	if !ok {
		writeErr(w, http.StatusNotFound, errNotFound.Error())
		return
	}

	switch err := s.m.CloseID(target.id()); {
	case errors.Is(err, errNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeErr(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"closed": true})
	}
}

func (s *server) edit(w http.ResponseWriter, r *http.Request) {
	body, err := readAndRestore(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "body too large")
		return
	}
	var b editBody
	if err := json.Unmarshal(body, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}

	var e editReq
	e.label, e.localPort, e.remotePort = b.Label, b.LocalPort, b.RemotePort
	e.remoteHost, e.pinned = b.RemoteHost, b.Pinned
	if b.TTL != nil {
		if *b.TTL < 0 {
			writeErr(w, http.StatusBadRequest, "ttl must not be negative")
			return
		}
		d := time.Duration(*b.TTL) * time.Second
		e.ttl = &d
	}

	view, err := s.m.Edit(r.PathValue("ref"), e)
	switch {
	case errors.Is(err, errNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeErr(w, forwardErrStatus(err), err.Error())
	default:
		writeJSON(w, http.StatusOK, view)
	}
}

func (s *server) resolveHost(w http.ResponseWriter, want string) (string, bool) {
	host := strings.TrimSpace(want)
	if host != "" {
		return host, true
	}
	if host = s.cfg.defaultHost(); host == "" {
		writeErr(w, http.StatusBadRequest, "host is required when several are configured")
		return "", false
	}
	return host, true
}

// readAndRestore reads the bounded body and puts it back, so a guard and the handler
// that follows it can each parse the same request.
func readAndRestore(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, errors.New("body too large")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// openBrowser is only ever handed a URL this daemon built. -u forces URL-scheme
// handling, so a string that also looks like a path cannot open a file instead.
func openBrowser(url string) {
	if url == "" {
		return
	}
	cmd := exec.Command("open", "-u", url)
	if err := cmd.Start(); err != nil {
		logf("open %s: %v", url, err)
		return
	}
	go cmd.Wait()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
