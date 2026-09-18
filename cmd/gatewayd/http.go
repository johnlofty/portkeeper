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
	"strconv"
	"strings"
	"sync"
	"time"

	localgateway "github.com/johnlofty/local-gateway"
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
	errNotAdmin     = errors.New("admin session required")
	errRemoteNotPub = errors.New("a remote-forward is an admin capability: publishing a Mac service to the remote cannot be requested anonymously")
	errOpenNotPub   = errors.New("open is an admin capability: opening a browser cannot be requested anonymously")
)

type forwardReq struct {
	Direction  string `json:"direction"`
	RemotePort int    `json:"remote_port"`
	LocalPort  int    `json:"local_port"`
	Host       string `json:"host"`
	Label      string `json:"label"`
	Open       bool   `json:"open"`
	TTL        *int   `json:"ttl"`
	Requester  string `json:"requester"`
}

type editBody struct {
	Label      *string `json:"label"`
	TTL        *int    `json:"ttl"`
	LocalPort  *int    `json:"local_port"`
	RemotePort *int    `json:"remote_port"`
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

// newServer wires one listener with two levels of authority.
//
// There is deliberately no port-based or address-based trust here. Requests arriving
// through the SSH RemoteForward are indistinguishable from ones typed into the Mac's
// own browser — both are loopback — so an unauthenticated request is treated as PUBLIC
// no matter where it appears to come from. RemoteAddr is never consulted for authority.
func newServer(m *manager, cfg *Config) http.Handler {
	s := &server{m: m, cfg: cfg, sess: newSessions()}
	mux := http.NewServeMux()

	// Public. Assume any process on the remote VM can reach these.
	mux.HandleFunc("GET /api/forwards", s.list)
	mux.HandleFunc("POST /api/forward", s.publicOpen)
	mux.HandleFunc("DELETE /api/forward/{ref}", s.publicClose)

	// Pre-/api client still deployed on the remote; same public authority.
	mux.HandleFunc("POST /forward", s.publicOpen)
	mux.HandleFunc("DELETE /forward/{ref}", s.publicClose)
	mux.HandleFunc("GET /forwards", s.list)

	// Authenticated. Full authority, both directions.
	mux.HandleFunc("POST /admin/login", s.login)
	mux.HandleFunc("POST /admin/logout", s.logout)
	mux.HandleFunc("GET /admin/hosts", s.requireAdmin(s.listHosts))
	mux.HandleFunc("POST /admin/hosts", s.requireAdmin(s.addHost))
	mux.HandleFunc("DELETE /admin/hosts/{alias}", s.requireAdmin(s.removeHost))
	mux.HandleFunc("GET /admin/status", s.requireAdmin(s.status))
	mux.HandleFunc("GET /admin/forwards", s.requireAdmin(s.list))
	mux.HandleFunc("POST /admin/forward", s.requireAdmin(s.adminOpen))
	mux.HandleFunc("PATCH /admin/forward/{ref}", s.requireAdmin(s.edit))
	mux.HandleFunc("DELETE /admin/forward/{ref}", s.requireAdmin(s.adminClose))

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
		"listen":        s.cfg.Listen,
		"hosts":         s.cfg.PublicHosts,
		"max_forwards":  s.cfg.MaxForwards,
		"default_ttl":   int(s.cfg.DefaultTTL.Seconds()),
		"admin_enabled": true,
		"forwards":      len(s.m.List()),
	})
}

// listHosts is admin-only on purpose: it describes the machines this Mac can reach, which
// is not something an anonymous caller on a remote VM has any business enumerating.
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

func (s *server) publicOpen(w http.ResponseWriter, r *http.Request) { s.open(w, r, false) }
func (s *server) adminOpen(w http.ResponseWriter, r *http.Request)  { s.open(w, r, true) }

func (s *server) open(w http.ResponseWriter, r *http.Request, admin bool) {
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

	// The two capabilities an anonymous caller must not have: publishing a Mac service
	// to the remote, and making something appear on the user's screen.
	if !admin {
		if dir == dirRemote {
			writeErr(w, http.StatusForbidden, errRemoteNotPub.Error())
			return
		}
		if req.Open {
			writeErr(w, http.StatusForbidden, errOpenNotPub.Error())
			return
		}
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

	view, reused, err := s.m.Open(openReq{
		host:       host,
		direction:  dir,
		remotePort: req.RemotePort,
		localPort:  req.LocalPort,
		label:      req.Label,
		ttl:        ttl,
		requester:  req.Requester,
		open:       req.Open,
		admin:      admin,
	})
	switch {
	case errors.Is(err, errPortRange), errors.Is(err, errUnknownHost),
		errors.Is(err, errBadDirection), errors.Is(err, errLocalNeeded),
		errors.Is(err, errOpenNotLocal):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, errAtCapacity):
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          view.ID,
		"direction":   view.Direction,
		"local_port":  view.LocalPort,
		"remote_port": view.RemotePort,
		"url":         view.URL, // always server-constructed; never echoed from the request
		"reused":      reused,
	})
}

func (s *server) publicClose(w http.ResponseWriter, r *http.Request) { s.close(w, r, false) }
func (s *server) adminClose(w http.ResponseWriter, r *http.Request)  { s.close(w, r, true) }

// close takes either a forward id or, for the client deployed before ids existed, a
// bare remote port with ?host=.
func (s *server) close(w http.ResponseWriter, r *http.Request, admin bool) {
	ref := r.PathValue("ref")

	var target *forward
	if port, convErr := strconv.Atoi(ref); convErr == nil {
		host, ok := s.resolveHost(w, r.URL.Query().Get("host"))
		if !ok {
			return
		}
		target, _ = s.m.find(localForwardID(host, port))
	} else {
		target, _ = s.m.find(ref)
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, errNotFound.Error())
		return
	}
	// Closing a remote-forward is the counterpart of creating one, so it needs the
	// same authority; otherwise anything on the VM could tear down the Mac services
	// published to it.
	if !admin && target.direction != dirLocal {
		writeErr(w, http.StatusForbidden, errRemoteNotPub.Error())
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
	case errors.Is(err, errPortRange):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeErr(w, http.StatusBadGateway, err.Error())
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
