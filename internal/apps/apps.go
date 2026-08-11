// Package apps implements app-level lifecycle: rendering an app's compose stack
// from a template, scaffolding starter content, allocating a host port, and
// driving start/stop/delete through the container runtime.
//
// Container apps (wordpress, laravel) generate the bizepp-style layout:
//
//	projects/<project>/<app>/_/compose.yml   (generated)
//	projects/<project>/<app>/app/            (bind-mounted content)
//
// Static apps run on the host (system Node, or file-served by Caddy) with no
// container, so their code lives directly in the app folder and their lifecycle
// goes through the hostproc supervisor instead of compose:
//
//	projects/<project>/<app>/                (your code, directly here)
package apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime" // stdlib; xdev/internal/runtime owns the plain name here
	"strings"
	"time"

	"xdev/internal/hostproc"
	"xdev/internal/naming"
	"xdev/internal/runtime"
	"xdev/internal/store"
	"xdev/internal/templates"
)

// defaultStartCmd is the command-mode default: install deps then run the dev
// server, binding the host port xdev allocated (expanded by the shell from the
// PORT env var the supervisor sets).
const defaultStartCmd = `npm install && npm run dev -- --host 127.0.0.1 --port $PORT --strictPort`

// Go apps are always command-mode: build a binary with the system Go toolchain,
// then run it on the allocated port (read from $PORT by the scaffold).
const (
	defaultGoBuildCmd = `go build -o app .`
	defaultGoStartCmd = `./app`
)

// Port range xdev allocates host ports from (Phase 1 exposes apps directly;
// Phase 2 moves them behind the shared Caddy proxy).
const (
	portMin = 20000
	portMax = 29999
)

// composeTimeout is generous because the first `up` may pull an image.
const composeTimeout = 5 * time.Minute

// Service holds dependencies for app operations.
type Service struct {
	store *store.Store
	sel   *runtime.Selector
	sup   *hostproc.Supervisor // supervises static (host) apps
	wpDir string               // data/wp — shared WP core + site docroots (wphost.go)
}

// New creates an app Service. wpDir is the absolute data/wp directory used by
// the shared WordPress host.
func New(st *store.Store, sel *runtime.Selector, sup *hostproc.Supervisor, wpDir string) *Service {
	return &Service{store: st, sel: sel, sup: sup, wpDir: wpDir}
}

// CreateOpts carries the fields the UI collects for a new app. The static-only
// fields are ignored for container types.
type CreateOpts struct {
	Name     string
	Type     string
	Domain   string
	CPULimit float64 // cores; 0 = unlimited (container apps only)
	MemLimit int64   // bytes; 0 = unlimited (container apps only)

	// Static-app config (see store.App).
	ServeMode string // serve | command (default serve)
	RootDir   string
	BuildCmd  string
	StartCmd  string

	// Archive, when set, is a .tar.gz backup unpacked over the new app's files
	// (after any scaffold, so the archive wins) before its first start.
	Archive io.Reader

	// Proxy-only: URL the domain forwards to (http(s)://host[:port]).
	Upstream string

	// Compose-only: the docker-compose.yml / compose.yml the user supplied
	// (pasted or uploaded). Blank means "scaffold the starter compose file".
	ComposeFile string

	// Compose-only: hostnames for domains 2..N, in slot order — domain 1 is
	// Domain above. xdev allocates one host port per domain and writes them as
	// ${PORT}/${PORT_1}…${PORT_N} for the file to publish, so ExtraDomains[0]
	// is the domain routed to ${PORT_2}. Blanks are rejected rather than
	// skipped: dropping one would renumber the slots after it.
	ExtraDomains []string

	// Laravel-only: hostname for the Adminer DB UI (blank = adminer.<app-domain>).
	AdminerDomain string

	// Database mode for wordpress/laravel: "shared" (default — one platform
	// xdev-db MariaDB) or "dedicated" (per-app db service).
	DBMode string

	// WordPress-only: "shared" (default — a docroot served by the platform
	// wp-host, always on the shared DB) or "separate" (the pre-existing
	// per-app WordPress container stack).
	WPMode string
}

// usesDB reports whether an app type carries a MariaDB (and can therefore opt
// into the shared server).
func usesDB(appType string) bool { return appType == "wordpress" || appType == "laravel" }

