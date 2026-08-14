package apps

// Bring-your-own-compose apps (store.TypeCompose). Unlike the templated types,
// the compose file is written by the user: they paste or upload a
// docker-compose.yml / compose.yml and xdev runs it as-is.
//
// Ports are xdev's, domains are the user's. The add-app form asks how many
// domains the stack needs and takes a hostname for each; xdev allocates one free
// host port per domain and writes them into the _/.env beside the file as
// PORT/PORT_1, PORT_2, … PORT_N (plus XDEV_ aliases). The file publishes those
// variables — "${PORT}:80", "${PORT_2}:8000" — so nothing is ever hard-coded and
// two stacks cannot collide on a number.
//
// Slot 1 carries the app's own domain; slots 2..N become secondary domain rows
// routed straight to their port, the same rows Adminer uses. The mapping is
// positional and fixed at create time: domain 2 is always ${PORT_2}.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"xdev/internal/store"
	"xdev/internal/templates"
)

// maxComposeBytes caps a supplied compose file. Compose files are a few KB;
// anything past this is a paste accident, not a stack.
const maxComposeBytes = 256 << 10

// MaxComposeSlots caps how many domains (and so how many allocated host ports) a
// single compose app can ask for. Past any real stack's public surface, and it
// keeps a typo'd ${PORT_900} from being read as a request for 900 ports. The
// add-app form reads this too (viewData "MaxDomains"), so the number lives here
// and nowhere else.
const MaxComposeSlots = 5

// composeEnvHeader tops the generated _/.env so the file explains itself — it is
// user-editable (the Env tab points here for compose apps), and the managed keys
// below it are rewritten on every start.
const composeEnvHeader = `# Environment for this app's compose stack (read by ` + "`compose`" + ` for ${VAR}
# substitution and by any service listing this file under env_file).
# PORT/PORT_n (and their XDEV_ aliases) are managed by xdev — one host port per
# domain this app was given, PORT_1 being the app's own domain — and are
# rewritten on every start. Add your own variables below.`

