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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xdev/internal/apps"
	"xdev/internal/auth"
	"xdev/internal/config"
	"xdev/internal/domains"
	"xdev/internal/hostproc"
	"xdev/internal/metrics"
	"xdev/internal/platform"
	"xdev/internal/projects"
	"xdev/internal/proxy"
	"xdev/internal/runtime"
	"xdev/internal/secrets"
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
	publicHost   string
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

	fs.StringVar(&o.publicHost, "public-host", envOr("XDEV_PUBLIC_HOST", ""), "address this server is reached at when an app has no domain (IP or name); enables port-only apps")

	fs.StringVar(&o.hostsFile, "hosts-file", envOr("XDEV_HOSTS_FILE", "/etc/hosts"), "hosts file to manage local domains in")
	fs.BoolVar(&o.manageHosts, "manage-hosts", envBoolOr("XDEV_MANAGE_HOSTS", true), "write local domains into the hosts file")

	fs.Usage = func() { printUsage(fs) }
	return fs, o
}

// localCAName is Caddy's id for its internal CA — the authority under which the
// local .test/.localhost root and intermediate are stored.
const localCAName = "local"

// pinCaddyDataDir keeps Caddy's data (local CA, certs) inside xdev's data dir
// instead of the OS default. Both proxy.caddyDataDir() and Caddy itself honor
// XDG_DATA_HOME, so this pins the local CA to <data>/caddy — the same host path
// mounted into a Caddy container. The server and doctor must agree on it, or
// doctor reports on a CA nothing is using. ponytail: env lever, no new flag.
func pinCaddyDataDir(dataDir string) {
	if os.Getenv("XDG_DATA_HOME") == "" {
		os.Setenv("XDG_DATA_HOME", dataDir)
	}
}

func runServer(o *options) error {
	cfg, err := config.Load(o.dataDir, o.projectsDir, o.addr, o.publicHost)
	if err != nil {
		return err
	}

	pinCaddyDataDir(cfg.DataDir)

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
	// Supervisor for static (host) apps; their log files live under the data dir.
	sup := hostproc.NewSupervisor(filepath.Join(cfg.DataDir, "run"))
	defer sup.StopAll()
	// Encrypts the deploy keys of git-backed apps. Created on first use beside
	// the database; a failure here is fatal because starting without it would
	// mean every existing key silently stops decrypting.
	keys, err := secrets.New(filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		return fmt.Errorf("secret key: %w", err)
	}
	appSvc := apps.New(st, engine, sup, filepath.Join(cfg.DataDir, "wp"), keys)
	// Where a container deploy writes its pre-migration database dump — the
	// same directory the backups UI already lists and serves.
	appSvc.SetBackupsRoot(filepath.Join(cfg.DataDir, "backups"))
	// How apps are addressed. ProxyEnabled is true from the service's point of
	// view because a hostname only exists when xdev means to route it — an app
	// with no domain (port-only) falls through to its published port either way.
	// The UI builds its own Reach with the *live* proxy state, since it also has
	// to describe an install whose Caddy is not answering.
	appSvc.SetReach(apps.Reach{
		PublicHost: cfg.PublicHost, ProxyEnabled: true, HTTPSPort: o.httpsPort,
	})
	// Deploy keys generated for an add-app dialog nobody submitted.
	if err := st.PruneUnboundDeployKeys(); err != nil {
		log.Printf("prune unbound deploy keys: %v", err)
	}
	// Deploys run in goroutines, so any that were in flight died with the last
	// process. Close them out, or they claim to be running forever — and a
	// running deploy is how the next one is refused.
	if err := st.ReapRunningDeployments(); err != nil {
		log.Printf("reap interrupted deployments: %v", err)
	}

	// --- reverse proxy (Caddy) ----------------------------------------------
	pm := proxy.NewManager(o.caddyAdmin, o.httpsPort, o.httpPort, o.acmeEmail, o.localCertTTL)
	recon := platform.NewReconciler(st, pm, o.hostsFile, o.manageHosts)
	// Where Caddy sends the per-app deploy endpoints (/_xdev/…). A wildcard or
	// blank host in the listen address means "every interface"; Caddy has to dial
	// something specific, and loopback is the one that is always right.
	recon.SetSelfAddr(loopbackAddr(cfg.Addr))
	if o.caddyManage {
		// Refresh the local CA intermediate when it has less than one full leaf
		// lifetime left, so newly-issued local certs always get their full
		// duration. Only the intermediate is replaced — the trusted root is
		// preserved, so no re-trust is needed.
		leafDur := 90 * 24 * time.Hour
		if d, err := time.ParseDuration(o.localCertTTL); err == nil {
			leafDur = d
		}
		if old, regen := proxy.RefreshStaleIntermediate(localCAName, leafDur); regen {
			log.Printf("refreshed Caddy local CA intermediate (had %.0f days left); local certs will now last %s",
				old.Hours()/24, o.localCertTTL)
		}

		sup, err := proxy.Start(pm)
		if err != nil {
			log.Printf("reverse proxy disabled: %v", err)
		} else {
			defer sup.Stop()
		}
	}
	// Push routes. Sync probes Caddy and owns the enabled flag, so an unreachable
	// proxy is not an error here — just routing that isn't live yet.
	if err := recon.Sync(); err != nil {
		log.Printf("reverse proxy: %v", err)
		log.Printf("  binding :%d/:%d likely needs privileges — for sudo-free local dev try"+
			" `-https-port 8443 -http-port 8080`, or run xdev with sudo for clean 443/80.", o.httpsPort, o.httpPort)
	}
	// Caddy may simply not be up yet (its container starts alongside xdev, with
	// no ordering between them), so keep trying in the background instead of
	// leaving routing dead until the next mutation or restart.
	proxyRetryCtx, stopProxyRetry := context.WithCancel(context.Background())
	defer stopProxyRetry()
	go recon.RetryUntilLive(proxyRetryCtx, 5*time.Second)

	srv, err := server.New(st, authsvc, engine, cfg, projSvc, appSvc, recon, o.httpsPort)
	if err != nil {
		return err
	}

	// --- metrics collector ---------------------------------------------------
	metricsCtx, stopMetrics := context.WithCancel(context.Background())
	defer stopMetrics()
	go metrics.New(st, engine, sup).Run(metricsCtx)

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
	if recon.Enabled() {
		log.Printf("  proxy:       Caddy serving sites on :%d (https) / :%d (http)", o.httpsPort, o.httpPort)
		log.Printf("  TLS:         local certs use Caddy's internal CA — `xdev doctor` prints the trust command for this OS")
	} else {
		log.Printf("  proxy:       not live yet — retrying in the background; sites reachable directly on their host ports")
	}
	if need, _ := authsvc.NeedsSetup(); need {
		log.Printf("  first run:   create your admin at http://%s/setup (or run: xdev create-admin you@example.com)", cfg.Addr)
	}
	log.Printf("  listening:   http://%s", cfg.Addr)

	// Respawn static command-mode apps that were running before this restart
	// (their host processes, unlike containers, don't survive an xdev restart).
	appSvc.ResumeStatic()

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

// loopbackAddr turns xdev's listen address into one Caddy can dial. A listen
// address may name every interface (":7331", "0.0.0.0:7331") — fine to bind,
// useless to connect to — so the host half is replaced with loopback, which is
// reachable whatever xdev bound. A specific address (127.0.0.1:7331, or an
// interface IP) is kept as it is.
func loopbackAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
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