// secondaryPrefix returns the default hostname prefix for a type's extra
// proxied HTTP UI (Adminer for laravel, the Stalwart admin for mail);
// "" means the type has none.
func secondaryPrefix(appType string) string {
	switch appType {
	case "laravel":
		return "adminer"
	case "mail":
		return "admin"
	}
	return ""
}

// Create persists a new app, writes its files, and starts it. It returns the
// saved app even if the initial start fails, with the error, so the UI can show
// the app in an error state rather than losing it.
func (s *Service) Create(projectID int64, opts CreateOpts) (store.App, error) {
	proj, err := s.store.ProjectByID(projectID)
	if err != nil {
		return store.App{}, err
	}
	// Apps run on their project's pinned engine, so the project's network and
	// its apps always agree even if the global default is switched later.
	appEngine := proj.Engine
	if appEngine == "" {
		appEngine = string(s.sel.Current())
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return store.App{}, errors.New("app name is required")
	}
	if !templates.IsValidType(opts.Type) {
		return store.App{}, fmt.Errorf("app type %q is not available yet", opts.Type)
	}

	slug := naming.Unique(name, func(c string) bool { return s.store.AppSlugExists(projectID, c) })

	// Domain is free-form. If left blank, default to the project's base domain
	// directly (so the first app is served at the bare domain); if that's
	// already taken, fall back to <app-slug>.<base-domain>.
	domain := normalizeHost(opts.Domain)
	if domain == "" {
		domain = proj.BaseDomain
		if s.store.DomainOwner(domain) != 0 {
			domain = slug + "." + proj.BaseDomain
		}
	}
	if err := validHost(domain); err != nil {
		return store.App{}, err
	}
	if owner := s.store.DomainOwner(domain); owner != 0 {
		return store.App{}, fmt.Errorf("domain %q is already in use", domain)
	}

	// Build the app row + on-disk layout, branching on execution model.
	appDir := filepath.Join(proj.Dir, slug)
	app := store.App{
		ProjectID: projectID,
		Name:      name,
		Slug:      slug,
		Type:      opts.Type,
		Status:    store.AppStopped,
		Domain:    domain,
		Runtime:   appEngine,
	}

	// adminerPort is allocated by layoutContainer for types that ship Adminer;
	// composeExtras are the secondary port -> hostname routes a compose app asked
	// for. Both are attached as domain rows once the app row exists.
	var adminerPort int
	var composeExtras []composeRoute
	switch {
	case opts.Type == store.TypeProxy:
		// Proxy apps are only a Caddy route to another server: no container,
		// process, port, or on-disk layout — just a validated upstream URL.
		app.Upstream, err = validUpstream(opts.Upstream)
		if err != nil {
			return store.App{}, err
		}
	case opts.Type == store.TypeStatic, opts.Type == store.TypeGo:
		if err := s.layoutStatic(&app, &opts, appDir); err != nil {
			return store.App{}, err
		}
		// Opt-in shared database: provision it on xdev-db and drop the credentials
		// into the app's .env, which the supervisor already layers into the
		// process environment (staticEnv) — so the app just reads DB_* from env.
		if opts.DBMode == store.DBShared {
			if err := s.provisionHostAppDB(&app, proj, appDir); err != nil {
				os.RemoveAll(appDir)
				return store.App{}, err
			}
		}
	case opts.Type == store.TypeCompose:
		// Bring-your-own compose: the user's file is the stack. xdev writes the
		// same _/ + app/ layout around it and allocates one host port per domain
		// asked for, which the file publishes as ${PORT}/${PORT_n}.
		composeExtras, err = s.layoutCompose(&app, &opts, proj, appDir)
		if err != nil {
			return store.App{}, err
		}
	case opts.Type == "wordpress" && opts.WPMode != "separate":
		// Shared-host WordPress (default): no container or compose of its own —
		// a docroot under data/wp/sites served by the platform wp-host, with its
		// database always on the shared xdev-db.
		if err := s.layoutWPShared(&app, proj); err != nil {
			return store.App{}, err
		}
	default:
		adminerPort, err = s.layoutContainer(&app, &opts, proj, appDir)
		if err != nil {
			return store.App{}, err
		}
	}

	// Import: unpack the supplied backup over the app's files. Proxy apps have
	// no directory, so there is nothing to import into.
	if opts.Archive != nil && !app.IsProxy() {
		target := appDir
		if app.IsSharedWP() {
			target = s.wpSiteDir(proj.Slug, app.Slug) // docroot, not <project>/<slug>
		}
		if err := extractAppArchive(opts.Archive, target, unpacksAll(app)); err != nil {
			os.RemoveAll(appDir)
			return store.App{}, fmt.Errorf("import archive: %w", err)
		}
	}

	// Resolve the Adminer hostname up front so a collision fails before we
	// persist anything.
	var adminerDomain string
	if adminerPort > 0 {
		adminerDomain = normalizeHost(opts.AdminerDomain)
		if adminerDomain == "" {
			adminerDomain = secondaryPrefix(opts.Type) + "." + domain
		}
		if err := validHost(adminerDomain); err != nil {
			os.RemoveAll(appDir)
			return store.App{}, err
		}
		if owner := s.store.DomainOwner(adminerDomain); owner != 0 {
			os.RemoveAll(appDir)
			return store.App{}, fmt.Errorf("domain %q is already in use", adminerDomain)
		}
	}

	saved, err := s.store.CreateApp(app)
	if err != nil {
		os.RemoveAll(appDir)
		if app.WPMode == store.WPShared {
			// Unwind the shared-site provisioning too (docroot + database).
			os.RemoveAll(s.wpSiteDir(proj.Slug, app.Slug))
			ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
			defer cancel()
			s.dropSharedDB(ctx, runtime.Engine(app.Runtime), sharedDBName(proj.Slug, app.Slug))
		}
		return store.App{}, err
	}

	// Attach the chosen domain for the reverse proxy. Local projects get an
	// internally-issued cert; prod uses ACME (Phase 4).
	sslMode, isLocal := "internal", true
	if proj.Environment == "prod" {
		sslMode, isLocal = "letsencrypt", false
	}
	if err := s.store.ReplaceAppDomain(saved.ID, domain, isLocal, sslMode); err != nil {
		// Non-fatal: the app still runs on its host port even without a domain.
		log.Printf("attach domain %s to app %d: %v", domain, saved.ID, err)
	}
	// Attach the Adminer hostname → its own host port (a secondary route).
	if adminerDomain != "" {
		if err := s.store.CreateDomain(saved.ID, adminerDomain, isLocal, sslMode, adminerPort); err != nil {
			log.Printf("attach adminer domain %s to app %d: %v", adminerDomain, saved.ID, err)
		}
	}
	// A compose app's other published ports, each on the hostname it was given.
	for _, r := range composeExtras {
		if err := s.store.CreateDomain(saved.ID, r.Host, isLocal, sslMode, r.Port); err != nil {
			log.Printf("attach domain %s (port %d) to app %d: %v", r.Host, r.Port, saved.ID, err)
		}
	}

	if err := s.Start(saved.ID); err != nil {
		return saved, err
	}
	return s.store.AppByID(saved.ID)
}

