package apps

// Webmail in a mail app: SnappyMail on the app's own domain, and Rail beside
// it on a hostname of its own. Either can be switched off, not both.
//
// Like the Laravel database UI (adminer.go), there is no column for any of it:
// SnappyMail is on when the compose file has a `webmail` service, Rail when it
// has a `rail` service, and Rail's hostname is the service-domain row routed to
// the port that service publishes. The mail admin console stays the app's
// first service domain; Rail's row comes after it, so everything that reads
// svc[0] as the console is unaffected.
//
// With SnappyMail off, Rail also publishes the app's own port, so the app's
// own domain keeps serving webmail (Rail's) instead of answering 502.
//
// Switching only ever touches the webmail containers (`up -d --no-deps` /
// `rm`): restarting the stack would bounce Stalwart, and mail delivery, for a
// webmail change.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
	"xdev/internal/templates"
)

const (
	railService   = "rail"
	snappyService = "webmail"
)

// CanToggleRail reports whether an app has switchable webmail: a mail app
// with a compose file.
func CanToggleRail(app store.App) bool {
	return app.Type == "mail" && app.ComposePath != ""
}

// railPortRE matches a published port line of the rail block: - "20031:8080",
// also as hand-edited in the Compose tab — single-quoted or bare, bound to an
// address, with /tcp or a trailing comment.
var railPortRE = regexp.MustCompile(`^\s*-\s*["']?(?:(?:[0-9.]+|\[[0-9A-Fa-f:]+\]):)?(\d+):8080(?:/tcp)?["']?\s*(?:#.*)?$`)

func hasComposeService(yaml, name string) bool {
	_, _, ok := findComposeService(strings.Split(yaml, "\n"), name)
	return ok
}

// composeRailPort returns Rail's own host port (the first port line of the
// rail service), or 0 when the compose file has no rail service.
func composeRailPort(yaml string) int {
	lines := strings.Split(yaml, "\n")
	start, indent, ok := findComposeService(lines, railService)
	if !ok {
		return 0
	}
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(strings.TrimSpace(l), "#") && lineIndent(l) <= indent {
			break
		}
		if m := railPortRE.FindStringSubmatch(l); m != nil {
			p, _ := strconv.Atoi(m[1])
			return p
		}
	}
	return 0
}

// railRows is the service-domain rows that belong to Rail: every one after the
// admin console's (svc[0]). Found from the rows, not the compose text, so a
// hand-edited rail block can't leave a hostname routed that no switch reaches.
func railRows(svc []store.ServiceDomain) []store.ServiceDomain {
	if len(svc) < 2 {
		return nil
	}
	return svc[1:]
}

// Webmail is what the Services tab shows about a mail app's webmail.
type Webmail struct {
	Snappy   bool   // SnappyMail is on (serving the app's own domain)
	Rail     bool   // Rail is on: a rail service, or a hostname routed to it
	RailHost string // Rail's own hostname ("" = off, or a rail service with no route)
	RailPort int    // Rail's own host port (0 = off, or unreadable)
}

// WebmailState reads a mail app's webmail from its compose file and domains.
func (s *Service) WebmailState(app store.App) Webmail {
	var w Webmail
	if !CanToggleRail(app) {
		return w
	}
	yaml, err := s.ReadCompose(app.ID)
	if err != nil {
		return w
	}
	w.Snappy = hasComposeService(yaml, snappyService)
	w.RailPort = composeRailPort(yaml)
	svc, _ := s.store.AppServiceDomains(app.ID)
	if rows := railRows(svc); len(rows) > 0 {
		w.RailHost = rows[0].Host
		if w.RailPort == 0 {
			w.RailPort = rows[0].Port
		}
	}
	w.Rail = w.RailHost != "" || hasComposeService(yaml, railService)
	return w
}

// webmailCtx is what both switches load before editing.
type webmailCtx struct {
	app  store.App
	proj store.Project
	yaml string
}

func (s *Service) loadWebmail(id int64) (webmailCtx, error) {
	var c webmailCtx
	var err error
	if c.app, err = s.store.AppByID(id); err != nil {
		return c, err
	}
	if !CanToggleRail(c.app) {
		return c, fmt.Errorf("%s apps have no webmail to switch", c.app.Type)
	}
	if c.proj, err = s.store.ProjectByID(c.app.ProjectID); err != nil {
		return c, err
	}
	if c.yaml, err = s.ReadCompose(c.app.ID); err != nil {
		return c, err
	}
	if strings.TrimSpace(c.yaml) == "" {
		return c, errors.New("this app has no compose file to add a service to")
	}
	return c, nil
}

