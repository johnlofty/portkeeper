package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	localgateway "github.com/johnlofty/portkeeper"
)

const (
	maxBody = 64 << 10

	sessionCookie = "lg_session"
	// Long enough to cover a working day without a re-login, short enough that a
	// forgotten browser tab does not stay privileged indefinitely.
	sessionTTL = 12 * time.Hour
)

var (
	errAdminDisabled = errors.New("admin is disabled: gatewayd started with no password, " +
		"from either LG_ADMIN_PASSWORD or ~/.config/local-gateway/admin-password")
	errNotAdmin = errors.New("admin session required")

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

// sessions holds the live admin logins. Ids are random and meaningless: the map is the
// only thing that makes one valid, so a stolen cookie stops working at logout.
type sessions struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newSessions() *sessions { return &sessions{m: map[string]time.Time{}} }

func (s *sessions) create(ttl time.Duration) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, exp := range s.m { // opportunistic sweep; the map never grows large
		if now.After(exp) {
			delete(s.m, k)
		}
	}
	s.m[id] = now.Add(ttl)
	return id, nil
}

func (s *sessions) valid(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.m, id)
		return false
	}
	return true
}

func (s *sessions) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

type server struct {
	m    *manager
	cfg  *Config
	sess *sessions
}

// newServer wires one listener with a single level of authority.
//
// Everything this daemon can do is an admin capability, and the only two anonymous
// routes are `GET /` — the console shell, which carries no data — and `POST /admin/login`,
// which is how a caller stops being anonymous. There is no public API and nothing on the
// remote can reach this listener at all any more; see "Retiring the control channel" in
// DESIGN.md.
//
// There is deliberately no port-based or address-based trust here either. Loopback origin
// is not evidence of anything, so RemoteAddr is never consulted for authority: a session
// cookie is the only thing that grants any.
func newServer(m *manager, cfg *Config) http.Handler {
	s := &server{m: m, cfg: cfg, sess: newSessions()}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /admin/login", s.login)
	mux.HandleFunc("POST /admin/logout", s.logout)
	mux.HandleFunc("GET /admin/hosts", s.requireAdmin(s.listHosts))
	mux.HandleFunc("POST /admin/hosts", s.requireAdmin(s.addHost))
	mux.HandleFunc("DELETE /admin/hosts/{alias}", s.requireAdmin(s.removeHost))
	mux.HandleFunc("GET /admin/hosts/{alias}/listeners", s.requireAdmin(s.hostListeners))
	mux.HandleFunc("GET /admin/status", s.requireAdmin(s.status))
	mux.HandleFunc("GET /admin/forwards", s.requireAdmin(s.list))
	mux.HandleFunc("POST /admin/forward", s.requireAdmin(s.open))
	mux.HandleFunc("PATCH /admin/forward/{ref}", s.requireAdmin(s.edit))
	mux.HandleFunc("DELETE /admin/forward/{ref}", s.requireAdmin(s.close))

	mux.HandleFunc("GET /{$}", s.root)
	return mux
}

func (s *server) isAdmin(r *http.Request) bool {
	if !s.cfg.adminEnabled() {
		return false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return s.sess.valid(c.Value)
}

func (s *server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.adminEnabled() {
			writeErr(w, http.StatusServiceUnavailable, errAdminDisabled.Error())
			return
		}
		if !s.isAdmin(r) {
			writeErr(w, http.StatusUnauthorized, errNotAdmin.Error())
			return
		}
		h(w, r)
	}
}

// root always serves the console shell, which decides for itself whether to show its
// login view or its table: every data route is 401-gated, so the shell carries no
// mappings, no config and no password. Serving one page keeps a single login UI rather
// than a second one inlined here that would drift from the console's own design.
func (s *server) root(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(localgateway.Console)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.adminEnabled() {
		writeErr(w, http.StatusServiceUnavailable, errAdminDisabled.Error())
		return
	}

	body, err := readAndRestore(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "body too large")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}

	// Constant time, and the password is never logged or echoed — not here, not in the
	// failure path, not anywhere. The server verifies it; it never hands it back out.
	if subtle.ConstantTimeCompare([]byte(s.cfg.AdminPassword), []byte(in.Password)) != 1 {
		logf("admin login rejected")
		writeErr(w, http.StatusUnauthorized, "incorrect password")
		return
	}

	id, err := s.sess.create(sessionTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	logf("admin session opened")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sess.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// status reports what the daemon is doing. It carries no secret: the password is not
// part of the response shape at all, so it cannot leak through this route.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"listen": s.cfg.Listen,
		// `hosts` is now the per-host connection state, keyed by alias; the flat list of
		// configured names it used to be moved to `eager_hosts`. What an operator wants
		// from this route is "is the link to that box up, and when does it try again",
		// which a list of names cannot answer.
		"hosts":         s.m.HostHealth(),
		"eager_hosts":   s.cfg.EagerHosts,
		"max_forwards":  s.cfg.MaxForwards,
		"default_ttl":   int(s.cfg.DefaultTTL.Seconds()),
		"admin_enabled": true,
		"forwards":      len(s.m.List()),
		"pinned":        s.pinCount(),
	})
}

func (s *server) pinCount() int {
	if s.m.pins == nil {
		return 0
	}
	return len(s.m.pins.List())
}

// hostListeners shows what a host is listening on. Like everything else here it needs a
// session, and it is the route the console's Discover button calls.
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

func (s *server) addHost(w http.ResponseWriter, r *http.Request) {
	body, err := readAndRestore(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "body too large")
		return
	}
	var in struct {
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if s.m.book == nil {
		writeErr(w, http.StatusServiceUnavailable, "no host book")
		return
	}
	if err := s.m.book.Add(strings.TrimSpace(in.Alias)); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) removeHost(w http.ResponseWriter, r *http.Request) {
	if s.m.book == nil {
		writeErr(w, http.StatusServiceUnavailable, "no host book")
		return
	}
	if err := s.m.book.Remove(r.PathValue("alias")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
		errors.As(err, &caller):
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