// layoutContainer builds a container app's _/compose.yml + app/ layout and sets
// the port, limits, and compose path on the app row. For types that ship extra
// infra (Adminer, db config, an init.sh entrypoint) it also writes those into
// _/, creates the _volumes/ bind-mount dirs, and allocates a second host port
// for Adminer — returned so the caller can attach its route.
func (s *Service) layoutContainer(app *store.App, opts *CreateOpts, proj store.Project, appDir string) (adminerPort int, err error) {
	ports := 1
	if secondaryPrefix(opts.Type) != "" {
		ports = 2
	}
	alloc, err := s.allocPorts(ports)
	if err != nil {
		return 0, err
	}
	port := alloc[0]
	if ports == 2 {
		adminerPort = alloc[1]
	}

	// Shared-database mode (default for db-backed types): provision the app's
	// database/user on the platform xdev-db server before writing any files, so
	// a provisioning failure leaves nothing to clean up. The rendered compose
	// then drops its db service and joins xdev_shared instead.
	shared := usesDB(opts.Type) && opts.DBMode != "dedicated"
	var dbName, dbPass string
	if shared {
		ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
		defer cancel()
		dbName = sharedDBName(proj.Slug, app.Slug)
		dbPass, err = s.provisionSharedDB(ctx, runtime.Engine(app.Runtime), dbName)
		if err != nil {
			return 0, err
		}
		app.DBMode = store.DBShared
	}

	underscore := filepath.Join(appDir, "_")
	content := filepath.Join(appDir, "app")
	dirs := []string{underscore, content}
	if opts.Type == "laravel" {
		// Redis (and the dedicated MariaDB, if any) persist under _volumes/
		// (bizepp format); podman needs the host dirs to pre-exist.
		dirs = append(dirs, filepath.Join(appDir, "_volumes", "redis"))
		if !shared {
			dirs = append(dirs, filepath.Join(appDir, "_volumes", "mysql"))
		}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return 0, err
		}
	}

	composeStr, err := templates.RenderCompose(opts.Type, templates.Data{
		ProjectSlug: proj.Slug,
		NetworkName: proj.NetworkName,
		AppSlug:     app.Slug,
		AppType:     opts.Type,
		Env:         proj.Environment,
		HostPort:    port,
		AdminerPort: adminerPort,
		CPULimit:    opts.CPULimit,
		MemLimit:    opts.MemLimit,
		SharedDB:    shared,
		DBName:      dbName,
		DBPass:      dbPass,
	})
	if err != nil {
		os.RemoveAll(appDir)
		return 0, err
	}
	composePath := filepath.Join(underscore, "compose.yml")
	if err := os.WriteFile(composePath, []byte(composeStr), 0o644); err != nil {
		os.RemoveAll(appDir)
		return 0, err
	}
	// Write compose support files (init.sh, db config) into _/, preserving their
	// subdirs. init.sh must be executable.
	if err := s.writeInfra(opts.Type, underscore); err != nil {
		os.RemoveAll(appDir)
		return 0, err
	}
	// Drop scaffold content (placeholder index.html etc.) without clobbering
	// anything already present.
	if err := s.writeScaffold(opts.Type, content); err != nil {
		os.RemoveAll(appDir)
		return 0, err
	}
	app.Port = port
	app.CPULimit = opts.CPULimit
	app.MemLimit = opts.MemLimit
	app.ComposePath = composePath
	return adminerPort, nil
}

