package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime" // stdlib; xdev/internal/runtime owns the plain name here
	"strings"
	"time"

	"xdev/internal/config"
	"xdev/internal/proxy"
	"xdev/internal/runtime"
	"xdev/internal/store"
	"xdev/internal/templates"
)

// runDoctor resolves config the same way the server does, then prints a
// readiness report. It exits non-zero (returns an error) if any *required*
// check fails, so installers can gate on it.
func runDoctor(args []string) error {
	fs, o := serverFlags()
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: xdev doctor [flags]   (same flags/env as the server)")
		printUsage(fs)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(o.dataDir, o.projectsDir, o.addr)
	if err != nil {
		return err
	}
	// Resolve the local CA out of the same dir the server uses, not the OS default.
	pinCaddyDataDir(cfg.DataDir)

	d := &doctorReport{}
	fmt.Println("xdev doctor")

	// --- engines -------------------------------------------------------------
	info := runtime.Detect()
	d.engine("podman", info.Podman)
	d.engine("docker", info.Docker)

	// Selected engine: persisted UI setting > flag/env > auto-detect. Opening
	// the store also lets us report the DB path and whether an admin exists.
	override := engineOverride(o.engine)
	st, storeErr := store.Open(cfg.DBPath)
	if storeErr == nil {
		defer st.Close()
		if v, _ := st.GetSetting("engine"); v == "podman" || v == "docker" {
			override = runtime.Engine(v)
		}
	}
	sel := runtime.NewSelector(info, override)
	if sel.Usable(sel.Current()) {
		d.ok("selected engine", string(sel.Current()))
	} else {
		d.fail("selected engine", fmt.Sprintf("%s not usable (needs the binary + compose plugin)", sel.Current()), true)
	}

	// --- caddy ---------------------------------------------------------------
	if o.caddyManage {
		// Native mode: xdev supervises the binary, so nothing is listening on the
		// admin port while doctor runs standalone — check the binary instead.
		if path, err := exec.LookPath("caddy"); err == nil {
			d.ok("caddy", caddyVersion(path))
		} else {
			d.fail("caddy", "not found on PATH (install caddy, or run with -caddy=false)", true)
		}
	} else {
		// Caddy runs outside xdev (containerized or self-managed). The only thing
		// that matters then is whether xdev can reach the admin API it pushes
		// routes to — one check covering every combination, because both engines
		// are dialed at the same XDEV_CADDY_ADMIN: podman publishes
		// 127.0.0.1:2019:2019, docker uses host networking and loopback 2019.
		// Unreachable is a required failure regardless of who owns that Caddy: no
		// admin API means xdev cannot route a single request.
		pm := proxy.NewManager(o.caddyAdmin, o.httpsPort, o.httpPort, o.acmeEmail, o.localCertTTL)
		if pm.Reachable() {
			d.ok("caddy admin", o.caddyAdmin+" reachable")
		} else {
			d.fail("caddy admin", o.caddyAdmin+" unreachable — xdev cannot push routes", true)
			d.note(fmt.Sprintf("containerized Caddy: %s compose -f <config dir>/caddy/docker-compose.yml up -d", sel.Current()))
		}
	}

	// --- local CA trust ------------------------------------------------------
	// Only meaningful for local dev: prod domains get ACME certs, never the
	// internal CA. manage-hosts is the existing local/prod signal (the installer
	// sets it true for local, false for prod).
	if o.manageHosts {
		switch root, exists := proxy.LocalCARoot(localCAName); {
		case !exists:
			d.skip("local CA", "not issued yet — Caddy mints it when the first .test/.localhost site is served")
		case proxy.LocalCATrusted(root):
			d.ok("local CA", "trusted by the system store")
		default:
			d.warn("local CA", "not trusted — browsers will warn on https://*.test")
			d.note(proxy.TrustCommand(root))
		}
	} else {
		d.skip("local CA", "prod mode serves ACME certificates")
	}

	// --- ports ---------------------------------------------------------------
	// Who binds the site ports depends on the mode, so the useful check inverts:
	// supervised Caddy is xdev's own child, so xdev must be able to bind them —
	// but a containerized or self-managed Caddy *owns* them, and asking whether
	// xdev could bind reports a healthy install as broken.
	label := fmt.Sprintf("ports %d / %d", o.httpsPort, o.httpPort)
	if o.caddyManage {
		if blocked := unbindablePorts(o.httpsPort, o.httpPort); len(blocked) == 0 {
			d.ok(label, "bindable")
		} else {
			d.fail(label, fmt.Sprintf("cannot bind %s (in use or needs privileges)", strings.Join(blocked, ", ")), true)
		}
	} else if quiet := unservedPorts(o.httpsPort, o.httpPort); len(quiet) == 0 {
		d.ok(label, "served by Caddy")
	} else {
		// Not a failure: Caddy publishes the site ports only once xdev has pushed
		// a config, so silence is expected until the first app exists.
		d.warn(label, fmt.Sprintf("nothing serving %s — expected before the first app; otherwise check Caddy",
			strings.Join(quiet, ", ")))
	}

	// --- data dir + admin ----------------------------------------------------
	if err := writableDir(cfg.DataDir); err != nil {
		d.fail("data dir", fmt.Sprintf("%s not writable: %v", cfg.DataDir, err), true)
	} else {
		d.ok("data dir", fmt.Sprintf("%s (db: %s)", cfg.DataDir, cfg.DBPath))
	}
	switch {
	case storeErr != nil:
		d.fail("admin account", fmt.Sprintf("cannot open store: %v", storeErr), true)
	default:
		if n, err := st.UserCount(); err != nil {
			d.fail("admin account", fmt.Sprintf("query failed: %v", err), true)
		} else if n == 0 {
			d.fail("admin account", "none yet  → run: xdev create-admin you@example.com", false)
		} else {
			d.ok("admin account", "configured")
		}
	}

	// --- app images ----------------------------------------------------------
	// Laravel's image is chosen per environment and per host architecture. A
	// mismatch here doesn't fail at pull time — the container starts and dies
	// with "exec format error" — so report what this machine will actually get
	// while there's still nothing to debug.
	// Ask the registry the same way app creation does, so what doctor reports and
	// what a new app actually gets can't disagree.
	archLookup := func(img string) ([]string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		arches, err := runtime.ImagePlatforms(ctx, sel.Current(), img)
		if err != nil {
			return nil, false
		}
		return arches, true
	}
	for _, env := range []string{"local", "prod"} {
		image, reason := templates.LaravelImageDetected(env, goruntime.GOARCH,
			os.Getenv("XDEV_LARAVEL_IMAGE"), archLookup)
		label := "laravel " + env // fits the aligned label column
		switch {
		case reason == "":
			d.ok(label, image)
		case strings.HasPrefix(reason, "no laravel image"):
			d.fail(label, reason, false)
		default:
			d.warn(label, reason)
		}
	}

	// --- hosts file (only relevant for local dev) ----------------------------
	if o.manageHosts {
		if err := writableFile(o.hostsFile); err != nil {
			d.warn("hosts file", fmt.Sprintf("%s not writable (%v) — fine on a server; needed for local .test domains", o.hostsFile, err))
		} else {
			d.ok("hosts file", o.hostsFile+" writable")
		}
	} else {
		d.skip("hosts file", "management disabled (-manage-hosts=false)")
	}

	if d.failed {
		return fmt.Errorf("doctor: one or more required checks failed")
	}
	return nil
}

