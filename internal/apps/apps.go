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
	"log"
	"net"
	"os"
	"path/filepath"
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
const defaultStartCmd = `npm install && npm run dev -- --host 127.0.0.1 --port $PORT`

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
}

// New creates an app Service.
func New(st *store.Store, sel *runtime.Selector, sup *hostproc.Supervisor) *Service {
	return &Service{store: st, sel: sel, sup: sup}
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

	if opts.Type == store.TypeStatic {
		if err := s.layoutStatic(&app, &opts, appDir); err != nil {
			return store.App{}, err
		}
	} else {
		if err := s.layoutContainer(&app, &opts, proj, appDir); err != nil {
			return store.App{}, err
		}
	}

	saved, err := s.store.CreateApp(app)
	if err != nil {
		os.RemoveAll(appDir)
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

	if err := s.Start(saved.ID); err != nil {
		return saved, err
	}
	return s.store.AppByID(saved.ID)
}

// layoutContainer builds a container app's _/compose.yml + app/ layout and sets
// the port, limits, and compose path on the app row.
func (s *Service) layoutContainer(app *store.App, opts *CreateOpts, proj store.Project, appDir string) error {
	port, err := s.allocPort()
	if err != nil {
		return err
	}
	underscore := filepath.Join(appDir, "_")
	content := filepath.Join(appDir, "app")
	for _, d := range []string{underscore, content} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	composeStr, err := templates.RenderCompose(opts.Type, templates.Data{
		ProjectSlug: proj.Slug,
		NetworkName: proj.NetworkName,
		AppSlug:     app.Slug,
		AppType:     opts.Type,
		Env:         proj.Environment,
		HostPort:    port,
		CPULimit:    opts.CPULimit,
		MemLimit:    opts.MemLimit,
	})
	if err != nil {
		os.RemoveAll(appDir)
		return err
	}
	composePath := filepath.Join(underscore, "compose.yml")
	if err := os.WriteFile(composePath, []byte(composeStr), 0o644); err != nil {
		os.RemoveAll(appDir)
		return err
	}
	// Drop scaffold content (placeholder index.html etc.) without clobbering
	// anything already present.
	if err := s.writeScaffold(opts.Type, content); err != nil {
		os.RemoveAll(appDir)
		return err
	}
	app.Port = port
	app.CPULimit = opts.CPULimit
	app.MemLimit = opts.MemLimit
	app.ComposePath = composePath
	return nil
}

// layoutStatic builds a static app's folder — code lives directly inside, with
// no _/ or app/ subdirectories — and records its serve config. Command-mode
// apps get a host port for their dev server; serve-mode apps are file-served by
// Caddy and need none.
func (s *Service) layoutStatic(app *store.App, opts *CreateOpts, appDir string) error {
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return err
	}
	mode := opts.ServeMode
	if mode != store.ServeCommand {
		mode = store.ServeStatic
	}
	app.ServeMode = mode
	app.RootDir = strings.Trim(strings.TrimSpace(opts.RootDir), "/")
	app.BuildCmd = strings.TrimSpace(opts.BuildCmd)

	switch mode {
	case store.ServeCommand:
		app.StartCmd = strings.TrimSpace(opts.StartCmd)
		if app.StartCmd == "" {
			app.StartCmd = defaultStartCmd
		}
		port, err := s.allocPort()
		if err != nil {
			os.RemoveAll(appDir)
			return err
		}
		app.Port = port
		// Drop a runnable Vite starter directly in the folder (skipping any files
		// the user already added) so `npm install && npm run dev` works on first
		// start; the user can replace it with their own project.
		if err := s.writeScaffold(store.TypeStatic, appDir); err != nil {
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
// (re)spawn their host process.
func (s *Service) Start(id int64) error {
	app, err := s.store.AppByID(id)
	if err != nil {
		return err
	}
	if app.IsStatic() {
		return s.startStatic(app)
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

	needsNode := app.ServeMode == store.ServeCommand || strings.TrimSpace(app.BuildCmd) != ""
	if needsNode && !hostproc.HasNode() {
		s.store.SetAppStatus(app.ID, store.AppError)
		return errors.New("system Node not found on PATH — install Node (e.g. `brew install node`) or re-run the xdev installer")
	}

	if cmd := strings.TrimSpace(app.BuildCmd); cmd != "" {
		if _, err := s.sup.RunBuild(dir, cmd, env); err != nil {
			s.store.SetAppStatus(app.ID, store.AppError)
			return err
		}
	}

	if app.ServeMode == store.ServeCommand {
		name := proj.Slug + "_" + app.Slug
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
// restart); static command-mode apps have their host process killed.
func (s *Service) Stop(id int64) error {
	app, err := s.store.AppByID(id)
	if err != nil {
		return err
	}
	if app.IsStatic() {
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
func (s *Service) Delete(id int64) error {
	app, err := s.store.AppByID(id)
	if err != nil {
		return err
	}
	if app.IsStatic() {
		s.sup.Stop(id)
		dir := s.appDir(app)
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
	if app.IsStatic() {
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
// skipping any that already exist.
func (s *Service) writeScaffold(appType, contentDir string) error {
	files, err := templates.ScaffoldFiles(appType)
	if err != nil {
		return err
	}
	for rel, data := range files {
		dest := filepath.Join(contentDir, rel)
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

// allocPort returns a free host port in [portMin, portMax] not already assigned
// to another app and not currently bound on the host.
func (s *Service) allocPort() (int, error) {
	used, err := s.store.UsedPorts()
	if err != nil {
		return 0, err
	}
	taken := make(map[int]bool, len(used))
	for _, p := range used {
		taken[p] = true
	}
	for p := portMin; p <= portMax; p++ {
		if taken[p] {
			continue
		}
		if portFree(p) {
			return p, nil
		}
	}
	return 0, errors.New("no free host port available in range")
}

// portFree reports whether a host port is available. It binds the wildcard
// address (":p", all interfaces/families) to match how the container engine
// publishes ports — a loopback-only check would miss a conflict with an engine
// already publishing on 0.0.0.0/[::] (e.g. another xdev instance's app).
func portFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
	if err != nil {
		return false
	}
	ln.Close()
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
