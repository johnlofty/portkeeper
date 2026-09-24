package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const testPassword = "correct-horse-battery-staple"

func testServer(t *testing.T) (http.Handler, *manager, *fakeRunner) {
	t.Helper()
	m, fr := testManager(t)
	m.cfg.AdminPassword = testPassword
	return newServer(m, m.cfg), m, fr
}

// post creates a mapping the way the console does: logged in, through /admin/forward.
// There is no anonymous way to do it any more, so every test that opens a forward over
// HTTP goes through here.
func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, "POST", "/admin/forward", body, adminCookie(t, h))
}

func do(t *testing.T, h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// adminCookie logs in and returns the session cookie, failing the test if login breaks.
func adminCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := do(t, h, "POST", "/admin/login", `{"password":"`+testPassword+`"}`, nil)
	if w.Code != 200 {
		t.Fatalf("login failed: %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("login set no session cookie")
	return nil
}

func TestOpenHappyPath(t *testing.T) {
	h, _, _ := testServer(t)
	w := post(t, h, `{"remote_port":8530,"label":"mkdp"}`)
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	var got struct {
		LocalPort int    `json:"local_port"`
		URL       string `json:"url"`
		Direction string `json:"direction"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.LocalPort != 8530 || got.URL != "http://127.0.0.1:8530" {
		t.Fatalf("unexpected response: %+v", got)
	}
	if got.Direction != string(dirLocal) {
		t.Fatalf("direction %q, want a local-forward by default", got.Direction)
	}
}

// Everything the old public tier was refused is now simply what /admin/forward does. A
// remote-forward, a browser open, a target host on the remote and a pin all go through
// the one authenticated route.
func TestAdminForwardCoversEveryCapability(t *testing.T) {
	h, m, _ := testServer(t)
	m.cfg.MaxForwards = 20
	var opened []string
	m.openURL = func(u string) { opened = append(opened, u) }
	c := adminCookie(t, h)

	if w := do(t, h, "POST", "/admin/forward", `{"direction":"remote-forward","local_port":3000}`, c); w.Code != 200 {
		t.Fatalf("remote-forward rejected: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/admin/forward", `{"remote_port":8530,"open":true}`, c); w.Code != 200 {
		t.Fatalf("open rejected: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/admin/forward", `{"remote_port":5432,"remote_host":"db","local_port":15432}`, c); w.Code != 200 {
		t.Fatalf("target host on the remote rejected: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/admin/forward", `{"remote_port":9100,"pinned":true}`, c); w.Code != 200 {
		t.Fatalf("pin rejected: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/admin/forward", `{"remote_port":8000,"remote_port_end":8002}`, c); w.Code != 200 {
		t.Fatalf("range rejected: %d %s", w.Code, w.Body)
	}

	if len(opened) != 1 || opened[0] != "http://127.0.0.1:8530" {
		t.Fatalf("opened %v, want only the server-built loopback URL", opened)
	}
	if n := len(m.List()); n != 7 {
		t.Fatalf("%d mappings, want 7 (remote, open, target, pin, three of a range)", n)
	}
	if n := len(m.pins.List()); n != 1 {
		t.Fatalf("%d pins, want the one that was asked for", n)
	}

	// And a remote-forward closes through the same route it was created by.
	var id string
	for _, v := range m.List() {
		if v.Direction == string(dirRemote) {
			id = v.ID
		}
	}
	if w := do(t, h, "DELETE", "/admin/forward/"+id, "", c); w.Code != 200 {
		t.Fatalf("close of a remote-forward: %d %s", w.Code, w.Body)
	}
}

// The whole authority model in one assertion: GET /, POST /admin/login and POST /admin/logout are
// the only routes an anonymous caller gets anything but a 401 from. This is the list the retired
// control channel used to sit outside of, so it is worth checking exhaustively rather
// than per feature.
func TestEveryRouteButRootAndLoginRequiresASession(t *testing.T) {
	h, _, _ := testServer(t)
	cases := []struct{ method, path, body string }{
		{"GET", "/admin/hosts", ""},
		{"POST", "/admin/hosts", `{"alias":"box2"}`},
		{"DELETE", "/admin/hosts/box2", ""},
		{"GET", "/admin/hosts/code/listeners", ""},
		{"GET", "/admin/status", ""},
		{"GET", "/admin/forwards", ""},
		{"POST", "/admin/forward", `{"remote_port":8530}`},
		{"PATCH", "/admin/forward/code:local-forward:8530", `{"label":"x"}`},
		{"DELETE", "/admin/forward/code:local-forward:8530", ""},
	}
	for _, c := range cases {
		if w := do(t, h, c.method, c.path, c.body, nil); w.Code != 401 {
			t.Errorf("%s %s: code %d, want 401", c.method, c.path, w.Code)
		}
	}

	// The three that are deliberately open. The shell carries no data, login is how a
	// session starts, and logout can only end the session it is handed — none of them
	// exercises any authority.
	if w := do(t, h, "GET", "/", "", nil); w.Code != 200 {
		t.Errorf("GET /: code %d, want the console shell", w.Code)
	}
	if w := do(t, h, "POST", "/admin/login", `{"password":"`+testPassword+`"}`, nil); w.Code != 200 {
		t.Errorf("POST /admin/login: code %d, want 200", w.Code)
	}
	if w := do(t, h, "POST", "/admin/logout", "", nil); w.Code != 200 {
		t.Errorf("POST /admin/logout without a session: code %d, want 200 (nothing to end, nothing leaked)", w.Code)
	}

	// The routes the control channel used to answer on are gone, not merely gated: a
	// 401 here would mean the handler was still wired up.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/forwards", ""},
		{"POST", "/api/forward", `{"remote_port":8530}`},
		{"DELETE", "/api/forward/code:local-forward:8530", ""},
		{"POST", "/forward", `{"remote_port":8530}`},
		{"GET", "/forwards", ""},
		{"DELETE", "/forward/8530", ""},
	} {
		if w := do(t, h, c.method, c.path, c.body, nil); w.Code != 404 {
			t.Errorf("%s %s: code %d, want 404 — the route should not exist", c.method, c.path, w.Code)
		}
	}

	// A made-up cookie is not a session.
	bogus := &http.Cookie{Name: sessionCookie, Value: "deadbeef"}
	if w := do(t, h, "GET", "/admin/status", "", bogus); w.Code != 401 {
		t.Errorf("forged cookie accepted: %d", w.Code)
	}
}

func TestLoginRejectsWrongPasswordAndSetsNoCookie(t *testing.T) {
	h, _, _ := testServer(t)
	w := do(t, h, "POST", "/admin/login", `{"password":"wrong"}`, nil)
	if w.Code != 401 {
		t.Fatalf("code %d, want 401", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("a failed login issued a session cookie")
		}
	}
}

func TestLoginCookieIsHardened(t *testing.T) {
	h, _, _ := testServer(t)
	c := adminCookie(t, h)
	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Error("session cookie is not SameSite=Strict")
	}
	if len(c.Value) < 32 {
		t.Errorf("session id looks too short to be random: %q", c.Value)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	h, _, _ := testServer(t)
	c := adminCookie(t, h)
	if w := do(t, h, "GET", "/admin/status", "", c); w.Code != 200 {
		t.Fatalf("session not usable before logout: %d", w.Code)
	}
	if w := do(t, h, "POST", "/admin/logout", "", c); w.Code != 200 {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := do(t, h, "GET", "/admin/status", "", c); w.Code != 401 {
		t.Fatalf("session still valid after logout: %d", w.Code)
	}
}

// The server verifies the password; it must never hand it back out. This walks every
// route that could plausibly carry it.
func TestPasswordNeverAppearsInAnyResponse(t *testing.T) {
	h, _, _ := testServer(t)
	c := adminCookie(t, h)
	post(t, h, `{"remote_port":8530}`)

	probes := []*httptest.ResponseRecorder{
		do(t, h, "GET", "/", "", nil), // login page
		do(t, h, "GET", "/", "", c),   // console
		do(t, h, "GET", "/admin/status", "", c),
		do(t, h, "GET", "/admin/forwards", "", c),
		do(t, h, "GET", "/admin/forwards", "", nil), // unauthorized error path
		do(t, h, "POST", "/admin/login", `{"password":"wrong"}`, nil),
		do(t, h, "POST", "/admin/login", `{"password":"`+testPassword+`"}`, nil),
		do(t, h, "GET", "/admin/status", "", nil), // unauthorized error path
	}
	for i, w := range probes {
		if strings.Contains(w.Body.String(), testPassword) {
			t.Fatalf("probe %d leaked the password: %s", i, w.Body)
		}
		for _, v := range w.Header() {
			if strings.Contains(strings.Join(v, " "), testPassword) {
				t.Fatalf("probe %d leaked the password in a header", i)
			}
		}
	}
}

// Without a password the daemon must fail closed rather than open. With no public API
// left, that means it can do nothing at all except serve the login page — which is what
// the startup log now says in as many words.
func TestAdminDisabledFailsClosed(t *testing.T) {
	m, _ := testManager(t)
	m.cfg.AdminPassword = ""
	h := newServer(m, m.cfg)

	if w := do(t, h, "POST", "/admin/login", `{"password":""}`, nil); w.Code != 503 {
		t.Errorf("login with admin disabled: code %d, want 503", w.Code)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/admin/status", ""},
		{"GET", "/admin/forwards", ""},
		{"POST", "/admin/forward", `{"remote_port":8530}`},
		{"DELETE", "/admin/forward/code:local-forward:8530", ""},
	} {
		if w := do(t, h, c.method, c.path, c.body, nil); w.Code != 503 {
			t.Errorf("%s %s with admin disabled: code %d, want 503", c.method, c.path, w.Code)
		}
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("something got through with admin disabled: %d mapping(s)", n)
	}
	// The shell still serves, so the page can say why nothing works.
	if w := do(t, h, "GET", "/", "", nil); w.Code != 200 {
		t.Errorf("GET / with admin disabled: code %d, want the login page", w.Code)
	}
}

// The shell is public; the data behind it is not. Serving one page to everyone keeps a
// single login UI, so what actually has to hold is that the page carries nothing worth
// gating and that every data route still refuses an anonymous caller.
func TestRootIsPublicShellButCarriesNoSecrets(t *testing.T) {
	h, _, _ := testServer(t)

	anon := do(t, h, "GET", "/", "", nil)
	if anon.Code != 200 {
		t.Fatalf("anonymous root: %d", anon.Code)
	}
	body := anon.Body.String()
	if strings.Contains(body, testPassword) {
		t.Fatal("the console shell contains the admin password")
	}

	// The shell must be inert without a session: the routes it calls all refuse.
	for _, route := range []string{"/admin/forwards", "/admin/status"} {
		if w := do(t, h, "GET", route, "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s: got %d, want 401", route, w.Code)
		}
	}

	// And an authenticated caller gets the same shell — the difference is the data.
	c := adminCookie(t, h)
	if in := do(t, h, "GET", "/", "", c); in.Body.String() != body {
		t.Fatal("authenticated root served a different shell")
	}
	if w := do(t, h, "GET", "/admin/forwards", "", c); w.Code != 200 {
		t.Fatalf("authenticated /admin/forwards: %d", w.Code)
	}
}

func TestRejectsBadPortOverHTTP(t *testing.T) {
	h, _, _ := testServer(t)
	for _, body := range []string{
		`{"remote_port":80}`, `{"remote_port":70000}`, `{"remote_port":0}`, `{}`,
	} {
		if w := post(t, h, body); w.Code != 400 {
			t.Errorf("%s: code %d, want 400", body, w.Code)
		}
	}
}

func TestRejectsMalformedBody(t *testing.T) {
	h, _, _ := testServer(t)
	if w := post(t, h, `{not json`); w.Code != 400 {
		t.Fatalf("code %d, want 400", w.Code)
	}
}

func TestRejectsHostOutsideAllowlistOverHTTP(t *testing.T) {
	h, _, _ := testServer(t)
	if w := post(t, h, `{"remote_port":8530,"host":"evil"}`); w.Code != 400 {
		t.Fatalf("code %d, want 400", w.Code)
	}
}

func TestCapacityReturns429(t *testing.T) {
	h, m, _ := testServer(t)
	for i := 0; i < m.cfg.MaxForwards; i++ {
		post(t, h, `{"remote_port":`+itoa(9000+i)+`}`)
	}
	if w := post(t, h, `{"remote_port":9999}`); w.Code != 429 {
		t.Fatalf("code %d, want 429", w.Code)
	}
}

func TestCloseEndpoint(t *testing.T) {
	h, m, fr := testServer(t)
	c := adminCookie(t, h)
	post(t, h, `{"remote_port":8530}`)
	id := m.List()[0].ID

	if w := do(t, h, "DELETE", "/admin/forward/"+id, "", c); w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	if !fr.sawSubcommand("cancel") {
		t.Fatal("no ssh -O cancel issued")
	}
	if w := do(t, h, "DELETE", "/admin/forward/"+id, "", c); w.Code != 404 {
		t.Fatalf("second close: code %d, want 404", w.Code)
	}
}

func TestEditUpdatesLabel(t *testing.T) {
	h, m, _ := testServer(t)
	post(t, h, `{"remote_port":8530,"label":"before"}`)
	id := m.List()[0].ID

	c := adminCookie(t, h)
	if w := do(t, h, "PATCH", "/admin/forward/"+id, `{"label":"after"}`, c); w.Code != 200 {
		t.Fatalf("admin edit: %d %s", w.Code, w.Body)
	}
	if got := m.List()[0].Label; got != "after" {
		t.Fatalf("label %q, want after", got)
	}
}

func TestListEndpointShape(t *testing.T) {
	h, _, _ := testServer(t)
	post(t, h, `{"remote_port":8530,"label":"mkdp"}`)

	w := do(t, h, "GET", "/admin/forwards", "", adminCookie(t, h))
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	var got []forwardView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RemotePort != 8530 || got[0].Label != "mkdp" || got[0].State != "alive" {
		t.Fatalf("unexpected list payload: %+v", got)
	}
	if got[0].URL != "http://127.0.0.1:8530" {
		t.Fatalf("list url %q", got[0].URL)
	}
	if got[0].ID != "code:local-forward:8530" {
		t.Fatalf("id %q", got[0].ID)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	h, _, _ := testServer(t)
	if w := do(t, h, "GET", "/nope", "", nil); w.Code != 404 {
		t.Fatalf("code %d, want 404", w.Code)
	}
}

func TestRequireLoopbackListen(t *testing.T) {
	for _, bad := range []string{"0.0.0.0:9996", "192.168.1.5:9996", "[::]:9996"} {
		if err := requireLoopback(bad); err == nil {
			t.Errorf("%s accepted, want rejection", bad)
		}
	}
	for _, ok := range []string{"127.0.0.1:9996", "localhost:9996", "[::1]:9996"} {
		if err := requireLoopback(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
