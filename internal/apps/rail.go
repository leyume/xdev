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
	"regexp"
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

// railPortRE matches a published port line of the rail block: - "20031:8080".
var railPortRE = regexp.MustCompile(`^\s*-\s*"?(\d+):8080"?\s*$`)

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

// Webmail is what the Services tab shows about a mail app's webmail.
type Webmail struct {
	Snappy   bool   // SnappyMail is on (serving the app's own domain)
	RailHost string // Rail's own hostname ("" = off, or a rail service with no route)
	RailPort int    // Rail's own host port (0 = off)
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
	if w.RailPort == 0 {
		return w
	}
	svc, _ := s.store.AppServiceDomains(app.ID)
	for _, d := range svc {
		if d.Port == w.RailPort {
			w.RailHost = d.Host
		}
	}
	return w
}

// RailHost returns Rail's hostname and host port, or "" and 0 when Rail is off.
func (s *Service) RailHost(app store.App) (string, int) {
	w := s.WebmailState(app)
	return w.RailHost, w.RailPort
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
	}
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

// SetRail adds Rail at host (default rail.<app domain>) or removes it. It
// returns the hostname it published ("" when turning Rail off). Turning it on
// while it is on moves it to host, keeping its port and data, and brings its
// block up to date with the current template.
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
	curPort := composeRailPort(c.yaml)

	// Every service domain but Rail's, in order (the admin console stays first).
	svc := make([]store.ServiceDomain, 0, len(existing)+1)
	for _, d := range existing {
		if curPort == 0 || d.Port != curPort {
			svc = append(svc, d)
		}
	}

	if on {
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
		for _, h := range own {
			if h == host {
				return "", fmt.Errorf("%q is this app's own domain — give Rail a hostname of its own", host)
			}
		}
		for _, d := range svc {
			if d.Host == host {
				return "", fmt.Errorf("%q already routes to another of this app's services", host)
			}
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
		if err := c.putRail(port, !snappy); err != nil {
			return "", err
		}
		if err := writeComposeEdit(app.ComposePath, c.yaml); err != nil {
			return "", err
		}
		svc = append(svc, store.ServiceDomain{Host: host, Port: port})
	} else if curPort != 0 {
		if !snappy {
			return "", fmt.Errorf("SnappyMail is off — turn it on before Rail, or %s serves no webmail", app.Domain)
		}
		// The container goes before the file stops naming it (see adminer.go).
		s.composeService(app, "rm", "--stop", "--force", railService)
		c.yaml, _ = composeRemoveService(c.yaml, railService)
		if err := writeComposeEdit(app.ComposePath, c.yaml); err != nil {
			return "", err
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
	if err := s.upWebmail(app, railService); err != nil {
		return host, err
	}
	return host, nil
}

// SetSnappyMail switches SnappyMail on or off. Off needs Rail on: Rail then
// takes over the app's own domain. Its data (the webmail_data volume) stays.
func (s *Service) SetSnappyMail(id int64, on bool) error {
	c, err := s.loadWebmail(id)
	if err != nil {
		return err
	}
	app := c.app
	snappy := hasComposeService(c.yaml, snappyService)
	if snappy == on {
		return nil
	}
	railPort := composeRailPort(c.yaml)

	if !on {
		if railPort == 0 {
			return fmt.Errorf("turn Rail on first — with neither, %s serves no webmail", app.Domain)
		}
		s.composeService(app, "rm", "--stop", "--force", snappyService)
		c.yaml, _ = composeRemoveService(c.yaml, snappyService)
		if err := c.putRail(railPort, true); err != nil {
			return err
		}
		if err := writeComposeEdit(app.ComposePath, c.yaml); err != nil {
			return err
		}
		// Rail is recreated with the app's port, now free.
		return s.upWebmail(app, railService)
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
// picks the change up on its next start.
func (s *Service) upWebmail(app store.App, service string) error {
	if app.Status != store.AppRunning {
		return nil
	}
	if out, err := s.composeService(app, "up", "-d", "--no-deps", service); err != nil {
		return fmt.Errorf("start %s: %v\n%s", service, err, out)
	}
	return nil
}

// UpdateRail pulls the newest Rail image and recreates only the rail
// container from it (Stalwart and SnappyMail are left running). It reports
// whether the container was recreated, i.e. a newer image was found.
func (s *Service) UpdateRail(id int64) (bool, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return false, err
	}
	if _, port := s.RailHost(app); port == 0 {
		return false, errors.New("Rail is not on for this app")
	}
	if out, err := s.composeService(app, "pull", railService); err != nil {
		return false, fmt.Errorf("pull rail: %v\n%s", err, out)
	}
	if app.Status != store.AppRunning {
		return false, nil // the new image is used on the next start
	}
	out, err := s.composeService(app, "up", "-d", "--no-deps", railService)
	if err != nil {
		return false, fmt.Errorf("start rail: %v\n%s", err, out)
	}
	return strings.Contains(out, "Recreate") || strings.Contains(out, "Started"), nil
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