func (c webmailCtx) data() templates.Data {
	return templates.Data{
		ProjectSlug: c.proj.Slug,
		AppSlug:     c.app.Slug,
		AppType:     c.app.Type,
		Env:         c.proj.Environment,
		HostPort:    c.app.Port,
		PublishIP:   railPublishIP(),
	}
}

// railPublishIP is the address Rail's ports are published on. Rail trusts
// X-Forwarded-For (the proxy replaces it with the real client), so its ports
// must not be reachable except through the proxy: loopback when the proxy
// dials host ports there (the default), all interfaces only when it comes in
// from a container network (XDEV_UPSTREAM_HOST=host.docker.internal etc.).
func railPublishIP() string {
	switch h := os.Getenv("XDEV_UPSTREAM_HOST"); h {
	case "", "127.0.0.1", "localhost", "::1":
		return "127.0.0.1"
	default:
		return ""
	}
}

// railDataUID is the user Rail's image runs as (nobody).
const railDataUID = 65534

// ensureRailData creates ./rail-data before compose would (as root) and gives
// it to Rail's user, so an image running unprivileged can write its database
// and key — also one left root-owned by an older image. Only root can chown;
// elsewhere (tests, a non-root install) it is left as it is.
func ensureRailData(composePath string) error {
	dir := filepath.Join(filepath.Dir(composePath), "rail-data")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return nil
	}
	return filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, railDataUID, railDataUID)
	})
}

// putRail replaces (or adds) the rail block, rendered for port; onAppPort
// also publishes the app's own port (SnappyMail off).
func (c *webmailCtx) putRail(port int, onAppPort bool) error {
	d := c.data()
	d.RailPort, d.RailOnAppPort = port, onAppPort
	block, err := templates.RenderPartial(c.app.Type, "rail_service", d)
	if err != nil {
		return err
	}
	c.yaml, _ = composeRemoveService(c.yaml, railService)
	c.yaml, err = composeAddService(c.yaml, block)
	return err
}

// RailHost returns Rail's hostname and host port, or "" and 0 when Rail is off.
func (s *Service) RailHost(app store.App) (string, int) {
	w := s.WebmailState(app)
	return w.RailHost, w.RailPort
}

// SetRail adds Rail at host (default rail.<app domain>) or removes it. It
// returns the hostname it published ("" when turning Rail off). Turning it on
// while it is on moves it to host, keeping its port and data, and brings its
// block up to date with the current template.
//
// Nothing is routed until Rail's container is up: a failed start puts the
// compose file back and leaves the domains as they were.
func (s *Service) SetRail(id int64, on bool, host string) (string, error) {
	c, err := s.loadWebmail(id)
	if err != nil {
		return "", err
	}
	app := c.app
	existing, err := s.store.AppServiceDomains(app.ID)
	if err != nil {
		return "", err
	}
	own, err := s.store.AppHostnames(app.ID)
	if err != nil {
		return "", err
	}
	if len(own) == 0 {
		own = []string{app.Domain}
	}
	snappy := hasComposeService(c.yaml, snappyService)
	hasRail := hasComposeService(c.yaml, railService)
	rows := railRows(existing)
	curPort := composeRailPort(c.yaml)
	if curPort == 0 && len(rows) > 0 {
		curPort = rows[0].Port
	}

	// The admin console's row stays first; Rail's (if any) is rebuilt below.
	svc := make([]store.ServiceDomain, 0, 2)
	if len(existing) > 0 {
		svc = append(svc, existing[0])
	}

	if on {
		// The proxy reads a mail app's first service domain as Stalwart's
		// console (HTTPS); without one, Rail's would be taken for it.
		if len(existing) == 0 {
			return "", errors.New("this mail app has no admin console hostname, so Rail can't get one of its own")
		}
		host = normalizeHost(host)
		if host == "" {
			host = "rail." + app.Domain
		}
		if err := validHost(host); err != nil {
			return "", err
		}
		if owner := s.store.DomainOwner(host); owner != 0 && owner != app.ID {
			return "", fmt.Errorf("domain %q is already in use by another app — free it first", host)
		}
		if slices.Contains(own, host) {
			return "", fmt.Errorf("%q is this app's own domain — give Rail a hostname of its own", host)
		}
		if host == existing[0].Host {
			return "", fmt.Errorf("%q is the mail admin console's hostname — give Rail a hostname of its own", host)
		}
		port := curPort
		if port == 0 {
			avoid := []int{app.Port}
			for _, d := range existing {
				avoid = append(avoid, d.Port)
			}
			ports, err := s.allocPorts(1, avoid...)
			if err != nil {
				return "", err
			}
			port = ports[0]
		}
		if err := ensureRailData(app.ComposePath); err != nil {
			return "", err
		}
		before := c.yaml
		if err := c.putRail(port, !snappy); err != nil {
			return "", err
		}
		if err := writeComposeEdit(app.ComposePath, c.yaml); err != nil {
			return "", err
		}
		if err := s.upWebmail(app, railService); err != nil {
			if !hasRail {
				s.removeWebmail(app, railService)
			}
			if werr := writeComposeEdit(app.ComposePath, before); werr != nil {
				log.Printf("app %d: restore compose file after Rail failed to start: %v", app.ID, werr)
			}
			return "", err
		}
		svc = append(svc, store.ServiceDomain{Host: host, Port: port})
	} else {
		if !hasRail && len(rows) == 0 {
			return "", nil // already off
		}
		if hasRail {
			if !snappy {
				return "", fmt.Errorf("SnappyMail is off — turn it on before Rail, or %s serves no webmail", app.Domain)
			}
			// The container goes before the file stops naming it.
			if err := s.removeWebmail(app, railService); err != nil {
				return "", err
			}
			c.yaml, _ = composeRemoveService(c.yaml, railService)
			if err := writeComposeEdit(app.ComposePath, c.yaml); err != nil {
				return "", err
			}
		}
	}

	sslMode, isLocal := "internal", true
	if c.proj.Environment == "prod" {
		sslMode, isLocal = "letsencrypt", false
	}
	if err := s.store.ReplaceAppDomains(app.ID, own, svc, isLocal, sslMode); err != nil {
		return "", err
	}
	if !on {
		return "", nil
	}
	return host, nil
}

