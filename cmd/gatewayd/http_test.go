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

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, "POST", "/api/forward", body, nil)
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

// The two capabilities an anonymous caller must not have. Anything on the remote VM can
// reach /api, so these are the boundary of what it may ask for.
func TestPublicAPIRefusesAdminCapabilities(t *testing.T) {
	h, m, _ := testServer(t)
	var opened []string
	m.openURL = func(u string) { opened = append(opened, u) }

	if w := post(t, h, `{"direction":"remote-forward","local_port":3000}`); w.Code != 403 {
		t.Errorf("remote-forward accepted anonymously: %d %s", w.Code, w.Body)
	}
	if w := post(t, h, `{"remote_port":8530,"open":true}`); w.Code != 403 {
		t.Errorf("open accepted anonymously: %d %s", w.Code, w.Body)
	}
	if len(opened) != 0 {
		t.Fatalf("a browser was opened for an anonymous caller: %v", opened)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("a refused request still created %d forward(s)", n)
	}
}

func TestAdminMayDoWhatPublicMayNot(t *testing.T) {
	h, m, _ := testServer(t)
	var opened []string
	m.openURL = func(u string) { opened = append(opened, u) }
	c := adminCookie(t, h)

	if w := do(t, h, "POST", "/admin/forward", `{"direction":"remote-forward","local_port":3000}`, c); w.Code != 200 {
		t.Fatalf("admin remote-forward rejected: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/admin/forward", `{"remote_port":8530,"open":true}`, c); w.Code != 200 {
		t.Fatalf("admin open rejected: %d %s", w.Code, w.Body)
	}
	if len(opened) != 1 || opened[0] != "http://127.0.0.1:8530" {
		t.Fatalf("opened %v, want only the server-built loopback URL", opened)
	}
}

func TestAdminRoutesRejectUnauthenticated(t *testing.T) {
	h, _, _ := testServer(t)
	cases := []struct{ method, path, body string }{
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
		do(t, h, "GET", "/api/forwards", "", nil),
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

// Without a password the daemon must fail closed rather than open.
func TestAdminDisabledFailsClosed(t *testing.T) {
	m, _ := testManager(t)
	m.cfg.AdminPassword = ""
	h := newServer(m, m.cfg)

	if w := do(t, h, "POST", "/admin/login", `{"password":""}`, nil); w.Code != 503 {
		t.Errorf("login with admin disabled: code %d, want 503", w.Code)
	}
	if w := do(t, h, "GET", "/admin/status", "", nil); w.Code != 503 {
		t.Errorf("admin route with admin disabled: code %d, want 503", w.Code)
	}
	// The public API keeps working.
	if w := post(t, h, `{"remote_port":8530}`); w.Code != 200 {
		t.Errorf("public API broken when admin is disabled: %d %s", w.Code, w.Body)
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

func TestPublicCloseCannotTouchRemoteForward(t *testing.T) {
	h, m, _ := testServer(t)
	c := adminCookie(t, h)
	if w := do(t, h, "POST", "/admin/forward", `{"direction":"remote-forward","local_port":3000}`, c); w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	id := m.List()[0].ID

	if w := do(t, h, "DELETE", "/api/forward/"+id, "", nil); w.Code != 403 {
		t.Fatalf("anonymous caller closed a remote-forward: %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/admin/forward/"+id, "", c); w.Code != 200 {
		t.Fatalf("admin close: %d %s", w.Code, w.Body)
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

// The client deployed before /api existed still posts to /forward.
func TestLegacyRoutesStillWork(t *testing.T) {
	h, _, fr := testServer(t)
	if w := do(t, h, "POST", "/forward", `{"remote_port":8530}`, nil); w.Code != 200 {
		t.Fatalf("legacy open: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/forwards", "", nil); w.Code != 200 {
		t.Fatalf("legacy list: %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/forward/8530", "", nil); w.Code != 200 {
		t.Fatalf("legacy close: %d %s", w.Code, w.Body)
	}
	if !fr.sawSubcommand("cancel") {
		t.Fatal("no ssh -O cancel issued")
	}
}

func TestCloseEndpoint(t *testing.T) {
	h, m, fr := testServer(t)
	post(t, h, `{"remote_port":8530}`)
	id := m.List()[0].ID

	if w := do(t, h, "DELETE", "/api/forward/"+id, "", nil); w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	if !fr.sawSubcommand("cancel") {
		t.Fatal("no ssh -O cancel issued")
	}
	if w := do(t, h, "DELETE", "/api/forward/"+id, "", nil); w.Code != 404 {
		t.Fatalf("second close: code %d, want 404", w.Code)
	}
}

func TestEditIsAdminOnlyAndUpdatesLabel(t *testing.T) {
	h, m, _ := testServer(t)
	post(t, h, `{"remote_port":8530,"label":"before"}`)
	id := m.List()[0].ID

	if w := do(t, h, "PATCH", "/api/forward/"+id, `{"label":"after"}`, nil); w.Code == 200 {
		t.Fatal("edit is reachable without admin")
	}

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

	w := do(t, h, "GET", "/api/forwards", "", nil)
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
