package apps

// Bring-your-own-compose apps (store.TypeCompose). Unlike the templated types,
// the compose file is written by the user: they paste or upload a
// docker-compose.yml / compose.yml and xdev runs it as-is. xdev's only stake in
// the file is the host ports it routes domains to, so the pieces here are
// (a) finding every published port in the file (with the service that owns it,
// so the UI can offer a hostname per port), and (b) keeping the _/.env next to
// it stocked with the variables the file may reference.
//
// One port is the app's primary — it gets the app's own domain, and may be
// published as ${PORT} to take the number xdev allocates. Any other published
// port can be given its own hostname on the add-app form; those become
// secondary domain rows, routed straight to that port like Adminer's.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"xdev/internal/store"
	"xdev/internal/templates"
)

// maxComposeBytes caps a supplied compose file. Compose files are a few KB;
// anything past this is a paste accident, not a stack.
const maxComposeBytes = 256 << 10

// composeEnvHeader tops the generated _/.env so the file explains itself — it is
// user-editable (the Env tab points here for compose apps), and the managed keys
// below it are rewritten on every start.
const composeEnvHeader = `# Environment for this app's compose stack (read by ` + "`compose`" + ` for ${VAR}
# substitution and by any service listing this file under env_file).
# PORT/XDEV_PORT are managed by xdev — the host port your domain routes to —
# and are rewritten on every start. Add your own variables below.`

var (
	// A compose file must declare services at the top level. This is the cheap
	// structural check xdev makes (there is no YAML parser in the build); the
	// engine does the real validation on `compose up`.
	composeServicesRe = regexp.MustCompile(`(?m)^services:[ \t]*(#.*)?$`)

	// A published port whose host side is the variable xdev sets: - "${PORT}:80",
	// - $PORT:8000, - "${PORT:-8080}:80", - "127.0.0.1:${XDEV_PORT}:80",
	// published: ${PORT}. Matched only in port position, so a ${PORT} mentioned
	// in an environment entry or a comment doesn't count as publishing one.
	composeShortPortVarRe = regexp.MustCompile(`^-[ \t]*["']?(?:[0-9.]+:)?\$\{?(?:XDEV_)?PORT\b`)
	composeLongPortVarRe  = regexp.MustCompile(`^published:[ \t]*["']?\$\{?(?:XDEV_)?PORT\b`)

	// Short-syntax published port: - "8080:80", - 8080:80/tcp,
	// - 127.0.0.1:8080:80. The container port is required, which keeps this from
	// matching a bare list item that happens to hold a number.
	composeShortPortRe = regexp.MustCompile(`^-[ \t]*["']?(?:[0-9.]+:)?(\d{2,5}):\d{1,5}(?:/(?:tcp|udp))?["']?$`)

	// Long-syntax published port: published: 8080 / published: "8080".
	composeLongPortRe = regexp.MustCompile(`^published:[ \t]*["']?(\d{2,5})["']?$`)

	// A service key: the name at the top indent level inside services:.
	composeServiceKeyRe = regexp.MustCompile(`^([A-Za-z0-9._-]+):$`)
)

// ComposePort is one host port a compose file publishes, and the service that
// publishes it. Var marks the port written as ${PORT}/${XDEV_PORT}, whose number
// xdev allocates (so Port is 0 until it does).
type ComposePort struct {
	Port    int
	Service string
	Var     bool
}

// Label names the port the way the add-app form shows it.
func (p ComposePort) Label() string {
	if p.Var {
		return "${PORT}"
	}
	return strconv.Itoa(p.Port)
}

// composeRoute pairs one of an app's published ports with the hostname that
// routes to it. The primary port uses the app's own domain and is not in here;
// these are the extras the user named on the form.
type composeRoute struct {
	Host string
	Port int
}

