// Package runtime detects which container engines are available on the host
// (kiri, podman and/or docker) and which one xdev should use by default.
//
// All three speak the same `<engine> compose ...` interface (Kiri's CLI takes
// docker's commands), so the rest of xdev only needs to know the engine name —
// it is the executable every call runs. The default prefers kiri, then podman,
// for local macOS development, and docker on a Linux server.
package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Engine is a container engine name, which is also the binary xdev runs.
type Engine string

const (
	Kiri   Engine = "kiri"
	Podman Engine = "podman"
	Docker Engine = "docker"
)

// Engines lists every engine xdev knows, in display order.
var Engines = []Engine{Kiri, Podman, Docker}

// Known reports whether e is an engine xdev can drive.
func Known(e Engine) bool {
	for _, k := range Engines {
		if e == k {
			return true
		}
	}
	return false
}

// EngineStatus describes one engine's availability on this host.
type EngineStatus struct {
	Engine     Engine
	Installed  bool   // the engine binary is on PATH
	ComposeOK  bool   // `<engine> compose version` works
	Ready      bool   // the daemon/machine responds (`<engine> ps` succeeds)
	Version    string // engine version line, best-effort
	ComposeVer string // compose version line, best-effort
}

// Info is the overall picture of container tooling on this host.
type Info struct {
	Engines []EngineStatus // one per Engines entry, same order
	Default Engine         // chosen default given what's installed + the OS
}

// Status returns e's entry (zero, not installed, for an unknown engine).
func (i Info) Status(e Engine) EngineStatus {
	for _, st := range i.Engines {
		if st.Engine == e {
			return st
		}
	}
	return EngineStatus{Engine: e}
}

// Detect probes the host for every engine and picks a sensible default.
func Detect() Info {
	addKiriToPath()
	var info Info
	for _, e := range Engines {
		info.Engines = append(info.Engines, probe(e))
	}
	info.Default = pickDefault(info, runtime.GOOS)
	return info
}

// kiriDirs are where Kiri.app puts its CLI. The app links it into ~/.kiri/bin,
// which a shell profile may or may not add to PATH — and a service started by
// launchd or a script never reads the profile.
var kiriDirs = []string{"~/.kiri/bin", "/Applications/Kiri.app/Contents/MacOS"}

// addKiriToPath appends Kiri's CLI directory to PATH when kiri is installed but
// not on it, so every `kiri ...` exec in xdev resolves.
func addKiriToPath() {
	if _, err := exec.LookPath(string(Kiri)); err == nil {
		return
	}
	home, _ := os.UserHomeDir()
	for _, d := range kiriDirs {
		d = strings.Replace(d, "~", home, 1)
		if st, err := os.Stat(filepath.Join(d, "kiri")); err == nil && !st.IsDir() {
			os.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+d)
			return
		}
	}
}

// pickDefault chooses the engine to use by default. It strongly prefers an
// engine whose daemon is actually running (so docker isn't picked when Docker
// Desktop is off), then falls back to whatever is installed, in the OS's order
// of preference: kiri, podman, docker on macOS; docker, podman, kiri on Linux.
func pickDefault(info Info, goos string) Engine {
	order := []Engine{Kiri, Podman, Docker}
	if goos != "darwin" {
		order = []Engine{Docker, Podman, Kiri}
	}
	for _, needReady := range []bool{true, false} {
		for _, e := range order {
			st := info.Status(e)
			if st.Installed && st.ComposeOK && (st.Ready || !needReady) {
				return e
			}
		}
	}
	if goos == "darwin" {
		return Podman // nothing usable; name the engine the docs install
	}
	return Docker
}

// probe checks a single engine: is the binary present, does compose work, and
// grab version strings for display.
func probe(engine Engine) EngineStatus {
	st := EngineStatus{Engine: engine}

	path, err := exec.LookPath(string(engine))
	if err != nil || path == "" {
		return st
	}
	st.Installed = true
	if engine == Kiri {
		// `kiri --version` prints usage; the engine behind it reports one.
		if v := firstLine(run(string(engine), "version", "--format", "{{.Server.Version}}")); v != "" {
			st.Version = "kiri · engine " + v
		}
	} else {
		st.Version = firstLine(run(string(engine), "--version"))
	}

	// Compose v2 is invoked as a subcommand: `docker compose` / `podman compose`.
	if out, err := exec.Command(string(engine), "compose", "version").CombinedOutput(); err == nil {
		st.ComposeOK = true
		st.ComposeVer = firstLine(string(out))
	}
	st.Ready = engineReady(engine)
	return st
}

// engineReady reports whether the engine's daemon/machine actually responds.
// `<engine> ps` fails fast with a clear error when the daemon is down, so it's a
// good cheap readiness probe (bounded by a short timeout in case it hangs).
func engineReady(engine Engine) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, string(engine), "ps").Run() == nil
}

func run(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