// writeInfra copies an app type's infra files into its _/ directory (init.sh
// gets the executable bit). No-op for types without an infra/ dir.
func (s *Service) writeInfra(appType, underscore string) error {
	files, err := templates.InfraFiles(appType)
	if err != nil {
		return err
	}
	for rel, data := range files {
		dest := filepath.Join(underscore, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(dest, data, mode); err != nil {
			return err
		}
	}
	return nil
}

// layoutStatic builds a host app's folder — code lives directly inside, with
// no _/ or app/ subdirectories — and records its serve config. Command-mode
// apps (all go apps, and static apps that ask for it) get a host port for their
// server; serve-mode apps are file-served by Caddy and need none.
func (s *Service) layoutStatic(app *store.App, opts *CreateOpts, appDir string) error {
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return err
	}
	mode := opts.ServeMode
	if mode != store.ServeCommand {
		mode = store.ServeStatic
	}
	if app.Type == store.TypeGo {
		mode = store.ServeCommand // a Go app is always a built binary we supervise
	}
	app.ServeMode = mode
	app.RootDir = strings.Trim(strings.TrimSpace(opts.RootDir), "/")
	app.BuildCmd = strings.TrimSpace(opts.BuildCmd)

	switch mode {
	case store.ServeCommand:
		app.StartCmd = strings.TrimSpace(opts.StartCmd)
		if app.Type == store.TypeGo {
			if app.BuildCmd == "" {
				app.BuildCmd = defaultGoBuildCmd
			}
			if app.StartCmd == "" {
				app.StartCmd = defaultGoStartCmd
			}
		} else if app.StartCmd == "" {
			app.StartCmd = defaultStartCmd
		}
		port, err := s.allocPort()
		if err != nil {
			os.RemoveAll(appDir)
			return err
		}
		app.Port = port
		// Drop a runnable starter directly in the folder (Vite for static, a
		// net/http server for go), skipping any files the user already added, so
		// the default build/start commands work on first start.
		if err := s.writeScaffold(app.Type, appDir); err != nil {
			os.RemoveAll(appDir)
			return err
		}
	case store.ServeStatic:
		// Drop a friendly placeholder if the served dir has no index yet, so the
		// domain shows something before the user adds their files.
		s.writeStaticPlaceholder(filepath.Join(appDir, app.RootDir), app.Name)
	}
	return nil
}

