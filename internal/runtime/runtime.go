// Package runtime detects which container engines are available on the host
// (podman and/or docker) and which one xdev should use by default.
//
// Convention carried over from the bizepp sample: podman is the default for
// local macOS development, docker is the default on a Linux server. Both speak
// the same `<engine> compose ...` interface (Compose v2 plugin), so the rest
// of xdev only needs to know the engine name.
package runtime

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Engine is a container engine name: "podman" or "docker".
type Engine string

const (
	Podman Engine = "podman"
	Docker Engine = "docker"
)

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
	Podman  EngineStatus
	Docker  EngineStatus
	Default Engine // chosen default given what's installed + the OS
}

// Detect probes the host for podman and docker and picks a sensible default.
func Detect() Info {
	podman := probe(Podman)
	docker := probe(Docker)

	info := Info{Podman: podman, Docker: docker}
	info.Default = pickDefault(podman, docker)
	return info
}

// pickDefault chooses the engine to use by default. It strongly prefers an
// engine whose daemon is actually running (so docker isn't picked when Docker
// Desktop is off), then falls back to whatever is installed, preferring podman
// on macOS and docker on Linux.
func pickDefault(podman, docker EngineStatus) Engine {
	osPref := func(a, b Engine) Engine {
		if runtime.GOOS == "darwin" {
			return a // podman on mac
		}
		return b // docker on linux
	}

	pReady := podman.Installed && podman.ComposeOK && podman.Ready
	dReady := docker.Installed && docker.ComposeOK && docker.Ready
	switch {
	case pReady && dReady:
		return osPref(Podman, Docker)
	case pReady:
		return Podman
	case dReady:
		return Docker
	}

	// Neither daemon is up; fall back to whatever is installed + compose-capable.
	pUsable := podman.Installed && podman.ComposeOK
	dUsable := docker.Installed && docker.ComposeOK
	switch {
	case pUsable && dUsable:
		return osPref(Podman, Docker)
	case pUsable:
		return Podman
	case dUsable:
		return Docker
	default:
		return osPref(Podman, Docker)
	}
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
	st.Version = firstLine(run(string(engine), "--version"))

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
