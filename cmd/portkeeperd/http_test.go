package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func testServer(t *testing.T) (http.Handler, *manager, *fakeRunner) {
	t.Helper()
	m, fr := testManager(t)
	return newServer(m, m.cfg), m, fr
}

// post creates a mapping the way the console does, through /admin/forward.
func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, "POST", "/admin/forward", body)
}

// newReq builds a request shaped like one from curl on this Mac: the daemon's own Host,
// a JSON Content-Type when there is a body, and no browser headers at all.
//
// httptest.NewRequest defaults Host to example.com, which the origin guard refuses, so
// every test that goes through the mux has to come through here.
func newReq(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Host = testCfg().Listen
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(h, newReq(method, path, body))
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
// the one route.
func TestAdminForwardCoversEveryCapability(t *testing.T) {
	h, m, _ := testServer(t)
	m.cfg.MaxForwards = 20
	var opened []string
	m.openURL = func(u string) { opened = append(opened, u) }

	if w := do(t, h, "POST", "/admin/forward", `{"direction":"remote-forward","local_port":3000}`); w.Code != 200 {
		t.Fatalf("remote-forward rejected: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/admin/forward", `{"remote_port":8530,"open":true}`); w.Code != 200 {
		t.Fatalf("open rejected: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/admin/forward", `{"remote_port":5432,"remote_host":"db","local_port":15432}`); w.Code != 200 {
		t.Fatalf("target host on the remote rejected: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/admin/forward", `{"remote_port":9100,"pinned":true}`); w.Code != 200 {
		t.Fatalf("pin rejected: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/admin/forward", `{"remote_port":8000,"remote_port_end":8002}`); w.Code != 200 {
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
	if w := do(t, h, "DELETE", "/admin/forward/"+id, ""); w.Code != 200 {
		t.Fatalf("close of a remote-forward: %d %s", w.Code, w.Body)
	}
}

// The whole authority model in one assertion: every data route refuses what a browser
// says is cross-site, refuses a Host that is not this daemon, refuses a body that is not
// JSON, and serves the same request with none of those headers. This is the table the
// session check used to be proved against, and it is walked exhaustively for the same
// reason: a route added without the guard should fail here, not in production.
func TestEveryDataRouteRefusesCrossSiteAndServesTheConsole(t *testing.T) {
	h, m, _ := testServer(t)
	m.cfg.MaxForwards = 20

	// Order matters only where one case's success is the next one's precondition: the
	// host has to exist before it can be removed.
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/admin/hosts", "", 200},
		{"POST", "/admin/hosts", `{"alias":"box2"}`, 200},
		{"DELETE", "/admin/hosts/box2", "", 200},
		{"GET", "/admin/hosts/code/listeners", "", 200},
		{"GET", "/admin/status", "", 200},
		{"GET", "/admin/forwards", "", 200},
		{"POST", "/admin/forward", `{"remote_port":8530}`, 200},
		{"PATCH", "/admin/forward/code:local-forward:8530", `{"label":"x"}`, 200},
		{"DELETE", "/admin/forward/code:local-forward:8530", "", 200},
		{"PATCH", "/admin/forward/code:local-forward:1234", `{"label":"x"}`, 404},
		{"DELETE", "/admin/forward/code:local-forward:1234", "", 404},
	}
	refusals := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"Sec-Fetch-Site: cross-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"Sec-Fetch-Site: same-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, 403},
		{"Origin: https://evil.example", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403},
		{"Origin: null", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		// A local-forward is same-site with the console; its page must not be able to
		// drive the daemon.
		{"Origin: a forwarded dev server", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:8530") }, 403},
		{"Host: evil.example", func(r *http.Request) { r.Host = "evil.example" }, 400},
		{"Host: right name, wrong port", func(r *http.Request) { r.Host = "127.0.0.1:9997" }, 400},
	}

	for _, c := range cases {
		// Every refusal first, so nothing has mutated state before it is checked.
		for _, rf := range refusals {
			r := newReq(c.method, c.path, c.body)
			rf.mutate(r)
			if w := serve(h, r); w.Code != rf.want {
				t.Errorf("%s %s with %s: code %d, want %d", c.method, c.path, rf.name, w.Code, rf.want)
			}
		}
		if c.body != "" {
			r := newReq(c.method, c.path, c.body)
			r.Header.Set("Content-Type", "text/plain")
			if w := serve(h, r); w.Code != 415 {
				t.Errorf("%s %s with Content-Type: text/plain: code %d, want 415", c.method, c.path, w.Code)
			}
		}
		if w := do(t, h, c.method, c.path, c.body); w.Code != c.want {
			t.Errorf("%s %s with no browser headers: code %d, want %d: %s", c.method, c.path, w.Code, c.want, w.Body)
		}
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("%d mapping(s) left; a refused request got through", n)
	}

	// What the console itself sends, from either loopback name. The Origin comparison is
	// against the Host that arrived, not against the configured listen address.
	for _, host := range []string{"127.0.0.1:9996", "localhost:9996", "[::1]:9996"} {
		r := newReq("POST", "/admin/forward", `{"remote_port":8531}`)
		r.Host = host
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Origin", "http://"+host)
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		if w := serve(h, r); w.Code != 200 {
			t.Errorf("the console's own request via %s: code %d, want 200: %s", host, w.Code, w.Body)
		}
	}

	// The shell: a link from another site is a cross-site navigation and must still
	// land, but a foreign Host is refused here too.
	r := newReq("GET", "/", "")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := serve(h, r); w.Code != 200 {
		t.Errorf("GET / with Sec-Fetch-Site: cross-site: code %d, want the console shell", w.Code)
	}
	r = newReq("GET", "/", "")
	r.Host = "evil.example"
	if w := serve(h, r); w.Code != 400 {
		t.Errorf("GET / with Host: evil.example: code %d, want 400", w.Code)
	}

	// The routes the control channel and the login used to answer on are gone, not
	// merely gated.
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/admin/login", `{"password":"x"}`},
		{"POST", "/admin/logout", ""},
		{"GET", "/api/forwards", ""},
		{"POST", "/api/forward", `{"remote_port":8530}`},
		{"DELETE", "/api/forward/code:local-forward:8530", ""},
		{"POST", "/forward", `{"remote_port":8530}`},
		{"GET", "/forwards", ""},
		{"DELETE", "/forward/8530", ""},
	} {
		if w := do(t, h, c.method, c.path, c.body); w.Code != 404 {
			t.Errorf("%s %s: code %d, want 404 — the route should not exist", c.method, c.path, w.Code)
		}
	}
}

// The guard's Content-Type rule is narrow on both sides: a charset is fine, anything
// else riding along with application/json is not, and a bodiless DELETE needs none.
func TestContentTypeRule(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":                  true,
		"application/json; charset=utf-8":   true,
		"Application/JSON":                  true,
		"application/json; boundary=x":      false,
		"text/plain":                        false,
		"application/x-www-form-urlencoded": false,
		"multipart/form-data; boundary=x":   false,
		"":                                  false,
	} {
		if got := isJSON(ct); got != want {
			t.Errorf("isJSON(%q) = %v, want %v", ct, got, want)
		}
	}

	h, m, _ := testServer(t)
	post(t, h, `{"remote_port":8530}`)
	r := newReq("DELETE", "/admin/forward/"+m.List()[0].ID, "")
	r.Header.Del("Content-Type")
	if w := serve(h, r); w.Code != 200 {
		t.Fatalf("bodiless DELETE with no Content-Type: code %d, want 200: %s", w.Code, w.Body)
	}
}

// The self-forward refusal is a caller mistake, so it reads as one over HTTP.
func TestSelfForwardIs400OverHTTP(t *testing.T) {
	h, m, _ := testServer(t)
	if w := post(t, h, `{"direction":"remote-forward","local_port":9996}`); w.Code != 400 {
		t.Fatalf("remote-forward of the listen port: code %d, want 400: %s", w.Code, w.Body)
	}
	if w := post(t, h, `{"remote_port":8530,"local_port":9996}`); w.Code != 400 {
		t.Fatalf("local-forward onto the listen port: code %d, want 400: %s", w.Code, w.Body)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("%d mapping(s) created", n)
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
	post(t, h, `{"remote_port":8530}`)
	id := m.List()[0].ID

	if w := do(t, h, "DELETE", "/admin/forward/"+id, ""); w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	if !fr.sawSubcommand("cancel") {
		t.Fatal("no ssh -O cancel issued")
	}
	if w := do(t, h, "DELETE", "/admin/forward/"+id, ""); w.Code != 404 {
		t.Fatalf("second close: code %d, want 404", w.Code)
	}
}

func TestEditUpdatesLabel(t *testing.T) {
	h, m, _ := testServer(t)
	post(t, h, `{"remote_port":8530,"label":"before"}`)
	id := m.List()[0].ID

	if w := do(t, h, "PATCH", "/admin/forward/"+id, `{"label":"after"}`); w.Code != 200 {
		t.Fatalf("admin edit: %d %s", w.Code, w.Body)
	}
	if got := m.List()[0].Label; got != "after" {
		t.Fatalf("label %q, want after", got)
	}
}

func TestListEndpointShape(t *testing.T) {
	h, _, _ := testServer(t)
	post(t, h, `{"remote_port":8530,"label":"mkdp"}`)

	w := do(t, h, "GET", "/admin/forwards", "")
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
	if w := do(t, h, "GET", "/nope", ""); w.Code != 404 {
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
