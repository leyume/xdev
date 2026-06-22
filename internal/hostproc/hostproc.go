// Package hostproc supervises static apps that run directly on the host (with
// the system's Node) instead of in a container. It spawns each app as a child
// process in its own process group, captures its output to a log file, and
// kills the whole group on stop — so an `npm`→`node` tree goes down cleanly.
//
// Only command-mode static apps have a process here; serve-mode static apps are
// file-served by Caddy and never start a process. xdev targets macOS and Linux,
// so the Unix process-group calls below are always available.
package hostproc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// stopGrace is how long a process gets to exit after SIGTERM before SIGKILL.
const stopGrace = 5 * time.Second

// Supervisor tracks the running host processes, keyed by app id.
type Supervisor struct {
	runDir string // where per-app log files live (under the data dir)
	mu     sync.Mutex
	procs  map[int64]*proc
}

type proc struct {
	cmd  *exec.Cmd
	done chan struct{} // closed once the process has been reaped
}

// NewSupervisor creates a Supervisor that writes logs under runDir.
func NewSupervisor(runDir string) *Supervisor {
	return &Supervisor{runDir: runDir, procs: map[int64]*proc{}}
}

// HasNode reports whether the system Node runtime is on PATH.
func HasNode() bool {
	_, err := exec.LookPath("node")
	return err == nil
}

// Start launches command for app id as a supervised background process, with
// its working directory set to dir and the given environment (env should be the
// full environment, e.g. os.Environ() plus PORT/HOST). Output is written to the
// app's log file (named after name, e.g. "<project>_<app>"). Any prior process
// for the same id is stopped first, so Start is idempotent (acts as restart).
func (s *Supervisor) Start(id int64, name, dir, command string, env []string) error {
	s.Stop(id)

	if err := os.MkdirAll(s.runDir, 0o755); err != nil {
		return err
	}
	logf, err := os.Create(s.logPath(name))
	if err != nil {
		return err
	}

	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Stdout = logf
	cmd.Stderr = logf
	// Own process group so Stop can signal the whole tree (npm -> node).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("start %q in %s: %w", command, dir, err)
	}

	p := &proc{cmd: cmd, done: make(chan struct{})}
	s.mu.Lock()
	s.procs[id] = p
	s.mu.Unlock()

	// Reap the process and drop it from the map when it exits on its own.
	go func() {
		cmd.Wait()
		logf.Close()
		s.mu.Lock()
		if s.procs[id] == p {
			delete(s.procs, id)
		}
		s.mu.Unlock()
		close(p.done)
	}()
	return nil
}

// Stop terminates app id's process group (SIGTERM, then SIGKILL after a grace
// period) and waits for it to be reaped. It is a no-op if nothing is running.
func (s *Supervisor) Stop(id int64) error {
	s.mu.Lock()
	p := s.procs[id]
	s.mu.Unlock()
	if p == nil || p.cmd.Process == nil {
		return nil
	}
	pgid := p.cmd.Process.Pid // group leader's pid == pgid (Setpgid)
	syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(stopGrace):
		syscall.Kill(-pgid, syscall.SIGKILL)
		<-p.done
	}
	return nil
}

// Running reports whether app id has a live supervised process.
func (s *Supervisor) Running(id int64) bool {
	s.mu.Lock()
	p := s.procs[id]
	s.mu.Unlock()
	if p == nil || p.cmd.Process == nil {
		return false
	}
	// Signal 0 probes liveness without affecting the process.
	return syscall.Kill(-p.cmd.Process.Pid, 0) == nil
}

// RunBuild runs a one-shot build command synchronously and returns its combined
// output. Used for serve-mode static apps (and any pre-start build step).
func (s *Supervisor) RunBuild(dir, command string, env []string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("build %q failed: %w\n%s", command, err, string(out))
	}
	return string(out), nil
}

// Logs returns the last `tail` lines of an app's log file ("" if none yet).
func (s *Supervisor) Logs(name string, tail int) (string, error) {
	b, err := os.ReadFile(s.logPath(name))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, "\n"), nil
}

// StopAll terminates every supervised process (used on xdev shutdown).
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	ids := make([]int64, 0, len(s.procs))
	for id := range s.procs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.Stop(id)
	}
}

func (s *Supervisor) logPath(name string) string {
	return filepath.Join(s.runDir, name+".log")
}
