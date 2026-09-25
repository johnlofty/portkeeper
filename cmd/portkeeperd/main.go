package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const reconcileInterval = 30 * time.Second

// version is the release this binary was built as. `make dist` and macos/build-app.sh set
// it with -ldflags "-X main.version=<tag>"; a plain `go build` reports "dev".
var version = "dev"

func main() {
	log.SetFlags(log.Ltime)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.ControlPath), 0o700); err != nil {
		log.Fatalf("control path: %v", err)
	}

	// Bind before touching any SSH state. The listen port is the singleton lock: two
	// daemons sharing one ControlPath destroy each other, because each one's startup
	// `ssh -O exit` and each reconcile's stop() kills the other's master, forever. A
	// second instance must lose here, while it still cannot do any damage.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatalf("listen %s: %v (is another portkeeperd already running?)", cfg.Listen, err)
	}
	// Before anything runs ssh: every invocation is given this file with -F.
	if err := writeSSHWrapper(cfg); err != nil {
		log.Fatalf("ssh config: %v", err)
	}
	run := execRunner{}
	book := newHostBook(cfg, cfg.SSHConfigPath, cfg.HostsFile)
	if n := book.importLegacy(cfg.LegacyHostsFile); n > 0 {
		log.Printf("%d host(s) from %s need a host name before they can connect", n, cfg.LegacyHostsFile)
	}

	m := newManager(cfg, run, nil)
	m.book = book
	m.pins = newPinBook(cfg.PinnedFile)
	if n := len(m.pins.List()); n > 0 {
		log.Printf("%d pinned mapping(s) from %s", n, cfg.PinnedFile)
	}
	m.paste = newImagePaste(cfg, run, systemPasteboard())
	m.paste.openLogin = m.openLogin
	if n := len(m.paste.enabledHosts()); n > 0 {
		log.Printf("image paste on for %d host(s)", n)
	}
	m.newMaster = func(host string) masterCtl {
		return &sshMaster{cfg: cfg, host: host, run: run}
	}
	// Backgrounded: bringing up a master can block for seconds per host (longer if one
	// is unreachable), and the console must be answering before that finishes.
	go m.Start()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.reconcile()
			}
		}
	}()

	srv := &http.Server{Handler: newServer(m, cfg)}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	log.Printf("portkeeperd %s listening on %s, hosts %v", version, cfg.Listen, cfg.EagerHosts)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Printf("http: %v", err)
	}

	m.Shutdown()
	log.Print("stopped")
}
