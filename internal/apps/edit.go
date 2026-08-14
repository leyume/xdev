package apps

// Editing an app after it exists. Create decides an app's *identity* — its
// type, slug, directory, containers, database — and none of that can move
// afterwards without being a migration rather than an edit. Everything else is
// configuration, and this file is where it changes: the display name, the
// hostnames it answers on, the per-type settings the create form collected, and
// (for apps that have one) the compose file itself.
//
// The whole update is validated before any of it is written, so a rejected form
// leaves the app exactly as it was — on disk, in the database, and in the proxy.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"xdev/internal/store"
)

// EditOpts carries the settings form. Fields that don't apply to an app's type
// are ignored, so the same struct serves every type.
type EditOpts struct {
	Name   string
	Domain string

	// Host apps (static, go).
	ServeMode string // serve | command
	RootDir   string
	BuildCmd  string
	StartCmd  string
	// SourceDir re-points the app at a different directory on the host; blank
	// puts it back on the managed <project.dir>/<slug>.
	SourceDir string

	// Proxy apps.
	Upstream string

	// Any app with a compose file: its new contents. Blank leaves the file
	// alone — the form only sends it when the app has one to edit.
	ComposeFile string

	// Compose apps: hostnames for domains 2..N in slot order, the same
	// positional mapping the create form uses (ExtraDomains[0] is ${PORT_2}).
	// Adding one allocates a port; removing one frees it.
	ExtraDomains []string

	// Types that bundle a second web UI (Laravel's Adminer, the mail admin
	// console): the hostname it is served at. Its port never moves.
	ServiceDomain string
}

// Update applies the settings form to an existing app and returns the saved row.
// The caller restarts the app (settings only reach a running stack on restart)
// and reconciles the proxy.
func (s *Service) Update(id int64, opts EditOpts) (store.App, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return store.App{}, err
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return app, err
	}

	// ---- validate everything first ----
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return app, errors.New("app name is required")
	}
	// The domain field lists every hostname this app answers on, comma-separated.
	// Dropping one from the list is how a hostname stops routing, so an empty
	// field is rejected rather than read as "no change".
	hosts, err := parseHosts(opts.Domain)
	if err != nil {
		return app, err
	}
	if len(hosts) == 0 {
		return app, errors.New("domain is required")
	}
	for _, h := range hosts {
		if owner := s.store.DomainOwner(h); owner != 0 && owner != app.ID {
			return app, fmt.Errorf("domain %q is already in use", h)
		}
	}
	domain := hosts[0]

	svc, err := s.editServiceDomains(app, opts, hosts)
	if err != nil {
		return app, err
	}

	// The compose file, when this app has one and a body was submitted. For a
	// bring-your-own stack it is checked against the domains above: every slot
	// needs a service, and every service slot needs a domain.
	var composeBody string
	if app.ComposePath != "" && strings.TrimSpace(opts.ComposeFile) != "" {
		if composeBody, err = s.checkComposeEdit(app, opts.ComposeFile, len(svc)+1, svc); err != nil {
			return app, err
		}
	}

	updated := app
	updated.Name = name
	updated.Domain = domain
	switch {
	case app.IsProxy():
		if updated.Upstream, err = validUpstream(opts.Upstream); err != nil {
			return app, err
		}
	case app.IsHostProc():
		if err := s.editHostApp(&updated, opts); err != nil {
			return app, err
		}
	}

	// ---- apply ----
	// A host app moved back onto the managed path needs that directory to exist:
	// one created against an external folder never had it made. Anything already
	// there is left alone, and the folder it is moving away from is untouched.
	if updated.IsHostProc() && updated.SourceDir == "" && app.SourceDir != "" {
		if err := os.MkdirAll(filepath.Join(proj.Dir, updated.Slug), 0o755); err != nil {
			return app, err
		}
	}
	if composeBody != "" {
		if err := writeComposeEdit(app.ComposePath, composeBody); err != nil {
			return app, err
		}
	}
	if err := s.store.UpdateApp(updated); err != nil {
		return app, err
	}
	sslMode, isLocal := "internal", true
	if proj.Environment == "prod" {
		sslMode, isLocal = "letsencrypt", false
	}
	if err := s.store.ReplaceAppDomains(app.ID, hosts, svc, isLocal, sslMode); err != nil {
		return app, err
	}
	if updated.IsCompose() {
		// Keep _/.env in step with the domains the app now has, so the numbers
		// behind ${PORT_n} are right even before the next start rewrites them.
		ports := make([]int, 0, len(svc)+1)
		ports = append(ports, updated.Port)
		for _, d := range svc {
			ports = append(ports, d.Port)
		}
		if err := ensureComposeEnv(filepath.Dir(app.ComposePath), ports); err != nil {
			return app, err
		}
	}
	return s.store.AppByID(app.ID)
}

