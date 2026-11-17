package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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
	if Known(override) {
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
func (s *Selector) Status(e Engine) EngineStatus { return s.info.Status(e) }

// Usable reports whether an engine is installed and has compose available.
func (s *Selector) Usable(e Engine) bool {
	st := s.Status(e)
	return st.Installed && st.ComposeOK
}

// UsableEngines lists the engines that can currently be used.
func (s *Selector) UsableEngines() []Engine {
	var out []Engine
	for _, e := range Engines {
		if s.Usable(e) {
			out = append(out, e)
		}
	}
	return out
}

// RunningContainers counts running containers across every usable engine via
// `<engine> ps -q`. The bool is false when no engine is usable or every query
// errored, so the caller can show "—" instead of a misleading 0.
//
// ponytail: counts ALL running containers on the host, not only xdev-managed
// ones — add `--filter name=<prefix>` per app if that distinction matters.
// IDs are counted once: two engines can be one daemon (a docker CLI whose
// context is kiri).
func (s *Selector) RunningContainers() (int, bool) {
	engs := s.UsableEngines()
	if len(engs) == 0 {
		return 0, false
	}
	ids, ok := map[string]bool{}, false
	for _, e := range engs {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		out, err := exec.CommandContext(ctx, string(e), "ps", "-q").Output()
		cancel()
		if err != nil {
			continue // daemon may be down; skip this engine
		}
		ok = true
		for _, id := range strings.Fields(string(out)) {
			ids[id] = true
		}
	}
	return len(ids), ok
}

// Set switches the active engine, rejecting an engine that isn't usable.
func (s *Selector) Set(e Engine) error {
	if !Known(e) {
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
