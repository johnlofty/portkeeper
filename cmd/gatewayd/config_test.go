package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writePw(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "admin-password")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask can narrow WriteFile's mode
		t.Fatal(err)
	}
	return p
}

func TestEnvPasswordWins(t *testing.T) {
	t.Setenv("LG_ADMIN_PASSWORD", "from-env")
	p := writePw(t, "from-file\n", 0o600)
	if got := loadAdminPassword(p); got != "from-env" {
		t.Fatalf("got %q, want the env value", got)
	}
}

func TestPasswordFileIsRead(t *testing.T) {
	t.Setenv("LG_ADMIN_PASSWORD", "")
	p := writePw(t, "hunter2\n", 0o600)
	if got := loadAdminPassword(p); got != "hunter2" {
		t.Fatalf("got %q, want hunter2", got)
	}
}

// Exactly one trailing newline goes; everything else is the password.
func TestPasswordFileTrimsOnlyOneNewline(t *testing.T) {
	t.Setenv("LG_ADMIN_PASSWORD", "")
	cases := map[string]string{
		"a b\n":       "a b",
		"a b\r\n":     "a b",
		"a b":         "a b",
		"a b\n\n":     "a b\n",
		"  spaced \n": "  spaced ",
	}
	for body, want := range cases {
		if got := loadAdminPassword(writePw(t, body, 0o600)); got != want {
			t.Errorf("%q -> %q, want %q", body, got, want)
		}
	}
}

// A password file anything else can read is worse than none, because it looks like
// protection. It must be refused, leaving admin disabled.
func TestPasswordFileRejectedWhenTooPermissive(t *testing.T) {
	t.Setenv("LG_ADMIN_PASSWORD", "")
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666, 0o660} {
		if got := loadAdminPassword(writePw(t, "hunter2\n", mode)); got != "" {
			t.Errorf("mode %04o accepted (got %q), want refusal", mode, got)
		}
	}
	if got := loadAdminPassword(writePw(t, "hunter2\n", 0o600)); got != "hunter2" {
		t.Errorf("mode 0600 refused: %q", got)
	}
}

func TestMissingPasswordFileDisablesAdmin(t *testing.T) {
	t.Setenv("LG_ADMIN_PASSWORD", "")
	if got := loadAdminPassword(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	c := &Config{}
	if c.adminEnabled() {
		t.Fatal("adminEnabled with no password")
	}
}