// provisionHostAppDB gives a host-process app its own database and user on the
// shared xdev-db server and writes the connection details into <appDir>/.env.
// Host processes can't resolve the container hostname, so they get the server's
// published loopback port instead of xdev-db:3306.
func (s *Service) provisionHostAppDB(app *store.App, proj store.Project, appDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
	defer cancel()
	engine := runtime.Engine(app.Runtime)
	// app.Port is allocated but not persisted yet — keep the server off it.
	dbPort, err := s.sharedDBHostPort(ctx, engine, app.Port)
	if err != nil {
		return err
	}
	name := sharedDBName(proj.Slug, app.Slug)
	pass, err := s.provisionSharedDB(ctx, engine, name)
	if err != nil {
		return err
	}
	app.DBMode = store.DBShared
	// 0600: the file holds the database password.
	return os.WriteFile(filepath.Join(appDir, ".env"), []byte(dbEnvFile(name, pass, dbPort)), 0o600)
}

// dbEnvFile renders the .env a host app gets for its shared database. DB_DSN is
// ready for database/sql with the go-sql-driver/mysql driver; the individual
// DB_* vars suit anything else.
func dbEnvFile(name, pass string, port int) string {
	return fmt.Sprintf(`# Database on the shared xdev MariaDB (created with this app).
# xdev loads these into the app's environment on start.
DB_HOST=127.0.0.1
DB_PORT=%[3]d
DB_NAME=%[1]s
DB_USER=%[1]s
DB_PASSWORD=%[2]s
DB_DSN=%[1]s:%[2]s@tcp(127.0.0.1:%[3]d)/%[1]s?parseTime=true
`, name, pass, port)
}

// writeStaticPlaceholder drops a minimal index.html into dir if one isn't there.
func (s *Service) writeStaticPlaceholder(dir, appName string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	index := filepath.Join(dir, "index.html")
	if _, err := os.Stat(index); err == nil {
		return // don't clobber existing content
	}
	html := "<!doctype html>\n<meta charset=utf-8>\n<title>" + appName + "</title>\n" +
		"<h1>" + appName + "</h1>\n" +
		"<p>Static app served by xdev. Replace this file with your site.</p>\n"
	os.WriteFile(index, []byte(html), 0o644)
}