// SetSnappyMail switches SnappyMail on or off. Off needs Rail on: Rail then
// takes over the app's own domain. Its data (the webmail_data volume) stays.
// On while already on starts it again, so a failed start can be retried.
func (s *Service) SetSnappyMail(id int64, on bool) error {
	c, err := s.loadWebmail(id)
	if err != nil {
		return err
	}
	app := c.app
	snappy := hasComposeService(c.yaml, snappyService)
	if snappy == on {
		if on {
			return s.upWebmail(app, snappyService)
		}
		return nil
	}
	railPort := composeRailPort(c.yaml)
	before := c.yaml

	if !on {
		if !hasComposeService(c.yaml, railService) || railPort == 0 {
			return fmt.Errorf("turn Rail on first — with neither, %s serves no webmail", app.Domain)
		}
		if err := s.removeWebmail(app, snappyService); err != nil {
			return err
		}
		c.yaml, _ = composeRemoveService(c.yaml, snappyService)
		if err := c.putRail(railPort, true); err != nil {
			return err
		}
		if err := writeComposeEdit(app.ComposePath, c.yaml); err != nil {
			return err
		}
		// Rail is recreated with the app's port, now free. If it can't take
		// it, SnappyMail goes back rather than leaving the domain with nothing.
		if err := s.upWebmail(app, railService); err != nil {
			if werr := writeComposeEdit(app.ComposePath, before); werr == nil {
				s.upWebmail(app, railService)
				s.upWebmail(app, snappyService)
			}
			return err
		}
		return nil
	}

	block, err := templates.RenderPartial(app.Type, "webmail_service", c.data())
	if err != nil {
		return err
	}
	if railPort != 0 {
		if err := c.putRail(railPort, false); err != nil {
			return err
		}
	}
	if c.yaml, err = composeAddService(c.yaml, block); err != nil {
		return err
	}
	if err := writeComposeEdit(app.ComposePath, c.yaml); err != nil {
		return err
	}
	// Rail first: it gives the app's port back before SnappyMail takes it.
	if railPort != 0 {
		if err := s.upWebmail(app, railService); err != nil {
			return err
		}
	}
	return s.upWebmail(app, snappyService)
}

// upWebmail (re)creates one webmail service of a running app; a stopped app
// picks the change up on its next start. Neither webmail block depends_on
// `mail`, so even a podman-compose that ignores --no-deps leaves Stalwart be.
func (s *Service) upWebmail(app store.App, service string, extra ...string) error {
	if app.Status != store.AppRunning {
		return nil
	}
	args := append([]string{"up", "-d", "--no-deps"}, extra...)
	if out, err := s.composeService(app, append(args, service)...); err != nil {
		return fmt.Errorf("start %s: %v\n%s", service, err, out)
	}
	return nil
}