var (
	// A compose file must declare services at the top level. This is the cheap
	// structural check xdev makes (there is no YAML parser in the build); the
	// engine does the real validation on `compose up`.
	composeServicesRe = regexp.MustCompile(`(?m)^services:[ \t]*(#.*)?$`)

	// A published port whose host side is one of the variables xdev sets:
	// - "${PORT}:80", - $PORT_2:8000, - "${PORT:-8080}:80",
	// - "127.0.0.1:${XDEV_PORT_3}:80", published: ${PORT_2}. Matched only in port
	// position, so a ${PORT} mentioned in an environment entry or a comment does
	// not count as publishing one. The capture is the slot number; empty means
	// bare ${PORT}, which is slot 1. `_2` is inside the word, so plain PORT\b
	// cannot swallow ${PORT_2}, and ${PORTAL} matches neither.
	composeShortPortVarRe = regexp.MustCompile(`^-[ \t]*["']?(?:[0-9.]+:)?\$\{?(?:XDEV_)?PORT(?:_(\d+))?\b`)
	composeLongPortVarRe  = regexp.MustCompile(`^published:[ \t]*["']?\$\{?(?:XDEV_)?PORT(?:_(\d+))?\b`)

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
// publishes it. Slot > 0 marks a port written as ${PORT_n} (bare ${PORT} being
// slot 1), whose number xdev allocates — Port stays 0 for those. A hard-coded
// port has Slot 0 and a number: xdev does not route it, it only refuses to hand
// the same number to another app.
type ComposePort struct {
	Port    int
	Slot    int
	Service string
}

// Var reports whether this entry publishes one of xdev's managed variables.
func (p ComposePort) Var() bool { return p.Slot > 0 }

// Label names the port the way the add-app form shows it.
func (p ComposePort) Label() string {
	if p.Var() {
		return portVar(p.Slot)
	}
	return strconv.Itoa(p.Port)
}

// portVar is the variable name for a slot: slot 1 is plain ${PORT} (the common
// single-domain case), the rest are ${PORT_n}. Both spellings are written to
// .env for slot 1, so a file may use either.
func portVar(slot int) string {
	if slot <= 1 {
		return "${PORT}"
	}
	return "${PORT_" + strconv.Itoa(slot) + "}"
}

// composeRoute pairs an allocated host port with the hostname routed to it.
// Slot 1 uses the app's own domain and is not in here; these are the extras.
type composeRoute struct {
	Host string
	Port int
}

// layoutCompose builds a bring-your-own-compose app: it validates the supplied
// file (or renders the starter one when none was given), allocates one host port
// per domain the user asked for, checks the file actually publishes each one,
// and writes the _/compose.yml + _/.env + app/ layout. Resource limits are not
// applied — the file is the user's, so limits belong in its own deploy blocks.
//
// The extra routes (domains 2..N, paired with their allocated port) come back
// for the caller to attach once the app row exists; their hostnames are
// validated here so a collision fails before anything is written. appHosts is
// every hostname domain 1 covers — one slot however many names it lists.
func (s *Service) layoutCompose(app *store.App, opts *CreateOpts, proj store.Project, appDir string, appHosts []string) ([]composeRoute, error) {
	yaml := strings.TrimSpace(normalizeYAML(opts.ComposeFile))
	starter := yaml == ""

	// One slot per domain: slot 1 is the app's own domain, the rest are the
	// extra hostnames from the form, in the order they were entered.
	hosts, err := s.composeExtraHosts(opts.ExtraDomains, appHosts, 0)
	if err != nil {
		return nil, err
	}
	slots := len(hosts) + 1

	// avoid: host ports the file hard-codes. xdev doesn't route them, but it must
	// not allocate the same number for a slot, and no other app may own it.
	var avoid []int
	if starter {
		if slots > 1 {
			return nil, errors.New("the starter compose file publishes a single service — paste or upload your own file to use more than one domain")
		}
	} else {
		if err := validCompose(yaml); err != nil {
			return nil, err
		}
		ports, err := ComposePorts(yaml)
		if err != nil {
			return nil, err
		}
		if err := checkComposeSlots(ports, slots); err != nil {
			return nil, err
		}
		for _, p := range ports {
			if p.Var() {
				continue
			}
			if err := s.portTaken(p.Port); err != nil {
				return nil, err
			}
			avoid = append(avoid, p.Port)
		}
	}

	ports, err := s.allocPorts(slots, avoid...)
	if err != nil {
		return nil, err
	}
	port := ports[0]
	extras := make([]composeRoute, 0, len(hosts))
	for i, h := range hosts {
		extras = append(extras, composeRoute{Host: h, Port: ports[i+1]})
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
	if err := ensureComposeEnv(underscore, ports); err != nil {
		os.RemoveAll(appDir)
		return nil, err
	}
	app.Port = port
	app.ComposePath = composePath
	return extras, nil
}

// composeExtraHosts validates the hostnames for domains 2..N, in the order the
// form submitted them — that order is the slot numbering the user sees, so a
// blank one is an error rather than something to skip: dropping it silently
// would renumber every ${PORT_n} after it. appDomains is every hostname the app
// itself answers on (domain 1 may list several); self is the app being edited
// (0 on create), whose own hostnames are not collisions.
func (s *Service) composeExtraHosts(domains []string, appDomains []string, self int64) ([]string, error) {
	if len(domains)+1 > MaxComposeSlots {
		return nil, fmt.Errorf("a compose app can have at most %d domains", MaxComposeSlots)
	}
	out := make([]string, 0, len(domains))
	for i, raw := range domains {
		slot := i + 2 // domain 1 is the app's own
		host := normalizeHost(raw)
		if host == "" {
			return nil, fmt.Errorf("domain %d is blank — give every domain a hostname, or ask for fewer", slot)
		}
		if err := validHost(host); err != nil {
			return nil, fmt.Errorf("domain %d: %w", slot, err)
		}
		if slices.Contains(appDomains, host) {
			return nil, fmt.Errorf("domain %d cannot be %q — that is this app's own domain (domain 1)", slot, host)
		}
		if owner := s.store.DomainOwner(host); owner != 0 && owner != self {
			return nil, fmt.Errorf("domain %q is already in use", host)
		}
		for _, h := range out {
			if h == host {
				return nil, fmt.Errorf("domain %q is used twice — give each one its own hostname", host)
			}
		}
		out = append(out, host)
	}
	return out, nil
}

// checkComposeSlots makes sure the file publishes exactly the ports the user
// asked for domains for: every slot 1..n must appear in port position, and a
// ${PORT_n} past the last domain is a mistake worth catching before the stack
// starts with a port nothing routes to.
func checkComposeSlots(ports []ComposePort, n int) error {
	for slot := 1; slot <= n; slot++ {
		if publishesSlot(ports, slot) {
			continue
		}
		if slot == 1 {
			return fmt.Errorf(`compose file never publishes %s — give the service that serves this app's domain ports: - "%s:<container-port>"`,
				portVar(1), portVar(1))
		}
		return fmt.Errorf(`domain %d has nothing to route to — publish its service on ports: - "%s:<container-port>"`, slot, portVar(slot))
	}
	for _, p := range ports {
		if p.Slot > n {
			return fmt.Errorf("compose file publishes %s but this app was given %d domain(s) — ask for %d",
				portVar(p.Slot), n, p.Slot)
		}
	}
	return nil
}

// prepareCompose runs before a compose app starts: it re-reads the file (the
// user may have edited it since the last start), checks every domain this app
// owns still has a port to route to, and refreshes the managed variables in
// _/.env so an edit can't leave a domain pointing at a dead port.
//
// The app's ports are xdev's own and do not move — the file references them by
// variable, so editing it changes which service is behind a domain, not which
// number. Domains are left alone too: they are explicit user configuration, not
// something to re-derive from the file.
func (s *Service) prepareCompose(app store.App) (store.App, error) {
	raw, err := os.ReadFile(app.ComposePath)
	if err != nil {
		return app, fmt.Errorf("read compose file: %w", err)
	}
	yaml := normalizeYAML(string(raw))
	if err := validCompose(yaml); err != nil {
		return app, err
	}
	ports, err := ComposePorts(yaml)
	if err != nil {
		return app, err
	}
	if app.Port == 0 { // an older row, or one whose port was cleared
		if app.Port, err = s.allocPort(); err != nil {
			return app, err
		}
		if err := s.store.SetAppPort(app.ID, app.Port); err != nil {
			return app, err
		}
	}

	// Slot order: the app's own port, then one per secondary domain in creation
	// order — the order the add-app form asked for them, so slot n is domain n.
	slots := []int{app.Port}
	if svc, err := s.store.AppServiceDomains(app.ID); err == nil {
		for _, d := range svc {
			slots = append(slots, d.Port)
		}
	}

	// A single-domain app whose file hard-codes its port instead of using
	// ${PORT} keeps the older behaviour: follow the file when the number
	// changes, so editing the port and restarting repoints Caddy.
	if len(slots) == 1 && !publishesSlot(ports, 1) && !publishesPort(ports, app.Port) {
		if moved, ok := firstFixedPort(ports); ok {
			if err := s.portTaken(moved); err != nil {
				return app, err
			}
			if err := s.store.SetAppPort(app.ID, moved); err != nil {
				return app, err
			}
			app.Port, slots[0] = moved, moved
		}
	}

	for i, p := range slots {
		// Either the file publishes the slot's variable, or it hard-codes that
		// exact number (an app created before ports were variables).
		if publishesSlot(ports, i+1) || publishesPort(ports, p) {
			continue
		}
		if i == 0 {
			return app, fmt.Errorf(`compose file no longer publishes %s — this app's domain has nothing to route to`, portVar(1))
		}
		return app, fmt.Errorf(`compose file no longer publishes %s — domain %d has nothing to route to`, portVar(i+1), i+1)
	}
	return app, ensureComposeEnv(filepath.Dir(app.ComposePath), slots)
}

// portTaken reports an error when another app (or service domain) already owns
// the host port a compose file asks for — two stacks publishing the same port
// means the second one fails to start and the domain routes to the wrong app.
// own lists the ports this app already holds: they are in the used set too, and
// re-saving a file that hard-codes one of them is not a collision with itself.
func (s *Service) portTaken(port int, own ...int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid host port %d in compose file", port)
	}
	used, err := s.store.UsedPorts()
	if err != nil {
		return err
	}
	for _, p := range own {
		if p == port {
			return nil
		}
	}
	for _, p := range used {
		if p == port {
			return fmt.Errorf("host port %d is already used by another app — publish a different port", port)
		}
	}
	return nil
}

// normalizeYAML makes a supplied compose file look the way one typed on this
// machine would. Two things arrive mangled otherwise, and both make the file
// unparseable rather than merely untidy:
//
//   - CRLF line endings. Browsers submit textarea content with them (the HTML
//     spec requires it), so *every* pasted file has them, and a Windows editor
//     writes them too. Line-anchored matching then sees "services:\r", which is
//     not a top-level services: block.
//   - A UTF-8 BOM, which editors put in front of the first key, so the line
//     reads as U+FEFF followed by "services:" and matches nothing.
//
// Applied on the way in, so what lands in _/compose.yml is plain LF text.
func normalizeYAML(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// validCompose does the structural checks xdev can make without a YAML parser:
// the file must be sane text, of a plausible size, and declare top-level
// services. Anything subtler is the engine's job to report on `compose up`.
func validCompose(yaml string) error {
	yaml = normalizeYAML(yaml)
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
// each tagged with the service that publishes it: the ${PORT_n} slots xdev fills
// in, and any hard-coded numbers (which xdev leaves alone beyond refusing to
// reuse them). Hard-coded ports published more than once collapse to their first
// appearance. A file that publishes nothing comes back empty — whether that is
// an error depends on how many domains were asked for, which checkComposeSlots
// decides.
//
// The scan is deliberately shallow (there is no YAML parser in the build): it
// tracks the current service by indentation inside the top-level services:
// block and matches port entries in both compose syntaxes.
func ComposePorts(yaml string) ([]ComposePort, error) {
	sc := bufio.NewScanner(strings.NewReader(normalizeYAML(yaml)))
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
		if m := matchPortVar(line); m != nil {
			slot, err := varSlot(m[1])
			if err != nil {
				return nil, err
			}
			if publishesSlot(out, slot) {
				return nil, fmt.Errorf("compose file publishes %s twice — each domain gets one port, so give the other service its own %s",
					portVar(slot), portVar(slot+1))
			}
			out = append(out, ComposePort{Service: service, Slot: slot})
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
	return out, nil
}

// matchPortVar returns the submatches of whichever ${PORT_n} form the line
// publishes, or nil when it publishes no managed variable.
func matchPortVar(line string) []string {
	if m := composeShortPortVarRe.FindStringSubmatch(line); m != nil {
		return m
	}
	return composeLongPortVarRe.FindStringSubmatch(line)
}

// varSlot turns the captured suffix of a ${PORT_n} into its slot number; the
// empty capture is bare ${PORT}, which is slot 1.
func varSlot(suffix string) (int, error) {
	if suffix == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 1 || n > MaxComposeSlots {
		return 0, fmt.Errorf("compose file publishes ${PORT_%s}, which is not a domain number xdev can allocate (1–%d)", suffix, MaxComposeSlots)
	}
	return n, nil
}

// publishesSlot reports whether the file publishes a given slot's variable.
func publishesSlot(ports []ComposePort, slot int) bool {
	for _, p := range ports {
		if p.Slot == slot {
			return true
		}
	}
	return false
}

// publishesPort reports whether the file hard-codes a given host port.
func publishesPort(ports []ComposePort, port int) bool {
	for _, p := range ports {
		if !p.Var() && p.Port == port {
			return true
		}
	}
	return false
}

// firstFixedPort returns the first hard-coded host port in the file.
func firstFixedPort(ports []ComposePort) (int, bool) {
	for _, p := range ports {
		if !p.Var() {
			return p.Port, true
		}
	}
	return 0, false
}

// ensureComposeEnv writes the variables xdev manages into the stack's _/.env —
// the file compose reads for ${VAR} substitution — keeping every other line the
// user put there. ports is slot order: ports[0] is the app's own domain, written
// as PORT and PORT_1 alike so a single-domain file can use the short spelling.
// Called on create and again on every start, so the numbers behind a stack's
// domains are always the ones xdev is routing.
func ensureComposeEnv(underscore string, ports []int) error {
	var managed [][2]string
	for i, p := range ports {
		v := strconv.Itoa(p)
		if i == 0 {
			managed = append(managed, [2]string{"PORT", v}, [2]string{"XDEV_PORT", v})
		}
		n := strconv.Itoa(i + 1)
		managed = append(managed, [2]string{"PORT_" + n, v}, [2]string{"XDEV_PORT_" + n, v})
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
