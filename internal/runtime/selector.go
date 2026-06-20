package runtime

import (
	"fmt"
	"sync"
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
