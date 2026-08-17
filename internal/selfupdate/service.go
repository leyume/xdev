package selfupdate

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// systemdUnit and launchdLabel are what deploy/xdev.service and
// deploy/com.leyume.xdev.plist install as. They are duplicated here rather than
// discovered because a wrong guess would restart somebody else's service.
const (
	systemdUnit  = "xdev"
	launchdLabel = "com.leyume.xdev"
)

// Service is the init system supervising xdev, or none.
type Service struct {
	Kind string // "systemd", "launchd", or "" when nothing supervises xdev
	Name string
}

// Managed reports whether there is a service to restart. When false the update
// still swaps the binary — it just takes effect the next time xdev is started
// by whatever does start it.
func (s Service) Managed() bool { return s.Kind != "" }

// String names the service the way its own tooling does, for messages.
func (s Service) String() string {
	if !s.Managed() {
		return "none"
	}
	return s.Kind + ":" + s.Name
}

// DetectService finds the init system supervising xdev.
//
// Presence of systemctl is not enough — plenty of machines have systemd but ran
// xdev from a terminal — so this asks whether the unit is actually loaded. A
// wrong answer here is the difference between restarting the service and
// silently not restarting anything.
func DetectService(ctx context.Context) Service {
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return Service{}
		}
		out, err := run(ctx, "systemctl", "list-unit-files", systemdUnit+".service", "--no-legend")
		if err != nil || strings.TrimSpace(out) == "" {
			return Service{}
		}
		return Service{Kind: "systemd", Name: systemdUnit}
	case "darwin":
		if _, err := exec.LookPath("launchctl"); err != nil {
			return Service{}
		}
		if _, err := run(ctx, "launchctl", "print", "system/"+launchdLabel); err != nil {
			return Service{}
		}
		return Service{Kind: "launchd", Name: launchdLabel}
	}
	return Service{}
}

// Restart restarts the service. On launchd, kickstart -k stops and starts in
// one step and, unlike unload/load, does not need the plist path.
func (s Service) Restart(ctx context.Context) error {
	if !s.Managed() {
		return nil
	}
	var err error
	if s.Kind == "systemd" {
		_, err = run(ctx, "systemctl", "restart", s.Name)
	} else {
		_, err = run(ctx, "launchctl", "kickstart", "-k", "system/"+s.Name)
	}
	return err
}

// Active reports whether the service is running right now.
func (s Service) Active(ctx context.Context) bool {
	if !s.Managed() {
		return false
	}
	if s.Kind == "systemd" {
		out, _ := run(ctx, "systemctl", "is-active", s.Name)
		// is-active exits non-zero for anything but "active", so the exit code
		// is redundant with the word — and the word survives a context that
		// expired mid-call, which the exit code does not.
		return strings.TrimSpace(out) == "active"
	}
	_, err := run(ctx, "launchctl", "print", "system/"+s.Name)
	return err == nil
}

// MainPID is the pid of the service's main process, or 0 when it is not
// running or cannot be determined.
//
// This exists because "is the service active?" cannot tell a successful restart
// apart from a restart that never happened. A restart can fail for reasons that
// leave the old process happily running — polkit refusing an interactive
// authorisation is the common one — and then an updater that only checks
// liveness declares success while the machine goes on serving the old binary.
// Requiring the pid to change is what makes the check mean "the new binary is
// running".
func (s Service) MainPID(ctx context.Context) int {
	if !s.Managed() {
		return 0
	}
	if s.Kind == "systemd" {
		out, err := run(ctx, "systemctl", "show", "-p", "MainPID", "--value", s.Name)
		if err != nil {
			return 0
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(out))
		return pid
	}
	out, err := run(ctx, "launchctl", "print", "system/"+s.Name)
	if err != nil {
		return 0
	}
	return parseLaunchdPID(out)
}

// parseLaunchdPID pulls the pid out of `launchctl print` output, which reports
// it as a "pid = 1234" line among many other properties.
func parseLaunchdPID(out string) int {
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "pid" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(val))
		if err == nil {
			return pid
		}
	}
	return 0
}

// WaitRestarted polls until the service is up under a *different* process than
// prevPID, or the deadline passes.
//
// A restarted xdev opens its database, runs migrations and rebuilds proxy
// config before it is serving, so checking once immediately after the restart
// reports a healthy service as failed and triggers a rollback nobody needed.
// Passing prevPID = 0 (the service was not running) accepts any live process.
func (s Service) WaitRestarted(ctx context.Context, d time.Duration, prevPID int) bool {
	if !s.Managed() {
		return false
	}
	deadline := time.Now().Add(d)
	for {
		if s.Active(ctx) {
			// A pid we cannot read is not evidence of anything, so fall back to
			// liveness rather than failing an update over an unreadable
			// property.
			pid := s.MainPID(ctx)
			if pid == 0 || pid != prevPID {
				return true
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Logs returns the tail of the service's log, to print when an update fails.
// Best effort: an empty string just means the user has to go look themselves.
func (s Service) Logs(ctx context.Context, lines int) string {
	if s.Kind != "systemd" {
		return ""
	}
	out, _ := run(ctx, "journalctl", "-u", s.Name, "-n", strconv.Itoa(lines), "--no-pager")
	return out
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