// Start (re)launches the app. Container apps recreate their compose stack
// (idempotent: up -d); static apps run their build step and, in command mode,
// (re)spawn their host process; proxy apps have nothing to run — running just
// means their route exists (the caller reconciles the proxy afterwards).
func (s *Service) Start(id int64) error {
	app, err := s.store.AppByID(id)
	if err != nil {
		return err
	}
	if app.IsProxy() {
		return s.store.SetAppStatus(app.ID, store.AppRunning)
	}
	if app.IsSharedWP() {
		// The site is served by the platform wp-host; make sure it's up, then
		// "running" just means the route exists.
		ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
		defer cancel()
		if _, err := s.ensureWPHost(ctx, runtime.Engine(app.Runtime)); err != nil {
			s.store.SetAppStatus(app.ID, store.AppError)
			return err
		}
		return s.store.SetAppStatus(app.ID, store.AppRunning)
	}
	if app.IsHostProc() {
		return s.startStatic(app)
	}
	if app.IsCompose() {
		// The file is the user's and may have been edited (or restored from a
		// backup) since the last start: re-read it, follow any port change, and
		// refresh the managed vars in _/.env before bringing the stack up.
		if _, err := s.prepareCompose(app); err != nil {
			s.store.SetAppStatus(app.ID, store.AppError)
			return err
		}
	}
	_, engine, workdir, pname, file, err := s.composeCtx(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
	defer cancel()
	if _, err := runtime.Up(ctx, engine, workdir, pname, file); err != nil {
		s.store.SetAppStatus(app.ID, store.AppError)
		return err
	}
	return s.store.SetAppStatus(app.ID, store.AppRunning)
}

// startStatic runs a static app's optional build step and, for command mode,
// (re)spawns its host process. Serve-mode apps need no process — Caddy serves
// their files — so a successful build (if any) is enough to mark them running.
func (s *Service) startStatic(app store.App) error {
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return err
	}
	dir := filepath.Join(proj.Dir, app.Slug)
	env := s.staticEnv(app, dir)

	// Both the build step and a supervised start command need the app's
	// toolchain on PATH; serve-mode static apps with no build step need nothing.
	if app.ServeMode == store.ServeCommand || strings.TrimSpace(app.BuildCmd) != "" {
		// Name the install route for the host OS: `brew` advice on a Linux server
		// is a dead end, and the installer is the answer on both.
		bin := "node"
		if app.Type == store.TypeGo {
			bin = "go"
		}
		install := "re-run the xdev installer, or install " + bin + " manually"
		if goruntime.GOOS == "darwin" {
			install = "brew install " + bin + ", or re-run the xdev installer"
		}
		if !hostproc.Has(bin) {
			s.store.SetAppStatus(app.ID, store.AppError)
			return errors.New("system " + bin + " not found on PATH — " + install)
		}
	}

	if cmd := strings.TrimSpace(app.BuildCmd); cmd != "" {
		if _, err := s.sup.RunBuild(dir, cmd, env); err != nil {
			s.store.SetAppStatus(app.ID, store.AppError)
			return err
		}
	}

	if app.ServeMode == store.ServeCommand {
		name := proj.Slug + "_" + app.Slug
		// Refuse to launch onto an occupied port (e.g. an orphaned dev server from
		// a prior run). Without this, a dev server like Vite — which has no strict
		// port — silently falls back to the next free port and ends up serving a
		// different app's domain through Caddy. Fail loudly instead.
		if app.Port > 0 && !portFree(app.Port) {
			s.store.SetAppStatus(app.ID, store.AppError)
			return fmt.Errorf("port %d already in use — another process is holding it; stop it and retry", app.Port)
		}
		if err := s.sup.Start(app.ID, name, dir, app.StartCmd, env); err != nil {
			s.store.SetAppStatus(app.ID, store.AppError)
			return err
		}
	}
	return s.store.SetAppStatus(app.ID, store.AppRunning)
}

// staticEnv builds the environment for a static app's process or build step: the
// host environment (so node/npm and PATH resolve), the allocated PORT/HOST, and
// the app's own .env file (KEY=VALUE lines) layered on top.
func (s *Service) staticEnv(app store.App, dir string) []string {
	env := os.Environ()
	if app.Port > 0 {
		env = append(env, fmt.Sprintf("PORT=%d", app.Port), "HOST=127.0.0.1")
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			env = append(env, line)
		}
	}
	return env
}

// ResumeStatic respawns command-mode static apps that were running before xdev
// restarted — their host processes die with xdev, unlike containers the engine
// keeps alive. Called once on boot.
func (s *Service) ResumeStatic() {
	apps, err := s.store.ResumableStaticApps()
	if err != nil {
		log.Printf("resume static apps: %v", err)
		return
	}
	for _, app := range apps {
		if err := s.startStatic(app); err != nil {
			log.Printf("resume static app %s: %v", app.Slug, err)
		}
	}
}

