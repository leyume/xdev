// Command xdev is the control plane: a single binary that serves the web UI,
// stores all state in sqlite, and manages projects, apps, containers, domains,
// and metrics.
//
// Besides running the server (the default), xdev exposes a few subcommands used
// by the installer and for day-to-day operations:
//
//	xdev version                 print version + go/os/arch
//	xdev doctor                  preflight/health check (engine, caddy, ports, …)
//	xdev create-admin <email>    create the first admin account (idempotent)
//	xdev write-hosts <file> [h…] rewrite the managed /etc/hosts block (internal)
//
// Every flag also has an XDEV_* env fallback so a service manager can configure
// xdev entirely from an EnvironmentFile. Precedence is: explicit flag > env >
// built-in default.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xdev/internal/apps"
	"xdev/internal/auth"
	"xdev/internal/config"
	"xdev/internal/domains"
	"xdev/internal/metrics"
	"xdev/internal/platform"
	"xdev/internal/projects"
	"xdev/internal/proxy"
	"xdev/internal/runtime"
	"xdev/internal/server"
	"xdev/internal/store"
)

// version is stamped at build time via -ldflags "-X main.version=<v>". It stays
// "dev" for plain `go build` / `go run`.
var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatalf("xdev: %v", err)
	}
}

func run() error {
	// --- subcommand dispatch -------------------------------------------------
	// Each subcommand parses only what it needs; the default (no subcommand, or
	// a leading flag) runs the server.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "help", "--help", "-h":
			// `xdev help` mirrors `xdev -h` for consistency; both print usage.
			fs, _ := serverFlags()
			printUsage(fs)
			return nil
		case "version":
			fmt.Println(versionString())
			return nil
		case "doctor":
			return runDoctor(os.Args[2:])
		case "create-admin":
			return runCreateAdmin(os.Args[2:])
		case "write-hosts":
			// Rewrites the hosts file's managed block. Run as root via the GUI
			// elevation prompt by the dashboard's one-click "add to hosts" button.
			if len(os.Args) < 3 {
				return fmt.Errorf("usage: xdev write-hosts <hosts-file> [hostname...]")
			}
			return domains.SyncHosts(os.Args[2], os.Args[3:])
		}
	}

	// --- flags ---------------------------------------------------------------
	fs, o := serverFlags()
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(versionString())
		return nil
	}

	return runServer(o)
}

// options is the fully-resolved server configuration, parsed from flags with
// XDEV_* env fallbacks.
type options struct {
	dataDir      string
	projectsDir  string
	addr         string
	secure       bool
	caddyManage  bool
	caddyAdmin   string
	httpsPort    int
	httpPort     int
	hostsFile    string
	manageHosts  bool
	acmeEmail    string
	engine       string
	localCertTTL string
}

// serverFlags builds the flag set shared by the server and `xdev doctor`, with
// every flag defaulting to its XDEV_* env var (and then a built-in default).
// The returned FlagSet has a friendly Usage attached.
func serverFlags() (*flag.FlagSet, *options) {
	fs := flag.NewFlagSet("xdev", flag.ContinueOnError)
	o := &options{}

	fs.StringVar(&o.dataDir, "data", envOr("XDEV_DATA", ""), "data directory (sqlite db + state)")
	fs.StringVar(&o.projectsDir, "projects", envOr("XDEV_PROJECTS", ""), "directory for generated project/app stacks")
	fs.StringVar(&o.addr, "addr", envOr("XDEV_ADDR", ""), "web UI listen address (host:port)")
	fs.BoolVar(&o.secure, "secure", envBoolOr("XDEV_SECURE", false), "set Secure flag on cookies (enable when served over HTTPS)")

	fs.StringVar(&o.engine, "engine", envOr("XDEV_ENGINE", ""), "container engine: podman | docker (default: auto-detect)")

	fs.BoolVar(&o.caddyManage, "caddy", envBoolOr("XDEV_CADDY", true), "supervise Caddy as a child process for reverse proxy + TLS")
	fs.StringVar(&o.caddyAdmin, "caddy-admin", envOr("XDEV_CADDY_ADMIN", "127.0.0.1:2019"), "Caddy admin API address")
	fs.IntVar(&o.httpsPort, "https-port", envIntOr("XDEV_HTTPS_PORT", 443), "public HTTPS port for proxied sites")
	fs.IntVar(&o.httpPort, "http-port", envIntOr("XDEV_HTTP_PORT", 80), "public HTTP port for proxied sites")
	fs.StringVar(&o.acmeEmail, "acme-email", envOr("XDEV_ACME_EMAIL", ""), "contact email for Let's Encrypt (production domains)")
	fs.StringVar(&o.localCertTTL, "local-cert-lifetime", envOr("XDEV_LOCAL_CERT_LIFETIME", "2160h"), "validity of locally-issued (.test/.localhost) TLS certs; keep under ~8000h")

	fs.StringVar(&o.hostsFile, "hosts-file", envOr("XDEV_HOSTS_FILE", "/etc/hosts"), "hosts file to manage local domains in")
	fs.BoolVar(&o.manageHosts, "manage-hosts", envBoolOr("XDEV_MANAGE_HOSTS", true), "write local domains into the hosts file")

	fs.Usage = func() { printUsage(fs) }
	return fs, o
}

