// Package apps implements app-level lifecycle: rendering an app's compose stack
// from a template, scaffolding starter content, allocating a host port, and
// driving start/stop/delete through the container runtime.
//
// Each app generates the bizepp-style layout:
//
//	projects/<project>/<app>/_/compose.yml   (generated)
//	projects/<project>/<app>/app/            (bind-mounted content)
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

	"xdev/internal/naming"
	"xdev/internal/runtime"
	"xdev/internal/store"
	"xdev/internal/templates"
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
}

// New creates an app Service.
func New(st *store.Store, sel *runtime.Selector) *Service {
	return &Service{store: st, sel: sel}
}

// Create renders and persists a new app, writes its files, and starts it.
// It returns the saved app even if the initial start fails, with the error, so
// the UI can show the app in an error state rather than losing it.
func (s *Service) Create(projectID int64, name, appType, domain string, cpuLimit float64, memLimit int64) (store.App, error) {
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
	name = strings.TrimSpace(name)
	if name == "" {
		return store.App{}, errors.New("app name is required")
	}
	if !templates.IsValidType(appType) {
		return store.App{}, fmt.Errorf("app type %q is not available yet", appType)
	}

	slug := naming.Unique(name, func(c string) bool { return s.store.AppSlugExists(projectID, c) })

	// Domain is free-form. If left blank, default to the project's base domain
	// directly (so the first app is served at the bare domain); if that's
	// already taken, fall back to <app-slug>.<base-domain>.
	domain = normalizeHost(domain)
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

	port, err := s.allocPort()
	if err != nil {
		return store.App{}, err
	}

	// Build the on-disk layout: <project.Dir>/<slug>/{_,app}.
	appDir := filepath.Join(proj.Dir, slug)
	underscore := filepath.Join(appDir, "_")
	content := filepath.Join(appDir, "app")
	for _, d := range []string{underscore, content} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return store.App{}, err
		}
	}

	// Render compose.
	composeStr, err := templates.RenderCompose(appType, templates.Data{
		ProjectSlug: proj.Slug,
		NetworkName: proj.NetworkName,
		AppSlug:     slug,
		AppType:     appType,
		Env:         proj.Environment,
		HostPort:    port,
		CPULimit:    cpuLimit,
		MemLimit:    memLimit,
	})
	if err != nil {
		os.RemoveAll(appDir)
		return store.App{}, err
	}
	composePath := filepath.Join(underscore, "compose.yml")
	if err := os.WriteFile(composePath, []byte(composeStr), 0o644); err != nil {
		os.RemoveAll(appDir)
		return store.App{}, err
	}

	// Drop scaffold content (placeholder index.html etc.) without clobbering
	// anything already present.
	if err := s.writeScaffold(appType, content); err != nil {
		os.RemoveAll(appDir)
		return store.App{}, err
	}

	saved, err := s.store.CreateApp(store.App{
		ProjectID:   projectID,
		Name:        name,
		Slug:        slug,
		Type:        appType,
		Status:      store.AppStopped,
		Domain:      domain,
		Port:        port,
		CPULimit:    cpuLimit,
		MemLimit:    memLimit,
		Runtime:     appEngine,
		ComposePath: composePath,
	})
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

// Start (re)creates and starts the app's containers (idempotent: uses up -d).
func (s *Service) Start(id int64) error {
	app, engine, workdir, pname, file, err := s.composeCtx(id)
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

// Stop stops the app's containers but keeps them for a quick restart.
func (s *Service) Stop(id int64) error {
	app, engine, workdir, pname, file, err := s.composeCtx(id)
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

// Delete brings the stack down, removes the app row, and deletes its directory.
func (s *Service) Delete(id int64) error {
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

// RefreshStatus reconciles the stored status with the runtime and returns the
// up-to-date status string.
func (s *Service) RefreshStatus(id int64) (string, error) {
	app, engine, workdir, pname, file, err := s.composeCtx(id)
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
