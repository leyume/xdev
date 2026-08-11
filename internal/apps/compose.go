package apps

// Bring-your-own-compose apps (store.TypeCompose). Unlike the templated types,
// the compose file is written by the user: they paste or upload a
// docker-compose.yml / compose.yml and xdev runs it as-is. xdev's only stake in
// the file is the host port it routes the app's domain to, so the two pieces
// here are (a) finding that port in the file and (b) keeping the _/.env next to
// it stocked with the variables the file may reference.

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
)

// layoutCompose builds a bring-your-own-compose app: it validates the supplied
// file (or renders the starter one when none was given), decides which host port
// the app's domain routes to, and writes the _/compose.yml + _/.env + app/
// layout. Resource limits are not applied — the file is the user's, so limits
// belong in its own deploy blocks.
func (s *Service) layoutCompose(app *store.App, opts *CreateOpts, proj store.Project, appDir string) error {
	yaml := strings.TrimSpace(opts.ComposeFile)
	starter := yaml == ""

	// Port: a supplied file decides (either it references ${PORT} and takes the
	// one xdev allocates, or it hard-codes a host port xdev routes to instead);
	// the starter template always gets an allocated one.
	var port int
	var err error
	if starter {
		if port, err = s.allocPort(); err != nil {
			return err
		}
	} else {
		if err := validCompose(yaml); err != nil {
			return err
		}
		fixed, usesVar, err := composeHostPort(yaml)
		if err != nil {
			return err
		}
		if usesVar {
			if port, err = s.allocPort(); err != nil {
				return err
			}
		} else {
			if err := s.portTaken(fixed); err != nil {
				return err
			}
			port = fixed
		}
	}

	underscore := filepath.Join(appDir, "_")
	content := filepath.Join(appDir, "app")
	for _, d := range []string{underscore, content} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			os.RemoveAll(appDir)
			return err
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
			return err
		}
		// Give the starter's nginx service something to serve. A supplied file
		// gets no scaffold — its content dir is whatever that file mounts.
		if err := s.writeScaffold(store.TypeCompose, content); err != nil {
			os.RemoveAll(appDir)
			return err
		}
	}

	composePath := filepath.Join(underscore, "compose.yml")
	if err := os.WriteFile(composePath, []byte(ensureTrailingNewline(yaml)), 0o644); err != nil {
		os.RemoveAll(appDir)
		return err
	}
	if err := ensureComposeEnv(underscore, port); err != nil {
		os.RemoveAll(appDir)
		return err
	}
	app.Port = port
	app.ComposePath = composePath
	return nil
}

// prepareCompose runs before a compose app starts: it re-reads the file (the
// user may have edited it since the last start), repoints the app at the host
// port it now publishes, and refreshes the managed variables in _/.env. The
// caller reconciles afterwards, so a port change lands in Caddy on the same
// start. It returns the app with an up-to-date port.
func (s *Service) prepareCompose(app store.App) (store.App, error) {
	raw, err := os.ReadFile(app.ComposePath)
	if err != nil {
		return app, fmt.Errorf("read compose file: %w", err)
	}
	yaml := string(raw)
	if err := validCompose(yaml); err != nil {
		return app, err
	}
	fixed, usesVar, err := composeHostPort(yaml)
	if err != nil {
		return app, err
	}
	// A ${PORT} file keeps the port xdev already allocated; a hard-coded one
	// wins whenever it changed under us.
	if !usesVar && fixed != app.Port {
		if err := s.portTaken(fixed); err != nil {
			return app, err
		}
		if err := s.store.SetAppPort(app.ID, fixed); err != nil {
			return app, err
		}
		app.Port = fixed
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

// composeHostPort finds the host port a compose file publishes for xdev to route
// the app's domain at. A ${PORT} reference means "use the port xdev allocates"
// (usesVar); otherwise the first hard-coded published port wins. A file that
// publishes nothing is an error: its domain would have nowhere to go.
func composeHostPort(yaml string) (port int, usesVar bool, err error) {
	sc := bufio.NewScanner(strings.NewReader(yaml))
	sc.Buffer(make([]byte, 0, 64<<10), maxComposeBytes)
	fixed := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, " #"); i >= 0 { // trailing comment
			line = strings.TrimSpace(line[:i])
		}
		// ${PORT} anywhere in port position wins outright: the user asked for
		// the port xdev hands out, wherever else the file pins ports.
		if composeShortPortVarRe.MatchString(line) || composeLongPortVarRe.MatchString(line) {
			return 0, true, nil
		}
		if fixed > 0 {
			continue // keep scanning only to see whether a ${PORT} shows up later
		}
		for _, re := range []*regexp.Regexp{composeShortPortRe, composeLongPortRe} {
			if m := re.FindStringSubmatch(line); m != nil {
				if p, err := strconv.Atoi(m[1]); err == nil && p > 0 && p <= 65535 {
					fixed = p
				}
				break
			}
		}
	}
	if err := sc.Err(); err != nil {
		return 0, false, err
	}
	if fixed > 0 {
		return fixed, false, nil
	}
	return 0, false, errors.New(`compose file publishes no host port — give the web service ports: - "${PORT}:80" so xdev can route your domain to it`)
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