// layoutCompose builds a bring-your-own-compose app: it validates the supplied
// file (or renders the starter one when none was given), decides which host port
// the app's domain routes to, resolves a hostname for each *other* published
// port the user named, and writes the _/compose.yml + _/.env + app/ layout.
// Resource limits are not applied — the file is the user's, so limits belong in
// its own deploy blocks.
//
// The extra routes come back for the caller to attach once the app row exists;
// their hostnames are validated here so a collision fails before anything is
// written.
func (s *Service) layoutCompose(app *store.App, opts *CreateOpts, proj store.Project, appDir string) ([]composeRoute, error) {
	yaml := strings.TrimSpace(opts.ComposeFile)
	starter := yaml == ""

	// Port: a supplied file decides (either it publishes ${PORT} and takes the
	// one xdev allocates, or it hard-codes host ports and xdev routes to the
	// primary); the starter template always gets an allocated one.
	var port int
	var extras []composeRoute
	var err error
	if starter {
		if port, err = s.allocPort(); err != nil {
			return nil, err
		}
	} else {
		if err := validCompose(yaml); err != nil {
			return nil, err
		}
		ports, err := ComposePorts(yaml)
		if err != nil {
			return nil, err
		}
		primary, _ := pickPrimary(ports, opts.PrimaryPort)
		if primary.Var {
			if port, err = s.allocPort(); err != nil {
				return nil, err
			}
		} else {
			if err := s.portTaken(primary.Port); err != nil {
				return nil, err
			}
			port = primary.Port
		}
		if extras, err = s.composeExtraRoutes(ports, primary, opts, app.Domain); err != nil {
			return nil, err
		}
	}

	underscore := filepath.Join(appDir, "_")
	content := filepath.Join(appDir, "app")
	for _, d := range []string{underscore, content} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			os.RemoveAll(appDir)
			return nil, err
		}
	}

	if starter {
		// No CPU/memory limits: this file is the user's from here on, so limits
		// belong in its own deploy blocks rather than in the app row.
		yaml, err = templates.RenderCompose(store.TypeCompose, templates.Data{
			ProjectSlug: proj.Slug,
			NetworkName: proj.NetworkName,
			AppSlug:     app.Slug,
			AppType:     opts.Type,
			Env:         proj.Environment,
			HostPort:    port,
		})
		if err != nil {
			os.RemoveAll(appDir)
			return nil, err
		}
		// Give the starter's nginx service something to serve. A supplied file
		// gets no scaffold — its content dir is whatever that file mounts.
		if err := s.writeScaffold(store.TypeCompose, content); err != nil {
			os.RemoveAll(appDir)
			return nil, err
		}
	}

	composePath := filepath.Join(underscore, "compose.yml")
	if err := os.WriteFile(composePath, []byte(ensureTrailingNewline(yaml)), 0o644); err != nil {
		os.RemoveAll(appDir)
		return nil, err
	}
	if err := ensureComposeEnv(underscore, port); err != nil {
		os.RemoveAll(appDir)
		return nil, err
	}
	app.Port = port
	app.ComposePath = composePath
	return extras, nil
}

// composeExtraRoutes turns the port -> hostname map the form submitted into
// validated routes for the published ports that are *not* the primary. A port
// with no hostname is simply left unrouted (still published, reachable on the
// host); a named one must resolve to a free hostname and a port no other app
// owns, or the whole create fails before writing anything.
func (s *Service) composeExtraRoutes(ports []ComposePort, primary ComposePort, opts *CreateOpts, appDomain string) ([]composeRoute, error) {
	var out []composeRoute
	for _, p := range ports {
		if p.Var || p.Port == primary.Port {
			continue // the primary carries the app's own domain
		}
		host := normalizeHost(opts.PortDomains[p.Port])
		if host == "" {
			continue // published but not routed — the user left it blank
		}
		if err := validHost(host); err != nil {
			return nil, fmt.Errorf("port %d: %w", p.Port, err)
		}
		if host == appDomain {
			return nil, fmt.Errorf("port %d cannot use %q — that is the app's own domain; mark this port primary instead", p.Port, host)
		}
		if owner := s.store.DomainOwner(host); owner != 0 {
			return nil, fmt.Errorf("domain %q is already in use", host)
		}
		for _, r := range out {
			if r.Host == host {
				return nil, fmt.Errorf("domain %q is used twice — give each port its own hostname", host)
			}
		}
		if err := s.portTaken(p.Port); err != nil {
			return nil, err
		}
		out = append(out, composeRoute{Host: host, Port: p.Port})
	}
	return out, nil
}