// editServiceDomains resolves the hostnames an app serves *besides* its own
// domains, each paired with the host port it routes to. own is every hostname
// the app itself answers on, which these have to stay clear of.
//
// A compose app has one per extra domain and the list is the user's to grow or
// shrink: slots that already exist keep their allocated port (so nothing behind
// an unchanged domain moves), new ones get a fresh port, and dropped ones
// release theirs. Every other type has a fixed set — one bundled UI (Adminer,
// the mail console) or none — where only the hostname is editable.
//
// Unlike the app's own domain field, these take a single hostname each: a slot's
// position is what maps it to ${PORT_n}, so a list in one field would blur how
// many slots there are.
func (s *Service) editServiceDomains(app store.App, opts EditOpts, own []string) ([]store.ServiceDomain, error) {
	existing, err := s.store.AppServiceDomains(app.ID)
	if err != nil {
		return nil, err
	}
	if !app.IsCompose() {
		if len(existing) == 0 {
			return nil, nil
		}
		host := normalizeHost(opts.ServiceDomain)
		if host == "" {
			host = existing[0].Host // not offered by this app's form: leave it
		}
		if err := validHost(host); err != nil {
			return nil, err
		}
		if owner := s.store.DomainOwner(host); owner != 0 && owner != app.ID {
			return nil, fmt.Errorf("domain %q is already in use", host)
		}
		if slices.Contains(own, host) {
			return nil, fmt.Errorf("%q is already one of this app's own domains — give the second one its own hostname", host)
		}
		out := make([]store.ServiceDomain, len(existing))
		copy(out, existing)
		out[0].Host = host
		return out, nil
	}

	hosts, err := s.composeExtraHosts(opts.ExtraDomains, own, app.ID)
	if err != nil {
		return nil, err
	}
	out := make([]store.ServiceDomain, 0, len(hosts))
	// Ports already allocated to this app's slots stay put; only the slots added
	// by this edit need new ones, and they must avoid the app's own port.
	need := len(hosts) - len(existing)
	var fresh []int
	if need > 0 {
		own := []int{app.Port}
		for _, d := range existing {
			own = append(own, d.Port)
		}
		if fresh, err = s.allocPorts(need, own...); err != nil {
			return nil, err
		}
	}
	for i, h := range hosts {
		port := 0
		if i < len(existing) {
			port = existing[i].Port
		} else {
			port = fresh[i-len(existing)]
		}
		out = append(out, store.ServiceDomain{Host: h, Port: port})
	}
	return out, nil
}

// editHostApp folds the static/go fields into the row. A command-mode app needs
// a port and a command; both are filled in the same way create fills them, so an
// app switched from serving files to running one behaves like a fresh one.
//
// The app's directory is editable here, unlike everything else that names where
// an app lives. It can be: a host app's files are the user's own, so re-pointing
// one only changes which directory the build runs in and which folder Caddy
// serves. Nothing is moved, copied, or deleted — including the directory being
// pointed away from, which is left exactly as it is.
func (s *Service) editHostApp(app *store.App, opts EditOpts) error {
	src, err := validSourceDir(opts.SourceDir)
	if err != nil {
		return err
	}
	app.SourceDir = src

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
	app.StartCmd = strings.TrimSpace(opts.StartCmd)
	if mode != store.ServeCommand {
		return nil
	}
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
	if app.Port == 0 { // was serving files: it needs a port to run on now
		port, err := s.allocPort()
		if err != nil {
			return err
		}
		app.Port = port
	}
	return nil
}

// checkComposeEdit validates a replacement compose file. Every stack gets the
// structural checks a supplied file gets at create; a bring-your-own stack also
// has to publish exactly the slots its domains expect, and may not hard-code a
// host port another app owns.
func (s *Service) checkComposeEdit(app store.App, body string, slots int, svc []store.ServiceDomain) (string, error) {
	yaml := ensureTrailingNewline(strings.TrimSpace(normalizeYAML(body)))
	if err := validCompose(yaml); err != nil {
		return "", err
	}
	if !app.IsCompose() {
		// A templated stack's ports are xdev's own, written into the file at
		// create; the file is the user's to edit, but its slots are not a
		// contract xdev checks.
		return yaml, nil
	}
	ports, err := ComposePorts(yaml)
	if err != nil {
		return "", err
	}
	if err := checkComposeSlots(ports, slots); err != nil {
		return "", err
	}
	own := []int{app.Port}
	for _, d := range svc {
		own = append(own, d.Port)
	}
	for _, p := range ports {
		if p.Var() {
			continue
		}
		if err := s.portTaken(p.Port, own...); err != nil {
			return "", err
		}
	}
	return yaml, nil
}

// ReadCompose returns the app's compose file for editing ("" when it has none —
// proxy apps and shared-host WordPress sites don't have one).
func (s *Service) ReadCompose(id int64) (string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", err
	}
	if app.ComposePath == "" {
		return "", nil
	}
	b, err := os.ReadFile(app.ComposePath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// writeComposeEdit replaces a compose file, keeping the previous contents beside
// it as compose.yml.bak. The engine is pointed at the file by name, so the
// backup is inert — but a stack that stops coming up after an edit is exactly
// when the old file is worth having.
func writeComposeEdit(path, body string) error {
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0o644)
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
