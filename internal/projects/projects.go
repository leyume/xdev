// Package projects implements project-level lifecycle: creating a project's
// directory + shared container network, and tearing them down. App-level
// operations live in package apps; the server coordinates deleting a project's
// apps before the project itself.
package projects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xdev/internal/config"
	"xdev/internal/naming"
	"xdev/internal/runtime"
	"xdev/internal/store"
)

// Service holds the dependencies for project operations.
type Service struct {
	store *store.Store
	cfg   config.Config
	sel   *runtime.Selector
}

// New creates a project Service.
func New(st *store.Store, cfg config.Config, sel *runtime.Selector) *Service {
	return &Service{store: st, cfg: cfg, sel: sel}
}

// Create makes a new project: assigns a unique slug, creates its directory and
// shared network, and persists the row. environment defaults to "local" and
// base_domain defaults to "<slug>.test".
func (s *Service) Create(name, baseDomain, environment string) (store.Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.Project{}, errors.New("project name is required")
	}
	if environment == "" {
		environment = "local"
	}
	slug := naming.Unique(name, s.store.ProjectSlugExists)
	if baseDomain == "" {
		// .localhost auto-resolves to 127.0.0.1 in browsers with no /etc/hosts
		// edit needed — the least-friction default for local development.
		baseDomain = slug + ".localhost"
	}
	network := "xdev_" + slug
	dir := filepath.Join(s.cfg.ProjectsDir, slug)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.Project{}, err
	}

	engine := s.sel.Current()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runtime.NetworkCreate(ctx, engine, network); err != nil {
		os.RemoveAll(dir)
		return store.Project{}, err
	}

	p, err := s.store.CreateProject(store.Project{
		Name:        name,
		Slug:        slug,
		BaseDomain:  baseDomain,
		Environment: environment,
		NetworkName: network,
		Engine:      string(engine),
		Dir:         dir,
	})
	if err != nil {
		os.RemoveAll(dir)
		runtime.NetworkRemove(ctx, engine, network)
		return store.Project{}, err
	}
	return p, nil
}

// MaxProjectName bounds a project's display name. Long enough for a real
// title, short enough that it cannot push the page head, the breadcrumb and
// every event message out of shape.
const MaxProjectName = 80

// Rename changes a project's display name and returns the updated project.
//
// Nothing else moves. The slug — and so the project's directory, network,
// container names and databases — is fixed at creation: renaming is a label
// change, and anyone expecting the URL to follow is better served by being told
// it does not than by having their apps' infrastructure quietly rebuilt
// underneath them.
func (s *Service) Rename(id int64, name string) (store.Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.Project{}, errors.New("project name is required")
	}
	if len([]rune(name)) > MaxProjectName {
		return store.Project{}, fmt.Errorf("project name is too long (max %d characters)", MaxProjectName)
	}
	p, err := s.store.ProjectByID(id)
	if err != nil {
		return store.Project{}, err
	}
	if p.Name == name {
		return p, nil
	}
	if err := s.store.RenameProject(id, name); err != nil {
		return store.Project{}, err
	}
	return s.store.ProjectByID(id)
}

// Delete removes the project's network, directory, and row. Callers must delete
// the project's apps first (so their containers are brought down).
func (s *Service) Delete(id int64) error {
	p, err := s.store.ProjectByID(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Best-effort network + directory cleanup; the row delete is the part that
	// must succeed for the project to disappear from the UI. Use the engine the
	// project was created with (falling back to the current default).
	engine := runtime.Engine(p.Engine)
	if engine == "" {
		engine = s.sel.Current()
	}
	runtime.NetworkRemove(ctx, engine, p.NetworkName)
	if p.Dir != "" {
		os.RemoveAll(p.Dir)
	}
	return s.store.DeleteProject(id)
}