// removeWebmail stops and deletes one webmail container by name through the
// engine: podman-compose has no `rm`. A container that isn't there is fine;
// any other failure of a running app is returned so the caller stops before
// editing the file.
func (s *Service) removeWebmail(app store.App, service string) error {
	_, engine, _, pname, _, err := s.composeCtx(app.ID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := runtime.Exec(ctx, engine, "rm", "-f", pname+"_"+service)
	if err == nil || noSuchContainer(out) {
		return nil
	}
	// A running one would keep its port; a stopped app's leftover can wait.
	if app.Status == store.AppRunning {
		return fmt.Errorf("remove the %s container: %v", service, err)
	}
	log.Printf("app %d: remove %s container: %v", app.ID, service, err)
	return nil
}

func noSuchContainer(out string) bool {
	o := strings.ToLower(out)
	return strings.Contains(o, "no such container") || strings.Contains(o, "no container with name")
}

// UpdateRail pulls the newest Rail image and, when the running container is on
// an older one, recreates only the rail container from it (Stalwart and
// SnappyMail are left running). It reports whether it recreated Rail; a
// stopped app gets the new image on its next start.
func (s *Service) UpdateRail(id int64) (bool, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return false, err
	}
	yaml, err := s.ReadCompose(app.ID)
	if err != nil {
		return false, err
	}
	if !hasComposeService(yaml, railService) {
		return false, errors.New("Rail is not on for this app")
	}
	if out, err := s.composeService(app, "pull", railService); err != nil {
		return false, fmt.Errorf("pull rail: %v\n%s", err, out)
	}
	if app.Status != store.AppRunning {
		return false, nil
	}
	_, engine, _, pname, _, err := s.composeCtx(app.ID)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	running, _ := runtime.Exec(ctx, engine, "inspect", "--format", "{{.Image}}", pname+"_"+railService)
	latest, _ := runtime.Exec(ctx, engine, "image", "inspect", "--format", "{{.Id}}", composeServiceImage(yaml, railService))
	if id := imageID(latest); id != "" && id == imageID(running) {
		return false, nil // already on the newest image
	}
	if err := ensureRailData(app.ComposePath); err != nil {
		return false, err
	}
	// podman-compose recreates on a config change only, never a new image.
	if err := s.upWebmail(app, railService, "--force-recreate"); err != nil {
		return false, err
	}
	return true, nil
}

func imageID(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "sha256:")
}

// composeServiceImage reads a service's image: line, defaulting to Rail's.
func composeServiceImage(yaml, name string) string {
	lines := strings.Split(yaml, "\n")
	if start, indent, ok := findComposeService(lines, name); ok {
		for _, l := range lines[start+1:] {
			t := strings.TrimSpace(l)
			if t != "" && !strings.HasPrefix(t, "#") && lineIndent(l) <= indent {
				break
			}
			if v, ok := strings.CutPrefix(t, "image:"); ok {
				return strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	return "docker.io/leyume/rail:latest"
}

// composeService runs one compose command against an app's stack.
func (s *Service) composeService(app store.App, args ...string) (string, error) {
	_, engine, workdir, pname, file, err := s.composeCtx(app.ID)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return runtime.Compose(ctx, engine, workdir, pname, file, args...)
}

// SetOwnHosts replaces the hostnames a mail app answers on itself — the ones
// SnappyMail (or, with it off, Rail) serves — from a comma-separated field,
// leaving its service domains (admin console, Rail) as they are. The first
// becomes the app's domain.
func (s *Service) SetOwnHosts(id int64, field string) ([]string, error) {
	c, err := s.loadWebmail(id)
	if err != nil {
		return nil, err
	}
	app := c.app
	hosts, err := parseHosts(field)
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, errors.New("give at least one hostname")
	}
	svc, err := s.store.AppServiceDomains(app.ID)
	if err != nil {
		return nil, err
	}
	for _, h := range hosts {
		if owner := s.store.DomainOwner(h); owner != 0 && owner != app.ID {
			return nil, fmt.Errorf("domain %q is already in use by another app — free it first", h)
		}
		for _, d := range svc {
			if d.Host == h {
				return nil, fmt.Errorf("%q already routes to another of this app's services", h)
			}
		}
	}
	sslMode, isLocal := "internal", true
	if c.proj.Environment == "prod" {
		sslMode, isLocal = "letsencrypt", false
	}
	if err := s.store.ReplaceAppDomains(app.ID, hosts, svc, isLocal, sslMode); err != nil {
		return nil, err
	}
	return hosts, s.store.SetAppDomain(app.ID, hosts[0])
}