func runServer(o *options) error {
	cfg, err := config.Load(o.dataDir, o.projectsDir, o.addr)
	if err != nil {
		return err
	}

	// --- core services -------------------------------------------------------
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	authsvc := auth.New(st, o.secure)
	rt := runtime.Detect()

	// Engine selection precedence: persisted UI setting > flag/env > auto-detect.
	override := engineOverride(o.engine)
	if v, _ := st.GetSetting("engine"); v == "podman" || v == "docker" {
		override = runtime.Engine(v)
	}
	engine := runtime.NewSelector(rt, override)

	projSvc := projects.New(st, cfg, engine)
	appSvc := apps.New(st, engine)

	// --- reverse proxy (Caddy) ----------------------------------------------
	pm := proxy.NewManager(o.caddyAdmin, o.httpsPort, o.httpPort, o.acmeEmail, o.localCertTTL)
	recon := platform.NewReconciler(st, pm, o.hostsFile, o.manageHosts)
	if o.caddyManage {
		// Refresh the local CA intermediate when it has less than one full leaf
		// lifetime left, so newly-issued local certs always get their full
		// duration. Only the intermediate is replaced — the trusted root is
		// preserved, so no re-trust is needed.
		leafDur := 90 * 24 * time.Hour
		if d, err := time.ParseDuration(o.localCertTTL); err == nil {
			leafDur = d
		}
		if old, regen := proxy.RefreshStaleIntermediate("local", leafDur); regen {
			log.Printf("refreshed Caddy local CA intermediate (had %.0f days left); local certs will now last %s",
				old.Hours()/24, o.localCertTTL)
		}

		sup, err := proxy.Start(pm)
		if err != nil {
			log.Printf("reverse proxy disabled: %v", err)
		} else {
			defer sup.Stop()
			recon.Enabled = true
		}
	} else {
		recon.Enabled = pm.Reachable()
	}
	if recon.Enabled {
		if err := recon.Sync(); err != nil {
			recon.Enabled = false
			log.Printf("reverse proxy disabled: %v", err)
			log.Printf("  binding :%d/:%d likely needs privileges — for sudo-free local dev try"+
				" `-https-port 8443 -http-port 8080`, or run xdev with sudo for clean 443/80.", o.httpsPort, o.httpPort)
		}
	}

	srv, err := server.New(st, authsvc, engine, cfg, projSvc, appSvc, recon, o.httpsPort)
	if err != nil {
		return err
	}

	// --- metrics collector ---------------------------------------------------
	metricsCtx, stopMetrics := context.WithCancel(context.Background())
	defer stopMetrics()
	go metrics.New(st, engine).Run(metricsCtx)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// --- startup banner ------------------------------------------------------
	log.Printf("xdev %s starting", version)
	log.Printf("  data dir:    %s", cfg.DataDir)
	log.Printf("  projects:    %s", cfg.ProjectsDir)
	log.Printf("  engine:      %s (podman usable=%v, docker usable=%v) — switch in the UI or with -engine",
		engine.Current(), engine.Usable(runtime.Podman), engine.Usable(runtime.Docker))
	if recon.Enabled {
		log.Printf("  proxy:       Caddy serving sites on :%d (https) / :%d (http)", o.httpsPort, o.httpPort)
		log.Printf("  TLS:         local certs use Caddy's internal CA — run `sudo caddy trust` once so browsers trust https://*.test")
	} else {
		log.Printf("  proxy:       disabled — sites reachable directly on their host ports")
	}
	if need, _ := authsvc.NeedsSetup(); need {
		log.Printf("  first run:   create your admin at http://%s/setup (or run: xdev create-admin you@example.com)", cfg.Addr)
	}
	log.Printf("  listening:   http://%s", cfg.Addr)

	// --- serve with graceful shutdown ---------------------------------------
	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case <-stop:
		log.Printf("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(ctx)
	}
}

// engineOverride maps the -engine flag/env value to a runtime.Engine, or "" for
// auto-detect.
func engineOverride(v string) runtime.Engine {
	switch v {
	case "podman":
		return runtime.Podman
	case "docker":
		return runtime.Docker
	}
	return runtime.Engine("")
}

// --- env helpers -------------------------------------------------------------

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envBoolOr parses 1/true/yes/on (case-insensitive) as true and 0/false/no/off
// as false; anything else (including unset) returns def.
func envBoolOr(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// envIntOr parses key as an integer, returning def when unset or unparseable.
func envIntOr(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