// prepareCompose runs before a compose app starts: it re-reads the file (the
// user may have edited it since the last start), repoints the app at the host
// port it now publishes, and refreshes the managed variables in _/.env. The
// caller reconciles afterwards, so a port change lands in Caddy on the same
// start. It returns the app with an up-to-date port.
//
// Secondary domains (the extra ports named at create time) are left alone: they
// are explicit user configuration, not something to re-derive from the file.
func (s *Service) prepareCompose(app store.App) (store.App, error) {
	raw, err := os.ReadFile(app.ComposePath)
	if err != nil {
		return app, fmt.Errorf("read compose file: %w", err)
	}
	yaml := string(raw)
	if err := validCompose(yaml); err != nil {
		return app, err
	}
	ports, err := ComposePorts(yaml)
	if err != nil {
		return app, err
	}
	// Stay where the app already is whenever the file still supports it: a
	// ${PORT} file keeps the allocated port, and a multi-port file must not
	// hand the app's domain to a different service just because it is listed
	// first. Only a primary port that vanished forces a move.
	if !hasVarPort(ports) && !publishesPort(ports, app.Port) {
		moved := ports[0].Port
		if err := s.portTaken(moved); err != nil {
			return app, err
		}
		if err := s.store.SetAppPort(app.ID, moved); err != nil {
			return app, err
		}
		app.Port = moved
	}
	if app.Port == 0 { // an older row, or one whose port was cleared
		if app.Port, err = s.allocPort(); err != nil {
			return app, err
		}
		if err := s.store.SetAppPort(app.ID, app.Port); err != nil {
			return app, err
		}
	}
	return app, ensureComposeEnv(filepath.Dir(app.ComposePath), app.Port)
}

// portTaken reports an error when another app (or service domain) already owns
// the host port a compose file asks for — two stacks publishing the same port
// means the second one fails to start and the domain routes to the wrong app.
func (s *Service) portTaken(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid host port %d in compose file", port)
	}
	used, err := s.store.UsedPorts()
	if err != nil {
		return err
	}
	for _, p := range used {
		if p == port {
			return fmt.Errorf("host port %d is already used by another app — publish a different port", port)
		}
	}
	return nil
}

// validCompose does the structural checks xdev can make without a YAML parser:
// the file must be sane text, of a plausible size, and declare top-level
// services. Anything subtler is the engine's job to report on `compose up`.
func validCompose(yaml string) error {
	if strings.TrimSpace(yaml) == "" {
		return errors.New("compose file is empty")
	}
	if len(yaml) > maxComposeBytes {
		return fmt.Errorf("compose file is too large (max %d KB)", maxComposeBytes>>10)
	}
	if !utf8.ValidString(yaml) {
		return errors.New("compose file is not valid UTF-8 text")
	}
	if !composeServicesRe.MatchString(yaml) {
		return errors.New(`compose file has no top-level "services:" block — paste a docker-compose.yml / compose.yml`)
	}
	for i, line := range strings.Split(yaml, "\n") {
		if strings.HasPrefix(line, "\t") {
			return fmt.Errorf("line %d is indented with a tab — YAML needs spaces", i+1)
		}
	}
	return nil
}