// doctorReport prints aligned check lines and remembers whether a required
// check failed.
type doctorReport struct{ failed bool }

func (d *doctorReport) ok(label, detail string)   { d.print("✓", label, detail) }
func (d *doctorReport) warn(label, detail string) { d.print("!", label, detail) }
func (d *doctorReport) skip(label, detail string) { d.print("–", label, detail) }

// note prints an indented continuation line under the previous check — for a
// fix-it command too long to sit in the detail column.
func (d *doctorReport) note(text string) { fmt.Printf("  %-16s   %s\n", "", text) }

func (d *doctorReport) fail(label, detail string, required bool) {
	d.print("✗", label, detail)
	if required {
		d.failed = true
	}
}

func (d *doctorReport) print(sym, label, detail string) {
	fmt.Printf("  %-16s %s %s\n", label, sym, detail)
}

// engine prints a one-line summary of one engine's status.
func (d *doctorReport) engine(name string, st runtime.EngineStatus) {
	label := "engine: " + name
	if !st.Installed {
		d.print("–", label, "not installed")
		return
	}
	parts := []string{"✓ installed"}
	if st.ComposeOK {
		parts = append(parts, "✓ compose")
	} else {
		parts = append(parts, "✗ compose")
	}
	if st.Ready {
		parts = append(parts, "✓ daemon up")
	} else {
		parts = append(parts, "✗ daemon down")
	}
	// No leading symbol column for engines — the inline marks carry the state.
	fmt.Printf("  %-16s %s\n", label, strings.Join(parts, "  "))
}

// caddyVersion returns a short version string for the caddy binary, best-effort.
func caddyVersion(path string) string {
	out, err := exec.Command(path, "version").Output()
	if err != nil {
		return "installed"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if fields := strings.Fields(line); len(fields) > 0 {
		return fields[0]
	}
	return "installed"
}

// unbindablePorts returns the ports (as strings) that cannot currently be bound.
// It binds the wildcard address (":p") to match how the container engine and
// Caddy publish ports.
func unbindablePorts(ports ...int) []string {
	var blocked []string
	for _, p := range ports {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			blocked = append(blocked, fmt.Sprintf("%d", p))
			continue
		}
		ln.Close()
	}
	return blocked
}

// unservedPorts returns the ports (as strings) that nothing is listening on.
// A successful dial is unambiguous where a failed bind is not: "cannot bind"
// conflates "someone is serving this" with "this needs privileges", and only the
// former means the proxy is doing its job.
func unservedPorts(ports ...int) []string {
	var quiet []string
	for _, p := range ports {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 2*time.Second)
		if err != nil {
			quiet = append(quiet, fmt.Sprintf("%d", p))
			continue
		}
		conn.Close()
	}
	return quiet
}

// writableDir reports whether dir exists and accepts a new file.
func writableDir(dir string) error {
	probe := filepath.Join(dir, ".xdev-doctor-write-test")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	os.Remove(probe)
	return nil
}

// writableFile reports whether path can be opened for writing (without
// modifying it).
func writableFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}
