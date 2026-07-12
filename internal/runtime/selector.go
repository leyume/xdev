package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// Selector holds the engine xdev should use right now. It starts from an
// override (a persisted setting or a flag) or the auto-detected default, and
// can be changed at runtime (e.g. from the UI) without a restart. Methods are
// safe for concurrent use.
type Selector struct {
	mu      sync.RWMutex
	current Engine
	info    Info // immutable detection snapshot
}

// NewSelector builds a Selector. override "" (or anything other than a known
// engine) falls back to the detected default.
func NewSelector(info Info, override Engine) *Selector {
	cur := info.Default
	if override == Podman || override == Docker {
		cur = override
	}
	return &Selector{current: cur, info: info}
}

// Current returns the engine in effect.
func (s *Selector) Current() Engine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Info returns the detection snapshot (what's installed / compose-capable).
func (s *Selector) Info() Info { return s.info }

// Status returns the detected status for one engine.
func (s *Selector) Status(e Engine) EngineStatus {
	if e == Docker {
		return s.info.Docker
	}
	return s.info.Podman
}

// Usable reports whether an engine is installed and has compose available.
func (s *Selector) Usable(e Engine) bool {
	st := s.Status(e)
	return st.Installed && st.ComposeOK
}

// UsableEngines lists the engines that can currently be used.
func (s *Selector) UsableEngines() []Engine {
	var out []Engine
	if s.Usable(Podman) {
		out = append(out, Podman)
	}
	if s.Usable(Docker) {
		out = append(out, Docker)
	}
	return out
}

// RunningContainers counts running containers across every usable engine via
// `<engine> ps -q`. The bool is false when no engine is usable or every query
// errored, so the caller can show "—" instead of a misleading 0.
//
// ponytail: counts ALL running containers on the host, not only xdev-managed
// ones — add `--filter name=<prefix>` per app if that distinction matters.
func (s *Selector) RunningContainers() (int, bool) {
	engs := s.UsableEngines()
	if len(engs) == 0 {
		return 0, false
	}
	total, ok := 0, false
	for _, e := range engs {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		out, err := exec.CommandContext(ctx, string(e), "ps", "-q").Output()
		cancel()
		if err != nil {
			continue // daemon may be down; skip this engine
		}
		ok = true
		total += countLines(out)
	}
	return total, ok
}

// countLines counts non-empty lines in `ps -q` output (one container id each).
func countLines(out []byte) int {
	n := 0
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 {
			n++
		}
	}
	return n
}

// Set switches the active engine, rejecting an engine that isn't usable.
func (s *Selector) Set(e Engine) error {
	if e != Podman && e != Docker {
		return fmt.Errorf("unknown engine %q", e)
	}
	if !s.Usable(e) {
		return fmt.Errorf("%s is not usable here (needs the binary + its compose plugin)", e)
	}
	s.mu.Lock()
	s.current = e
	s.mu.Unlock()
	return nil
}
