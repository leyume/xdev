package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Supervisor runs Caddy as a child process of xdev. This is the lean default
// for local development; on a server you'd instead run Caddy under systemd and
// point the Manager at its admin API (don't supervise here).
type Supervisor struct {
	cmd *exec.Cmd
}

// Start launches `caddy run` with its admin API at mgr.adminAddr and waits until
// that API is reachable. Caddy starts with an empty config (no public listeners)
// until the first Manager.Sync pushes routes.
func Start(mgr *Manager) (*Supervisor, error) {
	if _, err := exec.LookPath("caddy"); err != nil {
		return nil, fmt.Errorf("caddy binary not found on PATH (install it, e.g. `brew install caddy`): %w", err)
	}
	cmd := exec.Command("caddy", "run")
	// CADDY_ADMIN tells Caddy where to bind its admin API when no config sets it.
	cmd.Env = append(os.Environ(), "CADDY_ADMIN="+mgr.adminAddr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	s := &Supervisor{cmd: cmd}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.Reachable() {
			return s, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.Stop()
	return nil, fmt.Errorf("caddy admin API never became reachable at %s", mgr.adminAddr)
}

// Stop signals Caddy to shut down gracefully, falling back to a kill.
func (s *Supervisor) Stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	s.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-time.After(5 * time.Second):
		s.cmd.Process.Kill()
	case <-done:
	}
}
