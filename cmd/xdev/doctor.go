package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"xdev/internal/config"
	"xdev/internal/runtime"
	"xdev/internal/store"
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
		d.ok("selected engine", string(sel.Current()), true)
	} else {
		d.fail("selected engine", fmt.Sprintf("%s not usable (needs the binary + compose plugin)", sel.Current()), true)
	}

	// --- caddy ---------------------------------------------------------------
	if o.caddyManage {
		if path, err := exec.LookPath("caddy"); err == nil {
			d.ok("caddy", caddyVersion(path), true)
		} else {
			d.fail("caddy", "not found on PATH (install caddy, or run with -caddy=false)", true)
		}
	} else {
		d.skip("caddy", "supervision disabled (-caddy=false)")
	}

	// --- ports ---------------------------------------------------------------
	label := fmt.Sprintf("ports %d / %d", o.httpsPort, o.httpPort)
	if blocked := unbindablePorts(o.httpsPort, o.httpPort); len(blocked) == 0 {
		d.ok(label, "bindable", true)
	} else {
		d.fail(label, fmt.Sprintf("cannot bind %s (in use or needs privileges)", strings.Join(blocked, ", ")), true)
	}

	// --- data dir + admin ----------------------------------------------------
	if err := writableDir(cfg.DataDir); err != nil {
		d.fail("data dir", fmt.Sprintf("%s not writable: %v", cfg.DataDir, err), true)
	} else {
		d.ok("data dir", fmt.Sprintf("%s (db: %s)", cfg.DataDir, cfg.DBPath), true)
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
			d.ok("admin account", "configured", false)
		}
	}

	// --- hosts file (only relevant for local dev) ----------------------------
	if o.manageHosts {
		if err := writableFile(o.hostsFile); err != nil {
			d.warn("hosts file", fmt.Sprintf("%s not writable (%v) — fine on a server; needed for local .test domains", o.hostsFile, err))
		} else {
			d.ok("hosts file", o.hostsFile+" writable", false)
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

func (d *doctorReport) ok(label, detail string, _ bool) { d.print("✓", label, detail) }
func (d *doctorReport) warn(label, detail string)       { d.print("!", label, detail) }
func (d *doctorReport) skip(label, detail string)       { d.print("–", label, detail) }

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
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
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