// Stop stops the app: container apps stop their containers (kept for a quick
// restart); static command-mode apps have their host process killed; proxy and
// shared-WP apps just go stopped (the caller's reconcile drops their route —
// the shared wp-host keeps serving other sites).
func (s *Service) Stop(id int64) error {
	app, err := s.store.AppByID(id)
	if err != nil {
		return err
	}
	if app.IsProxy() || app.IsSharedWP() {
		return s.store.SetAppStatus(id, store.AppStopped)
	}
	if app.IsHostProc() {
		s.sup.Stop(id)
		return s.store.SetAppStatus(id, store.AppStopped)
	}
	_, engine, workdir, pname, file, err := s.composeCtx(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
	defer cancel()
	if _, err := runtime.Stop(ctx, engine, workdir, pname, file); err != nil {
		return err
	}
	return s.store.SetAppStatus(app.ID, store.AppStopped)
}

// Delete tears the app down, removes the app row, and deletes its directory.
// Shared-database apps get their database dumped into backupsRoot first, then
// dropped (with its user) from the shared server.
func (s *Service) Delete(id int64, backupsRoot string) error {
	app, err := s.store.AppByID(id)
	if err != nil {
		return err
	}
	if app.IsProxy() {
		return s.store.DeleteApp(id) // nothing on disk or in a runtime to tear down
	}
	if app.IsSharedWP() {
		// No container of its own — dump-then-drop the shared database, remove
		// the row, then the docroot under data/wp/sites.
		proj, err := s.store.ProjectByID(app.ProjectID)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
		defer cancel()
		s.archiveSharedDB(ctx, runtime.Engine(app.Runtime), app, proj.Slug, backupsRoot)
		if err := s.store.DeleteApp(id); err != nil {
			return err
		}
		os.RemoveAll(s.wpSiteDir(proj.Slug, app.Slug))
		return nil
	}
	if app.IsHostProc() {
		s.sup.Stop(id)
		dir := s.appDir(app)
		if app.DBMode == store.DBShared {
			// Same dump-then-drop as container apps, or the database outlives the
			// app that owned it.
			proj, err := s.store.ProjectByID(app.ProjectID)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
			defer cancel()
			s.archiveSharedDB(ctx, runtime.Engine(app.Runtime), app, proj.Slug, backupsRoot)
		}
		if err := s.store.DeleteApp(id); err != nil {
			return err
		}
		if dir != "" && dir != "." && dir != "/" {
			os.RemoveAll(dir)
		}
		return nil
	}
	_, engine, workdir, pname, file, err := s.composeCtx(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
	defer cancel()
	if app.DBMode == store.DBShared {
		if proj, err := s.store.ProjectByID(app.ProjectID); err == nil {
			s.archiveSharedDB(ctx, engine, app, proj.Slug, backupsRoot)
		}
	}
	// Best-effort container teardown; proceed with row/dir removal regardless.
	runtime.Down(ctx, engine, workdir, pname, file)
	if err := s.store.DeleteApp(id); err != nil {
		return err
	}
	appDir := filepath.Dir(workdir) // parent of the _/ directory
	if appDir != "" && appDir != "." && appDir != "/" {
		os.RemoveAll(appDir)
	}
	return nil
}

// RefreshStatus reconciles the stored status with reality and returns the
// up-to-date status string.
func (s *Service) RefreshStatus(id int64) (string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", err
	}
	if app.IsProxy() || app.IsSharedWP() {
		return app.Status, nil // no process or container of its own to check
	}
	if app.IsHostProc() {
		// Serve-mode apps have no process; their state is whatever create/start
		// set. Command-mode apps reflect the live host process.
		status := app.Status
		if app.ServeMode == store.ServeCommand {
			status = store.AppStopped
			if s.sup.Running(id) {
				status = store.AppRunning
			}
		}
		if status != app.Status {
			s.store.SetAppStatus(app.ID, status)
		}
		return status, nil
	}
	_, engine, workdir, pname, file, err := s.composeCtx(id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status := store.AppStopped
	if runtime.Running(ctx, engine, workdir, pname, file) {
		status = store.AppRunning
	}
	if status != app.Status {
		s.store.SetAppStatus(app.ID, status)
	}
	return status, nil
}

// composeCtx loads an app and derives everything the runtime needs: the engine,
// the working directory (the _/ dir), the compose project name, and the file.
func (s *Service) composeCtx(id int64) (store.App, runtime.Engine, string, string, string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return store.App{}, "", "", "", "", err
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return store.App{}, "", "", "", "", err
	}
	engine := s.sel.Current()
	if app.Runtime != "" {
		engine = runtime.Engine(app.Runtime)
	}
	workdir := filepath.Dir(app.ComposePath)
	pname := proj.Slug + "_" + app.Slug
	return app, engine, workdir, pname, app.ComposePath, nil
}

// writeScaffold copies template scaffold files into the app content dir,
// skipping any that already exist. A ".tpl" suffix is stripped from the
// destination (it keeps scaffold sources like main.go out of xdev's own build).
func (s *Service) writeScaffold(appType, contentDir string) error {
	files, err := templates.ScaffoldFiles(appType)
	if err != nil {
		return err
	}
	for rel, data := range files {
		dest := filepath.Join(contentDir, strings.TrimSuffix(rel, ".tpl"))
		if _, err := os.Stat(dest); err == nil {
			continue // don't overwrite user content
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// allocPort returns a single free host port (see allocPorts).
func (s *Service) allocPort(avoid ...int) (int, error) {
	ports, err := s.allocPorts(1, avoid...)
	if err != nil {
		return 0, err
	}
	return ports[0], nil
}

// allocPorts returns n distinct free host ports in [portMin, portMax] not
// already assigned (apps + secondary-service domains) and not currently bound on
// the host. The ports avoid each other so a multi-service app (e.g. Laravel +
// Adminer) gets a clean set in one pass. Callers pass any in-flight port in
// avoid: a port picked earlier in the same create is neither in the store yet
// nor bound, so it would otherwise be handed out twice.
func (s *Service) allocPorts(n int, avoid ...int) ([]int, error) {
	used, err := s.store.UsedPorts()
	if err != nil {
		return nil, err
	}
	out := pickPorts(n, append(used, avoid...), portFree)
	if len(out) < n {
		return nil, errors.New("no free host port available in range")
	}
	return out, nil
}

// pickPorts returns up to n ports in [portMin, portMax] that are neither taken
// nor rejected by free, never repeating one within a pass.
func pickPorts(n int, taken []int, free func(int) bool) []int {
	skip := make(map[int]bool, len(taken))
	for _, p := range taken {
		skip[p] = true
	}
	out := make([]int, 0, n)
	for p := portMin; p <= portMax && len(out) < n; p++ {
		if skip[p] || !free(p) {
			continue
		}
		out = append(out, p)
		skip[p] = true // don't hand the same port out twice in this pass
	}
	return out
}

// portFree reports whether a host port is available. It binds both the wildcard
// address (":p", all interfaces/families — how the container engine publishes
// ports) and loopback explicitly: SO_REUSEADDR lets a wildcard bind succeed on
// macOS even when something already holds 127.0.0.1:p (e.g. podman's gvproxy),
// so the wildcard check alone hands out ports that are already in use.
func portFree(p int) bool {
	for _, addr := range []string{fmt.Sprintf(":%d", p), fmt.Sprintf("127.0.0.1:%d", p)} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		ln.Close()
	}
	return true
}

// SetDomain changes the hostname an app is served at, updating both the app row
// and its proxy domain. The caller reconciles the proxy afterwards.
func (s *Service) SetDomain(id int64, domain string) error {
	app, err := s.store.AppByID(id)
	if err != nil {
		return err
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return err
	}
	domain = normalizeHost(domain)
	if err := validHost(domain); err != nil {
		return err
	}
	if owner := s.store.DomainOwner(domain); owner != 0 && owner != id {
		return fmt.Errorf("domain %q is already in use", domain)
	}
	sslMode, isLocal := "internal", true
	if proj.Environment == "prod" {
		sslMode, isLocal = "letsencrypt", false
	}
	if err := s.store.SetAppDomain(id, domain); err != nil {
		return err
	}
	return s.store.ReplaceAppDomain(id, domain, isLocal, sslMode)
}

// validUpstream validates a proxy app's upstream at the trust boundary: it must
// parse as a bare http(s) URL — scheme + host[:port], nothing else (no path,
// query, or credentials). Returns the normalized scheme://host[:port] form.
func validUpstream(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("upstream URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("invalid upstream %q — use http(s)://host[:port]", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// normalizeHost lowercases and trims a hostname.
func normalizeHost(h string) string { return strings.TrimSpace(strings.ToLower(h)) }

// validHost does a light sanity check on a hostname (no scheme, port, or path).
func validHost(h string) error {
	if h == "" {
		return errors.New("domain is required")
	}
	if strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return fmt.Errorf("invalid domain %q", h)
	}
	for _, r := range h {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.'
		if !ok {
			return fmt.Errorf("invalid domain %q (use letters, digits, '-' and '.')", h)
		}
	}
	return nil
}
