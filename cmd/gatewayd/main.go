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
		log.Fatalf("listen %s: %v (is another gatewayd already running?)", cfg.Listen, err)
	}
	if !cfg.adminEnabled() {
		log.Print("admin is DISABLED: /admin refuses everything and / serves only the " +
			"login page; the public /api still works. To enable it, either set " +
			"LG_ADMIN_PASSWORD, or write the password to " +
			"~/.config/local-gateway/admin-password and chmod 600 it.")
	}

	run := execRunner{}
	masters := map[string]masterCtl{}
	for _, h := range cfg.Hosts {
		masters[h] = &sshMaster{cfg: cfg, host: h, run: run}
	}

	m := newManager(cfg, run, masters)
	// Backgrounded: bringing up a master can block for seconds per host (longer if one
	// is unreachable), and the control channel must not be dead while that happens.
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

	log.Printf("listening on %s, hosts %v (admin %s)",
		cfg.Listen, cfg.Hosts, map[bool]string{true: "enabled", false: "disabled"}[cfg.adminEnabled()])
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Printf("http: %v", err)
	}

	m.Shutdown()
	log.Print("stopped")
}