// ComposePorts lists every host port a compose file publishes, in file order,
// each tagged with the service that publishes it — the add-app form offers a
// hostname per port from this. Ports published more than once collapse to their
// first appearance, and a file that publishes nothing is an error: its domain
// would have nowhere to go.
//
// The scan is deliberately shallow (there is no YAML parser in the build): it
// tracks the current service by indentation inside the top-level services:
// block and matches port entries in both compose syntaxes.
func ComposePorts(yaml string) ([]ComposePort, error) {
	sc := bufio.NewScanner(strings.NewReader(yaml))
	sc.Buffer(make([]byte, 0, 64<<10), maxComposeBytes)

	var out []ComposePort
	seen := map[int]bool{}
	service, inServices, serviceIndent := "", false, -1
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, " #"); i >= 0 { // trailing comment
			line = strings.TrimSpace(line[:i])
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		if indent == 0 {
			// A new top-level key: only services: holds the ports we care about.
			inServices, service, serviceIndent = line == "services:", "", -1
			continue
		}
		if inServices {
			if m := composeServiceKeyRe.FindStringSubmatch(line); m != nil {
				if serviceIndent < 0 {
					serviceIndent = indent // the first key inside services: sets the level
				}
				if indent == serviceIndent {
					service = m[1]
					continue
				}
			}
		}
		if composeShortPortVarRe.MatchString(line) || composeLongPortVarRe.MatchString(line) {
			if hasVarPort(out) {
				return nil, errors.New("compose file uses ${PORT} more than once — xdev allocates a single port, so hard-code the other published ports")
			}
			out = append(out, ComposePort{Service: service, Var: true})
			continue
		}
		for _, re := range []*regexp.Regexp{composeShortPortRe, composeLongPortRe} {
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if p, err := strconv.Atoi(m[1]); err == nil && p > 0 && p <= 65535 && !seen[p] {
				seen[p] = true
				out = append(out, ComposePort{Port: p, Service: service})
			}
			break
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New(`compose file publishes no host port — give the web service ports: - "${PORT}:80" so xdev can route your domain to it`)
	}
	return out, nil
}

// publishesPort reports whether the file still publishes a given host port.
func publishesPort(ports []ComposePort, port int) bool {
	for _, p := range ports {
		if !p.Var && p.Port == port {
			return true
		}
	}
	return false
}

// hasVarPort reports whether a ${PORT} entry was already found.
func hasVarPort(ports []ComposePort) bool {
	for _, p := range ports {
		if p.Var {
			return true
		}
	}
	return false
}

// pickPrimary returns the published port that carries the app's own domain. A
// ${PORT} entry always wins: its number is the one xdev allocates and writes
// into _/.env, so no other port can hold the app's domain without the two
// fighting over that number. Otherwise the caller's choice is honoured when the
// file still publishes it, with the first port as the fallback. The bool reports
// whether an explicit request was honoured.
func pickPrimary(ports []ComposePort, want int) (ComposePort, bool) {
	for _, p := range ports {
		if p.Var {
			return p, want <= 0
		}
	}
	if want > 0 {
		for _, p := range ports {
			if p.Port == want {
				return p, true
			}
		}
		return ports[0], false
	}
	return ports[0], true
}

// ensureComposeEnv writes the variables xdev manages into the stack's _/.env —
// the file compose reads for ${VAR} substitution — keeping every other line the
// user put there. Called on create and again on every start, so an edit that
// drops PORT can't leave the domain pointing at a dead port.
func ensureComposeEnv(underscore string, port int) error {
	managed := [][2]string{
		{"PORT", strconv.Itoa(port)},
		{"XDEV_PORT", strconv.Itoa(port)},
	}
	path := filepath.Join(underscore, ".env")
	var kept []string
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if isManagedEnvLine(line, managed) {
				continue // rewritten below
			}
			kept = append(kept, line)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		kept = strings.Split(composeEnvHeader, "\n")
	}

	var b strings.Builder
	for _, kv := range managed {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
	}
	for _, line := range kept {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// isManagedEnvLine reports whether an .env line assigns one of the keys xdev
// owns (so it gets replaced rather than duplicated).
func isManagedEnvLine(line string, managed [][2]string) bool {
	key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
	if !ok {
		return false
	}
	for _, kv := range managed {
		if strings.TrimSpace(key) == kv[0] {
			return true
		}
	}
	return false
}

// ensureTrailingNewline keeps written files POSIX-tidy (pasted textareas often
// arrive without one).
func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
